# Optional federation onboarding

Instructions for the agent assisting the user. Reading this guide changes nothing. Do not execute all examples as a script: inspect first, agree on a plan, then perform only authorized steps. Replace example addresses, paths, names, and public-key placeholders with verified values. No secrets belong in the conversation, terminal output, command history, or generated examples.

## 1. Decide whether federation is needed

Ask what the user wants to share, with whom, and in which direction. Explain the choices before changing anything:

- **One local-first Cortex:** agents on one machine can share the same local Cortex through stdio. No HTTP listener or federation is required.
- **Remote-only MCP access:** clients on other machines can connect to one Cortex's HTTP MCP endpoint. They still use that one Cortex, not independent replicas. Merely configuring a remote MCP client does not configure federation.
- **Federation:** distinct Cortex identities keep local data and pull events from trusted peers. Replication can disclose existing history, not just new test content. It is not a per-peer filtered sharing feature. Every bearer-key holder has access to the tools exposed by the selected HTTP endpoint; the shared key is not a federation-only credential.

Ask which hosts and Cortexes are trusted, whether historical content may replicate, whether local writes on each replica are intended, and whether the user needs two-way sync or a publisher/consumer arrangement. Ask before installing packages, provisioning or distributing credentials, editing manifests or client/service configuration, starting/restarting services, opening listeners/firewalls, making network requests, creating traces, or deleting anything. Approval should name the affected peers and data direction; do not infer it from a request to explain federation.

Use `host-a` and `host-b` below as generic host labels. Each can have a locally registered Cortex called `my-cortex`, but they must have **different stable Cortex IDs**. Initialize independent Cortexes rather than copying an existing Cortex directory to invent a new peer. A backup/restore preserves identity; a different registration name alone is not a new identity. Never regenerate or migrate an existing identity as an onboarding shortcut.

## 2. Inspect and resume, rather than starting over

On each intended peer, begin with installed-version and registered-Cortex inspection:

```sh
noema version
noema cortex list
noema --cortex my-cortex federation status
noema --cortex my-cortex federation peers
noema --cortex my-cortex federation key fingerprint
noema --cortex my-cortex verify cortex
```

Use the path from `cortex list` to inspect the selected fields of `cortex.md`: `id`, `name`, `access`, `signing`, and `federation`. Do not dump secret sidecar files or service environments. Record non-secret identities, public keys, endpoints, modes, pins, peer pause state, and existing listener/service ownership. A CLI fingerprint reflects that CLI process's environment/file configuration, not necessarily the running service's environment.

If no Cortex exists, return to `noema instructions quick-start`; initialize one only with permission. If peers already exist, preserve their IDs, cursors, pins, and pause state. Do not add duplicate peers. Resume an intentionally paused peer only after asking why it was paused and resolving the cause:

```sh
noema --cortex my-cortex federation resume-peer host-b
```

Configured peers, an access fingerprint, or a status JSON response do **not** prove reachability or replication. Status includes saved observations such as `cortex_id`, `last_event`, `last_seen`, and `health`; check their recency and source. Do not call the onboarding complete from configuration alone.

## 3. Establish authenticated, reachable HTTP service

A remote peer needs an HTTP listener reachable from the machine that will pull. Loopback (`127.0.0.1`, `::1`) is local to each host; it is not a remote peer address. Choose explicit local interface addresses and a stable DNS name, then authorize only the necessary network route/firewall scope. Do not expose an unauthenticated listener to the internet. This implementation refuses unauthenticated non-loopback serving, unspecified bind addresses such as `0.0.0.0`, and bearer-key serving without TLS.

### Shared bearer key

Federation sends the local active access key to every configured peer. All participating keyed peers must therefore use the same secret; there is no per-peer token setting. Provision/distribute it through an approved secure channel, never by printing it. Two supported sources, in priority order:

1. `NOEMA_MCP_KEY`, securely injected into the CLI/server process environment. A non-empty variable overrides the manifest's file. Do not place literal secrets in shell history or world-readable service files.
2. `access.shared_key_file` in `cortex.md`. Relative paths resolve inside the Cortex directory. The first non-empty line is the key; on Unix the file must not grant group/other permissions (use `0600`).

For a new ring only, after permission and after checking that the destination does not exist, this creates a random secret file without displaying or overwriting it, if OpenSSL is already installed:

```sh
(umask 077; set -C; openssl rand -base64 32 > /path/to/my-cortex/.access.secret)
```

