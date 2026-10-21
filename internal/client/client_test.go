package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/api"
	"github.com/samjingwen/tandem/internal/client"
)

type service struct {
	core.Service
	creates atomic.Int32
	seenOp  string
	fail    error
}

func (s *service) Snapshot(context.Context) (core.State, error) {
	state := core.NewState()
	state.InstanceID = "client-e2e"
	return state, nil
}

func (s *service) Create(_ context.Context, op string, body core.CreateTask) (core.Task, error) {
	s.creates.Add(1)
	s.seenOp = op
	return core.Task{ID: "task", ProjectID: body.ProjectID, Title: body.Title}, s.fail
}

func TestClientAgainstAPI(t *testing.T) {
	s := &service{}
	server := httptest.NewServer(api.New(s, api.Options{Token: "test-secret"}))
	defer server.Close()
	c, err := client.New(client.Options{URL: server.URL, Token: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	var state core.State
	if err := c.Do(context.Background(), http.MethodGet, "/v1/snapshot", "", nil, &state); err != nil || state.InstanceID != "client-e2e" {
		t.Fatalf("snapshot: %+v, %v", state, err)
	}
	var task core.Task
	if err := c.Do(context.Background(), http.MethodPost, "/v1/tasks", "stable-client-op", core.CreateTask{ProjectID: "p", Title: "test"}, &task); err != nil || task.ID != "task" {
		t.Fatalf("create: %+v, %v", task, err)
	}
	if s.seenOp != "stable-client-op" || s.creates.Load() != 1 {
		t.Fatal("operation ID not passed exactly once")
	}
	s.fail = core.ErrUncertain
	err = c.Do(context.Background(), http.MethodPost, "/v1/tasks", "second-op", core.CreateTask{}, &task)
	var typed *client.Error
	if !errors.As(err, &typed) || typed.Code != "uncertain" || typed.StatusCode != 409 || typed.OperationID != "second-op" {
		t.Fatalf("typed API error lost: %v", err)
	}
	if s.creates.Load() != 2 {
		t.Fatal("failed mutation was retried")
	}
	unauthorized, err := client.New(client.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = unauthorized.Do(context.Background(), http.MethodGet, "/v1/health", "", nil, nil)
	if !errors.As(err, &typed) || typed.StatusCode != 401 {
		t.Fatalf("missing auth error: %v", err)
	}
}

func TestNoRedirectsOrHTTPRetries(t *testing.T) {
	for _, status := range []int{302, 307, 308, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var initial, target atomic.Int32
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				target.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer redirect.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				initial.Add(1)
				if r.Header.Get("X-Operation-ID") != "stable" {
					t.Error("operation ID missing")
				}
				w.Header().Set("Location", redirect.URL+"/v1/tasks")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":"test_failure","message":"failed"}}`)
			}))
			defer server.Close()
			c, err := client.New(client.Options{URL: server.URL, Token: "test-secret", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			err = c.Do(context.Background(), http.MethodPost, "/v1/tasks", "stable", core.CreateTask{}, nil)
			if err == nil || initial.Load() != 1 || target.Load() != 0 {
				t.Fatalf("followed redirect or retried: initial=%d target=%d err=%v", initial.Load(), target.Load(), err)
			}
		})
	}
}

func TestAmbiguousDisconnectIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close() // The mutation may have committed before this loss.
	}))
	defer server.Close()
	c, err := client.New(client.Options{URL: server.URL, Token: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), http.MethodPost, "/v1/tasks", "stable-disconnect", core.CreateTask{}, nil)
	var transport *client.TransportError
	if !errors.As(err, &transport) || !transport.Uncertain || transport.OperationID != "stable-disconnect" || calls.Load() != 1 {
		t.Fatalf("ambiguous disconnect: calls=%d err=%v", calls.Load(), err)
	}
	if strings.Contains(err.Error(), "test-secret") {
		t.Fatal("token leaked")
	}
}

func TestNilMutationInputUsesNonReplayableObjectBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" || r.ContentLength != 2 {
			t.Errorf("mutation body must be nonempty: %q length=%d", body, r.ContentLength)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	c, err := client.New(client.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), http.MethodPost, "/v1/tasks/t/accept", "nil-input", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidationAndBoundedResponses(t *testing.T) {
	for _, value := range []string{"ftp://host", "https://user:private-secret@host", "http://host/v1", "http://host?token=private-secret", "http://host#private-secret", "://"} {
		_, err := client.New(client.Options{URL: value})
		if err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("unsafe URL accepted or exposed: %v", err)
		}
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	c, err := client.New(client.Options{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ method, path, op string }{
		{http.MethodPost, "/v1/tasks", ""},
		{http.MethodPost, "/v1/tasks", "bad value"},
		{http.MethodPost, "/v1/tasks", strings.Repeat("a", 129)},
		{http.MethodPut, "/v1/tasks", "op"},
		{http.MethodGet, "https://evil.example/v1/tasks", ""},
		{http.MethodGet, "//evil.example/v1/tasks", ""},
		{http.MethodGet, "/not-v1", ""},
		{http.MethodGet, "/v1/../secret", ""},
	} {
		if err := c.Do(context.Background(), test.method, test.path, test.op, nil, nil); err == nil {
			t.Fatalf("invalid request accepted: %+v", test)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached network")
	}
	for _, test := range []struct {
		body string
		code string
	}{
		{"private-invalid-response", "invalid_response"},
		{`{} {}`, "invalid_response"},
		{strings.Repeat("x", (8<<20)+1), "response_too_large"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, test.body) }))
		c, err := client.New(client.Options{URL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		err = c.Do(context.Background(), http.MethodGet, "/v1/snapshot", "", nil, nil)
		server.Close()
		var typed *client.Error
		if !errors.As(err, &typed) || typed.Code != test.code || strings.Contains(err.Error(), "private-invalid-response") {
			t.Fatalf("bad response: %v", err)
		}
	}
}

func TestFromEnvAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer environment-test-token" {
			t.Error("environment token missing")
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	t.Setenv("TANDEM_URL", server.URL)
	t.Setenv("TANDEM_TOKEN", "environment-test-token")
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var output json.RawMessage
	if err := c.Do(context.Background(), http.MethodGet, "/v1/health", "", nil, &output); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = c.Do(ctx, http.MethodGet, "/v1/health", "", nil, nil)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "environment-test-token") {
		t.Fatalf("cancellation not preserved or secret leaked: %v", err)
	}
}
