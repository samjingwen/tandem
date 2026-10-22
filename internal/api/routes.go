package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/samjingwen/tandem/core"
)

// TaskDetail contains durable coordination records, never runtime transcripts.
type TaskDetail struct {
	Task      core.Task       `json:"task"`
	Attempts  []core.Attempt  `json:"attempts"`
	Commands  []core.Command  `json:"commands"`
	Decisions []core.Decision `json:"decisions"`
	Result    *core.Result    `json:"result,omitempty"`
	Results   []core.Result   `json:"results"`
}

// Diagnostics does not interpret session idleness as task completion.
type Diagnostics struct {
	Ready      bool             `json:"ready"`
	Version    string           `json:"version"`
	InstanceID string           `json:"instanceId"`
	Revision   int64            `json:"revision"`
	Runtime    core.RuntimeInfo `json:"runtime"`
}

func (h *handler) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	parts := strings.Split(path, "/")
	allowed := routeMethods(parts)
	if allowed == "" {
		writeError(w, http.StatusNotFound, "not_found", "API route not found")
		return
	}
	if (r.Method != http.MethodGet && r.Method != http.MethodPost) || !strings.Contains(allowed, r.Method) {
		w.Header().Set("Allow", allowed)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported for this route")
		return
	}
	if r.Method == http.MethodGet {
		h.read(w, r, parts)
	} else {
		h.mutate(w, r, parts)
	}
}

func routeMethods(parts []string) string {
	if len(parts) == 2 && parts[0] == "reads" {
		switch parts[1] {
		case "snapshot", "records", "content":
			return "GET"
		}
	}
	if len(parts) == 1 {
		switch parts[0] {
		case "health", "ready", "version", "runtime", "doctor", "snapshot", "events", "decisions", "openapi.json":
			return "GET"
		case "projects", "tasks":
			return "GET, POST"
		}
	}
	if len(parts) >= 2 && parts[1] != "" {
		if parts[0] == "tasks" && len(parts) == 2 {
			return "GET"
		}
		if len(parts) == 3 {
			if parts[0] == "decisions" && parts[2] == "reply" {
				return "POST"
			}
			if parts[0] == "tasks" {
				switch parts[2] {
				case "results":
					return "GET, POST"
				case "followup", "control", "accept", "cleanup":
					return "POST"
				}
			}
		}
	}
	return ""
}

