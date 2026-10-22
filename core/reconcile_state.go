package core

import "fmt"

func controlQuiescent(o Observation) bool {
	return o.Exists && o.Owned && (o.State == "idle" || o.State == "failed")
}

func dispatchBudgetError(s *State, a Attempt) string {
	if a.FailureCount < 8 || s.Tasks[a.TaskID].Desired == "cancelled" {
		return ""
	}
	for _, c := range s.Commands {
		if c.AttemptID == a.ID && c.Kind == "prompt" && (c.Status == "pending" || c.Status == "parked") {
			return "dispatch retry budget exhausted; resume the task to retry pending delivery"
		}
	}
	for _, d := range s.Decisions {
		if d.AttemptID == a.ID && d.Status == "reply_pending" {
			return "dispatch retry budget exhausted; resume the task to retry pending delivery"
		}
	}
	return ""
}

func uncertainDispatch(s *State, attemptID string) bool {
	for _, c := range s.Commands {
		if c.AttemptID == attemptID && c.Kind == "prompt" && c.Status == "sending" {
			return true
		}
	}
	for _, d := range s.Decisions {
		if d.AttemptID == attemptID && d.Status == "uncertain" {
			return true
		}
	}
	return false
}

func consumesSlot(s *State, a Attempt) bool {
	if s.Tasks[a.TaskID].AttemptID != a.ID || a.Phase == "cleaned" {
		return false
	}
	if uncertainDispatch(s, a.ID) {
		return true
	}
	return a.Observed != "failed" && a.Observed != "idle"
}

func pendingPrompt(s *State, attemptID string) bool {
	for _, c := range s.Commands {
		if c.AttemptID == attemptID && c.Kind == "prompt" && (c.Status == "pending" || c.Status == "sending" || c.Status == "parked") {
			return true
		}
	}
	return false
}

func cleanupBarrier(a Attempt) bool {
	return a.Phase == "cleanup_pending" || a.Phase == "cleaned"
}

func cleanupEligible(s *State, a Attempt) bool {
	t := s.Tasks[a.TaskID]
	if t.State != "completed" && t.State != "cancelled" && t.State != "failed" {
		return false
	}
	if pendingPrompt(s, a.ID) || uncertainDispatch(s, a.ID) {
		return false
	}
	for _, d := range s.Decisions {
		if d.AttemptID == a.ID && d.Status == "reply_pending" {
			return false
		}
	}
	return true
}

func (e *Engine) capacityAvailable(s *State, a Attempt) bool {
	role := s.Tasks[a.TaskID].Role
	active := 0
	for _, other := range s.Attempts {
		if other.ID != a.ID && s.Tasks[other.TaskID].Role == role && consumesSlot(s, other) {
			active++
		}
	}
	limit := e.opts.Concurrency
	if role == "coordinator" {
		limit = 1
	}
	return active < limit
}

// refreshTaskState derives the public state from durable intent/evidence rather
// than toggling running/recovering on every unchanged reconciliation poll.
func refreshTaskState(s *State, taskID, observed string) {
	t := s.Tasks[taskID]
	if t.Desired != "active" || t.State == "completed" {
		return
	}
	a := s.Attempts[t.AttemptID]
	state := "running"
	message := ""
	pendingDecision := false
	for _, d := range s.Decisions {
		if d.AttemptID == a.ID && (d.Status == "pending" || d.Status == "reply_pending") {
			pendingDecision = true
		}
	}
	switch {
	case uncertainDispatch(s, a.ID):
		state = "recovering"
		message = a.Error
	case dispatchBudgetError(s, a) != "":
		state = "recovering"
		message = dispatchBudgetError(s, a)
		a.Error = message
		s.Attempts[a.ID] = a
	case observed == "failed" && !pendingPrompt(s, a.ID):
		if pendingDecision {
			state = "waiting_for_input"
			break
		}
		state = "failed"
		message = "runtime execution failed"
		a.Phase = "failed"
		s.Attempts[a.ID] = a
	default:
		for _, d := range s.Decisions {
			if d.AttemptID == a.ID && (d.Status == "pending" || d.Status == "reply_pending") {
				state = "waiting_for_input"
				if d.Error != "" {
					message = "decision reply was rejected; submit a corrected answer"
				}
			}
		}
		if state == "running" {
			if result, ok := s.Results[fmt.Sprintf("%s/%d", a.ID, t.Generation)]; ok && !result.Late {
				state = "ready_for_review"
			}
		}
		if state == "running" {
			for _, c := range s.Commands {
				if c.AttemptID == a.ID && c.Kind == "signal" && c.Status == "suppressed" && c.Generation == t.Generation {
					if observed == "idle" {
						state = "waiting_for_input"
					}
					message = coordinationBudgetMessage
				}
			}
		}
	}
	if t.State != state || t.Error != message {
		t.State = state
		t.Error = message
		t.UpdatedAt = now()
		s.Tasks[t.ID] = t
	}
}
