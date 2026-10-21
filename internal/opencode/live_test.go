package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
)

// TestLiveIsolated210 never discovers, reads, or mutates a shared service. The
// explicit executable runs with disposable HOME/XDG roots and no inherited auth.
func TestLiveIsolated210(t *testing.T) {
	binary := os.Getenv("TANDEM_OPENCODE_SMOKE_BINARY")
	if binary == "" {
		t.Skip("opt in with TANDEM_OPENCODE_SMOKE_BINARY=/absolute/path/to/2.1.0/opencode")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("smoke binary must be an explicit absolute path")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(secret)
	// Keep the process alive through test cleanup so stdin can close gracefully;
	// t.Context itself is canceled before cleanup callbacks run.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "serve", "--hostname", "127.0.0.1", "--port", "0", "--stdio")
	cmd.Dir = work
	cmd.Env = []string{
		"PATH=" + filepath.Dir(binary) + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_DATA_HOME=" + filepath.Join(root, "data"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"), "XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"OPENCODE_CONFIG_DIR=" + filepath.Join(root, "config", "opencode"),
		"OPENCODE_PASSWORD=" + password,
		"OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_FILEWATCHER=1", "OPENCODE_DISABLE_FFF=1",
		`OPENCODE_CONFIG_CONTENT={"plugins":[]}`,
	}
	plugin := os.Getenv("TANDEM_OPENCODE_SMOKE_PLUGIN")
	if plugin != "" {
		if !filepath.IsAbs(plugin) {
			t.Fatal("smoke plugin must be an explicit absolute built package path")
		}
		service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			t.Error("smoke plugin must not dispatch work to the service")
			w.WriteHeader(500)
		}))
		defer service.Close()
		config, err := json.Marshal(map[string]any{"plugins": []string{plugin}})
		if err != nil {
			t.Fatal(err)
		}
		cmd.Env[len(cmd.Env)-1] = "OPENCODE_CONFIG_CONTENT=" + string(config)
		cmd.Env = append(cmd.Env, "TANDEM_URL="+service.URL, "TANDEM_TOKEN="+password)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard // Do not capture potentially sensitive runtime diagnostics.
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("isolated runtime did not exit cleanly: %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	endpoint := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var ready struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(scanner.Bytes(), &ready) == nil && ready.URL != "" {
				endpoint <- ready.URL
				return
			}
		}
		endpoint <- ""
	}()
	var address string
	select {
	case address = <-endpoint:
	case <-ctx.Done():
		t.Fatal("isolated server did not become ready")
	}
	if address == "" {
		t.Fatal("isolated server exited before ready")
	}
	r, err := New(Options{URL: address, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	info, err := r.Info(ctx)
	if err != nil || !info.Compatible {
		t.Fatalf("isolated runtime must be exactly 2.1.0: %+v %v", info, err)
	}
	spec := core.SessionSpec{Reference: r.Reference("smoke-attempt"), TaskID: "smoke-task", AttemptID: "smoke-attempt", InstanceID: "smoke-instance", Directory: work, Title: "Tandem isolated adapter smoke"}
	if err := r.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := r.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	observation, err := r.Inspect(ctx, spec)
	if err != nil || !observation.Exists || !observation.Owned || observation.State != "idle" {
		t.Fatalf("inspect=%+v err=%v", observation, err)
	}
	// Admit parked input using the native API to exercise recovery without spending
	// model tokens or loading credentials. Adapter Prompt must not wake this item.
	command := "smoke-command"
	id := "msg_" + digest("tandem/prompt/"+command)
	var admitted envelope[admission]
	err = r.request(ctx, "POST", sessionPath(spec)+"/prompt", map[string]any{"id": id, "text": "parked smoke", "metadata": owner(spec, command), "resume": false}, &admitted)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prompt(ctx, spec, command, "parked smoke"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReconcilePrompt(ctx, spec, command, "parked smoke"); err != nil {
		t.Fatal(err)
	}
	if err := r.Interrupt(ctx, spec); err != nil {
		t.Fatal(err)
	}
	inbox, err := getData[[]admission](r, ctx, sessionPath(spec)+"/inbox")
	if err != nil || len(inbox) != 1 {
		t.Fatalf("parked input lost: %d %v", len(inbox), err)
	}
	if err := r.CancelPending(ctx, spec); err != nil {
		t.Fatal(err)
	}
	var form envelope[struct {
		ID string `json:"id"`
	}]
	if err := r.request(ctx, "POST", sessionPath(spec)+"/form", map[string]any{"title": "Smoke form", "fields": []any{map[string]any{"key": "approved", "type": "boolean"}}}, &form); err != nil {
		t.Fatal(err)
	}
	if form.Data == nil {
		t.Fatal("form missing data")
	}
	decision := core.Decision{TaskID: spec.TaskID, AttemptID: spec.AttemptID, Kind: "form", RuntimeRef: form.Data.ID}
	if err := r.Reply(ctx, spec, decision, json.RawMessage(`{"answer":{"approved":true}}`)); err != nil {
		t.Fatal(err)
	}
	if plugin != "" {
		testLivePlugin(t, ctx, r)
	}
	streamCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	err = r.Events(streamCtx, stop)
	if err == nil {
		t.Fatal("stream should end on cancellation")
	}
	t.Run("model-execution", func(t *testing.T) {
		t.Skip("isolated environment intentionally has no model credentials; no live model execution claimed")
	})
}

func testLivePlugin(t *testing.T, ctx context.Context, r *Runtime) {
	t.Helper()
	type pluginInfo struct {
		ID    string `json:"id"`
		State struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"state"`
	}
	active := false
	for range 100 {
		plugins, err := getData[[]pluginInfo](r, ctx, "/api/plugin")
		if err != nil {
			t.Fatal(err)
		}
		for _, plugin := range plugins {
			if plugin.State.Status == "failed" {
				t.Fatalf("isolated plugin id=%q failed: %s", plugin.ID, strings.ReplaceAll(plugin.State.Error, r.password, "[redacted]"))
			}
			if plugin.ID == "tandem" && plugin.State.Status == "active" {
				active = true
			}
		}
		if active {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("plugin activation timed out")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !active {
		t.Fatal("built Tandem plugin did not activate in isolated OpenCode")
	}
	// An actual native shell command checks the hook, without printing the token.
	var created envelope[struct {
		ID string `json:"id"`
	}]
	if err := r.request(ctx, "POST", "/api/shell", map[string]any{"command": `test -z "$TANDEM_TOKEN"`, "timeout": 5000}, &created); err != nil {
		t.Fatal(err)
	}
	if created.Data == nil {
		t.Fatal("missing shell admission")
	}
	for range 100 {
		result, err := getData[struct {
			Status string `json:"status"`
			Exit   int    `json:"exit"`
		}](r, ctx, "/api/shell/"+created.Data.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "running" {
			if result.Status != "exited" || result.Exit != 0 {
				t.Fatal("native shell inherited TANDEM_TOKEN")
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("shell did not complete")
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("shell did not complete")
}
