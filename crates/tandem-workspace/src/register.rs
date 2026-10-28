use std::{
    fs,
    io::Read,
    path::{Path, PathBuf},
};

use tandem_core::{Context, Error, Project, RegisterProject, Result};

use crate::{failure, files, process};

const BUDGET: usize = 256 * 1024;

fn markdown(path: &Path) -> bool {
    path.extension()
        .and_then(|v| v.to_str())
        .is_some_and(|v| v.eq_ignore_ascii_case("md"))
        && path
            .file_name()
            .and_then(|v| v.to_str())
            .is_some_and(|v| !v.to_lowercase().starts_with(".env"))
}

fn load(context: &Context, path: &Path, bytes: &mut Vec<u8>) -> Result<()> {
    context.check()?;
    if !markdown(path) {
        return Err(Error::invalid(
            "instruction files must be Markdown, never private environment/authentication files",
        ));
    }
    let resolved = files::canonical(path)?;
    if !markdown(&resolved) {
        return Err(Error::invalid(
            "instruction files must resolve to Markdown, never private environment/authentication files",
        ));
    }
    let file = files::open_regular(context, &resolved)?;
    if file
        .metadata()
        .map_err(|_| failure("cannot inspect instruction size"))?
        .len()
        > BUDGET as u64
    {
        return Err(Error::invalid(
            "instruction file exceeds the instruction budget",
        ));
    }
    let mut contents = Vec::new();
    file.take((BUDGET + 1) as u64)
        .read_to_end(&mut contents)
        .map_err(|_| failure("cannot read instruction file"))?;
    context.check()?;
    let header = format!("\n\nInstructions from {}:\n", files::text(path)?);
    if bytes
        .len()
        .saturating_add(header.len())
        .saturating_add(contents.len())
        > BUDGET
    {
        return Err(Error::invalid(
            "instructions exceed the budget; select a bounded explicit project policy",
        ));
    }
    bytes.extend_from_slice(header.as_bytes());
    bytes.extend(contents);
    Ok(())
}

// Go's JSON string encoder replaces each malformed UTF-8 byte separately. Keep
// that observable project/receipt text rather than silently choosing a different
// replacement grouping for a legacy policy file.
fn go_text(mut bytes: &[u8]) -> String {
    let mut text = String::new();
    while !bytes.is_empty() {
        match std::str::from_utf8(bytes) {
            Ok(valid) => {
                text.push_str(valid);
                break;
            }
            Err(error) => {
                let valid = error.valid_up_to();
                text.push_str(std::str::from_utf8(&bytes[..valid]).unwrap_or_default());
                let count = error.error_len().unwrap_or(bytes.len() - valid);
                for _ in 0..count {
                    text.push('\u{fffd}');
                }
                bytes = &bytes[valid + count..];
            }
        }
    }
    text
}

fn policy(
    context: &Context,
    path: &Path,
    input: &RegisterProject,
) -> Result<(String, Vec<String>)> {
    let mut bytes = input.instructions.as_bytes().to_vec();
    if bytes.len() > BUDGET || input.instruction_files.len() > 16 {
        return Err(Error::invalid(
            "project policy exceeds the instruction budget or 16 explicit files",
        ));
    }
    let mut ancestors: Vec<_> = path.ancestors().collect();
    ancestors.reverse();
    for ancestor in ancestors {
        context.check()?;
        let instruction = ancestor.join("AGENTS.md");
        match fs::symlink_metadata(&instruction) {
            Ok(_) => load(context, &instruction, &mut bytes)?,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => return Err(failure("cannot inspect ancestor instructions")),
        }
    }
    let mut files = Vec::new();
    for explicit in &input.instruction_files {
        let file = Path::new(explicit);
        let file = if file.is_absolute() {
            file.to_path_buf()
        } else {
            path.join(file)
        };
        let file = files::absolute_clean(&file)?;
        load(context, &file, &mut bytes)?;
        files.push(files::text(&file)?);
    }
    context.check()?;
    Ok((go_text(&bytes), files))
}

pub(crate) async fn register(context: &Context, input: &RegisterProject) -> Result<Project> {
    let raw = PathBuf::from(&input.path);
    let canonical = files::blocking(context, move |context| {
        context.check()?;
        files::canonical(&files::absolute_clean(&raw)?)
    })
    .await?;
    let top = process::git(
        context,
        &canonical,
        &["rev-parse".as_ref(), "--show-toplevel".as_ref()],
    )
    .await?;
    if top != files::text(&canonical)? {
        return Err(Error::invalid(
            "register a repository root, not a subdirectory",
        ));
    }
    if input.base_ref.is_empty() {
        return Err(Error::invalid(
            "baseRef is required; select the intended branch explicitly",
        ));
    }
    let reference = format!("{}^{{commit}}", input.base_ref);
    process::git(
        context,
        &canonical,
        &[
            "rev-parse".as_ref(),
            "--verify".as_ref(),
            "--end-of-options".as_ref(),
            reference.as_ref(),
        ],
    )
    .await?;
    let directory = canonical.clone();
    let source = input.clone();
    let (instructions, instruction_files) = files::blocking(context, move |context| {
        policy(&context, &directory, &source)
    })
    .await?;
    Ok(Project {
        name: input.name.clone(),
        path: files::text(&canonical)?,
        base_ref: input.base_ref.clone(),
        instructions,
        instruction_files,
        ..Project::default()
    })
}
