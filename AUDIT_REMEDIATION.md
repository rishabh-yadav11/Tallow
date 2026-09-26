# Audit remediation: behavior changes and measured evidence

This document records the behavior changes made while remediating the audit
findings, the evidence for each, and the mutations that were run against the
new tests. It is the operator-facing summary; the git history is the
authoritative record of how each change was reached.

Every coverage number below was measured with `go test -coverprofile` on the
committed tree, and every mutation result was produced by editing the source,
running the suite, and restoring from git. Mutations that did not apply to the
source (a pattern that matched nothing) are reported as NOT-APPLIED rather than
being silently counted as survivors.

## Behavior changes

### store: monotonic rollup watermarks (was: `started_at`)

Cost and usage rollups previously used the newest `requests.started_at` they
had seen as their watermark. `started_at` is not monotonic: rows are inserted as
requests complete, so a request that started before the watermark but finished
after it is backdated relative to the rollup and is never counted. Over time
the rollup silently under-reports, and the gap is invisible because nothing
errors.

Rollups now watermark on the monotonic ingestion sequence `requests.seq`.
Because a legacy database stored Unix-millisecond `started_at` values in the
watermark column, a table-rebuild migration rebases any such watermark onto
`MAX(seq)`. Without that step an upgraded database would keep comparing a
millisecond timestamp against sequence numbers, and would stop rolling up
entirely.

Operator-visible: cost and token totals now include late-completing requests.
Databases created before this change are migrated in place on open.

### secret: the in-memory keystore is not mutated before the write succeeds

`Set` and `Delete` applied the change to the in-memory map and then persisted.
If the write failed, two different bad things could happen, both silent:

- a failed `Set` left the new credential live in memory, so the process used a
  credential that was never durably stored, and a later successful save would
  resurrect it;
- a failed `Delete` stopped using a credential that was still on disk, so the
  next reload would bring it back.

Both now roll the in-memory state back when the write fails, and the rollback
is covered for overwrite and for a later save resurrecting a deleted key.

### budget: stale refunds can no longer inflate provider RPM headroom

`ProviderBudget.Release` (added for audit finding M5) forwards to
`FixedWindow.Refund`, whose rollover guard compared the refund timestamp only
against `resetAt`. A refund timestamped inside the current window's span but
originating from the *previous* window therefore decremented the current
window's count, even though the charge it was offsetting had already been reset
by the rollover. Every such refund manufactured headroom the provider never
paid for.

This is reachable, not theoretical. `router.go` refunds the provider slot with
`r.now()` evaluated at abort time, so any request admitted near a window
boundary and aborted just after it injected a free slot into the fresh window.
Repeated boundary crossings let a provider exceed its configured RPM.

`FixedWindow` now records the start of the current window, and `Refund` rejects
any refund that predates it, as well as any refund against a window that was
never established.

### app: the health loop tick is now a package var

`healthLoop` used a hardcoded 15s ticker, which made its scheduling
unobservable from a test. The interval is now a package var, mirroring the
existing `publishHook` seam in the same file. It remains 15s in production and
nothing writes to it.

## Coverage

| Package | Before | After |
| --- | --- | --- |
| `internal/store` | 41.3% | 79.2% |
| `internal/secret` | 78.9% | 85.7% |
| `internal/budget` | 72.9% | 98.8% |
| `internal/compact` | 66.7% | 90.5% |
| `internal/app` | 67.7% | 93.0% |

Notable per-function results: `budget.ProviderBudget` (all five methods,
including the M5 release path) went from 0% to covered; `budget.Refund` and
`compact.EstimateSize` from 0%; `app.probe` from 0% to 96.3%;
`app.healthLoop` from 20.7% to 100%.

## Mutation evidence

Each mutation is a real source edit, applied and verified to change the file
before the suite was run, then restored from git.

| Package | Control | Caught | Survivors |
| --- | --- | --- | --- |
| `store` | passed | 12/15 | 3, verified equivalent |
| `secret` | passed | 11/13 | 2, documented |
| `budget` | passed | 9/10 | 1, verified equivalent |
| `compact` | passed | 3/3 | 0 |
| `app` | passed | 10/11 | 1, fixed by a later commit |

A no-op control mutation was run for each package: an edit that changes the
source text without changing behavior, confirmed to survive. Without it, a
suite that passes for unrelated reasons would look like it caught everything.

### Survivors, and why

- **`budget`: deleting the zero-`startAt` check in `Refund`.** Verified
  equivalent, not a coverage gap. On a window that has never been charged the
  count is 0, so the `count > 0` clamp already refuses the decrement. The
  redundant branch was removed rather than left in place, and the test that
  pins the interaction (`TestRefundOnNeverUsedWindowIsANoOp`) documents why the
  clamp is the single load-bearing check.
- **`store`: 3 survivors**, each verified equivalent in the commit that
  introduced it.
- **`secret`: 2 survivors**, an unobservable fsync property and a redundant
  parse-error path, both documented at the point of use.
- **`app`: 1 survivor**, reported by the first mutation pass and then fixed:
  the probe tests asserted only `state != "open"`, which is equally true for a
  breaker with no recorded result at all, so dropping either the provider-level
  or the key-level record went unnoticed. The assertions now pin the exact
  state, and all four record calls are caught.

### Mutations that did not apply

An early compact mutation was reported NOT-APPLIED because the pattern assumed
the source read `len(s) <= max` when it in fact read `orig <= max`. The
mutation harness distinguishes this from a survivor and refuses to score it. The
existing exhaustive bound sweep (every `max` from 1 to 200 across five
multi-byte bodies) catches it once the correct pattern is used.

## Corrections made during the work

Three assertions I wrote were wrong about the codebase and were corrected
rather than worked around, because in each case the code was right and the test
was not:

- `EstimateSize` was documented as feeding a "trimmed N bytes" log line. It has
  no production caller and that log line was never wired up. The coverage is
  kept, but the comment now says plainly that the function is dead code, so
  the first caller does not rely on a false premise.
- `RecentRequests(0)` was expected to return nothing. The store clamps a
  non-positive limit to a default page. That clamp is defensive and
  unreachable from the admin socket, whose `limitParam` already maps
  `limit <= 0` to the default, so the test now pins the clamp and its bound.
- A health probe was expected to treat a 3xx as a failure. It treats any status
  below 400 as healthy. The security property that actually matters, that the
  provider credential is never sent to a redirect target, is asserted using the
  production HTTP client; the redirect refusal itself is already pinned
  end-to-end by `TestM2UpstreamClientEndToEnd`.

## Validation

Run against the committed tree:

- `gofmt -l .` — clean
- `go build ./...` — passes
- `go vet ./...` — passes
- `go test -count=1 ./...` — all packages pass
- `go test -race -count=1 ./...` — all packages pass
