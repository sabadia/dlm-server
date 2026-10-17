# Developer convenience targets. CI invokes the underlying commands directly,
# so nothing here is required to build on any particular OS.
BINARY  ?= dlm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build fmt lint vet test race vuln tidy cross clean

all: fmt lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/dlm

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

clean:
	rm -rf bin dist coverage.*
