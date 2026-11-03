package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/client"
	"github.com/samjingwen/tandem/internal/readmodel"
)

type projectedReadService struct {
	*revisionService
	views []string
}

func newProjectedReadService() *projectedReadService {
	base := newService()
	base.snapshot = func(context.Context) (core.State, error) {
		return core.State{}, errors.New("bounded read loaded the full operation journal")
	}
	return &projectedReadService{revisionService: &revisionService{fakeService: base}}
}

func (service *projectedReadService) ReadView(_ context.Context, operationID string) (core.State, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.views = append(service.views, operationID)
	state := service.state
	state.Operations = map[string]core.Operation{}
	if operation, found := service.state.Operations[operationID]; found {
		state.Operations[operationID] = operation
	}
	return state, service.fail
}

func TestBoundedReadsUseOnlyTheRequestedOperation(t *testing.T) {
	service := newProjectedReadService()
	outcome := json.RawMessage(`{"id":"original","projectId":"p","instructions":"original outcome"}`)
	service.state.Operations["receipt"] = core.Operation{ID: "receipt", Outcome: outcome}
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
	if manifest.Collections["tasks"] != 2 {
		t.Fatal("projected inventory changed manifest counts")
	}
	page, err := c.ReadPage(t.Context(), manifest.Revision, url.Values{"collection": {"receipts"}, "key": {"receipt"}, "projectId": {"p"}})
	if err != nil || len(page.Records) != 1 {
		t.Fatal("projected receipt is unavailable", err)
	}
	var data bytes.Buffer
	if err := c.WriteRecord(t.Context(), manifest.Revision, page.Records[0], &data); err != nil || !bytes.Equal(data.Bytes(), outcome) {
		t.Fatal("projected read changed the exact receipt", err)
	}
	if _, err := c.ReadPage(t.Context(), manifest.Revision, url.Values{"collection": {"receipts"}, "key": {"receipt"}, "projectId": {"foreign"}}); err == nil {
		t.Fatal("projected receipt bypassed project binding")
	}
	if _, err := c.ReadPage(t.Context(), manifest.Revision, url.Values{"collection": {"receipts"}}); err == nil {
		t.Fatal("projected read allowed journal enumeration")
	}
	if service.snapshots.Load() != 0 {
		t.Fatal("bounded reads used the full snapshot fallback")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.views) < 2 || service.views[0] != "" || service.views[1] != "receipt" {
		t.Fatal("bounded reads did not select the exact operation", service.views)
	}
}

func TestBoundedMutationReceiptUsesProjectedReadView(t *testing.T) {
	service := newProjectedReadService()
	task := service.state.Tasks["t"]
	task.Instructions = strings.Repeat("界", 100000)
	outcome, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	service.state.Operations["receipt"] = core.Operation{ID: "receipt", Outcome: outcome}
	handler := New(service, Options{}).(*handler)
	request := httptest.NewRequest("POST", "/v1/tasks/t/followup", nil)
	request.Header.Set("Prefer", "return=bounded")
	request.Header.Set("X-Operation-ID", "receipt")
	response := httptest.NewRecorder()
	handler.mutationResult(response, request, task, nil)
	var receipt readmodel.Receipt
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.Representation != "operation-receipt" {
		t.Fatal("large mutation did not use its projected durable receipt", response.Code)
	}
	if service.snapshots.Load() != 0 {
		t.Fatal("bounded mutation loaded the full operation journal")
	}
}
