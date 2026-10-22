package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	Concurrency                 int
	RuntimeID                   string
	MaxTasks                    int
	MaxDepth                    int
	MaxCoordinatorNotifications int
	RegisterProject             func(context.Context, RegisterProject) (Project, error)
	// AfterCreate is a failure-injection boundary, not a production callback.
	AfterCreate func(Attempt) error
}

type Engine struct {
	store      Store
	runtime    Runtime
	workspaces Workspaces
	opts       Options
	tickMu     sync.Mutex
	// effects serializes control barriers with external mutation admission.
	effects    sync.Mutex
	reconciled atomic.Bool
}

func New(store Store, runtime Runtime, workspaces Workspaces, opts Options) *Engine {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	if opts.MaxTasks <= 0 {
		opts.MaxTasks = 128
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 3
	}
	if opts.MaxCoordinatorNotifications <= 0 {
		opts.MaxCoordinatorNotifications = 32
	}
	return &Engine{store: store, runtime: runtime, workspaces: workspaces, opts: opts}
}

func ID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}
func now() time.Time                                          { return time.Now().UTC() }
func (e *Engine) Snapshot(ctx context.Context) (State, error) { return e.store.Snapshot(ctx) }

func (e *Engine) ReadRevision(ctx context.Context) (string, int64, error) {
	if reader, ok := e.store.(RevisionSource); ok {
		return reader.ReadRevision(ctx)
	}
	state, err := e.store.Snapshot(ctx)
	return state.InstanceID, state.Revision, err
}
func (e *Engine) RuntimeInfo(ctx context.Context) (RuntimeInfo, error) {
	info, err := e.runtime.Info(ctx)
	if err != nil {
		return info, &Error{Code: "runtime_unavailable", Message: "execution runtime metadata unavailable; check the configured endpoint, authorization and supported version"}
	}
	if err == nil && !e.reconciled.Load() {
		info.Compatible = false
		info.Detail = "startup/runtime reconciliation is pending"
	}
	return info, err
}

// mutate commits the idempotency receipt in the same transaction as the state
// change. Identical retries return the original outcome, not a new snapshot.
func mutate[T any](ctx context.Context, e *Engine, op, kind string, input any, change func(*State) (T, error)) (T, error) {
	var out T
	if strings.TrimSpace(op) == "" || len(op) > 160 {
		return out, Invalid("a stable operation ID of 1-160 characters is required")
	}
	b, err := json.Marshal(input)
	if err != nil {
		return out, err
	}
	hash := sha256.Sum256(append([]byte(kind+"\n"), b...))
	fingerprint := hex.EncodeToString(hash[:])
	err = e.store.Update(ctx, func(s *State) error {
		if old, ok := s.Operations[op]; ok {
			if old.Fingerprint != fingerprint {
				return &Error{Code: "operation_conflict", Message: "operation ID was already used for a different request"}
			}
			return json.Unmarshal(old.Outcome, &out)
		}
		var err error
		out, err = change(s)
		if err != nil {
			return err
		}
		body, err := json.Marshal(out)
		if err != nil {
			return err
		}
		s.Operations[op] = Operation{ID: op, Fingerprint: fingerprint, Outcome: body, CreatedAt: now()}
		return nil
	})
	return out, err
}

func (e *Engine) Register(ctx context.Context, op string, input RegisterProject) (Project, error) {
	snapshot, x := e.Snapshot(ctx)
	if x != nil {
		return Project{}, x
	}
	if old, ok := snapshot.Operations[op]; ok {
		b, err := json.Marshal(input)
		if err != nil {
			return Project{}, err
		}
		hash := sha256.Sum256(append([]byte("project.register\n"), b...))
		if old.Fingerprint != hex.EncodeToString(hash[:]) {
			return Project{}, &Error{Code: "operation_conflict", Message: "operation ID was already used for a different request"}
		}
		var previous Project
		err = json.Unmarshal(old.Outcome, &previous)
		return previous, err
	}
	if strings.TrimSpace(input.Name) == "" || input.Path == "" || input.BaseRef == "" {
		return Project{}, Invalid("name, path and baseRef are required")
	}
	if e.opts.RegisterProject == nil {
		return Project{}, Invalid("project registration validator is not configured")
	}
	// Repository inspection is read-only and occurs outside the DB transaction.
	p := Project{Name: input.Name, Path: input.Path, BaseRef: input.BaseRef, Instructions: input.Instructions}
	var err error
	if e.opts.RegisterProject != nil {
		p, err = e.opts.RegisterProject(ctx, input)
		if err != nil {
			return p, err
		}
	}
	return mutate(ctx, e, op, "project.register", input, func(s *State) (Project, error) {
		for _, v := range s.Projects {
			if v.Path == p.Path {
				p.ID = v.ID
				p.CreatedAt = v.CreatedAt
				s.Projects[p.ID] = p
				return p, nil
			}
		}
		p.ID = ID("prj_")
		p.CreatedAt = now()
		s.Projects[p.ID] = p
		return p, nil
	})
}

