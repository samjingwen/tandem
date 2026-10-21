.PHONY: prepare format format-check lint test test-race check build security plugin-check

prepare:
	go mod download
	npm --prefix plugin ci --ignore-scripts

format:
	gofmt -w core internal cmd
	npm --prefix plugin run format

format-check:
	@test -z "$$(gofmt -l core internal cmd)" || (gofmt -l core internal cmd; exit 1)

lint: format-check
	go vet ./...
	go tool staticcheck ./...

test:
	go test ./...

test-race:
	go test -race ./...

plugin-check:
	npm --prefix plugin run check

check: lint test plugin-check

security:
	go tool govulncheck ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/tandem ./cmd/tandem
