.PHONY: prepare format format-check lint test test-race check build build-core plugin-build security plugin-check prove-quality rust-prepare rust-format-check rust-lint rust-test rust-parity rust-build rust-security

RUST_AUDIT_VERSION := 0.22.2
RUST_AUDIT_ROOT := $(CURDIR)/.tmp/rust-tools

# Explicit maintained-language ownership. Go's ./... can otherwise discover Go
# sources shipped inside npm dependencies after plugin installation.
GO_PACKAGES := ./cmd/... ./core/... ./internal/...

# Race-instrumented large-content traversal can consume most of Go's default
# 10-minute package budget before the remaining regressions run. Bound the full
# suite separately from the unchanged fixture and production request deadlines.
GO_TEST_TIMEOUT := 20m

prepare:
	go mod download
	npm --prefix plugin ci --ignore-scripts
	$(MAKE) rust-prepare

rust-prepare:
	cargo fetch --locked
	cargo install --locked --root "$(RUST_AUDIT_ROOT)" --version $(RUST_AUDIT_VERSION) cargo-audit

format:
	gofmt -w core internal cmd
	npm --prefix plugin run format
	cargo fmt --all

format-check:
	@test -z "$$(gofmt -l core internal cmd)" || (gofmt -l core internal cmd; exit 1)

rust-format-check:
	cargo fmt --all -- --check

lint: format-check rust-format-check
	go vet $(GO_PACKAGES)
	go tool staticcheck $(GO_PACKAGES)
	$(MAKE) rust-lint

rust-lint:
	cargo clippy --workspace --all-targets --locked -- -D warnings

test:
	go test -timeout=$(GO_TEST_TIMEOUT) $(GO_PACKAGES)
	$(MAKE) rust-test rust-parity

rust-test:
	cargo test --workspace --all-targets --locked

rust-parity:
	cargo build --locked --example store-compat
	TANDEM_RUST_STORE_HELPER="$(CURDIR)/target/debug/examples/store-compat" go test ./internal/store -run '^TestRustStore' -count=1

test-race:
	go test -race -timeout=$(GO_TEST_TIMEOUT) $(GO_PACKAGES)

plugin-check:
	npm --prefix plugin run check

check: lint test plugin-check

security:
	go tool govulncheck $(GO_PACKAGES)
	$(MAKE) rust-security

rust-security:
	"$(RUST_AUDIT_ROOT)/bin/cargo-audit" audit --deny warnings --file Cargo.lock

prove-quality:
	sh tools/prove-quality.sh
	sh tools/prove-rust-quality.sh

build: build-core plugin-build rust-build

rust-build:
	cargo build --workspace --release --locked

build-core:
	mkdir -p bin
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -o bin/tandem ./cmd/tandem

plugin-build:
	npm --prefix plugin run build
