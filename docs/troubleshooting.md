# Troubleshooting

## JSONL telemetry files are empty

The audit pipeline batches entries before flushing to disk. The default `--flush-interval` is 5 seconds, and the default `--batch-size` is 1000 entries.

**Fix:** Send more requests, or reduce the flush interval for testing:

```bash
./outband --target ... --flush-interval 1s
```

## Drop counter is non-zero

The ring buffer is full. Payloads are being dropped from the audit pipeline (not from traffic — the proxy continues forwarding).

**Causes:**
- `--audit-capacity` is too small for the traffic volume
- `--workers` is insufficient — redaction workers can't keep up

**Fix:** Increase buffer capacity or worker count:

```bash
./outband --target ... --audit-capacity 104857600 --workers 8
```

If drops are sustained (not just a burst), the pipeline is chronically underprovisioned.

## Evidence summary shows 0 requests processed

The proxy is not receiving traffic, or the `--summary-interval` has not elapsed.

**Check:**
1. Verify the proxy is running: `curl http://localhost:8080/healthz`
2. Verify your application's base URL points to the proxy
3. Check if the readiness probe passes: `curl http://localhost:8080/readyz`
4. Reduce the summary interval for testing: `--summary-interval 1m`

## Proxy adds more than 2ms overhead

The proxy should add sub-millisecond overhead for most requests. Higher overhead typically indicates:

- **High CPU load** on the host. The proxy and audit pipeline share CPU with the application.
- **GC pressure** from very large payloads (>10MB per request). The ring buffer is pre-allocated to avoid GC, but payload reassembly uses heap memory.
- **Extreme concurrency** (>50,000 RPS). At very high concurrency, mutex contention in the collector can become measurable.

**Note:** The proxy measures overhead as the time from request arrival to upstream dial initiation. It does not include upstream response time or SSE stream duration.

## PDF evidence report not generated

PDF generation requires the enterprise binary (`outband-enterprise`). The open-source binary generates JSON evidence summaries only.

See [outband.dev/pricing](https://outband.dev/pricing) for enterprise features.

## JSONL file appears corrupted or has unparseable lines

This should not happen under normal operation. The JSONL writer serializes each entry as a complete JSON object followed by a newline, and rotation happens before batch writes begin — never mid-entry.

If you encounter this, it may indicate:
- The process was killed with `SIGKILL` (not `SIGTERM`) during a write
- Disk full during write
- Filesystem corruption

**Action:** File a bug at [github.com/outband-ai/outband/issues](https://github.com/outband-ai/outband/issues) with the corrupted file.

## Demo script fails with missing dependency

The demo script requires `jq`, `curl`, and `docker`. Install the missing tool:

```bash
# macOS
brew install jq curl docker

# Ubuntu/Debian
apt-get install jq curl docker.io
```

## Evidence summary latency shows unexpectedly high values

The `proxy_p50_latency_ms`, `proxy_p95_latency_ms`, and `proxy_p99_latency_ms` fields measure **proxy ingress overhead only** — the time from HTTP request arrival to upstream TCP dial initiation.

If these values are high (>5ms), check:
- Host CPU load
- Whether another process is contending on the same port

If you're seeing high values when measuring externally (e.g., via `curl` timing), you're measuring round-trip time including upstream response latency, not proxy overhead. The evidence summary's latency fields are the authoritative measurement.

## Readiness probe fails but healthz passes

`/healthz` only checks that the process is alive. `/readyz` additionally verifies:

1. The audit pipeline is initialized
2. The upstream target is TCP-reachable (2-second dial timeout)

If `/readyz` fails, check that `--target` is correct and the upstream API is reachable from the proxy host.

## Webhook delivery fails

The webhook target retries up to 3 times on 5xx errors with exponential backoff (500ms, 1s, 2s). 4xx errors are not retried.

**Check:**
- The `--webhook-url` is correct and reachable
- Custom headers in `--webhook-headers` are properly formatted: `Key=Value,Key2=Value2`
- The receiving endpoint accepts `Content-Type: application/json` POST requests
