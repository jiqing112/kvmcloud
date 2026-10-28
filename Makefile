BINARY_NAME   := kvmcloud
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0")
COMMIT        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
GO            ?= go
GOFLAGS       := -trimpath
LDFLAGS       := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

GOFILES       := $(shell find cmd internal -name '*.go')

.PHONY: all build control agent test coverage lint clean frontend release

all: build

build: control agent

control:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY_NAME)-control ./cmd/control

agent:
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY_NAME)-agent ./cmd/agent

frontend:
	cd web && npm ci && npm run build

test:
	$(GO) test ./... -count=1

coverage:
	$(GO) test ./... -count=1 -coverprofile=coverage.out
	$(GO) tool cover -func=coverage.out | tail -1
	$(GO) tool cover -html=coverage.out -o coverage.html

lint:
	$(GO) vet ./...

clean:
	rm -rf bin/ coverage.out coverage.html dist/

release:
	goreleaser release --snapshot --clean