func create(s *State, input CreateTask, opts Options) (Task, error) {
	if _, ok := s.Projects[input.ProjectID]; !ok {
		return Task{}, NotFound("project not found")
	}
	if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Instructions) == "" {
		return Task{}, Invalid("title and instructions are required")
	}
	if len(input.Instructions) > 128*1024 || len(input.Title) > 500 {
		return Task{}, Invalid("task exceeds input budget")
	}
	if len(s.Tasks) >= opts.MaxTasks {
		return Task{}, Invalid("instance task bound reached")
	}
	if input.Role == "" {
		input.Role = "worker"
	}
	if input.Role != "worker" && input.Role != "coordinator" {
		return Task{}, Invalid("role must be worker or coordinator")
	}
	if input.ParentID != "" {
		parent, ok := s.Tasks[input.ParentID]
		if !ok {
			return Task{}, NotFound("parent task not found")
		}
		if parent.ProjectID != input.ProjectID {
			return Task{}, Invalid("subtasks must belong to the parent's project")
		}
		if parent.Desired != "active" || parent.State == "completed" {
			return Task{}, Invalid("parent does not permit new work")
		}
		depth := 1
		for p := parent; p.ParentID != ""; p = s.Tasks[p.ParentID] {
			depth++
		}
		if depth > opts.MaxDepth {
			return Task{}, Invalid("decomposition depth bound reached")
		}
	}
	for _, id := range input.Dependencies {
		d, ok := s.Tasks[id]
		if !ok || d.ProjectID != input.ProjectID {
			return Task{}, Invalid("dependency must exist in this project")
		}
	}
	if input.ParentID != "" {
		seen := map[string]bool{}
		queue := append([]string{}, input.Dependencies...)
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if id == input.ParentID {
				return Task{}, Invalid("task completion dependencies must be acyclic")
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			queue = append(queue, s.Tasks[id].Dependencies...)
			for _, child := range s.Tasks {
				if child.ParentID == id {
					queue = append(queue, child.ID)
				}
			}
		}
	}
	t := Task{ID: ID("tsk_"), ProjectID: input.ProjectID, ParentID: input.ParentID, Dependencies: input.Dependencies, Title: input.Title, Instructions: input.Instructions, Role: input.Role, Desired: "active", State: "queued", Generation: 1, CreatedAt: now(), UpdatedAt: now()}
	if t.Dependencies == nil {
		t.Dependencies = []string{}
	}
	s.Tasks[t.ID] = t
	return t, nil
}

func (e *Engine) Create(ctx context.Context, op string, input CreateTask) (Task, error) {
	return mutate(ctx, e, op, "task.create", input, func(s *State) (Task, error) { return create(s, input, e.opts) })
}

func (e *Engine) Followup(ctx context.Context, op, id string, input Followup) (Task, error) {
	if strings.TrimSpace(input.Text) == "" || len(input.Text) > 128*1024 {
		return Task{}, Invalid("follow-up text is required and must fit the input budget")
	}
	e.effects.Lock()
	defer e.effects.Unlock()
	return mutate(ctx, e, op, "task.followup/"+id, input, func(s *State) (Task, error) {
		t, ok := s.Tasks[id]
		if !ok {
			return t, NotFound("task not found")
		}
		if t.Desired != "active" || t.State == "completed" {
			return t, Invalid("resume or explicitly reopen the task before following up")
		}
		if t.State == "recovering" {
			return t, ErrUncertain
		}
		if cleanupBarrier(s.Attempts[t.AttemptID]) {
			return t, Invalid("cleanup prevents continuation of this attempt")
		}
		t.Generation++
		t.UpdatedAt = now()
		t.Error = ""
		t.Instructions += "\n\nFollow-up:\n" + input.Text
		if t.AttemptID != "" {
			a := s.Attempts[t.AttemptID]
			a.Generation = t.Generation
			a.FailureCount = 0
			s.Attempts[a.ID] = a
			if _, initialized := s.Commands["initial_"+a.ID]; initialized {
				c := Command{ID: ID("cmd_"), TaskID: t.ID, AttemptID: a.ID, Kind: "prompt", Text: fmt.Sprintf("Task generation %d.\n%s", t.Generation, input.Text), Status: "pending", Generation: t.Generation, Sequence: s.Revision + 1}
				s.Commands[c.ID] = c
				t.State = "running"
			}
		}
		s.Tasks[id] = t
		return t, nil
	})
}

func descendants(s *State, id string, cascade bool) []string {
	ids := []string{id}
	if !cascade {
		return ids
	}
	for i := 0; i < len(ids); i++ {
		for _, t := range s.Tasks {
			if t.ParentID == ids[i] {
				ids = append(ids, t.ID)
			}
		}
	}
	return ids
}

