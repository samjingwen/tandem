package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/store"
	"github.com/samjingwen/tandem/internal/testruntime"
)

func requireRustEngine(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("TANDEM_RUST_ENGINE_HELPER")
	if binary == "" {
		t.Skip("make rust-parity supplies the disposable Rust engine consumer")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("engine fixture must be absolute")
	}
	return binary
}

type engineRequest struct {
	Kind      string `json:"kind"`
	Operation string `json:"operation"`
	Target    string `json:"target"`
	Input     any    `json:"input"`
}

func rustEngine(t *testing.T, request engineRequest, mode string, dir string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{mode}
	if dir != "" {
		args = append(args, dir)
	}
	cmd := exec.CommandContext(ctx, requireRustEngine(t), args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = bytes.NewReader(input)
	return cmd.CombinedOutput()
}

func TestRustEngineGoFingerprintOracle(t *testing.T) {
	_ = requireRustEngine(t)
	cases := []engineRequest{
		{Kind: "project.register", Input: core.RegisterProject{Name: "<&> 世界\u2028\u2029\b\f\n\r\t", Path: "/synthetic/project", BaseRef: "master"}},
		{Kind: "project.register", Input: core.RegisterProject{Name: "name", Path: "/synthetic/project", BaseRef: "master", Instructions: "policy", InstructionFiles: []string{"a", "b"}}},
		{Kind: "task.create", Input: core.CreateTask{ProjectID: "project", Title: "title", Instructions: "instructions"}},
		{Kind: "task.create", Input: core.CreateTask{ProjectID: "project", ParentID: "parent", Dependencies: []string{"b", "a"}, Title: "title", Instructions: "instructions", Role: "worker"}},
		{Kind: "task.followup/task", Input: core.Followup{Text: "quote \" slash \\ slash / \u2028 <&>"}},
		{Kind: "task.control/task", Input: core.Control{Action: "pause"}},
		{Kind: "task.control/task", Input: core.Control{Action: "cancel", Cascade: true}},
		{Kind: "task.result/task", Input: core.SubmitResult{AttemptID: "attempt", Generation: 9007199254740993, Summary: "summary", Evidence: []core.Evidence{{Name: "test", Reference: "fixture://passed", Passed: true}, {Name: "failed", Reference: "fixture://failed"}}}},
		{Kind: "task.accept/task", Input: struct{}{}},
		{Kind: "workspace.cleanup/attempt", Input: struct{}{}},
	}
	for _, raw := range []string{`null`, ` { "z": 1e+01, "a": 1.000000000000000001, "big":9007199254740993, "text":"<>&  ", "escaped":"\u003C\/\\\"" } `, `[true, null, {"b":2,"a":1}]`, `"\n\t<>&  "`} {
		cases = append(cases, engineRequest{Kind: "decision.reply/decision", Input: core.ReplyDecision{Answer: json.RawMessage(raw)}})
	}
	for index, request := range cases {
		body, err := json.Marshal(request.Input)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(append([]byte(request.Kind+"\n"), body...))
		output, err := rustEngine(t, request, "journal", "")
		if err != nil {
			t.Fatalf("case %d: %v: %s", index, err, output)
		}
		var got struct{ Body, Fingerprint string }
		if err = json.Unmarshal(output, &got); err != nil {
			t.Fatal(err)
		}
		if got.Body != string(body) || got.Fingerprint != hex.EncodeToString(hash[:]) {
			t.Fatalf("case %d: Go body %s, Rust body %s; hashes %s / %s", index, body, got.Body, hex.EncodeToString(hash[:]), got.Fingerprint)
		}
	}
}

type oracleWorkspaces struct{}

func (oracleWorkspaces) Plan(p core.Project, attempt string) core.Workspace {
	return core.Workspace{Owner: attempt, Path: "/synthetic/" + attempt, Branch: attempt, BaseRef: p.BaseRef}
}
func (oracleWorkspaces) Ensure(_ context.Context, _ core.Project, w core.Workspace) (core.Workspace, error) {
	w.Allocated = true
	return w, nil
}
func (oracleWorkspaces) Inspect(context.Context, core.Project, core.Workspace) error { return nil }
func (oracleWorkspaces) Remove(context.Context, core.Project, core.Workspace) error  { return nil }

func TestRustEngineReplaysActualGoOperationsAndGoReplaysRustCreate(t *testing.T) {
	_ = requireRustEngine(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	runtime := testruntime.New()
	newEngine := func() *core.Engine {
		return core.New(db, runtime, oracleWorkspaces{}, core.Options{RuntimeID: "fake", RegisterProject: func(_ context.Context, input core.RegisterProject) (core.Project, error) {
			return core.Project{Name: input.Name, Path: input.Path, BaseRef: input.BaseRef, Instructions: input.Instructions}, nil
		}})
	}
	engine := newEngine()
	var requests []engineRequest
	register := core.RegisterProject{Name: "Go oracle <&>  ", Path: "/synthetic/project", BaseRef: "master"}
	project, err := engine.Register(ctx, "register", register)
	if err != nil {
		t.Fatal(err)
	}
	requests = append(requests, engineRequest{Kind: "project.register", Operation: "register", Input: register})
	create := core.CreateTask{ProjectID: project.ID, Title: "task", Instructions: "original <&> 世界"}
	task, err := engine.Create(ctx, "create", create)
	if err != nil {
		t.Fatal(err)
	}
	requests = append(requests, engineRequest{Kind: "task.create", Operation: "create", Input: create})
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	follow := core.Followup{Text: "additional instruction"}
	if _, err = engine.Followup(ctx, "follow", task.ID, follow); err != nil {
		t.Fatal(err)
	}
	requests = append(requests, engineRequest{Kind: "task.followup/" + task.ID, Operation: "follow", Target: task.ID, Input: follow})
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := db.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attempt := state.Attempts[state.Tasks[task.ID].AttemptID]
	runtime.SetDecisions(attempt.SessionRef, core.Decision{RuntimeRef: "question", Kind: "question", Prompt: "answer?"})
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	state, err = db.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var decision core.Decision
	for _, d := range state.Decisions {
		decision = d
	}
	reply := core.ReplyDecision{Answer: json.RawMessage(` { "z": 1e+01, "a": 9007199254740993, "null":null, "text":"<&>" } `)}
	if _, err = engine.Reply(ctx, "reply", decision.ID, reply); err != nil {
		t.Fatal(err)
	}
	requests = append(requests, engineRequest{Kind: "decision.reply/" + decision.ID, Operation: "reply", Target: decision.ID, Input: reply})
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	control := core.Control{Action: "pause"}
	if _, err = engine.Control(ctx, "pause", task.ID, control); err != nil {
		t.Fatal(err)
	}
	requests = append(requests, engineRequest{Kind: "task.control/" + task.ID, Operation: "pause", Target: task.ID, Input: control})
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	control.Action = "resume"
	if _, err = engine.Control(ctx, "resume", task.ID, control); err != nil {
		t.Fatal(err)
	}
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	state, err = db.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result := core.SubmitResult{AttemptID: attempt.ID, Generation: state.Tasks[task.ID].Generation, Summary: "actual oracle", Evidence: []core.Evidence{{Name: "check", Reference: "fixture://assertion", Passed: true}}}
	if _, err = engine.SubmitResult(ctx, "result", task.ID, result); err != nil {
		t.Fatal(err)
	}
	requests = append(requests, engineRequest{Kind: "task.result/" + task.ID, Operation: "result", Target: task.ID, Input: result})
	runtime.SetState(attempt.SessionRef, "idle")
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Accept(ctx, "accept", task.ID); err != nil {
		t.Fatal(err)
	}
	requests = append(requests, engineRequest{Kind: "task.accept/" + task.ID, Operation: "accept", Target: task.ID, Input: struct{}{}})
	if _, err = engine.Cleanup(ctx, "cleanup", attempt.ID); err != nil {
		t.Fatal(err)
	}
	requests = append(requests, engineRequest{Kind: "workspace.cleanup/" + attempt.ID, Operation: "cleanup", Target: attempt.ID, Input: struct{}{}})
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := db.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	rows := storeRows(t, dir)
	for _, request := range requests {
		output, err := rustEngine(t, request, "call", dir)
		if err != nil {
			t.Fatalf("replay %s: %v: %s", request.Kind, err, output)
		}
		var got, want any
		if err = json.Unmarshal(output, &got); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(before.Operations[request.Operation].Outcome, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("replay %s returned current state instead of old outcome", request.Kind)
		}
		if !bytes.Equal(bytes.TrimSpace(output), before.Operations[request.Operation].Outcome) {
			t.Fatalf("replay %s changed original receipt bytes", request.Kind)
		}
	}
	if !reflect.DeepEqual(storeRows(t, dir), rows) {
		t.Fatal("replay rewrote persisted rows or history")
	}
	conflict := requests[1]
	conflict.Input = core.CreateTask{ProjectID: project.ID, Title: "different", Instructions: "different"}
	output, err := rustEngine(t, conflict, "call", dir)
	if err == nil || !strings.Contains(string(output), `"code":"operation_conflict"`) {
		t.Fatal("Rust did not preserve Go operation conflicts", err, string(output))
	}
	create.Title = "Rust created <&>"
	request := engineRequest{Kind: "task.create", Operation: "rust-create", Input: create}
	output, err = rustEngine(t, request, "call", dir)
	if err != nil {
		t.Fatal(err, string(output))
	}
	var rustTask core.Task
	if err = json.Unmarshal(output, &rustTask); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	engine = newEngine()
	replayed, err := engine.Create(ctx, request.Operation, create)
	if err != nil || !reflect.DeepEqual(replayed, rustTask) {
		t.Fatal("Go could not replay actual Rust admission", err)
	}
	state, err = db.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for key, old := range before.Operations {
		if !reflect.DeepEqual(old, state.Operations[key]) {
			t.Fatalf("receipt %s changed", key)
		}
	}
}

func TestRustEngineGoDecisionAndCoordinatorSignalOracle(t *testing.T) {
	_ = requireRustEngine(t)
	for _, childState := range []string{"ready_for_review", "waiting_for_input"} {
		t.Run(childState, func(t *testing.T) {
			ctx := context.Background()
			stamp := time.Date(2026, 10, 26, 1, 2, 3, 123, time.UTC)
			seed := func(s *core.State) error {
				s.Projects["project"] = core.Project{ID: "project", Name: "fixture", Path: "/synthetic/project", BaseRef: "master", CreatedAt: stamp}
				for _, id := range []string{"parent", "child"} {
					role, parent, state := "coordinator", "", "running"
					if id == "child" {
						role, parent, state = "worker", "parent", childState
					}
					s.Tasks[id] = core.Task{ID: id, ProjectID: "project", ParentID: parent, Dependencies: []string{}, Title: id, Instructions: "fixture <&>  ", Role: role, Desired: "active", State: state, Generation: 1, AttemptID: "attempt-" + id, CreatedAt: stamp, UpdatedAt: stamp}
					s.Attempts["attempt-"+id] = core.Attempt{ID: "attempt-" + id, TaskID: id, RuntimeID: "fake", SessionRef: "session-" + id, Phase: "executing", Observed: "idle", Generation: 1, Workspace: core.Workspace{Owner: "attempt-" + id, Path: "/synthetic/" + id, Allocated: true}, CreatedAt: stamp, UpdatedAt: stamp}
				}
				s.Results["attempt-child/1"] = core.Result{TaskID: "child", AttemptID: "attempt-child", Generation: 1, Summary: "Evidence <&> 世界  ", Evidence: []core.Evidence{{Name: "test", Reference: "fixture://test", Passed: true}}, CreatedAt: stamp}
				return nil
			}
			dir := filepath.Join(t.TempDir(), "go-state")
			rustDir := filepath.Join(t.TempDir(), "rust-state")
			db, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			rustDB, err := store.Open(rustDir)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.Update(ctx, seed); err != nil {
				t.Fatal(err)
			}
			if err = rustDB.Update(ctx, seed); err != nil {
				t.Fatal(err)
			}
			if err = rustDB.Close(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := db.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			runtime := testruntime.New()
			observations := map[string]any{}
			for _, attempt := range snapshot.Attempts {
				task := snapshot.Tasks[attempt.TaskID]
				spec := core.SessionSpec{Reference: attempt.SessionRef, TaskID: task.ID, AttemptID: attempt.ID, InstanceID: snapshot.InstanceID, Directory: attempt.Workspace.Path, Role: task.Role, Title: task.Title}
				if err = runtime.Ensure(ctx, spec); err != nil {
					t.Fatal(err)
				}
				decisions := []core.Decision{}
				if task.ID == "child" && childState == "waiting_for_input" {
					decisions = append(decisions, core.Decision{RuntimeRef: "request-<&>", Kind: "question", Prompt: "Choose <&> 世界  ", Details: json.RawMessage(`{"z":9007199254740993,"a":1e+01}`)})
				}
				runtime.SetDecisions(spec.Reference, decisions...)
				observations[spec.Reference] = map[string]any{"exists": true, "owned": true, "state": "idle", "decisions": decisions}
			}
			engine := core.New(db, runtime, oracleWorkspaces{}, core.Options{RuntimeID: "fake"})
			if err = engine.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			goState, err := db.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			output, err := rustEngine(t, engineRequest{Kind: "observe", Input: observations}, "observe", rustDir)
			if err != nil {
				t.Fatal(err, string(output))
			}
			var rustState core.State
			if err = json.Unmarshal(output, &rustState); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(goState.Commands, rustState.Commands) {
				t.Fatalf("notification identities/body/budget differ\nGo: %#v\nRust: %#v", goState.Commands, rustState.Commands)
			}
			if !reflect.DeepEqual(goState.Decisions, rustState.Decisions) {
				t.Fatal("decision identities or raw details differ")
			}
			if rustState.Tasks["parent"].Generation != 2 || len(rustState.Commands) != 2 {
				t.Fatal("oracle did not independently produce one signal and notification")
			}
			creates, prompts, _ := runtime.Counts()
			if creates != 2 || prompts != 0 {
				t.Fatal("read-only oracle made an unexpected mutation")
			}
		})
	}
}
