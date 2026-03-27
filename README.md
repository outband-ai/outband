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

## Flags

| Flag | Default | Description |
|---|---|---|
| `--target` | (required) | Upstream API URL |
| `--listen` | `localhost:8080` | Address to listen on |

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

## Architecture

- `httputil.ReverseProxy` with `Rewrite` (not deprecated `Director`)
- `http.Transport` configured for HTTP/2 (`ForceAttemptHTTP2`), connection pooling (1000 idle / 100 per host), and transparent compression passthrough
- `sync.Pool` buffer pool to reduce GC pressure (32KB buffers)
- SSE auto-detection: stdlib flushes immediately for `text/event-stream` and chunked responses
- Zero external dependencies -- stdlib only

## Tests

```bash
go test -v            # functional tests
go test -bench=. -v   # benchmarks
```

7 functional tests cover:
- Header transparency (no User-Agent leak, no X-Forwarded injection, auth preservation, hop-by-hop suppression)
- Protocol fidelity (SSE boundary preservation, 50MB streaming upload, query string verbatim)
- Lifecycle (upstream timeout, mid-stream disconnect propagation, error passthrough)

## License

Apache 2.0
