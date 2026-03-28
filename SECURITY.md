# Security

## Deployment Security

Outband runs as a localhost sidecar proxy. By default, it listens on `127.0.0.1:8080`, meaning only processes on the same host can reach it.

**Kubernetes (same-pod sidecar):** Localhost binding is sufficient. Containers within the same pod share a network namespace.

**Cross-host deployment:** If the proxy must accept connections from a different host, mutual TLS (mTLS) is required. Deploy behind a service mesh (Istio, Linkerd, Envoy) that handles mTLS termination. Outband does not terminate TLS itself.

mTLS support is not built into the MVP. This is tracked as a known requirement for enterprise deployments.

## Threat Model

Outband runs in the same trust boundary as the application it proxies. It has access to:

- All LLM API traffic passing through the proxy, including request bodies and response bodies.
- API keys passed in HTTP headers (e.g., `Authorization: Bearer sk-...`).

This is inherent to the sidecar deployment model. If the sidecar is compromised, the application's API keys are already exposed at the same level. The sidecar does not expand the trust boundary, though it adds a new process and associated parser, configuration, and runtime surfaces.

**Fail-open design:** If the audit pipeline experiences buffer pressure (ring buffer full, worker queue full), the proxy continues forwarding requests without blocking. Audit capture is dropped, not traffic. The drop counter tracks these events for compliance reporting.

## Data Handling

- **In memory only:** Raw request payloads are held in memory (ring buffer blocks, assembler reassembly buffers) during processing. They are never written to disk in raw form.
- **Redaction before persistence:** PII is detected and redacted by the `RedactorChain` before any telemetry entry is written to disk. JSONL files contain redacted payloads only.
- **Customer-controlled retention:** JSONL telemetry files and evidence summaries are written to the customer's own infrastructure. Retention is controlled via `--log-max-files` and `--log-max-age`.
- **No default network egress:** By default, no data is transmitted to Outband's infrastructure. Customers may opt in to webhook delivery (`--webhook-url`), which transmits evidence summaries to an external endpoint when explicitly configured. The open-source build contains no hardcoded external service calls.

## Supported PII Categories and Limitations

Outband detects the following PII categories using pattern-based (regex) matching:

| Category | Pattern | Validation |
|----------|---------|------------|
| SSN | `NNN-NN-NNNN` | Format only |
| Credit Card | Visa, Mastercard, Amex formats | Luhn checksum |
| Email | RFC 5322 simplified | None |
| Phone | US 10-digit with optional `+1` | Format only |
| IPv4 | Dotted quad | Octet range 0-255 |

**Limitations:**
- Pattern-based detection only. No NER, no ML-based entity recognition.
- Only scans extracted content fields (e.g., `messages[*].content`), not structural JSON fields.
- US-centric phone and SSN patterns. International formats are not covered.
- Does not detect names, street addresses, medical records, or other unstructured PII.

## Vulnerability Reporting

Security issues can be reported to **security@outband.dev**.

- **Acknowledgment:** Within 48 hours.
- **Assessment:** Within 7 days.
- **Disclosure:** We follow coordinated disclosure. We will work with reporters to agree on a disclosure timeline before any public communication.

Please do not open public GitHub issues for security vulnerabilities.
