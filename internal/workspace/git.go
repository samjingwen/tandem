// Package workspace owns worktree allocation. OpenCode's separate worktree
// allocator is deliberately not used.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/samjingwen/tandem/core"
)

type Git struct {
	Root         string
	UseWorktrunk bool
}

func Run(ctx context.Context, path string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = path
	b, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

func Register(ctx context.Context, r core.RegisterProject) (core.Project, error) {
	path, err := filepath.Abs(r.Path)
	if err != nil {
		return core.Project{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return core.Project{}, err
	}
	top, err := Run(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return core.Project{}, err
	}
	if top != path {
		return core.Project{}, core.Invalid("register a repository root, not a subdirectory")
	}
	if r.BaseRef == "" {
		return core.Project{}, core.Invalid("baseRef is required; select the intended branch explicitly")
	}
	if _, err = Run(ctx, path, "rev-parse", "--verify", r.BaseRef+"^{commit}"); err != nil {
		return core.Project{}, err
	}
	instructions := r.Instructions
	ancestors := []string{}
	for current := path; ; current = filepath.Dir(current) {
		ancestors = append(ancestors, current)
		if current == filepath.Dir(current) {
			break
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		file := filepath.Join(ancestors[i], "AGENTS.md")
		if b, e := os.ReadFile(file); e == nil {
			instructions += "\n\nInstructions from " + file + ":\n" + string(b)
		} else if !errors.Is(e, os.ErrNotExist) {
			return core.Project{}, e
		}
		if len(instructions) > 256*1024 {
			return core.Project{}, core.Invalid("ancestor/project instructions exceed instruction budget; configure a bounded explicit project policy")
		}
	}
	return core.Project{Name: r.Name, Path: path, BaseRef: r.BaseRef, Instructions: instructions}, nil
}

func (g Git) Plan(p core.Project, owner string) core.Workspace {
	allocator := "git"
	if g.UseWorktrunk {
		allocator = "worktrunk"
	}
	return core.Workspace{Allocator: allocator, Owner: owner, Path: filepath.Join(g.Root, p.ID, owner), Branch: "tandem/" + owner, BaseRef: p.BaseRef}
}

func (g Git) Ensure(ctx context.Context, p core.Project, w core.Workspace) (core.Workspace, error) {
	if _, err := os.Stat(w.Path); err == nil {
		if err = g.Inspect(ctx, p, w); err != nil {
			return w, err
		}
		w.Allocated = true
		sha, e := Run(ctx, w.Path, "rev-parse", "HEAD")
		if e != nil {
			return w, e
		}
		w.BaseSHA = sha
		return w, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return w, err
	}
	sha, err := Run(ctx, p.Path, "rev-parse", "--verify", w.BaseRef+"^{commit}")
	if err != nil {
		return w, err
	}
	if err = os.MkdirAll(filepath.Dir(w.Path), 0700); err != nil {
		return w, err
	}
	if g.UseWorktrunk {
		// Worktrunk is the single allocator in environments which require it.
		// Hooks are intentionally disabled: registration does not authorize arbitrary
		// canonical-checkout startup scripts or copying ignored/private files.
		c := exec.CommandContext(ctx, "wt", "--config-set", fmt.Sprintf("worktree-path = %q", w.Path), "switch", "--create", w.Branch, "--base", sha, "--no-hooks", "--no-cd", "--format", "json")
		c.Dir = p.Path
		if b, e := c.CombinedOutput(); e != nil {
			return w, fmt.Errorf("worktrunk allocation: %w: %s", e, b)
		}
	} else {
		if _, err = Run(ctx, p.Path, "worktree", "add", "-b", w.Branch, w.Path, sha); err != nil {
			return w, err
		}
	}
	w.BaseSHA = sha
	w.Allocated = true
	return w, g.Inspect(ctx, p, w)
}

func (g Git) Inspect(ctx context.Context, p core.Project, w core.Workspace) error {
	expectedAllocator := "git"
	if g.UseWorktrunk {
		expectedAllocator = "worktrunk"
	}
	if w.Allocator != expectedAllocator {
		return errors.New("workspace allocator changed; restore its recorded allocation authority")
	}
	if w.Owner == "" || w.Branch != "tandem/"+w.Owner || filepath.Clean(w.Path) != filepath.Join(g.Root, p.ID, w.Owner) {
		return errors.New("workspace ownership mismatch")
	}
	if st, err := os.Lstat(w.Path); err != nil {
		return err
	} else if st.Mode()&os.ModeSymlink != 0 {
		return errors.New("workspace path is a symlink")
	}
	branch, err := Run(ctx, w.Path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if branch != w.Branch {
		return errors.New("workspace branch does not match assignment")
	}
	common, err := Run(ctx, w.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	expected, err := Run(ctx, p.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if common != expected {
		return errors.New("workspace belongs to a different repository")
	}
	return nil
}

func (g Git) Remove(ctx context.Context, p core.Project, w core.Workspace) error {
	if err := g.Inspect(ctx, p, w); err != nil {
		return err
	}
	dirty, err := Run(ctx, w.Path, "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
	if err != nil {
		return err
	}
	if dirty != "" {
		return errors.New("refusing cleanup of dirty workspace, including untracked or ignored files")
	}
	// Never delete the branch: retained commits remain inspectable after cleanup.
	_, err = Run(ctx, p.Path, "worktree", "remove", w.Path)
	return err
}