func (e *Engine) Control(ctx context.Context, op, id string, input Control) (Task, error) {
	e.effects.Lock()
	defer e.effects.Unlock()
	if input.Action == "retry" {
		snapshot, err := e.Snapshot(ctx)
		if err != nil {
			return Task{}, err
		}
		if _, replay := snapshot.Operations[op]; !replay {
			if _, exists := snapshot.Tasks[id]; !exists {
				return Task{}, NotFound("task not found")
			}
			for _, target := range descendants(&snapshot, id, input.Cascade) {
				t := snapshot.Tasks[target]
				if target != id && (t.State == "completed" || t.Desired == "cancelled") {
					continue
				}
				a := snapshot.Attempts[t.AttemptID]
				if t.State != "failed" || a.Phase != "failed" || a.Observed != "failed" {
					return Task{}, Invalid("retry requires a conclusively failed, quiescent attempt")
				}
				for _, c := range snapshot.Commands {
					if c.AttemptID == a.ID && (c.Status == "pending" || c.Status == "sending" || c.Status == "parked") {
						return Task{}, Invalid("pending or uncertain dispatch must reconcile before retry")
					}
				}
				for _, d := range snapshot.Decisions {
					if d.AttemptID == a.ID && (d.Status == "reply_pending" || d.Status == "uncertain") {
						return Task{}, Invalid("uncertain decision replies must reconcile before retry")
					}
				}
				observed, err := e.runtime.Inspect(ctx, spec(snapshot, a))
				if err != nil {
					return Task{}, err
				}
				if !observed.Exists || !observed.Owned || observed.State != "failed" || len(observed.Decisions) > 0 {
					return Task{}, Invalid("runtime does not confirm a failed, quiescent attempt without pending decisions")
				}
				checker, ok := e.runtime.(QuiescenceChecker)
				if !ok {
					return Task{}, Invalid("runtime cannot verify failed-attempt quiescence")
				}
				if err = checker.CheckQuiescence(ctx, spec(snapshot, a)); err != nil {
					return Task{}, err
				}
			}
		}
	}
	return mutate(ctx, e, op, "task.control/"+id, input, func(s *State) (Task, error) {
		t, ok := s.Tasks[id]
		if !ok {
			return t, NotFound("task not found")
		}
		if input.Action != "pause" && input.Action != "resume" && input.Action != "cancel" && input.Action != "retry" {
			return t, Invalid("action must be pause, resume, cancel or retry")
		}
		for _, key := range descendants(s, id, input.Cascade) {
			v := s.Tasks[key]
			if v.State == "completed" || v.State == "cancelled" || (v.Desired == "cancelled" && input.Action != "cancel") {
				if key == id {
					return t, Invalid("terminal task cannot be controlled")
				}
				continue
			}
			if cleanupBarrier(s.Attempts[v.AttemptID]) {
				return t, Invalid("cleanup prevents controls for this attempt")
			}
			v.Generation++
			v.UpdatedAt = now()
			v.Error = ""
			switch input.Action {
			case "pause":
				v.Desired = "paused"
				if v.AttemptID == "" {
					v.State = "paused"
				} else {
					v.State = "recovering"
				}
			case "cancel":
				v.Desired = "cancelled"
				if v.AttemptID == "" {
					v.State = "cancelled"
				} else {
					v.State = "recovering"
				}
			case "resume":
				v.Desired = "active"
				if v.AttemptID == "" {
					v.State = "queued"
				} else {
					v.State = "recovering"
					a := s.Attempts[v.AttemptID]
					if a.Phase == "executing" || a.Phase == "session_bound" || a.Phase == "failed" {
						c := Command{ID: ID("cmd_"), TaskID: v.ID, AttemptID: a.ID, Kind: "prompt", Text: fmt.Sprintf("Resume task %s at generation %d. Reconcile prior artifacts and continue the existing instructions; do not start replacement workers.", v.ID, v.Generation), Status: "pending", Generation: v.Generation, Sequence: s.Revision + 1}
						s.Commands[c.ID] = c
					}
				}
			case "retry":
				prior := s.Attempts[v.AttemptID]
				if v.State != "failed" || prior.Phase != "failed" || prior.Observed != "failed" {
					return t, Invalid("retry requires a conclusively failed attempt")
				}
				v.Desired = "active"
				v.AttemptID = ""
				v.State = "queued"
				for key, d := range s.Decisions {
					if d.AttemptID == prior.ID && d.Status == "pending" {
						d.Status = "expired"
						s.Decisions[key] = d
					}
				}
			}
			if v.AttemptID != "" {
				a := s.Attempts[v.AttemptID]
				a.Generation = v.Generation
				a.FailureCount = 0
				s.Attempts[a.ID] = a
			}
			s.Tasks[key] = v
		}
		return s.Tasks[id], nil
	})
}

func (e *Engine) Reply(ctx context.Context, op, id string, input ReplyDecision) (Decision, error) {
	e.effects.Lock()
	defer e.effects.Unlock()
	// Persist reply intent before the external reply. Tick owns delivery/recovery.
	return mutate(ctx, e, op, "decision.reply/"+id, input, func(s *State) (Decision, error) {
		d, ok := s.Decisions[id]
		if !ok {
			return d, NotFound("decision not found")
		}
		t := s.Tasks[d.TaskID]
		if t.AttemptID != d.AttemptID || t.Desired != "active" {
			return d, Invalid("decision is not owned by an active task attempt")
		}
		if cleanupBarrier(s.Attempts[d.AttemptID]) {
			return d, Invalid("cleanup prevents replies for this attempt")
		}
		if d.Status != "pending" {
			return d, &Error{Code: "conflict", Message: "decision is already answered or being reconciled"}
		}
		if len(input.Answer) == 0 || !json.Valid(input.Answer) {
			return d, Invalid("answer must be valid JSON")
		}
		if d.Kind == "permission" {
			var reply struct {
				Decision string `json:"decision"`
				Message  string `json:"message,omitempty"`
			}
			decoder := json.NewDecoder(bytes.NewReader(input.Answer))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&reply) != nil || (reply.Decision != "once" && reply.Decision != "always" && reply.Decision != "reject") {
				return d, Invalid("permission answer requires decision: once, always or reject, with optional message")
			}
		}
		d.Status = "reply_pending"
		d.Reply = input.Answer
		d.Error = ""
		s.Decisions[id] = d
		t.State = "waiting_for_input"
		s.Tasks[t.ID] = t
		return d, nil
	})
}

