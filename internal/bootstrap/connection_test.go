package bootstrap_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/samjingwen/tandem/internal/bootstrap"
)

func TestPairUsesSupportedExchangeAndPrivateOwnConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture requires POSIX host")
	}
	exchanges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/connect/fixture-grant" || r.Method != "GET" || r.Header.Get("Accept") != "application/json" {
			t.Error("incorrect pairing exchange")
		}
		exchanges++
		_, _ = w.Write([]byte(`{"token":"fixture-token-not-a-real-credential"}`))
	}))
	defer server.Close()
	root := t.TempDir()
	binary := filepath.Join(root, "opencode-fixture")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n  'service status') echo '%s';;\n  'api get /api/info') echo '{\"version\":\"2.1.0\",\"urls\":[\"%s\"]}';;\n  'api post /api/pair') echo '{\"code\":\"fixture-grant\"}';;\n  *) exit 12;;\nesac\n", server.URL, server.URL)
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "instance")
	connection, err := bootstrap.Pair(context.Background(), binary, dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := bootstrap.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != connection || loaded.URL != server.URL || exchanges != 1 {
		t.Fatal("pairing not durable")
	}
	if _, err = bootstrap.Pair(context.Background(), binary, dir); err == nil {
		t.Fatal("existing connection was overwritten")
	}
	if err = os.Chmod(filepath.Join(dir, "opencode-connection.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = bootstrap.Load(dir); err == nil {
		t.Fatal("insecure configuration accepted")
	}
}

func TestClientTokenIsStablePrivateAndNeverFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	first, err := bootstrap.ClientToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := bootstrap.ClientToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatal("unstable token")
	}
	aliasDir := t.TempDir()
	if err = os.Symlink(filepath.Join(dir, "client.token"), filepath.Join(aliasDir, "client.token")); err != nil {
		t.Fatal(err)
	}
	if _, err = bootstrap.ClientToken(aliasDir); err == nil {
		t.Fatal("symlink token accepted")
	}
}
