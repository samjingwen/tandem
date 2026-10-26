//! Disposable test driver used by Go/Rust compatibility tests, not a service or
//! production CLI. It opens only the explicit test-owned directory supplied by
//! the harness and never discovers services, state paths, or credentials.

use std::io::{self, BufRead, Write};

use serde::Serialize;
use serde_json::json;
use tandem_core::{Error, Operation, RawJson, Result, Store, Task};
use tandem_store::Sqlite;

fn run() -> Result<()> {
    let args: Vec<_> = std::env::args_os().skip(1).collect();
    if args.len() != 2 {
        return Err(Error::invalid("expected fixture mode and owned directory"));
    }
    let mode = args[0]
        .to_str()
        .ok_or_else(|| Error::invalid("invalid fixture mode"))?;
    if !["snapshot", "noop", "update", "seed", "hold", "seed-hold"].contains(&mode) {
        return Err(Error::invalid("unknown fixture mode"));
    }
    let store = Sqlite::open(&args[1])?;
    match mode {
        "seed" | "seed-hold" => store.update(&mut |state| {
            if !state.tasks.is_empty() {
                return Err(Error::invalid("seed requires a fresh fixture store"));
            }
            state.tasks.insert(
                "tsk_rust".into(),
                Task {
                    id: "tsk_rust".into(),
                    title: "Rust created 世界".into(),
                    dependencies: Some(vec![]),
                    generation: 7,
                    ..Default::default()
                },
            );
            state.operations.insert(
                "rust-operation".into(),
                Operation {
                    id: "rust-operation".into(),
                    fingerprint: "synthetic-rust-fingerprint".into(),
                    outcome: RawJson::parse(
                        r#"{"value":9007199254740993,"answer":null,"text":"\u003c"}"#.into(),
                    )
                    .map_err(|_| Error::invalid("invalid fixture outcome"))?,
                    ..Default::default()
                },
            );
            Ok(())
        })?,
        "update" => store.update(&mut |state| {
            let task = state
                .tasks
                .get_mut("tsk_go")
                .ok_or_else(|| Error::invalid("fixture task absent"))?;
            task.title = "written-by-rust 世界".into();
            task.generation += 1;
            Ok(())
        })?,
        "noop" => store.update(&mut |_| Ok(()))?,
        _ => (),
    }
    if mode.ends_with("hold") {
        println!("{{\"locked\":true}}");
        io::stdout()
            .flush()
            .map_err(|_| Error::invalid("fixture receipt failed"))?;
        let mut line = String::new();
        io::stdin()
            .lock()
            .read_line(&mut line)
            .map_err(|_| Error::invalid("fixture lease failed"))?;
    } else {
        let state = store.snapshot()?;
        #[derive(Serialize)]
        struct Export<'a> {
            state: &'a tandem_core::State,
            operations: &'a std::collections::BTreeMap<String, Operation>,
        }
        let output = Export {
            state: &state,
            operations: &state.operations,
        };
        serde_json::to_writer(io::stdout().lock(), &output)
            .map_err(|_| Error::invalid("fixture output failed"))?;
        println!();
    }
    Ok(())
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{}", json!({"error":error}));
        std::process::exit(1);
    }
}
