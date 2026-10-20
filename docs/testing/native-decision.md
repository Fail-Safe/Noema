# Native decision evaluation

Noema can call a local or hosted TypeSafe-compatible decision endpoint to
evaluate retrieval candidates. Inference uses a configured endpoint; model
weights are not bundled. When evaluation is disabled or fails closed, native
retrieval results are preserved.

The default local endpoint is `http://127.0.0.1:18788/v1/decision`. Providers
that still expose the older TypeSafe-compatible path `/v1/systemone` can be
selected with an explicit `endpoint:`.

## Configuration

```yaml
decision:
  enabled: auto
  provider: auto   # auto | local | hosted
  model: clef-flash
  timeout_ms: 30000
  token_env: TYPESAFE_API_KEY
```

| `provider` | Behavior |
|---|---|
| `auto` (default) | Infer from model: `clef-flash` uses local activation; other models use hosted credentials |
| `local` | Local decision endpoint only; hosted credentials are rejected |
| `hosted` | Hosted TypeSafe credentials required |

Local mode never auto-falls back to hosted evaluation.

## CLI

- `noema decision status`: inspect activation in this process without a network request.
- `noema decision metrics`: inspect local successful evaluation totals, tokens, latency maximum, and retrieval fallbacks.
- `noema decision probe`: explicit synthetic authentication/response-shape check.
- `noema decision gate --title TITLE --body BODY`: advisory draft evaluation; does not write a trace.
- `noema decision pair PRIMARY_ID SECONDARY_ID`: advisory consolidation scoring; does not merge or archive.

## Runtime behavior

Requests have a total deadline, at most two concurrent evaluations per process, and up to two retries for HTTP 429/529. Numeric Retry-After is honored within that deadline. Redirects are disabled. Failed requests and invalid responses preserve native retrieval results and report a redacted fallback on stderr and in `op_metrics`. Historical operation rows named `jev_retrieval` are migrated to `decision_retrieval`. Successful usage is recorded in the local-only `jev_metrics` table with operation, resolved model, token counts, and elapsed milliseconds; no prompt, trace body, or token is recorded. These metrics are not federated.

## Credentials

The integration uses `decision:` and `noema decision`. The default hosted credential variable is `TYPESAFE_API_KEY`. A custom `token_env`, such as `NOEMA_TYPESAFE_API_KEY`, uses only that variable. There are no cross-variable fallbacks. A blank token leaves automatic activation disabled. Model selection remains independent.
