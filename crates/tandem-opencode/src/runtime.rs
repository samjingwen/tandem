use std::{
    collections::BTreeMap,
    path::{Component, Path, PathBuf},
    sync::Arc,
};

use async_trait::async_trait;
use reqwest::Method;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tandem_core::{
    Context, Decision, Error, Observation, RawJson, Result, Runtime, RuntimeCapabilities,
    RuntimeInfo, SessionSpec,
};

use crate::{
    OpenCode,
    client::{Failure, WireResult, digest},
    ownership_error,
    policy::{Rule, permissions},
};

#[derive(Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
struct Owner {
    instance_id: String,
    task_id: String,
    attempt_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    command_id: String,
    directory: String,
    role: String,
}

#[derive(Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
struct Metadata {
    tandem: Owner,
}

#[derive(Default, Serialize, Deserialize)]
#[serde(default)]
struct Location {
    directory: String,
}

#[derive(Default, Deserialize)]
#[serde(default)]
struct Session {
    id: String,
    metadata: Metadata,
    location: Location,
    outcome: String,
    permissions: Vec<Rule>,
}

#[derive(Default, Deserialize)]
#[serde(default)]
struct Payload {
    text: String,
    metadata: Metadata,
}

#[derive(Default, Deserialize)]
#[serde(default)]
struct Admission {
    id: String,
    #[serde(rename = "sessionID")]
    session_id: String,
    #[serde(rename = "type")]
    kind: String,
    payload: Payload,
}

#[derive(Default, Deserialize)]
#[serde(default)]
struct Message {
    id: String,
    #[serde(rename = "type")]
    kind: String,
    text: String,
    metadata: Metadata,
}

fn validate(spec: &SessionSpec) -> Result<()> {
    let suffix = spec.reference.strip_prefix("ses_").unwrap_or_default();
    let path = Path::new(&spec.directory);
    let normalized: PathBuf = path.components().collect();
    let clean = normalized.as_os_str() == path.as_os_str()
        && !path
            .components()
            .any(|part| matches!(part, Component::ParentDir));
    if suffix.is_empty()
        || !suffix.bytes().all(|b| b.is_ascii_alphanumeric())
        || spec.task_id.is_empty()
        || spec.attempt_id.is_empty()
        || spec.instance_id.is_empty()
        || !path.is_absolute()
        || !clean
    {
        return Err(Error::invalid(
            "session requires a valid preallocated reference, ownership, and absolute directory",
        ));
    }
    if !matches!(spec.role.as_str(), "" | "worker" | "coordinator") {
        return Err(Error::invalid(
            "managed session role must be worker or coordinator",
        ));
    }
    Ok(())
}

fn owner(spec: &SessionSpec, command: &str) -> Metadata {
    Metadata {
        tandem: Owner {
            instance_id: spec.instance_id.clone(),
            task_id: spec.task_id.clone(),
            attempt_id: spec.attempt_id.clone(),
            command_id: command.into(),
            directory: spec.directory.clone(),
            role: if spec.role.is_empty() {
                "worker".into()
            } else {
                spec.role.clone()
            },
        },
    }
}

fn owned(session: &Session, spec: &SessionSpec) -> bool {
    session.id == spec.reference
        && session.metadata == owner(spec, "")
        && session.location.directory == spec.directory
        && session.permissions == permissions(spec)
}

fn session_path(spec: &SessionSpec) -> String {
    format!("/api/session/{}", spec.reference)
}

fn escaped_segment(value: &str) -> String {
    // Encode every non-unreserved UTF-8 byte, including slash, ?, # and %. Never
    // allow a native decision identifier to change the request path/query.
    let mut result = String::new();
    for b in value.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.' | b'~') {
            result.push(char::from(b));
        } else {
            result.push_str(&format!("%{b:02X}"));
        }
    }
    result
}

fn encoded(value: &impl Serialize) -> WireResult<Vec<u8>> {
    serde_json::to_vec(value)
        .map_err(|_| Error::not_delivered(Error::invalid("invalid OpenCode request body")).into())
}

fn verify_message(message: &Message, id: &str, text: &str, want: &Metadata) -> Result<()> {
    if message.id != id
        || message.kind != "user"
        || message.text != text
        || &message.metadata != want
    {
        return Err(ownership_error());
    }
    Ok(())
}

