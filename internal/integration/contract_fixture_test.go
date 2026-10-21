package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/api"
	"github.com/samjingwen/tandem/internal/bootstrap"
	"github.com/samjingwen/tandem/internal/store"
	"github.com/samjingwen/tandem/internal/testruntime"
	"github.com/samjingwen/tandem/internal/workspace"
)

// TestServeTenaxeContractFixture serves a real, read-only Go API with SQLite and
// a controlled fake execution runtime for the optional external Spring/browser
// acceptance. It never starts a native runtime or reads existing user state.
func TestServeTenaxeContractFixture(t *testing.T) {
	root := os.Getenv("TANDEM_TENAXE_FIXTURE_DIR")
	if root == "" {
		t.Skip("explicit retained disposable fixture directory required")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("fixture path must be absolute")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal("fixture path must be fresh", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	repo := filepath.Join(root, "project")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "master"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "contract fixture"}} {
		if _, err := workspace.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "state")
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	runtime := testruntime.New()
	engine := core.New(db, runtime, workspace.Git{Root: filepath.Join(root, "worktrees")}, core.Options{RuntimeID: "controlled-fake", RegisterProject: workspace.Register})
	p, err := engine.Register(ctx, "fixture-project", core.RegisterProject{Name: "Disposable Go API contract fixture", Path: repo, BaseRef: "master"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := engine.Create(ctx, "fixture-task", core.CreateTask{ProjectID: p.ID, Title: "Retained actual-Go contract fixture", Instructions: "Controlled fixture data; no model work is requested."})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := engine.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attempt := snapshot.Attempts[snapshot.Tasks[task.ID].AttemptID]
	for generation := 1; generation <= 2; generation++ {
		runtime.SetState(attempt.SessionRef, "idle")
		if err = engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = engine.SubmitResult(ctx, core.ID("fixture-result_"), task.ID, core.SubmitResult{AttemptID: attempt.ID, Generation: generation, Summary: "Explicit controlled fixture evidence, not a live-model claim", Evidence: []core.Evidence{{Name: "fixture record", Reference: "fixture://contract-data", Passed: true}}}); err != nil {
			t.Fatal(err)
		}
		if generation == 1 {
			if _, err = engine.Followup(ctx, "fixture-followup", task.ID, core.Followup{Text: "Create a second generation for retained-result projection"}); err != nil {
				t.Fatal(err)
			}
			if err = engine.Tick(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = engine.Accept(ctx, "fixture-accept", task.ID); err != nil {
		t.Fatal(err)
	}
	token, err := bootstrap.ClientToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var once sync.Once
	handler := api.New(engine, api.Options{Token: token})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__fixture/stop" && r.Method == http.MethodPost && r.Header.Get("Authorization") == "Bearer "+token {
			w.WriteHeader(http.StatusNoContent)
			once.Do(func() { close(stop) })
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "read-only controlled fixture", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	handoff := struct {
		URL       string `json:"url"`
		ProjectID string `json:"projectId"`
		TaskID    string `json:"taskId"`
		TokenFile string `json:"tokenFile"`
		Runtime   string `json:"runtime"`
	}{server.URL, p.ID, task.ID, filepath.Join(dir, "client.token"), "controlled-fake"}
	data, err := json.Marshal(handoff)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "handoff.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual Go API fixture ready at %s; metadata only at %s", server.URL, filepath.Join(root, "handoff.json"))
	select {
	case <-stop:
	case <-ctx.Done():
		t.Fatal("external contract fixture timed out without owned teardown")
	}
}
