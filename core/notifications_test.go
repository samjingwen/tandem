package core_test

import (
	"testing"

	"github.com/samjingwen/tandem/core"
)

func TestCoordinatorWakesOnceForNewChildEvidenceNotIdleness(t *testing.T) {
	f := setup(t, core.Options{})
	parent, err := f.e.Create(ctx, "parent-intake", core.CreateTask{ProjectID: f.p.ID, Role: "coordinator", Title: "Coordinate", Instructions: "Delegate and return results"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.e.Create(ctx, "child-intake", core.CreateTask{ProjectID: f.p.ID, ParentID: parent.ID, Title: "Child", Instructions: "Implement and verify"})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	s := f.state(t)
	a := s.Attempts[s.Tasks[child.ID].AttemptID]
	f.r.SetState(a.SessionRef, "idle")
	f.tick(t)
	_, before, _ := f.r.Counts()
	if before != 2 {
		t.Fatal("idle session triggered a model wake", before)
	}
	if _, err = f.e.SubmitResult(ctx, "child-result", child.ID, core.SubmitResult{AttemptID: a.ID, Generation: 1, Summary: "Explicit reported evidence", Evidence: []core.Evidence{{Name: "artifact", Reference: "fixture://evidence", Passed: true}}}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	f.tick(t)
	f.tick(t)
	s = f.state(t)
	_, after, _ := f.r.Counts()
	if after != 3 || s.Tasks[parent.ID].Generation != 2 {
		t.Fatal("new evidence was not delivered exactly once", after)
	}
	// A restarted scheduler reads the persisted signal marker, not conversation
	// memory, and does not issue a duplicate judgment turn.
	f.e = core.New(f.s, f.r, f.w, core.Options{RuntimeID: "fake"})
	f.tick(t)
	_, restarted, _ := f.r.Counts()
	if restarted != after {
		t.Fatal("restart duplicated coordinator wake")
	}
}

func TestCoordinatorNotificationBudgetIsFinite(t *testing.T) {
	f := setup(t, core.Options{MaxCoordinatorNotifications: 1})
	parent, err := f.e.Create(ctx, "bounded-parent", core.CreateTask{ProjectID: f.p.ID, Role: "coordinator", Title: "Coordinate", Instructions: "Handle questions"})
	if err != nil {
		t.Fatal(err)
	}
	children := []core.Task{}
	for _, id := range []string{"child-1", "child-2"} {
		child, err := f.e.Create(ctx, id, core.CreateTask{ProjectID: f.p.ID, ParentID: parent.ID, Title: id, Instructions: "Work"})
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	f.tick(t)
	s := f.state(t)
	first := s.Attempts[s.Tasks[children[0].ID].AttemptID]
	f.r.SetState(first.SessionRef, "failed")
	f.tick(t)
	f.tick(t)
	second := s.Attempts[s.Tasks[children[1].ID].AttemptID]
	f.r.SetState(second.SessionRef, "failed")
	for range 4 {
		f.tick(t)
	}
	s = f.state(t)
	signals := 0
	for _, c := range s.Commands {
		if c.TaskID == parent.ID && c.Kind == "signal" && c.Status == "suppressed" {
			signals++
		}
	}
	if signals != 1 || s.Tasks[parent.ID].Error == "" {
		t.Fatal("automatic model judgment was not bounded", signals)
	}
	_, prompts, _ := f.r.Counts()
	if prompts != 4 {
		t.Fatal("notification budget allowed an extra model turn", prompts)
	}
}
