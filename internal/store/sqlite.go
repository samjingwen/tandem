// Package store provides a transactional SQLite state boundary and single-owner
// process lock. The lock is held until Close, never stolen on a timer.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/samjingwen/tandem/core"
	_ "modernc.org/sqlite"
)

//go:embed migrations/001_initial.sql
var initial string

type SQLite struct {
	db   *sql.DB
	lock *flock.Flock
	mu   sync.Mutex
}

func Open(directory string) (*SQLite, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must be owner-only; refusing to change existing permissions automatically")
	}
	for _, name := range []string{"scheduler.lock", "tandem.db"} {
		if st, e := os.Lstat(filepath.Join(dir, name)); e == nil && !st.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing nonregular state file %s", name)
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
	}
	l := flock.New(filepath.Join(dir, "scheduler.lock"))
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("state store already has an active scheduler owner; connect as a client instead")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "tandem.db"))
	if err != nil {
		_ = l.Unlock()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db, lock: l}
	if err = s.init(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) init() error {
	ctx := context.Background()
	for _, q := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON"} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)"); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return errors.New("database schema is newer than this executable")
	}
	if version == 1 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, initial); err != nil {
		return err
	}
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES('instance_id',?)", hex.EncodeToString(b)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(1,?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadMap[T any](ctx context.Context, q queryer, table string, target map[string]T) error {
	rows, err := q.QueryContext(ctx, "SELECT id,body FROM "+table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var body []byte
		if err = rows.Scan(&id, &body); err != nil {
			return err
		}
		var v T
		if err = json.Unmarshal(body, &v); err != nil {
			return fmt.Errorf("corrupt %s record: %w", table, err)
		}
		target[id] = v
	}
	return rows.Err()
}

func load(ctx context.Context, q queryer) (core.State, error) {
	s := core.NewState()
	if err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='instance_id'").Scan(&s.InstanceID); err != nil {
		return s, err
	}
	if err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='revision'").Scan(&s.Revision); err != nil {
		return s, err
	}
	for _, f := range []func() error{
		func() error { return loadMap(ctx, q, "projects", s.Projects) }, func() error { return loadMap(ctx, q, "tasks", s.Tasks) }, func() error { return loadMap(ctx, q, "attempts", s.Attempts) }, func() error { return loadMap(ctx, q, "commands", s.Commands) }, func() error { return loadMap(ctx, q, "decisions", s.Decisions) }, func() error { return loadMap(ctx, q, "results", s.Results) }, func() error { return loadMap(ctx, q, "operations", s.Operations) },
	} {
		if err := f(); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (s *SQLite) Snapshot(ctx context.Context) (core.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return load(ctx, s.db)
}

func writeMap[T any](ctx context.Context, tx *sql.Tx, table string, values map[string]T, revision int64) error {
	for id, v := range values {
		body, err := json.Marshal(v)
		if err != nil {
			return err
		}
		var old string
		err = tx.QueryRowContext(ctx, "SELECT body FROM "+table+" WHERE id=?", id).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if old == string(body) {
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO "+table+"(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", id, string(body)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO history(revision,entity,id,at,body) VALUES(?,?,?,?,?)", revision, table, id, time.Now().UTC().Format(time.RFC3339Nano), string(body)); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLite) Update(ctx context.Context, change func(*core.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := load(ctx, tx)
	if err != nil {
		return err
	}
	encode := func(s core.State) ([]byte, error) {
		return json.Marshal(struct {
			State      core.State
			Operations map[string]core.Operation
		}{s, s.Operations})
	}
	before, err := encode(state)
	if err != nil {
		return err
	}
	if err = change(&state); err != nil {
		return err
	}
	after, err := encode(state)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		return tx.Commit()
	}
	state.Revision++
	for _, f := range []func() error{
		func() error { return writeMap(ctx, tx, "projects", state.Projects, state.Revision) }, func() error { return writeMap(ctx, tx, "tasks", state.Tasks, state.Revision) }, func() error { return writeMap(ctx, tx, "attempts", state.Attempts, state.Revision) }, func() error { return writeMap(ctx, tx, "commands", state.Commands, state.Revision) }, func() error { return writeMap(ctx, tx, "decisions", state.Decisions, state.Revision) }, func() error { return writeMap(ctx, tx, "results", state.Results, state.Revision) }, func() error { return writeMap(ctx, tx, "operations", state.Operations, state.Revision) },
	} {
		if err = f(); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE metadata SET value=? WHERE key='revision'", strconv.FormatInt(state.Revision, 10)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.db.Close(), s.lock.Unlock())
}
