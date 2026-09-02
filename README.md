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
