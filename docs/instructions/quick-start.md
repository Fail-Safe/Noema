# Noema agent-guided quick start

Agent: help the user finish a working memory connection, not merely generate configuration. This guide is printed by `noema instructions quick-start` and needs no repository checkout. Read the installed command help if its version differs. Core Markdown storage, SQLite indexing, lexical search, and MCP need no embedding model, evaluator, cloud account, or model API key.

## 1. Inspect first; resume existing setup

Start with read-only discovery:

```sh
noema version
noema --help
noema cortex list
noema config get default
noema integrate list
noema integrate status --scope user
```

Inspect the OS, actual interactive shell and shell version, executable location (`command -v noema` on POSIX shells), package-manager installation, intended agent client/version, and whether this is a user-wide or project-only setup. `$SHELL` is a login-shell hint, not proof of the current shell. Check `NOEMA_CORTEX` without dumping unrelated environment variables or secrets. Inspect project integrations from the intended project directory with `noema integrate status --scope project`. Status describes files, not live MCP connectivity.

Do not reinitialize an existing cortex, replace someone else's integration, or overwrite shell startup files. Identify existing settings and resume the missing steps. Ask meaningful permission before installing packages, writing completions or shell startup configuration, creating a cortex, changing client configuration, networking, or deleting a test trace. Explain the specific destination and scope first. Never print token values or private memory contents in the completion report.

Choose either a local cortex (steps 3–4) or an existing remote HTTP cortex (step 5). Remote-only clients do not need a new local cortex.

## 2. Install and activate shell completions

First check whether the package manager already installed Noema completions and whether the shell already loads them. For example, Homebrew can supply completion directories. Reuse working packaged completions instead of installing a second copy; loading the completion framework or opening a fresh shell may be the only missing step. Do not install an additional completion package without permission.

For an uncovered bash, zsh, or fish installation, preview generated output with `noema completion bash`, `noema completion zsh`, or `noema completion fish`. After permission, install only for the chosen shell:

```sh
noema completion install --shell bash
# OR: noema completion install --shell zsh
# OR: noema completion install --shell fish
```

This writes a completion file, not shell startup configuration, and replaces an existing file at that destination. Inspect it before replacing it. Automatic detection uses `$SHELL`; explicit `--shell` avoids guessing.

- **Bash:** destination is `~/.bash_completion.d/noema`. Source that file in the current interactive bash with `source ~/.bash_completion.d/noema`. With permission, add a guarded source to the startup file actually loaded by this shell, usually `~/.bashrc`: `[[ -f ~/.bash_completion.d/noema ]] && source ~/.bash_completion.d/noema`. A login bash may need its existing profile to source `.bashrc`. On older macOS bash, check compatibility with the generated completion and use the user's supported bash/completion framework rather than silently installing or switching shells.
- **Zsh:** destination is `~/.zfunc/_noema`. Ensure `~/.zfunc` is in `fpath` **before** the existing `compinit` initialization. If none exists, the activation is `fpath+=(~/.zfunc)` followed by `autoload -Uz compinit && compinit`. Apply this to the current zsh as well as, with permission, its startup configuration. Avoid duplicate `compinit` calls or conflicting completion frameworks; initialize through the existing framework when present.
- **Fish:** destination is `~/.config/fish/completions/noema.fish`. Fish loads it automatically in new sessions. For immediate activation, run `source ~/.config/fish/completions/noema.fish` in fish. The installer uses this fixed home path, so check custom fish configuration directories explicitly.

Try `noema <Tab>` in the actual interactive shell. Report whether activation was observed or only installed. Other shells are not supported by this installer; do not pretend a bash completion works there.

## 3. Create or select a local cortex

Use `noema cortex list` and the default discovered above. If a suitable cortex exists, use it; select it persistently only with permission:

```sh
noema use my-cortex
```

Alternatively, `noema --cortex my-cortex ...` selects it for an individual command. Resolution uses explicit `--cortex`, then `NOEMA_CORTEX`, then the configured default, then a sole registered cortex. An environment override can therefore differ from the configured default. Resolving the sole cortex may persist a missing default.

If none exists, agree on its name and local parent directory, then create it:

```sh
noema init --name my-cortex
# Alternative location (choose this instead, not a second initialization):
# noema init --name my-cortex --path /absolute/parent-directory
```

