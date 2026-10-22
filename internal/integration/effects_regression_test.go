package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/testruntime"
)

type effectsRuntime struct {
	*testruntime.Fake
	stage       string
	call        func() error
	inspections int
}

func (r *effectsRuntime) Ensure(ctx context.Context, sp core.SessionSpec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.stage == "session-ensure" {
		if err := r.call(); err != nil {
			return err
		}
	}
	return r.Fake.Ensure(ctx, sp)
}

func (r *effectsRuntime) Inspect(ctx context.Context, sp core.SessionSpec) (core.Observation, error) {
	if err := ctx.Err(); err != nil {
		return core.Observation{}, err
	}
	r.inspections++
	if r.stage == "session-inspect" || (r.stage == "control-settle" && r.inspections == 2) {
		if err := r.call(); err != nil {
			return core.Observation{}, err
		}
	}
	return r.Fake.Inspect(ctx, sp)
}

func (r *effectsRuntime) Interrupt(ctx context.Context, sp core.SessionSpec) error {
	if r.stage == "interrupt" {
		if err := r.call(); err != nil {
			return err
		}
	}
	return r.Fake.Interrupt(ctx, sp)
}

type effectsWorkspaces struct {
	core.Workspaces
	stage string
	call  func() error
}

func (w effectsWorkspaces) Ensure(ctx context.Context, p core.Project, v core.Workspace) (core.Workspace, error) {
	allocated, err := w.Workspaces.Ensure(ctx, p, v)
	if err == nil && w.stage == "workspace-ensure" {
		err = w.call()
	}
	return allocated, err
}

func (w effectsWorkspaces) Inspect(ctx context.Context, p core.Project, v core.Workspace) error {
	err := w.Workspaces.Inspect(ctx, p, v)
	if err == nil && w.stage == "workspace-inspect" {
		err = w.call()
	}
	return err
}

func (w effectsWorkspaces) Remove(ctx context.Context, p core.Project, v core.Workspace) error {
	err := w.Workspaces.Remove(ctx, p, v)
	if err == nil && w.stage == "cleanup" {
		err = w.call()
	}
	return err
}

func TestEffectsBookkeepingAfterExpiry(t *testing.T) {
	for _, stage := range []string{"workspace-ensure", "workspace-inspect", "session-ensure", "session-inspect", "interrupt", "control-settle", "cleanup"} {
		for _, expiry := range []string{"cancel", "deadline"} {
			for _, success := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t", stage, expiry, success), func(t *testing.T) {
					f := setup(t)
					var task core.Task
					var attempt core.Attempt
					if stage == "workspace-ensure" || stage == "workspace-inspect" || stage == "session-ensure" {
						f.do(t, 0, http.MethodPost, "/v1/tasks", "create", core.CreateTask{ProjectID: f.project.ID, Title: stage, Instructions: "Bookkeeping fixture"}, &task)
					} else {
						task, attempt = f.task(t)
					}
					if stage == "session-inspect" {
						f.runtime.SetState(attempt.SessionRef, "idle")
					}
					if stage == "interrupt" || stage == "control-settle" || stage == "cleanup" {
						action := "pause"
						if stage == "cleanup" {
							action = "cancel"
						}
						f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "control", core.Control{Action: action}, nil)
						if stage == "cleanup" {
							f.tick(t)
							if _, err := f.engine.Cleanup(f.ctx, "cleanup", attempt.ID); err != nil {
								t.Fatal(err)
							}
						}
					}
					callCtx, cancel := context.WithCancel(f.ctx)
					if expiry == "deadline" {
						cancel()
						callCtx, cancel = context.WithTimeout(f.ctx, time.Second)
					}
					defer cancel()
					calls := 0
					call := func() error {
						calls++
						if expiry == "cancel" {
							cancel()
						}
						<-callCtx.Done()
						if success {
							return nil
						}
						return callCtx.Err()
					}
					r := &effectsRuntime{Fake: f.runtime, stage: stage, call: call}
					w := effectsWorkspaces{Workspaces: f.workspace, stage: stage, call: call}
					e := core.New(f.store, r, w, core.Options{RuntimeID: "fake"})
					_ = e.Tick(callCtx)
					s := f.snapshot(t, 1)
					a := s.Attempts[s.Tasks[task.ID].AttemptID]
					if calls != 1 {
						t.Fatalf("expiry injection calls=%d", calls)
					}
					if !success {
						if a.Error == "" || (stage != "cleanup" && a.FailureCount != 1) {
							t.Fatalf("lost failure outcome: %+v", a)
						}
					} else {
						switch stage {
						case "workspace-ensure":
							if !a.Workspace.Allocated || a.Phase != "session_pending" {
								t.Fatalf("lost workspace outcome: %+v", a)
							}
						case "session-ensure":
							if a.Phase != "session_bound" || s.Commands["initial_"+a.ID].Status != "pending" {
								t.Fatalf("lost session binding: %+v", a)
							}
						case "session-inspect":
							if a.Observed != "idle" || s.Tasks[task.ID].State == "completed" {
								t.Fatalf("lost observation: %+v", a)
							}
						case "interrupt":
							if a.FailureCount != 1 {
								t.Fatal("lost post-interrupt inspection failure", a)
							}
						case "control-settle":
							if s.Tasks[task.ID].State != "paused" || a.Observed != "idle" {
								t.Fatal("lost proven control settlement", a)
							}
						case "cleanup":
							if a.Phase != "cleaned" || a.Workspace.Allocated || s.Commands["cleanup_"+a.ID].Status != "completed" {
								t.Fatalf("lost cleanup outcome: %+v", a)
							}
						}
					}
				})
			}
		}
	}
}

