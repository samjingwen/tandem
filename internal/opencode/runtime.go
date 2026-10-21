package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"

	"github.com/samjingwen/tandem/core"
)

type ownership struct {
	InstanceID string `json:"instanceId"`
	TaskID     string `json:"taskId"`
	AttemptID  string `json:"attemptId"`
	CommandID  string `json:"commandId,omitempty"`
}
type metadata struct {
	Tandem ownership `json:"tandem"`
}
type location struct {
	Directory string `json:"directory"`
}
type session struct {
	ID       string   `json:"id"`
	Metadata metadata `json:"metadata"`
	Location location `json:"location"`
	Outcome  string   `json:"outcome"`
}

var sessionID = regexp.MustCompile(`^ses_[a-zA-Z0-9]+$`)

func validate(spec core.SessionSpec) error {
	if !sessionID.MatchString(spec.Reference) || spec.TaskID == "" || spec.AttemptID == "" || spec.InstanceID == "" || !filepath.IsAbs(spec.Directory) {
		return core.Invalid("session requires a valid preallocated reference, ownership, and absolute directory")
	}
	return nil
}

func owner(spec core.SessionSpec, command string) metadata {
	return metadata{Tandem: ownership{InstanceID: spec.InstanceID, TaskID: spec.TaskID, AttemptID: spec.AttemptID, CommandID: command}}
}
func owned(s session, spec core.SessionSpec) bool {
	return s.ID == spec.Reference && s.Metadata == owner(spec, "") && s.Location.Directory == spec.Directory
}

func (r *Runtime) owned(ctx context.Context, spec core.SessionSpec) error {
	if err := validate(spec); err != nil {
		return err
	}
	if err := r.compatible(ctx); err != nil {
		return err
	}
	s, err := getData[session](r, ctx, sessionPath(spec))
	if err != nil {
		return err
	}
	if !owned(s, spec) {
		return ErrOwnership
	}
	return nil
}

func (r *Runtime) Ensure(ctx context.Context, spec core.SessionSpec) error {
	if err := validate(spec); err != nil {
		return err
	}
	if err := r.compatible(ctx); err != nil {
		return err
	}
	s, err := getData[session](r, ctx, sessionPath(spec))
	if err == nil {
		if !owned(s, spec) {
			return ErrOwnership
		}
		return nil
	}
	if !statusIs(err, http.StatusNotFound) {
		return err
	}
	input := struct {
		ID       string   `json:"id"`
		Title    string   `json:"title"`
		Agent    string   `json:"agent,omitempty"`
		Model    *Model   `json:"model,omitempty"`
		Location location `json:"location"`
		Metadata metadata `json:"metadata"`
	}{spec.Reference, spec.Title, r.agent, r.model, location{spec.Directory}, owner(spec, "")}
	var result envelope[session]
	if err := r.request(ctx, http.MethodPost, "/api/session", input, &result); err != nil {
		return err
	}
	if result.Data == nil {
		return core.ErrUncertain
	}
	// OpenCode returns an existing ID without comparing the create payload.
	if !owned(*result.Data, spec) {
		return ErrOwnership
	}
	return nil
}

func (r *Runtime) Inspect(ctx context.Context, spec core.SessionSpec) (core.Observation, error) {
	observation := core.Observation{State: "unknown"}
	if err := validate(spec); err != nil {
		return observation, err
	}
	if err := r.compatible(ctx); err != nil {
		return observation, err
	}
	s, err := getData[session](r, ctx, sessionPath(spec))
	if statusIs(err, http.StatusNotFound) {
		return observation, nil
	}
	if err != nil {
		return observation, err
	}
	observation.Exists, observation.Owned = true, owned(s, spec)
	if !observation.Owned {
		return observation, nil
	}
	active, err := getData[map[string]struct {
		Type string `json:"type"`
	}](r, ctx, "/api/session/active")
	if err != nil {
		return observation, err
	}
	observation.State = "idle"
	if current, ok := active[spec.Reference]; ok {
		observation.State = "unknown"
		if current.Type == "running" {
			observation.State = "running"
		}
	} else if s.Outcome == "failed" {
		observation.State = "failed"
	}
	observation.Decisions, err = r.decisions(ctx, spec)
	return observation, err
}

