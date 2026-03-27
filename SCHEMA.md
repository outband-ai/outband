# Outband Evidence Schemas

Version: 1.0.0

This document defines the two output schemas produced by the Outband sidecar
audit pipeline. Each field is mapped to the compliance control it satisfies.

---

## Schema 1: Raw Telemetry Record (JSONL)

One JSON object per line. Data-plane output consumed by the customer's existing
log infrastructure. Outband does not ship these records anywhere.

Corresponds to the `telemetryLog` struct in `worker.go`.

| Field | Type | Description | Compliance Justification |
|---|---|---|---|
| `request_id` | uint64 | Links back to the session assembler's original request. Monotonically increasing per sidecar instance. | **CC6.1** — Unique identifier enabling correlation of access events across the audit trail. |
| `timestamp` | string (RFC 3339 / Go time.Time JSON encoding) | Time of capture. | **CC6.1** — Temporal ordering of access events for forensic reconstruction. **CC9.2** — Time-correlated evidence of continuous risk monitoring. |
| `original_hash` | string (SHA-256 hex) | Hash of raw payload before redaction. | **CC6.1** — Cryptographic proof of capture integrity; enables detection of tampering between capture and audit review. **ISO 42001** — Non-repudiation for AI transaction records. |
| `redacted_payload` | string | Content fields with PII markers applied. JSON object mapping extraction path to redacted text. | **CC6.1** — Demonstrates active data classification and tagging. **CC9.2** — Evidence that sensitive data exposure is mitigated before downstream consumption. |
| `redacted_hash` | string (SHA-256 hex) | Hash of redacted payload concatenated with nanosecond timestamp. | **CC6.1** — Tamper-evident seal on redacted output; timestamp binding prevents replay attacks. **ISO 42001** — Cryptographic evidence chain for AI data governance. |
| `redaction_level` | string | Output of `RedactorChain.Name()`. Identifies which redaction pipeline processed this record. | **CC6.1** — Audit trail of which access control policy was in effect. **CC9.2** — Documents the specific risk mitigation mechanism applied. |
| `pii_categories_found` | []string | Deduplicated, sorted PII types detected (e.g., `SSN`, `CC_NUMBER`, `EMAIL`, `PHONE`, `IP_ADDRESS`). | **CC6.1** — Data classification evidence: which categories of sensitive data were identified. **CC9.2** — Quantifies risk exposure per data category. |
| `extractor_used` | string | Which payload extractor processed this request (`openai`, `anthropic`, `unknown`). | **CC6.6** — Documents system boundary: which external AI API was intercepted. **ISO 42001** — Identifies the AI system whose data flow is governed. |
| `fields_scanned` | int | Count of content fields extracted and scanned by the redactor chain. | **CC6.6** — Boundary protection depth: quantifies inspection coverage per request. |
| `capture_complete` | bool | Whether full payload was captured (`true`) or abandoned mid-stream due to buffer pressure (`false`). | **CC6.6** — Transparency about inspection completeness. A `false` value is an explicit scope limitation for that request. **CC9.2** — Enables risk assessment of uninspected traffic. |

### Example Record

```json
{"request_id":42,"timestamp":"2026-03-27T14:30:22.123456789Z","original_hash":"a1b2c3...","redacted_payload":"{\"messages[0].content\":\"Hello, my SSN is [REDACTED:SSN:CC6.1]\"}","redacted_hash":"d4e5f6...","redaction_level":"pattern-based","pii_categories_found":["SSN"],"extractor_used":"openai","fields_scanned":3,"capture_complete":true}
```

---

## Schema 2: Evidence Summary (JSON)

A single structured summary produced once per aggregation window (default: 60
minutes). This summary is the source data for both the local JSON evidence file
and any downstream evidence report generation (e.g., PDF via separate tooling).

