# Tandem change delivery

This policy belongs to Tandem and applies to this repository and its explicitly assigned implementation worktrees.
Read it together with `AGENTS.md`, `docs/RUST_MIGRATION.md`, and the relevant API, operations, runtime, and architecture contracts.
Do not depend on an instruction file in a parent workspace.

## Scope and authority

Own the applicable delivery sequence: dedicated Worktrunk worktree -> requirements and implementation -> executable verification and review -> GitHub PR -> protected merge -> safe local primary-branch synchronization.
Commit intended changes, push the feature branch, open the PR, and merge once required checks and reviews pass.
Explicit Plan, review-only, PR-only, human-review, native/model, retained-runtime, release, and production limits take precedence.
Do not infer managed-task acceptance from session idleness or a worker's success report.
Do not invent a deployment or publication target.

The user retired the no-mistakes gate and configuration.
Do not initialize, install, invoke, or restore that gate here.
Preserve unpublished source and unresolved acceptance requirements when disconnecting it.
Its retirement does not waive executable quality checks, independent review, GitHub CI, or branch protections.

## Isolate and implement

- Inspect status, worktrees, the actual default branch, and active ownership before editing.
- Read `wt switch --help` and inspect setup hooks before creating a dedicated implementation worktree with Worktrunk.
- Fetch the intended base or use the base explicitly selected for the task.
- Use the returned worktree path and verify the actual directory, branch, and base before editing.
- Continue related work in its existing worktree without borrowing another task's changes.
- Preserve uncommitted work, private local material, other sessions, retained evidence, and persistent SQLite/runtime data.
- Preserve Go/Rust behavioral parity, SQLite compatibility, the v1 HTTP/OpenAPI contract, and OpenCode's ownership of agent execution.
- Keep dispatch uncertainty explicit and never replay an ambiguous external mutation speculatively.
- Regenerate generated contracts through their owning tools; never hand-edit generated output or `CHANGELOG.md`.
- Never read private environment/authentication files or commit credentials, transcripts, and machine-local records.

## Verify and review

- Use `make prepare` for project-managed dependency preparation.
- Run `make check`, relevant `make test-race`, and the migration's `make rust-test rust-parity` commands as required by the affected surfaces.
- Use `make build` when changing executable or packaged behavior and `make format` separately for safe formatting fixes.
- Retain Go, Rust, and plugin checks while those surfaces remain maintained; do not silently select Rust before its cutover criteria pass.
- Keep pinned toolchains, strict compiler/static-analysis checks, real interoperability tests, and existing quality/security enforcement probes.
- Preserve ownership-based exclusions for generated, vendored, or upstream-pinned files; do not weaken checks to obtain green output.
- Reproduce reported bugs through the user-facing path before fixing them.
- Preserve failing evidence and rerun checks invalidated by each fix.
- Review the full base-to-head diff with the installed Hardlook skill and record coverage, findings, and limitations.
- Keep actual GitHub CI and required reviews; missing, pending, failed, or incomplete evidence is not a pass.
- Synthetic fixtures do not establish live native/model, retained-state conversion, phone/remote, or production acceptance.

## Deliver and synchronize

- Review status, all intended commits, and the full diff; stage only this task's files.
- Prefer `gh-axi` for GitHub operations, with `gh` as a fallback.
- Include decisions, exact-head evidence, limitations, and any compatibility notes in the PR.
- Satisfy latest-head required checks, reviews, and conversation resolution without bypassing protections.
- Merge through GitHub and verify the actual merged state and exact merge SHA.
- Recheck the default-branch worktree's dirty/untracked/ignored collisions and active ownership before synchronization.
- Pull the actual default branch with fast-forward-only semantics when safe; never stash, reset, clean, force refs, or rebase local divergence to make it succeed.
- Verify local default HEAD equals the freshly fetched remote tip and contains the merge, retaining later remote commits.
- Report a concrete synchronization blocker rather than overwriting another task's state.
- Release only a clean merged revision through an established, separately authorized procedure with compatible data and bounded recovery.
- Report the PR, merge SHA, checks, primary path/branch/SHA, and any separately authorized release outcome honestly.
