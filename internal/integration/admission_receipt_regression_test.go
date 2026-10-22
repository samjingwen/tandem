package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/api"
)

func TestFirstExecutionAncestorBarrier(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		for _, depth := range []int{1, 2} {
			for _, outcome := range []string{"undelivered", "uncertain", "admitted-uncertain", "started"} {
				t.Run(fmt.Sprintf("%s/%d/%s", action, depth, outcome), func(t *testing.T) {
					f := setup(t, 4)
					parent, _ := f.task(t)
					ancestor := parent.ID
					for i := 1; i <= depth; i++ {
						var child core.Task
						f.do(t, 0, http.MethodPost, "/v1/tasks", fmt.Sprintf("child-%d", i), core.CreateTask{ProjectID: f.project.ID, ParentID: parent.ID, Title: "Child", Instructions: "Work"}, &child)
						parent = child
					}
					r := &outcomeRuntime{Fake: f.runtime}
					if outcome == "admitted-uncertain" {
						r.deliveryError = core.ErrUncertain
					} else if outcome != "started" {
						r.promptOutcome = func(_ctx context.Context) error {
							if outcome == "undelivered" {
								return &core.NotDelivered{Err: errors.New("offline before POST")}
							}
							return core.ErrUncertain
						}
					}
					engine := core.New(f.store, r, f.workspace, core.Options{RuntimeID: "fake", Concurrency: 4})
					if err := engine.Tick(f.ctx); err != nil {
						t.Fatal(err)
					}
					if outcome == "started" {
						f.do(t, 0, http.MethodPost, "/v1/tasks/"+parent.ID+"/followup", "followup", core.Followup{Text: "Additional work"}, nil)
					} else if outcome == "undelivered" {
						f.do(t, 0, http.MethodPost, "/v1/tasks/"+parent.ID+"/control", "resume-child", core.Control{Action: "resume"}, nil)
					}
					f.do(t, 0, http.MethodPost, "/v1/tasks/"+ancestor+"/control", "barrier", core.Control{Action: action}, nil)
					before := len(r.prompts)
					reconciled := 0
					r.afterReconcile = func(context.Context) { reconciled++ }
					r.promptOutcome = nil
					r.deliveryError = nil
					if err := engine.Tick(f.ctx); err != nil {
						t.Fatal(err)
					}
					if outcome == "started" {
						if len(r.prompts) <= before {
							t.Fatal("non-cascade control blocked an already-started child")
						}
					} else if len(r.prompts) != before {
						t.Fatal("ancestor barrier allowed first-execution mutation")
					}
					if outcome == "admitted-uncertain" && reconciled == 0 {
						t.Fatal("ancestor barrier blocked read-only admission reconciliation")
					}
					if outcome == "undelivered" {
						f.do(t, 0, http.MethodPost, "/v1/tasks/"+parent.ID+"/followup", "pre-initial-followup", core.Followup{Text: "Additional instructions before first execution"}, nil)
						if err := engine.Tick(f.ctx); err != nil {
							t.Fatal(err)
						}
						if len(r.prompts) != before {
							t.Fatal("pre-initial followup bypassed ancestor barrier")
						}
					}
					if outcome == "undelivered" && action == "pause" {
						f.do(t, 0, http.MethodPost, "/v1/tasks/"+ancestor+"/control", "resume", core.Control{Action: "resume"}, nil)
						if err := engine.Tick(f.ctx); err != nil {
							t.Fatal(err)
						}
						if len(r.prompts) <= before {
							t.Fatal("released ancestor barrier did not admit pending child")
						}
					}
				})
			}
		}
	}
}