Transfer that same secret securely to the other hosts; do not independently generate different keys on each. Do not overwrite an existing ring key. Merge this frontmatter fragment into the existing `cortex.md`, preserving other fields:

```yaml
access:
  shared_key_file: .access.secret
  tls_cert_path: /path/to/host-a.crt
  tls_key_path: /path/to/host-a.key
```

TLS paths may also be relative to the Cortex directory. Supply both certificate and private key. Use a currently valid certificate whose subject alternative names cover the DNS/IP address peers will use; make its issuer trusted on those peers. Do not disable certificate/hostname validation or bypass expired certificates to get onboarding to pass.

Compare only non-secret fingerprints over a trusted out-of-band channel:

```sh
noema --cortex my-cortex federation key fingerprint
```

All keyed participants should match. Check environment overrides and running-service configuration if the CLI result and service behavior disagree. Keep the bearer secret distinct from the TLS private key and the per-Cortex signing private key.

### Start the listener

After permission, on host-a, use an address actually assigned to that host. `192.0.2.10` is a documentation-only placeholder, not a deployable default:

```sh
noema --cortex my-cortex serve --transport http \
  --host 192.0.2.10 --port 3000 --allowed-host host-a
```

This uses the TLS paths and shared-key file above. Alternatively supply `--tls-cert /path/to/host-a.crt --tls-key /path/to/host-a.key`; CLI paths override the corresponding manifest paths. Configure host-b similarly with its own address/certificate and `--allowed-host host-b`.

`--host` chooses a bind address; `--allowed-host` admits an additional HTTP Host name/authority without binding another interface. If clients connect using a DNS alias not in the bind hosts, explicitly permit that alias (or required authority, such as `host-a:3000`) and ensure the certificate also covers it. Host validation and TLS hostname validation are different checks; neither replaces authentication. Repeat `--host` for additional required listeners. A roaming host may combine a stable loopback `--host` with an explicit `--host-dynamic` LAN address; the optional listener exists only while that address is local.

### Endpoint paths matter

- A federation peer `endpoint` is a **base URL**, such as `https://host-b:3000`, **without `/mcp`**. The syncer appends `/mcp` itself. Supplying `/mcp` would request `/mcp/mcp`.
- An MCP client URL includes the actual endpoint path: `https://host-a:3000/mcp`, or a supported role endpoint such as `/mcp/agent`. A remote-only client needs no local federation peer.
- The federation syncer uses full `/mcp`; do not substitute `/mcp/federation` in the base URL. Proxy setups must route the resulting path without relying on redirects; the federation HTTP client does not follow redirects.
- Keyed MCP clients send `Authorization: Bearer <key>` from secure runtime configuration. A literal `${NOEMA_MCP_KEY}` only works if that client actually expands environment variables.

For a private/self-signed issuer, place its verified CA PEM bundle on the **pulling** host and set `ca: /absolute/path/to/peer-ca.pem` on that peer entry. Prefer an absolute path: the implementation reads this path directly, not relative to the Cortex directory. Obtain the CA through a trusted channel; adding a CA is a trust change, not a workaround for an unexplained certificate error.

## 4. Establish signing trust and choose policy

Bearer authentication controls access to the endpoint. Ed25519 event signatures authenticate event origins. With permission, generate a signing key separately on each Cortex that does not already have one:

```sh
noema --cortex my-cortex keygen
```

This writes a protected `noema-signing.key` and the `signing` manifest block, and prints only the public key. Keep each private signing key on its owning Cortex. Do not use `keygen --force` during onboarding: rotation is not harmless and there is no historical key registry for replaying old signatures.

Exchange each stable Cortex ID and `signing.public_key` over an independently trusted channel. They are not bearer secrets. Compare them with the identity advertised by the intended remote endpoint (`cortex_identity` through an approved MCP connection) and with the later pinned ID in status.

Without a configured `pubkey`, the first handshake records the advertised peer ID/key using trust-on-first-use. This detects a later change; it does **not** independently verify the first peer. For first-contact assurance, set the verified public key on the peer entry **before the first sync**:

```yaml
federation:
  mode: sync
  interval: 30s
  verify: enforce
  peers:
    - name: host-b
      endpoint: https://host-b:3000
      pubkey: ed25519:REPLACE_WITH_VERIFIED_BASE64_PUBLIC_KEY
      # ca: /absolute/path/to/host-b-ca.pem
```

Merge, do not replace an existing federation block or paste the placeholder as a real key. A configured public-key pin must match the handshake. The stable peer ID is still learned on first contact, so independently compare that ID too. Relayed events from previously unknown origins can bootstrap from the public key carried by the event; signature verification is not an independently approved origin allowlist. Keep the trust boundary explicit and verify/pin every intended origin rather than claiming TOFU proves ownership.

