package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samjingwen/tandem/core"
)

func testSpec() core.SessionSpec {
	return core.SessionSpec{Reference: "ses_test", TaskID: "task", AttemptID: "attempt", InstanceID: "instance", Directory: "/work/task", Title: "Task", Role: "worker"}
}

func testRuntime(t *testing.T, handler http.HandlerFunc) *Runtime {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		username, password, ok := req.BasicAuth()
		if !ok || username != "opencode" || password != "test-password" {
			t.Errorf("invalid basic auth")
		}
		if req.URL.Path == "/api/info" {
			writeJSON(w, map[string]any{"version": Version})
			return
		}
		handler(w, req)
	}))
	t.Cleanup(server.Close)
	r, err := New(Options{URL: server.URL, Password: "test-password"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func writeData(w http.ResponseWriter, value any) { writeJSON(w, map[string]any{"data": value}) }
func ownedSession(spec core.SessionSpec) session {
	return session{ID: spec.Reference, Metadata: owner(spec, ""), Location: location{spec.Directory}, Permissions: sessionPermissions(spec)}
}
func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestInfoVersionIdentityAndConfiguration(t *testing.T) {
	for _, version := range []string{Version, "2.1.2", "1.2.0", ""} {
		t.Run(version, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { writeJSON(w, map[string]any{"version": version}) }))
			defer server.Close()
			r, _ := New(Options{URL: server.URL + "/", Password: "secret"})
			other, _ := New(Options{URL: server.URL, Password: "rotated"})
			info, err := r.Info(t.Context())
			if err != nil || info.Compatible != (version == Version) || info.ID != other.id || strings.Contains(info.ID, "secret") {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			if version != Version {
				requireError(t, r.Ensure(t.Context(), testSpec()), ErrVersion)
			}
		})
	}
	for _, raw := range []string{"", "ftp://localhost", "http://user:password@localhost", "http://localhost?token=x", "http://localhost#secret"} {
		if _, err := New(Options{URL: raw}); err == nil {
			t.Errorf("accepted invalid URL %q", raw)
		}
	}
	t.Setenv("OPENCODE_PASSWORD", "provided")
	r, err := New(Options{URL: "http://localhost:4000"})
	if err != nil || r.password != "provided" {
		t.Fatal("explicit environment handoff not used")
	}
	if r.Reference("attempt") != r.Reference("attempt") || !sessionID.MatchString(r.Reference("attempt")) || r.Reference("attempt") == r.Reference("other") {
		t.Fatal("invalid deterministic session reference")
	}
}

func TestEnsureExactWireAndRecovery(t *testing.T) {
	spec := testSpec()
	var mu sync.Mutex
	var saved *session
	posts := 0
	lost := true
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case req.Method == "GET" && req.URL.Path == sessionPath(spec):
			if saved == nil {
				w.WriteHeader(404)
				return
			}
			writeData(w, saved)
		case req.Method == "POST" && req.URL.Path == "/api/session":
			posts++
			var body map[string]json.RawMessage
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body) != 7 || body["parentID"] != nil {
				t.Errorf("wrong create wire: %v", body)
			}
			var id, agent string
			var loc location
			var md metadata
			var model Model
			var permissions []permissionRule
			_ = json.Unmarshal(body["id"], &id)
			_ = json.Unmarshal(body["agent"], &agent)
			_ = json.Unmarshal(body["location"], &loc)
			_ = json.Unmarshal(body["metadata"], &md)
			_ = json.Unmarshal(body["model"], &model)
			_ = json.Unmarshal(body["permissions"], &permissions)
			if id != spec.Reference || agent != "build" || loc.Directory != spec.Directory || md != owner(spec, "") || model.ID != "model" || model.ProviderID != "provider" {
				t.Error("wrong create payload")
			}
			if !slices.Equal(permissions, sessionPermissions(spec)) {
				t.Error("session creation omitted the pinned permission policy")
			}
			s := ownedSession(spec)
			saved = &s
			if lost {
				lost = false
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
				return
			}
			writeData(w, saved)
		default:
			t.Errorf("unexpected %s %s", req.Method, req.URL)
			w.WriteHeader(500)
		}
	})
	r.agent = "build"
	r.model = &Model{ID: "model", ProviderID: "provider"}
	requireError(t, r.Ensure(t.Context(), spec), core.ErrUncertain)
	r, err := New(Options{URL: r.base, Password: r.password})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Ensure(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("speculatively retried create: %d", posts)
	}
}

