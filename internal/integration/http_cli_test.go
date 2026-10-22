// These tests use the real engine, SQLite store, and disposable Git worktrees.
// Only the runtime is simulated. No model, native OpenCode, or shared checkout
// is invoked or modified.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/api"
	"github.com/samjingwen/tandem/internal/cli"
	"github.com/samjingwen/tandem/internal/client"
	"github.com/samjingwen/tandem/internal/store"
	"github.com/samjingwen/tandem/internal/testruntime"
	"github.com/samjingwen/tandem/internal/workspace"
)

type fixture struct {
	ctx         context.Context
	dir         string
	repo        string
	store       *store.SQLite
	engine      *core.Engine
	runtime     *testruntime.Fake
	workspace   workspace.Git
	server      *httptest.Server
	clients     [2]*client.Client
	runners     [2]cli.Runner
	project     core.Project
	concurrency int
}

func setup(t *testing.T, concurrency ...int) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	f := &fixture{ctx: ctx, dir: t.TempDir(), runtime: testruntime.New()}
	f.concurrency = 2
	if len(concurrency) > 0 {
		f.concurrency = concurrency[0]
	}
	f.repo = filepath.Join(f.dir, "repository")
	if err := os.Mkdir(f.repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "master"},
		{"config", "core.hooksPath", "/dev/null"},
		{"-c", "user.name=HTTP fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture"},
	} {
		if _, err := workspace.Run(ctx, f.repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	f.workspace = workspace.Git{Root: filepath.Join(f.dir, "worktrees")}
	t.Cleanup(func() {
		if f.server != nil {
			f.server.Close()
		}
		if f.store != nil {
			_ = f.store.Close()
		}
	})
	f.open(t)
	var project core.Project
	f.run(t, 0, &project, "projects", "register", "--name", "fixture", "--path", f.repo, "--base-ref", "master", "--operation-id", "register")
	f.project = project
	return f
}

func (f *fixture) open(t *testing.T) {
	t.Helper()
	s, err := store.Open(filepath.Join(f.dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	f.store = s
	f.engine = core.New(s, f.runtime, f.workspace, core.Options{RuntimeID: "fake", RegisterProject: workspace.Register, Concurrency: f.concurrency})
	f.server = httptest.NewServer(api.New(f.engine, api.Options{Token: "integration-test-token"}))
	for i := range f.clients {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(transport.CloseIdleConnections)
		httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		f.clients[i], err = client.New(client.Options{URL: f.server.URL, Token: "integration-test-token", HTTPClient: httpClient})
		if err != nil {
			t.Fatal(err)
		}
		serverURL := f.server.URL
		f.runners[i] = cli.Runner{HTTPClient: httpClient, Getenv: func(name string) string {
			if name == "TANDEM_URL" {
				return serverURL
			}
			if name == "TANDEM_TOKEN" {
				return "integration-test-token"
			}
			return ""
		}}
	}
}

func (f *fixture) run(t *testing.T, index int, output any, args ...string) {
	t.Helper()
	var out, receipt bytes.Buffer
	if err := f.runners[index].Run(f.ctx, append(args, "--json"), &out, &receipt); err != nil {
		t.Fatalf("CLI %v: %v; receipt=%s", args, err, receipt.String())
	}
	if err := json.Unmarshal(out.Bytes(), output); err != nil {
		t.Fatalf("CLI response %s: %v", out.String(), err)
	}
}

func (f *fixture) do(t *testing.T, index int, method, path, op string, input, output any) {
	t.Helper()
	if err := f.clients[index].Do(f.ctx, method, path, op, input, output); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
}

func (f *fixture) tick(t *testing.T) {
	t.Helper()
	if err := f.engine.Tick(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) snapshot(t *testing.T, index int) core.State {
	t.Helper()
	var state core.State
	f.run(t, index, &state, "snapshot")
	return state
}

func (f *fixture) task(t *testing.T) (core.Task, core.Attempt) {
	t.Helper()
	var task core.Task
	f.run(t, 0, &task, "tasks", "create", "--project", f.project.ID, "--title", "Verify HTTP coordination", "--instructions", "Implement and report evidence.", "--operation-id", "create")
	if len(f.snapshot(t, 1).Attempts) != 0 {
		t.Fatal("CLI unexpectedly started a scheduler")
	}
	f.tick(t)
	state := f.snapshot(t, 1)
	task = state.Tasks[task.ID]
	return task, state.Attempts[task.AttemptID]
}

func (f *fixture) result(t *testing.T, task core.Task, attempt core.Attempt, generation int, op string) core.Result {
	t.Helper()
	var result core.Result
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/results", op, core.SubmitResult{AttemptID: attempt.ID, Generation: generation, Summary: "Verified work", Evidence: []core.Evidence{{Name: "tests", Reference: "artifact://verification", Passed: true}}}, &result)
	return result
}

func TestCurrentResultUsesAttemptGeneration(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	f.result(t, task, attempt, task.Generation, "result")
	var result core.Result
	f.run(t, 1, &result, "tasks", "result", task.ID)
	if result.AttemptID != attempt.ID || result.Generation != task.Generation {
		t.Fatalf("wrong current result: %+v", result)
	}
	var detail api.TaskDetail
	f.run(t, 1, &detail, "tasks", "show", task.ID)
	if detail.Result == nil || detail.Result.Generation != task.Generation || detail.Task.State != "ready_for_review" {
		t.Fatalf("result/status absent from task detail: %+v", detail)
	}
}

func TestProjectRefreshKeepsIdentityAndPassesExplicitSharedPolicy(t *testing.T) {
	f := setup(t)
	policy := filepath.Join(f.dir, "shared-policy.md")
	if err := os.WriteFile(policy, []byte("Fixture shared policy: preserve the original checkout and run compiler checks.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var refreshed core.Project
	f.run(t, 0, &refreshed, "projects", "register", "--name", "refreshed fixture", "--path", f.repo, "--base-ref", "master", "--instruction-files", policy, "--operation-id", "refresh-project")
	if refreshed.ID != f.project.ID || !refreshed.CreatedAt.Equal(f.project.CreatedAt) || len(refreshed.InstructionFiles) != 1 {
		t.Fatal("configuration refresh changed project identity")
	}
	_, attempt := f.task(t)
	f.runtime.Mu.Lock()
	session := f.runtime.Sessions[attempt.SessionRef]
	delivered := false
	for _, prompt := range session.Prompts {
		if strings.Contains(prompt, "Fixture shared policy: preserve the original checkout and run compiler checks.") {
			delivered = true
		}
	}
	f.runtime.Mu.Unlock()
	// The emitted runtime prompt is the intentional data-transfer interface. This
	// assertion does not claim that a model followed the policy.
	if !delivered {
		t.Fatal("shared policy content was not delivered to the worker")
	}
	private := filepath.Join(f.repo, ".env")
	if err := os.WriteFile(private, []byte("FIXTURE_SECRET=never-copy-this-fixture-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.clients[0].Do(f.ctx, http.MethodPost, "/v1/projects", "reject-env-instruction", core.RegisterProject{Name: "fixture", Path: f.repo, BaseRef: "master", InstructionFiles: []string{private}}, nil); err == nil {
		t.Fatal("environment file accepted as instructions")
	}
}

func TestTaskCleanupResolvesOwnedAttempt(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	f.runtime.SetState(attempt.SessionRef, "idle")
	f.tick(t)
	f.result(t, task, attempt, task.Generation, "result")
	var accepted core.Task
	f.run(t, 1, &accepted, "tasks", "accept", task.ID, "--operation-id", "accept")
	artifact := filepath.Join(attempt.Workspace.Path, "valuable.txt")
	if err := os.WriteFile(artifact, []byte("retain until explicitly resolved"), 0600); err != nil {
		t.Fatal(err)
	}
	var receipt core.Workspace
	f.do(t, 1, http.MethodPost, "/v1/tasks/"+task.ID+"/cleanup", "cleanup", nil, &receipt)
	if !receipt.Allocated {
		t.Fatal("request claimed cleanup before reconciliation")
	}
	f.tick(t)
	refused := f.snapshot(t, 1).Attempts[attempt.ID]
	if refused.Phase != "cleanup_pending" || refused.Error == "" {
		t.Fatal("dirty cleanup was not visibly refused", refused)
	}
	if data, err := os.ReadFile(artifact); err != nil || string(data) != "retain until explicitly resolved" {
		t.Fatalf("refused cleanup lost fixture data: %q (%v)", data, err)
	}
	if err := os.Remove(artifact); err != nil { // Only this test's disposable file.
		t.Fatal(err)
	}
	var cleaned core.Workspace
	f.run(t, 1, &cleaned, "tasks", "cleanup", task.ID, "--operation-id", "cleanup")
	if cleaned != receipt || cleaned.Owner != attempt.ID {
		t.Fatalf("cleanup affected wrong ownership: %+v", cleaned)
	}
	f.tick(t)
	observed := f.snapshot(t, 1).Attempts[attempt.ID]
	if observed.Phase != "cleaned" || observed.Workspace.Allocated {
		t.Fatal("cleanup did not converge", observed)
	}
	if _, err := os.Stat(attempt.Workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("clean checkout still exists: %v", err)
	}
	if _, err := workspace.Run(f.ctx, f.repo, "rev-parse", "--verify", "refs/heads/"+attempt.Workspace.Branch); err != nil {
		t.Fatal("cleanup deleted retained branch", err)
	}
	// Exact retry must return the same core-owned receipt without touching Git.
	f.run(t, 0, &cleaned, "tasks", "cleanup", task.ID, "--operation-id", "cleanup")
}

func TestCLIResultRequiresExplicitGeneration(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	var result core.Result
	f.run(t, 1, &result, "tasks", "result", task.ID, "--attempt", attempt.ID, "--generation", "1", "--summary", "verified", "--evidence", `[{"name":"test","reference":"artifact://result","passed":true}]`, "--operation-id", "cli-result")
	if result.Generation != 1 || result.Late {
		t.Fatalf("CLI omitted generation: %+v", result)
	}
}

func TestRetryCannotForkAnAlreadyResumedFailedAttempt(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	f.runtime.SetState(attempt.SessionRef, "failed")
	f.tick(t)
	var followed core.Task
	f.run(t, 0, &followed, "tasks", "followup", task.ID, "--text", "Correct the failure in this existing attempt", "--operation-id", "followup-failed")
	f.tick(t)
	var retry core.Task
	if err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "unsafe-retry", core.Control{Action: "retry"}, &retry); err == nil {
		t.Fatal("retry forked a failed attempt whose continuation was already running")
	}
	f.tick(t)
	snapshot := f.snapshot(t, 1)
	if len(snapshot.Attempts) != 1 || snapshot.Tasks[task.ID].AttemptID != attempt.ID {
		t.Fatal("continuation was replaced by a duplicate worker")
	}
}

func TestIndependentClientsDuplicateCreateDisconnectAndRestart(t *testing.T) {
	f := setup(t)
	type outcome struct {
		task core.Task
		err  error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	input := core.CreateTask{ProjectID: f.project.ID, Title: "one request", Instructions: "one worker"}
	go func() {
		<-start
		var out, receipt bytes.Buffer
		err := f.runners[0].Run(f.ctx, []string{"tasks", "create", "--project", f.project.ID, "--title", input.Title, "--instructions", input.Instructions, "--operation-id", "concurrent-create", "--json"}, &out, &receipt)
		var task core.Task
		if err == nil {
			err = json.Unmarshal(out.Bytes(), &task)
		}
		results <- outcome{task, err}
	}()
	go func() {
		<-start
		var task core.Task
		err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks", "concurrent-create", input, &task)
		results <- outcome{task, err}
	}()
	close(start)
	one, two := <-results, <-results
	if one.err != nil || two.err != nil || one.task.ID != two.task.ID || one.task.ID == "" {
		t.Fatalf("concurrent duplicate create: %+v, %+v", one, two)
	}
	f.tick(t)
	before := f.snapshot(t, 0)
	if len(before.Tasks) != 1 || len(before.Attempts) != 1 {
		t.Fatalf("duplicate allocation: %+v", before)
	}
	attempt := before.Attempts[before.Tasks[one.task.ID].AttemptID]
	if err := f.workspace.Inspect(f.ctx, f.project, attempt.Workspace); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(attempt.Workspace.Path, "retained.txt")
	if err := os.WriteFile(artifact, []byte("worker artifact survives client/server loss"), 0600); err != nil {
		t.Fatal(err)
	}
	// Closing a client stream is not a worker interrupt or completion signal.
	streamCtx, cancel := context.WithCancel(f.ctx)
	r, err := http.NewRequestWithContext(streamCtx, http.MethodGet, f.server.URL+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer integration-test-token")
	stream, err := f.runners[0].HTTPClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := io.ReadFull(stream.Body, buffer); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = stream.Body.Close()
	f.runners[0].HTTPClient.CloseIdleConnections()
	if got := f.snapshot(t, 1).Tasks[one.task.ID]; got.State != "running" || got.AttemptID != attempt.ID {
		t.Fatalf("client disconnect affected task: %+v", got)
	}
	f.server.Close()
	f.server = nil
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = nil
	f.open(t)
	f.tick(t)
	after := f.snapshot(t, 1)
	if after.InstanceID != before.InstanceID || after.Tasks[one.task.ID].AttemptID != attempt.ID || after.Attempts[attempt.ID].SessionRef != attempt.SessionRef || after.Attempts[attempt.ID].Workspace != attempt.Workspace {
		t.Fatalf("restart changed durable binding: before=%+v after=%+v", attempt, after.Attempts[attempt.ID])
	}
	if err := f.workspace.Inspect(f.ctx, f.project, after.Attempts[attempt.ID].Workspace); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(artifact); err != nil || string(data) != "worker artifact survives client/server loss" {
		t.Fatalf("restart lost retained worktree data: %q (%v)", data, err)
	}
	creates, prompts, interrupts := f.runtime.Counts()
	if creates != 1 || prompts != 1 || interrupts != 0 {
		t.Fatalf("client/server restart mutated runtime: %d creates, %d prompts, %d interrupts", creates, prompts, interrupts)
	}
	var replay core.Task
	f.do(t, 0, http.MethodPost, "/v1/tasks", "concurrent-create", input, &replay)
	if replay.ID != one.task.ID || len(f.snapshot(t, 1).Attempts) != 1 {
		t.Fatal("reopened SQLite lost operation receipt")
	}
}

func TestAnotherClientAnswersDecisionAndAcceptsCurrentGeneration(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	f.runtime.SetDecisions(attempt.SessionRef, core.Decision{RuntimeRef: "runtime/permission/1", Kind: "permission", Prompt: "Allow fixture edit?"})
	f.tick(t)
	var decisions []core.Decision
	f.run(t, 0, &decisions, "decisions", "list", "--task", task.ID, "--status", "pending")
	if len(decisions) != 1 || !strings.HasPrefix(decisions[0].ID, "dec_") || strings.Contains(decisions[0].ID, "/") {
		t.Fatalf("decision ID is not a safe HTTP segment: %+v", decisions)
	}
	if f.snapshot(t, 1).Tasks[task.ID].State != "waiting_for_input" {
		t.Fatal("waiting decision absent from API status")
	}
	var reply core.Decision
	f.run(t, 1, &reply, "decisions", "reply", decisions[0].ID, "--answer", `{"decision":"reject"}`, "--operation-id", "reply-other-client")
	if reply.Status != "reply_pending" {
		t.Fatal("decision intent was not persisted")
	}
	f.tick(t)
	f.tick(t)
	if f.snapshot(t, 0).Decisions[reply.ID].Status != "answered" {
		t.Fatal("second client's reply did not reach the existing runtime")
	}
	var followup core.Task
	f.run(t, 1, &followup, "tasks", "followup", task.ID, "--text", "Verify the revised requirement", "--operation-id", "followup")
	f.result(t, task, attempt, 1, "stale-result")
	var ignored core.Result
	err := f.clients[0].Do(f.ctx, http.MethodGet, "/v1/tasks/"+task.ID+"/results", "", nil, &ignored)
	if typed, ok := err.(*client.Error); !ok || typed.StatusCode != 404 {
		t.Fatalf("stale result presented as current: %v", err)
	}
	var refused core.Task
	if err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/accept", "refuse-stale", nil, &refused); err == nil {
		t.Fatal("stale generation accepted")
	}
	f.tick(t)
	f.runtime.SetState(attempt.SessionRef, "idle")
	f.tick(t)
	if f.snapshot(t, 0).Tasks[task.ID].State != "running" {
		t.Fatal("idle inferred task completion")
	}
	var current core.Result
	f.run(t, 1, &current, "tasks", "result", task.ID, "--attempt", attempt.ID, "--generation", fmt.Sprint(followup.Generation), "--summary", "Revised work verified", "--evidence", `[{"name":"test","reference":"artifact://current","passed":true}]`, "--operation-id", "current-result")
	var accepted core.Task
	f.run(t, 0, &accepted, "tasks", "accept", task.ID, "--operation-id", "accept-current")
	var detail struct {
		Task     core.Task      `json:"task"`
		Result   *core.Result   `json:"result"`
		Results  []core.Result  `json:"results"`
		Commands []core.Command `json:"commands"`
	}
	f.run(t, 1, &detail, "tasks", "show", task.ID)
	if detail.Task.State != "completed" || detail.Result == nil || !detail.Result.Accepted || detail.Result.Generation != followup.Generation || len(detail.Results) != 2 || !detail.Results[0].Late {
		t.Fatalf("current/stale history or acceptance missing: %+v", detail)
	}
	for i := 1; i < len(detail.Commands); i++ {
		if detail.Commands[i-1].Sequence > detail.Commands[i].Sequence {
			t.Fatal("API command history is not in durable sequence order")
		}
	}
}

func TestCanceledTaskRetainsLateResultsWithoutCompletion(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	var canceled core.Task
	f.run(t, 1, &canceled, "tasks", "cancel", task.ID, "--operation-id", "cancel")
	f.tick(t)
	stale := f.result(t, task, attempt, task.Generation, "late-stale")
	late := f.result(t, task, attempt, canceled.Generation, "late-current")
	if !stale.Late || !late.Late {
		t.Fatal("canceled work was not marked late")
	}
	var result core.Result
	f.run(t, 0, &result, "tasks", "result", task.ID)
	if !result.Late || result.Accepted || result.Generation != canceled.Generation {
		t.Fatalf("current identity must not erase late status: %+v", result)
	}
	if err := f.clients[0].Do(f.ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/accept", "cannot-accept-late", nil, nil); err == nil {
		t.Fatal("canceled result was accepted")
	}
	var detail api.TaskDetail
	f.run(t, 1, &detail, "tasks", "show", task.ID)
	if detail.Task.State != "cancelled" || len(detail.Results) != 2 || detail.Result == nil || !detail.Result.Late {
		t.Fatalf("canceled task/history changed: %+v", detail)
	}
	creates, prompts, interrupts := f.runtime.Counts()
	if creates != 1 || prompts != 1 || interrupts != 1 {
		t.Fatalf("late result changed execution: %d creates, %d prompts, %d interrupts", creates, prompts, interrupts)
	}
}