`federation.verify` accepts exactly:

- `off` (default): no incoming signature verification.
- `warn`: verify and log problems, but still apply bad/unsigned events.
- `enforce`: reject unsigned, unverifiable, or conflicting-key events; also reject foreign-origin mutations of source-locked traces.

Generating keys alone does not enforce anything. Use `enforce` on accepting Cortexes for actual enforcement, after confirming the history they must replay is correctly signed. Existing unsigned events remain unsigned; newly generating a key does not retroactively sign history. Old events signed before a key rotation may also fail under the current key. If history cannot meet enforcement, explain the incompatibility and ask for a deliberate migration/trust decision; do not silently downgrade to `off`/`warn`, clear cursors, or rotate/re-pin keys.

Modes accept **`sync`, `publish`, or `subscribe`**. The two-way concept is called `sync` here; `both` is not an accepted value.

| Mode | Pulls peer events | Serves events | HTTP trace writes |
| --- | --- | --- | --- |
| `sync` (default) | Yes | Yes | Allowed |
| `publish` | No | Yes | Blocked; manage content locally via CLI/stdio |
| `subscribe` | Yes | No | Allowed |

Choose explicitly, with permission:

```sh
noema --cortex my-cortex federation set-mode sync
```

Modes govern behavior, not per-peer content selection. A subscriber can make local writes, but does not publish them through `sync_events`.

## 5. Configure the pull topology

Adding a peer changes local configuration and can trigger automatic historical pulls while a server is running. Complete trust/auth/policy setup first; stop or coordinate existing background sync before adding a peer if needed. `add-peer` only takes a name and base URL; it cannot set the CA or public-key pin. For pinned first contact, merge the complete peer entry above before allowing a pull, rather than briefly syncing an unpinned `add-peer` entry.

For an approved TOFU bootstrap, the CLI form is:

```sh
# On host-a: host-a will pull host-b.
noema --cortex my-cortex federation add-peer host-b https://host-b:3000
# On host-b: host-b will pull host-a.
noema --cortex my-cortex federation add-peer host-a https://host-a:3000
```

Labels must be unique in that manifest and cannot equal its own manifest name. Use the verified existing labels when resuming. Adding host-b on host-a does **not** configure host-b, push events to it, or prove reciprocal connectivity.

For two-way replication, each `sync` Cortex needs a reachable listener and a reciprocal pull entry. For one-way publishing, configure the consumer (`subscribe`) to pull the publisher (`publish`); the publisher does not need a reciprocal entry and cannot manually pull. Do not point a pull entry at a `subscribe` Cortex expecting it to serve events.

Restart the affected `noema serve` processes after approved configuration changes to make the resident access key, signing key, verification policy, listener settings, and HTTP write mode consistent. Some background federation configuration is re-read while running, but that is not a guarantee that every server setting hot-reloads. A CLI status command reads current disk configuration and cannot certify that an old process loaded it. Coordinate restarts with existing clients rather than starting a competing listener.

## 6. Prove replication on real peers

With permission to make the network calls, run a manual pull on the receiving host and inspect status:

```sh
# On host-a, pulling from its configured host-b entry:
noema --cortex my-cortex federation sync host-b
noema --cortex my-cortex federation status
```

Without a name, `federation sync` pulls all configured peers. Do not confuse it with plain `noema sync`, which reindexes local trace files. Automatic polling normally uses the configured interval (default `30s`) while an HTTP server owns background work. A successful zero-event pull can mean already caught up; it is not proof that new content crossed hosts. Manual pulls do not themselves provide an ongoing supervisor.

Ask permission for one uniquely named, non-sensitive test note and its intended replication scope. Choose a fresh title/body nonce so old replicas cannot produce a false positive. For host-b → host-a, create locally **on host-b**:

```sh
noema --cortex my-cortex add --title 'Federation check UNIQUE-NONCE' \
  --type note --author example-user --tag federation-check \
  --body 'Harmless host-b to host-a replication check UNIQUE-NONCE.'
```

Capture the actual ID from `Trace added: ...`; do not invent it. On **host-a**, pull and read that exact ID:

```sh
noema --cortex my-cortex federation sync host-b
noema --cortex my-cortex get ACTUAL_TRACE_ID
noema --cortex my-cortex federation status
```