fn verify_admission(
    admission: &Admission,
    spec: &SessionSpec,
    id: &str,
    text: &str,
    want: &Metadata,
) -> Result<()> {
    if admission.session_id != spec.reference {
        return Err(ownership_error());
    }
    verify_message(
        &Message {
            id: admission.id.clone(),
            kind: admission.kind.clone(),
            text: admission.payload.text.clone(),
            metadata: admission.payload.metadata.clone(),
        },
        id,
        text,
        want,
    )
}

impl OpenCode {
    async fn owned(&self, context: &Context, spec: &SessionSpec) -> WireResult<()> {
        validate(spec)?;
        self.compatible(context).await?;
        let session = self.data::<Session>(context, &session_path(spec)).await?;
        if !owned(&session, spec) {
            return Err(ownership_error().into());
        }
        Ok(())
    }

    async fn ensure_inner(&self, context: &Context, spec: &SessionSpec) -> WireResult<()> {
        validate(spec)?;
        self.compatible(context).await?;
        match self.data::<Session>(context, &session_path(spec)).await {
            Ok(session) => {
                return if owned(&session, spec) {
                    Ok(())
                } else {
                    Err(ownership_error().into())
                };
            }
            Err(Failure {
                status: Some(404), ..
            }) => {}
            Err(error) => return Err(error),
        }
        #[derive(Deserialize)]
        struct Response {
            data: Option<Session>,
        }
        let mut body = json!({"id":spec.reference,"title":spec.title,"location":{"directory":spec.directory},"metadata":owner(spec,""),"permissions":permissions(spec)});
        if let Some(model) = &self.model {
            body["model"] = serde_json::to_value(model)
                .map_err(|_| Error::invalid("invalid OpenCode model"))?;
        }
        let result: Response = self
            .json(context, Method::POST, "/api/session", Some(encoded(&body)?))
            .await?;
        let session = result.data.ok_or_else(Error::uncertain)?;
        if !owned(&session, spec) {
            return Err(ownership_error().into());
        }
        Ok(())
    }

    async fn inspect_inner(
        &self,
        context: &Context,
        spec: &SessionSpec,
    ) -> WireResult<Observation> {
        let mut observation = Observation {
            state: "unknown".into(),
            ..Observation::default()
        };
        validate(spec)?;
        self.compatible(context).await?;
        let session = match self.data::<Session>(context, &session_path(spec)).await {
            Ok(session) => session,
            Err(Failure {
                status: Some(404), ..
            }) => return Ok(observation),
            Err(error) => return Err(error),
        };
        observation.exists = true;
        observation.owned = owned(&session, spec);
        if !observation.owned {
            return Ok(observation);
        }
        #[derive(Deserialize)]
        struct Active {
            #[serde(rename = "type", default)]
            kind: String,
        }
        let active: BTreeMap<String, Active> = self.data(context, "/api/session/active").await?;
        observation.state = match active.get(&spec.reference) {
            Some(state) if state.kind == "running" => "running",
            Some(_) => "unknown",
            None if session.outcome == "failed" => "failed",
            None => "idle",
        }
        .into();
        observation.decisions = self.decisions(context, spec).await?;
        Ok(observation)
    }

    async fn find_prompt(
        &self,
        context: &Context,
        spec: &SessionSpec,
        id: &str,
        text: &str,
        want: &Metadata,
    ) -> WireResult<bool> {
        for check in 0..2 {
            match self
                .data::<Message>(context, &format!("{}/message/{id}", session_path(spec)))
                .await
            {
                Ok(message) => {
                    verify_message(&message, id, text, want)?;
                    return Ok(true);
                }
                Err(Failure {
                    status: Some(404), ..
                }) => {}
                Err(error) => return Err(error),
            }
            if check == 1 {
                break;
            }
            for item in self
                .data::<Vec<Admission>>(context, &format!("{}/inbox", session_path(spec)))
                .await?
            {
                if item.id == id {
                    verify_admission(&item, spec, id, text, want)?;
                    return Ok(true);
                }
            }
        }
        Ok(false)
    }

    async fn reconcile_inner(
        &self,
        context: &Context,
        spec: &SessionSpec,
        command: &str,
        text: &str,
    ) -> WireResult<()> {
        if command.is_empty() || text.is_empty() {
            return Err(Error::invalid("prompt requires command ID and text").into());
        }
        self.owned(context, spec).await?;
        let id = format!("msg_{}", digest(&format!("tandem/prompt/{command}")));
        let want = owner(spec, command);
        if self.find_prompt(context, spec, &id, text, &want).await? {
            return Ok(());
        }
        Err(Error::uncertain().into())
    }

