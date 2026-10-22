package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/api"
	"github.com/samjingwen/tandem/internal/cli"
	"github.com/samjingwen/tandem/internal/client"
)

type serviceCall struct {
	kind, op, id string
	body         any
}

type service struct {
	mu    sync.Mutex
	state core.State
	calls []serviceCall
	fail  error
}

func newService() *service {
	state := core.NewState()
	state.InstanceID = "cli-test"
	state.Projects["p"] = core.Project{ID: "p", Name: "Demo", Path: "/repo", BaseRef: "master"}
	state.Tasks["t"] = core.Task{ID: "t", ProjectID: "p", Title: "Task\x1b[31m\nunsafe", State: "running", Desired: "active", AttemptID: "a", Generation: 1}
	state.Attempts["a"] = core.Attempt{ID: "a", TaskID: "t", Generation: 1}
	state.Decisions["d"] = core.Decision{ID: "d", TaskID: "t", Kind: "question", Prompt: "Continue?", Status: "pending"}
	state.Results["a/1"] = core.Result{TaskID: "t", AttemptID: "a", Generation: 1, Summary: "needs review", Evidence: []core.Evidence{{Name: "tests", Reference: "artifact://tests", Passed: true}}}
	return &service{state: state}
}

func (s *service) Snapshot(context.Context) (core.State, error) { return s.state, nil }
func (s *service) RuntimeInfo(context.Context) (core.RuntimeInfo, error) {
	return core.RuntimeInfo{ID: "fake", Compatible: true}, nil
}

func (s *service) record(kind, op, id string, body any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, serviceCall{kind: kind, op: op, id: id, body: body})
	return s.fail
}

func (s *service) Register(_ context.Context, op string, body core.RegisterProject) (core.Project, error) {
	return core.Project{ID: "p", Name: body.Name}, s.record("register", op, "", body)
}

func (s *service) Create(_ context.Context, op string, body core.CreateTask) (core.Task, error) {
	return core.Task{ID: "t", Title: body.Title}, s.record("create", op, "", body)
}

func (s *service) Followup(_ context.Context, op, id string, body core.Followup) (core.Task, error) {
	return core.Task{ID: id}, s.record("followup", op, id, body)
}

func (s *service) Control(_ context.Context, op, id string, body core.Control) (core.Task, error) {
	return core.Task{ID: id}, s.record("control", op, id, body)
}

func (s *service) Reply(_ context.Context, op, id string, body core.ReplyDecision) (core.Decision, error) {
	return core.Decision{ID: id}, s.record("reply", op, id, body)
}

func (s *service) SubmitResult(_ context.Context, op, id string, body core.SubmitResult) (core.Result, error) {
	return core.Result{TaskID: id, AttemptID: body.AttemptID, Generation: body.Generation, Summary: body.Summary, Evidence: body.Evidence}, s.record("result", op, id, body)
}

func (s *service) Accept(_ context.Context, op, id string) (core.Task, error) {
	return core.Task{ID: id}, s.record("accept", op, id, nil)
}

func (s *service) Cleanup(_ context.Context, op, id string) (core.Workspace, error) {
	return core.Workspace{Owner: id}, s.record("cleanup", op, id, nil)
}

type receipt struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (r *receipt) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.Write(p)
}

func (r *receipt) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.String()
}

