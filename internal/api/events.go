package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// RevisionNotification is a live invalidation hint. Clients must fetch a snapshot
// for durable state. Revisions are not event offsets and have no replay contract.
type RevisionNotification struct {
	InstanceID string `json:"instanceId"`
	Revision   int64  `json:"revision"`
}

func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	state, err := h.service.Snapshot(r.Context())
	if err != nil {
		serviceError(w, err)
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusNotImplemented, "streaming_unavailable", "response streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Tandem-Events", "live-only")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	last := RevisionNotification{InstanceID: state.InstanceID, Revision: state.Revision}
	send := func(value RevisionNotification) error {
		deadline := time.Now().Add(5 * time.Second)
		if requestDeadline, ok := r.Context().Deadline(); ok && requestDeadline.Before(deadline) {
			deadline = requestDeadline
		}
		_ = controller.SetWriteDeadline(deadline)
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: revision\ndata: %s\n\n", data); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := send(last); err != nil {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			state, err := h.service.Snapshot(r.Context())
			if err != nil {
				// Do not append JSON or internal error details to a started SSE stream.
				return
			}
			next := RevisionNotification{InstanceID: state.InstanceID, Revision: state.Revision}
			if next != last {
				if err := send(next); err != nil {
					return
				}
				last = next
			}
		}
	}
}
