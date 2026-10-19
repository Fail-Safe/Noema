# Native TypeSafe evaluation

Noema can call a TypeSafe-compatible decision endpoint to evaluate retrieval
candidates. Inference uses a configured local or hosted endpoint; model weights
are not bundled. When evaluation is disabled or fails closed, native retrieval
results are preserved.

## Configuration

```yaml
typesafe_ai:
  enabled: auto
  provider: auto
  model: clef-flash
  timeout_ms: 30000
  token_env: TYPESAFE_API_KEY
```

## CLI

- `noema typesafe status`: inspect activation in this process without a network request.
- `noema typesafe metrics`: inspect local successful evaluation totals, tokens, latency maximum, and retrieval fallbacks.
- `noema typesafe probe`: explicit synthetic authentication/response-shape check.
- `noema typesafe gate --title TITLE --body BODY`: advisory draft evaluation; does not write a trace.
- `noema typesafe pair PRIMARY_ID SECONDARY_ID`: advisory consolidation scoring; does not merge or archive.

## Runtime behavior

Requests have a total deadline, at most two concurrent evaluations per process, and up to two retries for HTTP 429/529. Numeric Retry-After is honored within that deadline. Redirects are disabled. Failed requests and invalid responses preserve native retrieval results and report a redacted fallback on stderr and in `op_metrics`. Historical operation rows named `jev_retrieval` are migrated to `decision_retrieval`. Successful usage is recorded in the local-only `jev_metrics` table with operation, resolved model, token counts, and elapsed milliseconds; no prompt, trace body, or token is recorded. These metrics are not federated.

## Credentials

The integration uses `typesafe_ai:` and `noema typesafe`. The default credential variable is `TYPESAFE_API_KEY`. A custom `token_env`, such as `NOEMA_TYPESAFE_API_KEY`, uses only that variable. There are no cross-variable fallbacks. A blank token leaves automatic activation disabled. Model selection remains independent.
