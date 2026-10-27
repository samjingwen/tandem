package store_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/store"
)

// This is a disposable cross-language storage contract, not native runtime or
// retained-data acceptance. make rust-parity builds the explicit Rust example.
func requireRustFixture(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("TANDEM_RUST_STORE_HELPER")
	if binary == "" {
		t.Skip("make rust-parity supplies the compiled disposable Rust store fixture")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("Rust fixture executable must be absolute")
	}
	return binary
}

func rustFixture(t *testing.T, ctx context.Context, mode, dir string) *exec.Cmd {
	t.Helper()
	binary := requireRustFixture(t)
	command := exec.CommandContext(ctx, binary, mode, dir)
	command.Env = []string{"HOME=" + filepath.Dir(dir), "TMPDIR=" + filepath.Dir(dir), "PATH=/usr/bin:/bin"}
	return command
}

func rustRead(t *testing.T, mode, dir string) core.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := rustFixture(t, ctx, mode, dir)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("Rust store fixture failed: %v: %s", err, stderr.String())
	}
	var export struct {
		State      core.State                `json:"state"`
		Operations map[string]core.Operation `json:"operations"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &export); err != nil {
		t.Fatal(err)
	}
	export.State.Operations = export.Operations
	return export.State
}

func storeRows(t *testing.T, dir string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	out := map[string]string{}
	for _, table := range []string{"projects", "tasks", "attempts", "commands", "decisions", "results", "operations"} {
		rows, err := db.Query("SELECT id,body FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, body string
			if err = rows.Scan(&id, &body); err != nil {
				t.Fatal(err)
			}
			out[table+"/"+id] = body
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	var historyCount int
	if err = db.QueryRow("SELECT count(*) FROM history").Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(historyCount)
	out["history-count"] = string(b)
	return out
}

func TestRustStoreGoRustGoPersistence(t *testing.T) {
	_ = requireRustFixture(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	stamp := time.Date(2026, 10, 26, 3, 4, 5, 123456789, time.UTC)
	payload := strings.Repeat("完整🙂<&>\u2028\n", 100000)
	err = db.Update(ctx, func(s *core.State) error {
		s.Projects["prj_go"] = core.Project{ID: "prj_go", Name: "Go fixture", Path: "/synthetic/project", BaseRef: "master", Instructions: payload, InstructionFiles: []string{"/synthetic/policy.md"}, CreatedAt: stamp}
		s.Tasks["tsk_go"] = core.Task{ID: "tsk_go", ProjectID: "prj_go", Dependencies: []string{}, Title: "before Rust", Instructions: payload, Role: "worker", Desired: "active", State: "recovering", Generation: 9, AttemptID: "att_go", Error: "uncertain admission retained", CreatedAt: stamp, UpdatedAt: stamp}
		s.Tasks["tsk_null"] = core.Task{ID: "tsk_null"}
		s.Attempts["att_go"] = core.Attempt{ID: "att_go", TaskID: "tsk_go", RuntimeID: "synthetic-runtime", SessionRef: "opaque/reference", Workspace: core.Workspace{Allocator: "git", Owner: "att_go", Path: "/synthetic/worktree", Branch: "retained", BaseRef: "master", BaseSHA: "synthetic-base", Allocated: true}, Phase: "executing", Observed: "unknown", Generation: 9, FailureCount: 8, CreatedAt: stamp, UpdatedAt: stamp}
		s.Commands["cmd_go"] = core.Command{ID: "cmd_go", TaskID: "tsk_go", AttemptID: "att_go", Kind: "prompt", Text: payload, Status: "sending", Generation: 9, Sequence: 9007199254740993}
		s.Decisions["dec_go"] = core.Decision{ID: "dec_go", TaskID: "tsk_go", AttemptID: "att_go", RuntimeRef: "opaque/request", Kind: "permission", Prompt: payload, Details: json.RawMessage(`{"large":9007199254740993,"decimal":1.0000000000000000001,"items":[null,"<"]}`), Status: "uncertain", Reply: json.RawMessage(`null`), Error: "lost reply evidence"}
		s.Results["att_go/8"] = core.Result{TaskID: "tsk_go", AttemptID: "att_go", Generation: 8, Summary: payload, Evidence: []core.Evidence{{Name: "assertion", Reference: "fixture://old-generation", Passed: false}}, Late: true, CreatedAt: stamp}
		s.Operations["original-operation"] = core.Operation{ID: "original-operation", Fingerprint: "synthetic-original-fingerprint", Outcome: json.RawMessage(`{"old":"<","large":9007199254740993,"answer":null}`), CreatedAt: stamp}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	rows := storeRows(t, dir)
	read := rustRead(t, "noop", dir)
	if !reflect.DeepEqual(read, before) {
		t.Fatal("Rust did not retain the complete Go state/journal, generations or raw JSON")
	}
	if !reflect.DeepEqual(storeRows(t, dir), rows) {
		t.Fatal("Rust no-op rewrote rows or appended history")
	}
	read = rustRead(t, "update", dir)
	expected := before
	task := expected.Tasks["tsk_go"]
	task.Title = "written-by-rust 世界"
	task.Generation++
	expected.Tasks[task.ID] = task
	expected.Revision++
	if !reflect.DeepEqual(read, expected) {
		t.Fatal("Rust update changed retained content outside the intended fields")
	}
	db, err = store.Open(dir)
	if err != nil {
		t.Fatal("Go cannot reopen Rust-updated schema/lock", err)
	}
	defer func() { _ = db.Close() }()
	got, err := db.Snapshot(ctx)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatal("Go cannot recover exact Rust-updated state", err)
	}
}

func TestRustStoreRustGoRustPersistence(t *testing.T) {
	_ = requireRustFixture(t)
	dir := filepath.Join(t.TempDir(), "state")
	before := rustRead(t, "seed", dir)
	if len(before.InstanceID) != 32 || before.Tasks["tsk_rust"].Generation != 7 || before.Revision != 1 {
		t.Fatal("Rust did not create the specified durable fixture")
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(context.Background(), func(state *core.State) error {
		task := state.Tasks["tsk_rust"]
		task.Title = "written-by-go"
		state.Tasks[task.ID] = task
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	after := rustRead(t, "snapshot", dir)
	if after.InstanceID != before.InstanceID || after.Revision != 2 || after.Tasks["tsk_rust"].Title != "written-by-go" || !reflect.DeepEqual(before.Operations, after.Operations) {
		t.Fatal("Rust cannot recover Go's update or retained Rust receipts")
	}
}

type rustLease struct {
	command *exec.Cmd
	input   io.WriteCloser
	cancel  context.CancelFunc
	ended   bool
}

func startRustLease(t *testing.T, dir, mode string) *rustLease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	command := rustFixture(t, ctx, mode, dir)
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	command.Stderr = &bytes.Buffer{}
	if err = command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	lease := &rustLease{command: command, input: input, cancel: cancel}
	t.Cleanup(func() {
		if !lease.ended {
			_ = input.Close()
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		cancel()
	})
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != `{"locked":true}` {
		t.Fatal("owned Rust fixture did not confirm store ownership")
	}
	return lease
}

func (lease *rustLease) finish(t *testing.T, crash bool) {
	t.Helper()
	if crash {
		if err := lease.command.Process.Kill(); err != nil {
			t.Fatal(err)
		}
	}
	_ = lease.input.Close()
	err := lease.command.Wait()
	lease.ended = true
	lease.cancel()
	if !crash && err != nil {
		t.Fatal("Rust fixture did not exit gracefully", err)
	}
}

func TestRustStoreCrossLanguageOwnershipAndCrash(t *testing.T) {
	_ = requireRustFixture(t)
	dir := filepath.Join(t.TempDir(), "state")
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := rustFixture(t, ctx, "snapshot", dir)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `"code":"store_owned"`) {
		t.Fatal("Rust bypassed the live Go scheduler lock", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	lease := startRustLease(t, dir, "hold")
	if other, err := store.Open(dir); err == nil {
		_ = other.Close()
		t.Fatal("Go bypassed the live Rust scheduler lock")
	}
	alias := filepath.Join(filepath.Dir(dir), "alias")
	if err = os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if other, err := store.Open(alias); err == nil {
		_ = other.Close()
		t.Fatal("Go alias bypassed the live Rust scheduler lock")
	}
	lease.finish(t, false)
	lease = startRustLease(t, dir, "seed-hold")
	lease.finish(t, true)
	db, err = store.Open(dir)
	if err != nil {
		t.Fatal("Rust process death did not release ownership", err)
	}
	defer func() { _ = db.Close() }()
	state, err := db.Snapshot(context.Background())
	if err != nil || state.Tasks["tsk_rust"].Generation != 7 || len(state.Operations) != 1 {
		t.Fatal("committed Rust state was lost after owned process crash", err)
	}
}

func TestRustStoreMalformedTimestampMatchesGoRefusal(t *testing.T) {
	_ = requireRustFixture(t)
	dir := filepath.Join(t.TempDir(), "state")
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	fixture, err := sql.Open("sqlite", filepath.Join(dir, "tandem.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.Exec(`INSERT INTO tasks VALUES('tsk_time','{"id":"tsk_time","createdAt":"not-a-timestamp"}')`); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close()
	if _, err = db.Snapshot(context.Background()); err == nil {
		t.Fatal("Go reference accepted malformed persisted RFC3339 timestamp")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := rustFixture(t, ctx, "snapshot", dir).CombinedOutput()
	if err == nil || !strings.Contains(string(output), `"code":"corrupt_state"`) {
		t.Fatal("Rust accepted persisted timestamp corruption rejected by Go", err)
	}
}

func TestRustStoreTimestampCorpusMatchesGo(t *testing.T) {
	_ = requireRustFixture(t)
	for _, raw := range []string{
		`null`, `"0001-01-01T00:00:00Z"`, `"2026-10-26T03:04:05.123456789012Z"`,
		`"2026-10-26T03:04:05,123Z"`, `"2026-10-26T03:04:60Z"`, `"2026-10-26T03:04:05+24:00"`,
		`"2026-10-26T03:04:05+00:60"`, `"2026-10-26T3:04:05Z"`, `"2026-10-26t03:04:05z"`,
		`"2026-02-30T03:04:05Z"`, `"2026-10-26T03:04:05\u005a"`, `"\u0032026-10-26T03:04:05Z"`,
		`"2026-10-26\u005403:04:05Z"`, `"9999-12-31T23:59:59-00:01"`, `"9999-12-31T23:59:59-24:60"`,
		`"0000-01-01T00:00:00+00:01"`, `"0000-01-01T00:00:00+24:60"`,
	} {
		t.Run(raw, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			db, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			fixture, err := sql.Open("sqlite", filepath.Join(dir, "tandem.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = fixture.Exec(`INSERT INTO tasks VALUES('time',?)`, `{"id":"time","createdAt":`+raw+`}`); err != nil {
				t.Fatal(err)
			}
			_ = fixture.Close()
			_, goError := db.Snapshot(context.Background())
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			output, rustError := rustFixture(t, ctx, "snapshot", dir).CombinedOutput()
			if (goError == nil) != (rustError == nil) {
				t.Fatalf("timestamp %s: Go=%v, Rust=%v: %s", raw, goError, rustError, output)
			}
			if rustError == nil {
				var exported struct {
					State struct {
						Tasks map[string]struct {
							CreatedAt json.RawMessage `json:"createdAt"`
						} `json:"tasks"`
					} `json:"state"`
				}
				if err := json.Unmarshal(output, &exported); err != nil {
					t.Fatal(err)
				}
				want := raw
				if raw == "null" {
					want = `"0001-01-01T00:00:00Z"`
				} else {
					var text string
					if err := json.Unmarshal([]byte(raw), &text); err != nil {
						t.Fatal(err)
					}
					encoded, err := json.Marshal(text)
					if err != nil {
						t.Fatal(err)
					}
					want = string(encoded)
				}
				if got := string(exported.State.Tasks["time"].CreatedAt); got != want {
					t.Fatalf("timestamp spelling changed: got %s, want %s", got, want)
				}
				fixture, err := sql.Open("sqlite", filepath.Join(dir, "tandem.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer fixture.Close()
				var retained string
				if err := fixture.QueryRow(`SELECT body FROM tasks WHERE id='time'`).Scan(&retained); err != nil {
					t.Fatal(err)
				}
				if want := `{"id":"time","createdAt":` + raw + `}`; retained != want {
					t.Fatalf("persisted timestamp token changed: got %s, want %s", retained, want)
				}
			}
		})
	}
}
