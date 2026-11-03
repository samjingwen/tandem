package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/store"
)

func TestReadOperationOwnsExactBytesAndDoesNotWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Update(t.Context(), func(state *core.State) error {
		state.Operations["receipt"] = core.Operation{ID: "receipt", Fingerprint: "fingerprint", Outcome: json.RawMessage(`{"integer":9007199254740993,"decimal":1.23000,"text":"界<>&"}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	value, found, err := s.ReadOperation(t.Context(), "receipt")
	if err != nil || !found || !reflect.DeepEqual(value, before.Operations["receipt"]) {
		t.Fatal("targeted read changed the durable operation", err)
	}
	if !bytes.Contains(value.Outcome, []byte("9007199254740993")) || !bytes.Contains(value.Outcome, []byte("1.23000")) {
		t.Fatal("targeted read changed raw number precision")
	}
	value.Outcome[0] = '!'
	again, found, err := s.ReadOperation(t.Context(), "receipt")
	if err != nil || !found || !reflect.DeepEqual(again, before.Operations["receipt"]) {
		t.Fatal("caller mutation changed persisted receipt bytes", err)
	}
	if _, found, err := s.ReadOperation(t.Context(), "missing' OR 1=1 --"); err != nil || found {
		t.Fatal("missing operation was not a parameterized read", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := s.ReadOperation(canceled, "receipt"); !errors.Is(err, context.Canceled) {
		t.Fatal("targeted receipt read ignored cancellation", err)
	}
	after, err := s.Snapshot(t.Context())
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal("receipt reads changed persisted state or revision", err)
	}
}

func TestReadOperationRejectsCorruptTypedReceipt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	// Only this newly owned fixture receives deliberately corrupt typed data.
	db, err := sql.Open("sqlite", filepath.Join(dir, "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), "INSERT INTO operations(id,body) VALUES(?,?)", "corrupt", `{"id":"corrupt","createdAt":false,"outcome":{}}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReadOperation(t.Context(), "corrupt"); err == nil {
		t.Fatal("corrupt typed operation was accepted")
	}
}

func TestReadViewProjectsTheJournalWithoutChangingInventory(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Update(t.Context(), func(state *core.State) error {
		state.Projects["p"] = core.Project{ID: "p"}
		state.Tasks["t"] = core.Task{ID: "t", ProjectID: "p", Instructions: "complete retained 界 text"}
		for _, id := range []string{"first", "second"} {
			state.Operations[id] = core.Operation{ID: id, Outcome: json.RawMessage(`{"projectId":"p","integer":9007199254740993}`)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "first", "missing"} {
		view, err := s.ReadView(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		expected := before
		expected.Operations = map[string]core.Operation{}
		if operation, found := before.Operations[id]; found {
			expected.Operations[id] = operation
		}
		if !reflect.DeepEqual(view, expected) {
			t.Fatal("read view lost public inventory or selected unrelated receipts", id)
		}
		view.Tasks["t"] = core.Task{ID: "caller-only"}
		if operation, found := view.Operations[id]; found {
			operation.Outcome[0] = '!'
		}
	}
	after, err := s.Snapshot(t.Context())
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal("caller-owned view changed persisted data or revision", err)
	}
}

func TestReadViewSerializesRevisionWithInventory(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	write := func() error {
		return s.Update(t.Context(), func(state *core.State) error {
			state.Tasks["t"] = core.Task{ID: "t", Generation: int(state.Revision + 1)}
			state.Operations["stable"] = core.Operation{ID: "stable", Outcome: json.RawMessage(`{"id":"t"}`)}
			return nil
		})
	}
	if err := write(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for range 20 {
			if err := write(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 30 {
		view, err := s.ReadView(t.Context(), "stable")
		if err != nil || view.Revision != int64(view.Tasks["t"].Generation) || len(view.Operations) != 1 {
			t.Fatal("read view mixed transaction points", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
