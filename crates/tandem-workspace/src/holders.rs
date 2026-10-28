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
            Err(error)
                if error.kind() == std::io::ErrorKind::NotFound
                    && linux_exited(context, &entry.path())? => {}
            Err(_) => {
                return Err(failure(
                    "cannot establish an owned process cwd before cleanup",
                ));
            }
        }
    }
    context.check()
}

#[cfg(target_os = "linux")]
fn linux_exited(context: &Context, process: &Path) -> Result<bool> {
    use std::{fs, io::ErrorKind};
    context.check()?;
    let stat = match fs::read(process.join("stat")) {
        Ok(stat) => stat,
        Err(error) if error.kind() == ErrorKind::NotFound => {
            return match fs::metadata(process) {
                Err(error) if error.kind() == ErrorKind::NotFound => Ok(true),
                Ok(_) => Ok(false),
                Err(_) => Err(failure("cannot establish process exit before cleanup")),
            };
        }
        Err(_) => return Err(failure("cannot establish process exit before cleanup")),
    };
    let Some(end) = stat.iter().rposition(|byte| *byte == b')') else {
        return Ok(false);
    };
    let mut fields = stat[end + 1..]
        .split(u8::is_ascii_whitespace)
        .filter(|field| !field.is_empty());
    let state = fields.next();
    Ok(
        (state == Some(b"Z".as_slice()) || state == Some(b"X".as_slice()))
            && fields.nth(16) == Some(b"1".as_slice()),
    )
}

#[cfg(any(target_os = "macos", test))]
fn macos_name(value: &str) -> Result<std::path::PathBuf> {
    let invalid = || failure("ambiguous process cwd encoding; cleanup refused");
    let mut bytes = value.bytes();
    let mut decoded = Vec::with_capacity(value.len());
    while let Some(byte) = bytes.next() {
        let byte = if byte == b'\\' {
            match bytes.next() {
                Some(b'\\') => b'\\',
                Some(b'a') => 7,
                Some(b'b') => 8,
                Some(b'f') => 12,
                Some(b'n') => b'\n',
                Some(b'r') => b'\r',
                Some(b't') => b'\t',
                Some(b'v') => 11,
                Some(b'x') => {
                    let high = bytes
                        .next()
                        .and_then(|byte| char::from(byte).to_digit(16))
                        .ok_or_else(invalid)?;
                    let low = bytes
                        .next()
                        .and_then(|byte| char::from(byte).to_digit(16))
                        .ok_or_else(invalid)?;
                    u8::try_from(high * 16 + low).map_err(|_| invalid())?
                }
                _ => return Err(invalid()),
            }
        } else {
            byte
        };
        if byte == 0 {
            return Err(invalid());
        }
        decoded.push(byte);
    }
    let path = std::path::PathBuf::from(String::from_utf8(decoded).map_err(|_| invalid())?);
    if !path.is_absolute() {
        return Err(invalid());
    }
    Ok(path)
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
            let name = macos_name(value)?;
            if !pid {
                return Err(failure(
                    "cannot establish an owned process cwd before cleanup",
                ));
            }
            cwd = true;
            if name.starts_with(path) {
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

    #[cfg(target_os = "macos")]
    #[test]
    fn executable_lsof_unicode_holder_is_not_quiescent() {
        use std::{
            io::Write,
            process::{Command, Stdio},
        };
        let fixture = tempfile::tempdir().unwrap();
        let path = fixture
            .path()
            .canonicalize()
            .unwrap()
            .join("holder-é-\\xc3");
        std::fs::create_dir(&path).unwrap();
        let mut child = Command::new("sh")
            .args(["-c", "read release"])
            .current_dir(&path)
            .stdin(Stdio::piped())
            .spawn()
            .unwrap();
        let output = Command::new("lsof")
            .args([
                "-nP",
                "-a",
                "-d",
                "cwd",
                "-Fpn",
                "-p",
                &child.id().to_string(),
            ])
            .env_clear()
            .env("PATH", std::env::var_os("PATH").unwrap_or_default())
            .env("LC_ALL", "C")
            .output();
        child.stdin.take().unwrap().write_all(b"release\n").unwrap();
        child.wait().unwrap();
        let output = output.unwrap();
        assert!(output.status.success());
        assert!(output.stderr.is_empty());
        assert!(
            macos_output(&path, &output.stdout).is_err(),
            "live Unicode cwd holder was missed: {:?}",
            output.stdout
        );
        assert!(macos_output(&path.with_file_name("holder-é-\\xc3-other"), &output.stdout).is_ok());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn executable_live_thread_without_main_cwd_is_not_quiescent() {
        use std::{
            process::Command,
            time::{Duration, Instant},
        };
        let fixture = tempfile::tempdir().unwrap();
        let path = fixture.path().canonicalize().unwrap();
        let source = path.join("holder.c");
        std::fs::write(
            &source,
            br#"
#include <pthread.h>
#include <unistd.h>
#include <stdlib.h>
static void *hold(void *unused) {
    (void)unused;
    while (access("release", F_OK)) usleep(1000);
    exit(0);
}
int main(void) {
    pthread_t thread;
    if (pthread_create(&thread, NULL, hold, NULL)) return 1;
    pthread_exit(NULL);
}
"#,
        )
        .unwrap();
        let binary = path.join("holder");
        assert!(
            Command::new("cc")
                .args(["-pthread", "-o"])
                .arg(&binary)
                .arg(&source)
                .status()
                .unwrap()
                .success()
        );
        let mut child = Command::new(binary).current_dir(&path).spawn().unwrap();
        let cwd = std::path::PathBuf::from(format!("/proc/{}/cwd", child.id()));
        let deadline = Instant::now() + Duration::from_secs(5);
        while std::fs::read_link(&cwd).is_ok() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(5));
        }
        let missing = std::fs::read_link(&cwd).err().map(|error| error.kind());
        let live = child.try_wait();
        let result = linux(&Context::background(), &path);
        std::fs::write(path.join("release"), b"release").unwrap();
        assert!(child.wait().unwrap().success());
        assert_eq!(missing, Some(std::io::ErrorKind::NotFound));
        assert!(live.unwrap().is_none());
        assert!(result.is_err(), "live pthread cwd ownership was missed");
        assert!(linux_exited(&Context::background(), cwd.parent().unwrap()).unwrap());
    }

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
            b"p1\nn/fixture/\\q\n",
            b"p1\nn/fixture/\\xGG\n",
            b"p1\nn/fixture/\\x00\n",
        ] {
            assert!(macos_output(path, output).is_err());
        }
        assert!(
            macos_output(Path::new("/fixture/é"), b"p1\nn/fixture/\\xc3\\xa9/child\n").is_err()
        );
        assert!(macos_output(Path::new("/fixture/é"), b"p1\nn/fixture/\\xc3\\xa9-other\n").is_ok());
        assert_eq!(
            macos_name("/fixture/\\\\xc3-\\n\\t\\xc3\\xa9").unwrap(),
            Path::new("/fixture/\\xc3-\n\té")
        );
    }
}
