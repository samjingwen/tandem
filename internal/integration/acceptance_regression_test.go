package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/bootstrap"
)

func TestDaemonFixtureLeavesPopulatedSourceUntouched(t *testing.T) {
	f := setup(t)
	task, attempt := f.task(t)
	f.do(t, 0, http.MethodPost, "/v1/tasks/"+task.ID+"/followup", "pending", core.Followup{Text: "Existing unfinished work"}, nil)
	f.do(t, 0, http.MethodPost, "/v1/tasks", "queued", core.CreateTask{ProjectID: f.project.ID, Title: "Existing queued task", Instructions: "Must never dispatch"}, nil)
	var unexpected atomic.Int32
	var inspections atomic.Int32
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/info":
			inspections.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"2.1.0"}`))
		case "/api/event":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-req.Context().Done()
		default:
			unexpected.Add(1)
			http.Error(w, "no task/session operations authorized", http.StatusForbidden)
		}
	}))
	defer runtime.Close()
	source := filepath.Join(f.dir, "state")
	connection, err := json.Marshal(bootstrap.Connection{URL: runtime.URL, Password: "fixture-only-grant", Version: "2.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "opencode-connection.json"), connection, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := f.engine.Snapshot(f.ctx)
	if err != nil || before.Attempts[attempt.ID].Phase != "executing" || len(before.Tasks) != 2 {
		t.Fatal("source fixture was not populated", err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = nil
	readFiles := func() map[string]string {
		files := map[string]string{}
		err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			files[path] = string(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	files := readFiles()
	directory := daemonFixtureDirectory(t, source)
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("fixture directory is not owner-only", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "opencode-connection.json" {
		t.Fatal("fixture copied more than connection configuration", err)
	}
	binary := filepath.Join(t.TempDir(), "tandem")
	buildCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// This fixture exercises daemon behavior, not checkout-dependent VCS metadata.
	build := exec.CommandContext(buildCtx, "go", "build", "-buildvcs=false", "-o", binary, "./cmd/tandem")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production daemon: %v: %s", err, output)
	}
	verifyDaemonFixture(t, binary, directory)
	if unexpected.Load() != 0 || inspections.Load() == 0 {
		t.Fatal("fixture did not remain task-free", unexpected.Load(), inspections.Load())
	}
	if !reflect.DeepEqual(files, readFiles()) {
		t.Fatal("daemon fixture altered supplied persistent state or connection")
	}
}

func arithmeticWorkloadFixture(t *testing.T, workers [][]string) core.State {
	t.Helper()
	s := core.State{Tasks: map[string]core.Task{}, Attempts: map[string]core.Attempt{}}
	for index, operations := range workers {
		id := fmt.Sprint(index)
		directory := t.TempDir()
		for _, operation := range operations {
			for _, suffix := range []string{".go", "_test.go"} {
				if err := os.WriteFile(filepath.Join(directory, operation+suffix), []byte("package arithmetic\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		s.Tasks[id] = core.Task{ID: id, ParentID: "parent", Role: "worker", AttemptID: id}
		s.Attempts[id] = core.Attempt{ID: id, TaskID: id, Workspace: core.Workspace{Path: directory}}
	}
	return s
}

func TestArithmeticWorkloadCoverage(t *testing.T) {
	for _, test := range []struct {
		name    string
		workers [][]string
		valid   bool
	}{
		{"sum-product", [][]string{{"sum"}, {"product"}}, true},
		{"product-sum", [][]string{{"product"}, {"sum"}}, true},
		{"duplicate-sum", [][]string{{"sum"}, {"sum"}}, false},
		{"duplicate-product", [][]string{{"product"}, {"product"}}, false},
		{"missing-worker", [][]string{{"sum"}}, false},
		{"missing-artifact", [][]string{{"sum"}, {}}, false},
		{"ambiguous-worker", [][]string{{"sum", "product"}, {"product"}}, false},
		{"extra-worker", [][]string{{"sum"}, {"product"}, {"sum"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := arithmeticWorkloadFixture(t, test.workers)
			assignments, err := arithmeticWorkers(s, "parent")
			if (err == nil) != test.valid {
				t.Fatalf("coverage accepted=%t want %t: %v", err == nil, test.valid, err)
			}
			if test.valid {
				for index, operations := range test.workers {
					if assignments[fmt.Sprint(index)] != operations[0] {
						t.Fatal("worker assigned the wrong independent oracle")
					}
				}
			}
		})
	}
	t.Run("missing-requested-test", func(t *testing.T) {
		s := arithmeticWorkloadFixture(t, [][]string{{"sum"}, {"product"}})
		if err := os.Remove(filepath.Join(s.Attempts["1"].Workspace.Path, "product_test.go")); err != nil {
			t.Fatal(err)
		}
		if _, err := arithmeticWorkers(s, "parent"); err == nil {
			t.Fatal("missing requested test artifact passed")
		}
	})
}

func TestArithmeticBehavioralOracles(t *testing.T) {
	for _, operation := range []string{"sum", "product"} {
		for _, correct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", operation, correct), func(t *testing.T) {
				directory := t.TempDir()
				implementation := "func Sum(a,b int) int { return a+b }\n"
				if operation == "product" {
					implementation = "func Product(a,b int) int { return a*b }\n"
				}
				if !correct {
					if operation == "sum" {
						implementation = "func Sum(a,b int) int { return a*b }\n"
					} else {
						implementation = "func Product(a,b int) int { return a+b }\n"
					}
				}
				for name, text := range map[string]string{"go.mod": "module arithmetic-fixture\n\ngo 1.27.3\n", operation + ".go": "package arithmetic\n" + implementation} {
					if err := os.WriteFile(filepath.Join(directory, name), []byte(text), 0600); err != nil {
						t.Fatal(err)
					}
				}
				output, err := arithmeticOracle(context.Background(), directory, operation)
				if (err == nil) != correct {
					t.Fatalf("arithmetic oracle accepted=%t want %t: %v: %s", err == nil, correct, err, output)
				}
			})
		}
	}
}