type promptPayload struct {
	Text     string   `json:"text"`
	Metadata metadata `json:"metadata"`
}
type admission struct {
	ID        string        `json:"id"`
	SessionID string        `json:"sessionID"`
	Type      string        `json:"type"`
	Payload   promptPayload `json:"payload"`
}
type message struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Text     string   `json:"text"`
	Metadata metadata `json:"metadata"`
}

func verifyMessage(m message, id, text string, want metadata) error {
	if m.ID != id || m.Type != "user" || m.Text != text || m.Metadata != want {
		return ErrOwnership
	}
	return nil
}
func verifyAdmission(a admission, spec core.SessionSpec, id, text string, want metadata) error {
	if a.SessionID != spec.Reference {
		return ErrOwnership
	}
	return verifyMessage(message{a.ID, a.Type, a.Payload.Text, a.Payload.Metadata}, id, text, want)
}

// Prompt admits a durable command once. A known admission is not re-posted:
// OpenCode's same-ID retry with resume=true would wake a parked session.
func (r *Runtime) Prompt(ctx context.Context, spec core.SessionSpec, commandID, text string) error {
	if commandID == "" || text == "" {
		return core.Invalid("prompt requires command ID and text")
	}
	if err := r.owned(ctx, spec); err != nil {
		return err
	}
	id := "msg_" + digest("tandem/prompt/"+commandID)
	want := owner(spec, commandID)
	found, err := r.findPrompt(ctx, spec, id, text, want)
	if err != nil || found {
		return err
	}
	input := struct {
		ID       string   `json:"id"`
		Text     string   `json:"text"`
		Metadata metadata `json:"metadata"`
		Delivery string   `json:"delivery"`
		Resume   bool     `json:"resume"`
	}{id, text, want, "steer", true}
	var result envelope[admission]
	if err := r.request(ctx, http.MethodPost, sessionPath(spec)+"/prompt", input, &result); err != nil {
		return err
	}
	if result.Data == nil {
		return core.ErrUncertain
	}
	return verifyAdmission(*result.Data, spec, id, text, want)
}

// ReconcilePrompt is read-only recovery for a previously uncertain dispatch.
// Absence is still uncertain: the original admission may still be preparing.
// The engine must not turn that absence into another wake-capable POST.
func (r *Runtime) ReconcilePrompt(ctx context.Context, spec core.SessionSpec, commandID, text string) error {
	if commandID == "" || text == "" {
		return core.Invalid("prompt requires command ID and text")
	}
	if err := r.owned(ctx, spec); err != nil {
		return err
	}
	found, err := r.findPrompt(ctx, spec, "msg_"+digest("tandem/prompt/"+commandID), text, owner(spec, commandID))
	if err != nil {
		return err
	}
	if !found {
		return core.ErrUncertain
	}
	return nil
}

func (r *Runtime) findPrompt(ctx context.Context, spec core.SessionSpec, id, text string, want metadata) (bool, error) {
	path := sessionPath(spec) + "/message/" + id
	for check := 0; check < 2; check++ {
		m, err := getData[message](r, ctx, path)
		if err == nil {
			return true, verifyMessage(m, id, text, want)
		}
		if !statusIs(err, http.StatusNotFound) {
			return false, err
		}
		if check == 1 {
			break
		}
		inbox, err := getData[[]admission](r, ctx, sessionPath(spec)+"/inbox")
		if err != nil {
			return false, err
		}
		for _, item := range inbox {
			if item.ID == id {
				return true, verifyAdmission(item, spec, id, text, want)
			}
		}
		// Recheck projection after the inbox: it may have been delivered between reads.
	}
	return false, nil
}

// Interrupt parks execution, preserving all inbox items. It never resumes or deletes work.
func (r *Runtime) Interrupt(ctx context.Context, spec core.SessionSpec) error {
	if err := r.owned(ctx, spec); err != nil {
		return err
	}
	var result struct {
		Interrupted *bool `json:"interrupted"`
	}
	if err := r.request(ctx, http.MethodPost, sessionPath(spec)+"/interrupt?resume=false", nil, &result); err != nil {
		return err
	}
	if result.Interrupted == nil {
		return core.ErrUncertain
	}
	return nil
}

