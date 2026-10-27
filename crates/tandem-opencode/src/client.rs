use std::{future::Future, time::Duration};

use reqwest::{Client, Method, Response, Url};
use serde::de::DeserializeOwned;
use sha2::{Digest, Sha256};
use tandem_core::{Context, Error, Result, RuntimeInfo};

use crate::{Model, Options, VERSION};

pub(crate) const RESPONSE_LIMIT: usize = 8 << 20;

pub struct OpenCode {
    pub(crate) base: String,
    pub(crate) id: String,
    password: String,
    pub(crate) model: Option<Model>,
    client: Client,
}

pub(crate) struct Failure {
    pub error: Error,
    pub status: Option<u16>,
}

impl From<Error> for Failure {
    fn from(error: Error) -> Self {
        Self {
            error,
            status: None,
        }
    }
}

pub(crate) type WireResult<T> = std::result::Result<T, Failure>;

impl Failure {
    pub(crate) fn rejected(&self) -> bool {
        self.error.code != "uncertain"
            && (self.error.code == "invalid_request" || matches!(self.status, Some(401 | 403)))
    }
}

pub(crate) fn digest(value: &str) -> String {
    Sha256::digest(value.as_bytes())[..16]
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

impl OpenCode {
    pub fn new(options: Options) -> Result<Self> {
        let invalid = || {
            Error::invalid(
                "OpenCode URL must be an absolute HTTP(S) URL without credentials, query, or fragment",
            )
        };
        let parsed = Url::parse(&options.url).map_err(|_| invalid())?;
        if !matches!(parsed.scheme(), "http" | "https")
            || parsed.host_str().is_none()
            || !parsed.username().is_empty()
            || parsed.password().is_some()
            || options.url.contains('?')
            || parsed
                .fragment()
                .is_some_and(|fragment| !fragment.is_empty())
            || options.url.bytes().any(|byte| byte < 0x20 || byte == 0x7f)
        {
            return Err(invalid());
        }
        // Keep the Go URL identity, including an explicitly configured default
        // port. URL libraries otherwise erase that port during normalization.
        let raw = options.url.strip_suffix('#').unwrap_or(&options.url);
        let (scheme, rest) = raw.split_once("://").ok_or_else(invalid)?;
        let (host, path) = rest
            .split_once('/')
            .map_or((rest, String::new()), |(host, path)| {
                (host, format!("/{path}"))
            });
        if !matches!(scheme.to_ascii_lowercase().as_str(), "http" | "https")
            || host.contains('@')
            || host.is_empty()
            || host.contains('\\')
            || host.chars().any(char::is_whitespace)
        {
            return Err(invalid());
        }
        let path = go_endpoint_path(&path).ok_or_else(invalid)?;
        let base = format!(
            "{}://{}{path}",
            scheme.to_ascii_lowercase(),
            host.to_lowercase()
        );
        if options
            .model
            .as_ref()
            .is_some_and(|model| model.id.is_empty() || model.provider_id.is_empty())
        {
            return Err(Error::invalid("OpenCode model requires id and providerID"));
        }
        let client = Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .retry(reqwest::retry::never())
            .build()
            .map_err(|_| Error::new("runtime_error", "cannot construct OpenCode HTTP client"))?;
        let password = if options.password.is_empty() {
            std::env::var("OPENCODE_PASSWORD").unwrap_or_default()
        } else {
            options.password
        };
        Ok(Self {
            id: format!("opencode_{}", digest(&base)),
            base,
            password,
            model: options.model,
            client,
        })
    }

    pub(crate) async fn info_inner(&self, context: &Context) -> WireResult<RuntimeInfo> {
        #[derive(serde::Deserialize)]
        struct Info {
            #[serde(default)]
            version: String,
        }
        let response: Info = self.json(context, Method::GET, "/api/info", None).await?;
        let compatible = response.version == VERSION;
        Ok(RuntimeInfo {
            id: self.id.clone(),
            compatible,
            version: response.version,
            detail: if compatible {
                String::new()
            } else {
                format!("requires OpenCode {VERSION}")
            },
        })
    }

    pub(crate) async fn compatible(&self, context: &Context) -> WireResult<()> {
        if !self.info_inner(context).await?.compatible {
            return Err(Error::new("runtime_version", "incompatible OpenCode version").into());
        }
        Ok(())
    }

    pub(crate) async fn send(
        &self,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        events: bool,
    ) -> WireResult<Response> {
        let mut request = self
            .client
            .request(method, format!("{}{path}", self.base))
            .basic_auth("opencode", Some(&self.password))
            .header(
                "Accept",
                if events {
                    "text/event-stream"
                } else {
                    "application/json"
                },
            );
        if let Some(body) = body {
            request = request
                .header("Content-Type", "application/json")
                .body(body);
        }
        request
            .send()
            .await
            .map_err(|_| Error::new("runtime_error", "OpenCode request failed").into())
    }

    pub(crate) async fn request(
        &self,
        context: &Context,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        read: bool,
    ) -> WireResult<Vec<u8>> {
        let write = method != Method::GET;
        context.check().map_err(|error| {
            Failure::from(if write {
                Error::not_delivered(error)
            } else {
                error
            })
        })?;
        let context = context.with_timeout(Duration::from_secs(30));
        let result = within(&context, async {
            let mut response = self.send(method, path, body, false).await?;
            let status = response.status().as_u16();
            if !(200..300).contains(&status) {
                let error = if write && (!(400..500).contains(&status) || status == 408) {
                    Error::uncertain()
                } else if write && status == 400 {
                    Error::invalid("OpenCode rejected the request body")
                } else {
                    Error::new("runtime_http", format!("OpenCode HTTP status {status}"))
                };
                return Err(Failure {
                    error,
                    status: Some(status),
                });
            }
            if !read {
                return Ok(Vec::new());
            }
            let mut bytes = Vec::new();
            while let Some(chunk) = response.chunk().await.map_err(|_| {
                Failure::from(Error::new(
                    "runtime_error",
                    "invalid OpenCode JSON response",
                ))
            })? {
                if bytes.len().saturating_add(chunk.len()) > RESPONSE_LIMIT {
                    return Err(
                        Error::new("runtime_error", "invalid OpenCode JSON response").into(),
                    );
                }
                bytes.extend_from_slice(&chunk);
            }
            Ok(bytes)
        })
        .await;
        result.map_err(|failure| {
            if write && failure.status.is_none() {
                Failure::from(Error::uncertain())
            } else {
                failure
            }
        })
    }

    pub(crate) async fn json<T: DeserializeOwned>(
        &self,
        context: &Context,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
    ) -> WireResult<T> {
        let write = method != Method::GET;
        let bytes = self.request(context, method, path, body, true).await?;
        serde_json::from_slice(&bytes).map_err(|_| {
            Failure::from(if write {
                Error::uncertain()
            } else {
                Error::new("runtime_error", "invalid OpenCode JSON response")
            })
        })
    }

    pub(crate) async fn data<T: DeserializeOwned>(
        &self,
        context: &Context,
        path: &str,
    ) -> WireResult<T> {
        #[derive(serde::Deserialize)]
        struct Envelope<T> {
            data: Option<T>,
        }
        self.json::<Envelope<T>>(context, Method::GET, path, None)
            .await?
            .data
            .ok_or_else(|| Error::new("runtime_error", "missing OpenCode data envelope").into())
    }
}

/// Go's URL stores a decoded Path and an optional original RawPath. Trimming
/// decoded trailing slashes invalidates RawPath, so the remaining bytes must be
/// escaped canonically. Preserve a valid original spelling only when it still
/// denotes the unchanged path, including percent-escaped unreserved characters.
fn go_endpoint_path(raw: &str) -> Option<String> {
    fn hex(byte: u8) -> Option<u8> {
        match byte {
            b'0'..=b'9' => Some(byte - b'0'),
            b'a'..=b'f' => Some(byte - b'a' + 10),
            b'A'..=b'F' => Some(byte - b'A' + 10),
            _ => None,
        }
    }
    let input = raw.as_bytes();
    let mut decoded = Vec::new();
    let mut index = 0;
    while index < input.len() {
        if input[index] == b'%' {
            decoded.push(hex(*input.get(index + 1)?)? * 16 + hex(*input.get(index + 2)?)?);
            index += 3;
        } else {
            decoded.push(input[index]);
            index += 1;
        }
    }
    let original_len = decoded.len();
    while decoded.last() == Some(&b'/') {
        decoded.pop();
    }
    let unreserved =
        |byte: u8| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~');
    let valid_original = input
        .iter()
        .all(|&byte| unreserved(byte) || b"!$&'()*+,;=:@[]/%".contains(&byte));
    if decoded.len() == original_len && valid_original {
        return Some(raw.into());
    }
    let mut escaped = String::new();
    for byte in decoded {
        if unreserved(byte) || b"$&+,/:;=@".contains(&byte) {
            escaped.push(char::from(byte));
        } else {
            escaped.push_str(&format!("%{byte:02X}"));
        }
    }
    Some(escaped)
}

/// No cancel-on-drop ownership is introduced here. The engine retains the
/// effect; this adapter only stops the network wait when its context requests it.
pub(crate) async fn within<T>(
    context: &Context,
    work: impl Future<Output = WireResult<T>>,
) -> WireResult<T> {
    context.check()?;
    let deadline = async {
        match context.deadline() {
            Some(deadline) => tokio::time::sleep_until(deadline.into()).await,
            None => std::future::pending::<()>().await,
        }
    };
    tokio::select! {
        biased;
        () = context.cancellation_requested() => Err(Error::new("canceled", "operation canceled").into()),
        () = deadline => Err(Error::new("timeout", "operation deadline exceeded").into()),
        result = work => result,
    }
}
