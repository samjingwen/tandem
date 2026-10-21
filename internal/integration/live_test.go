package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/api"
	"github.com/samjingwen/tandem/internal/bootstrap"
	"github.com/samjingwen/tandem/internal/client"
	"github.com/samjingwen/tandem/internal/opencode"
	"github.com/samjingwen/tandem/internal/store"
	"github.com/samjingwen/tandem/internal/workspace"
)

// TestLiveCoordinatorDelegationAcceptance is an explicit model-backed evaluation,
// not deterministic CI. The supplied fixture directory must be freshly created.
// All task records/checkouts/evidence are retained there for inspection.
func TestLiveCoordinatorDelegationAcceptance(t *testing.T) {
	root := os.Getenv("TANDEM_LIVE_DEMO_DIR")
	connectionDir := os.Getenv("TANDEM_LIVE_CONNECTION_DIR")
	plugin := os.Getenv("TANDEM_LIVE_PLUGIN")
	if root == "" || connectionDir == "" || plugin == "" {
		t.Skip("requires explicit TANDEM_LIVE_DEMO_DIR, TANDEM_LIVE_CONNECTION_DIR, TANDEM_LIVE_PLUGIN; this test spends model capacity")
	}
	for _, path := range []string{root, connectionDir, plugin} {
		if !filepath.IsAbs(path) {
			t.Fatal("live fixture/config/plugin paths must be absolute")
		}
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal("live demo directory must not already exist", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	connection, err := bootstrap.Load(connectionDir)
	if err != nil {
		t.Fatal("load explicit Tandem-owned connection configuration", err)
	}
	runtime, err := opencode.New(opencode.Options{URL: connection.URL, Password: connection.Password})
	if err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Info(ctx)
	if err != nil || !info.Compatible {
		t.Fatalf("runtime compatibility: %+v %v", info, err)
	}
	stateDir := filepath.Join(root, "state")
	db, err := store.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	token, err := bootstrap.ClientToken(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	workspaces := workspace.Git{Root: filepath.Join(root, "worktrees"), UseWorktrunk: true}
	engine := core.New(db, runtime, workspaces, core.Options{RuntimeID: info.ID, Concurrency: 2, RegisterProject: workspace.Register})
	server := httptest.NewServer(api.New(engine, api.Options{Token: token}))
	defer server.Close()
	oneTransport := http.DefaultTransport.(*http.Transport).Clone()
	defer oneTransport.CloseIdleConnections()
	one, err := client.New(client.Options{URL: server.URL, Token: token, HTTPClient: &http.Client{Transport: oneTransport}})
	if err != nil {
		t.Fatal(err)
	}
	two, err := client.New(client.Options{URL: server.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "project")
	if err = os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{"$schema": "https://opencode.ai/config.json", "plugins": []any{map[string]any{"package": plugin, "options": map[string]any{"url": server.URL, "tokenFile": filepath.Join(stateDir, "client.token")}}}})
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string][]byte{"opencode.json": config, "go.mod": []byte("module tandem-live-fixture\n\ngo 1.27.3\n"), "AGENTS.md": []byte("# Disposable Tandem live acceptance project\nThis is a deliberately retained test fixture.\nModify only the assigned checkout.\nDo not use network, credentials, publication, or unmanaged subagents.\nUse Tandem tools for delegation and generation-specific result reports.\nDo not answer permission requests yourself.\n")} {
		if err = os.WriteFile(filepath.Join(repo, path), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-b", "master"}, {"add", "AGENTS.md", "go.mod", "opencode.json"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "live fixture"}} {
		if _, err = workspace.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	var project core.Project
	if err = one.Do(ctx, http.MethodPost, "/v1/projects", "live-register", core.RegisterProject{Name: "Live delegation fixture", Path: repo, BaseRef: "master"}, &project); err != nil {
		t.Fatal(err)
	}
	var parent core.Task
	instructions := "Use Tandem's create tool to delegate EXACTLY TWO worker children in this project and this parent task. One child implements package arithmetic, func Sum(a,b int) int in sum.go and sum_test.go. The other implements package arithmetic, func Product(a,b int) int in product.go and product_test.go. Give each child a distinct stable operation ID and explicit instructions to modify its OWN checkout, use native edit/patch tools (not shell), then report artifact evidence through tandem_result with the instruction generation provided in its prompt. Do not claim tests ran if they did not. Workers must not spawn more children or approve permissions. After dispatching both children, return a concise summary and STOP; do not poll, edit files, submit your own completion result, or accept/merge/publish anything. The external acceptance harness will run Go tests and inspect both checkouts."
	binary := os.Getenv("TANDEM_LIVE_BINARY")
	if binary == "" {
		binary = filepath.Join(filepath.Dir(plugin), "bin", "tandem")
	}
	intake := exec.CommandContext(ctx, binary, "--url", server.URL, "--json", "--operation-id", "live-intake", "tasks", "create", "--project", project.ID, "--role", "coordinator", "--title", "Delegate two independent arithmetic changes", "--instructions", instructions)
	intake.Env = []string{"PATH=" + os.Getenv("PATH"), "TANDEM_TOKEN=" + token}
	var intakeOut, intakeErr bytes.Buffer
	intake.Stdout = &intakeOut
	intake.Stderr = &intakeErr
	if err = intake.Run(); err != nil {
		t.Fatalf("standalone CLI intake failed: %v: %s", err, intakeErr.String())
	}
	if err = json.Unmarshal(intakeOut.Bytes(), &parent); err != nil {
		t.Fatal("invalid standalone CLI task receipt", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- engine.Run(runCtx, 250*time.Millisecond) }()
	defer func() {
		stop()
		<-done
		state, x := engine.Snapshot(context.Background())
		if x == nil {
			for _, a := range state.Attempts {
				task := state.Tasks[a.TaskID]
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_ = runtime.Interrupt(cleanupCtx, core.SessionSpec{Reference: a.SessionRef, TaskID: a.TaskID, AttemptID: a.ID, InstanceID: state.InstanceID, Directory: a.Workspace.Path, Role: task.Role, Title: task.Title})
				cancel()
			}
		}
	}()
	// The actual standalone CLI process has exited. Dispose of the setup client's
	// transport too; only the second client observes and continues existing work.
	oneTransport.CloseIdleConnections()
	var snapshot core.State
	for {
		if err = two.Do(ctx, http.MethodGet, "/v1/snapshot", "", nil, &snapshot); err != nil {
			t.Fatal(err)
		}
		children := 0
		ready := 0
		for _, task := range snapshot.Tasks {
			if task.State == "failed" {
				t.Fatalf("native task failed; retained task=%s attempt=%s fixture=%s", task.ID, task.AttemptID, root)
			}
			if task.ParentID == parent.ID {
				children++
				if task.State == "ready_for_review" && snapshot.Attempts[task.AttemptID].Observed == "idle" {
					ready++
				}
			}
		}
		if children > 2 {
			t.Fatal("coordinator exceeded bounded two-worker proposal")
		}
		if children == 2 && ready == 2 {
			break
		}
		for _, d := range snapshot.Decisions {
			if d.Status == "pending" {
				t.Fatalf("live evaluation requires a user decision, not auto-approval: task=%s decision=%s kind=%s; fixture retained at %s", d.TaskID, d.ID, d.Kind, root)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("live delegation did not finish; native/runtime access is not a passing model check; retained fixture=%s", root)
		case <-time.After(500 * time.Millisecond):
		}
	}
	seen := map[string]bool{}
	for _, task := range snapshot.Tasks {
		if task.ParentID != parent.ID {
			continue
		}
		a := snapshot.Attempts[task.AttemptID]
		if seen[a.Workspace.Path] {
			t.Fatal("native modifying workers shared a checkout")
		}
		seen[a.Workspace.Path] = true
		if err = workspaces.Inspect(ctx, project, a.Workspace); err != nil {
			t.Fatal(err)
		}
		validateArithmeticFixture(t, a.Workspace.Path)
		oracle := ""
		if _, x := os.Stat(filepath.Join(a.Workspace.Path, "sum.go")); x == nil {
			oracle = "if Sum(2,3)!=5 || Sum(-2,3)!=1 { t.Fatal(\"incorrect Sum\") }"
		} else if _, x = os.Stat(filepath.Join(a.Workspace.Path, "product.go")); x == nil {
			oracle = "if Product(4,7)!=28 || Product(-2,3)!=-6 { t.Fatal(\"incorrect Product\") }"
		} else {
			t.Fatal("worker did not produce its specified public Go artifact", task.ID)
		}
		verification := "package arithmetic\nimport \"testing\"\nfunc TestExternalTandemAcceptance(t *testing.T){" + oracle + "}\n"
		if err = os.WriteFile(filepath.Join(a.Workspace.Path, "tandem_acceptance_test.go"), []byte(verification), 0600); err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(ctx, "go", "test", "-timeout=15s", "./...")
		command.Dir = a.Workspace.Path
		// No control-plane/provider environment is injected into generated code.
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "CGO_ENABLED=0"}
		output, testErr := command.CombinedOutput()
		if testErr != nil {
			t.Fatalf("actual fixture tests failed for %s: %s", task.ID, output)
		}
		if err = os.WriteFile(filepath.Join(root, "verified-"+task.ID+".log"), output, 0600); err != nil {
			t.Fatal(err)
		}
		var detail api.TaskDetail
		if err = two.Do(ctx, http.MethodGet, "/v1/tasks/"+task.ID, "", nil, &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Result == nil || detail.Result.Generation != task.Generation {
			t.Fatal("worker omitted current-generation result")
		}
	}
	// A separate followup attaches to the same coordinator task/session.
	before := snapshot.Tasks[parent.ID].AttemptID
	var followed core.Task
	if err = two.Do(ctx, http.MethodPost, "/v1/tasks/"+parent.ID+"/followup", "live-followup", core.Followup{Text: "The external harness verified both worker checkouts. Reply briefly with their existing IDs. Do not create more work."}, &followed); err != nil {
		t.Fatal(err)
	}
	if followed.ID != parent.ID || followed.AttemptID != before {
		t.Fatal("followup changed task/session ownership")
	}
	for {
		var continued core.State
		if err = two.Do(ctx, http.MethodGet, "/v1/snapshot", "", nil, &continued); err != nil {
			t.Fatal(err)
		}
		admitted := false
		for _, c := range continued.Commands {
			if c.AttemptID == before && c.Generation == followed.Generation && c.Status == "admitted" {
				admitted = true
			}
		}
		if admitted && continued.Attempts[before].Observed == "idle" {
			if len(continued.Tasks) != 3 {
				t.Fatal("continuation duplicated delegated work")
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("same-session native continuation was not verified")
		case <-time.After(500 * time.Millisecond):
		}
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "instance=%s\nparent=%s\ncoordinatorAttempt=%s\nchildren=%d\nretained=%s\n", snapshot.InstanceID, parent.ID, before, len(seen), root)
	for _, task := range snapshot.Tasks {
		if task.ParentID == parent.ID {
			a := snapshot.Attempts[task.AttemptID]
			fmt.Fprintf(&summary, "worker=%s attempt=%s session=%s workspace=%s\n", task.ID, a.ID, a.SessionRef, a.Workspace.Path)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "acceptance.txt"), []byte(summary.String()), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(summary.String())
}

// This is a narrow execution-safety preflight for the generated demo artifact,
// not a source-shape assertion used to prove arithmetic correctness. The actual
// independent Go test oracle above proves behavior. This is not an OS sandbox.
func validateArithmeticFixture(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Size() > 256*1024 {
			t.Fatal("live demo produced an unexpected/nonregular/oversized artifact", entry.Name())
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			switch entry.Name() {
			case ".git", "go.mod", "AGENTS.md", "opencode.json", "README.md":
				continue
			default:
				t.Fatal("live arithmetic demo produced an unexpected execution input", entry.Name())
			}
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, entry.Name()), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if file.Name.Name != "arithmetic" {
			t.Fatal("unexpected demo package")
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil || path != "testing" {
				t.Fatal("demo imports are limited to testing; review unexpected generated code manually")
			}
		}
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if strings.Contains(comment.Text, "go:") {
					t.Fatal("compiler directives are not allowed in the arithmetic demo")
				}
			}
		}
		for _, decl := range file.Decls {
			if function, ok := decl.(*ast.FuncDecl); ok && function.Name.Name == "init" {
				t.Fatal("demo initializer requires manual review")
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if _, ok := node.(*ast.GoStmt); ok {
				t.Fatal("demo goroutines require manual review")
			}
			return true
		})
	}
}
