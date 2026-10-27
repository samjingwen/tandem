package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/samjingwen/tandem/core"
	"github.com/samjingwen/tandem/internal/workspace"
)

// The executable Go adapter is the independent oracle. Both implementations run
// actual Git and pinned Worktrunk in disposable repositories, never native agents.
type rustWorkspace struct {
	t                *testing.T
	root             string
	worktrunk        bool
	extraEnvironment []string // Explicit synthetic credentials only, never ambient secrets.
}

func requireRustWorkspace(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("TANDEM_RUST_WORKSPACE_HELPER")
	if binary == "" {
		t.Skip("make rust-workspace-parity supplies the actual Rust workspace consumer")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("workspace consumer path must be absolute")
	}
	return binary
}

func (r rustWorkspace) call(method string, p core.Project, w core.Workspace, extra map[string]any, out any) error {
	r.t.Helper()
	input := map[string]any{"root": r.root, "useWorktrunk": r.worktrunk, "method": method, "project": p, "workspace": w}
	for k, v := range extra {
		input[k] = v
	}
	body, err := json.Marshal(input)
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(r.t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, requireRustWorkspace(r.t))
	cmd.Stdin = bytes.NewReader(body)
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TMPDIR", "XDG_CONFIG_HOME", "SYSTEMROOT", "WINDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, r.extraEnvironment...)
	data, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("Rust workspace consumer: %v: %s", err, data)
	}
	var response struct {
		Value json.RawMessage
		Error *struct{ Code, Message string }
	}
	if err = json.Unmarshal(data, &response); err != nil {
		r.t.Fatalf("invalid Rust response: %v: %s", err, data)
	}
	if response.Error != nil {
		if response.Error.Code == "workspace_missing" {
			return core.ErrWorkspaceMissing
		}
		return errors.New(response.Error.Code + ": " + response.Error.Message)
	}
	if out != nil {
		if err = json.Unmarshal(response.Value, out); err != nil {
			r.t.Fatal(err)
		}
	}
	return nil
}

func (r rustWorkspace) Plan(p core.Project, owner string) (w core.Workspace) {
	if err := r.call("plan", p, core.Workspace{}, map[string]any{"owner": owner}, &w); err != nil {
		r.t.Fatal(err)
	}
	return
}
func (r rustWorkspace) Ensure(_ context.Context, p core.Project, w core.Workspace) (out core.Workspace, err error) {
	err = r.call("ensure", p, w, nil, &out)
	return
}
func (r rustWorkspace) Inspect(_ context.Context, p core.Project, w core.Workspace) error {
	return r.call("inspect", p, w, nil, nil)
}
func (r rustWorkspace) Remove(_ context.Context, p core.Project, w core.Workspace) error {
	return r.call("remove", p, w, nil, nil)
}

