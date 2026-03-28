# Configuration Reference

All configuration is via command-line flags. Run `outband --help` for the full list.

## Proxy

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--target` | string | (required) | Upstream API URL (e.g., `https://api.openai.com`). Must have `http` or `https` scheme. |
| `--listen` | string | `localhost:8080` | Address the proxy listens on. Use `0.0.0.0:8080` for Docker or Kubernetes. |
| `--api-format` | string | auto-detect | API payload format: `openai` or `anthropic`. Auto-detected from `--target` hostname if omitted. |
| `--version` | bool | `false` | Print version and exit. |
| `--shutdown-drain` | duration | `10s` | HTTP connection drain budget during graceful shutdown. Increase if serving long SSE streams. |

## Audit Pipeline

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--audit-capacity` | int | `52428800` (50MB) | Total ring buffer pool size in bytes. Set to `0` to disable the audit pipeline entirely. |
| `--audit-block-size` | int | `65536` (64KB) | Individual buffer block size. Must be ≤ `--audit-capacity`. |
| `--audit-queue-size` | int | `256` | Async queue slot count between TeeReader and assembler. |
| `--max-payload-size` | int | `10485760` (10MB) | Per-request payload size limit. Payloads exceeding this are dropped from audit (not blocked). |
| `--stale-timeout` | duration | `30s` | Timeout for incomplete payload reassembly. Payloads with missing blocks are evicted after this. |
| `--workers` | int | `GOMAXPROCS` | Number of redaction worker goroutines. Defaults to available CPU cores. |
| `--worker-queue-size` | int | `64` | Channel buffer size between assembler and workers. |
| `--collector-queue-size` | int | `128` | Channel buffer size between workers and collector. |
| `--batch-size` | int | `1000` | Collector batch size before flush. |
| `--flush-interval` | duration | `5s` | Collector flush interval. Batches are flushed on whichever trigger fires first: batch size or interval. |
| `--drop-poll-interval` | duration | `5s` | Interval for polling and logging the drop counter. |

## Telemetry Output

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--log-dir` | string | `.` | Directory for JSONL telemetry logs. Created if it does not exist. |
| `--log-max-size` | int64 | `104857600` (100MB) | Max JSONL file size in bytes before rotation. |
| `--log-max-age` | duration | `1h` | Max JSONL file age before rotation. |
| `--log-max-files` | int | `24` | Max rotated JSONL files to retain. Oldest files are deleted when this limit is exceeded. |

## Evidence Reporting

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--evidence-dir` | string | `.` | Directory for JSON evidence summaries. Created if it does not exist. |
| `--summary-interval` | duration | `60m` | Interval between evidence summary reports. Use `1m` for testing. |

## External Targets

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--webhook-url` | string | (none) | URL for evidence summary webhook delivery. When set, each evidence summary is POST-ed as JSON. |
| `--webhook-headers` | string | (none) | Comma-separated `key=value` headers for the webhook (e.g., `Authorization=Bearer token,X-Custom=val`). |

## Metrics

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--metrics-port` | int | `9090` | Port for the Prometheus metrics server. Serves `/metrics` endpoint. |

## Example Configurations

### Local Development

```bash
./outband \
  --target http://localhost:9090 \
  --listen localhost:8080 \
  --summary-interval 1m \
  --flush-interval 1s \
  --log-dir ./outband-logs \
  --evidence-dir ./outband-evidence
```

### Kubernetes Sidecar

```yaml
containers:
  - name: outband
    image: outband:latest
    args:
      - "--target"
      - "https://api.openai.com"
      - "--listen"
      - "0.0.0.0:8080"
      - "--log-dir"
      - "/data/logs"
      - "--evidence-dir"
      - "/data/evidence"
      - "--metrics-port"
      - "9091"
    ports:
      - containerPort: 8080
      - containerPort: 9091
    volumeMounts:
      - name: outband-data
        mountPath: /data
```

### Docker Standalone

```bash
docker run -d \
  -p 8080:8080 \
  -p 9090:9090 \
  -v $(pwd)/outband-data:/data \
  outband:latest \
  --target https://api.openai.com \
  --listen 0.0.0.0:8080 \
  --log-dir /data/logs \
  --evidence-dir /data/evidence
```

### Air-Gapped Production

No webhook, no external calls. Evidence stays on local disk:

```bash
./outband \
  --target https://internal-llm.corp.example.com \
  --listen 0.0.0.0:8080 \
  --audit-capacity 104857600 \
  --workers 4 \
  --log-dir /var/lib/outband/logs \
  --log-max-size 524288000 \
  --log-max-files 48 \
  --evidence-dir /var/lib/outband/evidence \
  --summary-interval 60m
```
