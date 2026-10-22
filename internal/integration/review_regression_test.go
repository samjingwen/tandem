package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/testruntime"
)

type outcomeRuntime struct {
	*testruntime.Fake
	promptOutcome  func(context.Context) error
	replyOutcome   func(context.Context) error
	afterDelivery  func(context.Context)
	afterReconcile func(context.Context)
	prompts        []string
	replies        int
}

func (r *outcomeRuntime) Prompt(ctx context.Context, spec core.SessionSpec, id, text string) error {
	r.prompts = append(r.prompts, text)
	if r.promptOutcome != nil {
		return r.promptOutcome(ctx)
	}
	err := r.Fake.Prompt(ctx, spec, id, text)
	if err == nil && r.afterDelivery != nil {
		r.afterDelivery(ctx)
	}
	return err
}

func (r *outcomeRuntime) Reply(ctx context.Context, spec core.SessionSpec, d core.Decision, answer json.RawMessage) error {
	r.replies++
	if r.replyOutcome != nil {
		return r.replyOutcome(ctx)
	}
	err := r.Fake.Reply(ctx, spec, d, answer)
	if err == nil && r.afterDelivery != nil {
		r.afterDelivery(ctx)
	}
	return err
}

func (r *outcomeRuntime) ReconcilePrompt(ctx context.Context, spec core.SessionSpec, id, text string) error {
	err := r.Fake.ReconcilePrompt(ctx, spec, id, text)
	if err == nil && r.afterReconcile != nil {
		r.afterReconcile(ctx)
	}
	return err
}

func TestExpiredDispatchContextPreservesOutcome(t *testing.T) {
	for _, expiry := range []string{"cancel", "deadline"} {
		for _, operation := range []string{"prompt", "reply"} {
			for _, outcome := range []string{"undelivered", "rejected", "ambiguous"} {
				t.Run(expiry+"/"+operation+"/"+outcome, func(t *testing.T) {
					f := setup(t)
					task, attempt := f.task(t)
					var decision core.Decision
					if operation == "prompt" {
						f.do(t, 1, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "continue", core.Followup{Text: "Continue after preflight"}, nil)
					} else {
						f.runtime.SetDecisions(attempt.SessionRef, core.Decision{RuntimeRef: "form_expiry", Kind: "form"})
						f.tick(t)
						for _, d := range f.snapshot(t, 1).Decisions {
							decision = d
						}
						f.do(t, 1, http.MethodPost, "/v1/decisions/"+decision.ID+"/reply", "answer", core.ReplyDecision{Answer: json.RawMessage(`{"answer":{"field":"value"}}`)}, nil)
					}
					var callCtx context.Context
					var cancel context.CancelFunc
					if expiry == "cancel" {
						callCtx, cancel = context.WithCancel(f.ctx)
					} else {
						callCtx, cancel = context.WithTimeout(f.ctx, time.Second)
					}
					defer cancel()
					injected := false
					fail := func(ctx context.Context) error {
						injected = true
						if expiry == "cancel" {
							cancel()
						}
						<-ctx.Done()
						if outcome == "ambiguous" {
							return ctx.Err()
						}
						if outcome == "rejected" {
							return &core.NotDelivered{Err: core.Invalid("deterministically rejected")}
						}
						return &core.NotDelivered{Err: ctx.Err()}
					}
					r := &outcomeRuntime{Fake: f.runtime}
					if operation == "prompt" {
						r.promptOutcome = fail
					} else {
						r.replyOutcome = fail
					}
					engine := core.New(f.store, r, f.workspace, core.Options{RuntimeID: "fake"})
					if err := engine.Tick(callCtx); err == nil || !injected {
						t.Fatalf("dispatch context did not expire at the runtime boundary: %v", err)
					}
					s := f.snapshot(t, 0)
					if operation == "prompt" {
						want := "pending"
						if outcome == "ambiguous" {
							want = "sending"
						}
						for _, c := range s.Commands {
							if c.ID != "initial_"+attempt.ID && c.Kind == "prompt" && c.Status != want {
								t.Fatalf("prompt status=%s want %s", c.Status, want)
							}
						}
					} else {
						want := "reply_pending"
						if outcome == "rejected" {
							want = "pending"
						} else if outcome == "ambiguous" {
							want = "uncertain"
						}
						if got := s.Decisions[decision.ID]; got.Status != want || (outcome == "rejected" && (got.Reply != nil || got.Error == "")) {
							t.Fatalf("decision=%+v want %s", got, want)
						}
					}
					if operation != "reply" || outcome != "rejected" {
						if a := s.Attempts[attempt.ID]; a.FailureCount != 1 || a.Error == "" || s.Tasks[task.ID].State != "recovering" {
							t.Fatalf("failure bookkeeping lost: %+v", a)
						}
					} else if s.Tasks[task.ID].State != "waiting_for_input" {
						t.Fatal("rejected answer did not allow correction")
					}
					r.promptOutcome, r.replyOutcome = nil, nil
					if operation == "reply" && outcome == "rejected" {
						f.do(t, 0, http.MethodPost, "/v1/decisions/"+decision.ID+"/reply", "correct-answer", core.ReplyDecision{Answer: json.RawMessage(`{"answer":{"field":"corrected"}}`)}, nil)
					}
					for range 2 {
						if err := engine.Tick(f.ctx); err != nil {
							t.Fatal(err)
						}
					}
					s = f.snapshot(t, 1)
					creates, prompts, _ := f.runtime.Counts()
					if creates != 1 || len(s.Attempts) != 1 || s.Tasks[task.ID].AttemptID != attempt.ID {
						t.Fatal("outcome recovery replaced the existing attempt")
					}
					if operation == "prompt" {
						want := 2
						if outcome == "ambiguous" {
							want = 1
						}
						if len(r.prompts) != want || prompts != want {
							t.Fatalf("prompt mutations=%d admitted=%d want %d", len(r.prompts), prompts, want)
						}
					} else if outcome == "ambiguous" {
						if r.replies != 1 || s.Decisions[decision.ID].Status != "uncertain" {
							t.Fatal("ambiguous reply was retried")
						}
					} else if r.replies != 2 || s.Decisions[decision.ID].Status != "answered" {
						t.Fatal("proven undelivered reply did not recover")
					}
				})
			}
		}
	}
}

