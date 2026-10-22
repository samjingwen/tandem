package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/opencode"
)

type controlSession struct {
	record   map[string]any
	inbox    map[string]any
	messages map[string]any
	running  bool
	failed   bool
	posts    int
}

type controlNative struct {
	sessions              map[string]*controlSession
	loseResponse          bool
	loseTask              string
	failRead              bool
	failOnInterrupt       bool
	posts                 int
	deletes               int
	interrupts            int
	beforeDelete          func()
	invalidAfterInterrupt string
}

func (n *controlNative) serve(w http.ResponseWriter, r *http.Request) {
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	data := func(v any) { write(map[string]any{"data": v}) }
	if r.URL.Path == "/api/info" {
		write(map[string]any{"version": "2.1.0"})
		return
	}
	if r.URL.Path == "/api/session/active" {
		active := map[string]any{}
		for id, s := range n.sessions {
			if s.running {
				active[id] = map[string]string{"type": "running"}
			}
		}
		data(active)
		return
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodPost {
		var record map[string]any
		_ = json.NewDecoder(r.Body).Decode(&record)
		n.sessions[record["id"].(string)] = &controlSession{record: record, inbox: map[string]any{}, messages: map[string]any{}}
		data(record)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/")
	s := n.sessions[parts[0]]
	if s == nil {
		w.WriteHeader(404)
		return
	}
	if len(parts) == 1 {
		s.record["outcome"] = ""
		if s.failed {
			s.record["outcome"] = "failed"
		}
		data(s.record)
		return
	}
	switch parts[1] {
	case "permission", "form":
		data([]any{})
	case "prompt":
		n.posts++
		s.posts++
		var input map[string]any
		_ = json.NewDecoder(r.Body).Decode(&input)
		id := input["id"].(string)
		item := map[string]any{"id": id, "sessionID": parts[0], "type": "user", "payload": map[string]any{"text": input["text"], "metadata": input["metadata"]}}
		s.inbox[id] = item
		s.running = true
		if n.loseResponse && (n.loseTask == "" || s.record["metadata"].(map[string]any)["tandem"].(map[string]any)["taskId"] == n.loseTask) {
			n.loseResponse = false
			w.WriteHeader(503)
			return
		}
		s.messages[id] = map[string]any{"id": id, "type": "user", "text": input["text"], "metadata": input["metadata"]}
		delete(s.inbox, id)
		data(item)
	case "message":
		if n.failRead {
			w.WriteHeader(503)
			return
		}
		if m, ok := s.messages[parts[2]]; ok {
			data(m)
		} else {
			w.WriteHeader(404)
		}
	case "inbox":
		if r.Method == http.MethodDelete {
			if n.beforeDelete != nil {
				n.beforeDelete()
			}
			n.deletes++
			delete(s.inbox, parts[2])
			w.WriteHeader(204)
			return
		}
		items := []any{}
		for _, item := range s.inbox {
			items = append(items, item)
		}
		data(items)
	case "interrupt":
		n.interrupts++
		if r.URL.RawQuery != "resume=false" {
			panic("interrupt must not resume")
		}
		interrupted := s.running
		s.running = false
		if n.failOnInterrupt {
			s.failed = true
		}
		switch n.invalidAfterInterrupt {
		case "missing":
			delete(n.sessions, parts[0])
		case "foreign":
			s.record["location"] = map[string]string{"directory": "/foreign"}
		case "running":
			s.running = true
		}
		write(map[string]bool{"interrupted": interrupted})
	default:
		w.WriteHeader(500)
	}
}

func installControlNative(t *testing.T, f *fixture) *controlNative {
	t.Helper()
	n := &controlNative{sessions: map[string]*controlSession{}}
	server := httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(server.Close)
	runtime, err := opencode.New(opencode.Options{URL: server.URL, Password: "fixture-password"})
	if err != nil {
		t.Fatal(err)
	}
	f.engine = core.New(f.store, runtime, f.workspace, core.Options{RuntimeID: "fixture-native", Concurrency: 4})
	return n
}

func TestCancellationPreservesUnreconciledInbox(t *testing.T) {
	for _, outcome := range []string{"idle", "failed"} {
		for _, initial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/initial=%t", outcome, initial), func(t *testing.T) {
				f := setup(t)
				n := installControlNative(t, f)
				n.failOnInterrupt = outcome == "failed"
				n.loseResponse = initial
				task, attempt := f.task(t)
				if !initial {
					f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "followup", core.Followup{Text: "Continue"}, nil)
					n.loseResponse = true
					f.tick(t)
				}
				session := n.sessions[attempt.SessionRef]
				session.inbox["foreign"] = map[string]any{"id": "foreign", "sessionID": attempt.SessionRef, "type": "user", "payload": map[string]any{"text": "foreign"}}
				posts := n.posts
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "cancel", core.Control{Action: "cancel"}, nil)
				n.failRead = true
				f.tick(t)
				s := f.snapshot(t, 1)
				if s.Tasks[task.ID].State != "recovering" || n.deletes != 0 || len(session.inbox) != 2 || n.interrupts == 0 {
					t.Fatalf("unresolved cancellation destroyed admission evidence: state=%s deletes=%d inbox=%d", s.Tasks[task.ID].State, n.deletes, len(session.inbox))
				}
				sending := 0
				for _, c := range s.Commands {
					if c.AttemptID == attempt.ID {
						if c.Status == "sending" {
							sending++
						} else if c.Status != "admitted" {
							t.Fatalf("lost uncertainty: %+v", c)
						}
					}
				}
				if sending != 1 {
					t.Fatal("uncertain prompt was not retained as sending")
				}
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "repeat-cancel", core.Control{Action: "cancel"}, nil)
				n.failRead = false
				n.beforeDelete = func() {
					state, err := f.engine.Snapshot(f.ctx)
					if err != nil {
						t.Error(err)
						return
					}
					for _, c := range state.Commands {
						if c.AttemptID == attempt.ID && c.Status == "sending" {
							t.Error("deletion preceded durable admission")
						}
					}
					if state.Attempts[attempt.ID].Phase != "executing" {
						t.Error("deletion preceded durable execution latch")
					}
				}
				f.tick(t)
				s = f.snapshot(t, 1)
				if s.Tasks[task.ID].State != "cancelled" || n.deletes != 1 || len(session.inbox) != 1 || session.inbox["foreign"] == nil || n.posts != posts {
					t.Fatalf("cancellation did not recover without reposting or deleting foreign input: %+v", s.Tasks[task.ID])
				}
			})
		}
	}
}