func TestUnsafeOwnedSessionPolicyFailsClosedWithoutRewriting(t *testing.T) {
	spec := testSpec()
	writes := 0
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			writes++
			w.WriteHeader(500)
			return
		}
		s := ownedSession(spec)
		s.Permissions = nil
		writeData(w, s)
	})
	requireError(t, r.Ensure(t.Context(), spec), ErrOwnership)
	got, err := r.Inspect(t.Context(), spec)
	if err != nil || !got.Exists || got.Owned || writes != 0 {
		t.Fatalf("unsafe policy adopted or rewritten: %+v writes=%d err=%v", got, writes, err)
	}
}

func TestReplyRejectionClassificationAndRacingMissingDecisions(t *testing.T) {
	spec := testSpec()
	d := core.Decision{TaskID: spec.TaskID, AttemptID: spec.AttemptID, Kind: "form", RuntimeRef: "form_1"}
	for _, code := range []int{400, 401, 403, 404, 408, 409, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == sessionPath(spec) {
					writeData(w, ownedSession(spec))
					return
				}
				if req.Method == "POST" {
					w.WriteHeader(code)
					return
				}
				if strings.HasSuffix(req.URL.Path, "/form") {
					writeData(w, []any{map[string]string{"id": "form_1", "sessionID": spec.Reference}})
					return
				}
				writeData(w, []any{})
			})
			err := r.Reply(t.Context(), spec, d, json.RawMessage(`{"answer":{"field":"invalid for this native field type"}}`))
			if code == 400 || code == 401 || code == 403 {
				if !KnownRejected(err) {
					t.Fatalf("deterministic rejection not classified: %v", err)
				}
				if code == 400 {
					var invalid *core.Error
					if !errors.As(err, &invalid) || invalid.Code != "invalid_request" {
						t.Fatalf("native 400 not mapped to core.Invalid: %v", err)
					}
				}
			} else {
				requireError(t, err, core.ErrUncertain)
				if KnownRejected(err) {
					t.Fatal("uncertainty classified as rejection")
				}
			}
		})
	}
	if KnownRejected(errors.Join(core.Invalid("local"), core.ErrUncertain)) {
		t.Fatal("wrapped uncertainty classified as deterministic")
	}
}

func TestReplyCannotTargetReusedIDWithChangedRequestDetails(t *testing.T) {
	spec := testSpec()
	posts := 0
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			posts++
			w.WriteHeader(204)
			return
		}
		if req.URL.Path == sessionPath(spec) {
			writeData(w, ownedSession(spec))
			return
		}
		if strings.HasSuffix(req.URL.Path, "/permission") {
			writeData(w, []any{map[string]any{"id": "per_same", "sessionID": spec.Reference, "action": "shell", "resources": []string{"different command"}}})
			return
		}
		writeData(w, []any{})
	})
	d := core.Decision{TaskID: spec.TaskID, AttemptID: spec.AttemptID, Kind: "permission", RuntimeRef: "per_same", Details: json.RawMessage(`{"id":"per_same","sessionID":"ses_test","action":"shell","resources":["original command"]}`)}
	requireError(t, r.Reply(t.Context(), spec, d, json.RawMessage(`{"decision":"once"}`)), core.ErrUncertain)
	if posts != 0 {
		t.Fatal("changed native request received a stale answer")
	}
	if !sameRequest(json.RawMessage(`{"a":1,"b":true}`), json.RawMessage(`{"b":true,"a":1}`)) {
		t.Fatal("JSON key ordering must not change request identity")
	}
}

func TestEnsureOnly404CreatesAndAlwaysChecksCollision(t *testing.T) {
	spec := testSpec()
	for _, code := range []int{401, 403, 500, 200, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			posts := 0
			r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				s := ownedSession(spec)
				s.Location.Directory = "/someone-else"
				if req.Method == "POST" {
					posts++
					writeData(w, s)
					return
				}
				if code == 200 {
					writeData(w, s)
					return
				}
				w.WriteHeader(code)
			})
			err := r.Ensure(t.Context(), spec)
			if code == 200 || code == 404 {
				requireError(t, err, ErrOwnership)
			} else if err == nil {
				t.Fatal("failed GET accepted")
			}
			if (code == 404 && posts != 1) || (code != 404 && posts != 0) {
				t.Fatal("create attempted without authoritative absence")
			}
		})
	}
	for _, mutate := range []func(*session){func(s *session) { s.Metadata.Tandem.InstanceID = "other" }, func(s *session) { s.Metadata.Tandem.AttemptID = "other" }, func(s *session) { s.Metadata.Tandem.TaskID = "other" }, func(s *session) { s.ID = "ses_other" }} {
		s := ownedSession(spec)
		mutate(&s)
		r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) { writeData(w, s) })
		requireError(t, r.Ensure(t.Context(), spec), ErrOwnership)
	}
}

