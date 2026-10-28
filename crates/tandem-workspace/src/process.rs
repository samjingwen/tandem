use std::{ffi::OsStr, path::Path, process::Stdio, time::Duration};

use tandem_core::{Context, Error, Result};
use tokio::{
    io::{AsyncRead, AsyncReadExt},
    process::Command,
};

use crate::failure;

const OUTPUT_LIMIT: usize = 1 << 20;

pub(crate) struct Output {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
}

async fn bounded_read(mut reader: impl AsyncRead + Unpin) -> Result<Vec<u8>> {
    let mut output = Vec::new();
    let mut buffer = [0; 8192];
    loop {
        let count = reader
            .read(&mut buffer)
            .await
            .map_err(|_| failure("cannot read workspace command output"))?;
        if count == 0 {
            return Ok(output);
        }
        if output.len().saturating_add(count) > OUTPUT_LIMIT {
            return Err(failure(
                "workspace command output exceeds the verification budget",
            ));
        }
        output.extend_from_slice(&buffer[..count]);
    }
}

/// Child credentials/control settings are not inherited. A command's diagnostics
/// are bounded and never promoted into public errors or interpreted as a retry.
pub(crate) async fn run(
    context: &Context,
    directory: &Path,
    program: &str,
    args: &[&OsStr],
) -> Result<Output> {
    context.check()?;
    let context = context.with_timeout(Duration::from_secs(30));
    let mut command = Command::new(program);
    command
        .current_dir(directory)
        .args(args)
        .env_clear()
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    for key in [
        "PATH",
        "HOME",
        "USER",
        "LOGNAME",
        "SHELL",
        "LANG",
        "LC_ALL",
        "TMPDIR",
        "XDG_CONFIG_HOME",
        "SYSTEMROOT",
        "WINDIR",
    ] {
        if let Some(value) = std::env::var_os(key) {
            command.env(key, value);
        }
    }
    command
        .env("GIT_CONFIG_COUNT", "2")
        .env("GIT_CONFIG_KEY_0", "core.hooksPath")
        .env("GIT_CONFIG_VALUE_0", "/dev/null")
        .env("GIT_CONFIG_KEY_1", "core.fsmonitor")
        .env("GIT_CONFIG_VALUE_1", "false")
        .env("GIT_OPTIONAL_LOCKS", "0");
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        command.as_std_mut().process_group(0);
    }
    let mut child = command
        .spawn()
        .map_err(|_| failure("required workspace command is unavailable"))?;
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| failure("workspace stdout is unavailable"))?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| failure("workspace stderr is unavailable"))?;
    let mut reaped = false;
    let result = {
        // Drain both bounded pipes before reaping the child. During cancellation
        // its PID is still reserved, so signalling our isolated group cannot
        // target a reused PID belonging to another task.
        let work = async {
            let (stdout, stderr) = tokio::try_join!(bounded_read(stdout), bounded_read(stderr))?;
            let status = child
                .wait()
                .await
                .map_err(|_| failure("cannot join workspace command"))?;
            reaped = true;
            if !status.success() {
                return Err(failure(
                    "workspace command failed; ownership must be reconciled",
                ));
            }
            Ok(Output { stdout, stderr })
        };
        let deadline = async {
            match context.deadline() {
                Some(deadline) => tokio::time::sleep_until(deadline.into()).await,
                None => std::future::pending::<()>().await,
            }
        };
        tokio::select! {
            biased;
            () = context.cancellation_requested() => Err(Error::new("canceled","operation canceled")),
            () = deadline => Err(Error::new("timeout","workspace command deadline exceeded")),
            result = work => result,
        }
    };
    if !reaped {
        #[cfg(unix)]
        if let Some(pid) = child
            .id()
            .filter(|pid| *pid > 1)
            .and_then(|pid| i32::try_from(pid).ok())
            .and_then(rustix::process::Pid::from_raw)
        {
            // This is cancellation of the command tree created above, never
            // cleanup of a worker or signalling an inspected cwd holder.
            let _ = rustix::process::kill_process_group(pid, rustix::process::Signal::KILL);
        }
        let _ = child.start_kill();
        child
            .wait()
            .await
            .map_err(|_| failure("cannot join canceled workspace command"))?;
    }
    result
}

