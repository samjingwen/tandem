use serde::{Deserialize, Serialize};
use tandem_core::SessionSpec;

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub(crate) struct Rule {
    pub action: String,
    pub resource: String,
    pub effect: String,
}

// Exact ordered 2.1.0 policy. The unchanged plugin additionally guards canonical
// paths and native tool entrypoints; neither layer is an OS sandbox.
pub(crate) fn permissions(spec: &SessionSpec) -> Vec<Rule> {
    let mut rules = Vec::new();
    let mut add = |action: &str, resource: &str, effect: &str| {
        rules.push(Rule {
            action: action.into(),
            resource: resource.into(),
            effect: effect.into(),
        })
    };
    for (action, resource, effect) in [
        ("*", "*", "deny"),
        ("execute", "*", "allow"),
        ("read", "*", "allow"),
        ("read", "*.env", "deny"),
        ("read", "*.env.*", "deny"),
        ("read", "*.env.example", "allow"),
        ("glob", "*", "allow"),
        ("grep", "*", "ask"),
        ("question", "*", "allow"),
        ("shell", "*", "ask"),
        ("external_directory", "*", "deny"),
        ("subagent", "*", "deny"),
        ("opencode_*", "*", "deny"),
    ] {
        add(action, resource, effect);
    }
    for action in [
        "tandem_create",
        "tandem_status",
        "tandem_followup",
        "tandem_decision",
        "tandem_result",
        "tandem_control",
    ] {
        add(action, "*", "allow");
    }
    if spec.role == "coordinator" {
        add("edit", "*", "deny");
        add("tandem.permission.reply", "*", "ask");
    } else {
        for (resource, effect) in [
            ("*", "allow"),
            ("*.env", "deny"),
            ("*.env.*", "deny"),
            ("*.env.example", "allow"),
        ] {
            add("edit", resource, effect);
        }
    }
    for action in ["read", "edit"] {
        if action == "edit" && spec.role == "coordinator" {
            continue;
        }
        for resource in ["..", "../*", "/*", "?:/*"] {
            add(action, resource, "deny");
        }
    }
    rules
}
