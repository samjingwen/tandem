package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/samjingwen/tandem/core"
)

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
