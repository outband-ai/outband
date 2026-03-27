# outband

A transparent, provider-agnostic reverse proxy for AI APIs. Sits in the middle of your AI application traffic for observation and control without measurable latency impact.

Works with any HTTP API: OpenAI, Anthropic, Stability AI, Venice AI, or your own.

## Quick Start

```bash
go build -o outband .
./outband --target https://api.openai.com
```

Your app talks to `localhost:8080` instead of the API directly. All headers, auth tokens, streaming responses, and error codes pass through untouched.

```bash
# Example: proxy OpenAI traffic
curl -H "Authorization: Bearer $API_KEY" \
     http://localhost:8080/v1/chat/completions \
     -d '{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}'
```

## Docker

Run the proxy with a mock LLM endpoint — no API keys needed:

```bash
docker compose up
```

This starts:
- **proxy** on `localhost:8081` — the outband reverse proxy
- **mockllm** on `localhost:9090` — a mock LLM that echoes OpenAI-style responses with 200ms simulated latency

Try it:

```bash
# Standard request
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}'

# Streaming (SSE)
curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

Check the mockllm container logs to see the forwarded request.

### Mock Configuration

| Environment Variable | Default | Description |
|---|---|---|
| `MOCK_DELAY` | `200ms` | Initial response delay |
| `MOCK_CHUNKS` | `5` | Number of SSE chunks in streaming mode |
| `MOCK_CHUNK_DELAY` | `50ms` | Delay between SSE chunks |

Override via environment: `MOCK_DELAY=500ms docker compose up`

## Flags

| Flag | Default | Description |
|---|---|---|
| `--target` | (required) | Upstream API URL |
| `--listen` | `localhost:8080` | Address to listen on |
| `--version` | | Print version and exit |
| `--api-format` | (auto-detect) | API format: `openai`, `anthropic` |
| `--audit-capacity` | `52428800` (50MB) | Audit buffer pool size (0 to disable) |
| `--audit-block-size` | `65536` (64KB) | Audit buffer block size |
| `--audit-queue-size` | `256` | Audit queue slot count |
| `--max-payload-size` | `10485760` (10MB) | Per-request payload size limit |
| `--stale-timeout` | `30s` | Staleness timeout for incomplete payloads |
| `--workers` | `GOMAXPROCS` | Redaction worker goroutines |
| `--worker-queue-size` | `64` | Assembler → worker channel buffer |
| `--collector-queue-size` | `128` | Worker → collector channel buffer |
| `--batch-size` | `1000` | Collector batch size before flush |
| `--flush-interval` | `5s` | Collector flush interval |
| `--drop-poll-interval` | `5s` | Drop counter polling interval |

All numeric and duration flags are validated at startup. Invalid values (zero durations, negative sizes) produce clear error messages and a non-zero exit code.

## Health Probes

| Endpoint | Purpose | 200 when |
|---|---|---|
| `GET /healthz` | Liveness probe | Server goroutine is running (always 200) |
| `GET /readyz` | Readiness probe | Audit pipeline is initialized AND upstream target is TCP-reachable |

If audit is disabled (`--audit-capacity=0`), `/readyz` only checks upstream reachability.

## What Gets Proxied

Everything. Outband is a transparent L7 proxy:

- Request/response headers (including auth, custom headers)
- SSE streaming (`text/event-stream`) with immediate flush
- Chunked transfer encoding
- WebSocket upgrades
- Large payloads (image uploads, 100k+ token contexts)
- Query strings (verbatim, no re-encoding)
- Error responses (429, 500, etc. passed through as-is)

What it does **not** do: inject `X-Forwarded-*` headers, add a `User-Agent`, modify request/response bodies, or buffer uploads.

## Benchmarks

All benchmarks run on Apple M2, Go 1.26.1, macOS. Upstream is a local `httptest.Server` to isolate proxy overhead from network variance.

### Sequential (direct vs proxied, instant upstream response)

```
goos: darwin
goarch: arm64
pkg: outband
cpu: Apple M2
BenchmarkProxySequential/direct-8            29500      39265 ns/op    6.44 MB/s    5966 B/op     68 allocs/op
BenchmarkProxySequential/proxied-8           14145      90115 ns/op    2.81 MB/s   13226 B/op    143 allocs/op
```

Latency percentiles (n=10,000):

|  | direct | proxied | overhead |
|---|---|---|---|
| p50 | 38us | 81us | **43us** |
| p95 | 57us | 108us | **51us** |
| p99 | 83us | 188us | **105us** |

### Concurrent (b.RunParallel, 75ms simulated upstream TTFB, ~2KB response)

```
BenchmarkProxyConcurrent/direct-8            1202    9651835 ns/op    0.21 MB/s    9953 B/op     87 allocs/op
BenchmarkProxyConcurrent/proxied-8           1194    9688215 ns/op    0.21 MB/s   16410 B/op    155 allocs/op
```

Latency percentiles (n~1,200, dominated by 75ms upstream sleep):

|  | direct | proxied | overhead |
|---|---|---|---|
| p50 | 76.55ms | 76.97ms | **0.42ms** |
| p95 | 78.51ms | 78.53ms | **0.02ms** |
| p99 | 80.76ms | 81.25ms | **0.49ms** |

GC: 12 cycles / 2.3ms total STW pause during proxied run (155 allocs/op).

### Streaming SSE (50 concurrent clients, 50 chunks at 5ms intervals, 75ms initial delay)

```
BenchmarkProxyStreaming/direct-8             2605     447416 ns/op   37417 B/op    154 allocs/op
BenchmarkProxyStreaming/proxied-8            1810     645613 ns/op   93004 B/op    310 allocs/op
```

Inter-chunk latency percentiles (n~10,000):

|  | direct | proxied | overhead |
|---|---|---|---|
| p50 | 5.19ms | 5.12ms | **-0.07ms** |
| p95 | 6.03ms | 6.46ms | **0.43ms** |
| p99 | 7.01ms | 7.91ms | **0.90ms** |

## PII Redaction

Outband automatically scans request payloads for personally identifiable information (PII) and replaces matches with redaction markers before any telemetry is persisted. The upstream request is never modified — redaction applies only to the audit copy.

### Supported PII Categories

| Category | Pattern | Validation | Marker |
|---|---|---|---|
| SSN | `NNN-NN-NNNN` | — | `[REDACTED:SSN:CC6.1]` |
| Credit Card | Visa and Mastercard (16 digits), American Express (15 digits) | Luhn checksum | `[REDACTED:CC_NUMBER:CC6.1]` |
| Email | RFC 5322 simplified `local@domain.tld` | — | `[REDACTED:EMAIL:CC6.1]` |
| Phone (US) | 10-digit with optional `+1` country code | — | `[REDACTED:PHONE:CC6.1]` |
| IPv4 | Dotted quad `N.N.N.N` | Octet range 0–255 | `[REDACTED:IP_ADDRESS:CC6.1]` |

The `CC6.1` tag maps to SOC 2 Common Criteria control 6.1 (Logical and Physical Access Controls).

### What Is Scanned

Only natural-language content fields extracted from the request body:

| Provider | Extracted paths |
|---|---|
| OpenAI | `messages[*].content` (string or multimodal array) |
| Anthropic | `system` (string or content block array), `messages[*].content` |
| SSE streams | Delta content concatenated per choice/block before scanning |

### What Is NOT Scanned

- **Structural JSON fields**: model names, token counts, IDs (`workspace_id`, `request_id`), temperature, and all other configuration parameters. This is by design — scanning structural fields produces false positives (e.g., a 16-digit workspace ID triggering credit card detection).
- **HTTP headers**: Authorization tokens, cookies, custom headers.
- **URL paths and query parameters**.
- **Response bodies** (future work).

### What Is NOT Caught

Pattern-based redaction (`redaction_level: "pattern-based"`) catches structured PII with well-defined formats. It does **not** catch:

- Names mentioned in prose
- Street addresses
- Medical information
- Company-proprietary data
- Any unstructured PII without a recognizable pattern

A future `"nlp-based"` redaction level will address unstructured PII categories.

## Architecture

- `httputil.ReverseProxy` with `Rewrite` (not deprecated `Director`)
- `http.Transport` configured for HTTP/2 (`ForceAttemptHTTP2`), connection pooling (1000 idle / 100 per host), and transparent compression passthrough
- `sync.Pool` buffer pool to reduce GC pressure (32KB buffers)
- SSE auto-detection: stdlib flushes immediately for `text/event-stream` and chunked responses
- Audit pipeline: TeeReader → ring buffer → session assembler → worker pool (extract + redact + hash) → double-buffered collector

## Versioning

Build version is injected at compile time via ldflags:

```bash
go build -ldflags "-X main.version=v1.2.3" -o outband .
```

The Docker build accepts a `VERSION` build arg:

```bash
docker build --build-arg VERSION=v1.2.3 --target proxy -t outband:v1.2.3 .
```

Check the running version:

```bash
./outband --version
```

Every telemetry log entry includes a `version` field so auditors can trace which sidecar version generated each record. Entries from different versions are forward-compatible.

### Upgrade Process

1. Pull the new image: `docker pull outband:v1.1.0`
2. Restart the container (or update the K8s deployment image tag)
3. Verify with `/healthz` — returns 200 once the proxy is listening and upstream is reachable
4. Verify with `/readyz` — returns 200 once the audit pipeline is initialized
5. Confirm telemetry entries show the new `version` field

## Tests

```bash
go test -v            # functional tests
go test -bench=. -v   # benchmarks
```

## License

Apache 2.0
