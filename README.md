# Tallow

A self-hosted, OpenAI-compatible **LLM gateway** in one static Go binary. Point any
tool that already speaks the OpenAI API (coding agents, scripts, IDE plugins) at
Tallow with a base URL + API key, and Tallow routes each request across the pool of
provider accounts and keys **you** already hold — with budget enforcement,
failover, caching, and cost tracking.

Tallow runs continuously on a daily-driver machine, so **low idle memory and low
per-request CPU are hard constraints**, not afterthoughts. Every mechanism was
chosen with that in mind (fixed-window counters, in-process caching, streaming
passthrough, pure-Go static binary).

---

## What it does

- Any OpenAI-compatible endpoint is a "provider" — just a base URL + key. No
  hardcoded vendors.
- Full chat-completions surface: streaming (SSE), function/tool calling, and
  multi-turn conversations, not just single-turn completions.
- **Model-alias routing** — clients ask for an alias (e.g. `deepseek-v4-flash`);
  Tallow picks the actual provider/key. The client never selects an endpoint.
- Per-key and per-provider budgets, hard-limited (RPM, request windows, cost).
- Proactive health checks (circuit-breaker style) + ordered fallback.
- Two caching mechanisms: a normalized exact-match cache, and provider-native
  prompt-cache passthrough via sticky routing.
- SQLite (WAL) cost/error tracking with **tiered retention** and indefinite rollups.
- Encrypted-at-rest provider keys (never plaintext on disk).
- Hot-reload (add/remove providers/keys/aliases without restart).
- A decoupled **TUI** dashboard (`tallowctl`) that talks only to a stable admin
  interface — a web UI could be added later without touching gateway internals.

---

## Quick start

```sh
# Build (pure Go, no C toolchain)
CGO_ENABLED=0 go build -o tallow ./cmd/tallow
CGO_ENABLED=0 go build -o tallowctl ./cmd/tallowctl

# 1. Set a master key (env, or a 0600 keyfile). It encrypts provider keys at rest.
export TALLOW_MASTER_KEY="$(openssl rand -hex 32)"

# 2. Copy + edit the config
cp examples/config.toml ./config.toml

# 3. Register a provider key (encrypted, never plaintext on disk)
TALLOW_PROVIDER_KEY="sk-..." tallowctl key add myprovider:account-1 --config ./config.toml \
  --secret-env TALLOW_PROVIDER_KEY

# 4. Run the gateway
./tallow --config ./config.toml

# 5. Point any OpenAI-compatible client at it
#    base_url = http://127.0.0.1:8080, api_key = one of `auth.api_keys`
```

Open the dashboard in another terminal: `tallowctl dashboard`.

