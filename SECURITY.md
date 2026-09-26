# Security Policy

Tallow is a security-sensitive tool: it holds your LLM provider API keys and
proxies all traffic to them. Please report vulnerabilities responsibly.

## Reporting a vulnerability

**Do not open a public GitHub issue for security reports.**

Use GitHub's private vulnerability reporting on this repository
(Security tab → Report a vulnerability), or contact the maintainer directly.
Include:

- What component is affected (proxy, admin socket, keystore, config, ...)
- How to reproduce (config, request, and logs; redact real API keys)
- Your assessment of impact

You should get an initial response within a few days. Please allow time for a
fix before any public disclosure.

## Network egress: the `base_url` SSRF guard

Tallow forwards a request carrying your provider `Authorization: Bearer <secret>`
header and the user's full prompt to whatever `base_url` names. That makes
`base_url` the one config field that decides where a credentialed request goes,
so it is guarded rather than trusted by convention.

**Default: a `base_url` must resolve to a public address.** Loopback, RFC1918,
carrier-grade NAT, link-local (including the cloud metadata address
`169.254.169.254`), IPv6 unique-local, `0.0.0.0/8`, `240.0.0.0/4`, multicast,
and the IPv4-mapped forms of all of these are refused. An address is permitted
only if it is positively identified as globally routable unicast space.

The check runs **per dial**, on the address the network is actually about to
use, not as a string check on `base_url` at load time. That is deliberate: a
string check judges the hostname you typed, while the resolved address is what
decides whether a credentialed request is about to leave the machine for a host
inside your network. Running per dial also covers pooled idle connections and a
hostname that re-resolves between requests, and every address a name resolves to
must be permitted, so a name pointing at both a public and a private address is
refused rather than connecting to whichever one the dialer happened to pick.

A refused connection produces a `502` to the client and logs the reason, naming
the address that was refused and how to proceed. The request never reaches the
internal host.

**The client's error never names the upstream.** The `502` body and the audit row
report which route failed, `provider/key`, and a failure category such as
`connection failed`, and nothing else. They never carry a `base_url`, host, port,
or credential, including in a transport error, where Go would otherwise render
the whole URL. The address a request was sent to is an internal detail of the
guard, so the full text goes to the log, where the operator can read it, and not
to the caller. The caller already knows which alias it asked for; it does not get
to learn the addresses behind it.

### Using a private upstream

A private upstream is a legitimate deployment: vLLM on `10.0.0.5`, Ollama on
`localhost`, a corporate gateway, a vPC on a peer link. If your `base_url` points
at one, opt out explicitly:

```go
import "github.com/rishabh-yadav11/tallow/internal/config"

config.SetAllowPrivateBaseURL(true) // call before app.New
```

**What you are accepting.** With the guard disabled, anyone who can influence
`base_url` obtains an authenticated HTTP request primitive into your internal
network, carrying your real provider credentials and your users' prompts. The
default-on guard exists to remove that escalation for configs delivered by
templating systems, CI jobs, control planes, or synced dotfiles - writers who can
set `base_url` without being able to read your keystore.

This is a deliberate, deployment-wide assertion rather than a config field. It
is intentionally **not** spelled in `config.toml`: an operator who can write the
config should not be able to opt a single provider entry out of the policy by
editing one line. Keep the default unless you have a specific private upstream,
and scope it to the process rather than globally.

### Note on redirects

Independently of the address guard, the upstream client **refuses to follow
redirects** at all. A 307 or 308 from an upstream would otherwise cause Go to
re-send the request, including the `Authorization` header, to whatever host the
redirect names. The upstream request therefore goes to exactly the URL the
router selected, and nowhere else.

## Data at rest

The SQLite store holds raw request and response bodies, so the database file is
created `0600` and its directory `0700`. A pre-existing database that is
group- or world-readable is tightened on open. The keystore is written `0600`
via a temporary file and an atomic rename, and provider secrets are encrypted
at rest under `TALLOW_MASTER_KEY`.

Cache hits are recorded as their own status with no provider attribution: a
response served from cache did not come from a provider, so crediting one would
report spend that never happened. The route that originally produced the answer
is preserved in the row's `route_reason` as `cache_hit_from:<provider>`.

## Scope notes

- The admin interface is a **local Unix socket**; anything with local file
  access to that socket is trusted by design. Report socket-permission or
  path-traversal issues as vulnerabilities.
- The master key (`TALLOW_MASTER_KEY` / keyfile) protects provider keys at
  rest. Weaknesses in `internal/secret` (keystore encryption, key loading) are
  in scope.
- SSRF via provider `base_url`, auth bypass on `/v1/*`, and secret leakage
  through the admin API, logs, or SQLite contents are in scope.
- By default the proxy listens on `127.0.0.1`. Moving `listen` to a routable
  address puts the gateway in front of untrusted clients; auth is opt-in
  (`auth.open = true` disables it), so a non-loopback `listen` should be paired
  with an explicit `api_keys` allow-list.

## Supported versions

Pre-1.0: only the latest commit on `main` is supported. Please build from
source and update before reporting.