`--path` is the **parent**, not the final cortex directory: the second example creates `/absolute/parent-directory/my-cortex`. Without it, the root is `~/.noema/my-cortex`. Creation registers the cortex and sets it as default only when no default is set. Do not reuse an already registered name for a different location. If needed, explicitly select the agreed existing/new cortex with `noema use my-cortex`.

Record the actual root printed by initialization or listed in the registry. Traces are Markdown with YAML frontmatter under `traces/`; Markdown is the source of truth, while local SQLite is a derived search/index store. The root also contains `cortex.md` and generated `AGENTS.md` guidance. Keep this memory directory somewhere the user intends to retain and back up; it is not merely an agent configuration directory.

The registry is `$XDG_CONFIG_HOME/noema/config.yaml` when set, otherwise `~/Library/Application Support/noema/config.yaml` on macOS or `~/.config/noema/config.yaml` elsewhere. Inspect only relevant entries; do not dump secrets from other configuration.

## 4. Connect a supported agent locally (default: stdio)

Choose the client actually used: `codex`, `claude-code`, `cursor`, `opencode` (OpenCode v1), or `ohmypi` (Oh My Pi / OMP). `noema integrate list` is authoritative for this binary. Pi is planned, not a supported adapter. Ask whether configuration should be user-wide (`--scope user`) or project-local (`--scope project`, run from the intended project). Inspect version-specific client help and existing configuration before edits.

Project scope targets the nearest ancestor containing `.git`, falling back to the current directory outside Git. Confirm the generated target paths rather than assuming the current subdirectory is the project root.

The following example chooses Codex and user scope. Replace the client and scope consistently with the agreed choices; use the selected cortex explicitly:

```sh
noema integrate codex status --scope user
noema --cortex my-cortex integrate codex print --scope user
noema --cortex my-cortex integrate codex install --scope user --check
```

`print` previews generated fragments. `install --check` previews without applying integration changes; a nonzero exit is expected while components are missing or would change. Read its component report: pending installation is not proof of a broken server. Conflicts or drift require investigation and a user decision, not an automatic `--force`. Do not chain preview and install with `&&`, because the pending preview intentionally exits nonzero.

After reviewing the proposed targets and obtaining permission:

```sh
noema --cortex my-cortex integrate codex install --scope user
noema integrate codex status --scope user --check
```

The ordinary adapter installs stdio MCP and startup memory guidance. The agent launches `noema serve` as its subprocess; do **not** start a standalone HTTP service, open a port, or install a daemon for this local workflow. Preserve unrelated client settings and existing hand-managed connections. A healthy status only proves recognized configuration files, not that the selected agent connected.

Codex also supports optional `--prefetch`, but it is not required for the first MCP connection. Do not use `--prefetch-only` or `--continuity` as a substitute for installing normal read/write MCP: those modes deliberately preserve or disable the ordinary managed connection. Avoid adding advanced modes before the basic round-trip works.

For an **unsupported MCP client**, preview `noema --cortex my-cortex serve --print-config`. It prints an MCP configuration fragment using this executable and cortex. Adapt and merge only its `noema` entry into the client's documented configuration format with permission. Never redirect it over an existing configuration file or replace the whole MCP server map. Check executable availability in the client's launch environment. Also install equivalent startup guidance using the client's documented rules mechanism: call `get_instructions`, then `recall_context` with task-relevant queries, `include_preferences=true`, `record_usage=false`, and no known revision at first startup. Retain preference bodies with the returned revision and reuse it only while those bodies remain in the same context; reload after context loss or switching cortexes. Do not infer native MCP support where none exists.

Restart/reload the chosen client or its MCP connections as its documentation requires; begin a fresh session if startup rules need one. Then perform step 6 from that actual agent.

## 5. Alternative: connect to an existing remote cortex

Use this instead of creating a local cortex when the user already has a remote MCP endpoint. Confirm the intended endpoint and cortex identity, access permission, client HTTP support, trusted TLS, and how the client obtains credentials. Prefer a provided authenticated HTTPS URL such as `https://memory.example.com/mcp` (or its intended role endpoint). Connecting is networking and needs permission. Do not create a public listener, disable authentication, bypass TLS verification, or deploy a new server as an onboarding default.

If a local cortex is already available, the supported adapters can generate remote configuration, for example:

```sh
noema integrate ohmypi print --scope user --transport http \
  --url https://memory.example.com/mcp --bearer-token-env NOEMA_MCP_KEY
noema integrate ohmypi install --scope user --transport http \
  --url https://memory.example.com/mcp --bearer-token-env NOEMA_MCP_KEY --check
```

Review, obtain permission, and repeat the installation without `--check` to apply it. The same preview, pending-exit, scope, drift, and restart rules apply. **Caveat:** in this version, adapter `print`/`install` still resolve a registered local cortex even for HTTP. That local selection does not establish the remote server's identity. Do not initialize a dummy local cortex to satisfy this limitation.

The adapter currently accepts only the exact URL path `/mcp`; role endpoints such as `/mcp/agent` require native/manual client configuration instead.

For a remote-only machine without a registered local cortex, use the chosen client's documented native HTTP MCP configuration instead: safely merge the existing endpoint and a bearer **environment-variable reference**, not a token value. Client schemas differ; do not invent a universal JSON format. `noema serve --print-config` can preview a local stdio fragment for unsupported clients, but it is not an arbitrary remote-URL generator. Configure equivalent startup memory guidance as in step 4. Clearly report manual configuration rather than claiming the adapter installed it.

Provision the named secret through an approved secret/environment mechanism that the launched client actually inherits, without echoing it, embedding it in commands, or writing it into committed project settings. `--bearer-token-env NOEMA_MCP_KEY` means the variable's **name**, not `$NOEMA_MCP_KEY` and not the credential. Never print the secret to verify it. An adapter's status check is not a remote authentication or reachability test. Restart/reload, then prove access through the real agent.

## 6. Prove actual agent access

Use the chosen agent's Noema MCP connection, not a shell invocation masquerading as agent verification. Discover tools and their schemas in that client; tool names may have a client-specific prefix.

1. Call MCP `get_instructions`. Follow its guidance. Call `cortex_usage` or `cortex_identity` when available to confirm the intended cortex, especially remotely; do not rely on a connection label alone.
2. Call MCP `recall_context` with a harmless onboarding query, `include_preferences=true`, `record_usage=false`, and no `known_preferences_revision`. Retain returned preference bodies/revision together and follow the server's startup policy. If unavailable, use `get_instructions` guidance and explicitly report the fallback or version mismatch.
3. Ask permission for a uniquely identifiable, harmless test trace and its later cleanup. Generate a fresh alphanumeric marker, for example `onboardingcheck` followed by random hexadecimal characters. Via MCP `create_trace`, create a `note` with that marker in its title/body and a harmless onboarding tag. Include no personal data, credentials, or actual preference. Use the discovered schema and retain the returned trace ID.
4. Via MCP `get_trace`, fetch that exact ID and confirm the marker. Via MCP `search_traces`, search the marker with lexical mode and confirm the same ID. Lexical search requires no embedding model. Check tool errors rather than assuming an MCP call succeeded.
5. With cleanup permission, call MCP `delete_trace` on **only that returned test ID**. This moves it to recoverable trash; do not purge or delete unrelated traces. If cleanup was declined or unavailable, report the exact test ID and retained state without concealing it.

If these tools are missing or calls fail, inspect the chosen client's MCP connection errors, startup environment, transport, credentials, tool profile/endpoint, and reload state. A CLI `list`, healthy integration status, generated config, or direct HTTP probe is not proof that this agent has access. Report the remaining blocker honestly rather than marking onboarding complete.

## 7. Optional federation and completion report

Federation is opt-in, separate from connecting an agent to one cortex. Do not configure peers or expose networking merely to finish this quick start. If requested, continue with:

```sh
noema instructions federation
```

Finish with a concise checklist containing evidence, not assumptions:

- Installed Noema version, OS/shell, executable location; existing setup resumed or new setup created.
- Completion source and actual activation observed, or why it was skipped/pending.
- Local cortex name/root/default and any environment override, **or** remote-only endpoint and verified cortex identity; no secret values.
- Chosen client/version, user/project scope, transport, configuration targets changed or deliberately preserved, and restart/reload performed.
- CLI preview/status results distinguished from actual agent MCP results.
- Actual agent `get_instructions`, `recall_context`, and create/get/lexical-search round-trip outcomes; test ID and cleanup/trash status.
- Federation skipped or explicitly requested; any outstanding permissions, failures, or unverified steps.