    async fn interrupt_inner(&self, context: &Context, spec: &SessionSpec) -> WireResult<()> {
        self.owned(context, spec).await?;
        #[derive(Deserialize)]
        struct Interrupted {
            interrupted: Option<bool>,
        }
        let result: Interrupted = self
            .json(
                context,
                Method::POST,
                &format!("{}/interrupt?resume=false", session_path(spec)),
                None,
            )
            .await?;
        if result.interrupted.is_none() {
            return Err(Error::uncertain().into());
        }
        Ok(())
    }

    async fn cancel_inner(&self, context: &Context, spec: &SessionSpec) -> WireResult<()> {
        self.interrupt_inner(context, spec).await?;
        for item in self
            .data::<Vec<Admission>>(context, &format!("{}/inbox", session_path(spec)))
            .await?
        {
            let command = &item.payload.metadata.tandem.command_id;
            if command.is_empty()
                || item.kind != "user"
                || item.session_id != spec.reference
                || item.payload.metadata != owner(spec, command)
                || item.id != format!("msg_{}", digest(&format!("tandem/prompt/{command}")))
            {
                continue;
            }
            self.request(
                context,
                Method::DELETE,
                &format!("{}/inbox/{}", session_path(spec), escaped_segment(&item.id)),
                None,
                false,
            )
            .await?;
        }
        Ok(())
    }

    async fn decisions(&self, context: &Context, spec: &SessionSpec) -> WireResult<Vec<Decision>> {
        let mut result = Vec::new();
        for kind in ["permission", "form"] {
            for raw in self
                .data::<Vec<RawJson>>(context, &format!("{}/{kind}", session_path(spec)))
                .await?
            {
                #[derive(Default, Deserialize)]
                #[serde(default)]
                struct Pending {
                    id: String,
                    #[serde(rename = "sessionID")]
                    session_id: String,
                    title: String,
                    action: String,
                }
                let pending: Pending = serde_json::from_str(raw.as_str())
                    .map_err(|_| Error::new("runtime_error", "invalid OpenCode decision"))?;
                if pending.id.is_empty() || pending.session_id != spec.reference {
                    return Err(ownership_error().into());
                }
                result.push(Decision {
                    task_id: spec.task_id.clone(),
                    attempt_id: spec.attempt_id.clone(),
                    runtime_ref: pending.id,
                    kind: kind.into(),
                    prompt: if kind == "permission" {
                        pending.action
                    } else {
                        pending.title
                    },
                    details: Some(raw),
                    status: "pending".into(),
                    ..Decision::default()
                });
            }
        }
        Ok(result)
    }

    async fn reply_inner(
        &self,
        context: &Context,
        spec: &SessionSpec,
        decision: &Decision,
        answer: &RawJson,
    ) -> WireResult<()> {
        if decision.task_id != spec.task_id
            || decision.attempt_id != spec.attempt_id
            || decision.runtime_ref.is_empty()
            || !matches!(decision.kind.as_str(), "permission" | "form")
        {
            return Err(Error::not_delivered(ownership_error()).into());
        }
        self.owned(context, spec)
            .await
            .map_err(|f| Failure::from(Error::not_delivered(f.error)))?;
        validate_answer(&decision.kind, answer)
            .map_err(|error| Failure::from(Error::not_delivered(error)))?;
        let pending = self
            .decisions(context, spec)
            .await
            .map_err(|f| Failure::from(Error::not_delivered(f.error)))?;
        for item in pending {
            if item.runtime_ref != decision.runtime_ref || item.kind != decision.kind {
                continue;
            }
            if let Some(before) = &decision.details
                && !same_request(before, item.details.as_ref().ok_or_else(ownership_error)?)
            {
                return Err(Error::uncertain().into());
            }
            return self
                .request(
                    context,
                    Method::POST,
                    &format!(
                        "{}/{}/{}/reply",
                        session_path(spec),
                        decision.kind,
                        escaped_segment(&decision.runtime_ref)
                    ),
                    Some(answer.as_str().as_bytes().to_vec()),
                    false,
                )
                .await
                .map(|_| ())
                .map_err(|failure| {
                    if matches!(failure.status, Some(404 | 409)) {
                        Failure::from(Error::uncertain())
                    } else if failure.rejected() {
                        Failure::from(Error::not_delivered(failure.error))
                    } else {
                        failure
                    }
                });
        }
        Err(Error::uncertain().into())
    }

