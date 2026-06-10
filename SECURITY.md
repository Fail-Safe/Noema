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

## Federation Trust Model

Federation authenticity rests on two layers. The transport layer (the shared
bearer key, optionally over TLS) controls *who may connect*. The event layer
(per-cortex Ed25519 signatures) authenticates *which cortex authored each
event*, independent of which peer relayed it. A cortex that has run `noema keygen` signs every event it emits;
peers pin its public key per cortex id on first contact and, when
`federation.verify` is set to `enforce`, reject any event that is not correctly
signed by its owning cortex — which also extends source-lock enforcement to the
replay path.

Known residual: key distribution defaults to trust-on-first-use, so an attacker
who intercepts the very first handshake or event from a cortex id can pin their
own key for it. This is mitigated by running the handshake over TLS and, for a
high-assurance peer, by hard-pinning its key out-of-band: set `pubkey:` on that
peer's entry in `cortex.md` and the peer must advertise exactly that key or the
sync is refused, bypassing first-use entirely. Even so, signing makes federation
*authenticated-trust*, not zero-trust. Reports that defeat the intended
guarantees **after** a key is correctly pinned — signature forgery, verification
bypass, source-lock bypass under `enforce`, or downgrade attacks — are in scope.

## Supported Versions

Only the latest release is actively supported with security patches. Upgrade to the latest version before reporting.