pub(crate) async fn git(context: &Context, directory: &Path, args: &[&OsStr]) -> Result<String> {
    let output = run(context, directory, "git", args).await?;
    // Successful Git progress/warnings do not become path/reference text.
    drop(output.stderr);
    let text = String::from_utf8(output.stdout)
        .map_err(|_| failure("workspace command returned invalid path/reference text"))?;
    Ok(text.trim().to_owned())
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;

    #[tokio::test]
    async fn command_output_is_bounded_and_diagnostics_are_not_exposed() {
        let root = tempfile::tempdir().unwrap();
        let output = run(
            &Context::background(),
            root.path(),
            "sh",
            &["-c".as_ref(), "printf 'out'; printf 'err' >&2".as_ref()],
        )
        .await
        .unwrap();
        assert_eq!(output.stdout, b"out");
        assert_eq!(output.stderr, b"err");
        for script in [
            "printf 'synthetic-secret' >&2; exit 1",
            "yes synthetic-secret",
            "yes synthetic-secret >&2",
        ] {
            let error = tokio::time::timeout(
                Duration::from_secs(5),
                run(
                    &Context::background(),
                    root.path(),
                    "sh",
                    &["-c".as_ref(), script.as_ref()],
                ),
            )
            .await
            .unwrap()
            .err()
            .unwrap();
            assert_eq!(error.code, "workspace_unavailable");
            assert!(!error.message.contains("synthetic-secret"));
        }
    }

    #[tokio::test]
    async fn canceled_command_group_is_joined_and_partial_effect_is_retained() {
        let root = tempfile::tempdir().unwrap();
        let marker = root.path().join("allocated");
        let late = root.path().join("late-effect");
        let directory = root.path().to_path_buf();
        let context = Context::background();
        let command_context = context.clone();
        let command = tokio::spawn(async move {
            run(
                &command_context,
                &directory,
                "sh",
                &[
                    "-c".as_ref(),
                    "printf 'owned' > allocated; (sleep 1; touch late-effect) & wait".as_ref(),
                ],
            )
            .await
        });
        tokio::time::timeout(Duration::from_secs(5), async {
            while !marker.exists() {
                tokio::time::sleep(Duration::from_millis(1)).await;
            }
        })
        .await
        .unwrap();
        context.cancel();
        let error = tokio::time::timeout(Duration::from_secs(5), command)
            .await
            .unwrap()
            .unwrap()
            .err()
            .unwrap();
        assert_eq!(error.code, "canceled");
        assert_eq!(std::fs::read_to_string(marker).unwrap(), "owned");
        tokio::time::sleep(Duration::from_millis(1200)).await;
        assert!(
            !late.exists(),
            "a canceled command descendant continued mutating"
        );
    }

    #[tokio::test]
    async fn deadline_stops_command_and_precanceled_context_starts_no_effect() {
        let root = tempfile::tempdir().unwrap();
        let context = Context::background().with_timeout(Duration::from_millis(50));
        let error = tokio::time::timeout(
            Duration::from_secs(5),
            run(
                &context,
                root.path(),
                "sh",
                &["-c".as_ref(), "sleep 30".as_ref()],
            ),
        )
        .await
        .unwrap()
        .err()
        .unwrap();
        assert_eq!(error.code, "timeout");
        let context = Context::background();
        context.cancel();
        let error = run(
            &context,
            root.path(),
            "sh",
            &["-c".as_ref(), "touch should-not-exist".as_ref()],
        )
        .await
        .err()
        .unwrap();
        assert_eq!(error.code, "canceled");
        assert!(!root.path().join("should-not-exist").exists());
    }
}
