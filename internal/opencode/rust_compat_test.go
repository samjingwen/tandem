package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
)

// The independent oracle is the pinned Go adapter plus the wire/ownership/
// uncertainty contracts in docs/OPENCODE.md. Only the HTTP server is simulated;
// the Rust consumer executes the actual HTTP/SSE adapter, never a copied model.
type pinnedRuntime interface {
	core.Runtime
	core.PromptReconciler
	core.PendingCanceller
	core.QuiescenceChecker
}

type rustRuntime struct {
	t     *testing.T
	url   string
	model *Model
}

func requireRustRuntime(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("TANDEM_RUST_OPENCODE_HELPER")
	if binary == "" {
		t.Skip("make rust-runtime-parity supplies the real Rust adapter consumer")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("runtime fixture path must be absolute")
	}
	return binary
}

type runtimeResponse struct {
	Value json.RawMessage `json:"value"`
	Wakes int             `json:"wakes"`
	Error *struct {
		Code         string
		Message      string
		NotDelivered bool
	} `json:"error"`
}

func (r *rustRuntime) call(ctx context.Context, method string, spec core.SessionSpec, extra map[string]any, output any) (int, error) {
	r.t.Helper()
	input := map[string]any{"url": r.url, "password": "test-password", "method": method, "spec": spec, "model": r.model}
	for key, value := range extra {
		input[key] = value
	}
	if deadline, ok := ctx.Deadline(); ok {
		input["timeoutMs"] = max(0, time.Until(deadline).Milliseconds())
	}
	if ctx.Err() != nil {
		input["canceled"] = true
	}
	body, err := json.Marshal(input)
	if err != nil {
		r.t.Fatal(err)
	}
	processContext, cancel := context.WithTimeout(r.t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(processContext, requireRustRuntime(r.t))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + r.t.TempDir()}
	cmd.Stdin = bytes.NewReader(body)
	data, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("Rust runtime consumer: %v: %s", err, data)
	}
	var response runtimeResponse
	if err = json.Unmarshal(data, &response); err != nil {
		r.t.Fatalf("Rust output: %v: %s", err, data)
	}
	if response.Error != nil {
		var result error
		switch response.Error.Code {
		case "uncertain":
			result = core.ErrUncertain
		case "invalid_request":
			result = core.Invalid(response.Error.Message)
		case "runtime_ownership":
			result = ErrOwnership
		case "runtime_version":
			result = ErrVersion
		case "runtime_eof":
			result = io.EOF
		case "canceled":
			result = context.Canceled
		case "timeout":
			result = context.DeadlineExceeded
		default:
			result = errors.New(response.Error.Message)
		}
		if response.Error.NotDelivered {
			result = &core.NotDelivered{Err: result}
		}
		return response.Wakes, result
	}
	if output != nil {
		if err = json.Unmarshal(response.Value, output); err != nil {
			r.t.Fatalf("Rust result: %v", err)
		}
	}
	return response.Wakes, nil
}