func TestAdmissionRecoveryAfterExpiry(t *testing.T) {
	for _, desired := range []string{"active", "paused"} {
		for _, expiry := range []string{"cancel", "deadline"} {
			t.Run(desired+"/"+expiry, func(t *testing.T) {
				f := setup(t)
				task, attempt := f.task(t)
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "follow", core.Followup{Text: "Lost admission response"}, nil)
				f.runtime.LosePromptResponse = true
				f.tick(t)
				if desired == "paused" {
					f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "pause", core.Control{Action: "pause"}, nil)
				}
				f.runtime.SetState(attempt.SessionRef, "idle")
				callCtx, cancel := context.WithCancel(f.ctx)
				if expiry == "deadline" {
					cancel()
					callCtx, cancel = context.WithTimeout(f.ctx, time.Second)
				}
				defer cancel()
				calls := 0
				r := &outcomeRuntime{Fake: f.runtime, afterReconcile: func(ctx context.Context) {
					calls++
					if expiry == "cancel" {
						cancel()
					}
					<-ctx.Done()
				}}
				e := core.New(f.store, r, f.workspace, core.Options{RuntimeID: "fake"})
				_ = e.Tick(callCtx)
				s := f.snapshot(t, 1)
				for _, c := range s.Commands {
					if c.Kind == "prompt" && c.Status != "admitted" {
						t.Fatal("lost read-only admission recovery", c)
					}
				}
				if calls != 1 || len(r.prompts) != 0 || (desired == "paused" && s.Tasks[task.ID].State != "paused") {
					t.Fatal("admission recovery repeated a mutation or lost settlement")
				}
			})
		}
	}
}

func TestExpiredExternalFailuresBoundMutations(t *testing.T) {
	for _, stage := range []string{"workspace-ensure", "session-ensure", "interrupt"} {
		t.Run(stage, func(t *testing.T) {
			f := setup(t)
			var task core.Task
			if stage == "interrupt" {
				task, _ = f.task(t)
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "pause", core.Control{Action: "pause"}, nil)
			} else {
				f.do(t, 0, http.MethodPost, "/v1/tasks", "create", core.CreateTask{ProjectID: f.project.ID, Title: stage, Instructions: "Bounded failures"}, &task)
			}
			calls := 0
			for range 10 {
				callCtx, cancel := context.WithCancel(f.ctx)
				call := func() error { calls++; cancel(); return callCtx.Err() }
				r := &effectsRuntime{Fake: f.runtime, stage: stage, call: call}
				w := effectsWorkspaces{Workspaces: f.workspace, stage: stage, call: call}
				e := core.New(f.store, r, w, core.Options{RuntimeID: "fake"})
				_ = e.Tick(callCtx)
				cancel()
			}
			s := f.snapshot(t, 1)
			a := s.Attempts[s.Tasks[task.ID].AttemptID]
			if calls != 8 || a.FailureCount != 8 {
				t.Fatalf("external retry budget lost: calls=%d attempt=%+v", calls, a)
			}
		})
	}
}

