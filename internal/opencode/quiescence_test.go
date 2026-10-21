package opencode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCleanupQuiescenceChecksScopedNativeBackgroundResources(t *testing.T) {
	for _, busy := range []string{"none", "shell", "pty", "persistent", "wrong-location"} {
		t.Run(busy, func(t *testing.T) {
			spec := testSpec()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				username, password, ok := req.BasicAuth()
				if !ok || username != "opencode" || password != "fixture-password" {
					t.Error("invalid background-check authentication")
				}
				var result any
				switch req.URL.Path {
				case "/api/info":
					result = map[string]any{"version": Version, "capabilities": map[string]any{"persistentPty": true}}
				case sessionPath(spec):
					result = map[string]any{"data": map[string]any{"id": spec.Reference, "metadata": owner(spec, ""), "location": location{spec.Directory}, "permissions": sessionPermissions(spec)}}
				case "/api/session/active":
					result = map[string]any{"data": map[string]any{}}
				case sessionPath(spec) + "/permission", sessionPath(spec) + "/form":
					result = map[string]any{"data": []any{}}
				case "/api/shell", "/api/pty":
					if req.URL.Query().Get("location[directory]") != spec.Directory {
						t.Fatal("background query was not explicitly location-scoped")
					}
					resources := []any{}
					if busy == "shell" && req.URL.Path == "/api/shell" || busy == "pty" && req.URL.Path == "/api/pty" {
						resources = append(resources, map[string]any{"status": "running"})
					}
					directory := spec.Directory
					if busy == "wrong-location" {
						directory = "/other-project"
					}
					result = map[string]any{"location": location{directory}, "data": resources}
				case "/api/experimental/session/" + spec.Reference + "/terminal":
					resources := []any{}
					if busy == "persistent" {
						resources = append(resources, map[string]any{"sessionID": spec.Reference, "status": "running"})
					}
					result = map[string]any{"data": resources}
				default:
					t.Fatalf("unexpected endpoint %s", req.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			r, err := New(Options{URL: server.URL, Password: "fixture-password"})
			if err != nil {
				t.Fatal(err)
			}
			err = r.CheckQuiescence(t.Context(), spec)
			if (err == nil) != (busy == "none") {
				t.Fatalf("busy=%s: %v", busy, err)
			}
		})
	}
}