func (r *rustRuntime) Info(ctx context.Context) (value core.RuntimeInfo, err error) {
	_, err = r.call(ctx, "info", core.SessionSpec{}, nil, &value)
	return
}
func (r *rustRuntime) Reference(command string) (value string) {
	_, err := r.call(r.t.Context(), "reference", core.SessionSpec{}, map[string]any{"command": command}, &value)
	if err != nil {
		r.t.Fatal(err)
	}
	return
}
func (r *rustRuntime) Ensure(ctx context.Context, spec core.SessionSpec) error {
	_, err := r.call(ctx, "ensure", spec, nil, nil)
	return err
}
func (r *rustRuntime) Inspect(ctx context.Context, spec core.SessionSpec) (value core.Observation, err error) {
	_, err = r.call(ctx, "inspect", spec, nil, &value)
	return
}
func (r *rustRuntime) Prompt(ctx context.Context, spec core.SessionSpec, command, text string) error {
	_, err := r.call(ctx, "prompt", spec, map[string]any{"command": command, "text": text}, nil)
	return err
}
func (r *rustRuntime) ReconcilePrompt(ctx context.Context, spec core.SessionSpec, command, text string) error {
	_, err := r.call(ctx, "reconcile", spec, map[string]any{"command": command, "text": text}, nil)
	return err
}
func (r *rustRuntime) Interrupt(ctx context.Context, spec core.SessionSpec) error {
	_, err := r.call(ctx, "interrupt", spec, nil, nil)
	return err
}
func (r *rustRuntime) CancelPending(ctx context.Context, spec core.SessionSpec) error {
	_, err := r.call(ctx, "cancel", spec, nil, nil)
	return err
}
func (r *rustRuntime) CheckQuiescence(ctx context.Context, spec core.SessionSpec) error {
	_, err := r.call(ctx, "quiescence", spec, nil, nil)
	return err
}
func (r *rustRuntime) Reply(ctx context.Context, spec core.SessionSpec, decision core.Decision, answer json.RawMessage) error {
	_, err := r.call(ctx, "reply", spec, map[string]any{"decision": decision, "answer": answer}, nil)
	return err
}
func (r *rustRuntime) Events(ctx context.Context, notify func()) error {
	wakes, err := r.call(ctx, "events", core.SessionSpec{}, nil, nil)
	for range wakes {
		notify()
	}
	return err
}

type runtimeFactory func(*testing.T, string, *Model) pinnedRuntime

func eachRuntime(t *testing.T, run func(*testing.T, runtimeFactory)) {
	t.Helper()
	_ = requireRustRuntime(t)
	for _, language := range []string{"go", "rust"} {
		t.Run(language, func(t *testing.T) {
			run(t, func(t *testing.T, url string, model *Model) pinnedRuntime {
				if language == "rust" {
					return &rustRuntime{t: t, url: url, model: model}
				}
				runtime, err := New(Options{URL: url, Password: "test-password", Model: model})
				if err != nil {
					t.Fatal(err)
				}
				return runtime
			})
		})
	}
}

func authenticatedFixture(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "opencode" || password != "test-password" {
			t.Error("wrong Basic authentication")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRustOpenCodeIdentityAndExactCreatePolicy(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		for _, role := range []string{"worker", "coordinator", ""} {
			t.Run("role="+role, func(t *testing.T) {
				spec := testSpec()
				spec.Role = role
				var saved *session
				var mu sync.Mutex
				var posts atomic.Int32
				model := &Model{ID: "model", ProviderID: "provider", Variant: "variant"}
				server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case r.URL.Path == "/api/info":
						writeJSON(w, map[string]any{"version": Version})
					case r.Method == "GET" && r.URL.Path == sessionPath(spec):
						if saved == nil {
							http.NotFound(w, r)
						} else {
							writeData(w, saved)
						}
					case r.Method == "POST" && r.URL.Path == "/api/session":
						posts.Add(1)
						var input struct {
							ID, Title   string
							Model       *Model
							Location    location
							Metadata    metadata
							Permissions []permissionRule
							ParentID    string
						}
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						if err := json.Unmarshal(body, &input); err != nil {
							t.Error(err)
							return
						}
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(body, &fields); err != nil {
							t.Error(err)
							return
						}
						if len(fields) != 6 || fields["agent"] != nil || fields["parentID"] != nil {
							t.Error("unexpected native agent/parent/configuration override")
						}
						if input.ID != spec.Reference || input.Title != spec.Title || input.Location.Directory != spec.Directory || input.Metadata != owner(spec, "") || !reflect.DeepEqual(input.Permissions, sessionPermissions(spec)) || !reflect.DeepEqual(input.Model, model) || input.ParentID != "" {
							t.Errorf("incorrect native create body: %+v", input)
						}
						s := ownedSession(spec)
						saved = &s
						writeData(w, s)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						http.Error(w, "unexpected", 500)
					}
				})
				runtime := newRuntime(t, server.URL+"/", model)
				if runtime.Reference("attempt") != "ses_4e11e895ca0608470a01070215037a08" {
					t.Fatal("reference differs from independent SHA-256 example")
				}
				first, err := runtime.Info(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				other, err := newRuntime(t, server.URL, model).Info(t.Context())
				if err != nil || first.ID != other.ID || first.Version != Version || !first.Compatible {
					t.Fatalf("info mismatch: %+v %+v %v", first, other, err)
				}
				for range 2 {
					if err := newRuntime(t, server.URL, model).Ensure(t.Context(), spec); err != nil {
						t.Fatal(err)
					}
				}
				if posts.Load() != 1 {
					t.Fatal("existing owned session recreated")
				}
				mu.Lock()
				saved.Metadata.Tandem.Directory = "/foreign"
				mu.Unlock()
				if !errors.Is(runtime.Ensure(t.Context(), spec), ErrOwnership) || posts.Load() != 1 {
					t.Fatal("foreign assignment adopted or overwritten")
				}
				mu.Lock()
				saved.Metadata = owner(spec, "")
				saved.Permissions[0].Effect = "allow"
				mu.Unlock()
				if !errors.Is(runtime.Ensure(t.Context(), spec), ErrOwnership) || posts.Load() != 1 {
					t.Fatal("edited native capability policy was adopted or rewritten")
				}
			})
		}
	})
}