func (e *Engine) SubmitResult(ctx context.Context, op, id string, input SubmitResult) (Result, error) {
	return mutate(ctx, e, op, "task.result/"+id, input, func(s *State) (Result, error) {
		t, ok := s.Tasks[id]
		if !ok {
			return Result{}, NotFound("task not found")
		}
		a, ok := s.Attempts[input.AttemptID]
		if !ok || a.TaskID != id {
			return Result{}, Invalid("result attempt does not belong to this task")
		}
		if strings.TrimSpace(input.Summary) == "" || len(input.Evidence) == 0 {
			return Result{}, Invalid("summary and explicit evidence are required; idleness is not evidence")
		}
		if input.Generation < 1 || input.Generation > a.Generation {
			return Result{}, Invalid("result must identify an issued instruction generation for this attempt")
		}
		for _, ev := range input.Evidence {
			if ev.Name == "" || ev.Reference == "" {
				return Result{}, Invalid("evidence needs a name and reference")
			}
		}
		r := Result{TaskID: id, AttemptID: a.ID, Generation: input.Generation, Summary: input.Summary, Evidence: input.Evidence, Late: t.Desired != "active" || t.AttemptID != a.ID || t.Generation != input.Generation || t.State == "completed" || cleanupBarrier(a), CreatedAt: now()}
		key := fmt.Sprintf("%s/%d", a.ID, input.Generation)
		if _, exists := s.Results[key]; exists {
			return Result{}, &Error{Code: "conflict", Message: "this attempt generation already has a result; use its original operation ID to retry"}
		}
		s.Results[key] = r
		if !r.Late {
			t.State = "ready_for_review"
			t.UpdatedAt = now()
			s.Tasks[id] = t
		}
		return r, nil
	})
}

func (e *Engine) Accept(ctx context.Context, op, id string) (Task, error) {
	e.effects.Lock()
	defer e.effects.Unlock()
	snapshot, err := e.Snapshot(ctx)
	if err != nil {
		return Task{}, err
	}
	if _, replay := snapshot.Operations[op]; !replay {
		t, ok := snapshot.Tasks[id]
		if !ok {
			return Task{}, NotFound("task not found")
		}
		a, ok := snapshot.Attempts[t.AttemptID]
		if !ok {
			return Task{}, Invalid("task has no execution attempt")
		}
		observation, err := e.runtime.Inspect(ctx, spec(snapshot, a))
		if err != nil {
			return Task{}, err
		}
		if !observation.Exists || !observation.Owned || observation.State != "idle" {
			return Task{}, Invalid("runtime must currently confirm owned quiescence before acceptance")
		}
		if err = e.workspaces.Inspect(ctx, snapshot.Projects[t.ProjectID], a.Workspace); err != nil {
			return Task{}, err
		}
	}
	return mutate(ctx, e, op, "task.accept/"+id, struct{}{}, func(s *State) (Task, error) {
		t, ok := s.Tasks[id]
		if !ok {
			return t, NotFound("task not found")
		}
		key := fmt.Sprintf("%s/%d", t.AttemptID, t.Generation)
		r, ok := s.Results[key]
		if !ok || r.Late || t.State != "ready_for_review" || t.Desired != "active" {
			return t, Invalid("task has no current reviewable result")
		}
		a := s.Attempts[t.AttemptID]
		if a.Observed != "idle" {
			return t, Invalid("execution must be confirmed quiescent before accepting")
		}
		for _, c := range s.Commands {
			if c.AttemptID == a.ID && (c.Status == "pending" || c.Status == "sending" || c.Status == "parked") {
				return t, Invalid("pending dispatch must settle before acceptance")
			}
		}
		for _, ev := range r.Evidence {
			if !ev.Passed {
				return t, Invalid("failed evidence cannot be accepted")
			}
		}
		for _, d := range s.Decisions {
			if d.TaskID == id && (d.Status == "pending" || d.Status == "reply_pending" || d.Status == "uncertain") {
				return t, Invalid("resolve pending decisions before accepting")
			}
		}
		for _, child := range s.Tasks {
			if child.ParentID == id && child.State != "completed" {
				return t, Invalid("all subtasks must be completed before parent acceptance")
			}
		}
		r.Accepted = true
		s.Results[key] = r
		t.State = "completed"
		t.UpdatedAt = now()
		s.Tasks[id] = t
		return t, nil
	})
}

func spec(s State, a Attempt) SessionSpec {
	t := s.Tasks[a.TaskID]
	return SessionSpec{Reference: a.SessionRef, TaskID: t.ID, AttemptID: a.ID, InstanceID: s.InstanceID, Directory: a.Workspace.Path, Role: t.Role, Title: t.Title}
}

