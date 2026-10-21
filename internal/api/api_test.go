package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
)

type call struct {
	kind, op, id string
	body         any
	deadline     time.Time
}

type fakeService struct {
	mu       sync.Mutex
	state    core.State
	runtime  core.RuntimeInfo
	fail     error
	calls    []call
	snapshot func(context.Context) (core.State, error)
}

func newService() *fakeService {
	s := core.NewState()
	s.InstanceID, s.Revision = "instance", 7
	s.Projects["p"] = core.Project{ID: "p", Name: "project", Path: "/repo", BaseRef: "master"}
	s.Tasks["t"] = core.Task{ID: "t", ProjectID: "p", Title: "task", State: "ready_for_review", Desired: "active", AttemptID: "a", Generation: 1}
	s.Tasks["z"] = core.Task{ID: "z", ProjectID: "other", State: "paused", Desired: "paused", Generation: 1}
	s.Attempts["a"] = core.Attempt{ID: "a", TaskID: "t", Phase: "observing", Observed: "idle", SessionRef: "runtime-ref"}
	s.Commands["c"] = core.Command{ID: "c", TaskID: "t", Status: "uncertain"}
	s.Decisions["d"] = core.Decision{ID: "d", TaskID: "t", Status: "pending", Kind: "permission", Details: json.RawMessage(`{"reason":"test"}`)}
	s.Results["a/1"] = core.Result{TaskID: "t", AttemptID: "a", Generation: 1, Summary: "ready for review", Evidence: []core.Evidence{{Name: "tests", Reference: "artifact://check", Passed: false}}, Late: true}
	s.Operations["private-op"] = core.Operation{ID: "private-op", Fingerprint: "private-fingerprint", Outcome: json.RawMessage(`{"secret":"private-outcome"}`)}
	return &fakeService{state: s, runtime: core.RuntimeInfo{ID: "fake", Version: "test", Compatible: true}}
}

