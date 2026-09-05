# cpa-guard

`cpa-guard` is a small HTTP retry proxy for a CPA/OpenAI-compatible gateway. It is designed to run beside CLIProxyAPI without changing the gateway's configuration.

```text
Ingress → cpa-guard:8217 → CLIProxyAPI:8317 → OpenAI
```

The proxy buffers each request body once, then can replay it on another attempt. It retries only when no upstream response has been sent to the client. By default the retryable statuses are `502`, `503`, and `504`; connection failures and response-header timeouts are also retryable. Once a successful streaming response starts, it is passed through and is never replayed.

`MAX_ATTEMPTS` is the total number of attempts, including the first one. It is intentionally capped at 500. Retries use exponential backoff with jitter and honor a bounded `Retry-After`. `MAX_ELAPSED_MS` is a second safety budget; a client-side timeout or cancellation still wins and cannot be overridden by this proxy.

The proxy never logs request paths, bodies, authorization headers, or upstream error bodies. Metrics are disabled by default because `/metrics` would otherwise be reachable through a broad ingress path; enable them only behind an internal-only route.

## Configuration

Copy `.env.example` to the service's `.env` and adjust the retry budget to the caller's deadline. The important defaults for a local deployment are:

| Variable | Default | Meaning |
| --- | ---: | --- |
| `APP_PORT` | `8217` | Listening port when `LISTEN_ADDR` is unset |
| `UPSTREAM_URL` | `http://127.0.0.1:8317` | Existing CPA endpoint |
| `MODEL_ACL_FILE` | `model-acl.toml` | ACL file in the working directory; optional path override (legacy JSON also supported) |
| `MAX_ATTEMPTS` | `100` | Total attempts, capped at 500 |
| `MAX_ELAPSED_MS` | `3300000` in the example | Overall retry budget |
| `RETRY_STATUSES` | `502,503,504` | Statuses retried before output starts |
| `MEMORY_BODY_BYTES` | `2097152` | Keep small replay bodies in memory; larger ones use `BODY_DIR` |
| `BODY_DIR` | `/home/cliproxy/cpa-guard/tmp` | Temporary request-body spool |
| `DEBUG` | `false` | Log safe request-body failure diagnostics; never logs bodies or authorization headers |

When diagnosing a `request_body_error`, set `DEBUG=true` and restart cpa-guard.
The next failure emits a `request_body_debug` JSON line containing the underlying
read/spool error plus request-body metadata (length, transfer encoding, and the
configured limits). It does not log the request body, query string, or
authorization headers. Set it back to `false` after diagnosis.

Do not enable high retry counts without accounting for duplicate upstream work and billing. A 503 can arrive after an upstream has accepted a POST, so tool calls and other side effects must remain idempotent at the caller level.

## Model access control

Create `model-acl.toml` in the working directory and restart the service;
no environment variable is required. With the included installer, the path is
`/home/cliproxy/cpa-guard/model-acl.toml` (beside `.env`):

```toml
# Only these keys may use this model.
[models."gpt-4o"]
allow_keys = ["key-for-team-a", "key-for-team-b"]

# Explicitly allow every key (also the default for models not listed).
[models."gpt-4o-mini"]
allow_keys = ["*"]

# Disable this model for every key.
[models."claude-sonnet-4-5"]
allow_keys = []
```

Rules use exact, case-sensitive model names accepted by CPA. Always quote model
names, especially those containing dots or slashes. Models not listed are
allowed by default; an empty TOML file imposes no model restrictions.
Every listed model must have an `allow_keys` array. Comments, multiline arrays,
and trailing commas are supported. Unknown fields, duplicate rules, missing
`allow_keys`, and invalid values prevent startup.

The guard forwards the original key; CPA remains responsible for validating it.
`["*"]` allows any key through the model ACL, including keys not mentioned in
this file, but does not bypass CPA authentication. Clients must send exactly one
credential: `Authorization: Bearer <key>`, `X-Api-Key`, or `X-Goog-Api-Key`.
Missing, conflicting, and query-string credentials receive `401`. A key denied
access to the requested model receives `403` before any upstream attempt.

