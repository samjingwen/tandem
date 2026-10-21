.PHONY: prepare format format-check lint test test-race check build build-core plugin-build security plugin-check prove-quality

# Explicit maintained-language ownership. Go's ./... can otherwise discover Go
# sources shipped inside npm dependencies after plugin installation.
GO_PACKAGES := ./cmd/... ./core/... ./internal/...

prepare:
	go mod download
	npm --prefix plugin ci --ignore-scripts

format:
	gofmt -w core internal cmd
	npm --prefix plugin run format

format-check:
	@test -z "$$(gofmt -l core internal cmd)" || (gofmt -l core internal cmd; exit 1)

lint: format-check
	go vet $(GO_PACKAGES)
	go tool staticcheck $(GO_PACKAGES)

test:
	go test $(GO_PACKAGES)

test-race:
	go test -race $(GO_PACKAGES)

plugin-check:
	npm --prefix plugin run check

check: lint test plugin-check

security:
	go tool govulncheck $(GO_PACKAGES)

prove-quality:
	sh tools/prove-quality.sh

build: build-core plugin-build

build-core:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/tandem ./cmd/tandem

plugin-build:
	npm --prefix plugin run build
