#!/bin/sh
# Exercise the actual consumers on isolated, disposable source, not text checks
# over configuration. Keep non-secret probe outputs for inspection in .tmp.
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$root/.tmp"
fixture=$(mktemp -d "$root/.tmp/rust-quality.XXXXXX")
mkdir "$fixture/src"
cat > "$fixture/Cargo.toml" <<'EOF'
[workspace]
[package]
name = "rust-quality-proof"
version = "0.0.0"
edition = "2024"
publish = false
EOF
cat > "$fixture/src/main.rs" <<'EOF'
fn main() {
    let values: Vec<_> = std::env::args().collect();
    println!("{}", values.is_empty());
}
EOF
export CARGO_TARGET_DIR="$fixture/target"
cargo generate-lockfile --manifest-path "$fixture/Cargo.toml"
cargo fmt --manifest-path "$fixture/Cargo.toml" -- --check > "$fixture/conforming-format.log" 2>&1
cargo clippy --manifest-path "$fixture/Cargo.toml" --locked -- -D warnings > "$fixture/conforming-clippy.log" 2>&1
cargo check --manifest-path "$fixture/Cargo.toml" --locked > "$fixture/conforming-compile.log" 2>&1

cat > "$fixture/src/main.rs" <<'EOF'
fn main(){let values = vec![1,2,3]; println!("{}", values.len() == 0);}
EOF
if cargo fmt --manifest-path "$fixture/Cargo.toml" -- --check > "$fixture/rejected-format.log" 2>&1; then
    echo "Rust formatter accepted an unformatted fixture" >&2
    exit 1
fi
cargo fmt --manifest-path "$fixture/Cargo.toml"
if cargo clippy --manifest-path "$fixture/Cargo.toml" --locked -- -D warnings > "$fixture/rejected-clippy.log" 2>&1; then
    echo "Clippy accepted a deliberate code-smell violation" >&2
    exit 1
fi
grep -q 'clippy::len_zero' "$fixture/rejected-clippy.log"
cat > "$fixture/src/main.rs" <<'EOF'
fn main() {
    let _: u8 = "not a number";
}
EOF
if cargo check --manifest-path "$fixture/Cargo.toml" --locked > "$fixture/rejected-compile.log" 2>&1; then
    echo "Rust compiler accepted a type error" >&2
    exit 1
fi
grep -q 'E0308' "$fixture/rejected-compile.log"
echo "Rust formatter, Clippy, and compiler rejected real violations; conforming consumers passed. Evidence: $fixture"
