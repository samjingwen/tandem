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
	"net/url"
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
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	var pluginConfiguration []byte
	var explicitConfiguration string
	if plugin != "" {
		if !filepath.IsAbs(plugin) {
			t.Fatal("smoke plugin must be an explicit absolute built package path")
		}
		service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			t.Error("smoke plugin must not dispatch work to the service")
			w.WriteHeader(500)
		}))
		defer service.Close()
		tokenFile := filepath.Join(root, "tandem-fixture-token")
		if err := os.WriteFile(tokenFile, []byte("tandem-fixture-"+password+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		assertFixture := nativeAssertFixture(t, root, plugin)
		config, err := json.Marshal(map[string]any{"plugins": []any{map[string]any{"package": plugin, "options": map[string]any{"url": service.URL, "tokenFile": tokenFile, "coordinatorDirectory": work}}, assertFixture}})
		if err != nil {
			t.Fatal(err)
		}
		pluginConfiguration = config
		explicitConfiguration = filepath.Join(root, "isolated-opencode.json")
		if err := os.WriteFile(explicitConfiguration, []byte(`{"plugins":[]}`), 0600); err != nil {
			t.Fatal(err)
		}
		cmd.Env[len(cmd.Env)-1] = "OPENCODE_CONFIG=" + explicitConfiguration
		// No TANDEM_URL is inherited. The token sentinel cannot authenticate and
		// exists only to verify shell clearing; real plugin credentials use options.
		cmd.Env = append(cmd.Env, "TANDEM_TOKEN=non-credential-smoke-sentinel")
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
	if err := r.CheckQuiescence(ctx, spec); err != nil {
		t.Fatal("native background quiescence could not be established", err)
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
	if err := r.Reply(ctx, spec, decision, json.RawMessage(`{"answer":{"approved":"wrong type"}}`)); !KnownRejected(err) {
		t.Fatalf("native invalid form answer should be deterministically rejected: %v", err)
	}
	if err := r.Reply(ctx, spec, decision, json.RawMessage(`{"answer":{"approved":true}}`)); err != nil {
		t.Fatal(err)
	}
	if plugin != "" {
		// This is the one owned disposable runtime, with no other workers or
		// pending forms. The pinned endpoint reloads ALL locations, so this test
		// must never be adapted to mutate a shared runtime.
		if err := os.WriteFile(explicitConfiguration, pluginConfiguration, 0600); err != nil {
			t.Fatal(err)
		}
		if err := r.request(ctx, "POST", "/api/location/reload", nil, nil); err != nil {
			t.Fatal(err)
		}
		testLivePlugin(t, ctx, r)
		testLivePermissionPolicy(t, ctx, r, spec)
		testLiveNativeAssertion(t, ctx, r, spec.Directory)
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

func nativeAssertFixture(t *testing.T, root, plugin string) string {
	t.Helper()
	directory := filepath.Join(root, "native-assert-plugin")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"authority.js", "client.js"} {
		data, err := os.ReadFile(filepath.Join(plugin, "dist", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := `import { confirmPermissionReply } from "./authority.js"
export default {
  id: "tandem-native-assert-fixture",
  async setup(ctx) {
    await ctx.session.hook("prompt", async (event) => {
      if (event.prompt.text !== "__tandem_assert_probe__") return
      await confirmPermissionReply(ctx.permission.assert, {
        sessionID: event.sessionID, agent: "build", messageID: event.messageID,
        id: "native-assert-fixture", signal: new AbortController().signal,
      }, "smoke-instance", {
        id: "decision-fixture", taskId: "smoke-task", attemptId: "smoke-attempt",
        details: { id: "per_original", action: "shell", resources: ["fixture-only"] },
      }, { decision: "once" }, "native-assert-operation")
    })
  },
}
`
	if err := os.WriteFile(filepath.Join(directory, "package.json"), []byte(`{"type":"module"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "server.js"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func nativePermission(t *testing.T, ctx context.Context, r *Runtime, spec core.SessionSpec, action string, resources []string, metadata map[string]any, save []string) (string, string) {
	t.Helper()
	var result envelope[struct {
		ID     string `json:"id"`
		Effect string `json:"effect"`
	}]
	input := map[string]any{"action": action, "resources": resources}
	if metadata != nil {
		input["metadata"] = metadata
	}
	if save != nil {
		input["save"] = save
	}
	if err := r.request(ctx, "POST", sessionPath(spec)+"/permission", input, &result); err != nil {
		t.Fatal(err)
	}
	if result.Data == nil {
		t.Fatal("missing native permission evaluation")
	}
	return result.Data.ID, result.Data.Effect
}

func testLivePermissionPolicy(t *testing.T, ctx context.Context, r *Runtime, worker core.SessionSpec) {
	t.Helper()
	denied := worker
	denied.Reference = r.Reference("explicit-deny-fixture")
	denied.AttemptID = "explicit-deny-fixture"
	denied.TaskID = "explicit-deny-fixture"
	var deniedSession envelope[session]
	if err := r.request(ctx, "POST", "/api/session", map[string]any{"id": denied.Reference, "location": location{denied.Directory}, "metadata": owner(denied, ""), "permissions": []permissionRule{{"*", "*", "deny"}}}, &deniedSession); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"shell", "tandem.permission.reply"} {
		_, effect := nativePermission(t, ctx, r, denied, action, []string{"explicitly-forbidden"}, nil, nil)
		if effect != "deny" {
			t.Fatalf("native explicit deny was relaxed for %s: got %s", action, effect)
		}
	}
	outside := filepath.Join(filepath.Dir(worker.Directory), "outside-fixture")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worker.Directory, "escape-fixture")); err != nil {
		t.Fatal(err)
	}
	// A managed session's stored assignment must not be re-rooted by its current
	// native location, including after a move to a different project/location.
	moved := worker
	moved.Reference = r.Reference("moved-native-fixture")
	moved.AttemptID = "moved-native-fixture"
	moved.TaskID = "moved-native-fixture"
	var movedSession envelope[session]
	if err := r.request(ctx, "POST", "/api/session", map[string]any{"id": moved.Reference, "location": location{outside}, "metadata": owner(moved, ""), "permissions": sessionPermissions(moved)}, &movedSession); err != nil {
		t.Fatal(err)
	}
	waitLivePlugin(t, ctx, r, "/api/plugin?"+url.Values{"location[directory]": []string{outside}}.Encode())
	_, effect := nativePermission(t, ctx, r, moved, "read", []string{"file-in-other-project"}, nil, nil)
	if effect != "deny" {
		t.Fatal("moved native location redefined the assigned directory")
	}
	coordinator := worker
	coordinator.Reference = r.Reference("smoke-coordinator")
	coordinator.AttemptID = "smoke-coordinator"
	coordinator.TaskID = "smoke-coordinator"
	coordinator.Role = "coordinator"
	if err := r.Ensure(ctx, coordinator); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []core.SessionSpec{worker, coordinator} {
		edit := "allow"
		if spec.Role == "coordinator" {
			edit = "deny"
		}
		for _, probe := range []struct {
			action, resource, effect string
			metadata                 map[string]any
		}{
			{"read", "README.md", "allow", nil}, {"edit", "new.go", edit, nil},
			{"read", "../outside-fixture/file", "deny", nil}, {"edit", "../outside-fixture/file", "deny", nil},
			{"read", filepath.Join(outside, "file"), "deny", nil}, {"external_directory", outside + "/*", "deny", nil},
			{"subagent", "general", "deny", nil}, {"opencode_session_move", "*", "deny", nil}, {"foreign_mcp_tool", "*", "deny", nil},
			{"read", "escape-fixture/file", "deny", nil}, {"grep", "fixture-pattern", "deny", map[string]any{"path": "escape-fixture"}},
			{"grep", "fixture-pattern", "ask", map[string]any{"path": "."}},
			{"edit", "opencode.json", "deny", nil}, {"shell", "fixture-command-never-executed", "ask", nil},
		} {
			id, effect := nativePermission(t, ctx, r, spec, probe.action, []string{probe.resource}, probe.metadata, nil)
			if effect != probe.effect {
				t.Fatalf("role=%s action=%s resource=%q effect=%s want=%s", spec.Role, probe.action, probe.resource, effect, probe.effect)
			}
			if effect == "ask" {
				if err := r.request(ctx, "POST", sessionPath(spec)+"/permission/"+url.PathEscape(id)+"/reply", map[string]any{"decision": "reject"}, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Explicit native saved decisions remain authoritative. Tandem must not
	// replace an actual user "always" choice with a new ask on every command.
	id, effect := nativePermission(t, ctx, r, worker, "shell", []string{"fixture-saved-shell"}, nil, []string{"*"})
	if effect != "ask" {
		t.Fatal("shell was not interactive")
	}
	if err := r.request(ctx, "POST", sessionPath(worker)+"/permission/"+id+"/reply", map[string]any{"decision": "always"}, nil); err != nil {
		t.Fatal(err)
	}
	_, effect = nativePermission(t, ctx, r, worker, "shell", []string{"different-never-executed-command"}, nil, nil)
	if effect != "allow" {
		t.Fatal("Tandem did not preserve the native user's saved shell decision")
	}
}

func testLiveNativeAssertion(t *testing.T, ctx context.Context, r *Runtime, directory string) {
	t.Helper()
	spec := core.SessionSpec{Reference: "ses_nativeAssertFixture", Directory: directory}
	var created envelope[session]
	if err := r.request(ctx, "POST", "/api/session", map[string]any{"id": spec.Reference, "location": location{directory}, "permissions": []permissionRule{{"*", "*", "allow"}}}, &created); err != nil {
		t.Fatal(err)
	}
	// Even a broad preexisting native allow and a saved project approval cannot
	// bypass the plugin's dedicated confirmation action.
	id, effect := nativePermission(t, ctx, r, spec, "tandem.permission.reply", []string{"seed"}, nil, []string{"*"})
	if effect != "ask" {
		t.Fatal("permission forwarding gate silently allowed")
	}
	if err := r.request(ctx, "POST", sessionPath(spec)+"/permission/"+id+"/reply", map[string]any{"decision": "always"}, nil); err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() {
		var admitted envelope[admission]
		completed <- r.request(ctx, "POST", sessionPath(spec)+"/prompt", map[string]any{"id": "msg_nativeAssertFixture", "text": "__tandem_assert_probe__", "resume": false}, &admitted)
	}()
	for range 100 {
		pending, err := getData[[]struct {
			ID       string                     `json:"id"`
			Action   string                     `json:"action"`
			Metadata map[string]json.RawMessage `json:"metadata"`
		}](r, ctx, sessionPath(spec)+"/permission")
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range pending {
			if item.Action != "tandem.permission.reply" {
				continue
			}
			if string(item.Metadata["answer"]) != `{"decision":"once"}` || !strings.Contains(string(item.Metadata["request"]), "per_original") {
				t.Fatal("native assertion lost original request/answer metadata")
			}
			select {
			case err := <-completed:
				t.Fatalf("assertion did not wait for the native user: %v", err)
			default:
			}
			if err := r.request(ctx, "POST", sessionPath(spec)+"/permission/"+item.ID+"/reply", map[string]any{"decision": "once"}, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-completed:
				if err != nil {
					t.Fatal(err)
				}
				return
			case <-ctx.Done():
				t.Fatal("native assertion failed to resume after explicit user reply")
			}
		}
		select {
		case err := <-completed:
			t.Fatalf("native assertion bypassed pending user confirmation: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("native assertion did not create a pending user confirmation")
}

func waitLivePlugin(t *testing.T, ctx context.Context, r *Runtime, route string) {
	t.Helper()
	type pluginInfo struct {
		ID     string          `json:"id"`
		Source json.RawMessage `json:"source"`
		State  struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"state"`
	}
	active := false
	for range 100 {
		plugins, err := getData[[]pluginInfo](r, ctx, route)
		if err != nil {
			t.Fatal(err)
		}
		for _, plugin := range plugins {
			if plugin.State.Status == "failed" {
				t.Fatalf("isolated plugin id=%q source=%s failed: %s", plugin.ID, plugin.Source, strings.ReplaceAll(plugin.State.Error, r.password, "[redacted]"))
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
}

func testLivePlugin(t *testing.T, ctx context.Context, r *Runtime) {
	t.Helper()
	waitLivePlugin(t, ctx, r, "/api/plugin")
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