func (f *fakeService) Snapshot(ctx context.Context) (core.State, error) {
	if f.snapshot != nil {
		return f.snapshot(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.fail
}

func (f *fakeService) RuntimeInfo(context.Context) (core.RuntimeInfo, error) {
	return f.runtime, f.fail
}

func (f *fakeService) record(ctx context.Context, kind, op, id string, body any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	deadline, _ := ctx.Deadline()
	f.calls = append(f.calls, call{kind: kind, op: op, id: id, body: body, deadline: deadline})
	return f.fail
}

func (f *fakeService) Register(ctx context.Context, op string, body core.RegisterProject) (core.Project, error) {
	return core.Project{ID: "p", Name: body.Name}, f.record(ctx, "register", op, "", body)
}

func (f *fakeService) Create(ctx context.Context, op string, body core.CreateTask) (core.Task, error) {
	return core.Task{ID: "t", Title: body.Title}, f.record(ctx, "create", op, "", body)
}

func (f *fakeService) Followup(ctx context.Context, op, id string, body core.Followup) (core.Task, error) {
	return core.Task{ID: id}, f.record(ctx, "followup", op, id, body)
}

func (f *fakeService) Control(ctx context.Context, op, id string, body core.Control) (core.Task, error) {
	return core.Task{ID: id}, f.record(ctx, "control", op, id, body)
}

func (f *fakeService) Reply(ctx context.Context, op, id string, body core.ReplyDecision) (core.Decision, error) {
	return core.Decision{ID: id}, f.record(ctx, "reply", op, id, body)
}

func (f *fakeService) SubmitResult(ctx context.Context, op, id string, body core.SubmitResult) (core.Result, error) {
	return core.Result{TaskID: id, AttemptID: body.AttemptID, Generation: body.Generation, Evidence: body.Evidence}, f.record(ctx, "result", op, id, body)
}

func (f *fakeService) Accept(ctx context.Context, op, id string) (core.Task, error) {
	return core.Task{ID: id}, f.record(ctx, "accept", op, id, nil)
}

func (f *fakeService) Cleanup(ctx context.Context, op, id string) (core.Workspace, error) {
	return core.Workspace{Owner: id}, f.record(ctx, "cleanup", op, id, nil)
}

func exchange(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func mutationHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "X-Operation-ID": "test:stable-1"}
}

func TestMutationContract(t *testing.T) {
	tests := []struct {
		path, body, kind, id string
		input                any
	}{
		{"/v1/projects", `{"name":"demo","path":"/repo","baseRef":"master","instructions":"local"}`, "register", "", core.RegisterProject{Name: "demo", Path: "/repo", BaseRef: "master", Instructions: "local"}},
		{"/v1/tasks", `{"projectId":"p","title":"Build","instructions":"Do it","role":"worker","parentId":"parent","dependencies":["dependency"]}`, "create", "", core.CreateTask{ProjectID: "p", Title: "Build", Instructions: "Do it", Role: "worker", ParentID: "parent", Dependencies: []string{"dependency"}}},
		{"/v1/tasks/t/followup", `{"text":"continue"}`, "followup", "t", core.Followup{Text: "continue"}},
		{"/v1/tasks/t/control", `{"action":"pause","cascade":true}`, "control", "t", core.Control{Action: "pause", Cascade: true}},
		{"/v1/tasks/t/results", `{"attemptId":"a","generation":1,"summary":"review","evidence":[{"name":"tests","reference":"artifact://report","passed":false}]}`, "result", "t", core.SubmitResult{AttemptID: "a", Generation: 1, Summary: "review", Evidence: []core.Evidence{{Name: "tests", Reference: "artifact://report", Passed: false}}}},
		{"/v1/tasks/t/accept", `{}`, "accept", "t", nil},
		{"/v1/tasks/t/cleanup", ``, "cleanup", "a", nil},
		{"/v1/decisions/d/reply", `{"answer":{"permission":"once"}}`, "reply", "d", core.ReplyDecision{Answer: json.RawMessage(`{"permission":"once"}`)}},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			s := newService()
			w := exchange(New(s, Options{}), http.MethodPost, test.path, test.body, mutationHeaders())
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if len(s.calls) != 1 {
				t.Fatalf("got %d calls", len(s.calls))
			}
			got := s.calls[0]
			if got.kind != test.kind || got.id != test.id || got.op != "test:stable-1" {
				t.Fatalf("wrong delegation: %+v", got)
			}
			actual, _ := json.Marshal(got.body)
			want, _ := json.Marshal(test.input)
			if string(actual) != string(want) {
				t.Fatalf("got %s, want %s", actual, want)
			}
			if got.deadline.IsZero() || time.Until(got.deadline) > requestTimeout {
				t.Fatal("service request context is not bounded")
			}
			if w.Header().Get("X-Operation-ID") != got.op {
				t.Fatal("operation ID not echoed")
			}
		})
	}
}

