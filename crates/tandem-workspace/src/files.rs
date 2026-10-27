use std::{
    fs::{self, File},
    path::{Component, Path, PathBuf},
};

use tandem_core::{Context, Result};

use crate::failure;

pub(crate) fn absolute_clean(path: &Path) -> Result<PathBuf> {
    let absolute = std::path::absolute(path)
        .map_err(|_| failure("cannot establish absolute workspace path"))?;
    let mut cleaned = PathBuf::new();
    for component in absolute.components() {
        match component {
            Component::CurDir => {}
            Component::ParentDir => {
                cleaned.pop();
            }
            other => cleaned.push(other.as_os_str()),
        }
    }
    Ok(cleaned)
}

pub(crate) fn text(path: &Path) -> Result<String> {
    path.to_str()
        .map(str::to_owned)
        .ok_or_else(|| failure("workspace paths must have a UTF-8 representation"))
}

#[cfg(unix)]
fn directory(context: &Context, path: &Path, create: bool) -> Result<std::os::fd::OwnedFd> {
    use rustix::fs::{Mode, OFlags, mkdirat, open, openat};
    if !path.is_absolute() {
        return Err(failure("workspace directory must be absolute"));
    }
    let flags = OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC | OFlags::NOFOLLOW;
    let mut descriptor = open("/", flags, Mode::empty())
        .map_err(|_| failure("cannot open workspace root directory"))?;
    for component in path.components() {
        context.check()?;
        let Component::Normal(name) = component else {
            if matches!(component, Component::RootDir) {
                continue;
            }
            return Err(failure("workspace directory is not canonical"));
        };
        match openat(&descriptor, name, flags, Mode::empty()) {
            Ok(next) => descriptor = next,
            Err(rustix::io::Errno::NOENT) if create => {
                match mkdirat(&descriptor, name, Mode::from_bits_truncate(0o700)) {
                    Ok(()) | Err(rustix::io::Errno::EXIST) => {}
                    Err(_) => return Err(failure("cannot create private workspace directory")),
                }
                descriptor = openat(&descriptor, name, flags, Mode::empty())
                    .map_err(|_| failure("workspace directory changed or is a symlink"))?;
            }
            Err(_) => {
                return Err(failure(
                    "workspace directory is unavailable or is a symlink",
                ));
            }
        }
    }
    Ok(descriptor)
}

pub(crate) fn private_parents(context: &Context, path: &Path) -> Result<()> {
    #[cfg(unix)]
    {
        directory(context, path, true)?;
    }
    #[cfg(not(unix))]
    {
        context.check()?;
        for parent in path.ancestors() {
            if fs::symlink_metadata(parent).is_ok_and(|m| m.file_type().is_symlink()) {
                return Err(failure("workspace parent is a symlink"));
            }
        }
        fs::create_dir_all(path).map_err(|_| failure("cannot create workspace parent"))?;
    }
    context.check()
}

pub(crate) fn verify_directory(context: &Context, path: &Path) -> Result<()> {
    #[cfg(unix)]
    {
        directory(context, path, false)?;
    }
    #[cfg(not(unix))]
    {
        context.check()?;
        if canonical(path)? != path {
            return Err(failure("workspace directory is not canonical"));
        }
        if !path.is_dir() {
            return Err(failure("workspace directory is unavailable"));
        }
    }
    context.check()
}

pub(crate) fn open_regular(context: &Context, path: &Path) -> Result<File> {
    context.check()?;
    #[cfg(unix)]
    let file = {
        use rustix::fs::{Mode, OFlags, openat};
        let parent = path
            .parent()
            .ok_or_else(|| failure("instruction path has no parent"))?;
        let name = path
            .file_name()
            .ok_or_else(|| failure("instruction path has no filename"))?;
        let descriptor = directory(context, parent, false)?;
        File::from(
            openat(
                &descriptor,
                name,
                OFlags::RDONLY | OFlags::CLOEXEC | OFlags::NOFOLLOW | OFlags::NONBLOCK,
                Mode::empty(),
            )
            .map_err(|_| failure("instruction file changed or is unavailable"))?,
        )
    };
    #[cfg(not(unix))]
    let file = File::open(path).map_err(|_| failure("instruction file is unavailable"))?;
    let metadata = file
        .metadata()
        .map_err(|_| failure("cannot inspect instruction file"))?;
    if !metadata.is_file() {
        return Err(failure("instruction file must be regular"));
    }
    context.check()?;
    Ok(file)
}

pub(crate) async fn blocking<T: Send + 'static>(
    context: &Context,
    work: impl FnOnce(Context) -> Result<T> + Send + 'static,
) -> Result<T> {
    context.check()?;
    let context = context.clone();
    // Joining, rather than detaching a blocking filesystem operation on timeout,
    // retains the engine's effect ownership until all actual work has settled.
    tokio::task::spawn_blocking(move || work(context))
        .await
        .map_err(|_| failure("workspace filesystem operation failed"))?
}

pub(crate) fn canonical(path: &Path) -> Result<PathBuf> {
    fs::canonicalize(path)
        .map_err(|_| failure("cannot resolve canonical workspace/instruction path"))
}