func TestDispatchBudgetSurvivesIdleAndResume(t *testing.T) {
	for _, operation := range []string{"prompt", "reply"} {
		t.Run(operation, func(t *testing.T) {
			f := setup(t)
			task, attempt := f.task(t)
			f.runtime.SetState(attempt.SessionRef, "idle")
			var decision core.Decision
			if operation == "prompt" {
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "follow", core.Followup{Text: "Continue existing work"}, nil)
			} else {
				f.runtime.SetDecisions(attempt.SessionRef, core.Decision{Kind: "form", RuntimeRef: "budget-form"})
				f.tick(t)
				for _, d := range f.snapshot(t, 1).Decisions {
					decision = d
				}
				f.do(t, 0, http.MethodPost, "/v1/decisions/"+decision.ID+"/reply", "answer", core.ReplyDecision{Answer: json.RawMessage(`{"answer":{"value":"yes"}}`)}, nil)
			}
			failures := 0
			fail := func(context.Context) error {
				failures++
				return &core.NotDelivered{Err: errors.New("offline before POST")}
			}
			r := &outcomeRuntime{Fake: f.runtime}
			if operation == "prompt" {
				r.promptOutcome = fail
			} else {
				r.replyOutcome = fail
			}
			e := core.New(f.store, r, f.workspace, core.Options{RuntimeID: "fake"})
			for range 10 {
				if err := e.Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			s := f.snapshot(t, 1)
			if failures != 8 || s.Tasks[task.ID].State != "recovering" || !strings.Contains(s.Tasks[task.ID].Error, "resume") || s.Attempts[attempt.ID].Error == "" {
				t.Fatalf("lost exhausted-budget diagnostic: failures=%d task=%+v", failures, s.Tasks[task.ID])
			}
			if err := e.Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			if after := f.snapshot(t, 0); after.Revision != s.Revision {
				t.Fatal("unchanged exhausted delivery churned persistent state")
			}
			r.promptOutcome, r.replyOutcome = nil, nil
			f.run(t, 1, &task, "tasks", "resume", task.ID, "--operation-id", "resume")
			if err := e.Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			s = f.snapshot(t, 0)
			if len(s.Attempts) != 1 || s.Tasks[task.ID].AttemptID != attempt.ID || s.Attempts[attempt.ID].FailureCount != 0 || s.Tasks[task.ID].Error != "" {
				t.Fatal("explicit resume did not recover the same attempt")
			}
			for _, c := range s.Commands {
				if c.Kind == "prompt" && c.Status != "admitted" {
					t.Fatalf("resume left prompt undelivered: %+v", c)
				}
			}
			if operation == "reply" && s.Decisions[decision.ID].Status != "answered" {
				t.Fatal("resume did not deliver retained reply")
			}
		})
	}
}

func TestSuccessfulDispatchAfterExpiry(t *testing.T) {
	for _, operation := range []string{"prompt", "reply"} {
		for _, expiry := range []string{"cancel", "deadline"} {
			t.Run(operation+"/"+expiry, func(t *testing.T) {
				f := setup(t)
				task, attempt := f.task(t)
				var decision core.Decision
				if operation == "prompt" {
					f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "follow", core.Followup{Text: "Deliver once"}, nil)
				} else {
					f.runtime.SetDecisions(attempt.SessionRef, core.Decision{Kind: "form", RuntimeRef: "delivered-form"})
					f.tick(t)
					for _, d := range f.snapshot(t, 1).Decisions {
						decision = d
					}
					f.do(t, 0, http.MethodPost, "/v1/decisions/"+decision.ID+"/reply", "answer", core.ReplyDecision{Answer: json.RawMessage(`{"answer":{"field":"value"}}`)}, nil)
				}
				callCtx, cancel := context.WithCancel(f.ctx)
				if expiry == "deadline" {
					cancel()
					callCtx, cancel = context.WithTimeout(f.ctx, time.Second)
				}
				defer cancel()
				calls := 0
				r := &outcomeRuntime{Fake: f.runtime, afterDelivery: func(ctx context.Context) {
					calls++
					if expiry == "cancel" {
						cancel()
					}
					<-ctx.Done()
				}}
				e := core.New(f.store, r, f.workspace, core.Options{RuntimeID: "fake"})
				_ = e.Tick(callCtx)
				s := f.snapshot(t, 1)
				if calls != 1 || s.Attempts[attempt.ID].Error != "" || s.Attempts[attempt.ID].FailureCount != 0 {
					t.Fatal("lost proven delivery after expiry")
				}
				for _, c := range s.Commands {
					if c.Kind == "prompt" && c.Status != "admitted" {
						t.Fatal("proven prompt delivery remained uncertain", c)
					}
				}
				if operation == "reply" && s.Decisions[decision.ID].Status != "answered" {
					t.Fatal("proven reply remained uncertain")
				}
				r.afterDelivery = nil
				if err := e.Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
				if len(r.prompts) != map[string]int{"prompt": 1, "reply": 0}[operation] || r.replies != map[string]int{"prompt": 0, "reply": 1}[operation] {
					t.Fatal("proven delivery was repeated")
				}
			})
		}
	}
}