func TestFailedNativeControlSettlement(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		for _, cascade := range []bool{false, true} {
			for _, during := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/cascade=%t/during=%t", action, cascade, during), func(t *testing.T) {
					f := setup(t, 4)
					n := installControlNative(t, f)
					parent, _ := f.task(t)
					task := parent
					if cascade {
						f.do(t, 0, http.MethodPost, "/v1/tasks", "child", core.CreateTask{ProjectID: f.project.ID, ParentID: parent.ID, Title: "Child", Instructions: "Work"}, &task)
						f.tick(t)
					}
					n.failOnInterrupt = during
					if !during {
						for _, s := range n.sessions {
							s.failed = true
							s.running = false
						}
					}
					f.do(t, 0, http.MethodPost, "/v1/tasks/"+parent.ID+"/control", "control", core.Control{Action: action, Cascade: cascade}, nil)
					f.tick(t)
					s := f.snapshot(t, 1)
					want := "paused"
					if action == "cancel" {
						want = "cancelled"
					}
					for _, id := range []string{parent.ID, task.ID} {
						if s.Tasks[id].State != want || s.Attempts[s.Tasks[id].AttemptID].Observed != "failed" {
							t.Fatalf("historical failure prevented settlement or was erased: %+v %+v", s.Tasks[id], s.Attempts[s.Tasks[id].AttemptID])
						}
					}
					interrupts := n.interrupts
					f.tick(t)
					if n.interrupts != interrupts {
						t.Fatal("settled control repeated external writes")
					}
				})
			}
		}
	}
}

func TestPausedAdmissionRetainsStartedChild(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		for _, depth := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/%d", action, depth), func(t *testing.T) {
				f := setup(t, 4)
				n := installControlNative(t, f)
				ancestor, _ := f.task(t)
				child := ancestor
				for i := 0; i < depth; i++ {
					f.do(t, 0, http.MethodPost, "/v1/tasks", fmt.Sprintf("child-%d", i), core.CreateTask{ProjectID: f.project.ID, ParentID: child.ID, Title: "Child", Instructions: "Work"}, &child)
				}
				n.loseResponse = true
				n.loseTask = child.ID
				f.tick(t)
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+child.ID+"/control", "pause-child", core.Control{Action: "pause"}, nil)
				f.tick(t)
				s := f.snapshot(t, 1)
				if s.Tasks[child.ID].State != "paused" || s.Attempts[s.Tasks[child.ID].AttemptID].Phase != "executing" {
					t.Fatal("paused admission lost already-started latch")
				}
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+ancestor.ID+"/control", "ancestor-control", core.Control{Action: action}, nil)
				posts := n.posts
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+child.ID+"/control", "resume-child", core.Control{Action: "resume"}, nil)
				f.tick(t)
				f.tick(t)
				if n.posts != posts+1 {
					t.Fatalf("expected exactly one continuation, got %d", n.posts-posts)
				}
				s = f.snapshot(t, 1)
				if s.Tasks[child.ID].AttemptID == "" || len(n.sessions) != depth+1 || s.Tasks[child.ID].State != "running" {
					t.Fatal("continuation replaced or blocked child")
				}
				if n.sessions[s.Attempts[s.Tasks[child.ID].AttemptID].SessionRef].posts != 2 {
					t.Fatal("child did not receive exactly one original prompt and one continuation")
				}
			})
		}
	}
}

func TestControlRequiresOwnedQuiescentReadback(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		for _, invalid := range []string{"missing", "foreign", "running"} {
			t.Run(action+"/"+invalid, func(t *testing.T) {
				f := setup(t)
				n := installControlNative(t, f)
				task, _ := f.task(t)
				n.invalidAfterInterrupt = invalid
				f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/control", "control", core.Control{Action: action}, nil)
				f.tick(t)
				if f.snapshot(t, 1).Tasks[task.ID].State != "recovering" {
					t.Fatal("control settled without owned quiescent readback")
				}
			})
		}
	}
}