Protect the file as a secret (for example, mode `0600`, readable by the service
account). `model-acl.toml` and `model-acl.json` are ignored by Git. Restart the
service after changes. If the default file is absent, ACL is disabled and
existing proxy behavior is preserved. An unreadable or invalid default file
prevents startup. To use another location, set `MODEL_ACL_FILE` explicitly;
an explicitly configured file must exist. Remove the default file and leave
`MODEL_ACL_FILE` unset to disable ACL.

For compatibility, files without a `.toml` extension retain the original JSON
format: `{"key-a":["gpt-4o"],"key-admin":["*"]}`. This legacy format is a
**per-key whitelist**: unknown keys are rejected, and an empty object denies all
keys. TOML uses **per-model rules**, with unlisted models allowed by default.

Restricted keys support JSON `POST` requests to `/v1/chat/completions`,
`/v1/completions`, `/v1/responses`, `/v1/embeddings`, `/v1/messages`,
`/v1/messages/count_tokens`, `/v1/images/generations`, `/v1/audio/speech`, and
`/v1/moderations`. Each requires `Content-Type: application/json` and an explicit
nonempty string `model`. Invalid, missing, or ambiguous model fields receive
`400`. Compressed bodies, query parameters, encoded paths, multipart requests,
WebSocket connections, and other routes are denied for restricted keys.
Streaming JSON requests (`"stream": true`) retain normal proxy behavior.

With TOML configuration, `GET /v1/models` filters the upstream list, removing
models the current key cannot use while retaining unlisted models. Invalid or
oversized upstream lists return `502` instead of leaking an unfiltered list.
With legacy JSON configuration, restricted keys receive their configured model
names locally; unrestricted keys receive the upstream list.
A key that can access every model can use all upstream routes. For a key with
any model restriction, routes that cannot reliably identify a model are denied.
`/healthz`, `/readyz`, and the optionally enabled `/metrics` remain local endpoints
with their existing access behavior. Route client traffic through the guard to
enforce this policy; direct access to CPA bypasses it.

## Retry only for selected keys

Add a `[retry]` table to the same `model-acl.toml` and restart the service:

```toml
[retry]
allow_keys = ["abc", "def"]

# Optional, independent model access rule:
[models."gpt-6-astra"]
allow_keys = ["abc"]
```

Only the listed keys use the configured retry budget (`MAX_ATTEMPTS`,
`MAX_ELAPSED_MS`, backoff, and `RETRY_STATUSES`). Other keys get one upstream
attempt: the first HTTP response is passed through, and a network failure
returns the guard's `503` error without another attempt or a backoff wait.
The same policy covers both retryable HTTP statuses and network failures.
Successful streaming responses are still never replayed.

`allow_keys = []` disables retries for everyone; `allow_keys = ["*"]` allows
retries for everyone. Omitting `[retry]` retains the existing retry behavior.
A `[retry]` table without `allow_keys` is invalid. Key matching is exact and
supports the same three credential headers as model ACL; missing or ambiguous
credentials do not qualify for a key-specific retry allowance.

Retry permission does not grant access to any model or bypass CPA authentication.
A file containing only `[retry]` configures retries without enabling model ACL;
requests still reach the upstream for their initial attempt. Model ACL, when
configured, is checked first and can reject a request before any attempt.

## Build and test

```bash
go test ./...
go vet ./...
go build -trimpath -o cpa-guard .
```

The release workflow publishes architecture-specific archives:

```text
cpa-guard_<version>_linux_amd64.tar.gz
cpa-guard_<version>_linux_aarch64.tar.gz
```

Each archive contains the `cpa-guard` binary, `.env.example`, and the installer script.

The included GitHub Actions run formatting/vet/race tests on pull requests and
publish both Linux archives when a `v*` tag is pushed. The release assets are
compatible with the installer URL layout used by the release workflow.

## Install

The installer keeps state under `/home/cliproxy/cpa-guard`:

```bash
sudo ./cpa-guard-install --version 0.1.0-0
sudoedit /home/cliproxy/cpa-guard/.env
sudo systemctl restart cpa-guard
curl -fsS http://127.0.0.1:8217/healthz
```

It does not read or modify the gateway configuration. The installer only creates the `cpa-guard` subtree, `/usr/local/bin/cpa-guard`, and `cpa-guard.service`.
Port `8217` is plain HTTP for the internal ingress-to-host hop; keep it
firewalled to your ingress/network ranges.
