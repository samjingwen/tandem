package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/store"
	"github.com/samjingwen/tandem/internal/testruntime"
	"github.com/samjingwen/tandem/internal/workspace"
)

var ctx = context.Background()

type fixture struct {
	e   *core.Engine
	s   *store.SQLite
	r   *testruntime.Fake
	w   workspace.Git
	p   core.Project
	dir string
}

func setup(t *testing.T, options core.Options) *fixture {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "master"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "fixture"}} {
		if _, err := workspace.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := testruntime.New()
	w := workspace.Git{Root: filepath.Join(dir, "worktrees")}
	options.RuntimeID = "fake"
	options.RegisterProject = workspace.Register
	e := core.New(s, r, w, options)
	p, err := e.Register(ctx, "register", core.RegisterProject{Name: "fixture", Path: repo, BaseRef: "master"})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{e: e, s: s, r: r, w: w, p: p, dir: dir}
}
func (f *fixture) task(t *testing.T, op string) core.Task {
	t.Helper()
	v, err := f.e.Create(ctx, op, core.CreateTask{ProjectID: f.p.ID, Title: op, Instructions: "Implement the requested change and verify it."})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func (f *fixture) tick(t *testing.T) {
	t.Helper()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) state(t *testing.T) core.State {
	t.Helper()
	s, err := f.e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConcurrentDuplicateSubmission(t *testing.T) {
	f := setup(t, core.Options{})
	input := core.CreateTask{ProjectID: f.p.ID, Title: "same", Instructions: "same request"}
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); v, err := f.e.Create(ctx, "same-op", input); ids <- v.ID; errs <- err }()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ""
	for id := range ids {
		if first != "" && first != id {
			t.Fatal("duplicate tasks")
		}
		first = id
	}
	f.tick(t)
	s := f.state(t)
	if len(s.Tasks) != 1 || len(s.Attempts) != 1 {
		t.Fatalf("duplicate state: %+v", s.Tasks)
	}
	creates, prompts, _ := f.r.Counts()
	if creates != 1 || prompts != 1 {
		t.Fatal(creates, prompts)
	}
	input.Title = "conflict"
	if _, err := f.e.Create(ctx, "same-op", input); err == nil {
		t.Fatal("conflicting ID reuse accepted")
	}
}