func TestPauseSettlesAfterInterruptBudget(t *testing.T) {
	for _, cascade := range []bool{false, true} {
		t.Run(fmt.Sprint(cascade), func(t *testing.T) {
			f := setup(t)
			parent, parentAttempt := f.task(t)
			attempts := []core.Attempt{parentAttempt}
			if cascade {
				var child core.Task
				f.do(t, 0, http.MethodPost, "/v1/tasks", "child", core.CreateTask{ProjectID: f.project.ID, ParentID: parent.ID, Title: "child", Instructions: "Child work"}, &child)
				f.tick(t)
				s := f.snapshot(t, 0)
				attempts = append(attempts, s.Attempts[s.Tasks[child.ID].AttemptID])
			}
			for _, a := range attempts {
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+a.TaskID+"/followup", "follow-"+a.ID, core.Followup{Text: "Park this pending work"}, nil)
			}
			f.do(t, 0, http.MethodPost, "/v1/tasks/"+parent.ID+"/control", "pause", core.Control{Action: "pause", Cascade: cascade}, nil)
			calls := 0
			r := &effectsRuntime{Fake: f.runtime, stage: "interrupt", call: func() error { calls++; return errors.New("interrupt rejected") }}
			e := core.New(f.store, r, f.workspace, core.Options{RuntimeID: "fake"})
			for range 10 {
				if err := e.Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 8*len(attempts) {
				t.Fatal("interrupt budget was not enforced", calls)
			}
			for _, a := range attempts {
				f.runtime.SetState(a.SessionRef, "idle")
			}
			if err := e.Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			s := f.snapshot(t, 1)
			for _, a := range attempts {
				if s.Tasks[a.TaskID].State != "paused" {
					t.Fatal("idle worker did not settle pause", s.Tasks[a.TaskID])
				}
				parked := false
				for _, c := range s.Commands {
					if c.AttemptID == a.ID && c.Status == "parked" {
						parked = true
					}
				}
				if !parked {
					t.Fatal("pending continuation was not parked")
				}
			}
		})
	}
}

func TestUnboundControlsSettleAfterDisconnectBudget(t *testing.T) {
	for _, phase := range []string{"workspace_pending", "session_pending"} {
		for _, cascade := range []bool{false, true} {
			for _, action := range []string{"pause", "cancel"} {
				t.Run(fmt.Sprintf("%s/%t/%s", phase, cascade, action), func(t *testing.T) {
					f := setup(t)
					calls := 0
					fail := func() error { calls++; return errors.New("allocation unavailable") }
					r := &effectsRuntime{Fake: f.runtime, stage: "session-ensure", call: fail}
					w := effectsWorkspaces{Workspaces: f.workspace}
					if phase == "workspace_pending" {
						w.stage, w.call = "workspace-ensure", fail
					}
					e := core.New(f.store, r, w, core.Options{RuntimeID: "fake", Concurrency: 2})
					var parent core.Task
					f.do(t, 0, http.MethodPost, "/v1/tasks", "parent", core.CreateTask{ProjectID: f.project.ID, Title: "Parent", Instructions: "Unbound control fixture"}, &parent)
					if cascade {
						f.do(t, 0, http.MethodPost, "/v1/tasks", "child", core.CreateTask{ProjectID: f.project.ID, ParentID: parent.ID, Title: "Child", Instructions: "Unbound descendant"}, nil)
					}
					if err := e.Tick(f.ctx); err != nil {
						t.Fatal(err)
					}
					s := f.snapshot(t, 0)
					wantAttempts := 1
					if cascade {
						wantAttempts = 2
					}
					if len(s.Attempts) != wantAttempts {
						t.Fatal("fixture did not allocate all attempts", s.Attempts)
					}
					for _, a := range s.Attempts {
						if a.Phase != phase {
							t.Fatal("fixture did not reach requested phase", a)
						}
					}
					f.do(t, 0, http.MethodPost, "/v1/tasks/"+parent.ID+"/control", "control", core.Control{Action: action, Cascade: cascade}, nil)
					f.runtime.Disconnected = true
					for range 8 {
						if err := e.Tick(f.ctx); err == nil {
							t.Fatal("runtime disconnect was not observed")
						}
					}
					for _, a := range f.snapshot(t, 0).Attempts {
						if a.FailureCount != 8 {
							t.Fatal("disconnect budget was not exhausted", a)
						}
					}
					beforeCalls := calls
					f.runtime.Disconnected = false
					for range 2 {
						if err := e.Tick(f.ctx); err != nil {
							t.Fatal(err)
						}
					}
					s = f.snapshot(t, 1)
					want := "paused"
					if action == "cancel" {
						want = "cancelled"
					}
					for _, a := range s.Attempts {
						if s.Tasks[a.TaskID].State != want || a.Observed != "idle" || a.Phase != phase {
							t.Fatal("unbound control failed to settle locally", s.Tasks[a.TaskID], a)
						}
					}
					if calls != beforeCalls || f.runtime.EnsureRequests != 0 || f.runtime.PromptRequests != 0 || f.runtime.Interrupts != 0 {
						t.Fatal("unbound settlement attempted an external mutation")
					}
				})
			}
		}
	}
}

