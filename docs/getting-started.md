# Getting Started

Get from zero to a working compliance proxy in under 10 minutes.

## Prerequisites

- **Docker** (recommended) or **Go 1.26+** for building from source
- An OpenAI or Anthropic API key (or use the included mock endpoint for testing)

## Quick Start

```bash
git clone https://github.com/outband-ai/outband.git
cd outband
docker compose up -d
```

This starts:
- **mockllm** on port 9090 — a mock OpenAI-compatible endpoint
- **proxy** on port 8081 — the Outband sidecar proxy forwarding to mockllm

## Send Your First Request

```bash
curl http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello world"}]
  }'
```

The proxy forwards the request to the mock endpoint and returns the response. Behind the scenes, the audit pipeline captures the request payload, scans for PII, and writes a telemetry entry.

## Check Telemetry

```bash
cat outband-telemetry-current.jsonl | jq .
```

Each line is a JSON object containing:
- `original_hash` — SHA-256 of the raw payload
- `redacted_payload` — the payload with any PII replaced by `[REDACTED:CATEGORY:CC6.1]` tags
- `pii_categories_found` — list of detected PII types
- `capture_complete` — whether the full payload was captured

## Run the Demo

```bash
./demo.sh
```

The demo script sends requests through the proxy and displays the full pipeline: PII detection, telemetry logging, and evidence summary generation. Use `--record` for screen recording pauses.

## Point Your Application at Outband

Change your application's base URL to point to the Outband proxy instead of the API provider directly.

**Environment variable:**

```bash
export OPENAI_API_BASE=http://localhost:8081
```

**OpenAI Python SDK:**

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8081/v1")
```

**LangChain:**

```python
from langchain_openai import ChatOpenAI
llm = ChatOpenAI(openai_api_base="http://localhost:8081/v1")
```

**Anthropic Python SDK:**

```python
from anthropic import Anthropic
client = Anthropic(base_url="http://localhost:8081")
```

When targeting a real API provider, start the proxy with `--target`:

```bash
./outband --target https://api.openai.com --listen localhost:8080
```

Then point your application at `http://localhost:8080`.

## View Your First Evidence Summary

Evidence summaries are generated at a configurable interval (default: 60 minutes). For testing, use a shorter interval:

```bash
./outband --target https://api.openai.com --summary-interval 1m --evidence-dir ./evidence
```

After the interval elapses, check the evidence directory:

```bash
ls evidence/
cat evidence/evidence-*.json | jq .
```

The evidence summary contains aggregate statistics: total requests processed, audit coverage percentage, PII detection counts by category, proxy latency percentiles, and SOC 2 controls satisfied.

## Next Steps

- [Configuration Reference](configuration.md) — all flags and tuning options
- [Troubleshooting](troubleshooting.md) — common issues and fixes
