// Package core defines Tandem's runtime-independent coordination contract.
package core

import (
	"context"
	"encoding/json"
	"time"
)

const Version = "0.1.0"

type Project struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Path             string    `json:"path"`
	BaseRef          string    `json:"baseRef"`
	Instructions     string    `json:"instructions"`
	InstructionFiles []string  `json:"instructionFiles,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
}

type Task struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"projectId"`
	ParentID     string    `json:"parentId,omitempty"`
	Dependencies []string  `json:"dependencies"`
	Title        string    `json:"title"`
	Instructions string    `json:"instructions"`
	Role         string    `json:"role"`
	Desired      string    `json:"desired"`
	State        string    `json:"state"`
	Generation   int       `json:"generation"`
	AttemptID    string    `json:"attemptId,omitempty"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type Attempt struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"taskId"`
	RuntimeID    string    `json:"runtimeId"`
	SessionRef   string    `json:"sessionRef"`
	Workspace    Workspace `json:"workspace"`
	Phase        string    `json:"phase"`
	Observed     string    `json:"observed"`
	Generation   int       `json:"generation"`
	FailureCount int       `json:"failureCount"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type Workspace struct {
	Allocator string `json:"allocator"`
	Owner     string `json:"owner"`
	Path      string `json:"path"`
	Branch    string `json:"branch"`
	BaseRef   string `json:"baseRef"`
	BaseSHA   string `json:"baseSha"`
	Allocated bool   `json:"allocated"`
}

type Command struct {
	ID         string `json:"id"`
	TaskID     string `json:"taskId"`
	AttemptID  string `json:"attemptId"`
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	Status     string `json:"status"`
	Generation int    `json:"generation"`
	Sequence   int64  `json:"sequence"`
}

type Decision struct {
	ID         string          `json:"id"`
	TaskID     string          `json:"taskId"`
	AttemptID  string          `json:"attemptId"`
	RuntimeRef string          `json:"runtimeRef"`
	Kind       string          `json:"kind"`
	Prompt     string          `json:"prompt"`
	Details    json.RawMessage `json:"details,omitempty"`
	Status     string          `json:"status"`
	Reply      json.RawMessage `json:"reply,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type Evidence struct {
	Name      string `json:"name"`
	Reference string `json:"reference"`
	Passed    bool   `json:"passed"`
}

type Result struct {
	TaskID     string     `json:"taskId"`
	AttemptID  string     `json:"attemptId"`
	Generation int        `json:"generation"`
	Summary    string     `json:"summary"`
	Evidence   []Evidence `json:"evidence"`
	Accepted   bool       `json:"accepted"`
	Late       bool       `json:"late"`
	CreatedAt  time.Time  `json:"createdAt"`
}

type Operation struct {
	ID          string          `json:"id"`
	Fingerprint string          `json:"fingerprint"`
	Outcome     json.RawMessage `json:"outcome"`
	CreatedAt   time.Time       `json:"createdAt"`
}

type State struct {
	InstanceID string               `json:"instanceId"`
	Revision   int64                `json:"revision"`
	Projects   map[string]Project   `json:"projects"`
	Tasks      map[string]Task      `json:"tasks"`
	Attempts   map[string]Attempt   `json:"attempts"`
	Commands   map[string]Command   `json:"commands"`
	Decisions  map[string]Decision  `json:"decisions"`
	Results    map[string]Result    `json:"results"`
	Operations map[string]Operation `json:"-"`
}

func NewState() State {
	return State{Projects: map[string]Project{}, Tasks: map[string]Task{}, Attempts: map[string]Attempt{}, Commands: map[string]Command{}, Decisions: map[string]Decision{}, Results: map[string]Result{}, Operations: map[string]Operation{}}
}

type Store interface {
	Snapshot(context.Context) (State, error)
	Update(context.Context, func(*State) error) error
	Close() error
}

// RevisionSource permits a cheap atomic invalidation check without loading
// retained content or the operation journal. Implementations must serialize it
// with updates just as they serialize Snapshot. It does not expose storage APIs.
type RevisionSource interface {
	ReadRevision(context.Context) (string, int64, error)
}