func TestPromptLostResponseRecoveryNeverWakesKnownAdmission(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprint(delivered), func(t *testing.T) {
			spec := testSpec()
			var admitted *admission
			posts := 0
			r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				switch {
				case req.URL.Path == sessionPath(spec):
					writeData(w, ownedSession(spec))
				case strings.Contains(req.URL.Path, "/message/"):
					if admitted != nil && delivered {
						writeData(w, message{ID: admitted.ID, Type: "user", Text: admitted.Payload.Text, Metadata: admitted.Payload.Metadata})
						return
					}
					w.WriteHeader(404)
				case strings.HasSuffix(req.URL.Path, "/inbox"):
					items := []admission{}
					if admitted != nil && !delivered {
						items = append(items, *admitted)
					}
					writeData(w, items)
				case strings.HasSuffix(req.URL.Path, "/prompt") && req.Method == "POST":
					posts++
					var body struct {
						ID       string   `json:"id"`
						Text     string   `json:"text"`
						Metadata metadata `json:"metadata"`
						Delivery string   `json:"delivery"`
						Resume   bool     `json:"resume"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.ID != "msg_"+digest("tandem/prompt/command") || body.Delivery != "steer" || !body.Resume || body.Metadata != owner(spec, "command") {
						t.Error("wrong prompt wire")
					}
					admitted = &admission{ID: body.ID, SessionID: spec.Reference, Type: "user", Payload: promptPayload{body.Text, body.Metadata}}
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
				default:
					t.Errorf("unexpected %s %s", req.Method, req.URL)
					w.WriteHeader(500)
				}
			})
			requireError(t, r.Prompt(t.Context(), spec, "command", "hello"), core.ErrUncertain)
			r, err := New(Options{URL: r.base, Password: r.password})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.ReconcilePrompt(t.Context(), spec, "command", "hello"); err != nil {
				t.Fatal(err)
			}
			if err := r.Prompt(t.Context(), spec, "command", "hello"); err != nil {
				t.Fatal(err)
			}
			requireError(t, r.Prompt(t.Context(), spec, "command", "different"), ErrOwnership)
			if posts != 1 {
				t.Fatalf("known command re-posted, posts=%d", posts)
			}
		})
	}
}

func TestReconcileUncertainPreparationIsReadOnly(t *testing.T) {
	spec := testSpec()
	writes := 0
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			writes++
			w.WriteHeader(500)
			return
		}
		if req.URL.Path == sessionPath(spec) {
			writeData(w, ownedSession(spec))
			return
		}
		if strings.HasSuffix(req.URL.Path, "/inbox") {
			writeData(w, []admission{})
			return
		}
		w.WriteHeader(404)
	})
	requireError(t, r.ReconcilePrompt(t.Context(), spec, "command", "hello"), core.ErrUncertain)
	if writes != 0 {
		t.Fatal("reconciliation attempted a wake-capable write")
	}
}

func TestRedirectsCannotLeakCredentialsOrReplayWrites(t *testing.T) {
	spec := testSpec()
	redirected := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { redirected++; w.WriteHeader(500) }))
	defer other.Close()
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	requireError(t, r.Ensure(t.Context(), spec), core.ErrUncertain)
	if redirected != 0 {
		t.Fatal("redirect forwarded an authenticated mutation")
	}
}

func TestPromptAdmissionCollisionAndDeliveryReadRace(t *testing.T) {
	spec := testSpec()
	reads := 0
	posts := 0
	id := "msg_" + digest("tandem/prompt/command")
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == sessionPath(spec):
			writeData(w, ownedSession(spec))
		case strings.Contains(req.URL.Path, "/message/"):
			reads++
			if reads == 1 {
				w.WriteHeader(404)
				return
			}
			writeData(w, message{ID: id, Type: "user", Text: "hello", Metadata: owner(spec, "command")})
		case strings.HasSuffix(req.URL.Path, "/inbox"):
			writeData(w, []admission{})
		default:
			posts++
			w.WriteHeader(500)
		}
	})
	if err := r.Prompt(t.Context(), spec, "command", "hello"); err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("reposted admission delivered during reads")
	}
	for _, foreign := range []bool{false, true} {
		r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.URL.Path == sessionPath(spec):
				writeData(w, ownedSession(spec))
			case strings.Contains(req.URL.Path, "/message/"):
				w.WriteHeader(404)
			case strings.HasSuffix(req.URL.Path, "/inbox"):
				writeData(w, []admission{})
			default:
				md := owner(spec, "command")
				text := "changed"
				if foreign {
					text = "hello"
					md.Tandem.InstanceID = "foreign"
				}
				writeData(w, admission{ID: id, SessionID: spec.Reference, Type: "user", Payload: promptPayload{text, md}})
			}
		})
		requireError(t, r.Prompt(t.Context(), spec, "command", "hello"), ErrOwnership)
	}
}

func TestInspectActiveOutcomeAndPendingDecisions(t *testing.T) {
	spec := testSpec()
	for _, running := range []bool{false, true} {
		r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case sessionPath(spec):
				s := ownedSession(spec)
				s.Outcome = "failed"
				writeData(w, s)
			case "/api/session/active":
				active := map[string]any{}
				if running {
					active[spec.Reference] = map[string]string{"type": "running"}
				}
				writeData(w, active)
			case sessionPath(spec) + "/permission":
				writeData(w, []any{map[string]any{"id": "per_1", "sessionID": spec.Reference, "action": "shell", "resources": []string{"git status"}}})
			case sessionPath(spec) + "/form":
				writeData(w, []any{map[string]any{"id": "form_1", "sessionID": spec.Reference, "title": "Choose", "fields": []any{}}})
			default:
				t.Errorf("unexpected %s", req.URL)
				w.WriteHeader(500)
			}
		})
		got, err := r.Inspect(t.Context(), spec)
		want := "failed"
		if running {
			want = "running"
		}
		if err != nil || got.State != want || !got.Exists || !got.Owned || len(got.Decisions) != 2 {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	}
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == sessionPath(spec) {
			s := ownedSession(spec)
			s.Outcome = "succeeded"
			writeData(w, s)
			return
		}
		if req.URL.Path == "/api/session/active" {
			writeData(w, map[string]any{})
			return
		}
		writeData(w, []any{})
	})
	got, err := r.Inspect(t.Context(), spec)
	if err != nil || got.State != "idle" {
		t.Fatalf("idle must not become completed: %+v %v", got, err)
	}
}

func TestInterruptParksAndExplicitCancellationOnlyRemovesOwned(t *testing.T) {
	spec := testSpec()
	deleted := []string{}
	interrupts := 0
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == sessionPath(spec):
			writeData(w, ownedSession(spec))
		case strings.HasSuffix(req.URL.Path, "/interrupt"):
			interrupts++
			if req.Method != "POST" || req.URL.RawQuery != "resume=false" {
				t.Error("interrupt must park")
			}
			writeJSON(w, map[string]bool{"interrupted": true})
		case req.Method == "DELETE":
			deleted = append(deleted, req.URL.Path)
			w.WriteHeader(204)
		case strings.HasSuffix(req.URL.Path, "/inbox"):
			mine := admission{ID: "msg_" + digest("tandem/prompt/c"), SessionID: spec.Reference, Type: "user", Payload: promptPayload{"hello", owner(spec, "c")}}
			foreign := mine
			foreign.ID = "msg_foreign"
			foreign.Payload.Metadata.Tandem.InstanceID = "foreign"
			native := mine
			native.Type = "compaction"
			writeData(w, []admission{mine, foreign, native})
		default:
			t.Errorf("unexpected %s", req.URL)
			w.WriteHeader(500)
		}
	})
	if err := r.Interrupt(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if interrupts != 1 || len(deleted) != 0 {
		t.Fatal("pause deleted inbox")
	}
	if err := r.CancelPending(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "msg_"+digest("tandem/prompt/c")) {
		t.Fatalf("wrong cancellations %v", deleted)
	}
}

func TestReplyNativeWireAndMissingDecisionUncertainty(t *testing.T) {
	spec := testSpec()
	posts := 0
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == sessionPath(spec) {
			writeData(w, ownedSession(spec))
			return
		}
		if req.Method == "POST" {
			posts++
			body, _ := io.ReadAll(req.Body)
			want := `{"decision":"once","message":"approved"}`
			if strings.Contains(req.URL.Path, "/form/") {
				want = `{"answer":{"choice":["one"],"count":2,"enabled":true}}`
			}
			if string(body) != want || !strings.HasSuffix(req.URL.Path, "/reply") {
				t.Errorf("wrong reply %s %s", req.URL, body)
			}
			w.WriteHeader(204)
			return
		}
		id := "per_1"
		if strings.HasSuffix(req.URL.Path, "/form") {
			id = "form_1"
		}
		writeData(w, []any{map[string]string{"id": id, "sessionID": spec.Reference}})
	})
	for _, kind := range []string{"permission", "form"} {
		id := "per_1"
		answer := `{"decision":"once","message":"approved"}`
		if kind == "form" {
			id = "form_1"
			answer = `{"answer":{"choice":["one"],"count":2,"enabled":true}}`
		}
		d := core.Decision{TaskID: spec.TaskID, AttemptID: spec.AttemptID, Kind: kind, RuntimeRef: id}
		if err := r.Reply(t.Context(), spec, d, json.RawMessage(answer)); err != nil {
			t.Fatal(err)
		}
		d.RuntimeRef = "missing"
		requireError(t, r.Reply(t.Context(), spec, d, json.RawMessage(answer)), core.ErrUncertain)
		d.TaskID = "other"
		requireError(t, r.Reply(t.Context(), spec, d, json.RawMessage(answer)), ErrOwnership)
	}
	if posts != 2 {
		t.Fatal("replied to missing/foreign decision")
	}
	for _, raw := range []string{`{"reply":"once"}`, `{"decision":"allow"}`, `{"decision":"once","extra":true}`} {
		if validateAnswer("permission", json.RawMessage(raw)) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if validateAnswer("form", json.RawMessage(`{"answer":["one"]}`)) == nil {
		t.Fatal("accepted untyped form answer")
	}
	for _, raw := range []string{`{"answer":{"key":null}}`, `{"answer":{"key":{}}}`, `{"answer":{"key":[2]}}`} {
		if validateAnswer("form", json.RawMessage(raw)) == nil {
			t.Errorf("accepted invalid typed form value: %s", raw)
		}
	}
}

func TestSSEFramesBoundsCancellationAndNoReplay(t *testing.T) {
	var wakes atomic.Int32
	frames := ": heartbeat\r\n\r\nid: ignored\nevent: ignored\ndata: {\"type\":\n" + "data: \"session.updated\",\"location\":{\"directory\":\"/foreign\"},\"data\":{}}\n\ndata: {\"type\":\"server.connected\"}\n\n"
	requireError(t, consumeEvents(strings.NewReader(frames), func() { wakes.Add(1) }), io.EOF)
	if wakes.Load() != 2 {
		t.Fatal("bad SSE frame dispatch")
	}
	for _, frame := range []string{"data: \"{\\\"type\\\":\\\"session.updated\\\"}\"\n\n", "data: {bad}\n\n", "data: " + strings.Repeat("x", 1<<20) + "\n\n", strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", 1025)} {
		if err := consumeEvents(strings.NewReader(frame), func() { t.Error("invalid frame dispatched") }); err == nil || errors.Is(err, io.EOF) {
			t.Fatal("invalid frame accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/event" || req.Header.Get("Last-Event-ID") != "" {
			t.Error("unexpected stream request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\"}\n\n")
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	})
	err := r.Events(ctx, func() { cancel() })
	if r.client.Timeout != 0 {
		t.Fatal("event client has an overall timeout")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stream did not honor cancellation: %v", err)
	}
}

func TestReadFailureNeverMutatesAndWriteMalformedIsUncertain(t *testing.T) {
	spec := testSpec()
	posts := 0
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			posts++
		}
		_, _ = io.WriteString(w, `{"data":`)
	})
	if err := r.Ensure(t.Context(), spec); err == nil || posts != 0 {
		t.Fatal("malformed GET permitted create")
	}
	r = testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `{"data":`)
	})
	requireError(t, r.Ensure(t.Context(), spec), core.ErrUncertain)
}