func (e *Engine) Cleanup(ctx context.Context, op, id string) (Workspace, error) {
	e.effects.Lock()
	defer e.effects.Unlock()
	s, err := e.Snapshot(ctx)
	if err != nil {
		return Workspace{}, err
	}
	a, ok := s.Attempts[id]
	if !ok {
		return Workspace{}, NotFound("attempt not found")
	}
	t := s.Tasks[a.TaskID]
	// Check for an existing operation before touching external resources.
	if old, ok := s.Operations[op]; ok {
		var w Workspace
		hash := sha256.Sum256([]byte("workspace.cleanup/" + id + "\n{}"))
		if old.Fingerprint != hex.EncodeToString(hash[:]) {
			return w, &Error{Code: "operation_conflict", Message: "operation ID conflict"}
		}
		return w, json.Unmarshal(old.Outcome, &w)
	}
	if op == "" {
		return Workspace{}, Invalid("operation ID required")
	}
	if a.Phase == "cleaned" {
		return a.Workspace, Invalid("workspace is already cleaned; retry its original operation ID for its receipt")
	}
	if !cleanupEligible(&s, a) {
		return a.Workspace, Invalid("task must be terminal without unsettled dispatch before cleanup")
	}
	o, err := e.runtime.Inspect(ctx, spec(s, a))
	if err != nil {
		return a.Workspace, err
	}
	if !o.Exists || !o.Owned || (o.State != "idle" && o.State != "failed") {
		return a.Workspace, Invalid("runtime quiescence and ownership must be proven before cleanup")
	}
	checker, ok := e.runtime.(QuiescenceChecker)
	if !ok {
		return a.Workspace, Invalid("runtime cannot verify background-resource quiescence; cleanup is unavailable")
	}
	if err = checker.CheckQuiescence(ctx, spec(s, a)); err != nil {
		return a.Workspace, err
	}
	return mutate(ctx, e, op, "workspace.cleanup/"+id, struct{}{}, func(s *State) (Workspace, error) {
		v := s.Attempts[id]
		if !cleanupEligible(s, v) {
			return v.Workspace, Invalid("task must be terminal without unsettled dispatch before cleanup")
		}
		v.Phase = "cleanup_pending"
		s.Attempts[id] = v
		c := Command{ID: "cleanup_" + id, TaskID: t.ID, AttemptID: id, Kind: "cleanup", Status: "pending", Generation: v.Generation, Sequence: s.Revision + 1}
		s.Commands[c.ID] = c
		return v.Workspace, nil
	})
}

// Run uses events as hints plus an independent reconciliation cadence. It never
// invokes a model to poll. Shutdown stops this owner, not the runtime workers.
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	go func() {
		backoff := time.Second
		for ctx.Err() == nil {
			_ = e.runtime.Events(ctx, notify)
			notify()
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-wake:
		}
	}
}

func (e *Engine) Tick(ctx context.Context) error {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	info, err := e.runtime.Info(ctx)
	if err != nil || !info.Compatible {
		e.reconciled.Store(false)
		snapshot, x := e.Snapshot(ctx)
		if x == nil {
			for id, a := range snapshot.Attempts {
				t := snapshot.Tasks[a.TaskID]
				if t.AttemptID == id && a.Phase != "cleaned" && t.State != "completed" && t.State != "cancelled" {
					_ = e.mark(ctx, id, "disconnected", errors.New("runtime unavailable or incompatible"))
				}
			}
		}
		return errors.Join(err, errors.New("runtime compatibility unavailable; dispatch disabled"))
	}
	s, err := e.Snapshot(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(s.Attempts))
	for id, a := range s.Attempts {
		t := s.Tasks[a.TaskID]
		terminal := t.State == "completed" || t.State == "failed"
		if a.Phase == "cleanup_pending" || (t.AttemptID == id && a.Phase != "cleaned" && !(terminal && (a.Observed == "idle" || a.Observed == "failed") && !pendingPrompt(&s, id))) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err = e.reconcile(ctx, id); err != nil && ctx.Err() != nil {
			return err
		}
	}
	for {
		var allocated *Attempt
		err = e.store.Update(ctx, func(s *State) error {
			workers, coordinators := 0, 0
			for _, a := range s.Attempts {
				t := s.Tasks[a.TaskID]
				if consumesSlot(s, a) {
					if t.Role == "coordinator" {
						coordinators++
					} else {
						workers++
					}
				}
			}
			tasks := make([]Task, 0)
			for _, t := range s.Tasks {
				if t.Desired == "active" && t.State == "queued" && t.AttemptID == "" {
					tasks = append(tasks, t)
				}
			}
			sort.Slice(tasks, func(i, j int) bool {
				if tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
					return tasks[i].ID < tasks[j].ID
				}
				return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
			})
			for _, t := range tasks {
				if t.Role == "worker" && workers >= e.opts.Concurrency || t.Role == "coordinator" && coordinators >= 1 {
					continue
				}
				eligible := true
				for _, dep := range t.Dependencies {
					if s.Tasks[dep].State != "completed" {
						eligible = false
					}
				}
				for parent := t.ParentID; parent != ""; parent = s.Tasks[parent].ParentID {
					if s.Tasks[parent].Desired != "active" {
						eligible = false
					}
				}
				if !eligible {
					continue
				}
				a := Attempt{ID: ID("run_"), TaskID: t.ID, RuntimeID: e.opts.RuntimeID, Phase: "workspace_pending", Observed: "unknown", Generation: t.Generation, CreatedAt: now(), UpdatedAt: now()}
				a.SessionRef = e.runtime.Reference(a.ID)
				a.Workspace = e.workspaces.Plan(s.Projects[t.ProjectID], a.ID)
				s.Attempts[a.ID] = a
				t.AttemptID = a.ID
				t.State = "dispatching"
				s.Tasks[t.ID] = t
				allocated = &a
				break
			}
			return nil
		})
		if err != nil {
			return err
		}
		if allocated == nil {
			break
		}
		if err = e.reconcile(ctx, allocated.ID); err != nil && ctx.Err() != nil {
			return err
		}
	}
	if err = e.notifyCoordinators(ctx); err != nil {
		return err
	}
	e.reconciled.Store(true)
	return nil
}