func TestFollowupBeforeInitialCommandCoalescesAfterCreateCrash(t *testing.T) {
	for _, viaCLI := range []bool{false, true} {
		t.Run(fmt.Sprint(viaCLI), func(t *testing.T) {
			f := setup(t)
			f.do(t, 0, http.MethodPost, "/v1/projects", "project-policy", core.RegisterProject{Name: "fixture", Path: f.repo, BaseRef: "master", Instructions: "Project policy payload"}, nil)
			var task core.Task
			f.do(t, 0, http.MethodPost, "/v1/tasks", "create", core.CreateTask{ProjectID: f.project.ID, Title: "Crash window", Instructions: "Original task context"}, &task)
			crashing := core.New(f.store, f.runtime, f.workspace, core.Options{RuntimeID: "fake", AfterCreate: func(core.Attempt) error {
				return errors.New("crash before binding commit")
			}})
			if err := crashing.Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			s := f.snapshot(t, 1)
			attempt := s.Attempts[s.Tasks[task.ID].AttemptID]
			if attempt.Phase != "session_pending" || len(s.Commands) != 0 {
				t.Fatal("did not reach session-create/binding crash window")
			}
			f.server.Close()
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			follow := func(op, text string) {
				if viaCLI {
					var got core.Task
					f.run(t, 1, &got, "tasks", "followup", task.ID, "--text", text, "--operation-id", op)
				} else {
					f.do(t, 1, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", op, core.Followup{Text: text}, nil)
				}
			}
			follow("early", "Early followup payload")
			follow("early", "Early followup payload")
			if len(f.snapshot(t, 0).Commands) != 0 {
				t.Fatal("followup created a command before the initial prompt")
			}
			r := &outcomeRuntime{Fake: f.runtime}
			engine := core.New(f.store, r, f.workspace, core.Options{RuntimeID: "fake"})
			if err := engine.Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			follow("late", "Later continuation payload")
			follow("late", "Later continuation payload")
			for range 2 {
				if err := engine.Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if len(r.prompts) != 2 {
				t.Fatalf("delivered %d prompts, want initial then continuation", len(r.prompts))
			}
			initial, continuation := r.prompts[0], r.prompts[1]
			if !strings.Contains(initial, "Project policy payload") || !strings.Contains(initial, "Instruction generation: 2") || !strings.Contains(initial, "Original task context") || strings.Index(initial, "Original task context") > strings.Index(initial, "Early followup payload") || strings.Count(initial, "Early followup payload") != 1 {
				t.Fatalf("initial prompt lost or reordered context: %s", initial)
			}
			if strings.Contains(initial, "Later continuation payload") || strings.Contains(continuation, "Early followup payload") || strings.Count(continuation, "Later continuation payload") != 1 || !strings.Contains(continuation, "Task generation 3.") {
				t.Fatalf("followup duplicated or delivered out of order: %q", r.prompts)
			}
			creates, prompts, _ := f.runtime.Counts()
			if creates != 1 || prompts != 2 || len(f.snapshot(t, 1).Attempts) != 1 {
				t.Fatal("crash recovery duplicated the worker")
			}
		})
	}
}

func TestUndeliveredPromptRetriesWithoutReplacingAttempt(t *testing.T) {
	for _, failure := range []error{errors.New("preflight read disconnected"), core.Invalid("request rejected")} {
		t.Run(failure.Error(), func(t *testing.T) {
			f := setup(t)
			f.runtime.PromptError = &core.NotDelivered{Err: failure}
			task, attempt := f.task(t)
			s := f.snapshot(t, 1)
			if s.Commands["initial_"+attempt.ID].Status != "pending" || f.runtime.PromptRequests != 1 {
				t.Fatal("proven non-delivery became an uncertain admission")
			}
			f.runtime.PromptError = nil
			f.tick(t)
			s = f.snapshot(t, 1)
			creates, prompts, _ := f.runtime.Counts()
			if creates != 1 || prompts != 1 || s.Tasks[task.ID].AttemptID != attempt.ID || s.Tasks[task.ID].State != "running" || s.Commands["initial_"+attempt.ID].Status != "admitted" {
				t.Fatal("non-delivery did not recover in the existing attempt")
			}
		})
	}
}

func TestUndeliveredDecisionReplyRetriesOrAllowsCorrection(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			f := setup(t)
			_, attempt := f.task(t)
			f.runtime.SetDecisions(attempt.SessionRef, core.Decision{RuntimeRef: "form_1", Kind: "form"})
			f.tick(t)
			var decision core.Decision
			for _, d := range f.snapshot(t, 1).Decisions {
				decision = d
			}
			failure := errors.New("preflight read failed")
			if invalid {
				failure = core.Invalid("native form validation rejected answer")
			}
			f.runtime.ReplyError = &core.NotDelivered{Err: failure}
			input := core.ReplyDecision{Answer: json.RawMessage(`{"answer":{"field":"value"}}`)}
			f.do(t, 1, http.MethodPost, "/v1/decisions/"+decision.ID+"/reply", "reply", input, nil)
			f.tick(t)
			got := f.snapshot(t, 0).Decisions[decision.ID]
			want := "reply_pending"
			if invalid {
				want = "pending"
			}
			if got.Status != want {
				t.Fatalf("reply status=%s want %s", got.Status, want)
			}
			f.runtime.ReplyError = nil
			if invalid {
				input.Answer = json.RawMessage(`{"answer":{"field":"corrected"}}`)
				f.do(t, 0, http.MethodPost, "/v1/decisions/"+decision.ID+"/reply", "correct-reply", input, nil)
			}
			f.tick(t)
			if f.snapshot(t, 1).Decisions[decision.ID].Status != "answered" {
				t.Fatal("proven undelivered reply could not recover")
			}
		})
	}
}

