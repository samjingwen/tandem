// Package testruntime is a controlled in-memory runtime for deterministic
// failure injection. It never invokes a model or executes project commands.
package testruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/samjingwen/tandem/core"
)

type Session struct {
	Spec      core.SessionSpec
	State     string
	Prompts   map[string]string
	Decisions []core.Decision
}
type Fake struct {
	Mu                   sync.Mutex
	Sessions             map[string]*Session
	Creates              int
	EnsureRequests       int
	EnsureFailures       bool
	PromptCalls          int
	PromptRequests       int
	PromptError          error
	DeferPromptAdmission bool
	Interrupts           int
	Disconnected         bool
	LoseCreateResponse   bool
	LosePromptResponse   bool
	LoseReplyResponse    bool
	ReplyError           error
	BeforeCreate         func()
	Wake                 chan struct{}
	EventStarted         chan struct{}
	EventDropped         chan struct{}
}

func New() *Fake {
	return &Fake{Sessions: map[string]*Session{}, Wake: make(chan struct{}, 16), EventStarted: make(chan struct{}, 16), EventDropped: make(chan struct{}, 16)}
}
func (f *Fake) Info(context.Context) (core.RuntimeInfo, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Disconnected {
		return core.RuntimeInfo{}, errors.New("injected runtime disconnect")
	}
	return core.RuntimeInfo{ID: "fake", Version: "test", Compatible: true}, nil
}
func (f *Fake) Reference(id string) string { return "fake_" + id }
func (f *Fake) Ensure(_ context.Context, s core.SessionSpec) error {
	if f.BeforeCreate != nil {
		f.BeforeCreate()
	}
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.EnsureRequests++
	if f.EnsureFailures {
		return core.ErrUncertain
	}
	if f.Disconnected {
		return core.ErrUncertain
	}
	if old, ok := f.Sessions[s.Reference]; ok {
		if old.Spec != s {
			return errors.New("ownership mismatch")
		}
		return nil
	}
	f.Sessions[s.Reference] = &Session{Spec: s, State: "idle", Prompts: map[string]string{}}
	f.Creates++
	if f.LoseCreateResponse {
		f.LoseCreateResponse = false
		return core.ErrUncertain
	}
	return nil
}
func (f *Fake) Inspect(_ context.Context, s core.SessionSpec) (core.Observation, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Disconnected {
		return core.Observation{}, errors.New("injected disconnect")
	}
	v, ok := f.Sessions[s.Reference]
	if !ok {
		return core.Observation{}, nil
	}
	return core.Observation{Exists: true, Owned: v.Spec.Reference == s.Reference && v.Spec.TaskID == s.TaskID && v.Spec.AttemptID == s.AttemptID && v.Spec.InstanceID == s.InstanceID && v.Spec.Directory == s.Directory, State: v.State, Decisions: append([]core.Decision{}, v.Decisions...)}, nil
}
func (f *Fake) Prompt(_ context.Context, s core.SessionSpec, id, text string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.PromptRequests++
	if f.PromptError != nil {
		return f.PromptError
	}
	if f.DeferPromptAdmission {
		return core.ErrUncertain
	}
	v := f.Sessions[s.Reference]
	if v == nil {
		return core.ErrUncertain
	}
	if old, ok := v.Prompts[id]; ok {
		if old != text {
			return errors.New("conflicting message ID")
		}
		return nil
	}
	v.Prompts[id] = text
	v.State = "running"
	f.PromptCalls++
	if f.LosePromptResponse {
		f.LosePromptResponse = false
		return core.ErrUncertain
	}
	return nil
}
func (f *Fake) Interrupt(_ context.Context, s core.SessionSpec) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.Disconnected {
		return core.ErrUncertain
	}
	v := f.Sessions[s.Reference]
	if v == nil {
		return core.ErrUncertain
	}
	v.State = "idle"
	f.Interrupts++
	return nil
}

func (f *Fake) CancelPending(ctx context.Context, s core.SessionSpec) error {
	return f.Interrupt(ctx, s)
}
func (f *Fake) CheckQuiescence(ctx context.Context, s core.SessionSpec) error {
	observation, err := f.Inspect(ctx, s)
	if err != nil {
		return err
	}
	if !observation.Exists || !observation.Owned || len(observation.Decisions) > 0 || (observation.State != "idle" && observation.State != "failed") {
		return core.Invalid("fake runtime is not quiescent")
	}
	return nil
}
func (f *Fake) ReconcilePrompt(_ context.Context, s core.SessionSpec, id, text string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	v := f.Sessions[s.Reference]
	if v == nil {
		return core.ErrUncertain
	}
	if actual, ok := v.Prompts[id]; !ok || actual != text {
		return core.ErrUncertain
	}
	return nil
}
func (f *Fake) Reply(_ context.Context, s core.SessionSpec, d core.Decision, _ json.RawMessage) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.ReplyError != nil {
		return f.ReplyError
	}
	v := f.Sessions[s.Reference]
	if v == nil {
		return core.ErrUncertain
	}
	for i, x := range v.Decisions {
		if x.RuntimeRef == d.RuntimeRef {
			v.Decisions = append(v.Decisions[:i], v.Decisions[i+1:]...)
			if f.LoseReplyResponse {
				f.LoseReplyResponse = false
				return core.ErrUncertain
			}
			return nil
		}
	}
	return errors.New("decision no longer pending")
}
func (f *Fake) Events(ctx context.Context, notify func()) error {
	select {
	case f.EventStarted <- struct{}{}:
	default:
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.Wake:
			notify()
			select {
			case f.EventDropped <- struct{}{}:
			default:
			}
			return errors.New("injected event connection drop")
		}
	}
}
func (f *Fake) SetState(ref, state string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Sessions[ref].State = state
}
func (f *Fake) SetDecisions(ref string, d ...core.Decision) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Sessions[ref].Decisions = d
}
func (f *Fake) Counts() (int, int, int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.Creates, f.PromptCalls, f.Interrupts
}