Compare ID and nonce-bearing content with the source. Confirm these commands ran against the distinct host-a and host-b Cortex IDs, not twice against one shared remote MCP server. For two-way `sync`, repeat with a different harmless note created on host-a and pulled/read on host-b. For publisher → subscriber, create locally on the publisher and pull/read only on the subscriber; do not claim reverse replication is expected.

Report exactly which direction, identities, trace IDs, content match, and manual/automatic path were exercised. A mock endpoint, saved status, configured peer list, or connection to one shared Cortex is not proof of cross-peer replication. If automatic polling is required, also observe a fresh note arrive without a manual pull before claiming that path works.

Keep test traces unless the user authorizes cleanup. With permission, `noema --cortex my-cortex remove ACTUAL_TRACE_ID` moves a test trace to recoverable trash and emits a replicating mutation; coordinate the appropriate source/direction and check the receivers. Do not purge, use force, or delete unrelated data. Removal does not erase immutable event history or undo prior disclosure.

## 7. Preview persistence, then ask before installation

After the foreground service and real replication are working, the same explicit serve arguments can print a service template:

```sh
noema --cortex my-cortex serve --transport http \
  --host 192.0.2.10 --port 3000 --allowed-host host-a \
  --tls-cert /path/to/host-a.crt --tls-key /path/to/host-a.key \
  --print-systemd-unit
```

On macOS use `--print-launchd-plist` instead. These flags print generated output and exit; they do **not** install, enable, load, or start a service. They require explicit `--cortex`, HTTP transport, and a host. Preview checks are not a runtime TLS/bind/reachability test.

**This implementation prints a literal `NOEMA_MCP_KEY=replace-with-real-key` environment placeholder** in the systemd unit and an equivalent entry in the launchd plist. Do not install that unchanged: it would override a real manifest sidecar key. Prefer removing the placeholder entirely and using the protected `access.shared_key_file`. Alternatively, provision an approved protected environment mechanism; for systemd, a separately secured `EnvironmentFile=` is an operator edit, not something this generator adds. Never embed a real secret in the printed/shared template or a world-readable plist/unit.

Review executable path, selected Cortex, service user/home, local addresses, DNS allowlist, TLS paths, credential-file access, and existing service ownership. Ask permission before writing system service/LaunchAgent files or issuing systemd/launchd installation, enablement, or restart operations. Use the host's existing supervision convention rather than replacing another service. After installation/restart, verify the resident identity/authentication and repeat the real-peer proof before reporting persistent operation.

## Troubleshoot without bypassing trust

- **No events / stale status:** inspect `federation status` and `federation peers` on both sides; check direction, mode, pause state, cursor/last-seen freshness, and the resident process. Use a targeted `federation sync PEER` for its actual error. A publisher cannot pull; a subscriber cannot serve events.
- **Connection/DNS/bind failure:** verify the chosen address belongs to the serving host, the intended process is listening, DNS resolves from the pulling host, and an authorized route/firewall permits that exact connection. Loopback and an unassigned dynamic LAN listener cannot satisfy remote reachability. Ask before network changes.
- **404 or protocol failure:** peer entries need the base URL without `/mcp`; remote MCP clients need a real `/mcp` endpoint. Check proxy routing and avoid redirects.
- **401:** compare `federation key fingerprint` on each host under the appropriate process configuration; inspect environment override presence without revealing values. Restart after approved credential changes. Do not disable authentication.
- **Host rejected:** check DNS alias/authority against `--allowed-host`; this does not fix a certificate name mismatch.
- **TLS failure:** use `verify cortex` for local manifest/certificate diagnostics; correct certificate validity, names, chains, and the peer's trusted `ca` file. Do not disable TLS verification or use an insecure expiration bypass.
- **Identity/key mismatch or rejected signatures:** stop onboarding, retain pins/cursors, compare the expected stable ID and public key out of band, and investigate replacement, rotation, historical signatures, or an incorrect endpoint. If stopping further pulls is authorized, `federation pause-peer PEER` is available. Never use `reset-peer`, `re-pin-peer`, forced key generation, identity migration, or a verification downgrade just to silence the error.
- **Concurrent content divergence:** inspect `noema --cortex my-cortex list --type divergence`; ask the user before choosing a resolution. Do not overwrite a replica to make a check appear clean.

Finish with a short factual handoff: topology and trust decision, configured versus running state, exercised replication directions, persistent-service state, retained test IDs, and any unresolved prerequisite. Optional background reading: https://github.com/Fail-Safe/noema#federation . All essential steps and limitations are included above so this guide also works offline.