func (e *Engine) bookkeeping(ctx context.Context, update func(*State) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return e.store.Update(ctx, update)
}

func (e *Engine) mark(ctx context.Context, id, state string, err error) error {
	return e.bookkeeping(ctx, func(s *State) error {
		a := s.Attempts[id]
		before := a
		a.Observed = state
		if err != nil {
			a.Error = err.Error()
			if a.FailureCount < 8 {
				a.FailureCount++
			}
			if a.FailureCount >= 8 && s.Tasks[a.TaskID].Desired == "cancelled" {
				a.Error += "; cancellation retry budget exhausted; repeat cancel to retry"
			}
		}
		if message := dispatchBudgetError(s, a); message != "" {
			a.Error = message
		}
		if a.Observed != before.Observed || a.Error != before.Error || a.FailureCount != before.FailureCount {
			a.UpdatedAt = now()
		}
		s.Attempts[id] = a
		t := s.Tasks[a.TaskID]
		if (state == "unknown" || state == "disconnected" || errors.Is(err, ErrUncertain)) && t.State != "completed" && !cleanupBarrier(a) {
			if t.State != "recovering" || t.Error != a.Error {
				t.UpdatedAt = now()
			}
			t.State = "recovering"
			t.Error = a.Error
			s.Tasks[t.ID] = t
		}
		return nil
	})
}

