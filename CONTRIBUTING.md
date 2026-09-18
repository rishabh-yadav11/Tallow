# Contributing to Tallow

Thanks for your interest in contributing!

## Building and testing

```sh
make            # fmt-check + vet + build + test
make test-race  # tests with the race detector
make fmt        # gofmt all files
```

Tallow must build as a pure-Go static binary:

```sh
CGO_ENABLED=0 go build -o tallow ./cmd/tallow
```

Do not introduce C dependencies (including cgo SQLite bindings); the SQLite
driver is the pure-Go `modernc.org/sqlite` on purpose.

## Before you open a PR

1. `make` passes (fmt-check, vet, build, test).
2. New behavior has tests. E2E-style tests live in `internal/app`.
3. `gofmt` is clean (the Makefile gate enforces it).
4. No new dependencies without a strong reason; every dependency ships in a
   single static binary and affects idle memory.

## Commit conventions

Use Conventional Commits style (`fix:`, `feat:`, `test:`, `docs:`, `chore:`,
`ci:`) as the existing history does.

## Design constraints to respect

- Low idle memory and low per-request CPU are hard constraints. Prefer
  fixed-window counters, in-process caches with hard bounds, and streaming
  passthrough.
- Provider keys must never be written to disk in plaintext; the keystore is
  AES-256-GCM sealed under the master key.
- The admin TUI talks only to the stable Unix-socket admin interface, never to
  gateway internals.

## Reporting bugs and security issues

- Bugs and feature requests: open a GitHub issue.
- Security issues: **do not** open a public issue; see [SECURITY.md](SECURITY.md).