func (h *handler) read(w http.ResponseWriter, r *http.Request, parts []string) {
	if parts[0] == "reads" {
		h.boundedRead(w, r, parts[1])
		return
	}
	path := strings.Join(parts, "/")
	switch path {
	case "openapi.json":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAPIDocument)
		return
	case "health":
		writeJSON(w, http.StatusOK, struct {
			Status string `json:"status"`
		}{Status: "ok"})
		return
	case "version":
		writeJSON(w, http.StatusOK, struct {
			Version string `json:"version"`
			API     string `json:"api"`
		}{Version: core.Version, API: "v1"})
		return
	case "runtime":
		value, err := h.service.RuntimeInfo(r.Context())
		respond(w, value, err)
		return
	case "ready", "doctor":
		h.diagnostics(w, r)
		return
	case "events":
		h.events(w, r)
		return
	}
	state, err := h.service.Snapshot(r.Context())
	if err != nil {
		serviceError(w, err)
		return
	}
	switch path {
	case "snapshot":
		writeJSON(w, http.StatusOK, state)
	case "projects":
		projects := make([]core.Project, 0, len(state.Projects))
		for _, project := range state.Projects {
			projects = append(projects, project)
		}
		sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
		writeJSON(w, http.StatusOK, projects)
	case "tasks":
		tasks := make([]core.Task, 0, len(state.Tasks))
		for _, task := range state.Tasks {
			if project := r.URL.Query().Get("projectId"); project != "" && task.ProjectID != project {
				continue
			}
			if status := r.URL.Query().Get("state"); status != "" && task.State != status {
				continue
			}
			tasks = append(tasks, task)
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
		writeJSON(w, http.StatusOK, tasks)
	case "decisions":
		decisions := make([]core.Decision, 0, len(state.Decisions))
		for _, decision := range state.Decisions {
			if task := r.URL.Query().Get("taskId"); task != "" && decision.TaskID != task {
				continue
			}
			if status := r.URL.Query().Get("status"); status != "" && decision.Status != status {
				continue
			}
			decisions = append(decisions, decision)
		}
		sort.Slice(decisions, func(i, j int) bool { return decisions[i].ID < decisions[j].ID })
		writeJSON(w, http.StatusOK, decisions)
	default:
		task, exists := state.Tasks[parts[1]]
		if !exists {
			serviceError(w, core.NotFound("task not found"))
			return
		}
		if len(parts) == 3 {
			result, exists := currentResult(state, task)
			if !exists {
				serviceError(w, core.NotFound("task result not found"))
				return
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		writeJSON(w, http.StatusOK, detail(state, task))
	}
}

func detail(state core.State, task core.Task) TaskDetail {
	value := TaskDetail{Task: task, Attempts: []core.Attempt{}, Commands: []core.Command{}, Decisions: []core.Decision{}, Results: []core.Result{}}
	for _, attempt := range state.Attempts {
		if attempt.TaskID == task.ID {
			value.Attempts = append(value.Attempts, attempt)
		}
	}
	for _, command := range state.Commands {
		if command.TaskID == task.ID {
			value.Commands = append(value.Commands, command)
		}
	}
	for _, decision := range state.Decisions {
		if decision.TaskID == task.ID {
			value.Decisions = append(value.Decisions, decision)
		}
	}
	sort.Slice(value.Attempts, func(i, j int) bool { return value.Attempts[i].ID < value.Attempts[j].ID })
	sort.Slice(value.Commands, func(i, j int) bool {
		if value.Commands[i].Sequence == value.Commands[j].Sequence {
			return value.Commands[i].ID < value.Commands[j].ID
		}
		return value.Commands[i].Sequence < value.Commands[j].Sequence
	})
	sort.Slice(value.Decisions, func(i, j int) bool { return value.Decisions[i].ID < value.Decisions[j].ID })
	for _, result := range state.Results {
		if result.TaskID == task.ID {
			value.Results = append(value.Results, result)
		}
	}
	sort.Slice(value.Results, func(i, j int) bool {
		if value.Results[i].Generation == value.Results[j].Generation {
			return value.Results[i].AttemptID < value.Results[j].AttemptID
		}
		return value.Results[i].Generation < value.Results[j].Generation
	})
	if result, ok := currentResult(state, task); ok {
		value.Result = &result
	}
	return value
}

func currentResult(state core.State, task core.Task) (core.Result, bool) {
	result, ok := state.Results[fmt.Sprintf("%s/%d", task.AttemptID, task.Generation)]
	return result, ok && task.AttemptID != "" && result.TaskID == task.ID && result.AttemptID == task.AttemptID && result.Generation == task.Generation
}

func (h *handler) diagnostics(w http.ResponseWriter, r *http.Request) {
	state, err := h.service.Snapshot(r.Context())
	if err != nil {
		serviceError(w, err)
		return
	}
	runtime, err := h.service.RuntimeInfo(r.Context())
	if err != nil {
		serviceError(w, err)
		return
	}
	status := http.StatusOK
	if !runtime.Compatible && r.URL.Path == "/v1/ready" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, Diagnostics{Ready: runtime.Compatible, Version: core.Version, InstanceID: state.InstanceID, Revision: state.Revision, Runtime: runtime})
}

func (h *handler) mutate(w http.ResponseWriter, r *http.Request, parts []string) {
	op := r.Header.Get("X-Operation-ID")
	if len(parts) == 1 {
		switch parts[0] {
		case "projects":
			if body, ok := decode[core.RegisterProject](w, r, false); ok {
				value, err := h.service.Register(r.Context(), op, body)
				h.mutationResult(w, r, value, err)
			}
			return
		case "tasks":
			if body, ok := decode[core.CreateTask](w, r, false); ok {
				value, err := h.service.Create(r.Context(), op, body)
				h.mutationResult(w, r, value, err)
			}
			return
		}
	}
	if len(parts) == 3 && parts[1] != "" {
		id := parts[1]
		if parts[0] == "decisions" && parts[2] == "reply" {
			if body, ok := decode[core.ReplyDecision](w, r, false); ok {
				value, err := h.service.Reply(r.Context(), op, id, body)
				h.mutationResult(w, r, value, err)
			}
			return
		}
		if parts[0] == "tasks" && h.taskMutation(w, r, op, id, parts[2]) {
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "API mutation route not found")
}

func (h *handler) taskMutation(w http.ResponseWriter, r *http.Request, op, id, action string) bool {
	switch action {
	case "followup":
		if body, ok := decode[core.Followup](w, r, false); ok {
			value, err := h.service.Followup(r.Context(), op, id, body)
			h.mutationResult(w, r, value, err)
		}
	case "control":
		if body, ok := decode[core.Control](w, r, false); ok {
			value, err := h.service.Control(r.Context(), op, id, body)
			h.mutationResult(w, r, value, err)
		}
	case "results":
		if body, ok := decode[core.SubmitResult](w, r, false); ok {
			value, err := h.service.SubmitResult(r.Context(), op, id, body)
			h.mutationResult(w, r, value, err)
		}
	case "accept", "cleanup":
		if _, ok := decode[struct{}](w, r, true); ok {
			if action == "accept" {
				value, err := h.service.Accept(r.Context(), op, id)
				h.mutationResult(w, r, value, err)
			} else {
				h.cleanup(w, r, op, id)
			}
		}
	default:
		return false
	}
	return true
}

func (h *handler) cleanup(w http.ResponseWriter, r *http.Request, op, taskID string) {
	state, err := h.service.Snapshot(r.Context())
	if err != nil {
		serviceError(w, err)
		return
	}
	task, ok := state.Tasks[taskID]
	if !ok {
		serviceError(w, core.NotFound("task not found"))
		return
	}
	attempt, ok := state.Attempts[task.AttemptID]
	if !ok || attempt.ID == "" || attempt.ID != task.AttemptID || attempt.TaskID != taskID {
		writeError(w, http.StatusConflict, "attempt_unavailable", "task has no current owned attempt to clean up")
		return
	}
	// Core rechecks terminal state, runtime quiescence, workspace ownership,
	// and the operation fingerprint before any external cleanup. Never retry a
	// rejected binding with a replacement attempt or a new operation ID.
	value, err := h.service.Cleanup(r.Context(), op, attempt.ID)
	h.mutationResult(w, r, value, err)
}
