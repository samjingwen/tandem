//! Concrete Git/Worktrunk implementation of the recorded Workspaces boundary.
//! The engine owns admission, persistence and runtime quiescence. This adapter
//! never discovers a runtime, retries uncertain allocation or deletes branches.

mod files;
mod holders;
mod process;
mod register;

#[cfg(all(test, unix))]
mod settlement_tests;

use std::{
    ffi::OsStr,
    fs,
    path::{Component, Path, PathBuf},
};

use async_trait::async_trait;
use tandem_core::{Context, Error, Project, RegisterProject, Result, Workspace, Workspaces};

fn failure(message: &str) -> Error {
    Error::new("workspace_unavailable", message)
}

#[derive(Clone)]
pub struct Git {
    root: PathBuf,
    use_worktrunk: bool,
}

impl Git {
    /// The composition root supplies its canonical state root. Construction and
    /// planning create no directories and run no external commands.
    pub fn new(root: impl AsRef<Path>, use_worktrunk: bool) -> Result<Self> {
        let root = files::absolute_clean(root.as_ref())?;
        files::text(&root)?;
        Ok(Self {
            root,
            use_worktrunk,
        })
    }

    fn allocator(&self) -> &str {
        if self.use_worktrunk {
            "worktrunk"
        } else {
            "git"
        }
    }

    fn binding(&self, project: &Project, workspace: &Workspace) -> Result<()> {
        fn component(value: &str) -> bool {
            let mut parts = Path::new(value).components();
            !value.is_empty()
                && matches!(parts.next(), Some(Component::Normal(_)))
                && parts.next().is_none()
                && !value.contains(['/', '\\', '\0'])
        }
        if workspace.allocator != self.allocator() {
            return Err(failure(
                "workspace allocator changed; restore its recorded allocation authority",
            ));
        }
        if !component(&project.id)
            || !component(&workspace.owner)
            || workspace.branch != format!("tandem/{}", workspace.owner)
            || files::absolute_clean(Path::new(&workspace.path))?
                != self.root.join(&project.id).join(&workspace.owner)
            || !Path::new(&workspace.path).is_absolute()
        {
            return Err(failure("workspace ownership mismatch"));
        }
        Ok(())
    }

    async fn directory(context: &Context, path: &Path) -> Result<()> {
        let path = path.to_path_buf();
        files::blocking(context, move |context| {
            files::verify_directory(&context, &path)
        })
        .await
    }

    async fn inspect_owned(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        self.binding(project, workspace)?;
        let path = PathBuf::from(&workspace.path);
        let check = path.clone();
        files::blocking(context, move |context| {
            context.check()?;
            match fs::symlink_metadata(&check) {
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    return Err(Error::new("workspace_missing", "workspace is missing"));
                }
                Err(_) => return Err(failure("cannot inspect workspace path")),
                Ok(metadata) if !metadata.is_dir() || metadata.file_type().is_symlink() => {
                    return Err(failure("workspace path is not a regular directory"));
                }
                Ok(_) => {}
            }
            files::verify_directory(&context, &check)
        })
        .await?;
        Self::directory(context, Path::new(&project.path)).await?;
        let top = process::git(
            context,
            &path,
            &["rev-parse".as_ref(), "--show-toplevel".as_ref()],
        )
        .await?;
        if Path::new(&top) != path {
            return Err(failure("workspace is not a checkout root"));
        }
        let branch = process::git(
            context,
            &path,
            &["symbolic-ref".as_ref(), "--short".as_ref(), "HEAD".as_ref()],
        )
        .await?;
        if branch != workspace.branch {
            return Err(failure("workspace branch does not match assignment"));
        }
        let args: &[&OsStr] = &[
            "rev-parse".as_ref(),
            "--path-format=absolute".as_ref(),
            "--git-common-dir".as_ref(),
        ];
        let common = process::git(context, &path, args).await?;
        let expected = process::git(context, Path::new(&project.path), args).await?;
        if common != expected {
            return Err(failure("workspace belongs to a different repository"));
        }
        Ok(())
    }
}

