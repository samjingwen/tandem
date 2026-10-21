package store_test

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/store"
)

func TestPersistentTransactionsAndRollback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
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
	dir := filepath.Join(t.TempDir(), "state")
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
	c = exec.Command(os.Args[0], "-test.run=^TestSecondServiceProcessCannotOwnState$")
	c.Env = append(os.Environ(), "TANDEM_LOCK_HELPER=1", "TANDEM_LOCK_DIR="+dir)
	if err = c.Run(); err != nil {
		t.Fatal("released lock could not be acquired", err)
	}
}

func TestOwnerCrashReleasesLockAndPreservesCommittedState(t *testing.T) {
	if os.Getenv("TANDEM_CRASH_HELPER") == "1" {
		s, err := store.Open(os.Getenv("TANDEM_CRASH_DIR"))
		if err != nil {
			os.Exit(21)
		}
		if err = s.Update(context.Background(), func(state *core.State) error {
			state.Tasks["crash-record"] = core.Task{ID: "crash-record", Title: "durable before hard exit"}
			return nil
		}); err != nil {
			os.Exit(22)
		}
		_, _ = os.Stdout.WriteString("committed\n")
		select {}
	}
	dir := filepath.Join(t.TempDir(), "state")
	command := exec.Command(os.Args[0], "-test.run=^TestOwnerCrashReleasesLockAndPreservesCommittedState$")
	command.Env = append(os.Environ(), "TANDEM_CRASH_HELPER=1", "TANDEM_CRASH_DIR="+dir)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != "committed" {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("crash fixture did not commit")
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal("stale OS ownership survived process death", err)
	}
	defer func() { _ = s.Close() }()
	state, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Tasks["crash-record"].Title != "durable before hard exit" {
		t.Fatal("committed WAL state lost after crash")
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

func TestExistingPermissiveDirectoryIsRefusedWithoutChangingIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := store.Open(dir); err == nil {
		_ = s.Close()
		t.Fatal("permissive state directory accepted")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatal("existing directory permissions were changed")
	}
	if _, err = os.Stat(filepath.Join(dir, "tandem.db")); !os.IsNotExist(err) {
		t.Fatal("database created before privacy preflight")
	}
}
