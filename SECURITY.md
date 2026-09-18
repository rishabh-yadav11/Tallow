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

## Scope notes

- The admin interface is a **local Unix socket**; anything with local file
  access to that socket is trusted by design. Report socket-permission or
  path-traversal issues as vulnerabilities.
- The master key (`TALLOW_MASTER_KEY` / keyfile) protects provider keys at
  rest. Weaknesses in `internal/secret` (keystore encryption, key loading) are
  in scope.
- SSRF via provider `base_url`, auth bypass on `/v1/*`, and secret leakage
  through the admin API, logs, or SQLite contents are in scope.

## Supported versions

Pre-1.0: only the latest commit on `main` is supported. Please build from
source and update before reporting.
