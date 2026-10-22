package integration_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/bootstrap"
	"github.com/samjingwen/tandem/internal/client"
)

func daemonFixtureDirectory(t *testing.T, source string) string {
	t.Helper()
	connection, err := bootstrap.Load(source)
	if err != nil {
		t.Fatal("load explicit Tandem-owned connection configuration", err)
	}
	directory := t.TempDir()
	if err = os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(connection)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "opencode-connection.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// TestLiveDaemonProcessOwnershipAndClientLifetime exercises the actual built
// service binary against an explicitly supplied Tandem-owned runtime connection.
// It starts no tasks/models and never restarts or stops the OpenCode service.
func TestLiveDaemonProcessOwnershipAndClientLifetime(t *testing.T) {
	directory := os.Getenv("TANDEM_LIVE_CONNECTION_DIR")
	binary := os.Getenv("TANDEM_LIVE_BINARY")
	if directory == "" || binary == "" {
		t.Skip("requires explicit TANDEM_LIVE_CONNECTION_DIR and TANDEM_LIVE_BINARY")
	}
	if !filepath.IsAbs(directory) || !filepath.IsAbs(binary) {
		t.Fatal("live paths must be absolute")
	}
	directory = daemonFixtureDirectory(t, directory)
	verifyDaemonFixture(t, binary, directory)
}

func verifyDaemonFixture(t *testing.T, binary, directory string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(random[:])
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TANDEM_TOKEN=" + token}
	command := exec.CommandContext(ctx, binary, "serve", "--state-dir", directory, "--bind", "127.0.0.1:0", "--allocator", "git")
	command.Env = env
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait(); close(done) }()
	defer func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
	}()
	ready := make(chan []byte, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- append([]byte{}, scanner.Bytes()...)
		} else {
			ready <- nil
		}
	}()
	var receipt struct {
		URL        string `json:"url"`
		InstanceID string `json:"instanceId"`
	}
	select {
	case line := <-ready:
		if json.Unmarshal(line, &receipt) != nil || receipt.URL == "" || receipt.InstanceID == "" {
			t.Fatal("service did not emit a valid startup receipt")
		}
	case <-ctx.Done():
		t.Fatal("service startup timed out")
	}
	firstTransport := http.DefaultTransport.(*http.Transport).Clone()
	defer firstTransport.CloseIdleConnections()
	first, err := client.New(client.Options{URL: receipt.URL, Token: token, HTTPClient: &http.Client{Transport: firstTransport}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot core.State
	if err = first.Do(ctx, http.MethodGet, "/v1/snapshot", "", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 0 || len(snapshot.Attempts) != 0 || len(snapshot.Projects) != 0 {
		t.Fatal("daemon fixture inherited existing work")
	}
	firstTransport.CloseIdleConnections()
	second, err := client.New(client.Options{URL: receipt.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	var reconnected core.State
	if err = second.Do(ctx, http.MethodGet, "/v1/snapshot", "", nil, &reconnected); err != nil {
		t.Fatal(err)
	}
	if reconnected.InstanceID != receipt.InstanceID || snapshot.InstanceID != reconnected.InstanceID {
		t.Fatal("client replacement changed instance identity")
	}
	other := exec.CommandContext(ctx, binary, "serve", "--state-dir", directory, "--bind", "127.0.0.1:0", "--allocator", "git")
	other.Env = env
	if err = other.Run(); err == nil {
		t.Fatal("second actual service process became a scheduler")
	}
	for {
		var diagnostic struct {
			Ready bool `json:"ready"`
		}
		err = second.Do(ctx, http.MethodGet, "/v1/ready", "", nil, &diagnostic)
		if err == nil && diagnostic.Ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("actual service never became runtime-compatible/ready", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	select {
	case err := <-done:
		t.Fatal("client disconnect terminated the daemon", err)
	default:
	}
	t.Logf("actual daemon instance %s remained ready across independent clients and refused a second scheduler", receipt.InstanceID)
}