func TestFailedContinuationsRespectCapacityAndRemainEligible(t *testing.T) {
	for _, role := range []string{"worker", "coordinator"} {
		for _, action := range []string{"followup", "resume", "reply"} {
			t.Run(role+"/"+action, func(t *testing.T) {
				f := setup(t, 1)
				create := func(op string) core.Task {
					var task core.Task
					f.do(t, 0, http.MethodPost, "/v1/tasks", op, core.CreateTask{ProjectID: f.project.ID, Role: role, Title: op, Instructions: "Fixture work"}, &task)
					return task
				}
				a := create("a")
				f.tick(t)
				s := f.snapshot(t, 0)
				attempt := s.Attempts[s.Tasks[a.ID].AttemptID]
				if action == "reply" {
					f.runtime.SetDecisions(attempt.SessionRef, core.Decision{RuntimeRef: "form_1", Kind: "form"})
				}
				f.runtime.SetState(attempt.SessionRef, "failed")
				f.tick(t)
				b := create("b")
				f.tick(t)
				if action == "followup" {
					f.do(t, 1, http.MethodPost, "/v1/tasks/"+a.ID+"/followup", "continue", core.Followup{Text: "Continue existing work"}, nil)
				} else if action == "resume" {
					f.do(t, 1, http.MethodPost, "/v1/tasks/"+a.ID+"/control", "continue", core.Control{Action: "resume"}, nil)
				} else {
					var decision core.Decision
					for _, d := range f.snapshot(t, 0).Decisions {
						decision = d
					}
					f.do(t, 1, http.MethodPost, "/v1/decisions/"+decision.ID+"/reply", "continue", core.ReplyDecision{Answer: json.RawMessage(`{"answer":{"field":"value"}}`)}, nil)
				}
				for range 3 {
					f.tick(t)
				}
				s = f.snapshot(t, 1)
				_, prompts, _ := f.runtime.Counts()
				if prompts != 2 || s.Tasks[a.ID].State == "failed" {
					t.Fatal("continuation exceeded capacity or lost eligibility")
				}
				if action == "reply" {
					for _, d := range s.Decisions {
						if d.Status != "reply_pending" {
							t.Fatal("reply woke a failed session without capacity")
						}
					}
				}
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+b.ID+"/control", "pause-b", core.Control{Action: "pause"}, nil)
				for range 3 {
					f.tick(t)
				}
				s = f.snapshot(t, 1)
				_, prompts, _ = f.runtime.Counts()
				want := 3
				if action == "reply" {
					want = 2
					for _, d := range s.Decisions {
						if d.Status != "answered" {
							t.Fatal("deferred reply never delivered")
						}
					}
				}
				if prompts != want || len(s.Attempts) != 2 || s.Tasks[a.ID].AttemptID != attempt.ID {
					t.Fatal("deferred continuation was lost or replaced")
				}
			})
		}
	}
}