    async fn quiescence_inner(&self, context: &Context, spec: &SessionSpec) -> WireResult<()> {
        self.owned(context, spec).await?;
        let observation = self.inspect_inner(context, spec).await?;
        if !observation.exists
            || !observation.owned
            || !matches!(observation.state.as_str(), "idle" | "failed")
        {
            return Err(Error::invalid("runtime execution is not quiescent").into());
        }
        if !observation.decisions.is_empty() {
            return Err(Error::invalid("runtime still owns pending permissions or forms").into());
        }
        #[derive(Default, Deserialize)]
        #[serde(default)]
        struct Resource {
            status: String,
            #[serde(rename = "sessionID")]
            session_id: String,
        }
        #[derive(Deserialize)]
        struct Inventory {
            #[serde(default)]
            location: Location,
            data: Option<Vec<Resource>>,
        }
        let mut query =
            reqwest::Url::parse("http://fixture.invalid/").map_err(|_| ownership_error())?;
        query
            .query_pairs_mut()
            .append_pair("location[directory]", &spec.directory);
        for path in ["/api/shell", "/api/pty"] {
            let result: Inventory = self
                .json(
                    context,
                    Method::GET,
                    &format!("{path}?{}", query.query().unwrap_or_default()),
                    None,
                )
                .await?;
            if result.location.directory != spec.directory || result.data.is_none() {
                return Err(ownership_error().into());
            }
            for resource in result.data.unwrap_or_default() {
                if resource.status == "running" {
                    return Err(Error::invalid(
                        "runtime location still owns a running shell or terminal",
                    )
                    .into());
                }
                if !matches!(resource.status.as_str(), "exited" | "killed" | "timeout") {
                    return Err(Error::uncertain().into());
                }
            }
        }
        #[derive(Default, Deserialize)]
        #[serde(default)]
        struct Capabilities {
            #[serde(rename = "persistentPty")]
            persistent_pty: bool,
        }
        #[derive(Deserialize)]
        struct Info {
            #[serde(default)]
            capabilities: Capabilities,
        }
        let info: Info = self.json(context, Method::GET, "/api/info", None).await?;
        if info.capabilities.persistent_pty {
            for resource in self
                .data::<Vec<Resource>>(
                    context,
                    &format!("/api/experimental/session/{}/terminal", spec.reference),
                )
                .await?
            {
                if resource.session_id != spec.reference {
                    return Err(ownership_error().into());
                }
                if resource.status == "running" {
                    return Err(
                        Error::invalid("runtime session still owns a persistent terminal").into(),
                    );
                }
                if resource.status != "exited" {
                    return Err(Error::uncertain().into());
                }
            }
        }
        Ok(())
    }
}

