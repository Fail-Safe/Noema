# Set up Noema with your AI agent

These guides are bundled with this Noema binary. Printing them is read-only:
no Cortex, configuration, credentials, or network connection is required.
They do not install anything or execute the steps they describe.

## Start here

```sh
noema instructions quick-start
```

Give your agent this prompt:

> Run `noema instructions quick-start`, then help me follow it on this machine.
> Inspect what is already installed first, explain proposed changes, and ask
> before modifying shell or agent configuration, installing packages, or
> deleting anything. Verify that this agent can actually write and retrieve
> a harmless test memory through Noema's MCP connection.

The quick-start covers:

1. Inspecting the installed version, shell, Cortexes, and agent integrations.
2. Installing and activating shell completions when needed.
3. Creating or selecting the first local Cortex, or using an existing remote one.
4. Connecting an AI agent through MCP and loading its startup instructions.
5. Proving a memory create/get/search round-trip through the actual agent.

No embedding service, language-model API key, or federation is needed for
local lexical search and MCP memory access.

## Optional: connect multiple Cortexes

```sh
noema instructions federation
```

Use this guide only when you want distinct Cortexes to exchange memories.
Connecting several agents to one MCP server does not require federation.
The guide covers peer identities, trust, authentication, TLS, signing, sync
modes, persistent serving, and verification on the real peers. Agree on what
may replicate before enabling it; federation is not a per-tag sharing filter.

## Instructions for the helping agent

- Treat this runbook as setup guidance, not permission to change the machine.
- Inspect existing state and resume it. Do not replace existing Cortexes or
  overwrite unrelated client configuration.
- Ask only for consequential choices: storage location, agent/client scope,
  remote endpoint, peer trust, and permission for changes.
- Use this binary's `--help` for command syntax. Inspect only needed fields;
  never dump secret-bearing configuration, environment variables, or tokens.
- Prefer local stdio MCP for a first local setup. Do not open a listener,
  install a background service, or enable federation without explicit approval.
- Stop on an error, explain it without exposing secrets, and resolve the cause
  before continuing. Do not bypass TLS, identity, signature, or ownership checks.
- Report what is configured separately from what has been exercised. A config
  file or healthy CLI status does not prove the agent has loaded its MCP tools.

For everyday use after setup, call Noema's MCP `get_instructions` tool and
follow the live server's startup preference/retrieval policy. That tool
explains memory use in the connected Cortex; `noema instructions` explains
how to get connected in the first place.