func TestRestartReattachesExistingWorker(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "restart")
	f.tick(t)
	before := f.state(t)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(filepath.Join(f.dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.s = reopened
	f.e = core.New(reopened, f.r, f.w, core.Options{RuntimeID: "fake"})
	f.tick(t)
	after := f.state(t)
	id := before.Tasks[task.ID].AttemptID
	if after.Tasks[task.ID].AttemptID != id || after.Attempts[id].Workspace != before.Attempts[id].Workspace {
		t.Fatal("restart changed binding")
	}
	creates, prompts, _ := f.r.Counts()
	if creates != 1 || prompts != 1 {
		t.Fatal("restart duplicated worker", creates, prompts)
	}
}

func TestCrashAfterSessionCreationRecoversExactReference(t *testing.T) {
	crash := true
	f := setup(t, core.Options{AfterCreate: func(core.Attempt) error {
		if crash {
			return errors.New("injected process crash")
		}
		return nil
	}})
	f.task(t, "crash")
	f.tick(t)
	s := f.state(t)
	for _, a := range s.Attempts {
		if a.Phase != "session_pending" {
			t.Fatal(a.Phase)
		}
	}
	crash = false
	f.e = core.New(f.s, f.r, f.w, core.Options{RuntimeID: "fake"})
	f.tick(t)
	creates, prompts, _ := f.r.Counts()
	if creates != 1 || prompts != 1 {
		t.Fatal(creates, prompts)
	}
}

func TestLostCreateAndPromptResponsesAreIdempotentlyReconciled(t *testing.T) {
	f := setup(t, core.Options{})
	f.r.LoseCreateResponse = true
	task := f.task(t, "loss")
	f.tick(t)
	if f.state(t).Tasks[task.ID].State != "recovering" {
		t.Fatal("missing uncertainty")
	}
	f.r.LosePromptResponse = true
	f.tick(t)
	f.tick(t)
	creates, prompts, _ := f.r.Counts()
	if creates != 1 || prompts != 1 {
		t.Fatal(creates, prompts)
	}
}

func TestDroppedEventsConvergeWithoutReplay(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "events")
	f.tick(t)
	s := f.state(t)
	ref := s.Attempts[s.Tasks[task.ID].AttemptID].SessionRef
	f.r.SetState(ref, "failed")
	f.r.Wake <- struct{}{}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.e.Run(runCtx, 10*time.Millisecond) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(3 * time.Second)
	for {
		if f.state(t).Tasks[task.ID].State == "failed" {
			return
		}
		select {
		case <-deadline:
			t.Fatal("did not converge after event loss")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestDifferentOwnedWorktreesAndBoundedConcurrency(t *testing.T) {
	f := setup(t, core.Options{Concurrency: 2})
	for _, id := range []string{"one", "two", "three"} {
		f.task(t, id)
	}
	f.tick(t)
	s := f.state(t)
	if len(s.Attempts) != 2 {
		t.Fatal("concurrency bound", len(s.Attempts))
	}
	seen := map[string]bool{}
	for _, a := range s.Attempts {
		if seen[a.Workspace.Path] {
			t.Fatal("shared workspace")
		}
		seen[a.Workspace.Path] = true
		if err := f.w.Inspect(ctx, f.p, a.Workspace); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPauseBeforeDispatchAndResumeReconcile(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "pause")
	if _, err := f.e.Control(ctx, "pause-op", task.ID, core.Control{Action: "pause"}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	creates, _, _ := f.r.Counts()
	if creates != 0 {
		t.Fatal("paused task launched")
	}
	if _, err := f.e.Control(ctx, "resume-op", task.ID, core.Control{Action: "resume"}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	s := f.state(t)
	attempt := s.Tasks[task.ID].AttemptID
	if _, err := f.e.Control(ctx, "pause-running", task.ID, core.Control{Action: "pause"}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if f.state(t).Tasks[task.ID].State != "paused" {
		t.Fatal("pause unconfirmed")
	}
	if _, err := f.e.Control(ctx, "resume-running", task.ID, core.Control{Action: "resume"}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if f.state(t).Tasks[task.ID].AttemptID != attempt {
		t.Fatal("resume replaced existing attempt")
	}
	creates, _, interrupts := f.r.Counts()
	if creates != 1 || interrupts != 1 {
		t.Fatal(creates, interrupts)
	}
}

func TestCancelRacesWithInflightCreationAndRetainsLateResult(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "cancel")
	entered := make(chan struct{})
	release := make(chan struct{})
	f.r.BeforeCreate = func() { close(entered); <-release }
	tickDone := make(chan error, 1)
	go func() { tickDone <- f.e.Tick(ctx) }()
	<-entered
	cancelDone := make(chan error, 1)
	go func() {
		_, err := f.e.Control(ctx, "cancel-op", task.ID, core.Control{Action: "cancel"})
		cancelDone <- err
	}()
	close(release)
	if err := <-tickDone; err != nil {
		t.Fatal(err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	f.r.BeforeCreate = nil
	f.tick(t)
	s := f.state(t)
	if s.Tasks[task.ID].State != "cancelled" {
		t.Fatal("not cancelled")
	}
	a := s.Attempts[s.Tasks[task.ID].AttemptID]
	r, err := f.e.SubmitResult(ctx, "late-result", task.ID, core.SubmitResult{AttemptID: a.ID, Generation: 1, Summary: "late work", Evidence: []core.Evidence{{Name: "test", Reference: "retained artifact", Passed: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Late || f.state(t).Tasks[task.ID].State != "cancelled" {
		t.Fatal("late result revived task")
	}
	creates, _, _ := f.r.Counts()
	f.tick(t)
	after, _, _ := f.r.Counts()
	if after != creates {
		t.Fatal("cancel redispatched")
	}
}

func TestIdleIsNotCompletionAndAcceptanceNeedsEvidence(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "idle")
	f.tick(t)
	s := f.state(t)
	a := s.Attempts[s.Tasks[task.ID].AttemptID]
	f.r.SetState(a.SessionRef, "idle")
	f.tick(t)
	if f.state(t).Tasks[task.ID].State != "running" {
		t.Fatal("idle inferred completion")
	}
	if _, err := f.e.Accept(ctx, "accept-no-result", task.ID); err == nil {
		t.Fatal("accepted without evidence")
	}
	if _, err := f.e.SubmitResult(ctx, "result", task.ID, core.SubmitResult{AttemptID: a.ID, Generation: 1, Summary: "done", Evidence: []core.Evidence{{Name: "unit tests", Reference: "test-log", Passed: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Accept(ctx, "accept", task.ID); err != nil {
		t.Fatal(err)
	}
	if f.state(t).Tasks[task.ID].State != "completed" {
		t.Fatal("not completed")
	}
}

func TestPendingDecisionIsDurableAndOwnershipChecked(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "decision")
	f.tick(t)
	s := f.state(t)
	a := s.Attempts[s.Tasks[task.ID].AttemptID]
	f.r.SetDecisions(a.SessionRef, core.Decision{Kind: "permission", RuntimeRef: "permission-1", Prompt: "allow edit?"})
	f.tick(t)
	s = f.state(t)
	if len(s.Decisions) != 1 || s.Tasks[task.ID].State != "waiting_for_input" {
		t.Fatal("decision not exposed")
	}
	other := core.New(f.s, f.r, f.w, core.Options{RuntimeID: "fake"})
	for id := range s.Decisions {
		if _, err := other.Reply(ctx, "reply", id, core.ReplyDecision{Answer: json.RawMessage(`{"reply":"reject"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	f.tick(t)
	for _, d := range f.state(t).Decisions {
		if d.Status != "answered" {
			t.Fatal(d.Status)
		}
	}
}

func TestDirtyAndUnownedWorktreesRefuseCleanup(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "cleanup")
	f.tick(t)
	s := f.state(t)
	a := s.Attempts[s.Tasks[task.ID].AttemptID]
	if err := os.WriteFile(filepath.Join(a.Workspace.Path, "valuable.txt"), []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.w.Remove(ctx, f.p, a.Workspace); err == nil {
		t.Fatal("dirty worktree removed")
	}
	a.Workspace.Owner = "someone-else"
	if err := f.w.Remove(ctx, f.p, a.Workspace); err == nil {
		t.Fatal("unowned worktree removed")
	}
}

func TestCoordinatorDelegationDependenciesAndParentPause(t *testing.T) {
	f := setup(t, core.Options{Concurrency: 2})
	parent, err := f.e.Create(ctx, "coordinator", core.CreateTask{ProjectID: f.p.ID, Role: "coordinator", Title: "Coordinate", Instructions: "Delegate two independent changes"})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"delegated-one", "delegated-two"} {
		if _, err = f.e.Create(ctx, op, core.CreateTask{ProjectID: f.p.ID, ParentID: parent.ID, Title: op, Instructions: "Implement and verify"}); err != nil {
			t.Fatal(err)
		}
	}
	f.tick(t)
	if len(f.state(t).Attempts) != 3 {
		t.Fatal("delegation not bounded separately")
	}
	if _, err = f.e.Control(ctx, "pause-tree", parent.ID, core.Control{Action: "pause", Cascade: true}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	for _, v := range f.state(t).Tasks {
		if v.State != "paused" {
			t.Fatal(v.State)
		}
	}
}

func TestLostPermissionReplyDoesNotInventApproval(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "reply-loss")
	f.tick(t)
	s := f.state(t)
	a := s.Attempts[s.Tasks[task.ID].AttemptID]
	f.r.SetDecisions(a.SessionRef, core.Decision{Kind: "permission", RuntimeRef: "p", Prompt: "allow?"})
	f.tick(t)
	f.r.LoseReplyResponse = true
	for id := range f.state(t).Decisions {
		if _, err := f.e.Reply(ctx, "answer", id, core.ReplyDecision{Answer: json.RawMessage(`{"decision":"reject"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	f.tick(t)
	f.tick(t)
	for _, d := range f.state(t).Decisions {
		if d.Status != "uncertain" {
			t.Fatal("lost reply incorrectly resolved", d.Status)
		}
	}
	if f.state(t).Tasks[task.ID].State != "recovering" {
		t.Fatal("uncertainty not surfaced")
	}
}

func TestStaleResultCannotCompleteFollowup(t *testing.T) {
	f := setup(t, core.Options{})
	task := f.task(t, "stale")
	f.tick(t)
	s := f.state(t)
	a := s.Attempts[s.Tasks[task.ID].AttemptID]
	if _, err := f.e.Followup(ctx, "followup", task.ID, core.Followup{Text: "New requirements"}); err != nil {
		t.Fatal(err)
	}
	r, err := f.e.SubmitResult(ctx, "old-result", task.ID, core.SubmitResult{AttemptID: a.ID, Generation: 1, Summary: "old work", Evidence: []core.Evidence{{Name: "old tests", Reference: "old log", Passed: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Late {
		t.Fatal("stale result wasn't identified")
	}
	if _, err = f.e.Accept(ctx, "accept-stale", task.ID); err == nil {
		t.Fatal("accepted stale result")
	}
}

func TestFollowupWaitsForExecutionCapacity(t *testing.T) {
	f := setup(t, core.Options{Concurrency: 1})
	one := f.task(t, "first")
	f.tick(t)
	s := f.state(t)
	ref := s.Attempts[s.Tasks[one.ID].AttemptID].SessionRef
	f.r.SetState(ref, "idle")
	f.task(t, "second")
	f.tick(t)
	if _, err := f.e.Followup(ctx, "followup-capacity", one.ID, core.Followup{Text: "Continue"}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	_, prompts, _ := f.r.Counts()
	if prompts != 2 {
		t.Fatal("followup exceeded worker limit", prompts)
	}
}
