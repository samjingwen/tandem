use std::path::Path;

use tandem_core::{Context, Result};

use crate::failure;

/// Read-only cwd inventory. This is not a sandbox or a guarantee against a
/// concurrent user process changing cwd after verification. Never signal a
/// holder, adopt it, or stop a shared service to make cleanup succeed.
pub(crate) async fn refuse(context: &Context, path: &Path) -> Result<()> {
    #[cfg(target_os = "linux")]
    {
        let path = path.to_path_buf();
        crate::files::blocking(context, move |context| linux(&context, &path)).await
    }
    #[cfg(target_os = "macos")]
    {
        let context = context.with_timeout(std::time::Duration::from_secs(5));
        let uid = rustix::process::getuid().as_raw().to_string();
        // The inventory process itself must not acquire a cwd in the checkout.
        let directory = path
            .parent()
            .ok_or_else(|| failure("workspace parent is missing"))?;
        let output = crate::process::run(
            &context,
            directory,
            "lsof",
            &[
                "-nP".as_ref(),
                "-a".as_ref(),
                "-d".as_ref(),
                "cwd".as_ref(),
                "-Fpn".as_ref(),
                "-u".as_ref(),
                uid.as_ref(),
            ],
        )
        .await?;
        if !output.stderr.is_empty() {
            return Err(failure(
                "cannot prove workspace cwd quiescence through lsof; cleanup refused",
            ));
        }
        macos_output(path, &output.stdout)
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        let _ = (context, path);
        Err(failure(
            "workspace cleanup process verification is supported on macOS/Linux only",
        ))
    }
}

#[cfg(target_os = "linux")]
fn linux(context: &Context, path: &Path) -> Result<()> {
    use std::{fs, os::unix::fs::MetadataExt};
    let entries = fs::read_dir("/proc")
        .map_err(|_| failure("cannot inspect process cwd ownership before cleanup"))?;
    let uid = rustix::process::getuid().as_raw();
    for entry in entries {
        context.check()?;
        let entry = entry.map_err(|_| failure("cannot enumerate process ownership"))?;
        if entry
            .file_name()
            .to_str()
            .and_then(|v| v.parse::<u32>().ok())
            .is_none()
        {
            continue;
        }
        let metadata = match fs::metadata(entry.path()) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(_) => return Err(failure("cannot establish process ownership before cleanup")),
        };
        if metadata.uid() != uid {
            continue;
        }
        match fs::read_link(entry.path().join("cwd")) {
            Ok(cwd) if cwd.starts_with(path) => {
                return Err(failure(
                    "refusing cleanup: a process still holds a cwd inside the workspace",
                ));
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => {
                return Err(failure(
                    "cannot establish an owned process cwd before cleanup",
                ));
            }
        }
    }
    context.check()
}

#[cfg(any(target_os = "macos", test))]
fn macos_output(path: &Path, bytes: &[u8]) -> Result<()> {
    let output =
        std::str::from_utf8(bytes).map_err(|_| failure("cannot decode process cwd ownership"))?;
    let mut pid = false;
    let mut cwd = false;
    let mut found = false;
    for line in output.lines() {
        if let Some(value) = line.strip_prefix('p') {
            if pid && !cwd {
                return Err(failure(
                    "cannot establish an owned process cwd before cleanup",
                ));
            }
            if value.parse::<u32>().is_err() {
                return Err(failure("cannot decode process ownership"));
            }
            pid = true;
            cwd = false;
            found = true;
        } else if let Some(value) = line.strip_prefix('n') {
            if !pid || !Path::new(value).is_absolute() {
                return Err(failure(
                    "cannot establish an owned process cwd before cleanup",
                ));
            }
            cwd = true;
            if Path::new(value).starts_with(path) {
                return Err(failure(
                    "refusing cleanup: a process still holds a cwd inside the workspace",
                ));
            }
        } else if line != "fcwd" {
            return Err(failure("ambiguous process cwd metadata; cleanup refused"));
        }
    }
    if !found || !cwd {
        return Err(failure(
            "cannot prove workspace cwd quiescence through lsof; cleanup refused",
        ));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn process_inventory_is_bounded_to_path_components_and_fails_closed() {
        let path = Path::new("/fixture/work");
        assert!(macos_output(path, b"p1\nfcwd\nn/fixture/work-other\n").is_ok());
        for output in [
            b"p1\nn/fixture/work\n".as_slice(),
            b"p1\nn/fixture/work/child\n",
            b"",
            b"p1\n",
            b"p1\nnUNKNOWN\n",
            b"p1\np2\nn/other\n",
        ] {
            assert!(macos_output(path, output).is_err());
        }
    }
}
