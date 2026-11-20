# ccsbx build. Every tunable is a make variable: `make GO=/path/to/go test`.
#   make vet     go vet + the zero-modules check (no go.sum allowed)
#   make test    vet + unit tests (HOME is a tmpdir) + the fence-test cleanup guard (stub docker)
#   make build   static binary into bin/ccsbx for this machine, plus a cross-compile check
#   make dist    linux/darwin x amd64/arm64 into dist/
#   make dist-linux   the installer's binaries (ccsbx ccsbx-root ccsbx-hostd ccsbx-signer, those whose cmd/ exists) for
#                linux/$(LINUX_ARCH) into dist/, with a SHA256SUMS that lists exactly them (install-linux.sh gate "bins")
#   make e2e     build, then e2e/run: one TAP stream in a scratch realm (e2e/run --help; skips are listed)
GO      ?= $(HOME)/.local/go/bin/go
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)
TARGETS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

LINUX_ARCH ?= $(shell $(GO) env GOARCH)
INSTALL_BINS = ccsbx ccsbx-root ccsbx-hostd ccsbx-signer

.PHONY: vet test build dist dist-linux e2e
vet:
	@test ! -e go.sum || (echo "go.sum present: ccsbx must have zero third-party modules" && exit 1)
	$(GO) vet ./...

test: vet
	HOME=$$(mktemp -d) $(GO) test ./...
	tests/fence-cleanup-test.sh

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ccsbx ./cmd/ccsbx
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ccsbx-hostd ./cmd/ccsbx-hostd
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/ccsbx-signer ./cmd/ccsbx-signer
	@for t in $(TARGETS); do \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) build -trimpath -ldflags "$(LDFLAGS)" ./... || exit 1; \
	done

dist:
	@for t in $(TARGETS); do \
	  CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ccsbx-$${t%/*}-$${t#*/} ./cmd/ccsbx || exit 1; \
	done

dist-linux:
	@mkdir -p dist
	@built=""; for b in $(INSTALL_BINS); do \
	  if [ -d cmd/$$b ]; then CGO_ENABLED=0 GOOS=linux GOARCH=$(LINUX_ARCH) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/$$b ./cmd/$$b || exit 1; built="$$built $$b"; \
	  else echo "dist-linux: cmd/$$b does not exist yet, so the installer's binaries gate will refuse (a file of that name left in dist/ is NOT listed)"; fi; \
	done; cd dist && sha256sum $$built > SHA256SUMS && cat SHA256SUMS
	@echo "dist-linux: SHA256SUMS lists only what this run built. It sits next to the binaries, so it guards against accidents, not against a writer of dist/: compare it with a build you trust (CI, a second machine) before 'install --yes'."

e2e: build
	CCSBX_BIN=$(CURDIR)/bin/ccsbx e2e/run
