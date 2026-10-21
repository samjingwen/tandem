package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/workspace"
)

func TestWorktrunkAllocationAndRecordedAuthority(t *testing.T) {
	if _, err := exec.LookPath("wt"); err != nil {
		t.Skip("optional Worktrunk executable not installed; Git allocator is covered by core integration tests")
	}
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "master"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "fixture"}} {
		if _, err := workspace.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	p := core.Project{ID: "fixture", Path: repo, BaseRef: "master"}
	g := workspace.Git{Root: filepath.Join(root, "trees"), UseWorktrunk: true}
	w := g.Plan(p, "run_fixture")
	allocated, err := g.Ensure(ctx, p, w)
	if err != nil {
		t.Fatal(err)
	}
	if !allocated.Allocated || allocated.BaseSHA == "" || allocated.Allocator != "worktrunk" {
		t.Fatal(allocated)
	}
	if err = (workspace.Git{Root: g.Root}).Inspect(ctx, p, allocated); err == nil {
		t.Fatal("changed authority accepted")
	}
	if err = g.Remove(ctx, p, allocated); err != nil {
		t.Fatal(err)
	}
	if _, err = workspace.Run(ctx, repo, "rev-parse", "--verify", allocated.Branch); err != nil {
		t.Fatal("cleanup deleted branch", err)
	}
}

func TestRegistrationLoadsAncestorsAndLeavesDirtyCanonicalUntouched(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "master"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "fixture"}} {
		if _, err := workspace.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{filepath.Join(root, "AGENTS.md"): "ancestor instruction", filepath.Join(repo, "AGENTS.md"): "project instruction", filepath.Join(repo, "user-work.txt"): "preserve"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := workspace.Run(ctx, repo, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := workspace.Register(ctx, core.RegisterProject{Name: "fixture", Path: repo, BaseRef: "master"})
	if err != nil {
		t.Fatal(err)
	}
	// Instructions are an intentionally emitted worker interface, not a source
	// scan used as evidence that the model follows them.
	if p.Instructions == "" {
		t.Fatal("no project instructions loaded")
	}
	after, err := workspace.Run(ctx, repo, "status", "--porcelain=v1")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("registration changed canonical work")
	}
}