func workspaceFixture(t *testing.T) (string, core.Project) {
	t.Helper()
	requireRustWorkspace(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"home", "xdg", "repo"} {
		if err = os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	p := core.Project{ID: "prj_fixture", Name: "Fixture", Path: filepath.Join(root, "repo"), BaseRef: "master"}
	for _, args := range [][]string{{"init", "-b", "master"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "fixture"}} {
		gitFixture(t, p.Path, args...)
	}
	return root, p
}

func gitFixture(t *testing.T, path string, args ...string) string {
	t.Helper()
	out, err := workspace.Run(t.Context(), path, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func writeFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRustWorkspaceRegisterAndPurePlanOracle(t *testing.T) {
	root, p := workspaceFixture(t)
	writeFixture(t, filepath.Join(root, "AGENTS.md"), "ancestor\n")
	writeFixture(t, filepath.Join(p.Path, "AGENTS.md"), "project\n")
	writeFixture(t, filepath.Join(p.Path, "policy.MD"), "explicit <>&\n")
	// Dirty tracked, ignored and untracked files must stay byte-identical.
	writeFixture(t, filepath.Join(p.Path, ".gitignore"), "ignored\n")
	writeFixture(t, filepath.Join(p.Path, "tracked"), "original")
	gitFixture(t, p.Path, "add", ".")
	gitFixture(t, p.Path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "policy")
	writeFixture(t, filepath.Join(p.Path, "tracked"), "dirty")
	writeFixture(t, filepath.Join(p.Path, "ignored"), "keep ignored")
	before := gitFixture(t, p.Path, "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
	input := core.RegisterProject{Name: p.Name, Path: p.Path, BaseRef: p.BaseRef, Instructions: "inline", InstructionFiles: []string{"./policy.MD", "policy.MD"}}
	want, err := workspace.Register(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	r := rustWorkspace{t: t, root: filepath.Join(root, "trees")}
	var got core.Project
	if err = r.call("register", p, core.Workspace{}, map[string]any{"input": input}, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("registration differs\nGo: %#v\nRust: %#v", want, got)
	}
	if !strings.Contains(got.Instructions, "ancestor\n\n\nInstructions from "+filepath.Join(p.Path, "AGENTS.md")) || strings.Count(got.Instructions, "explicit <>&") != 2 {
		t.Fatalf("policy order/duplicates not preserved: %q", got.Instructions)
	}
	for _, useWT := range []bool{false, true} {
		r.worktrunk = useWT
		r.extraEnvironment = []string{"PATH=" + filepath.Join(root, "no-executables")}
		g := workspace.Git{Root: r.root, UseWorktrunk: useWT}
		if got, want := r.Plan(p, "run_fixture"), g.Plan(p, "run_fixture"); got != want {
			t.Fatalf("plan differs: %#v != %#v", got, want)
		}
	}
	r.extraEnvironment = nil
	if _, err = os.Lstat(r.root); !os.IsNotExist(err) {
		t.Fatalf("planning allocated a directory: %v", err)
	}
	if after := gitFixture(t, p.Path, "status", "--porcelain=v1", "--untracked-files=all", "--ignored"); after != before {
		t.Fatalf("registration changed checkout: %s -> %s", before, after)
	}
	if content, err := os.ReadFile(filepath.Join(p.Path, "ignored")); err != nil || string(content) != "keep ignored" {
		t.Fatal("registration changed ignored data")
	}
	// Malformed bytes are compared via actual Go JSON encoding, not Rust's decoder.
	writeFixture(t, filepath.Join(p.Path, "broken.md"), string([]byte{0xe2, 0x82, 0xff, 'x'}))
	input.InstructionFiles = []string{"broken.md"}
	want, err = workspace.Register(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(want)
	if err = json.Unmarshal(encoded, &want); err != nil {
		t.Fatal(err)
	}
	if err = r.call("register", p, core.Workspace{}, map[string]any{"input": input}, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("legacy policy text differs: %q != %q", want.Instructions, got.Instructions)
	}
	alias := filepath.Join(root, "repo-alias")
	if err = os.Symlink(p.Path, alias); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("policy.MD", filepath.Join(p.Path, "policy-alias.md")); err != nil {
		t.Fatal(err)
	}
	input.Path = alias
	input.InstructionFiles = []string{"policy-alias.md"}
	want, err = workspace.Register(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.call("register", p, core.Workspace{}, map[string]any{"input": input}, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("canonical repository/policy alias registration differs")
	}
}

func TestRustWorkspaceRegistrationRefusalsOracle(t *testing.T) {
	root, p := workspaceFixture(t)
	r := rustWorkspace{t: t, root: filepath.Join(root, "trees")}
	writeFixture(t, filepath.Join(p.Path, ".env.md"), "synthetic denied policy")
	if err := os.Symlink(".env.md", filepath.Join(p.Path, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(p.Path, "directory.md"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(p.Path, "large.md"), strings.Repeat("x", 256*1024+1))
	if err := os.Mkdir(filepath.Join(p.Path, "subdir"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path, base string
		files            []string
	}{
		{"missing base", p.Path, "", nil}, {"unknown base", p.Path, "absent", nil}, {"subdirectory", filepath.Join(p.Path, "subdir"), "master", nil},
		{"environment", p.Path, "master", []string{".env.md"}}, {"environment alias", p.Path, "master", []string{"alias.md"}},
		{"directory", p.Path, "master", []string{"directory.md"}}, {"oversized", p.Path, "master", []string{"large.md"}},
		{"many", p.Path, "master", make([]string, 17)},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := core.RegisterProject{Name: p.Name, Path: test.path, BaseRef: test.base, InstructionFiles: test.files}
			if _, err := workspace.Register(t.Context(), input); err == nil {
				t.Fatal("Go unexpectedly accepted invalid policy")
			}
			local := r
			local.t = t
			if err := local.call("register", p, core.Workspace{}, map[string]any{"input": input}, nil); err == nil {
				t.Fatal("Rust accepted invalid policy")
			}
		})
	}
}

func TestRustWorkspaceAllocationRecoveryAndCleanupOracle(t *testing.T) {
	for _, useWT := range []bool{false, true} {
		name := "git"
		if useWT {
			name = "worktrunk"
		}
		t.Run(name, func(t *testing.T) {
			root, p := workspaceFixture(t)
			if useWT {
				if _, err := exec.LookPath("wt"); err != nil {
					t.Fatal("pinned Worktrunk consumer is required, not skipped")
				}
			}
			// Neither allocator may execute Git or Worktrunk project hooks.
			marker := filepath.Join(root, "hook-ran")
			if err := os.Mkdir(filepath.Join(p.Path, ".config"), 0700); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, filepath.Join(p.Path, ".config", "wt.toml"), "pre-start = \"touch "+marker+"\"\n")
			if err := os.WriteFile(filepath.Join(p.Path, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, implementation := range []string{"go", "rust"} {
				t.Run(implementation, func(t *testing.T) {
					r := rustWorkspace{t: t, root: filepath.Join(root, "trees \"quoted\" 世界"), worktrunk: useWT}
					g := workspace.Git{Root: r.root, UseWorktrunk: useWT}
					var port core.Workspaces = g
					if implementation == "rust" {
						port = r
					}
					w := port.Plan(p, "run_"+implementation)
					if w != g.Plan(p, w.Owner) {
						t.Fatal("unstable plan")
					}
					if err := port.Inspect(t.Context(), p, w); !errors.Is(err, core.ErrWorkspaceMissing) {
						t.Fatalf("missing classification: %v", err)
					}
					allocated, err := port.Ensure(t.Context(), p, w)
					if err != nil {
						t.Fatal(err)
					}
					if !allocated.Allocated || allocated.BaseSHA != gitFixture(t, p.Path, "rev-parse", "HEAD") {
						t.Fatalf("allocation record: %#v", allocated)
					}
					if _, err = os.Stat(marker); !os.IsNotExist(err) {
						t.Fatal("checkout hook executed")
					}
					for _, inspector := range []core.Workspaces{g, r} {
						if err = inspector.Inspect(t.Context(), p, allocated); err != nil {
							t.Fatal(err)
						}
					}
					// Recover through the other implementation, including a newer retained HEAD.
					gitFixture(t, w.Path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "worker commit")
					want, err := g.Ensure(t.Context(), p, w)
					if err != nil {
						t.Fatal(err)
					}
					got, err := r.Ensure(t.Context(), p, w)
					if err != nil {
						t.Fatal(err)
					}
					if got != want {
						t.Fatalf("recovered workspace differs: %#v != %#v", got, want)
					}
					for _, file := range []string{"tracked", "untracked", "ignored"} {
						if file == "tracked" {
							writeFixture(t, filepath.Join(w.Path, file), "committed")
							gitFixture(t, w.Path, "add", file)
							gitFixture(t, w.Path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "tracked")
						}
						if file == "ignored" {
							writeFixture(t, filepath.Join(w.Path, ".gitignore"), "ignored\n")
							gitFixture(t, w.Path, "add", ".gitignore")
							gitFixture(t, w.Path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "ignore")
						}
						writeFixture(t, filepath.Join(w.Path, file), "retained")
						for _, remover := range []core.Workspaces{g, r} {
							if err = remover.Remove(t.Context(), p, w); err == nil {
								t.Fatalf("removed %s data", file)
							}
						}
						if file == "tracked" {
							writeFixture(t, filepath.Join(w.Path, file), "committed")
						} else {
							if err = os.Remove(filepath.Join(w.Path, file)); err != nil {
								t.Fatal(err)
							}
						}
					}
					retained := gitFixture(t, w.Path, "rev-parse", "HEAD")
					if err = port.Remove(t.Context(), p, w); err != nil {
						t.Fatal(err)
					}
					if _, err = os.Lstat(w.Path); !os.IsNotExist(err) {
						t.Fatalf("checkout still exists: %v", err)
					}
					if got := gitFixture(t, p.Path, "rev-parse", w.Branch); got != retained {
						t.Fatal("cleanup deleted/changed retained branch")
					}
					if err = r.Inspect(t.Context(), p, w); !errors.Is(err, core.ErrWorkspaceMissing) {
						t.Fatalf("cleanup recovery classification: %v", err)
					}
				})
			}
			if useWT {
				// Positive control: the configured project hook really executes when
				// deliberately invoked in this fixture, not an unrecognized config.
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "wt", "--yes", "hook", "pre-start")
				cmd.Dir = p.Path
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "XDG_CONFIG_HOME=" + os.Getenv("XDG_CONFIG_HOME"), "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null", "GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("hook positive control: %v: %s", err, output)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal("configured hook positive control did not execute", err)
				}
			}
		})
	}
}

func TestRustWorkspaceRefusesUnownedAndSymlinkBindings(t *testing.T) {
	root, p := workspaceFixture(t)
	r := rustWorkspace{t: t, root: filepath.Join(root, "trees")}
	w := r.Plan(p, "run_owned")
	allocated, err := r.Ensure(t.Context(), p, w)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"allocator", "owner", "path", "branch"} {
		t.Run(field, func(t *testing.T) {
			bad := allocated
			switch field {
			case "allocator":
				bad.Allocator = "worktrunk"
			case "owner":
				bad.Owner = "different"
			case "path":
				bad.Path = p.Path
			case "branch":
				bad.Branch = "master"
			}
			local := r
			local.t = t
			for _, method := range []string{"ensure", "inspect", "remove"} {
				if err := local.call(method, p, bad, nil, nil); err == nil {
					t.Fatalf("%s accepted wrong %s", method, field)
				}
			}
		})
	}
	gitFixture(t, allocated.Path, "symbolic-ref", "HEAD", "refs/heads/master")
	for _, method := range []string{"ensure", "inspect", "remove"} {
		if err = r.call(method, p, allocated, nil, nil); err == nil {
			t.Fatal("accepted checkout whose actual branch changed")
		}
	}
	gitFixture(t, allocated.Path, "symbolic-ref", "HEAD", "refs/heads/"+allocated.Branch)
	// Repointing the recorded project must not adopt a same-name foreign branch.
	foreign := p
	foreign.Path = filepath.Join(root, "foreign")
	if err = os.Mkdir(foreign.Path, 0700); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, foreign.Path, "init", "-b", "master")
	if err = r.Inspect(t.Context(), foreign, allocated); err == nil {
		t.Fatal("adopted another repository")
	}
	for _, method := range []string{"ensure", "inspect", "remove"} {
		if err = r.call(method, foreign, allocated, nil, nil); err == nil {
			t.Fatal("accepted foreign repository")
		}
	}
	alias := r.Plan(p, "run_alias")
	if err = os.Symlink(allocated.Path, alias.Path); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"ensure", "inspect", "remove"} {
		if err = r.call(method, p, alias, nil, nil); err == nil {
			t.Fatal("accepted symlink checkout")
		}
	}
	if err = r.Inspect(t.Context(), p, allocated); err != nil {
		t.Fatal("damaged original owned checkout", err)
	}
	// Nonempty/missing ownership and parent symlinks are refused before allocation.
	bad := r.Plan(p, "../escape")
	if _, err = r.Ensure(t.Context(), p, bad); err == nil {
		t.Fatal("allocated escaped owner")
	}
	parent := filepath.Join(root, "aliased-parent")
	if err = os.Symlink(filepath.Dir(allocated.Path), parent); err != nil {
		t.Fatal(err)
	}
	aliased := rustWorkspace{t: t, root: parent}
	if _, err = aliased.Ensure(t.Context(), p, aliased.Plan(p, "run_new")); err == nil {
		t.Fatal("allocated through a symlink ancestor")
	}
}

func TestRustWorkspaceCleanupRefusesLiveCwdWithoutSignalling(t *testing.T) {
	root, p := workspaceFixture(t)
	r := rustWorkspace{t: t, root: filepath.Join(root, "trees")}
	w, err := r.Ensure(t.Context(), p, r.Plan(p, "run_held"))
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	child.Dir = w.Path
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	joined := false
	t.Cleanup(func() {
		if !joined {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	for _, remover := range []core.Workspaces{workspace.Git{Root: r.root}, r} {
		if err = remover.Remove(t.Context(), p, w); err == nil {
			t.Fatal("removed process-held checkout")
		}
	}
	if err = child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("cleanup signalled/killed holder", err)
	}
	if _, err = os.Stat(w.Path); err != nil {
		t.Fatal("cleanup changed held workspace", err)
	}
	// The fixture owner, not the adapter, explicitly stops its own test process.
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	joined = true
	if err = r.Remove(t.Context(), p, w); err != nil {
		t.Fatal("refused after fixture holder joined", err)
	}
}

func TestRustWorkspaceChildrenOmitCredentialsAndDisableHooks(t *testing.T) {
	root, p := workspaceFixture(t)
	actual, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nset -eu\n"
	var synthetic []string
	for _, name := range []string{"TANDEM_TOKEN", "OPENCODE_SERVER_PASSWORD", "OPENCODE_URL", "AWS_SECRET_ACCESS_KEY", "GIT_DIR", "GIT_SSH_COMMAND"} {
		synthetic = append(synthetic, name+"=synthetic-private-control")
		script += "test -z \"${" + name + "+present}\" || exit 91\n"
	}
	synthetic = append(synthetic, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=synthetic-unsafe-hook")
	script += "test \"$GIT_CONFIG_COUNT\" = 2\ntest \"$GIT_CONFIG_KEY_0\" = core.hooksPath\ntest \"$GIT_CONFIG_VALUE_0\" = /dev/null\ntest \"$GIT_CONFIG_KEY_1\" = core.fsmonitor\ntest \"$GIT_CONFIG_VALUE_1\" = false\ntest \"$GIT_OPTIONAL_LOCKS\" = 0\nexec '" + actual + "' \"$@\"\n"
	if err = os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := rustWorkspace{t: t, root: filepath.Join(root, "trees"), extraEnvironment: synthetic}
	w, err := r.Ensure(t.Context(), p, r.Plan(p, "run_credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Inspect(t.Context(), p, w); err != nil {
		t.Fatal(err)
	}
}

func TestRustWorkspaceAmbiguousAllocationRecoversWithoutAnotherMutation(t *testing.T) {
	for _, useWT := range []bool{false, true} {
		program := "git"
		if useWT {
			program = "wt"
		}
		t.Run(program, func(t *testing.T) {
			root, p := workspaceFixture(t)
			actual, err := exec.LookPath(program)
			if err != nil {
				t.Fatal("actual allocator required", err)
			}
			bin := filepath.Join(root, "bin")
			if err = os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "mutation-count")
			// Only the post-execution result boundary is fault-injected. The real
			// allocator creates its actual Git worktree and branch before failure.
			script := "#!/bin/sh\nset -eu\n'" + actual + "' \"$@\"\n"
			if !useWT {
				script += "if test \"$1\" = worktree && test \"$2\" = add; then\n"
			}
			script += "printf x >> '" + marker + "'\nexit 91\n"
			if !useWT {
				script += "fi\n"
			}
			if err = os.WriteFile(filepath.Join(bin, program), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			r := rustWorkspace{t: t, root: filepath.Join(root, "trees"), worktrunk: useWT}
			w := r.Plan(p, "run_uncertain")
			if _, err = r.Ensure(t.Context(), p, w); err == nil {
				t.Fatal("post-allocation failure was hidden")
			}
			if err = r.Inspect(t.Context(), p, w); err != nil {
				t.Fatal("partial allocation was discarded", err)
			}
			recovered, err := r.Ensure(t.Context(), p, w)
			if err != nil || !recovered.Allocated {
				t.Fatal("read-only allocation recovery failed", err)
			}
			count, err := os.ReadFile(marker)
			if err != nil || string(count) != "x" {
				t.Fatalf("ambiguous mutation was retried: %q, %v", count, err)
			}
		})
	}
}