func TestAuthenticationProtectsEveryV1Route(t *testing.T) {
	h := New(newService(), Options{Token: "private-token"})
	for _, path := range []string{"health", "ready", "version", "runtime", "doctor", "projects", "tasks", "tasks/t", "tasks/t/results", "decisions", "snapshot", "events", "openapi.json", "unknown"} {
		w := exchange(h, http.MethodGet, "/v1/"+path, "", nil)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s not protected: %d", path, w.Code)
		}
	}
	for _, authorization := range []string{"Basic private-token", "Bearer wrong", "Bearer private-token extra", "bearer private-token"} {
		w := exchange(h, http.MethodGet, "/v1/health", "", map[string]string{"Authorization": authorization})
		want := http.StatusUnauthorized
		if authorization == "bearer private-token" {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Errorf("unexpected bearer result: %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "private-token") {
			t.Fatal("token leaked")
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.Header.Add("Authorization", "Bearer private-token")
	r.Header.Add("Authorization", "Bearer private-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("duplicate authorization accepted")
	}
}

func TestMutationGuards(t *testing.T) {
	tests := []struct {
		name, key, value string
		status           int
	}{
		{"missing operation", "X-Operation-ID", "", 400},
		{"bad operation", "X-Operation-ID", "invalid value", 400},
		{"long operation", "X-Operation-ID", strings.Repeat("a", 129), 400},
		{"cross origin", "Origin", "https://evil.example", 403},
		{"null origin", "Origin", "null", 403},
		{"malformed origin", "Origin", "://", 403},
		{"same site", "Sec-Fetch-Site", "same-site", 403},
		{"cross site", "Sec-Fetch-Site", "cross-site", 403},
		{"wrong content type", "Content-Type", "text/plain", 415},
		{"no content type", "Content-Type", "", 415},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newService()
			headers := mutationHeaders()
			headers[test.key] = test.value
			w := exchange(New(s, Options{}), http.MethodPost, "/v1/tasks", `{}`, headers)
			if w.Code != test.status || len(s.calls) != 0 {
				t.Fatalf("got %d and %d calls: %s", w.Code, len(s.calls), w.Body.String())
			}
		})
	}
	for _, origin := range []string{"", "http://example.com"} {
		headers := mutationHeaders()
		headers["Origin"] = origin
		if origin == "" {
			delete(headers, "Origin")
		}
		w := exchange(New(newService(), Options{}), http.MethodPost, "/v1/tasks", `{}`, headers)
		if w.Code != http.StatusOK {
			t.Fatalf("same-origin/native request rejected: %d", w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Add("X-Operation-ID", "first")
	r.Header.Add("X-Operation-ID", "second")
	w := httptest.NewRecorder()
	New(newService(), Options{}).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatal("duplicate operation IDs accepted")
	}
}

func TestStrictBoundedJSON(t *testing.T) {
	for _, test := range []struct {
		path, body string
		status     int
	}{
		{"/v1/tasks", `{"unknown":"private-input"}`, 400},
		{"/v1/tasks", `{} {}`, 400},
		{"/v1/tasks", `{} garbage`, 400},
		{"/v1/tasks", ``, 400},
		{"/v1/tasks", `null`, 400},
		{"/v1/tasks", `[]`, 400},
		{"/v1/tasks", `{"title":true}`, 400},
		{"/v1/tasks/t/accept", `{"force":true}`, 400},
		{"/v1/tasks/t/cleanup", `null`, 400},
		{"/v1/tasks/t/results", `{"evidence":[{"unknown":"private-input"}]}`, 400},
		{"/v1/tasks", `{"instructions":"` + strings.Repeat("x", maxBodyBytes) + `"}`, 413},
		{"/v1/tasks/t/accept", strings.Repeat(" ", maxBodyBytes+1), 413},
	} {
		s := newService()
		w := exchange(New(s, Options{}), http.MethodPost, test.path, test.body, mutationHeaders())
		if w.Code != test.status || len(s.calls) != 0 {
			t.Fatalf("strict JSON: got status %d calls %d, want %d: %s", w.Code, len(s.calls), test.status, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-input") {
			t.Fatal("decoder error leaked input")
		}
	}
}

func TestMethodsDoNotFallThroughToMutations(t *testing.T) {
	s := newService()
	h := New(s, Options{})
	for _, test := range []struct{ method, path, allow string }{
		{http.MethodGet, "/v1/tasks/t/accept", "POST"},
		{http.MethodGet, "/v1/decisions/d/reply", "POST"},
		{http.MethodPost, "/v1/health", "GET"},
		{http.MethodDelete, "/v1/tasks/t", "GET"},
		{http.MethodPut, "/v1/projects", "GET, POST"},
	} {
		w := exchange(h, test.method, test.path, "", mutationHeaders())
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != test.allow {
			t.Fatalf("%s %s: %d %s", test.method, test.path, w.Code, w.Body.String())
		}
	}
	if len(s.calls) != 0 {
		t.Fatal("wrong method invoked a mutation")
	}
}

func TestRetainedResultsAreNotConfusedWithCurrentResult(t *testing.T) {
	s := newService()
	task := s.state.Tasks["t"]
	task.Generation = 3
	s.state.Tasks[task.ID] = task
	s.state.Results["a/2"] = core.Result{TaskID: "t", AttemptID: "a", Generation: 2, Late: true}
	s.state.Results["old/1"] = core.Result{TaskID: "t", AttemptID: "old", Generation: 1, Late: true}
	s.state.Results["other/4"] = core.Result{TaskID: "other-task", AttemptID: "other", Generation: 4}
	s.state.Commands["z-first"] = core.Command{ID: "z-first", TaskID: "t", Sequence: 1}
	s.state.Commands["a-last"] = core.Command{ID: "a-last", TaskID: "t", Sequence: 3}
	s.state.Commands["c"] = core.Command{ID: "c", TaskID: "t", Sequence: 2}
	h := New(s, Options{})
	if w := exchange(h, http.MethodGet, "/v1/tasks/t/results", "", nil); w.Code != 404 {
		t.Fatal("old result was selected as current")
	}
	w := exchange(h, http.MethodGet, "/v1/tasks/t", "", nil)
	var value TaskDetail
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Result != nil || len(value.Results) != 3 || value.Results[2].Generation != 2 {
		t.Fatalf("incorrect retained history: %+v", value)
	}
	if value.Commands[0].ID != "z-first" || value.Commands[2].ID != "a-last" {
		t.Fatal("commands sorted by ID instead of durable sequence")
	}
}

func TestCleanupRejectsMissingOrUnownedAttemptBeforeMutation(t *testing.T) {
	for _, mutate := range []func(*core.State){
		func(s *core.State) { delete(s.Tasks, "t") },
		func(s *core.State) { delete(s.Attempts, "a") },
		func(s *core.State) { a := s.Attempts["a"]; a.TaskID = "other"; s.Attempts["a"] = a },
		func(s *core.State) { a := s.Attempts["a"]; a.ID = "other"; s.Attempts["a"] = a },
	} {
		s := newService()
		mutate(&s.state)
		w := exchange(New(s, Options{}), http.MethodPost, "/v1/tasks/t/cleanup", `{}`, mutationHeaders())
		if (w.Code != 404 && w.Code != 409) || len(s.calls) != 0 {
			t.Fatalf("unsafe cleanup delegation: status %d calls %+v", w.Code, s.calls)
		}
	}
}

func TestReadContracts(t *testing.T) {
	s := newService()
	h := New(s, Options{})
	for _, path := range []string{"health", "ready", "version", "runtime", "doctor", "projects", "tasks", "tasks/t", "tasks/t/results", "decisions", "snapshot", "openapi.json"} {
		w := exchange(h, http.MethodGet, "/v1/"+path, "", nil)
		if w.Code != http.StatusOK || !json.Valid(w.Body.Bytes()) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d: %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), `"operations"`) {
			t.Fatalf("operations leaked in %s", path)
		}
	}
	w := exchange(h, http.MethodGet, "/v1/tasks/t", "", nil)
	var detail TaskDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Task.State != "ready_for_review" || detail.Attempts[0].Observed != "idle" || detail.Commands[0].Status != "uncertain" || detail.Decisions[0].Status != "pending" {
		t.Fatalf("status/evidence transformed: %+v", detail)
	}
	if detail.Result.Accepted || !detail.Result.Late || detail.Result.Evidence[0].Passed || detail.Result.Evidence[0].Reference != "artifact://check" {
		t.Fatalf("result/evidence transformed: %+v", detail.Result)
	}
	w = exchange(h, http.MethodGet, "/v1/tasks?projectId=p&state=ready_for_review", "", nil)
	var tasks []core.Task
	if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil || len(tasks) != 1 || tasks[0].ID != "t" {
		t.Fatalf("task filtering: %s (%v)", w.Body.String(), err)
	}
	w = exchange(h, http.MethodGet, "/v1/decisions?taskId=other&status=pending", "", nil)
	if w.Body.String() != "[]\n" {
		t.Fatalf("empty list must be an array: %s", w.Body.String())
	}
	for _, path := range []string{"tasks/missing", "tasks/missing/results", "unknown", "tasks/t/unknown"} {
		w := exchange(h, http.MethodGet, "/v1/"+path, "", nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	delete(s.state.Results, "a/1")
	if w := exchange(h, http.MethodGet, "/v1/tasks/t/results", "", nil); w.Code != http.StatusNotFound {
		t.Fatal("missing result is not 404")
	}
	s.runtime.Compatible = false
	if w := exchange(h, http.MethodGet, "/v1/ready", "", nil); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"ready":false`) {
		t.Fatal("incompatible runtime reported ready")
	}
	if w := exchange(h, http.MethodGet, "/v1/doctor", "", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"compatible":false`) {
		t.Fatal("doctor must retain incompatible runtime diagnostics")
	}
}

func TestServiceErrorsAndCancellation(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{core.Invalid("title required"), 400, "invalid_request"},
		{core.NotFound("task missing"), 404, "not_found"},
		{core.ErrUncertain, 409, "uncertain"},
		{&core.Error{Code: "operation_conflict", Message: "operation ID already used"}, 409, "operation_conflict"},
		{&core.Error{Code: "runtime_unavailable", Message: "runtime offline"}, 503, "runtime_unavailable"},
		{errors.New("private-backend-data"), 500, "internal"},
		{context.DeadlineExceeded, 504, "timeout"},
		{context.Canceled, 408, "canceled"},
	} {
		s := newService()
		s.fail = test.err
		w := exchange(New(s, Options{}), http.MethodPost, "/v1/tasks", `{}`, mutationHeaders())
		var payload errorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if w.Code != test.status || payload.Error.Code != test.code || strings.Contains(w.Body.String(), "private-backend-data") {
			t.Fatalf("error mapping: %d %s", w.Code, w.Body.String())
		}
		if len(s.calls) != 1 {
			t.Fatal("service mutation retried")
		}
	}
	s := newService()
	s.snapshot = func(ctx context.Context) (core.State, error) {
		<-ctx.Done()
		return core.State{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/v1/snapshot", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	New(s, Options{}).ServeHTTP(w, r)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("caller deadline lost: %d", w.Code)
	}
}

func TestEventsAreLiveOnlyAndCancel(t *testing.T) {
	s := newService()
	server := httptest.NewServer(New(s, Options{}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Last-Event-ID", "999999")
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("X-Tandem-Events") != "live-only" {
		t.Fatal("missing streaming contract headers")
	}
	reader := bufio.NewReader(response.Body)
	readEvent := func() string {
		t.Helper()
		var event strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line == "\n" {
				break
			}
			event.WriteString(line)
		}
		return event.String()
	}
	first := readEvent()
	if !strings.Contains(first, `"revision":7`) || strings.Contains(first, "id:") || strings.Contains(first, "private-") {
		t.Fatalf("unexpected initial event: %s", first)
	}
	s.mu.Lock()
	s.state.Revision = 8
	s.mu.Unlock()
	next := readEvent()
	if !strings.Contains(next, `"revision":8`) {
		t.Fatalf("change not notified: %s", next)
	}
	cancel()
	_, _ = io.ReadAll(response.Body) // Cancellation may race with a clean server EOF.
}

func TestStreamHonorsEarlierDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		New(newService(), Options{}).ServeHTTP(w, r)
	}()
	select {
	case <-finished:
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "event: revision") {
			t.Fatal("initial stream notification lost")
		}
	case <-time.After(time.Second):
		t.Fatal("stream ignored request deadline")
	}
}
