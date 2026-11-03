# Warm Worktrunk checkouts

New worktrees reuse compatible local dependencies and compiled dependencies instead of repeating downloads and cold builds.
The project-owned `.config/wt.toml` runs one blocking `pre-start` hook, so copying finishes before background hooks or `wt switch --execute` start a dependent command.
This is a development optimization, not runtime state migration or a security sandbox.

## Normal workflow

Warm a trusted source checkout using the normal project commands:

```sh
make prepare
make check
make build
wt switch --create feature/example
make prepare
make check
make build
```

Preparation and successful Rust build/check targets record compatibility metadata under ignored `.tmp/worktree-cache/`.
The hook uses the base worktree when available, otherwise the primary worktree.
An older source checkout without this configuration or compatibility metadata is a cache miss, not permission to borrow unrelated work.
New checkouts never start application servers or native OpenCode through this hook.

Preview or retry a copy explicitly from a trusted worktree of the same repository:

```sh
python3 tools/worktree-cache.py copy --from /path/to/source/worktree --dry-run
python3 tools/worktree-cache.py copy --from /path/to/source/worktree
```

The helper invokes the real `wt step copy-ignored --require-include` consumer without `--force`.
It uses Worktrunk's independent reflink/copy behavior, never shared writable cache symlinks.
Existing destination caches are left untouched, not merged with source caches.
If a started copy fails, the blocking hook fails rather than letting dependent commands trust partial data.
Preserve the checkout and run normal preparation before building again.

## What is reused

`.worktreeinclude` permits only these ignored artifacts:

| Artifact | Compatibility and validation |
| --- | --- |
| `plugin/node_modules/` | Matching manifests/lockfile, prepared installed-lock digest, Node version/ABI/architecture, npm version, OS/libc and relevant build flags |
| Rust debug/release `.fingerprint`, `deps`, `build`, and compiler cache metadata | Matching workspace manifests/lockfile/toolchain, actual rustc host/version, cargo version, target/build flags and OS/libc |
| Official Worktrunk archive under `.tmp/worktrunk/` | Exact current-platform archive digest from the existing preparation tool; preparation re-extracts and verifies the executable |
| `.tmp/rust-tools/bin/cargo-audit` | Current project tool-version pin, compiler/platform signature and recorded binary digest; installation receipts/control files are not copied |

Cargo remains responsible for fingerprint validation and rebuilding changed workspace crates.
Copied compiled dependencies are not a promise that path-dependent workspace artifacts never rebuild.
Custom Cargo target/build directories are not copied.
Incremental compiler state, application binaries, plugin `dist`, unrelated target outputs and all retained validation evidence are excluded.

Go module/build caches, Cargo registry/git caches, npm package downloads and pip package downloads normally already live in tool-managed machine caches shared by worktrees.
They are reused in place by their normal consumers, not copied wholesale into Git worktrees or redirected into private tool homes.
Do not copy a Cargo/npm home: it may contain authentication or configuration.
Python's path-bound `.tmp/python-tools` virtual environment is recreated by normal `make python-prepare`, reusing pip's package download cache.

## Safety and cache misses

Copying is allowed only with matching source/destination allowlists and recorded compatible inputs.
The metadata is a trusted local build receipt, not an artifact signature or proof that another developer's cache is trustworthy.
Use only trusted, quiescent source worktrees.
The helper coordinates with project npm/tool preparation locks and holds existing Cargo profile locks while copying compiled caches.
It skips busy caches instead of waiting on, stealing, or copying control locks.
Do not run out-of-band npm mutations against the source during copying.

Worktrunk 0.81.0 copies an allowlisted directory as a whole; its additional excludes do not sanitize that directory's descendants.
Therefore the helper inspects directory entry names/types without reading private file contents before selecting a cache tree.
An environment file, credential store, runtime database, process lock, special file or escaping symlink inside a selected tree excludes that entire cache.
Ordinary shipped dependency lockfiles such as `yarn.lock` are immutable dependency data, not process locks.
Relative internal npm `.bin` symlinks are allowed; links outside their own cache are refused.
Do not bypass this preflight by using raw directory copying as a substitute.

No `.env`, authentication stores, application/service ownership records, databases, transcripts, user data, Python environments or validation evidence are selected from the project root.
Unknown metadata, changed lockfiles, compiler/Node ABI differences, platform/target differences and busy caches fall back to normal preparation/build.
Neither source caches nor destination files are deleted to force a match.

## Reproducible cold path and tests

The lockfiles, toolchain pins and cold-install path remain authoritative:

```sh
make plugin-prepare COLD=1
make prepare
make check
make build
```

`COLD=1` forces the existing script-disabled `npm ci` rather than accepting an installed dependency receipt.
Alternatively, `npm --prefix plugin ci --ignore-scripts` is still available directly; the helper does not change npm's lockfile resolution.
Other standard preparation commands continue to fetch missing prerequisites normally.
When all relevant downloads are cached, the tool-specific offline options can demonstrate that a warm build needs no network.
Do not describe missing cold prerequisites or a partially prepared environment as a passing offline build.

`make python-test` runs real Git/Worktrunk fixture tests for copying, cold misses, version/lock incompatibility, destination preservation, independent writes, busy sources, symlink/privacy refusals and blocking hook ordering.
These tests use synthetic private data and disposable repositories, not personal environment files or runtime state.
Python quality checks and enforcement probes include both cache tools.
