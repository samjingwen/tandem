.PHONY: prepare format format-check lint test test-race check build build-core plugin-build security plugin-check prove-quality rust-prepare rust-format-check rust-lint rust-test rust-parity rust-safety rust-build rust-security python-prepare python-format python-format-check python-lint

RUST_AUDIT_VERSION := 0.22.2
RUST_AUDIT_ROOT := $(CURDIR)/.tmp/rust-tools
PYTHON := python3
PYTHON_TOOLS := $(CURDIR)/.tmp/python-tools
RUFF := $(PYTHON_TOOLS)/bin/ruff
# Only this driver is maintained Python; do not scan dependencies or fixtures.
PYTHON_SOURCES := tools/test-engine-safety.py

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
	$(MAKE) python-prepare

python-prepare:
	$(PYTHON) -m venv "$(PYTHON_TOOLS)"
	"$(PYTHON_TOOLS)/bin/python" -m pip install --disable-pip-version-check -r tools/python-requirements.txt

python-format:
	"$(RUFF)" check --config ruff.toml --fix $(PYTHON_SOURCES)
	"$(RUFF)" format --config ruff.toml $(PYTHON_SOURCES)

python-format-check:
	"$(RUFF)" format --config ruff.toml --check $(PYTHON_SOURCES)

python-lint: python-format-check
	"$(RUFF)" check --config ruff.toml $(PYTHON_SOURCES)

rust-prepare:
	cargo fetch --locked
	cargo install --locked --root "$(RUST_AUDIT_ROOT)" --version $(RUST_AUDIT_VERSION) cargo-audit

format:
	gofmt -w core internal cmd
	npm --prefix plugin run format
	cargo fmt --all
	$(MAKE) python-format

format-check:
	@test -z "$$(gofmt -l core internal cmd)" || (gofmt -l core internal cmd; exit 1)

rust-format-check:
	cargo fmt --all -- --check

lint: format-check rust-format-check python-lint
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
	cargo build --locked --example engine-compat
	TANDEM_RUST_STORE_HELPER="$(CURDIR)/target/debug/examples/store-compat" TANDEM_RUST_ENGINE_HELPER="$(CURDIR)/target/debug/examples/engine-compat" go test ./internal/store -run '^TestRust(Store|Engine)' -count=1
	$(MAKE) rust-safety

rust-safety:
	cargo build --locked -p tandem-engine --example engine-safety
	python3 tools/test-engine-safety.py "$(CURDIR)/target/debug/examples/engine-safety"

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
	RUFF="$(RUFF)" sh tools/prove-python-quality.sh

build: build-core plugin-build rust-build

rust-build:
	cargo build --workspace --release --locked

build-core:
	mkdir -p bin
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -o bin/tandem ./cmd/tandem

plugin-build:
	npm --prefix plugin run build