#[async_trait]
impl Workspaces for Git {
    fn plan(&self, project: &Project, attempt_id: &str) -> Workspace {
        Workspace {
            allocator: self.allocator().into(),
            owner: attempt_id.into(),
            path: self
                .root
                .join(&project.id)
                .join(attempt_id)
                .to_string_lossy()
                .into_owned(),
            branch: format!("tandem/{attempt_id}"),
            base_ref: project.base_ref.clone(),
            ..Workspace::default()
        }
    }

    async fn register(&self, context: &Context, input: &RegisterProject) -> Result<Project> {
        register::register(context, input).await
    }

    async fn ensure(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<Workspace> {
        context.check()?;
        self.binding(project, workspace)?;
        let mut allocated = workspace.clone();
        match self.inspect_owned(context, project, workspace).await {
            Ok(()) => {
                allocated.base_sha = process::git(
                    context,
                    Path::new(&workspace.path),
                    &["rev-parse".as_ref(), "HEAD".as_ref()],
                )
                .await?;
                allocated.allocated = true;
                return Ok(allocated);
            }
            Err(error) if error.code == "workspace_missing" => {}
            Err(error) => return Err(error),
        }
        Self::directory(context, Path::new(&project.path)).await?;
        if workspace.base_ref.is_empty() {
            return Err(Error::invalid("recorded workspace baseRef is required"));
        }
        let reference = format!("{}^{{commit}}", workspace.base_ref);
        let sha = process::git(
            context,
            Path::new(&project.path),
            &[
                "rev-parse".as_ref(),
                "--verify".as_ref(),
                "--end-of-options".as_ref(),
                reference.as_ref(),
            ],
        )
        .await?;
        let parent = Path::new(&workspace.path)
            .parent()
            .ok_or_else(|| failure("workspace parent is missing"))?
            .to_path_buf();
        files::blocking(context, move |context| {
            files::private_parents(&context, &parent)
        })
        .await?;
        // No retry or rollback on command failure/cancellation. An allocated
        // checkout may already exist; the next engine reconciliation inspects it.
        if self.use_worktrunk {
            let quoted = serde_json::to_string(&workspace.path)
                .map_err(|_| failure("invalid workspace path"))?;
            let config = format!("worktree-path = {quoted}");
            process::run(
                context,
                Path::new(&project.path),
                "wt",
                &[
                    "--config-set".as_ref(),
                    config.as_ref(),
                    "switch".as_ref(),
                    "--create".as_ref(),
                    workspace.branch.as_ref(),
                    "--base".as_ref(),
                    sha.as_ref(),
                    "--no-hooks".as_ref(),
                    "--no-cd".as_ref(),
                    "--format".as_ref(),
                    "json".as_ref(),
                ],
            )
            .await?;
        } else {
            process::git(
                context,
                Path::new(&project.path),
                &[
                    "worktree".as_ref(),
                    "add".as_ref(),
                    "-b".as_ref(),
                    workspace.branch.as_ref(),
                    workspace.path.as_ref(),
                    sha.as_ref(),
                ],
            )
            .await?;
        }
        self.inspect_owned(context, project, workspace).await?;
        allocated.base_sha = sha;
        allocated.allocated = true;
        Ok(allocated)
    }

    async fn inspect(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        self.inspect_owned(context, project, workspace).await
    }

    async fn remove(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        self.inspect_owned(context, project, workspace).await?;
        let path = Path::new(&workspace.path);
        let dirty = process::git(
            context,
            path,
            &[
                "status".as_ref(),
                "--porcelain=v1".as_ref(),
                "--untracked-files=all".as_ref(),
                "--ignored".as_ref(),
            ],
        )
        .await?;
        if !dirty.is_empty() {
            return Err(failure(
                "refusing cleanup of dirty workspace, including untracked or ignored files",
            ));
        }
        holders::refuse(context, path).await?;
        // Recheck the binding after the read-only process inventory. Git performs
        // its own final dirty check; no --force and no branch deletion are used.
        self.inspect_owned(context, project, workspace).await?;
        process::git(
            context,
            Path::new(&project.path),
            &[
                "worktree".as_ref(),
                "remove".as_ref(),
                workspace.path.as_ref(),
            ],
        )
        .await?;
        Ok(())
    }
}
