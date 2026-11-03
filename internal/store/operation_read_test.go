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
