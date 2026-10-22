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

func TestGitSubprocessDoesNotReceiveControlCredentials(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "git")
	script := "#!/bin/sh\nif [ -n \"${TANDEM_TOKEN:-}\" ] || [ -n \"${OPENCODE_PASSWORD:-}\" ]; then exit 38; fi\nprintf 'safe'\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TANDEM_TOKEN", "fixture-control-canary")
	t.Setenv("OPENCODE_PASSWORD", "fixture-runtime-canary")
	output, err := workspace.Run(context.Background(), root, "status")
	if err != nil {
		t.Fatal(err)
	}
	if output != "safe" {
		t.Fatal("subprocess environment wasn't isolated")
	}
}

func TestAllocationDoesNotRunProjectCheckoutHooks(t *testing.T) {
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
	marker := filepath.Join(root, "hook-was-run")
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\nprintf 'unsafe' > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	p := core.Project{ID: "fixture", Path: repo, BaseRef: "master"}
	g := workspace.Git{Root: filepath.Join(root, "trees")}
	if _, err := g.Ensure(ctx, p, g.Plan(p, "hook_test")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("allocation executed project-controlled hook")
	}
}

func TestCleanupDoesNotRemoveWorkspaceWithLiveProcess(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "master"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture"}} {
		if _, err := workspace.Run(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	p := core.Project{ID: "fixture", Path: repo, BaseRef: "master"}
	g := workspace.Git{Root: filepath.Join(root, "trees")}
	w, err := g.Ensure(ctx, p, g.Plan(p, "live_process"))
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command("sleep", "30")
	process.Dir = w.Path
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait() }()
	if err = g.Remove(ctx, p, w); err == nil {
		t.Fatal("cleanup removed a process-owned working directory")
	}
	if _, err = os.Stat(w.Path); err != nil {
		t.Fatal("workspace lost despite refusal", err)
	}
}
