use serde_json::{Value, json};
use tandem_core::{Decision, RawJson, State, Task, Timestamp};

#[test]
fn journal_is_not_part_of_public_state() {
    let mut state = State::default();
    state.operations.insert("op-1".into(), Default::default());
    let value = serde_json::to_value(state).unwrap();
    assert!(value.get("operations").is_none());
    assert_eq!(value["tasks"], json!({}));
}

#[test]
fn retained_json_keeps_numbers_escapes_null_and_precision() {
    let source = r#"{"large":9007199254740993,"decimal":1.0000000000000000001,"escaped":"\u003c\u2028","answer":null}"#;
    let raw = RawJson::parse(source.into()).unwrap();
    assert_eq!(serde_json::to_string(&raw).unwrap(), source);
    assert_eq!(raw.as_str(), source);
    assert!(RawJson::parse("{broken".into()).is_err());
}

#[test]
fn absent_and_explicit_null_decision_payloads_are_distinct() {
    let explicit: Decision = serde_json::from_value(json!({"details":null,"reply":null})).unwrap();
    assert_eq!(explicit.details.as_ref().unwrap().as_str(), "null");
    assert_eq!(explicit.reply.as_ref().unwrap().as_str(), "null");
    let absent = serde_json::to_value(Decision::default()).unwrap();
    assert!(absent.get("details").is_none());
    assert!(absent.get("reply").is_none());
    assert_eq!(
        serde_json::to_value(explicit).unwrap()["details"],
        Value::Null
    );
}

#[test]
fn persisted_extensibility_and_original_time_are_not_discarded() {
    let mut task: Task = serde_json::from_value(json!({
        "id":"tsk_1", "dependencies":[], "state":"future-observation",
        "createdAt":"2026-10-26T12:00:00.123456789+08:00",
        "futureField":{"list":[true,null,"世界"]}
    }))
    .unwrap();
    task.title = "changed".into();
    let encoded = serde_json::to_value(task).unwrap();
    assert_eq!(encoded["dependencies"], json!([]));
    assert_eq!(encoded["futureField"], json!({"list":[true,null,"世界"]}));
    assert_eq!(encoded["state"], "future-observation");
    assert_eq!(encoded["createdAt"], "2026-10-26T12:00:00.123456789+08:00");
    assert_eq!(Timestamp::default().0, "0001-01-01T00:00:00Z");
    let legacy: Task = serde_json::from_str(r#"{"dependencies":null}"#).unwrap();
    assert_eq!(
        serde_json::to_value(legacy).unwrap()["dependencies"],
        Value::Null
    );
}