| Field | Type | Description | Compliance Justification |
|---|---|---|---|
| `window_start` | string (RFC 3339 / Go time.Time JSON encoding) | Beginning of the aggregation window. | **CC6.1** — Defines the temporal scope of this evidence artifact. |
| `window_end` | string (RFC 3339 / Go time.Time JSON encoding) | End of the aggregation window. | **CC6.1** — Defines the temporal scope of this evidence artifact. |
| `total_requests_processed` | uint64 | Total HTTP requests that transited the proxy during the window. | **CC6.6** — Quantifies total traffic volume at the system boundary. |
| `total_requests_audited` | uint64 | Requests that completed the full audit pipeline (extraction → redaction → logging). | **CC6.1** — Quantifies the scope of access event logging. **CC6.6** — Demonstrates inspection depth at the boundary. |
| `total_requests_dropped` | uint64 | Requests where audit capture was abandoned due to buffer pressure. | **CC6.6** — Transparency about uninspected traffic. **CC9.2** — Quantifies risk from dropped audit coverage. |
| `audit_coverage_percent` | float64 | `audited / processed * 100`. The single most important compliance metric. | **CC6.1, CC6.6, CC9.2** — Aggregate measure of control effectiveness. Auditors look at this number first. |
| `total_pii_detected` | uint64 | Total PII instances identified across all audited requests. | **CC6.1** — Aggregate data classification output. **CC9.2** — Quantifies total risk exposure detected. |
| `redaction_events_by_category` | map[string]uint64 | PII detection counts broken down by category (e.g., `{"SSN": 10, "EMAIL": 25}`). | **CC6.1** — Per-category data classification evidence. **CC9.2** — Enables risk assessment by data sensitivity tier. |
| `redaction_level` | string | Redaction pipeline identifier (output of `RedactorChain.Name()`). | **CC6.1** — Documents which access control policy was in effect during this window. |
| `pii_categories_not_covered` | []string | Explicit scope limitations: PII types the system cannot detect (e.g., names, addresses, medical info). | **CC9.2** — Proactive risk disclosure. Auditors specifically ask about coverage boundaries. Listing limitations builds credibility. |
| `system_uptime_seconds` | float64 | Sidecar process uptime during this window. | **CC6.6** — Availability evidence for the boundary control. Demonstrates the proxy was operational during the reporting period. |
| `proxy_p50_latency_ms` | int | 50th percentile proxy ingress overhead in milliseconds. Measures time from request arrival to upstream dial initiation — NOT round-trip. | **CC6.6** — Performance evidence that the boundary control does not degrade system availability. |
| `proxy_p95_latency_ms` | int | 95th percentile proxy ingress overhead. | **CC6.6** — Tail latency evidence for boundary control performance. |
| `proxy_p99_latency_ms` | int | 99th percentile proxy ingress overhead. | **CC6.6** — Worst-case latency evidence. If under 2ms, demonstrates zero-impact boundary protection. |
| `latency_overflow_count` | uint64 | Requests with ingress overhead ≥100ms (histogram overflow). Should be 0 under normal operation. | **CC6.6** — Anomaly indicator. Non-zero values suggest proxy misconfiguration or resource contention. |
| `io_errors` | uint64 | JSONL write failures during this window (disk full, permission errors). | **CC9.2** — Transparency about evidence persistence failures. Non-zero values mean some raw telemetry was not written to disk. |
| `partial_window` | bool | `true` if this summary covers an incomplete aggregation period (e.g., graceful shutdown). | **CC6.1** — Explicitly marks evidence artifacts with reduced temporal scope so auditors know the window is incomplete. |
| `schema_version` | string | Schema version identifier (e.g., `"1.0.0"`). | **CC6.1** — Enables forward-compatible evidence parsing. Auditors can verify which schema version produced the artifact. |
| `sidecar_version` | string | Sidecar binary version (set via build-time ldflags). | **CC6.1** — Ties evidence to a specific software release for reproducibility. |
| `soc2_controls_satisfied` | []string | SOC 2 controls this evidence artifact addresses (e.g., `["CC6.1", "CC6.6", "CC9.2"]`). | Self-referential: explicitly declares which controls this summary provides evidence for. |
| `iso42001_controls_satisfied` | []string | ISO 42001 controls this evidence artifact addresses. | Self-referential: maps evidence to AI governance framework requirements. |

### SOC 2 Controls Mapping

**CC6.1 — Logical Access Controls:**
All outbound LLM API requests are intercepted and logged. PII is detected and
tagged in the audit trail. `total_requests_processed` quantifies monitored
traffic. `redaction_events_by_category` demonstrates active pattern enforcement.
Cryptographic hashes (`original_hash`, `redacted_hash`) provide tamper-evident
evidence integrity.

**CC6.6 — System Boundary Protection:**
The sidecar proxy operates as a network boundary control between the application
and external LLM APIs. `audit_coverage_percent` shows what fraction of traffic
was inspected. `proxy_p99_latency_ms` confirms the boundary control does not
degrade system performance. `capture_complete` flags per-request inspection gaps.

**CC9.2 — Risk Mitigation:**
Automated PII redaction reduces data exposure risk. `total_pii_detected`
quantifies identified risk. Pattern-based detection covers: SSNs, credit card
numbers (Luhn-validated), email addresses, phone numbers (US formats), IPv4
addresses. `pii_categories_not_covered` explicitly documents scope limitations
(names, medical info, addresses, non-US formats).

### ISO 42001 Controls Mapping

**AI Data Governance:**
Continuous monitoring of AI system inputs demonstrates governance over AI-related
data flows. Cryptographic evidence chain (SHA-256 hashing) provides
non-repudiation for AI transaction records. Per-request audit trail enables
forensic analysis of AI system behavior.

### Latency Measurement Clarification

`proxy_p50_latency_ms`, `proxy_p95_latency_ms`, and `proxy_p99_latency_ms`
measure **proxy ingress overhead only** — the time between HTTP request arrival
and upstream dial initiation. This captures request parsing, TeeReader setup,
body cloning initialization, and header mutation. It does **not** include
upstream response time, SSE streaming duration, or network round-trip.

Expected values: sub-2ms under normal load. The fixed-bucket histogram
(`[100]uint64`, 0–99ms) stores these measurements with zero heap allocation.
Latencies ≥100ms are counted in `latency_overflow_count` but do not corrupt
the histogram.

### Example Summary

```json
{
  "window_start": "2026-03-27T14:00:00Z",
  "window_end": "2026-03-27T15:00:00Z",
  "total_requests_processed": 12847,
  "total_requests_audited": 12831,
  "total_requests_dropped": 16,
  "audit_coverage_percent": 99.88,
  "total_pii_detected": 347,
  "redaction_events_by_category": {
    "SSN": 12,
    "CC_NUMBER": 3,
    "EMAIL": 198,
    "PHONE": 89,
    "IP_ADDRESS": 45
  },
  "redaction_level": "pattern-based",
  "pii_categories_not_covered": ["NAME", "STREET_ADDRESS", "MEDICAL", "NON_US_ID"],
  "system_uptime_seconds": 3600.0,
  "proxy_p50_latency_ms": 0,
  "proxy_p95_latency_ms": 1,
  "proxy_p99_latency_ms": 1,
  "latency_overflow_count": 0,
  "io_errors": 0,
  "partial_window": false,
  "schema_version": "1.0.0",
  "sidecar_version": "v0.4.0",
  "soc2_controls_satisfied": ["CC6.1", "CC6.6", "CC9.2"],
  "iso42001_controls_satisfied": ["A.10.2", "A.10.3", "A.10.4"]
}
```
