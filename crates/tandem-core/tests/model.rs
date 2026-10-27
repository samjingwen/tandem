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

#[test]
fn timestamp_validation_keeps_spelling_and_go_null_semantics() {
    for text in [
        "0001-01-01T00:00:00Z",
        "2026-10-26T03:04:05.123456789+00:00",
        "2026-10-26T12:00:00+08:00",
        "9999-12-31T23:59:59-00:01",
        "9999-12-31T23:59:59-24:60",
        "0000-01-01T00:00:00+00:01",
        "0000-01-01T00:00:00+24:60",
    ] {
        let encoded = serde_json::to_string(text).unwrap();
        let timestamp: Timestamp = serde_json::from_str(&encoded).unwrap();
        assert_eq!(timestamp.0, text);
    }
    assert!(serde_json::from_str::<Timestamp>(r#""not-a-timestamp""#).is_err());
    assert_eq!(
        serde_json::from_str::<Timestamp>("null").unwrap(),
        Timestamp::default()
    );
    let earlier = Timestamp("2026-10-26T08:00:00+08:00".into());
    let later = Timestamp("2026-10-26T01:00:00Z".into());
    assert!(earlier.instant().unwrap() < later.instant().unwrap());
    for (earlier, later) in [
        ("9999-12-31T23:59:59Z", "9999-12-31T23:59:59-00:01"),
        ("0000-01-01T00:00:00+00:01", "0000-01-01T00:00:00Z"),
        ("0000-01-01T00:00:00+24:60", "0000-01-01T00:00:00+00:01"),
        ("9999-12-31T23:59:59-00:01", "9999-12-31T23:59:59-24:60"),
    ] {
        assert!(
            Timestamp(earlier.into()).instant().unwrap()
                < Timestamp(later.into()).instant().unwrap()
        );
    }
    for raw in [
        r#""2026-10-26T03:04:05\u005a""#,
        r#""\u0032026-10-26T03:04:05Z""#,
        r#""2026-10-26\u005403:04:05Z""#,
    ] {
        assert!(serde_json::from_str::<Timestamp>(raw).is_err());
    }
}

#[test]
fn input_answers_distinguish_missing_from_explicit_null_and_evidence_is_strict() {
    let missing: tandem_core::ReplyDecision = serde_json::from_str("{}").unwrap();
    let null: tandem_core::ReplyDecision = serde_json::from_str(r#"{"answer":null}"#).unwrap();
    assert!(missing.answer.is_none());
    assert_eq!(null.answer.unwrap().as_str(), "null");
    assert!(serde_json::from_str::<tandem_core::SubmitResult>(r#"{"evidence":[{"name":"test","reference":"fixture://test","passed":true,"unexpected":true}]}"#).is_err());
}

#[test]
fn legacy_go_time_spellings_retain_text_and_order_by_the_same_instant() {
    for (source, canonical) in [
        ("2026-10-26T3:04:05,123Z", "2026-10-26T03:04:05.123Z"),
        ("2026-10-26T03:04:05+24:00", "2026-10-25T03:04:05Z"),
        ("2026-10-26T03:04:05+00:60", "2026-10-26T02:04:05Z"),
        (
            "2026-10-26T03:04:05.123456789012Z",
            "2026-10-26T03:04:05.123456789Z",
        ),
    ] {
        let timestamp: Timestamp =
            serde_json::from_str(&serde_json::to_string(source).unwrap()).unwrap();
        assert_eq!(timestamp.0, source);
        assert_eq!(
            timestamp.instant().unwrap(),
            Timestamp(canonical.into()).instant().unwrap()
        );
    }
    assert!(serde_json::from_str::<Timestamp>(r#""2026-10-26t03:04:05z""#).is_err());
    assert!(serde_json::from_str::<Timestamp>(r#""2026-10-26T03:04:60Z""#).is_err());
}