func TestCLIMutationsThroughHTTP(t *testing.T) {
	tests := []struct {
		args []string
		kind string
		id   string
		body any
	}{
		{[]string{"projects", "register", "--name", "Demo", "--path", "/repo", "--base-ref", "master", "--instructions", "Project notes"}, "register", "", core.RegisterProject{Name: "Demo", Path: "/repo", BaseRef: "master", Instructions: "Project notes"}},
		{[]string{"tasks", "create", "--project", "p", "--title", "Build", "--instructions", "Do it", "--role", "worker", "--parent", "parent", "--depends-on", "a,b"}, "create", "", core.CreateTask{ProjectID: "p", Title: "Build", Instructions: "Do it", Role: "worker", ParentID: "parent", Dependencies: []string{"a", "b"}}},
		{[]string{"tasks", "followup", "t", "--text", "Continue"}, "followup", "t", core.Followup{Text: "Continue"}},
		{[]string{"tasks", "pause", "t", "--cascade"}, "control", "t", core.Control{Action: "pause", Cascade: true}},
		{[]string{"tasks", "resume", "t"}, "control", "t", core.Control{Action: "resume"}},
		{[]string{"tasks", "cancel", "--cascade=false", "t"}, "control", "t", core.Control{Action: "cancel"}},
		{[]string{"tasks", "retry", "t"}, "control", "t", core.Control{Action: "retry"}},
		{[]string{"tasks", "accept", "t"}, "accept", "t", nil},
		{[]string{"tasks", "cleanup", "t"}, "cleanup", "a", nil},
		{[]string{"tasks", "result", "t", "--attempt", "a", "--generation", "1", "--summary", "Review", "--evidence", `[{"name":"tests","reference":"artifact://tests","passed":false}]`}, "result", "t", core.SubmitResult{AttemptID: "a", Generation: 1, Summary: "Review", Evidence: []core.Evidence{{Name: "tests", Reference: "artifact://tests", Passed: false}}}},
		{[]string{"decisions", "reply", "d", "--answer", `{"choice":"yes"}`}, "reply", "d", core.ReplyDecision{Answer: json.RawMessage(`{"choice":"yes"}`)}},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args[:2], " ")+test.id, func(t *testing.T) {
			s := newService()
			stderr := &receipt{}
			handler := api.New(s, api.Options{Token: "private-cli-token"})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stderr.String() != "Operation ID: supplied-stable-id\n" {
					t.Error("mutation dispatched before operation ID receipt")
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			runner := cli.Runner{Getenv: func(name string) string {
				if name == "TANDEM_TOKEN" {
					return "private-cli-token"
				}
				return "http://127.0.0.1:1" // Explicit --url must take precedence.
			}}
			args := append([]string{"--url", server.URL, "--json"}, test.args...)
			args = append(args, "--operation-id=supplied-stable-id")
			var stdout bytes.Buffer
			if err := runner.Run(context.Background(), args, &stdout, stderr); err != nil {
				t.Fatal(err)
			}
			if !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String()+stderr.String(), "private-cli-token") {
				t.Fatal("machine output invalid or token leaked")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.calls) != 1 {
				t.Fatalf("expected one service call, got %d", len(s.calls))
			}
			got := s.calls[0]
			if got.kind != test.kind || got.op != "supplied-stable-id" || got.id != test.id {
				t.Fatalf("wrong call: %+v", got)
			}
			actual, _ := json.Marshal(got.body)
			want, _ := json.Marshal(test.body)
			if string(actual) != string(want) {
				t.Fatalf("got %s, want %s", actual, want)
			}
		})
	}
}

