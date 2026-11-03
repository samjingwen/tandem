package core_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/store"
)

type countedReceiptStore struct {
	*store.SQLite
	updates atomic.Int64
}

func (s *countedReceiptStore) Update(ctx context.Context, change func(*core.State) error) error {
	s.updates.Add(1)
	return s.SQLite.Update(ctx, change)
}

func receiptStore(t *testing.T) *countedReceiptStore {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Update(t.Context(), func(state *core.State) error {
		state.Projects["project"] = core.Project{ID: "project"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &countedReceiptStore{SQLite: s}
}

func TestImmutableReceiptReplayDoesNotEnterWriteTransaction(t *testing.T) {
	counted := receiptStore(t)
	e := core.New(counted, nil, nil, core.Options{})
	task, err := e.Create(t.Context(), "create", core.CreateTask{ProjectID: "project", Title: "Receipt", Instructions: "Original"})
	if err != nil {
		t.Fatal(err)
	}
	input := core.Followup{Text: "First continuation"}
	original, err := e.Followup(t.Context(), "first", task.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Followup(t.Context(), "second", task.ID, core.Followup{Text: "Later generation"}); err != nil {
		t.Fatal(err)
	}
	before := counted.updates.Load()
	replay, err := e.Followup(t.Context(), "first", task.ID, input)
	if err != nil || !reflect.DeepEqual(replay, original) {
		t.Fatal("replay changed the immutable outcome", err)
	}
	if counted.updates.Load() != before {
		t.Fatal("read-only receipt replay entered a write transaction")
	}
	if _, err := e.Followup(t.Context(), "first", task.ID, core.Followup{Text: "Conflicting input"}); err == nil {
		t.Fatal("conflicting operation reuse was accepted")
	}
	if counted.updates.Load() != before {
		t.Fatal("receipt conflict entered a write transaction")
	}
}

func TestReceiptReplayRetainsStoreFallback(t *testing.T) {
	counted := receiptStore(t)
	// Embedding only Store deliberately hides the optional optimized reader.
	fallback := struct{ core.Store }{counted}
	e := core.New(fallback, nil, nil, core.Options{})
	input := core.CreateTask{ProjectID: "project", Title: "Fallback", Instructions: "Original"}
	original, err := e.Create(t.Context(), "create", input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := e.Create(t.Context(), "create", input)
	if err != nil || !reflect.DeepEqual(replay, original) || counted.updates.Load() != 2 {
		t.Fatal("store without an optimized reader lost transactional replay", err)
	}
}

type failedReceiptStore struct{ *countedReceiptStore }

func (s failedReceiptStore) ReadOperation(context.Context, string) (core.Operation, bool, error) {
	return core.Operation{}, false, context.DeadlineExceeded
}

func TestReceiptReadFailureNeverFallsThroughToAdmission(t *testing.T) {
	counted := receiptStore(t)
	e := core.New(failedReceiptStore{counted}, nil, nil, core.Options{})
	_, err := e.Create(t.Context(), "create", core.CreateTask{ProjectID: "project", Title: "No admission", Instructions: "Original"})
	if !errors.Is(err, context.DeadlineExceeded) || counted.updates.Load() != 0 {
		t.Fatal("failed receipt read reached a write transaction", err)
	}
}

type simultaneousReceiptMisses struct {
	*countedReceiptStore
	misses atomic.Int64
	ready  chan struct{}
}

func (s *simultaneousReceiptMisses) ReadOperation(ctx context.Context, id string) (core.Operation, bool, error) {
	operation, found, err := s.SQLite.ReadOperation(ctx, id)
	if err == nil && !found {
		if s.misses.Add(1) == 2 {
			close(s.ready)
		}
		select {
		case <-s.ready:
		case <-ctx.Done():
			return core.Operation{}, false, ctx.Err()
		}
	}
	return operation, found, err
}

func TestConcurrentReceiptMissesRecheckInsideAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	counted := receiptStore(t)
	s := &simultaneousReceiptMisses{countedReceiptStore: counted, ready: make(chan struct{})}
	e := core.New(s, nil, nil, core.Options{})
	input := core.CreateTask{ProjectID: "project", Title: "Concurrent", Instructions: "Original"}
	var results [2]core.Task
	var failures [2]error
	var wait sync.WaitGroup
	for index := range results {
		wait.Go(func() { results[index], failures[index] = e.Create(ctx, "create", input) })
	}
	wait.Wait()
	if failures[0] != nil || failures[1] != nil || !reflect.DeepEqual(results[0], results[1]) || s.misses.Load() != 2 {
		t.Fatal("simultaneous read misses did not return the same committed outcome", failures)
	}
	state, err := s.Snapshot(t.Context())
	if err != nil || len(state.Tasks) != 1 || len(state.Operations) != 1 || state.Revision != 2 {
		t.Fatal("read misses bypassed transactional duplicate detection", err)
	}
}

func TestReadViewRetainsSnapshotFallbackWithoutJournalEnumeration(t *testing.T) {
	counted := receiptStore(t)
	if err := counted.Update(t.Context(), func(state *core.State) error {
		state.Operations["first"] = core.Operation{ID: "first", Outcome: []byte(`{"value":1}`)}
		state.Operations["second"] = core.Operation{ID: "second", Outcome: []byte(`{"value":2}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fallback := struct{ core.Store }{counted}
	e := core.New(fallback, nil, nil, core.Options{})
	for _, id := range []string{"", "first", "missing"} {
		view, err := e.ReadView(t.Context(), id)
		if err != nil || len(view.Projects) != 1 || len(view.Operations) > 1 {
			t.Fatal("fallback read view changed the inventory contract", err)
		}
		if id == "first" && view.Operations[id].ID != id || id != "first" && len(view.Operations) != 0 {
			t.Fatal("fallback read view exposed unrelated receipts")
		}
	}
}