#[async_trait]
impl Runtime for OpenCode {
    fn reference(&self, attempt_id: &str) -> String {
        format!("ses_{}", digest(&format!("tandem/session/{attempt_id}")))
    }
    fn capabilities(&self) -> RuntimeCapabilities {
        RuntimeCapabilities {
            reconcile_prompt: true,
            cancel_pending: true,
            quiescence: true,
        }
    }
    async fn info(&self, context: &Context) -> Result<RuntimeInfo> {
        self.info_inner(context).await.map_err(|f| f.error)
    }
    async fn ensure(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        self.ensure_inner(context, spec).await.map_err(|f| f.error)
    }
    async fn inspect(&self, context: &Context, spec: &SessionSpec) -> Result<Observation> {
        self.inspect_inner(context, spec).await.map_err(|f| f.error)
    }
    async fn prompt(
        &self,
        context: &Context,
        spec: &SessionSpec,
        command: &str,
        text: &str,
    ) -> Result<()> {
        // Only failures before the POST or explicit deterministic rejection prove
        // non-delivery. Ownership/admission mismatches after sending do not.
        if command.is_empty() || text.is_empty() {
            return Err(Error::not_delivered(Error::invalid(
                "prompt requires command ID and text",
            )));
        }
        self.owned(context, spec)
            .await
            .map_err(|f| Error::not_delivered(f.error))?;
        let id = format!("msg_{}", digest(&format!("tandem/prompt/{command}")));
        let want = owner(spec, command);
        if self
            .find_prompt(context, spec, &id, text, &want)
            .await
            .map_err(|f| Error::not_delivered(f.error))?
        {
            return Ok(());
        }
        #[derive(Deserialize)]
        struct Response {
            data: Option<Admission>,
        }
        let body =
            encoded(&json!({"id":id,"text":text,"metadata":want,"delivery":"steer","resume":true}))
                .map_err(|f| f.error)?;
        let result: Response = self
            .json(
                context,
                Method::POST,
                &format!("{}/prompt", session_path(spec)),
                Some(body),
            )
            .await
            .map_err(|f| {
                if f.rejected() {
                    Error::not_delivered(f.error)
                } else {
                    f.error
                }
            })?;
        verify_admission(
            &result.data.ok_or_else(Error::uncertain)?,
            spec,
            &id,
            text,
            &want,
        )
    }
    async fn reconcile_prompt(
        &self,
        context: &Context,
        spec: &SessionSpec,
        command: &str,
        text: &str,
    ) -> Result<()> {
        self.reconcile_inner(context, spec, command, text)
            .await
            .map_err(|f| f.error)
    }
    async fn interrupt(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        self.interrupt_inner(context, spec)
            .await
            .map_err(|f| f.error)
    }
    async fn reply(
        &self,
        context: &Context,
        spec: &SessionSpec,
        decision: &Decision,
        answer: &RawJson,
    ) -> Result<()> {
        self.reply_inner(context, spec, decision, answer)
            .await
            .map_err(|f| f.error)
    }
    async fn cancel_pending(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        self.cancel_inner(context, spec).await.map_err(|f| f.error)
    }
    async fn check_quiescence(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        self.quiescence_inner(context, spec)
            .await
            .map_err(|f| f.error)
    }
    async fn events(&self, context: &Context, notify: Arc<dyn Fn() + Send + Sync>) -> Result<()> {
        self.events_inner(context, notify)
            .await
            .map_err(|f| f.error)
    }
}

fn same_request(before: &RawJson, after: &RawJson) -> bool {
    fn equal(left: &Value, right: &Value) -> bool {
        match (left, right) {
            (Value::Number(left), Value::Number(right)) => match (left.as_f64(), right.as_f64()) {
                (Some(l), Some(r)) => l.is_finite() && r.is_finite() && l == r,
                _ => false,
            },
            (Value::Array(l), Value::Array(r)) => {
                l.len() == r.len() && l.iter().zip(r).all(|(a, b)| equal(a, b))
            }
            (Value::Object(l), Value::Object(r)) => {
                l.len() == r.len()
                    && l.iter()
                        .all(|(key, value)| r.get(key).is_some_and(|other| equal(value, other)))
            }
            _ => left == right,
        }
    }
    match (
        serde_json::from_str::<Value>(before.as_str()),
        serde_json::from_str::<Value>(after.as_str()),
    ) {
        (Ok(l), Ok(r)) => equal(&l, &r),
        _ => false,
    }
}

fn validate_answer(kind: &str, raw: &RawJson) -> Result<()> {
    let parsed: Value = serde_json::from_str(raw.as_str())
        .map_err(|_| Error::invalid("decision reply must be a JSON object"))?;
    let fields = parsed
        .as_object()
        .ok_or_else(|| Error::invalid("decision reply must be a JSON object"))?;
    if kind == "permission" {
        if !matches!(
            fields.get("decision").and_then(Value::as_str),
            Some("once" | "always" | "reject")
        ) {
            return Err(Error::invalid(
                "permission requires decision: once, always, or reject",
            ));
        }
        if fields
            .iter()
            .any(|(key, value)| key != "decision" && (key != "message" || !value.is_string()))
        {
            return Err(Error::invalid("invalid permission reply field"));
        }
        return Ok(());
    }
    let answer = fields
        .get("answer")
        .and_then(Value::as_object)
        .filter(|_| fields.len() == 1)
        .ok_or_else(|| Error::invalid("form reply requires answer object with typed field keys"))?;
    for (key, value) in answer {
        let valid = match value {
            Value::String(_) | Value::Bool(_) => true,
            Value::Number(n) => n.as_f64().is_some_and(f64::is_finite),
            Value::Array(items) => items.iter().all(Value::is_string),
            _ => false,
        };
        if key.is_empty() || !valid {
            return Err(Error::invalid(
                "form values must be strings, numbers, booleans, or string arrays",
            ));
        }
    }
    Ok(())
}
