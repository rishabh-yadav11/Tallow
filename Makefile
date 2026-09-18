GO ?= go
PKGS := ./...

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build release test test-race vet fmt fmt-check tidy clean

all: fmt-check vet build test

build:
	$(GO) build ./...

# Static release binaries with the version stamped from git tags.
# Pure Go on purpose: CGO_ENABLED=0.
release:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o tallow ./cmd/tallow
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o tallowctl ./cmd/tallowctl

test:
	$(GO) test ./...

# Race-enabled suite with a repeat to shake out flakes. CI uses this.
test-race:
	$(GO) test -race -count=1 $(PKGS)

vet:
	$(GO) vet $(PKGS)

fmt:
	$(GO) fmt $(PKGS)

# Fails if any file needs formatting (used by CI).
fmt-check:
	@out="$$($(GO) fmt $(PKGS) 2>&1)"; \
	if [ -n "$$out" ]; then \
		echo "These files need gofmt:"; echo "$$out"; exit 1; \
	fi

tidy:
	$(GO) mod tidy

# Optional deep SAST gates (need network/install). Not required for `make all`.
vet-govulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

vet-lint:
	$(GO) run github.com/golangci/golangci-lint/cmd/golangci-lint@latest run ./...

vet-gosec:
	$(GO) run github.com/securego/gosec/v2/cmd/gosec@latest ./...

clean:
	$(GO) clean ./...