func TestCleanupBarsContinuationAndRetainsLateEvidence(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	f.runtime.SetState(attempt.SessionRef, "failed")
	f.tick(t)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/cleanup", "cleanup", nil, nil)
	f.runtime.Disconnected = true
	if err := f.engine.Tick(f.ctx); err == nil {
		t.Fatal("disconnected runtime unexpectedly reconciled")
	}
	f.runtime.Disconnected = false
	for _, phase := range []string{"cleanup_pending", "cleaned"} {
		if f.snapshot(t, 1).Attempts[attempt.ID].Phase != phase {
			t.Fatal("unexpected cleanup phase")
		}
		if err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", phase+"-followup", core.Followup{Text: "must not run"}, nil); err == nil {
			t.Fatal("cleanup admitted followup")
		}
		if err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/control", phase+"-resume", core.Control{Action: "resume"}, nil); err == nil {
			t.Fatal("cleanup admitted resume")
		}
		if phase == "cleanup_pending" {
			if r := f.result(t, task, attempt, 1, "late-result"); !r.Late {
				t.Fatal("cleanup result reopened task")
			}
		}
		f.tick(t)
	}
	if _, err := os.Stat(attempt.Workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("eligible clean workspace was not removed: %v", err)
	}
}

func TestCleanupRevalidatesPersistedTaskEligibility(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	f.runtime.SetState(attempt.SessionRef, "failed")
	f.tick(t)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/cleanup", "cleanup", nil, nil)
	if err := f.store.Update(f.ctx, func(s *core.State) error {
		v := s.Tasks[task.ID]
		v.State = "running"
		s.Tasks[v.ID] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if _, err := os.Stat(attempt.Workspace.Path); err != nil {
		t.Fatal("cleanup removed a nonterminal task's workspace", err)
	}
}

func TestCreationRejectsTransitiveAndImplicitCompletionCycles(t *testing.T) {
	f := setup(t)
	create := func(op, parent string, deps ...string) core.Task {
		var task core.Task
		f.do(t, 0, http.MethodPost, "/v1/tasks", op, core.CreateTask{ProjectID: f.project.ID, ParentID: parent, Dependencies: deps, Title: op, Instructions: "Fixture work"}, &task)
		return task
	}
	p := create("parent", "")
	d := create("dependency", "", p.ID)
	q := create("other-parent", "")
	x := create("child", p.ID, q.ID)
	for _, input := range []core.CreateTask{
		{ParentID: p.ID, Dependencies: []string{p.ID}},
		{ParentID: p.ID, Dependencies: []string{d.ID}},
		{ParentID: q.ID, Dependencies: []string{p.ID}},
	} {
		input.ProjectID, input.Title, input.Instructions = f.project.ID, "cycle", "must reject"
		if err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks", core.ID("cycle_"), input, nil); err == nil {
			t.Fatal("accepted an unsatisfiable completion cycle", input)
		}
	}
	create("valid-sibling", p.ID, x.ID)
	if len(f.snapshot(t, 1).Tasks) != 5 {
		t.Fatal("cycle rejection changed state or rejected valid sibling ordering")
	}
}

func TestFutureResultCannotReserveAnUnissuedGeneration(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	input := core.SubmitResult{AttemptID: attempt.ID, Generation: 2, Summary: "Premature", Evidence: []core.Evidence{{Name: "test", Reference: "fixture://premature", Passed: true}}}
	if err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/results", "future", input, nil); err == nil {
		t.Fatal("future result reserved an unissued generation")
	}
	var followed core.Task
	f.run(t, 1, &followed, "tasks", "followup", task.ID, "--text", "Issue generation two", "--operation-id", "followup")
	f.tick(t)
	if old := f.result(t, task, attempt, 1, "old"); !old.Late {
		t.Fatal("genuine old-generation evidence was not retained as late")
	}
	if current := f.result(t, task, attempt, 2, "current"); current.Late {
		t.Fatal("issued generation was poisoned by premature result")
	}
	if got := f.snapshot(t, 0); len(got.Results) != 2 || got.Tasks[task.ID].State != "ready_for_review" {
		t.Fatal("result history or current reviewability lost")
	}
}

