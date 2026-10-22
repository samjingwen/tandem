package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Events consumes one live-only connection, across all locations, without replay.
// The engine owns reconnect/backoff and coalesces these payload-free wake signals.
func (r *Runtime) Events(ctx context.Context, notify func()) error {
	if err := r.compatible(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/api/event", nil)
	if err != nil {
		return err
	}
	r.authenticate(req)
	req.Header.Set("Accept", "text/event-stream")
	// No overall client timeout: only the caller controls the stream lifetime.
	response, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &httpError{status: response.StatusCode}
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "text/event-stream" {
		return errors.New("invalid OpenCode event content type")
	}
	return consumeEvents(response.Body, notify)
}

func consumeEvents(reader io.Reader, notify func()) error {
	const limit = 1 << 20
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), limit)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			var event struct {
				Type string `json:"type"`
			}
			// Frames contain a JSON object, not a JSON-encoded JSON string.
			if json.Unmarshal([]byte(data.String()), &event) != nil || event.Type == "" {
				return errors.New("invalid OpenCode SSE event")
			}
			data.Reset()
			if notify != nil {
				notify()
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if data.Len()+len(value)+1 > limit {
			return errors.New("OpenCode SSE frame exceeds size limit")
		}
		data.WriteString(value)
		data.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF // Partial frames are not dispatched; EOF requires reconciliation/reconnect.
}
