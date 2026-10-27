use std::sync::Arc;

use reqwest::Method;
use serde::Deserialize;
use tandem_core::{Context, Error};

use crate::{
    OpenCode,
    client::{WireResult, within},
};

const LIMIT: usize = 1 << 20;

impl OpenCode {
    pub(crate) async fn events_inner(
        &self,
        context: &Context,
        notify: Arc<dyn Fn() + Send + Sync>,
    ) -> WireResult<()> {
        self.compatible(context).await?;
        within(context, async {
            let mut response = self.send(Method::GET, "/api/event", None, true).await?;
            if response.status().as_u16() != 200 {
                return Err(Error::new(
                    "runtime_http",
                    format!("OpenCode HTTP status {}", response.status().as_u16()),
                )
                .into());
            }
            let content_type = response
                .headers()
                .get("Content-Type")
                .and_then(|value| value.to_str().ok())
                .and_then(|value| value.parse::<mime::Mime>().ok());
            if content_type.is_none_or(|value| value.essence_str() != "text/event-stream") {
                return Err(
                    Error::new("runtime_error", "invalid OpenCode event content type").into(),
                );
            }
            let mut parser = Parser::default();
            while let Some(chunk) = response
                .chunk()
                .await
                .map_err(|_| Error::new("runtime_error", "OpenCode event stream failed"))?
            {
                for byte in chunk {
                    parser.push(byte, &notify)?;
                }
            }
            // Do not dispatch a partial final frame, replay or reconnect here.
            Err(Error::new("runtime_eof", "OpenCode event stream ended").into())
        })
        .await
    }
}

#[derive(Default)]
struct Parser {
    line: Vec<u8>,
    data: Vec<u8>,
}

impl Parser {
    fn push(&mut self, byte: u8, notify: &Arc<dyn Fn() + Send + Sync>) -> tandem_core::Result<()> {
        if byte != b'\n' {
            self.line.push(byte);
            if self.line.len() >= LIMIT {
                return Err(Error::new(
                    "runtime_error",
                    "OpenCode SSE line exceeds size limit",
                ));
            }
            return Ok(());
        }
        if self.line.last() == Some(&b'\r') {
            self.line.pop();
        }
        if self.line.is_empty() {
            if !self.data.is_empty() {
                #[derive(Deserialize)]
                struct Event {
                    #[serde(rename = "type")]
                    kind: String,
                }
                let event: Event = serde_json::from_slice(&self.data)
                    .map_err(|_| Error::new("runtime_error", "invalid OpenCode SSE event"))?;
                if event.kind.is_empty() {
                    return Err(Error::new("runtime_error", "invalid OpenCode SSE event"));
                }
                self.data.clear();
                notify();
            }
        } else if let Some(value) = self.line.strip_prefix(b"data:") {
            let value = value.strip_prefix(b" ").unwrap_or(value);
            if self
                .data
                .len()
                .saturating_add(value.len())
                .saturating_add(1)
                > LIMIT
            {
                return Err(Error::new(
                    "runtime_error",
                    "OpenCode SSE frame exceeds size limit",
                ));
            }
            self.data.extend_from_slice(value);
            self.data.push(b'\n');
        } else if self.line == b"data" {
            if self.data.len() == LIMIT {
                return Err(Error::new(
                    "runtime_error",
                    "OpenCode SSE frame exceeds size limit",
                ));
            }
            self.data.push(b'\n');
        }
        self.line.clear();
        Ok(())
    }
}