func TestManagedReceiptProtocol(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	f.ctx = ctx
	var task core.Task
	f.do(t, 0, http.MethodPost, "/v1/tasks", "create", core.CreateTask{ProjectID: f.project.ID, Role: "coordinator", Title: "Receipt coordinator", Instructions: "Coordinate"}, &task)
	f.tick(t)
	state := f.snapshot(t, 0)
	task = state.Tasks[task.ID]
	attempt := state.Attempts[task.AttemptID]
	large := strings.Repeat("界", 43000)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "large-one", core.Followup{Text: large}, nil)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "large-two", core.Followup{Text: large}, nil)
	var child core.Task
	f.do(t, 0, http.MethodPost, "/v1/tasks", "child", core.CreateTask{ProjectID: f.project.ID, ParentID: task.ID, Title: "Decision child", Instructions: large}, &child)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+child.ID+"/followup", "child-followup-one", core.Followup{Text: large}, nil)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+child.ID+"/followup", "child-followup-two", core.Followup{Text: large}, nil)
	f.tick(t)
	state = f.snapshot(t, 0)
	childAttempt := state.Attempts[state.Tasks[child.ID].AttemptID]
	details, _ := json.Marshal(map[string]string{"question": large + large + large})
	f.runtime.SetDecisions(childAttempt.SessionRef, core.Decision{RuntimeRef: "large-form", Kind: "form", Prompt: "Continue?", Details: details})
	f.tick(t)
	state = f.snapshot(t, 0)
	var decisionID string
	for id, d := range state.Decisions {
		if d.TaskID == child.ID {
			decisionID = id
		}
	}
	input, _ := json.Marshal(map[string]any{"url": f.server.URL, "task": task, "attempt": attempt, "token": "integration-test-token", "text": large, "childId": child.ID, "decisionId": decisionID})
	command := exec.CommandContext(f.ctx, "node", "../../plugin/test/receipt-protocol.ts")
	command.Stdin = bytes.NewReader(input)
	receipts := map[string]json.RawMessage{}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual managed plugin protocol: %v\n%s", err, output)
	}
	if err := json.Unmarshal(output, &receipts); err != nil {
		t.Fatal(err, string(output))
	}
	if len(receipts) != 5 {
		t.Fatal("not every managed mutation returned a recoverable outcome")
	}
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "later-followup", core.Followup{Text: "Later revision, never substitute this task for an original receipt"}, nil)
	var replay, original core.Task
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "plugin-followup", core.Followup{Text: large}, &replay)
	if err := json.Unmarshal(receipts["plugin-followup"], &original); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replay, original) {
		t.Fatal("identical mutation replay substituted newer task data")
	}
	fresh := httptest.NewServer(api.New(f.engine, api.Options{Token: "integration-test-token"}))
	defer fresh.Close()
	input, _ = json.Marshal(map[string]any{"url": fresh.URL, "task": task, "attempt": attempt, "token": "integration-test-token", "recover": true, "expected": receipts["plugin-followup"], "receipts": receipts})
	command = exec.CommandContext(f.ctx, "node", "../../plugin/test/receipt-protocol.ts")
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fresh-handler retained-operation recovery: %v\n%s", err, output)
	}
}

func TestDecisionListPromptProjection(t *testing.T) {
	for _, prompt := range []string{"Continue?", "Continue?\x1b[31m", "Allow shell command?", strings.Repeat("界", 1000) + "\x1b[31m"} {
		for _, kind := range []string{"form", "permission"} {
			t.Run(fmt.Sprintf("%s/%d", kind, len(prompt)), func(t *testing.T) {
				f := setup(t)
				_, attempt := f.task(t)
				f.runtime.SetDecisions(attempt.SessionRef, core.Decision{RuntimeRef: "question", Kind: kind, Prompt: prompt})
				f.tick(t)
				var out, receipt bytes.Buffer
				if err := f.runners[0].Run(f.ctx, []string{"decisions", "list"}, &out, &receipt); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "Continue?") && !strings.Contains(out.String(), "Allow shell command?") && !strings.Contains(out.String(), "[content]") {
					t.Fatal("decision prompt missing", out.String())
				}
				if strings.Contains(out.String(), "\x1b") {
					t.Fatal("terminal escape reached readable output")
				}
				var decisions []core.Decision
				f.run(t, 0, &decisions, "decisions", "list")
				if len(decisions) != 1 || decisions[0].Prompt != prompt {
					t.Fatal("full decision content was lost")
				}
			})
		}
	}
}
