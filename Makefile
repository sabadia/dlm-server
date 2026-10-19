# Developer convenience targets. CI invokes the underlying commands directly,
# so nothing here is required to build on any particular OS.
BINARY  ?= dlm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build generate generate-check docs fmt lint vet test race vuln tidy cross snapshot openapi clean

all: fmt lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/dlm

generate:
	go generate ./...

generate-check:
	sqlc diff -f internal/storage/sqlite/sqlc.yaml

# Regenerate the reference documents: the command reference and the OpenAPI
# document (a test fails when they are stale).
docs:
	go run ./cmd/dlm docs > docs/CLI.md
	go run ./cmd/dlm openapi > docs/openapi.json

fmt:
	golangci-lint fmt ./...

lint:
	golangci-lint run ./...

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

vuln:
	govulncheck ./...

tidy:
	go mod tidy

cross:
	@for os in linux darwin windows; do for arch in amd64 arm64; do \
		ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o bin/$(BINARY)-$$os-$$arch$$ext ./cmd/dlm || exit 1; \
	done; done

# Build every release artifact locally into dist/ without publishing.
snapshot:
	goreleaser release --snapshot --clean --skip=publish

# Print the OpenAPI document of this checkout.
openapi:
	go run -ldflags "$(LDFLAGS)" ./cmd/dlm openapi

clean:
	rm -rf bin dist completions release-assets coverage.*