type RuntimeInfo struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	Compatible bool   `json:"compatible"`
	Detail     string `json:"detail,omitempty"`
}

type SessionSpec struct {
	Reference  string
	TaskID     string
	AttemptID  string
	InstanceID string
	Directory  string
	Role       string
	Title      string
}

type Observation struct {
	Exists    bool
	Owned     bool
	State     string // running, idle, failed, disconnected, unknown
	Decisions []Decision
}

// Runtime external writes must be recoverable by the exact reference or return
// ErrUncertain. A timeout is never evidence that an action did not happen.
type Runtime interface {
	Info(context.Context) (RuntimeInfo, error)
	Reference(string) string
	Ensure(context.Context, SessionSpec) error
	Inspect(context.Context, SessionSpec) (Observation, error)
	Prompt(context.Context, SessionSpec, string, string) error
	Interrupt(context.Context, SessionSpec) error
	Reply(context.Context, SessionSpec, Decision, json.RawMessage) error
	Events(context.Context, func()) error
}

// PromptReconciler checks an uncertain admission without a wake-capable retry.
type PromptReconciler interface {
	ReconcilePrompt(context.Context, SessionSpec, string, string) error
}

// PendingCanceller removes only this attempt's owned pending runtime inputs.
type PendingCanceller interface {
	CancelPending(context.Context, SessionSpec) error
}

// QuiescenceChecker verifies runtime-owned jobs before destructive checkout
// cleanup. A foreground-idle session alone is not sufficient for that action.
type QuiescenceChecker interface {
	CheckQuiescence(context.Context, SessionSpec) error
}

type Workspaces interface {
	Plan(Project, string) Workspace
	Ensure(context.Context, Project, Workspace) (Workspace, error)
	Inspect(context.Context, Project, Workspace) error
	Remove(context.Context, Project, Workspace) error
}

type CreateTask struct {
	ProjectID    string   `json:"projectId"`
	ParentID     string   `json:"parentId,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	Title        string   `json:"title"`
	Instructions string   `json:"instructions"`
	Role         string   `json:"role,omitempty"`
}

type RegisterProject struct {
	Name             string   `json:"name"`
	Path             string   `json:"path"`
	BaseRef          string   `json:"baseRef"`
	Instructions     string   `json:"instructions,omitempty"`
	InstructionFiles []string `json:"instructionFiles,omitempty"`
}

type Followup struct {
	Text string `json:"text"`
}
type Control struct {
	Action  string `json:"action"`
	Cascade bool   `json:"cascade"`
}
type SubmitResult struct {
	AttemptID  string     `json:"attemptId"`
	Generation int        `json:"generation"`
	Summary    string     `json:"summary"`
	Evidence   []Evidence `json:"evidence"`
}
type ReplyDecision struct {
	Answer json.RawMessage `json:"answer"`
}

// Service is the common command/read surface consumed by API clients.
type Service interface {
	Snapshot(context.Context) (State, error)
	RuntimeInfo(context.Context) (RuntimeInfo, error)
	Register(context.Context, string, RegisterProject) (Project, error)
	Create(context.Context, string, CreateTask) (Task, error)
	Followup(context.Context, string, string, Followup) (Task, error)
	Control(context.Context, string, string, Control) (Task, error)
	Reply(context.Context, string, string, ReplyDecision) (Decision, error)
	SubmitResult(context.Context, string, string, SubmitResult) (Result, error)
	Accept(context.Context, string, string) (Task, error)
	Cleanup(context.Context, string, string) (Workspace, error)
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string      { return e.Message }
func Invalid(message string) error  { return &Error{Code: "invalid_request", Message: message} }
func NotFound(message string) error { return &Error{Code: "not_found", Message: message} }

var ErrUncertain = &Error{Code: "uncertain", Message: "external outcome cannot be established; reconciliation required"}

type NotDelivered struct{ Err error }

func (e *NotDelivered) Error() string { return e.Err.Error() }
func (e *NotDelivered) Unwrap() error { return e.Err }

var ErrWorkspaceMissing = &Error{Code: "workspace_missing", Message: "owned workspace directory is absent"}
