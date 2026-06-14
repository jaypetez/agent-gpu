# Security Policy

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report them privately using GitHub's
[private vulnerability reporting](https://github.com/jaypetez/agent-gpu/security/advisories/new)
("Report a vulnerability" under the Security tab). If that is unavailable, contact the maintainer
directly.

Please include:

- A description of the vulnerability and its impact.
- Steps to reproduce (proof-of-concept if possible).
- Affected version(s) or commit(s).
- Any suggested mitigation.

We will acknowledge your report, keep you informed of progress, and credit you in the fix (unless
you prefer to remain anonymous). Please give us a reasonable window to release a fix before any
public disclosure.

## Supported versions

agent-gpu is pre-1.0. Until a stable release, only the latest `main` and the most recent tagged
release receive security fixes.

| Version | Supported |
| ------- | --------- |
| `main` / latest release | ✅ |
| Older pre-releases | ❌ |

## Scope & hardening notes

Because agent-gpu brokers access to inference resources, the following areas are
security-sensitive — reports here are especially welcome:

- **API key handling** — keys are stored only as salted hashes and returned in plaintext once.
- **Permissions** — role-based access and per-model allow/deny lists are deny-by-default.
- **Quotas / rate limiting** — abuse prevention per key and globally.
- **Server↔worker transport** — authentication and integrity of job dispatch.
- **Secret redaction** — secrets must never appear in logs.