func TestCLIReadCommandsThroughHTTP(t *testing.T) {
	server := httptest.NewServer(api.New(newService(), api.Options{Token: "read-token"}))
	defer server.Close()
	runner := cli.Runner{Getenv: func(name string) string {
		if name == "TANDEM_URL" {
			return server.URL
		}
		return "read-token"
	}}
	for _, args := range [][]string{
		{"projects", "list"}, {"tasks", "list", "--project", "p", "--state", "running"},
		{"tasks", "show", "t"}, {"tasks", "result", "t"},
		{"decisions", "list", "--task", "t", "--status", "pending"},
		{"snapshot"}, {"doctor"}, {"version"},
	} {
		var stdout, stderr bytes.Buffer
		if err := runner.Run(context.Background(), append(args, "--json"), &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !json.Valid(stdout.Bytes()) || stderr.Len() != 0 {
			t.Fatalf("read output: %s; %s", stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if err := runner.Run(context.Background(), []string{"tasks", "list"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "PROJECTID") || strings.ContainsAny(stdout.String(), "\x1b\r") || !strings.Contains(stdout.String(), "running") {
		t.Fatalf("human table is missing fields or contains terminal controls: %q", stdout.String())
	}
}

func TestGeneratedOperationIDAndUncertainFailure(t *testing.T) {
	s := newService()
	s.fail = core.ErrUncertain
	server := httptest.NewServer(api.New(s, api.Options{}))
	defer server.Close()
	runner := cli.Runner{Getenv: func(string) string { return "" }}
	var stdout, stderr bytes.Buffer
	err := runner.Run(context.Background(), []string{"tasks", "accept", "t", "--url", server.URL}, &stdout, &stderr)
	var typed *client.Error
	if !errors.As(err, &typed) || typed.Code != "uncertain" || !strings.HasPrefix(typed.OperationID, "op_") {
		t.Fatalf("wrong uncertain error: %v", err)
	}
	if stderr.String() != "Operation ID: "+typed.OperationID+"\n" || stdout.Len() != 0 {
		t.Fatal("missing pre-dispatch receipt or misleading stdout")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 1 || s.calls[0].op != typed.OperationID {
		t.Fatal("uncertain command retried or changed operation ID")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("writer unavailable") }

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 1, nil }

func TestNoDispatchWithoutReceipt(t *testing.T) {
	s := newService()
	server := httptest.NewServer(api.New(s, api.Options{}))
	defer server.Close()
	runner := cli.Runner{Getenv: func(string) string { return "" }}
	for _, writer := range []io.Writer{brokenWriter{}, shortWriter{}} {
		err := runner.Run(context.Background(), []string{"tasks", "accept", "t", "--url", server.URL}, io.Discard, writer)
		if err == nil || !strings.Contains(err.Error(), "before dispatch") {
			t.Fatalf("wrong receipt failure: %v", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 0 {
		t.Fatal("dispatch happened despite failed receipt")
	}
}

func TestInputAndInvalidCommands(t *testing.T) {
	s := newService()
	server := httptest.NewServer(api.New(s, api.Options{}))
	defer server.Close()
	for _, test := range []struct {
		args  []string
		input string
	}{
		{[]string{"tasks", "followup", "t", "--text", "-"}, "stdin followup\n"},
		{[]string{"tasks", "followup", "t", "--text", "--help"}, ""},
		{[]string{"tasks", "followup", "t", "--text", "--json"}, ""},
		{[]string{"tasks", "create", "--project", "p", "--title", "From stdin", "--instructions", "-"}, "stdin instructions\n"},
		{[]string{"decisions", "reply", "d", "--answer", "-"}, `{"answer":"stdin"}`},
		{[]string{"tasks", "result", "t", "--attempt", "a", "--generation", "1", "--summary", "Review", "--evidence", "-"}, `[{"name":"test","reference":"artifact://stdin","passed":true}]`},
	} {
		runner := cli.Runner{In: strings.NewReader(test.input), Getenv: func(string) string { return "" }}
		args := append(test.args, "--url", server.URL)
		if err := runner.Run(context.Background(), args, io.Discard, io.Discard); err != nil {
			t.Fatalf("stdin %v: %v", test.args, err)
		}
	}
	s.mu.Lock()
	initial := len(s.calls)
	s.mu.Unlock()
	for _, args := range [][]string{
		{"serve"}, {"tasks", "create"}, {"tasks", "show"}, {"tasks", "show", "../other"},
		{"tasks", "accept", "t", "--force"}, {"tasks", "pause", "t", "--cascade=bad"},
		{"tasks", "result", "t", "--summary", "Missing attempt"},
		{"tasks", "result", "t", "--attempt", "a", "--generation", "1", "--summary", "x", "--evidence", `[{"unknown":true}]`},
		{"tasks", "result", "t", "--attempt", "a", "--summary", "x"},
		{"tasks", "result", "t", "--attempt", "a", "--generation", "0", "--summary", "x"},
		{"tasks", "result", "t", "--attempt", "a", "--generation", "-1", "--summary", "x"},
		{"tasks", "result", "t", "--attempt", "a", "--generation", "1.5", "--summary", "x"},
		{"tasks", "result", "t", "--attempt", "a", "--generation", "bad", "--summary", "x"},
		{"decisions", "reply", "d", "--answer", "not-json"},
		{"tasks", "followup", "t", "--text", "a", "--text", "b"},
		{"snapshot", "--operation-id", "read-op"}, {"snapshot", "--json=invalid"},
		{"snapshot", "--url"}, {"snapshot", "--url", server.URL, "--url", server.URL},
	} {
		runner := cli.Runner{Getenv: func(name string) string {
			if name == "TANDEM_URL" {
				return server.URL
			}
			return ""
		}}
		if err := runner.Run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Errorf("invalid command accepted: %v", args)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != initial {
		t.Fatal("invalid CLI command invoked service")
	}
}

func TestHelpDoesNotUseTheNetwork(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"tasks", "create", "--help"}, {"-h"}} {
		var stdout bytes.Buffer
		runner := cli.Runner{Getenv: func(string) string { t.Fatal("help consulted environment"); return "" }}
		if err := runner.Run(context.Background(), args, &stdout, io.Discard); err != nil || !strings.Contains(stdout.String(), "Tandem HTTP client") {
			t.Fatalf("help %v: %s (%v)", args, stdout.String(), err)
		}
	}
}
