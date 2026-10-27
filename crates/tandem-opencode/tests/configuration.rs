use tandem_opencode::{Model, OpenCode, Options};

#[test]
fn explicit_configuration_rejects_unsafe_endpoints_and_incomplete_models() {
    for url in [
        "",
        "ftp://localhost",
        "http://user:secret@localhost",
        "http://localhost?",
        "http://localhost?secret=value",
        "http://localhost#secret",
        "http://localhost/\n",
    ] {
        let result = OpenCode::new(Options {
            url: url.into(),
            password: "fixture-only".into(),
            model: None,
        });
        let error = result.err().expect("unsafe endpoint accepted");
        assert_eq!(error.code, "invalid_request");
        assert!(!error.message.contains("secret"));
    }
    assert!(
        OpenCode::new(Options {
            url: "http://127.0.0.1:1".into(),
            password: "fixture-only".into(),
            model: Some(Model {
                id: "".into(),
                provider_id: "provider".into(),
                variant: "".into()
            })
        })
        .is_err()
    );
}

#[test]
fn operator_model_references_retain_native_defaults_and_exact_identifiers() {
    assert_eq!(Model::parse("").unwrap(), None);
    assert_eq!(
        Model::parse("provider/model/path#variant").unwrap(),
        Some(Model {
            id: "model/path".into(),
            provider_id: "provider".into(),
            variant: "variant".into()
        })
    );
    for invalid in [
        "provider",
        "/model",
        "provider/",
        "provider/model#",
        "provider/model#a#b",
        "provider /model",
        "provider/model\u{0085}",
        "provider/model\0",
    ] {
        assert_eq!(Model::parse(invalid).unwrap_err().code, "invalid_request");
    }
}