func TestRustOpenCodeOnly404CreatesAndAmbiguityNeverResends(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		for _, status := range []int{200, 400, 401, 403, 404, 408, 409, 500, 503} {
			t.Run(fmt.Sprint(status), func(t *testing.T) {
				spec := testSpec()
				var posts atomic.Int32
				server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/info" {
						writeJSON(w, map[string]any{"version": Version})
						return
					}
					if r.Method == "POST" {
						posts.Add(1)
						writeData(w, ownedSession(spec))
						return
					}
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"sensitive":"must-not-escape"}`)
				})
				err := newRuntime(t, server.URL, nil).Ensure(t.Context(), spec)
				if status == 404 {
					if err != nil || posts.Load() != 1 {
						t.Fatalf("404 didn't create: %v", err)
					}
				} else if err == nil || posts.Load() != 0 {
					t.Fatalf("non-404 created: status %d err %v", status, err)
				}
				if err != nil && strings.Contains(err.Error(), "must-not-escape") {
					t.Fatal("native body leaked")
				}
			})
		}
		for _, evidence := range []string{"inbox", "message", "absent", "collision", "inbox-collision", "projection-race"} {
			t.Run(evidence, func(t *testing.T) {
				spec := testSpec()
				const command = "dispatch"
				const text = "exact <&> 世界"
				id := "msg_" + digest("tandem/prompt/"+command)
				var posts atomic.Int32
				var admitted bool
				var projected bool
				var mu sync.Mutex
				server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case r.URL.Path == "/api/info":
						writeJSON(w, map[string]any{"version": Version})
					case r.URL.Path == sessionPath(spec):
						writeData(w, ownedSession(spec))
					case strings.Contains(r.URL.Path, "/message/"):
						if admitted && (evidence == "message" || evidence == "collision" || (evidence == "projection-race" && projected)) {
							content := text
							if evidence == "collision" {
								content = "foreign"
							}
							writeData(w, message{ID: id, Type: "user", Text: content, Metadata: owner(spec, command)})
						} else {
							http.NotFound(w, r)
						}
					case strings.HasSuffix(r.URL.Path, "/inbox"):
						items := []admission{}
						if admitted && (evidence == "inbox" || evidence == "inbox-collision") {
							content := text
							if evidence == "inbox-collision" {
								content = "foreign"
							}
							items = append(items, admission{ID: id, SessionID: spec.Reference, Type: "user", Payload: promptPayload{Text: content, Metadata: owner(spec, command)}})
						}
						if admitted && evidence == "projection-race" {
							projected = true
						}
						writeData(w, items)
					case strings.HasSuffix(r.URL.Path, "/prompt"):
						posts.Add(1)
						admitted = true
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL)
					}
				})
				runtime := newRuntime(t, server.URL, nil)
				if !errors.Is(runtime.Prompt(t.Context(), spec, command, text), core.ErrUncertain) || posts.Load() != 1 {
					t.Fatal("lost prompt not uncertain or retried")
				}
				err := newRuntime(t, server.URL, nil).ReconcilePrompt(t.Context(), spec, command, text)
				if evidence == "absent" {
					requireError(t, err, core.ErrUncertain)
				} else if evidence == "collision" || evidence == "inbox-collision" {
					requireError(t, err, ErrOwnership)
				} else if err != nil {
					t.Fatal(err)
				}
				if posts.Load() != 1 {
					t.Fatal("read-only recovery reposted")
				}
				if evidence == "message" || evidence == "inbox" || evidence == "projection-race" {
					if err := newRuntime(t, server.URL, nil).Prompt(t.Context(), spec, command, text); err != nil || posts.Load() != 1 {
						t.Fatal("known admission woke again")
					}
				}
			})
		}
	})
}

func TestRustOpenCodeReplyProofAndPendingIdentity(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		for _, kind := range []string{"permission", "form"} {
			for _, status := range []int{200, 400, 401, 403, 404, 408, 409, 500} {
				t.Run(fmt.Sprintf("%s/%d", kind, status), func(t *testing.T) {
					spec := testSpec()
					var posts atomic.Int32
					var missing atomic.Bool
					details := json.RawMessage(fmt.Sprintf(`{"id":"request","sessionID":%q,"title":"Question","action":"shell","extra":{"n":1}}`, spec.Reference))
					decision := core.Decision{TaskID: spec.TaskID, AttemptID: spec.AttemptID, RuntimeRef: "request", Kind: kind, Details: details}
					answer := json.RawMessage(`{"decision":"once","message":"Explicit user approval"}`)
					if kind == "form" {
						answer = json.RawMessage(`{"answer":{"approved":true,"count":9007199254740993,"choices":["one"]}}`)
					}
					server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.URL.Path == "/api/info":
							writeJSON(w, map[string]any{"version": Version})
						case r.URL.Path == sessionPath(spec):
							writeData(w, ownedSession(spec))
						case strings.HasSuffix(r.URL.Path, "/reply"):
							posts.Add(1)
							body, _ := io.ReadAll(r.Body)
							if !bytes.Equal(body, answer) {
								t.Errorf("native answer changed: %s", body)
							}
							w.WriteHeader(status)
						case strings.HasSuffix(r.URL.Path, "/"+kind):
							items := []json.RawMessage{}
							if !missing.Load() {
								items = append(items, details)
							}
							writeData(w, items)
						default:
							writeData(w, []any{})
						}
					})
					runtime := newRuntime(t, server.URL, nil)
					err := runtime.Reply(t.Context(), spec, decision, answer)
					var proof *core.NotDelivered
					if errors.As(err, &proof) != (status == 400 || status == 401 || status == 403) {
						t.Fatalf("incorrect non-delivery proof at status %d: %v", status, err)
					}
					if (err == nil) != (status == 200) || posts.Load() != 1 {
						t.Fatalf("reply status/count: %v %d", err, posts.Load())
					}
					if status == 404 || status == 408 || status == 409 || status >= 500 {
						requireError(t, err, core.ErrUncertain)
					}
					decision.Details = json.RawMessage(`{"changed":true}`)
					requireError(t, runtime.Reply(t.Context(), spec, decision, answer), core.ErrUncertain)
					if posts.Load() != 1 {
						t.Fatal("stale decision replied")
					}
					missing.Store(true)
					decision.Details = details
					requireError(t, runtime.Reply(t.Context(), spec, decision, answer), core.ErrUncertain)
					if posts.Load() != 1 {
						t.Fatal("missing decision treated as an answered or retryable reply")
					}
					invalid := json.RawMessage(`{"reply":"once"}`)
					if kind == "form" {
						invalid = json.RawMessage(`{"answer":null}`)
					}
					err = runtime.Reply(t.Context(), spec, decision, invalid)
					if !errors.As(err, &proof) || posts.Load() != 1 {
						t.Fatal("invalid native answer was sent or failed to prove non-delivery")
					}
				})
			}
		}
	})
}

func TestRustOpenCodeScopedQuiescenceAndObservation(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		for _, state := range []string{"idle", "failed", "running", "permission", "form", "shell", "pty", "persistent", "foreign", "unknown", "missing-inventory"} {
			t.Run(state, func(t *testing.T) {
				spec := testSpec()
				var writes atomic.Int32
				server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						writes.Add(1)
						t.Error("quiescence mutated runtime")
					}
					switch r.URL.Path {
					case "/api/info":
						writeJSON(w, map[string]any{"version": Version, "capabilities": map[string]any{"persistentPty": true}})
					case sessionPath(spec):
						s := ownedSession(spec)
						s.Outcome = "succeeded"
						if state == "failed" {
							s.Outcome = "failed"
						}
						writeData(w, s)
					case "/api/session/active":
						active := map[string]any{}
						if state == "running" {
							active[spec.Reference] = map[string]any{"type": "running"}
						}
						writeData(w, active)
					case sessionPath(spec) + "/permission", sessionPath(spec) + "/form":
						items := []any{}
						if strings.HasSuffix(r.URL.Path, "/"+state) {
							items = append(items, map[string]any{"id": "decision", "sessionID": spec.Reference, "title": "Question"})
						}
						writeData(w, items)
					case "/api/shell", "/api/pty":
						if r.URL.Query().Get("location[directory]") != spec.Directory {
							t.Error("inventory not location scoped")
						}
						where := spec.Directory
						if state == "foreign" {
							where = "/foreign"
						}
						resources := []any{}
						if r.URL.Path == "/api/"+state {
							resources = append(resources, map[string]any{"status": "running"})
						}
						if state == "unknown" {
							resources = append(resources, map[string]any{"status": "mystery"})
						}
						if state == "missing-inventory" {
							writeJSON(w, map[string]any{"location": location{Directory: where}})
						} else {
							writeJSON(w, map[string]any{"location": location{Directory: where}, "data": resources})
						}
					case "/api/experimental/session/" + spec.Reference + "/terminal":
						items := []any{}
						if state == "persistent" {
							items = append(items, map[string]any{"sessionID": spec.Reference, "status": "running"})
						}
						writeData(w, items)
					default:
						t.Errorf("unexpected endpoint %s", r.URL)
					}
				})
				runtime := newRuntime(t, server.URL, nil)
				observation, err := runtime.Inspect(t.Context(), spec)
				if err != nil || !observation.Exists || !observation.Owned {
					t.Fatalf("inspect: %+v %v", observation, err)
				}
				want := "idle"
				if state == "running" || state == "failed" {
					want = state
				}
				if observation.State != want {
					t.Fatalf("observation state %s", observation.State)
				}
				err = runtime.CheckQuiescence(t.Context(), spec)
				if (err == nil) != (state == "idle" || state == "failed") || writes.Load() != 0 {
					t.Fatalf("unsafe cleanup permission for %s: %v", state, err)
				}
			})
		}
	})
}

func TestRustOpenCodeSSEBoundsAndCancellation(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		for _, scenario := range []struct {
			name, body, content string
			wakes               int
			hold                bool
		}{
			{"valid", ": comment\r\ndata: {\r\ndata: \"type\":\"foreign-location-event\",\r\ndata: \"secret\":\"never-forward\"}\r\n\r\ndata: {\"type\":\"next\"}\n\ndata: {\"type\":\"partial\"}", "text/event-stream; charset=utf-8", 2, false},
			{"double-encoded", "data: \"{\\\"type\\\":\\\"bad\\\"}\"\n\n", "text/event-stream", 0, false},
			{"missing-type", "data: {}\n\n", "text/event-stream", 0, false},
			{"line-bound", "data: " + strings.Repeat("x", 1<<20) + "\n\n", "text/event-stream", 0, false},
			{"frame-bound", strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", 1025) + "\n", "text/event-stream", 0, false},
			{"content-type", "data: {\"type\":\"wrong\"}\n\n", "application/json", 0, false},
			{"deadline", "", "text/event-stream", 0, true},
			{"cancel", "data: {\"type\":\"cancel-now\"}\n\n", "text/event-stream", 1, true},
		} {
			t.Run(scenario.name, func(t *testing.T) {
				var streams atomic.Int32
				server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/info" {
						writeJSON(w, map[string]any{"version": Version})
						return
					}
					if r.URL.Path != "/api/event" || r.URL.RawQuery != "" || r.Header.Get("Last-Event-ID") != "" {
						t.Error("unexpected SSE path/replay")
					}
					streams.Add(1)
					w.Header().Set("Content-Type", scenario.content)
					w.WriteHeader(200)
					_, _ = io.WriteString(w, scenario.body)
					w.(http.Flusher).Flush()
					if scenario.hold {
						<-r.Context().Done()
					}
				})
				ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
				defer cancel()
				var wakes atomic.Int32
				runtime := newRuntime(t, server.URL, nil)
				var err error
				if scenario.name == "cancel" {
					if rust, ok := runtime.(*rustRuntime); ok {
						count, result := rust.call(ctx, "events", core.SessionSpec{}, map[string]any{"cancelOnEvent": true}, nil)
						wakes.Store(int32(count))
						err = result
					} else {
						cancelContext, stop := context.WithCancel(ctx)
						defer stop()
						err = runtime.Events(cancelContext, func() { wakes.Add(1); stop() })
					}
					requireError(t, err, context.Canceled)
				} else {
					err = runtime.Events(ctx, func() { wakes.Add(1) })
				}
				if err == nil || wakes.Load() != int32(scenario.wakes) || streams.Load() != 1 {
					t.Fatalf("SSE result: wakes=%d streams=%d err=%v", wakes.Load(), streams.Load(), err)
				}
				if strings.Contains(err.Error(), "never-forward") {
					t.Fatal("event payload leaked")
				}
			})
		}
	})
}

func TestRustOpenCodePromptNonDeliveryAndLiveVersion(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		for _, stage := range []string{"preflight", "post"} {
			for _, status := range []int{200, 302, 307, 400, 401, 403, 404, 408, 409, 429, 500, 503} {
				t.Run(fmt.Sprintf("%s/%d", stage, status), func(t *testing.T) {
					spec := testSpec()
					var posts atomic.Int32
					server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
						if r.Method == "POST" {
							posts.Add(1)
							w.WriteHeader(status)
							_, _ = io.WriteString(w, `{"sensitive":"never-echo-native-body"}`)
							return
						}
						switch r.URL.Path {
						case "/api/info":
							if stage == "preflight" {
								w.WriteHeader(status)
								_, _ = io.WriteString(w, `malformed`)
							} else {
								writeJSON(w, map[string]any{"version": Version})
							}
						case sessionPath(spec):
							writeData(w, ownedSession(spec))
						case sessionPath(spec) + "/inbox":
							writeData(w, []any{})
						default:
							http.NotFound(w, r)
						}
					})
					err := newRuntime(t, server.URL, nil).Prompt(t.Context(), spec, "dispatch", "text")
					var proof *core.NotDelivered
					wantProof := stage == "preflight" || status == 400 || status == 401 || status == 403
					if err == nil || errors.As(err, &proof) != wantProof {
						t.Fatalf("non-delivery classification: %v", err)
					}
					wantPosts := int32(1)
					if stage == "preflight" {
						wantPosts = 0
					}
					if posts.Load() != wantPosts {
						t.Fatal("preflight posted, or mutation automatically retried")
					}
					if stage == "post" && (status == 200 || status < 400 || status == 408 || status >= 500) {
						requireError(t, err, core.ErrUncertain)
					}
					if strings.Contains(err.Error(), "never-echo-native-body") {
						t.Fatal("response content leaked")
					}
				})
			}
		}
		var version atomic.Value
		version.Store(Version)
		var writes atomic.Int32
		server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				writes.Add(1)
			}
			if r.URL.Path == "/api/info" {
				writeJSON(w, map[string]any{"version": version.Load()})
				return
			}
			writeData(w, ownedSession(testSpec()))
		})
		runtime := newRuntime(t, server.URL, nil)
		if err := runtime.Ensure(t.Context(), testSpec()); err != nil {
			t.Fatal(err)
		}
		version.Store("2.1.4")
		requireError(t, runtime.Ensure(t.Context(), testSpec()), ErrVersion)
		if writes.Load() != 0 {
			t.Fatal("version drift mutated session")
		}
	})
}

func TestRustOpenCodeLostCreateAndSelectiveCancellation(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		spec := testSpec()
		var mu sync.Mutex
		var saved *session
		var creates, interrupts atomic.Int32
		owned := admission{ID: "msg_" + digest("tandem/prompt/cancel-me"), SessionID: spec.Reference, Type: "user", Payload: promptPayload{Text: "text", Metadata: owner(spec, "cancel-me")}}
		foreign := owned
		foreign.Payload.Metadata = owner(spec, "foreign")
		foreign.Payload.Metadata.Tandem.InstanceID = "elsewhere"
		wrongID := owned
		wrongID.ID = "msg_notmatching"
		control := owned
		control.Type = "compaction"
		inbox := []admission{foreign, wrongID, control, owned}
		var deleted []string
		server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.URL.Path == "/api/info":
				writeJSON(w, map[string]any{"version": Version})
			case r.Method == "POST" && r.URL.Path == "/api/session":
				creates.Add(1)
				s := ownedSession(spec)
				saved = &s
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
			case r.URL.Path == sessionPath(spec):
				if saved == nil {
					http.NotFound(w, r)
				} else {
					writeData(w, saved)
				}
			case strings.HasSuffix(r.URL.Path, "/interrupt"):
				interrupts.Add(1)
				if r.URL.Query().Get("resume") != "false" {
					t.Error("interrupt resumed execution")
				}
				writeJSON(w, map[string]any{"interrupted": false})
			case r.Method == "DELETE":
				deleted = append(deleted, r.URL.Path)
				if r.URL.Path != sessionPath(spec)+"/inbox/"+owned.ID {
					t.Error("foreign/control input deleted")
				}
				inbox = inbox[:3]
				w.WriteHeader(204)
			case strings.HasSuffix(r.URL.Path, "/inbox"):
				writeData(w, inbox)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL)
			}
		})
		requireError(t, newRuntime(t, server.URL, nil).Ensure(t.Context(), spec), core.ErrUncertain)
		if err := newRuntime(t, server.URL, nil).Ensure(t.Context(), spec); err != nil || creates.Load() != 1 {
			t.Fatalf("lost create duplicated or lost ownership: %v", err)
		}
		if err := newRuntime(t, server.URL, nil).Interrupt(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		parked := len(inbox) == 4 && len(deleted) == 0
		mu.Unlock()
		if !parked {
			t.Fatal("pause interruption deleted pending inputs")
		}
		for range 2 {
			if err := newRuntime(t, server.URL, nil).CancelPending(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(deleted) != 1 || len(inbox) != 3 || interrupts.Load() != 3 {
			t.Fatalf("unexpected cancellation effects: deletes=%v inbox=%d interrupts=%d", deleted, len(inbox), interrupts.Load())
		}
	})
}

func TestRustOpenCodeRedirectsDoNotForwardOrReplay(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		var forwarded atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwarded.Add(1)
			t.Error("redirect followed, exposing authority")
			w.WriteHeader(200)
		}))
		defer target.Close()
		for _, stage := range []string{"preflight", "post"} {
			t.Run(stage, func(t *testing.T) {
				spec := testSpec()
				var posts atomic.Int32
				server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "POST" {
						posts.Add(1)
						http.Redirect(w, r, target.URL+"/leak", http.StatusTemporaryRedirect)
						return
					}
					if r.URL.Path == "/api/info" {
						if stage == "preflight" {
							http.Redirect(w, r, target.URL+"/leak", http.StatusFound)
						} else {
							writeJSON(w, map[string]any{"version": Version})
						}
						return
					}
					if r.URL.Path == sessionPath(spec) {
						writeData(w, ownedSession(spec))
					} else if strings.HasSuffix(r.URL.Path, "/inbox") {
						writeData(w, []any{})
					} else {
						http.NotFound(w, r)
					}
				})
				err := newRuntime(t, server.URL, nil).Prompt(t.Context(), spec, "command", "text")
				if err == nil || forwarded.Load() != 0 {
					t.Fatal("redirect accepted or followed")
				}
				if stage == "post" {
					requireError(t, err, core.ErrUncertain)
					if posts.Load() != 1 {
						t.Fatal("redirected mutation retried")
					}
				} else if posts.Load() != 0 {
					t.Fatal("failed preflight posted")
				}
			})
		}
	})
}

func TestRustOpenCodePreflightCancellationAndResponseCaps(t *testing.T) {
	eachRuntime(t, func(t *testing.T, newRuntime runtimeFactory) {
		for _, scenario := range []string{"canceled", "deadline", "oversize"} {
			t.Run(scenario, func(t *testing.T) {
				var requests atomic.Int32
				server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if scenario == "oversize" {
						writeJSON(w, map[string]any{"version": Version, "padding": strings.Repeat("x", 8<<20)})
					} else {
						t.Error("expired preflight contacted runtime")
					}
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if scenario == "canceled" {
					cancel()
				}
				if scenario == "deadline" {
					var stop context.CancelFunc
					ctx, stop = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
					defer stop()
				}
				err := newRuntime(t, server.URL, nil).Prompt(ctx, testSpec(), "command", "text")
				var proof *core.NotDelivered
				if err == nil || !errors.As(err, &proof) {
					t.Fatalf("preflight didn't prove non-delivery: %v", err)
				}
				if scenario != "oversize" && requests.Load() != 0 {
					t.Fatal("expired preflight sent a request")
				}
				if scenario == "oversize" && requests.Load() != 1 {
					t.Fatal("read was retried or posted")
				}
			})
		}
	})
}

func TestRustOpenCodeGoEndpointIdentityOracle(t *testing.T) {
	_ = requireRustRuntime(t)
	server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"version": Version}) })
	for _, suffix := range []string{"", "/", "///", "/prefix/", "/a%2fb/", "/space here", "/a%2Fb", "/%FF", "/a/../b", "/%41/%e4%b8%96/", "/!/", "/é/"} {
		t.Run(suffix, func(t *testing.T) {
			url := server.URL + suffix
			goRuntime, err := New(Options{URL: url, Password: "test-password"})
			if err != nil {
				t.Fatal(err)
			}
			want, err := goRuntime.Info(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			rust := &rustRuntime{t: t, url: url}
			got, err := rust.Info(t.Context())
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("Go identity %+v; Rust %+v, err=%v", want, got, err)
			}
		})
	}
}

func TestRustOpenCodeGoAuthorityIdentityOracle(t *testing.T) {
	data, err := os.ReadFile("../../crates/tandem-opencode/tests/endpoint-authorities.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		URL  string
		Base string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	server := authenticatedFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/info" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		writeJSON(w, map[string]any{"version": Version})
	})
	for _, test := range cases {
		t.Run(test.URL, func(t *testing.T) {
			runtime, err := New(Options{URL: test.URL, Password: "test-password"})
			if err != nil {
				t.Fatal(err)
			}
			if runtime.base != test.Base {
				t.Fatalf("Go normalized %q to %q, want %q", test.URL, runtime.base, test.Base)
			}
			runtime.base = server.URL
			info, err := runtime.Info(t.Context())
			if err != nil || info.ID != "opencode_"+digest(test.Base) || !info.Compatible || info.Version != Version {
				t.Fatalf("Go Info: %+v, err=%v", info, err)
			}
		})
	}
}
