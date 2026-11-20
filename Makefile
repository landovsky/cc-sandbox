# ccsbx build. Every tunable is a make variable: `make GO=/path/to/go test`.
#   make vet     go vet + the zero-modules check (no go.sum allowed)
#   make test    vet + unit tests (HOME is a tmpdir) + the fence-test cleanup guard (stub docker)
#   make build   static binary into bin/ccsbx for this machine, plus a cross-compile check
#   make dist    linux/darwin x amd64/arm64 into dist/
#   make e2e     build, then e2e/run: one TAP stream in a scratch realm (e2e/run --help; skips are listed)
GO      ?= $(HOME)/.local/go/bin/go
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)
TARGETS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: vet test build dist e2e
vet:
	@test ! -e go.sum || (echo "go.sum present: ccsbx must have zero third-party modules" && exit 1)
	$(GO) vet ./...

test: vet
	HOME=$$(mktemp -d) $(GO) test ./...
	tests/fence-cleanup-test.sh

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ccsbx ./cmd/ccsbx
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ccsbx-signer ./cmd/ccsbx-signer
	@for t in $(TARGETS); do \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o /dev/null ./cmd/ccsbx || exit 1; \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o /dev/null ./cmd/ccsbx-signer || exit 1; \
	done

dist:
	@for t in $(TARGETS); do \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ccsbx-$${t%/*}-$${t#*/} ./cmd/ccsbx || exit 1; \
	done

e2e: build
	CCSBX_BIN=$(CURDIR)/bin/ccsbx e2e/run
