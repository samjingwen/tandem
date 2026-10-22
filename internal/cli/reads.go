package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/samjingwen/tandem/internal/client"
	"github.com/samjingwen/tandem/internal/readmodel"
)

// Legacy HTTP shapes remain available. CLI reads use the additive read surface
// and stream explicitly requested full records, never an assembled State.
func readCommand(ctx context.Context, c *client.Client, req request, out io.Writer, human bool) (bool, error) {
	path, err := url.Parse(req.path)
	if err != nil {
		return false, err
	}
	if req.method != "GET" || path.Path == "/v1/doctor" || path.Path == "/v1/version" {
		return false, nil
	}
	manifest, err := c.Manifest(ctx)
	if err != nil {
		return true, err
	}
	revision := manifest.Revision
	parts := strings.Split(strings.TrimPrefix(path.Path, "/v1/"), "/")
	if parts[0] == "snapshot" {
		if !req.full {
			return true, json.NewEncoder(out).Encode(manifest)
		}
		if _, err := fmt.Fprintf(out, "{\"instanceId\":%q,\"revision\":%d", revision.InstanceID, revision.Revision); err != nil {
			return true, err
		}
		for _, kind := range []string{"projects", "tasks", "attempts", "commands", "decisions", "results"} {
			if _, err := fmt.Fprintf(out, ",%q:", kind); err != nil {
				return true, err
			}
			if err := streamRecords(ctx, c, revision, url.Values{"collection": {kind}}, out, true); err != nil {
				return true, err
			}
		}
		_, err = io.WriteString(out, "}\n")
		return true, err
	}
	if len(parts) == 1 {
		query := path.Query()
		query.Set("collection", parts[0])
		if human && req.table != "" {
			if err := printTable(out, req.table, json.RawMessage("[]")); err != nil {
				return true, err
			}
			return true, c.Walk(ctx, revision, query, func(row readmodel.Record) error {
				data, err := json.Marshal([]map[string]any{row.Summary})
				if err != nil {
					return err
				}
				return printTableRows(out, req.table, data, false)
			})
		}
		return true, streamRecords(ctx, c, revision, query, out, false)
	}
	task, err := readOne(ctx, c, revision, "tasks", parts[1])
	if err != nil {
		return true, err
	}
	var result *readmodel.Record
	if attempt, ok := task.Summary["attemptId"].(string); ok && attempt != "" {
		if generation, ok := task.Summary["generation"].(float64); ok {
			row, err := readOne(ctx, c, revision, "results", fmt.Sprintf("%s/%.0f", attempt, generation))
			var typed *client.Error
			if err == nil {
				result = &row
			} else if !errors.As(err, &typed) || typed.StatusCode != 404 {
				return true, err
			}
		}
	}
	if len(parts) == 3 {
		if result == nil {
			return true, &client.Error{StatusCode: 404, Code: "not_found", Message: "task result not found"}
		}
		return true, c.WriteRecord(ctx, revision, *result, out)
	}
	if _, err := io.WriteString(out, "{\"task\":"); err != nil {
		return true, err
	}
	if err := c.WriteRecord(ctx, revision, task, out); err != nil {
		return true, err
	}
	for _, kind := range []string{"attempts", "commands", "decisions", "results"} {
		if _, err := fmt.Fprintf(out, ",%q:", kind); err != nil {
			return true, err
		}
		if err := streamRecords(ctx, c, revision, url.Values{"collection": {kind}, "taskId": {parts[1]}}, out, false); err != nil {
			return true, err
		}
	}
	if result != nil {
		if _, err := io.WriteString(out, ",\"result\":"); err != nil {
			return true, err
		}
		if err := c.WriteRecord(ctx, revision, *result, out); err != nil {
			return true, err
		}
	}
	_, err = io.WriteString(out, "}\n")
	return true, err
}

func readOne(ctx context.Context, c *client.Client, revision readmodel.Revision, kind, key string) (readmodel.Record, error) {
	page, err := c.ReadPage(ctx, revision, url.Values{"collection": {kind}, "key": {key}, "limit": {"1"}})
	if err != nil {
		return readmodel.Record{}, err
	}
	if len(page.Records) != 1 || page.Records[0].Key != key || page.NextCursor != "" {
		return readmodel.Record{}, errors.New("invalid single-record response")
	}
	return page.Records[0], nil
}

func streamRecords(ctx context.Context, c *client.Client, revision readmodel.Revision, query url.Values, out io.Writer, keyed bool) error {
	open, close := "[", "]\n"
	if keyed {
		open, close = "{", "}"
	}
	if _, err := io.WriteString(out, open); err != nil {
		return err
	}
	first := true
	err := c.Walk(ctx, revision, query, func(record readmodel.Record) error {
		if !first {
			if _, err := io.WriteString(out, ","); err != nil {
				return err
			}
		}
		first = false
		if keyed {
			if _, err := fmt.Fprintf(out, "%q:", record.Key); err != nil {
				return err
			}
		}
		return c.WriteRecord(ctx, revision, record, out)
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, close)
	return err
}
