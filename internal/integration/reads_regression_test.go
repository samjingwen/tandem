package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/api"
	"github.com/samjingwen/tandem/internal/client"
	"github.com/samjingwen/tandem/internal/readmodel"
)

type readBudgetTransport struct {
	t     *testing.T
	base  http.RoundTripper
	pages int
}

func (transport *readBudgetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if request.Method == http.MethodGet && !strings.HasPrefix(request.URL.Path, "/v1/reads/") {
		transport.t.Error("bounded consumer requested a legacy full read", request.URL.Path)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	// These constants are the public protocol budgets, independent of producer
	// constants. All bytes count, including JSON escaping, envelopes and newline.
	if len(data) > 256*1024 {
		transport.t.Errorf("wire response is %d bytes", len(data))
	}
	if request.URL.Path == "/v1/reads/snapshot" && len(data) > 4096 {
		transport.t.Error("manifest grows with retained content")
	}
	if request.URL.Path == "/v1/reads/content" {
		var page struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(data, &page) != nil || len(page.Text) > 32*1024 {
			transport.t.Error("invalid content-page budget")
		}
	}
	transport.pages++
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

// The independent oracle is the admitted inventory and its exact input bytes,
// not a size limit inferred from the implementation. Execution stays simulated.
func TestLargeInventoryCLIReads(t *testing.T) {
	f := setup(t)
	// Large persisted fixtures deliberately retain multi-MiB histories and run
	// under the race detector too. Keep a bounded fixture lifetime independent
	// of the unchanged production per-request timeouts.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	f.ctx = ctx
	instructions := strings.Repeat("界", (128<<10)/3) + "ab"
	want := make(map[string]string)
	for i := range 65 {
		var task core.Task
		f.do(t, 0, http.MethodPost, "/v1/tasks", fmt.Sprintf("large-%d", i), core.CreateTask{ProjectID: f.project.ID, Title: fmt.Sprintf("Task %d", i), Instructions: instructions}, &task)
		want[task.ID] = instructions
	}
	transport := &readBudgetTransport{t: t, base: f.runners[1].HTTPClient.Transport}
	tracked := *f.runners[1].HTTPClient
	tracked.Transport = transport
	f.runners[1].HTTPClient = &tracked
	var out, receipt bytes.Buffer
	if err := f.runners[1].Run(ctx, []string{"tasks", "list", "--json"}, &out, &receipt); err != nil {
		t.Fatalf("65 admitted 128-KiB tasks must remain readable through CLI: %v", err)
	}
	var tasks []core.Task
	if err := json.Unmarshal(out.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != len(want) {
		t.Fatalf("read %d tasks, admitted %d", len(tasks), len(want))
	}
	for _, task := range tasks {
		text, exists := want[task.ID]
		if !exists || task.Instructions != text {
			t.Fatal("duplicate, missing, or damaged task content", task.ID)
		}
		delete(want, task.ID)
	}
	if len(want) != 0 {
		t.Fatal("CLI omitted admitted tasks")
	}
	if transport.pages < 65 {
		t.Fatal("CLI did not traverse bounded content pages")
	}
	var manifest readmodel.Manifest
	f.run(t, 1, &manifest, "snapshot")
	if manifest.Collections["tasks"] != 65 {
		t.Fatal("lean manifest omitted inventory count")
	}
	creates, prompts, interrupts := f.runtime.Counts()
	if creates != 0 || prompts != 0 || interrupts != 0 {
		t.Fatal("read fixture invoked runtime mutations")
	}
}

func TestAccumulatedContentAndExactMutationReceipts(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	f.ctx = ctx
	// Match the real client's 35-second request boundary. The ordinary tiny
	// fixture's 10-second shortcut times out legitimate multi-MiB SQLite outcome
	// encoding under the race detector, before the API's 30-second boundary.
	for index := range f.runners {
		transport := *f.runners[index].HTTPClient
		transport.Timeout = 35 * time.Second
		f.runners[index].HTTPClient = &transport
		var err error
		f.clients[index], err = client.New(client.Options{URL: f.server.URL, Token: "integration-test-token", HTTPClient: &transport})
		if err != nil {
			t.Fatal(err)
		}
	}
	task, attempt := f.task(t)
	// This fixture represents a valid retained accumulation. Admission size is
	// per followup, not an aggregate retention bound. Continue it via the real
	// CLI and verify both current state and the exact original operation receipt.
	retained := strings.Repeat("界\n", (8<<20)/4+1)
	if err := f.store.Update(ctx, func(state *core.State) error {
		value := state.Tasks[task.ID]
		value.Instructions = retained
		state.Tasks[task.ID] = value
		for id, command := range state.Commands {
			command.Text = retained
			state.Commands[id] = command
		}
		state.Decisions["dec_fixture"] = core.Decision{ID: "dec_fixture", TaskID: task.ID, AttemptID: attempt.ID, RuntimeRef: "native-fixture", Kind: "form", Status: "pending", Prompt: strings.Repeat("問", 11000), Details: json.RawMessage(`{"question":"` + strings.Repeat("界", 22000) + `"}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	evidence := []core.Evidence{{Name: "independent fixture oracle", Reference: strings.Repeat("artifact://界/", 8000), Passed: true}}
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/results", "large-evidence", core.SubmitResult{AttemptID: attempt.ID, Generation: 1, Summary: strings.Repeat("verified\n", 9000), Evidence: evidence}, nil)
	var first core.Task
	f.run(t, 1, &first, "tasks", "followup", task.ID, "--text", "first continuation", "--operation-id", "accumulated-first")
	if !strings.HasPrefix(first.Instructions, retained) || !strings.HasSuffix(first.Instructions, "first continuation") {
		t.Fatal("large mutation receipt lost accumulated text")
	}
	var second, replay core.Task
	f.run(t, 1, &second, "tasks", "followup", task.ID, "--text", "second continuation", "--operation-id", "accumulated-second")
	f.run(t, 1, &replay, "tasks", "followup", task.ID, "--text", "first continuation", "--operation-id", "accumulated-first")
	if !reflect.DeepEqual(first, replay) || first.Generation == second.Generation {
		t.Fatal("bounded receipt returned current state instead of the exact idempotent outcome")
	}
	var legacy json.RawMessage
	err := f.clients[0].Do(ctx, http.MethodGet, "/v1/tasks/"+task.ID, "", nil, &legacy)
	var typed *client.Error
	if !errors.As(err, &typed) || typed.Code != "response_too_large" {
		t.Fatal("fixture failed to cross legacy detail boundary", err)
	}
	var detail api.TaskDetail
	f.run(t, 1, &detail, "tasks", "show", task.ID)
	if detail.Task.Instructions != second.Instructions || len(detail.Commands) < 3 || detail.Commands[0].Text != retained || len(detail.Decisions) != 1 || len(detail.Results) != 1 || !reflect.DeepEqual(detail.Results[0].Evidence, evidence) {
		t.Fatal("streamed task detail lost text, history, decision, or evidence")
	}
	manifest, err := f.clients[0].Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.engine.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	collections := map[string]any{"projects": state.Projects, "tasks": state.Tasks, "attempts": state.Attempts, "commands": state.Commands, "decisions": state.Decisions, "results": state.Results}
	for kind, records := range collections {
		data, err := json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]json.RawMessage
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatal(err)
		}
		err = f.clients[0].Walk(ctx, manifest.Revision, url.Values{"collection": {kind}, "limit": {"1"}}, func(record readmodel.Record) error {
			expected, exists := want[record.Key]
			if !exists {
				return fmt.Errorf("duplicate or unknown %s record", kind)
			}
			var content bytes.Buffer
			if err := f.clients[0].WriteRecord(ctx, manifest.Revision, record, &content); err != nil {
				return err
			}
			if !bytes.Equal(content.Bytes(), expected) {
				return fmt.Errorf("damaged %s record %s", kind, record.Key)
			}
			delete(want, record.Key)
			return nil
		})
		if err != nil || len(want) != 0 {
			t.Fatalf("complete %s traversal: %v; missing=%d", kind, err, len(want))
		}
	}
}

func TestReadCursorBindingAndRecovery(t *testing.T) {
	f := setup(t)
	task, _ := f.task(t)
	var another core.Task
	f.do(t, 0, http.MethodPost, "/v1/tasks", "second", core.CreateTask{ProjectID: f.project.ID, Title: "second", Instructions: "fixture"}, &another)
	manifest, err := f.clients[0].Manifest(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.clients[0].ReadPage(f.ctx, manifest.Revision, url.Values{"collection": {"tasks"}, "projectId": {f.project.ID}, "limit": {"1"}})
	if err != nil || page.NextCursor == "" || len(page.Records) != 1 {
		t.Fatal("fixture did not produce a page boundary", err)
	}
	check := func(path string, code string) {
		t.Helper()
		err := f.clients[0].Do(f.ctx, http.MethodGet, path, "", nil, nil)
		var typed *client.Error
		if !errors.As(err, &typed) || typed.Code != code {
			t.Fatalf("expected %s, got %v", code, err)
		}
	}
	for key, value := range map[string]string{"collection": "projects", "projectId": "foreign", "taskId": task.ID, "state": "paused", "limit": "2", "instanceId": "other", "revision": "999"} {
		query := url.Values{"cursor": {page.NextCursor}, key: {value}}
		check("/v1/reads/records?"+query.Encode(), "invalid_cursor")
	}
	check("/v1/reads/records?cursor=x"+url.QueryEscape(page.NextCursor[1:]), "invalid_cursor")
	check("/v1/reads/content?cursor="+url.QueryEscape(page.Records[0].Content.Cursor)+"&projectId=foreign", "invalid_cursor")
	for _, query := range []string{"limit=65", "limit=0", "collection=tasks&collection=projects", "unknown=x", "revision=no"} {
		check("/v1/reads/records?"+query, "invalid_request")
	}
	// Resume a complete page at an unchanged revision, with no missing/duplicate IDs.
	next, err := f.clients[0].ReadPage(f.ctx, manifest.Revision, url.Values{"collection": {"tasks"}, "cursor": {page.NextCursor}})
	if err != nil || len(next.Records) != 1 || next.Records[0].Key == page.Records[0].Key || next.NextCursor != "" {
		t.Fatal("inconsistent page traversal", err)
	}
	f.do(t, 1, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "revision-change", core.Followup{Text: "new revision"}, nil)
	check("/v1/reads/records?cursor="+url.QueryEscape(page.NextCursor), "stale_read")
	check("/v1/reads/content?cursor="+url.QueryEscape(page.Records[0].Content.Cursor), "stale_read")
	fresh, err := f.clients[0].Manifest(f.ctx)
	if err != nil || fresh.Revision == manifest.Revision {
		t.Fatal("manifest did not advance", err)
	}
	if _, err := f.clients[0].ReadPage(f.ctx, fresh.Revision, url.Values{"collection": {"tasks"}}); err != nil {
		t.Fatal("fresh traversal failed", err)
	}
	// Restart rotates the cursor key; old opaque tokens fail explicitly rather
	// than silently changing scope, even if the underlying revision is unchanged.
	oldServer := f.server
	f.server = httptest.NewServer(api.New(f.engine, api.Options{Token: "integration-test-token"}))
	defer oldServer.Close()
	other, err := client.New(client.Options{URL: f.server.URL, Token: "integration-test-token"})
	if err != nil {
		t.Fatal(err)
	}
	err = other.Do(f.ctx, http.MethodGet, "/v1/reads/records?cursor="+url.QueryEscape(page.NextCursor), "", nil, nil)
	var typed *client.Error
	if !errors.As(err, &typed) || typed.Code != "invalid_cursor" {
		t.Fatal("restart accepted an old cursor", err)
	}
}