func (e *Engine) reconcile(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	e.effects.Lock()
	defer e.effects.Unlock()
	s, err := e.Snapshot(ctx)
	if err != nil {
		return err
	}
	a := s.Attempts[id]
	t := s.Tasks[a.TaskID]
	p := s.Projects[t.ProjectID]
	if a.Phase == "cleanup_pending" {
		if !cleanupEligible(&s, a) {
			return Invalid("cleanup requires a terminal task without unsettled dispatch")
		}
		observation, x := e.runtime.Inspect(ctx, spec(s, a))
		if x == nil && (!observation.Exists || !observation.Owned || (observation.State != "idle" && observation.State != "failed")) {
			x = Invalid("cleanup waits for confirmed runtime ownership and quiescence")
		}
		if x == nil {
			checker, ok := e.runtime.(QuiescenceChecker)
			if !ok {
				x = Invalid("runtime cannot verify background-resource quiescence")
			} else {
				x = checker.CheckQuiescence(ctx, spec(s, a))
			}
		}
		if x == nil {
			x = e.workspaces.Remove(ctx, p, a.Workspace)
		}
		if x != nil && !errors.Is(x, ErrWorkspaceMissing) {
			return e.bookkeeping(ctx, func(s *State) error { v := s.Attempts[id]; v.Error = x.Error(); s.Attempts[id] = v; return nil })
		}
		return e.bookkeeping(ctx, func(s *State) error {
			v := s.Attempts[id]
			v.Workspace.Allocated = false
			v.Phase = "cleaned"
			v.Error = ""
			s.Attempts[id] = v
			c := s.Commands["cleanup_"+id]
			c.Status = "completed"
			s.Commands[c.ID] = c
			return nil
		})
	}
	if a.Phase == "workspace_pending" || a.Phase == "session_pending" {
		if t.Desired != "active" {
			return e.bookkeeping(ctx, func(s *State) error {
				v := s.Tasks[t.ID]
				v.State = v.Desired
				s.Tasks[v.ID] = v
				a := s.Attempts[id]
				a.Observed = "idle"
				s.Attempts[id] = a
				return nil
			})
		}
		for parent := t.ParentID; parent != ""; parent = s.Tasks[parent].ParentID {
			if s.Tasks[parent].Desired != "active" {
				return e.store.Update(ctx, func(s *State) error {
					v := s.Tasks[t.ID]
					v.State = "queued"
					s.Tasks[v.ID] = v
					vAttempt := s.Attempts[id]
					vAttempt.Observed = "idle"
					s.Attempts[id] = vAttempt
					return nil
				})
			}
		}
	}
	if a.RuntimeID != e.opts.RuntimeID {
		return e.mark(ctx, id, "unknown", errors.New("runtime binding changed; explicit reconciliation required"))
	}
	if (a.Phase == "workspace_pending" || a.Phase == "session_pending") && a.Observed == "idle" && t.Desired == "active" {
		if !e.capacityAvailable(&s, a) {
			return nil
		}
		a.Observed = "unknown"
		if err = e.store.Update(ctx, func(s *State) error { s.Attempts[id] = a; return nil }); err != nil {
			return err
		}
	}
	if a.Phase == "workspace_pending" {
		if a.FailureCount >= 8 {
			return nil
		}
		w, x := e.workspaces.Ensure(ctx, p, a.Workspace)
		if x != nil {
			return e.mark(ctx, id, "unknown", x)
		}
		a.Workspace = w
		a.Phase = "session_pending"
		if err = e.bookkeeping(ctx, func(s *State) error { s.Attempts[id] = a; return nil }); err != nil {
			return err
		}
	}
	if err = e.workspaces.Inspect(ctx, p, a.Workspace); err != nil {
		return e.mark(ctx, id, "unknown", err)
	}
	sp := spec(s, a)
	if a.Phase == "session_pending" {
		if a.FailureCount >= 8 {
			observed, x := e.runtime.Inspect(ctx, sp)
			if x != nil || !observed.Exists || !observed.Owned {
				return nil
			}
		} else if err = e.runtime.Ensure(ctx, sp); err != nil {
			return e.mark(ctx, id, "unknown", err)
		}
		if e.opts.AfterCreate != nil {
			if err = e.opts.AfterCreate(a); err != nil {
				return err
			}
		}
		a.Phase = "session_bound"
		a.FailureCount = 0
		if err = e.bookkeeping(ctx, func(s *State) error {
			s.Attempts[id] = a
			c := Command{ID: "initial_" + id, TaskID: t.ID, AttemptID: id, Kind: "prompt", Text: workerPrompt(s, t, a), Status: "pending", Generation: t.Generation, Sequence: s.Revision + 1}
			s.Commands[c.ID] = c
			return nil
		}); err != nil {
			return err
		}
	}
	o, err := e.runtime.Inspect(ctx, sp)
	if err != nil {
		return e.mark(ctx, id, "disconnected", err)
	}
	if !o.Exists || !o.Owned {
		return e.mark(ctx, id, "unknown", errors.New("session missing or ownership mismatch; no replacement dispatched"))
	}
	if o.State == "unknown" || o.State == "disconnected" {
		return e.mark(ctx, id, o.State, ErrUncertain)
	}
	if err = e.bookkeeping(ctx, func(s *State) error {
		v := s.Attempts[id]
		clearError := !uncertainDispatch(s, id) && dispatchBudgetError(s, v) == "" && !(s.Tasks[v.TaskID].Desired == "cancelled" && v.FailureCount >= 8)
		if v.Observed != o.State || (v.Error != "" && clearError) {
			v.UpdatedAt = now()
		}
		v.Observed = o.State
		if clearError {
			v.Error = ""
		}
		s.Attempts[id] = v
		seen := map[string]bool{}
		for _, d := range o.Decisions {
			key := sha256.Sum256([]byte(id + "\x00" + d.Kind + "\x00" + d.RuntimeRef))
			d.ID = "dec_" + hex.EncodeToString(key[:16])
			d.TaskID = t.ID
			d.AttemptID = id
			seen[d.ID] = true
			if old, ok := s.Decisions[d.ID]; ok {
				d.Status = old.Status
				d.Reply = old.Reply
				d.Error = old.Error
			} else {
				d.Status = "pending"
			}
			s.Decisions[d.ID] = d
		}
		for key, d := range s.Decisions {
			if d.AttemptID == id && !seen[key] && (d.Status == "pending" || d.Status == "reply_pending" || d.Status == "uncertain") {
				if d.Status != "uncertain" {
					d.Status = "expired"
				}
				s.Decisions[key] = d
			}
		}
		refreshTaskState(s, t.ID, o.State)
		return nil
	}); err != nil {
		return err
	}
	if t.Desired != "active" {
		// An admission request may still be preparing after its client timed out.
		// Runtime idleness alone cannot settle pause/cancel in that window.
		uncertainAdmission := false
		for key, c := range s.Commands {
			if c.AttemptID == id && c.Kind == "prompt" && c.Status == "sending" {
				reconciler, ok := e.runtime.(PromptReconciler)
				if !ok || reconciler.ReconcilePrompt(ctx, sp, c.ID, c.Text) != nil {
					uncertainAdmission = true
					continue
				}
				if err = e.bookkeeping(ctx, func(s *State) error {
					v := s.Commands[key]
					v.Status = "admitted"
					s.Commands[key] = v
					a := s.Attempts[id]
					a.FailureCount = 0
					s.Attempts[id] = a
					return nil
				}); err != nil {
					return err
				}
				a.FailureCount = 0
			}
		}
		cancelPending, canCancel := e.runtime.(PendingCanceller)
		if o.State != "idle" && o.State != "failed" || (t.Desired == "cancelled" && canCancel && t.State != t.Desired) {
			if a.FailureCount >= 8 {
				return nil
			}
			if t.Desired == "cancelled" && canCancel {
				err = cancelPending.CancelPending(ctx, sp)
			} else {
				err = e.runtime.Interrupt(ctx, sp)
			}
			if err != nil {
				return e.mark(ctx, id, "unknown", err)
			}
			o, err = e.runtime.Inspect(ctx, sp)
			if err != nil {
				return e.mark(ctx, id, "disconnected", err)
			}
			if o.State != "idle" {
				return e.mark(ctx, id, "unknown", ErrUncertain)
			}
		}
		if uncertainAdmission {
			return e.mark(ctx, id, o.State, ErrUncertain)
		}
		return e.bookkeeping(ctx, func(s *State) error {
			v := s.Tasks[t.ID]
			if v.State != v.Desired {
				v.State = v.Desired
				v.UpdatedAt = now()
			}
			s.Tasks[v.ID] = v
			a := s.Attempts[id]
			a.Observed = "idle"
			s.Attempts[id] = a
			for key, c := range s.Commands {
				if c.AttemptID == id && (c.Status == "pending" || c.Status == "parked") {
					c.Status = "parked"
					if t.Desired == "cancelled" {
						c.Status = "cancelled"
					}
					s.Commands[key] = c
				}
			}
			return nil
		})
	}
	s, err = e.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, d := range s.Decisions {
		if d.AttemptID == id && d.Status == "uncertain" {
			return nil
		}
		if d.AttemptID == id && d.Status == "reply_pending" {
			if a.FailureCount >= 8 || ((o.State == "idle" || o.State == "failed") && !e.capacityAvailable(&s, a)) {
				return nil
			}
			if err = e.store.Update(ctx, func(s *State) error {
				v := s.Decisions[d.ID]
				v.Status = "uncertain"
				s.Decisions[d.ID] = v
				return nil
			}); err != nil {
				return err
			}
			if err = e.runtime.Reply(ctx, sp, d, d.Reply); err != nil {
				var undelivered *NotDelivered
				var rejected *Error
				if errors.As(err, &rejected) && rejected.Code == "invalid_request" {
					return e.bookkeeping(ctx, func(s *State) error {
						v := s.Decisions[d.ID]
						v.Status = "pending"
						v.Reply = nil
						v.Error = rejected.Message
						s.Decisions[d.ID] = v
						t := s.Tasks[v.TaskID]
						t.State = "waiting_for_input"
						t.Error = "decision reply was rejected; submit a corrected answer"
						s.Tasks[t.ID] = t
						return nil
					})
				}
				if errors.As(err, &undelivered) {
					if x := e.bookkeeping(ctx, func(s *State) error {
						v := s.Decisions[d.ID]
						v.Status = "reply_pending"
						s.Decisions[d.ID] = v
						return nil
					}); x != nil {
						return x
					}
				}
				return e.mark(ctx, id, "unknown", err)
			}
			if err = e.bookkeeping(ctx, func(s *State) error {
				v := s.Decisions[d.ID]
				v.Status = "answered"
				s.Decisions[d.ID] = v
				a := s.Attempts[id]
				a.Phase = "executing"
				a.Observed = "running"
				a.FailureCount = 0
				a.Error = ""
				s.Attempts[id] = a
				refreshTaskState(s, a.TaskID, "running")
				return nil
			}); err != nil {
				return err
			}
		}
	}
	commands := make([]Command, 0)
	for _, c := range s.Commands {
		if c.AttemptID == id && (c.Status == "pending" || c.Status == "sending" || c.Status == "parked") {
			commands = append(commands, c)
		}
	}
	sort.Slice(commands, func(i, j int) bool {
		if commands[i].Sequence == commands[j].Sequence {
			return commands[i].ID < commands[j].ID
		}
		return commands[i].Sequence < commands[j].Sequence
	})
	for _, c := range commands {
		if c.Kind != "prompt" {
			continue
		}
		if c.Status == "sending" {
			reconciler, ok := e.runtime.(PromptReconciler)
			if !ok {
				err = ErrUncertain
			} else {
				err = reconciler.ReconcilePrompt(ctx, sp, c.ID, c.Text)
			}
		} else {
			if (o.State == "idle" || o.State == "failed") && !e.capacityAvailable(&s, a) {
				return nil
			}
			if s.Attempts[id].FailureCount >= 8 {
				return nil
			}
			if err = e.store.Update(ctx, func(s *State) error { v := s.Commands[c.ID]; v.Status = "sending"; s.Commands[c.ID] = v; return nil }); err != nil {
				return err
			}
			err = e.runtime.Prompt(ctx, sp, c.ID, c.Text)
		}
		if err != nil {
			var undelivered *NotDelivered
			if errors.As(err, &undelivered) {
				if x := e.bookkeeping(ctx, func(s *State) error { v := s.Commands[c.ID]; v.Status = "pending"; s.Commands[c.ID] = v; return nil }); x != nil {
					return x
				}
			}
			observed := "unknown"
			if c.Status == "sending" && errors.Is(err, ErrUncertain) {
				observed = o.State
			}
			return e.mark(ctx, id, observed, err)
		}
		if err = e.bookkeeping(ctx, func(s *State) error {
			v := s.Commands[c.ID]
			v.Status = "admitted"
			s.Commands[c.ID] = v
			a := s.Attempts[id]
			a.Phase = "executing"
			a.FailureCount = 0
			a.Observed = "running"
			a.Error = ""
			s.Attempts[id] = a
			refreshTaskState(s, a.TaskID, "running")
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func workerPrompt(s *State, t Task, a Attempt) string {
	p := s.Projects[t.ProjectID]
	role := "You are a Tandem worker. Work only in the assigned checkout. Do not spawn unmanaged subagents, merge, publish, delete worktrees, or answer permission requests automatically. Worktrees are not security sandboxes."
	if t.Role == "coordinator" {
		role = "You are a Tandem coordinator. Do not edit project source. Use Tandem tools to inspect project tasks, route followups, and create bounded subtasks with explicit parent and dependencies. Never start workers outside Tandem. Ask when project/task routing is ambiguous. Wait for evidence and report outcomes; idleness does not mean completion."
	}
	return fmt.Sprintf("%s\nInstance: %s\nTask: %s\nAttempt: %s\nInstruction generation: %d\nProject: %s\nAssigned directory: %s\nBase: %s (%s)\n\nProject instructions:\n%s\n\nTask:\n%s\n\nReport explicit results and verification references via Tandem's task result operation, including this instruction generation. Keep secrets out of results and logs.\n", role, s.InstanceID, t.ID, a.ID, t.Generation, p.ID, a.Workspace.Path, a.Workspace.BaseRef, a.Workspace.BaseSHA, p.Instructions, t.Instructions)
}
