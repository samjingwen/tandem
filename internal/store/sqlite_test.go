package store_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/store"
)

func TestPersistentTransactionsAndRollback(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(ctx, func(s *core.State) error { s.Tasks["one"] = core.Task{ID: "one", Title: "durable"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(ctx, func(s *core.State) error { s.Tasks["two"] = core.Task{ID: "two"}; return core.Invalid("rollback") }); err == nil {
		t.Fatal("expected rollback")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	after, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.InstanceID != before.InstanceID || after.Tasks["one"].Title != "durable" || len(after.Tasks) != 1 {
		t.Fatal("lost or partially committed state")
	}
}

func TestSecondServiceProcessCannotOwnState(t *testing.T) {
	if os.Getenv("TANDEM_LOCK_HELPER") == "1" {
		s, err := store.Open(os.Getenv("TANDEM_LOCK_DIR"))
		if err != nil {
			os.Exit(17)
		}
		_ = s.Close()
		os.Exit(0)
	}
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(os.Args[0], "-test.run=^TestSecondServiceProcessCannotOwnState$")
	c.Env = append(os.Environ(), "TANDEM_LOCK_HELPER=1", "TANDEM_LOCK_DIR="+dir)
	err = c.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatal("second process acquired scheduler lock", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Run(); err == nil {
		t.Fatal("exec.Cmd must not be reused")
	}
	c = exec.Command(os.Args[0], "-test.run=^TestSecondServiceProcessCannotOwnState$")
	c.Env = append(os.Environ(), "TANDEM_LOCK_HELPER=1", "TANDEM_LOCK_DIR="+dir)
	if err = c.Run(); err != nil {
		t.Fatal("released lock could not be acquired", err)
	}
}

func TestSymlinkAliasSharesOwnershipLock(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "real")
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	alias := filepath.Join(root, "alias")
	if err = os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if other, err := store.Open(alias); err == nil {
		_ = other.Close()
		t.Fatal("alias bypassed owner lock")
	}
}