// CancelPending is an explicit cancellation action, not pause. Call only after
// durable engine cancellation; canceled command IDs must never be re-dispatched.
// It parks execution first, then deletes only verified owned pending user input.
// Foreign and native control inbox items are preserved.
func (r *Runtime) CancelPending(ctx context.Context, spec core.SessionSpec) error {
	if err := r.Interrupt(ctx, spec); err != nil {
		return err
	}
	inbox, err := getData[[]admission](r, ctx, sessionPath(spec)+"/inbox")
	if err != nil {
		return err
	}
	for _, item := range inbox {
		command := item.Payload.Metadata.Tandem.CommandID
		if command == "" || item.Type != "user" || item.SessionID != spec.Reference || item.Payload.Metadata != owner(spec, command) || item.ID != "msg_"+digest("tandem/prompt/"+command) {
			continue
		}
		if err := r.request(ctx, http.MethodDelete, sessionPath(spec)+"/inbox/"+url.PathEscape(item.ID), nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) decisions(ctx context.Context, spec core.SessionSpec) ([]core.Decision, error) {
	var result []core.Decision
	for _, kind := range []string{"permission", "form"} {
		items, err := getData[[]json.RawMessage](r, ctx, sessionPath(spec)+"/"+kind)
		if err != nil {
			return nil, err
		}
		for _, raw := range items {
			var item struct {
				ID        string `json:"id"`
				SessionID string `json:"sessionID"`
				Title     string `json:"title"`
				Action    string `json:"action"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			if item.ID == "" || item.SessionID != spec.Reference {
				return nil, ErrOwnership
			}
			prompt := item.Title
			if kind == "permission" {
				prompt = item.Action
			}
			result = append(result, core.Decision{TaskID: spec.TaskID, AttemptID: spec.AttemptID, RuntimeRef: item.ID, Kind: kind, Prompt: prompt, Details: raw, Status: "pending"})
		}
	}
	return result, nil
}

// Reply accepts the native HTTP body, not the different Promise-client input.
// An already missing decision is uncertain, never presumed answered or reissued.
func (r *Runtime) Reply(ctx context.Context, spec core.SessionSpec, decision core.Decision, answer json.RawMessage) error {
	if decision.TaskID != spec.TaskID || decision.AttemptID != spec.AttemptID || decision.RuntimeRef == "" || (decision.Kind != "permission" && decision.Kind != "form") {
		return ErrOwnership
	}
	if err := r.owned(ctx, spec); err != nil {
		return err
	}
	if err := validateAnswer(decision.Kind, answer); err != nil {
		return err
	}
	pending, err := r.decisions(ctx, spec)
	if err != nil {
		return err
	}
	for _, item := range pending {
		if item.RuntimeRef == decision.RuntimeRef && item.Kind == decision.Kind {
			return r.request(ctx, http.MethodPost, sessionPath(spec)+"/"+decision.Kind+"/"+url.PathEscape(decision.RuntimeRef)+"/reply", answer, nil)
		}
	}
	return core.ErrUncertain
}

func validateAnswer(kind string, raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return core.Invalid("decision reply must be a JSON object")
	}
	if kind == "permission" {
		var decision string
		if json.Unmarshal(fields["decision"], &decision) != nil || (decision != "once" && decision != "always" && decision != "reject") {
			return core.Invalid("permission requires decision: once, always, or reject")
		}
		for key, value := range fields {
			if key == "decision" {
				continue
			}
			var message string
			if key != "message" || json.Unmarshal(value, &message) != nil {
				return core.Invalid("invalid permission reply field")
			}
		}
		return nil
	}
	var answer map[string]json.RawMessage
	if len(fields) != 1 || json.Unmarshal(fields["answer"], &answer) != nil || answer == nil {
		return core.Invalid("form reply requires answer object with typed field keys")
	}
	for key, rawValue := range answer {
		var value any
		if key == "" || json.Unmarshal(rawValue, &value) != nil {
			return core.Invalid("invalid form answer value")
		}
		switch typed := value.(type) {
		case string, float64, bool:
		case []any:
			for _, item := range typed {
				if _, ok := item.(string); !ok {
					return core.Invalid("form multiselect values must be strings")
				}
			}
		default:
			return core.Invalid("form values must be strings, numbers, booleans, or string arrays")
		}
	}
	return nil
}