type cancellationBudgetRuntime struct {
	*outcomeRuntime
	calls int
	fail  bool
}

func (r *cancellationBudgetRuntime) CancelPending(ctx context.Context, sp core.SessionSpec) error {
	r.calls++
	if r.fail {
		return errors.New("native cancellation rejected")
	}
	return r.Fake.CancelPending(ctx, sp)
}

func TestCancellationBudgetPreservesActionableFailure(t *testing.T) {
	for _, pending := range []string{"pending", "parked", "reply_pending"} {
		t.Run(pending, func(t *testing.T) {
			f := setup(t)
			task, attempt := f.task(t)
			if pending == "reply_pending" {
				f.runtime.SetDecisions(attempt.SessionRef, core.Decision{Kind: "form", RuntimeRef: "cancel-form"})
				f.tick(t)
				for _, d := range f.snapshot(t, 0).Decisions {
					f.do(t, 0, http.MethodPost, "/v1/decisions/"+d.ID+"/reply", "answer", core.ReplyDecision{Answer: json.RawMessage(`{"answer":{"value":"yes"}}`)}, nil)
				}
			} else {
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "follow", core.Followup{Text: "Never deliver cancelled work"}, nil)
				if pending == "parked" {
					f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "pause", core.Control{Action: "pause"}, nil)
					f.tick(t)
				}
			}
			found := false
			for _, c := range f.snapshot(t, 0).Commands {
				found = found || c.Status == pending
			}
			for _, d := range f.snapshot(t, 0).Decisions {
				found = found || d.Status == pending
			}
			if !found {
				t.Fatal("fixture did not retain requested pending delivery")
			}
			f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "cancel", core.Control{Action: "cancel"}, nil)
			r := &cancellationBudgetRuntime{outcomeRuntime: &outcomeRuntime{Fake: f.runtime}, fail: true}
			e := core.New(f.store, r, f.workspace, core.Options{RuntimeID: "fake"})
			for range 10 {
				if err := e.Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			f.runtime.SetState(attempt.SessionRef, "idle")
			if err := e.Tick(f.ctx); err != nil {
				t.Fatal(err)
			}
			s := f.snapshot(t, 1)
			for _, message := range []string{s.Tasks[task.ID].Error, s.Attempts[attempt.ID].Error} {
				if !strings.Contains(message, "native cancellation rejected") || !strings.Contains(message, "repeat cancel") || strings.Contains(message, "resume") {
					t.Fatal("lost actionable cancellation failure", message)
				}
			}
			if r.calls != 8 || s.Tasks[task.ID].State != "recovering" {
				t.Fatal("cancellation budget not enforced", r.calls, s.Tasks[task.ID])
			}
			if _, err := f.engine.Control(f.ctx, "invalid-resume", task.ID, core.Control{Action: "resume"}); err == nil {
				t.Fatal("cancelled task accepted resume")
			}
			r.fail = false
			f.run(t, 1, &task, "tasks", "cancel", task.ID, "--operation-id", "repeat-cancel")
			for range 2 {
				if err := e.Tick(f.ctx); err != nil {
					t.Fatal(err)
				}
			}
			s = f.snapshot(t, 0)
			if s.Tasks[task.ID].State != "cancelled" || s.Tasks[task.ID].Error != "" || s.Attempts[attempt.ID].Error != "" || r.calls != 9 || len(r.prompts) != 0 || r.replies != 0 || len(s.Attempts) != 1 {
				t.Fatal("repeat cancel did not settle without re-admitting work", s.Tasks[task.ID], r.calls)
			}
			for _, c := range s.Commands {
				if c.Status == "pending" || c.Status == "parked" || c.Status == "sending" {
					t.Fatal("cancelled prompt remained eligible", c)
				}
			}
		})
	}
}