func TestOldAttemptCannotClaimReplacementGeneration(t *testing.T) {
	f := setup(t)
	task, old := f.task(t)
	f.runtime.SetState(old.SessionRef, "failed")
	f.tick(t)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "retry", core.Control{Action: "retry"}, nil)
	f.tick(t)
	s := f.snapshot(t, 1)
	current := s.Attempts[s.Tasks[task.ID].AttemptID]
	input := core.SubmitResult{AttemptID: old.ID, Generation: 2, Summary: "Unissued for old attempt", Evidence: []core.Evidence{{Name: "test", Reference: "fixture://old", Passed: true}}}
	if err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks/"+task.ID+"/results", "future-old", input, nil); err == nil {
		t.Fatal("old attempt claimed a generation issued only to its replacement")
	}
	if r := f.result(t, task, old, 1, "old-result"); !r.Late {
		t.Fatal("genuine old attempt evidence was not retained")
	}
	if r := f.result(t, task, current, 2, "replacement-result"); r.Late {
		t.Fatal("replacement result was not reviewable")
	}
}

func TestCascadeResumeCannotCrossChildCleanupBarrier(t *testing.T) {
	f := setup(t)
	parent, _ := f.task(t)
	var child core.Task
	f.do(t, 0, http.MethodPost, "/v1/tasks", "child", core.CreateTask{ProjectID: f.project.ID, ParentID: parent.ID, Title: "child", Instructions: "Fixture work"}, &child)
	f.tick(t)
	s := f.snapshot(t, 0)
	attempt := s.Attempts[s.Tasks[child.ID].AttemptID]
	f.runtime.SetState(attempt.SessionRef, "failed")
	f.tick(t)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+child.ID+"/cleanup", "cleanup-child", nil, nil)
	before := f.snapshot(t, 1)
	if err := f.clients[1].Do(f.ctx, http.MethodPost, "/v1/tasks/"+parent.ID+"/control", "cascade", core.Control{Action: "resume", Cascade: true}, nil); err == nil {
		t.Fatal("cascade resumed a cleanup-bound child")
	}
	after := f.snapshot(t, 0)
	if after.Tasks[parent.ID].Generation != before.Tasks[parent.ID].Generation || after.Tasks[child.ID].Generation != before.Tasks[child.ID].Generation || len(after.Commands) != len(before.Commands) {
		t.Fatal("rejected cascade partially changed state")
	}
	f.tick(t)
	if f.snapshot(t, 1).Attempts[attempt.ID].Phase != "cleaned" {
		t.Fatal("rejected cascade prevented eligible cleanup")
	}
}
