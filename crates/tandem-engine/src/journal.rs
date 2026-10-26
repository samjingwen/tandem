//! Go-compatible operation fingerprint bytes. Typed field order and omission
//! belong to core input types; raw answers keep order/number spelling while Go's
//! compact/HTML-safe JSON encoding removes insignificant whitespace.

use serde::Serialize;
use sha2::{Digest, Sha256};
use tandem_core::{Error, Result};

pub fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

pub fn fingerprint<T: Serialize + ?Sized>(kind: &str, input: &T) -> Result<String> {
    let body = encode(input)?;
    let mut hash = Sha256::new();
    hash.update(kind.as_bytes());
    hash.update(b"\n");
    hash.update(body.as_bytes());
    Ok(hex(&hash.finalize()))
}

pub fn encode<T: Serialize + ?Sized>(value: &T) -> Result<String> {
    let source = serde_json::to_vec(value)
        .map_err(|_| Error::new("internal", "coordination JSON could not be encoded"))?;
    let mut output = Vec::with_capacity(source.len());
    let (mut quoted, mut escaped, mut index) = (false, false, 0);
    while index < source.len() {
        let byte = source[index];
        if quoted && escaped {
            output.push(byte);
            escaped = false;
        } else if quoted && byte == b'\\' {
            output.push(byte);
            escaped = true;
        } else if byte == b'"' {
            quoted = !quoted;
            output.push(byte);
        } else if quoted && matches!(byte, b'<' | b'>' | b'&') {
            output.extend_from_slice(match byte {
                b'<' => b"\\u003c",
                b'>' => b"\\u003e",
                _ => b"\\u0026",
            });
        } else if quoted
            && source
                .get(index..index + 3)
                .is_some_and(|bytes| bytes == [0xe2, 0x80, 0xa8] || bytes == [0xe2, 0x80, 0xa9])
        {
            output.extend_from_slice(if source[index + 2] == 0xa8 {
                b"\\u2028"
            } else {
                b"\\u2029"
            });
            index += 2;
        } else if quoted || !matches!(byte, b' ' | b'\n' | b'\r' | b'\t') {
            output.push(byte);
        }
        index += 1;
    }
    String::from_utf8(output)
        .map_err(|_| Error::new("internal", "coordination JSON contains invalid UTF-8"))
}

pub(crate) fn digest_prefix(value: &[u8]) -> String {
    hex(&Sha256::digest(value)[..16])
}
