use tandem_core::{Error, Operation, Project, RawJson, Store, Task};
use tandem_store::Sqlite;
use rusqlite::Connection;
use serde_json::json;

fn main() {
    let root = std::path::PathBuf::from(std::env::args_os().nth(1).unwrap());
    let store = Sqlite::open(&root).unwrap();
    let raw = r#"{"large":9007199254740993,"decimal":1.0000000000000000001,"answer":null,"text":"\u003c"}"#;
    let payload = "完整🙂<&>\u{2028}".repeat(100_000);
    store.update(&mut |s| {
        s.tasks.insert("task".into(), Task { id: "task".into(), title: "before".into(), instructions: payload.clone(), ..Default::default() });
        s.operations.insert("receipt".into(), Operation { id: "receipt".into(), fingerprint: "immutable-contract".into(), outcome: RawJson::parse(raw.into()).unwrap(), ..Default::default() });
        Ok(())
    }).unwrap();
    let before = store.snapshot().unwrap();
    let connection = Connection::open(root.join("tandem.db")).unwrap();
    let rows = || -> Vec<(i64,String,String,String)> {
        connection.prepare("SELECT revision,entity,id,body FROM history ORDER BY revision,entity,id").unwrap()
          .query_map([], |r| Ok((r.get(0)?,r.get(1)?,r.get(2)?,r.get(3)?))).unwrap().map(Result::unwrap).collect()
    };
    let original_history = rows();
    for mode in ["callback_error", "instance", "revision", "delete_task", "delete_receipt", "rewrite_receipt", "unique_path", "conflicting_extension"] {
        let error = store.update(&mut |s| {
            s.tasks.get_mut("task").unwrap().title = "must roll back".into();
            match mode {
                "callback_error" => return Err(Error::invalid("deliberate consumer abort")),
                "instance" => s.instance_id = "forged".into(),
                "revision" => s.revision += 1,
                "delete_task" => s.tasks.clear(),
                "delete_receipt" => s.operations.clear(),
                "rewrite_receipt" => s.operations.get_mut("receipt").unwrap().fingerprint = "forged".into(),
                "unique_path" => for id in ["a", "b"] { s.projects.insert(id.into(), Project { id: id.into(), path: "/synthetic/same".into(), ..Default::default() }); },
                "conflicting_extension" => { s.tasks.get_mut("task").unwrap().extra.insert("title".into(), json!("shadow")); },
                _ => unreachable!()
            }
            Ok(())
        }).unwrap_err();
        assert_eq!(store.snapshot().unwrap(), before);
        assert_eq!(rows(), original_history);
        println!("{}", json!({"scenario":mode,"error":error,"revision":before.revision,"history_rows":rows().len(),"rollback_state_and_history_unchanged":true}));
    }
    store.update(&mut |_| Ok(())).unwrap();
    assert_eq!(store.snapshot().unwrap(), before);
    assert_eq!(rows(), original_history);
    println!("{}",json!({"scenario":"quiet_noop","revision":before.revision,"history_rows":rows().len()}));
    std::thread::scope(|scope| {
        for _ in 0..8 { scope.spawn(|| { store.update(&mut |s| { s.tasks.get_mut("task").unwrap().generation += 1; Ok(()) }).unwrap(); }); }
    });
    let after = store.snapshot().unwrap();
    assert_eq!(after.revision,9);
    assert_eq!(after.tasks["task"].generation,8);
    assert_eq!(after.tasks["task"].instructions,payload);
    assert_eq!(after.operations["receipt"].outcome.as_str(),raw);
    assert_eq!(rows().len(),10);
    assert_eq!(rows().iter().filter(|r|r.1=="operations").count(),1);
    drop(connection);
    drop(store);
    let reopened = Sqlite::open(&root).unwrap();
    assert_eq!(reopened.snapshot().unwrap(),after);
    println!("{}",json!({"scenario":"concurrent_updates_and_reopen","revision":after.revision,"generation":after.tasks["task"].generation,"instructions_bytes":payload.len(),"receipt":after.operations["receipt"],"history_rows":10,"identity_stable":true}));
}
