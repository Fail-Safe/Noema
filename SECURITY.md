# Security Policy

## Reporting a Vulnerability

**Please do not open a public GitHub Issue for security vulnerabilities.**

Instead, use GitHub's private vulnerability reporting:

1. Go to [Report a vulnerability](https://github.com/Fail-Safe/Noema/security/advisories/new)
2. Fill in a description of the issue, steps to reproduce, and any affected versions
3. Submit — this creates a private advisory visible only to you and the maintainers

You will receive a response acknowledging the report within 72 hours. Fixes for confirmed vulnerabilities are prioritized and shipped as patch releases.

## Scope

Noema is pre-1.0 software. The following areas are in scope for security reports:

- Path traversal or file-system escapes (e.g. via crafted trace IDs or federation events)
- Authentication bypass on the keyed HTTP transport
- Federation event replay attacks (hash forgery, source-lock abuse, payload injection)
- SQL injection or FTS5 query injection
- TLS configuration weaknesses in the MCP HTTP server
- Denial of service via unbounded input (query length, payload size, connection exhaustion)

Out of scope:

- Vulnerabilities that require local filesystem access to the cortex directory (Noema trusts the local operator)
- Issues in dependencies — please report those upstream, but feel free to flag them here if Noema's usage makes the issue exploitable

## Supported Versions

Only the latest release is actively supported with security patches. Upgrade to the latest version before reporting.
