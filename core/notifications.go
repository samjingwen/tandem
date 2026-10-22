package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const coordinationBudgetMessage = "automatic coordination notification budget exhausted; inspect pending decisions or send an explicit follow-up"

type childUpdate struct {
	TaskID     string     `json:"taskId"`
	AttemptID  string     `json:"attemptId"`
	Generation int        `json:"generation"`
	State      string     `json:"state"`
	Result     *Result    `json:"reportedResult,omitempty"`
	Decisions  []Decision `json:"pendingDecisions,omitempty"`
}

func updateForChild(s *State, child Task) (childUpdate, bool) {
	a := s.Attempts[child.AttemptID]
	switch child.State {
	case "ready_for_review", "completed":
		if a.Observed != "idle" {
			return childUpdate{}, false
		}
	case "failed", "waiting_for_input":
	default:
		return childUpdate{}, false
	}
	update := childUpdate{TaskID: child.ID, AttemptID: child.AttemptID, Generation: child.Generation, State: child.State}
	if result, ok := s.Results[fmt.Sprintf("%s/%d", child.AttemptID, child.Generation)]; ok {
		copy := result
		update.Result = &copy
	}
	for _, d := range s.Decisions {
		if d.TaskID == child.ID && d.AttemptID == child.AttemptID && d.Status == "pending" {
			update.Decisions = append(update.Decisions, d)
		}
	}
	sort.Slice(update.Decisions, func(i, j int) bool { return update.Decisions[i].ID < update.Decisions[j].ID })
	return update, child.State != "waiting_for_input" || len(update.Decisions) > 0
}

// notifyCoordinators persists one current signal per child. Its previous values
// remain in SQLite history, while current-state memory does not grow per poll.
// Only genuinely new evidence/questions wake a model, with a finite turn budget.
func (e *Engine) notifyCoordinators(ctx context.Context) error {
	return e.store.Update(ctx, func(s *State) error {
		parents := []string{}
		for id, t := range s.Tasks {
			if t.Role == "coordinator" && t.Desired == "active" && t.State != "completed" && t.AttemptID != "" {
				parents = append(parents, id)
			}
		}
		sort.Strings(parents)
		for _, id := range parents {
			parent := s.Tasks[id]
			a := s.Attempts[parent.AttemptID]
			if a.Phase != "executing" && a.Phase != "session_bound" {
				continue
			}
			used := 0
			for _, c := range s.Commands {
				if c.AttemptID == a.ID && c.Kind == "prompt" && strings.HasPrefix(c.ID, "coordination_") {
					used++
				}
			}
			children := []string{}
			for key, t := range s.Tasks {
				if t.ParentID == id {
					children = append(children, key)
				}
			}
			sort.Strings(children)
			var updates []childUpdate
			var keys []string
			for _, key := range children {
				update, ok := updateForChild(s, s.Tasks[key])
				if !ok {
					continue
				}
				body, err := json.Marshal(update)
				if err != nil {
					return err
				}
				hash := sha256.Sum256([]byte(a.ID + "\x00" + key))
				signalID := "signal_" + hex.EncodeToString(hash[:16])
				if old, exists := s.Commands[signalID]; exists && old.Text == string(body) {
					continue
				}
				updates = append(updates, update)
				keys = append(keys, signalID)
			}
			if len(updates) == 0 {
				continue
			}
			parent.Generation++
			parent.UpdatedAt = now()
			parent.State = "running"
			a.Generation = parent.Generation
			status := "coalesced"
			if used >= e.opts.MaxCoordinatorNotifications {
				status = "suppressed"
				parent.State = "waiting_for_input"
				parent.Error = coordinationBudgetMessage
			}
			for index, key := range keys {
				body, err := json.Marshal(updates[index])
				if err != nil {
					return err
				}
				s.Commands[key] = Command{ID: key, TaskID: id, AttemptID: a.ID, Kind: "signal", Status: status, Text: string(body), Generation: parent.Generation, Sequence: s.Revision + 1}
			}
			s.Tasks[id] = parent
			s.Attempts[a.ID] = a
			if status == "suppressed" {
				continue
			}
			body, err := json.Marshal(updates)
			if err != nil {
				return err
			}
			hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s", a.ID, parent.Generation, body)))
			commandID := "coordination_" + hex.EncodeToString(hash[:16])
			text := fmt.Sprintf("New coordination evidence for task %s, context generation %d. These are reported child outcomes or actual pending questions, not independent verification. Summarize actionable results or ask for a necessary decision through the existing conversation. Do not recreate workers, auto-approve permissions, merge/publish, or repeatedly call a model/status tool to poll. Submit any updated result with this generation and actual evidence.\n%s", id, parent.Generation, body)
			s.Commands[commandID] = Command{ID: commandID, TaskID: id, AttemptID: a.ID, Kind: "prompt", Status: "pending", Text: text, Generation: parent.Generation, Sequence: s.Revision + 1}
		}
		return nil
	})
}