[![CI](https://github.com/rishabh-yadav11/Tallow/actions/workflows/ci.yml/badge.svg)](https://github.com/rishabh-yadav11/Tallow/actions/workflows/ci.yml)

---

## Architecture

```
                 ┌──────────────────────────  tallow (one static binary) ──────────────────────────┐
OpenAI client ──►│ HTTP /v1/chat/completions                                                        │
                 │   │ auth · concurrency queue                                                     │
                 │   ▼                                                                              │
                 │  compaction (tool-output truncation)                                             │
                 │   ▼                                                                              │
                 │  exact-match cache      ── hit ──► serve                                         │
                 │   ▼ miss                                                          ┌───────────┐  │
                 │  routing (alias → provider → key)  ── budget / health ──►        │ registry  │  │
                 │   ▼  (sticky per session)                                        │ (hot-swap)│  │
                 │  upstream forward (SSE passthrough, first-token buffer)          └───────────┘  │
                 │   ▼                                                                              │
                 │  metadata / raw bodies / errors ──► SQLite (WAL) ──► rollups (indefinite)         │
                 │                                                                                  │
                 │  admin HTTP over Unix socket ──► metrics · config · budgets · logs               │
                 └──────────────────────────────────────────────────────────────────────────────────┘
                          ▲ Unix socket
                 ┌────────┴────────┐
                 │  tallowctl TUI  │  (first frontend; any future UI hits the same socket)
                 └─────────────────┘
```

The gateway core exposes **state and controls** (metrics, live budgets, logs,
config) over a stable minimal interface — an HTTP API on a local Unix socket. The
TUI is built strictly against that interface and never reaches into internals, so a
web GUI is possible later without changing the core.

---

## Routing: two layers + sticky

A request names an **alias** (the client's `model` field). Resolution is two-layer:

1. **Provider layer** — which provider serves this alias. An alias maps to an
   *ordered* list of `(provider, upstream-model)` targets. That order **is** the
   fallback order. A provider may additionally declare a `fallback` chain, which is
   spliced into the same ordered list with the same upstream model string.
2. **Key layer** — within the chosen provider, which account/key to use. Keys are
   round-robin among those currently healthy and under budget (least-loaded-first
   isn't needed at personal scale; rotation is deterministic and cheap).

**Sticky routing**: once a `(provider, key)` is chosen for a session, it is pinned
for the session's remainder (idle-expiring). This is what makes *provider-side
prompt caching* actually pay off — repeated requests with the same prefix keep
hitting the same provider/key, so the provider's own cache is reused.

Session identity:
- An `X-Session-Id` request header, when present, scopes stickiness explicitly.
- Otherwise, affinity is `(client bearer token, alias)` — so two concurrent
  "sessions" from the same client key on the same alias share a pin (documented
  limitation; use `X-Session-Id` to separate them).

**Capabilities are manual.** The user declares context window, tool support, etc.
per `(alias, provider)` target. Tallow never auto-detects them.

---

## Caching: exactly two mechanisms

No semantic/embedding caching, no near-duplicate hashing (SimHash/MinHash) — both
were evaluated and rejected for compute cost.

**1. Normalized exact-match cache.** Before hashing, Tallow strips volatile fields
(`timestamp`, `request_id`, `session_id`, `metadata`, `user`, `stream`) and
canonicalizes JSON key order (deterministic sorted-map marshaling). The SHA-256 of
the result is the cache key. The key therefore includes `model`, `temperature`,
`tools`/`functions`, `messages`, and every other semantic field — not just message
text. Identical *non-streaming* requests are served from cache.

> Streaming requests are deliberately **not** cached: replaying a stream faithfully
> would require buffering it, which violates the streaming-passthrough constraint.
> Repeat *streaming* requests are instead served cheaply by provider-native prompt
> caching via sticky routing (mechanism 2). This is the split between the two
> mechanisms.

The cache is in-process (bounded LRU + TTL). No Redis, no vector DB.

**2. Provider-native prompt-cache passthrough.** Tallow forwards
`cache_control`/`cache-control` markers through unchanged (it mutates only the
`model` field when rewriting an alias to its upstream name), and relies on sticky
routing so repeated same-prefix requests reach the same provider/key.

Plus a **tool-output compaction** stage — a *separate* pipeline step from response
caching — that truncates `role: "tool"` output before it reaches the model
(inspired by rtk-ai's compaction).

---

## Retry policy (exact, deliberate)

- Buffer only until the **first token** arrives from upstream.
- **Failure before the first token** → transparently retry on the next key/provider.
  The client never sees the failure.
- **Failure after streaming has started** → **no** seamless recovery. The client sees
  the interruption. This is a deliberate compute/memory tradeoff — there is no
  full-response buffering retry path.

Retryable = transport errors + HTTP 408/429/5xx. Non-retryable 4xx (e.g. a genuine
400 from a bad request) is returned to the client immediately rather than replayed
across every key.

---

## Budgets & rate limits

- **Per-key**: RPM cap and/or a hard `max_requests` within a configurable window,
  plus a per-key in-flight concurrency cap and an optional USD cost cap.
- **Per-provider**: an optional aggregate RPM cap (`provider.rpm`).
- **Fixed window** — a counter + reset timestamp per provider/key. Explicitly
  chosen over sliding window / token bucket for low compute cost.
- **Hard limits**: once hit, routing to that key/provider is *blocked outright* until
  the window resets. No soft-warning.
- A `KeyBudget.Acquire` peeks every limit before consuming any, so a request blocked
  by one limit never burns a slot on another.

---

## Failover & health

- **Proactive**: a background loop issues a minimal 1-token chat completion probe to
  each key of each health-check-enabled provider on an interval, driving a
  circuit breaker (`closed → open → half-open → closed`) so a dead key/provider is
  marked unhealthy *before* routing reaches it. Real-request failures feed the same
  breakers.
- **Fallback**: per the alias's ordered target list (and provider `fallback` chains).

---

## Observability

Toggleable entirely via `observability.enabled` (it has a passive cost, so it ships
off-by-default in code, on in the example). When on, live in-memory metrics —
requests, errors, cached hits, tokens, cost, p50/p95 latency, per-provider
breakdown — are exposed on the admin socket. **Every request records a
`route_reason`** that explains *why* a routing decision fired (e.g. `sticky:p1/k1`,
`provider:p1/key:k1`, `provider:p2:breaker_open`), not just the outcome.

The retention store always records per-request metadata regardless of this flag
(that's cost tracking, a separate concern); observability governs only the extra
live aggregation.

---

## Data retention (tiered, SQLite WAL)

| Data | Retention | Mechanism |
|---|---|---|
| Raw request/response bodies | ~3–7 days | age-based delete |
| Per-request metadata (provider, key, latency, tokens, cost, status) | ~30 days | age-based delete |
| Daily/weekly rollups (cost/requests/errors per provider/key) | indefinite | aggregated from metadata *before* deletion (tiny footprint) |
| Errors | ~90 days | age-based delete (longer window) |
| Cache entries | TTL | separate lifecycle, not part of age-based retention |
| Config/key-change audit log | indefinite | append-only, tiny |

A `rollup_watermark` guarantees each request is aggregated exactly once before its
metadata rows are aged out; `VACUUM` runs periodically.

A retention window of `0` means **keep forever** (no age-based delete for that
class of data); positive values are the number of days a row is kept.

---

## Config schema (versioned)

`config.toml` is hand-editable TOML with a top-level `version` field. A mismatch is
a hard error, so future schema changes never silently break an existing config.
Full annotated example: [`examples/config.toml`](examples/config.toml).

Provider keys are **not** in the config. They live in an encrypted **side-storage
keystore** (`secret.keystore`), managed by `tallowctl key add/rm/list`, sealed with
AES-256-GCM under a key derived from the master key. Config only references a key by
`ref`. So plaintext keys never touch disk at any point.

Hot-reload: changing providers/keys/aliases (and the auth allow-list, and the
observability flag) takes effect without restart — Tallow watches the config file
and swaps the live registry atomically. A few knobs are startup-only (listen
address, admin socket, concurrency cap, cache TTL/entries, retention durations),
documented in the example.

---

## Admin interface (stable, minimal)

HTTP over a Unix socket (`server.admin_socket`). The TUI and any future frontend use
only this:

| Endpoint | Purpose |
|---|---|
| `GET /health` | liveness + version |
| `GET /metrics` | live observability snapshot (gated by `observability.enabled`) |
| `GET /providers` | providers + keys with live health, budgets, cost |
| `GET /aliases` | aliases + ordered targets |
| `GET /budgets` | live per-key budget status |
| `GET /health-states` | provider/key circuit-breaker states |
| `GET /logs?limit=N` | recent request metadata (incl. `route_reason`) |
| `GET /rollups` · `GET /audit` · `GET /cache` | retention/rollup/audit/cache views |
| `POST /reload` | trigger config hot-reload |

---

## Resource discipline (cross-cutting)

- Streaming is **passed through**, never buffered in full (except the deliberate
  first-token buffer).
- In-process caching only — no Redis/vector DB.
- Global + per-key in-flight caps, with a **bounded queue** (wait, then 429).
- Per-host connection pooling / HTTP keep-alive (one shared `http.Transport`).
- Pure-Go SQLite (`modernc.org/sqlite`) → `CGO_ENABLED=0`, truly static single binary.

---

## Explicitly out of scope (this version)

Semantic/embedding caching · near-duplicate hashing · sliding-window or token-bucket
rate limiting · seamless mid-stream retry after the first token · a web/GUI ·
auto-detected capability mapping · external cache/vector-DB dependencies.

---

## License

Tallow is licensed under the **Business Source License 1.1** ([LICENSE](LICENSE)).

- **You may**: use it internally (including production use inside your
  organization), self-host it, modify it, and redistribute it.
- **You may not**: offer it (or a product substantially overlapping its
  gateway capabilities) to third parties as a paid hosted or embedded
  competing service.
- **Change Date**: four years after first publication of each version, the
  work converts to **MIT** under the license's change terms.
- Need different terms (embedding, hosted offering, commercial licensing)?
  Open a discussion or contact the repository owner.

Note: BUSL-1.1 is a source-available license, not an open-source license; the
work converts to MIT on the Change Date. License text (c) MariaDB plc.