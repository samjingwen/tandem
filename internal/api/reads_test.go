package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/client"
	"github.com/samjingwen/tandem/internal/readmodel"
)

type revisionService struct {
	*fakeService
	snapshots atomic.Int64
}

func TestRecordPageByteBudgetIncludesEscapingAndCursor(t *testing.T) {
	service := newService()
	service.state.Projects = map[string]core.Project{}
	for index := range 64 {
		id := "project-" + strconv.Itoa(index)
		service.state.Projects[id] = core.Project{ID: id, Name: strings.Repeat("<&", 512), Path: "/repo/" + id, BaseRef: "master"}
	}
	server := httptest.NewServer(New(service, Options{}))
	defer server.Close()
	c, err := client.New(client.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := c.Manifest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{"collection": {"projects"}, "limit": {"64"}}
	first, err := c.ReadPage(t.Context(), manifest.Revision, query)
	if err != nil || len(first.Records) == 0 || len(first.Records) >= 64 || first.NextCursor == "" {
		t.Fatal("byte budget did not end the escaped metadata page early", err)
	}
	seen := map[string]bool{}
	if err := c.Walk(t.Context(), manifest.Revision, query, func(record readmodel.Record) error {
		if seen[record.Key] {
			t.Error("byte-limited traversal duplicated a record")
		}
		seen[record.Key] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 64 {
		t.Fatal("byte-limited traversal omitted records")
	}
}

func (service *revisionService) Snapshot(ctx context.Context) (core.State, error) {
	service.snapshots.Add(1)
	return service.fakeService.Snapshot(ctx)
}

func (service *revisionService) ReadRevision(context.Context) (string, int64, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.state.InstanceID, service.state.Revision, nil
}

func TestContentCachePreservesRevisionAndEvictionSemantics(t *testing.T) {
	service := &revisionService{fakeService: newService()}
	task := service.state.Tasks["t"]
	task.Instructions = strings.Repeat("界\x00<\n", 16000)
	service.state.Tasks["t"] = task
	handler := New(service, Options{}).(*handler)
	server := httptest.NewServer(handler)
	defer server.Close()
	c, err := client.New(client.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := c.Manifest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.ReadPage(t.Context(), manifest.Revision, url.Values{"collection": {"tasks"}, "key": {"t"}})
	if err != nil || len(page.Records) != 1 {
		t.Fatal(err)
	}
	before := service.snapshots.Load()
	var first bytes.Buffer
	if err := c.WriteRecord(t.Context(), manifest.Revision, page.Records[0], &first); err != nil {
		t.Fatal(err)
	}
	if service.snapshots.Load() != before {
		t.Fatal("cached chunks reloaded the full inventory/journal")
	}
	// Force ordinary cache eviction in this disposable handler only. The issued
	// content reference must still work, with identical bytes, after reloading.
	for index := range 129 {
		cursor := readCursor{Collection: "tasks", Key: strings.Repeat("x", index+1)}
		handler.readCache.put(cursor, []byte("fixture"))
	}
	var second bytes.Buffer
	if err := c.WriteRecord(t.Context(), manifest.Revision, page.Records[0], &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) || service.snapshots.Load() != before+1 {
		t.Fatal("eviction changed content or repeatedly reloaded it")
	}
	service.mu.Lock()
	service.state.Revision++
	service.mu.Unlock()
	err = c.Do(t.Context(), http.MethodGet, "/v1/reads/content?cursor="+url.QueryEscape(page.Records[0].Content.Cursor), "", nil, nil)
	if err == nil {
		t.Fatal("cache served stale content after a revision change")
	}
}

func TestReceiptProjectBindings(t *testing.T) {
	service := newService()
	outcomes := map[string]string{
		"create":   `{"id":"old-task","projectId":"p","instructions":"original"}`,
		"followup": `{"id":"t","projectId":"p","instructions":"original"}`,
		"reply":    `{"id":"old-decision","taskId":"t","reply":{"answer":"yes"}}`,
		"result":   `{"taskId":"t","attemptId":"a","summary":"original"}`,
		"control":  `{"id":"t","projectId":"p","desired":"paused"}`,
		"register": `{"id":"p","path":"/repo"}`,
		"cleanup":  `{"owner":"a","path":"/repo/worktree"}`,
	}
	for op, data := range outcomes {
		service.state.Operations[op] = core.Operation{ID: op, Outcome: json.RawMessage(data)}
	}
	server := httptest.NewServer(New(service, Options{}))
	defer server.Close()
	c, err := client.New(client.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := c.Manifest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for op, data := range outcomes {
		for _, project := range []string{"", "p"} {
			page, err := c.ReadPage(t.Context(), manifest.Revision, url.Values{"collection": {"receipts"}, "key": {op}, "projectId": {project}})
			if err != nil || len(page.Records) != 1 {
				t.Fatal(op, err)
			}
			var content readmodel.ContentPage
			err = c.Do(t.Context(), http.MethodGet, "/v1/reads/content?"+url.Values{"cursor": {page.Records[0].Content.Cursor}, "projectId": {"p"}}.Encode(), "", nil, &content)
			if err != nil || content.Text != data {
				t.Fatal("receipt lost its verified project binding or original outcome", op, err)
			}
		}
		if _, err := c.ReadPage(t.Context(), manifest.Revision, url.Values{"collection": {"receipts"}, "key": {op}, "projectId": {"other"}}); err == nil {
			t.Fatal("foreign-project receipt was exposed", op)
		}
	}
	if _, err := c.ReadPage(t.Context(), manifest.Revision, url.Values{"collection": {"receipts"}, "key": {"private-op"}, "projectId": {"p"}}); err == nil {
		t.Fatal("unbound receipt accepted a claimed project")
	}
}
