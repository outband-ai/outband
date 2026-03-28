// Copyright 2026 The Outband Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package outband

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// TelemetryLog is the output record from the processing pipeline.
type TelemetryLog struct {
	RequestID          uint64    `json:"request_id"`
	Timestamp          time.Time `json:"timestamp"`
	Version            string    `json:"version"`
	OriginalHash       string    `json:"original_hash"`
	RedactedPayload    string    `json:"redacted_payload"`
	RedactedHash       string    `json:"redacted_hash"`
	RedactionLevel     string    `json:"redaction_level"`
	PIICategoriesFound []string  `json:"pii_categories_found"`
	ExtractorUsed      string    `json:"extractor_used"`
	FieldsScanned      int       `json:"fields_scanned"`
	CaptureComplete    bool      `json:"capture_complete"`

	// Response fields — populated only by enterprise binary.
	ResponseRedactedPayload string   `json:"response_redacted_payload,omitempty"`
	ResponseHash            string   `json:"response_hash,omitempty"`
	ResponsePIICategories   []string `json:"response_pii_categories,omitempty"`
	ResponseCaptureComplete bool     `json:"response_capture_complete,omitempty"`
	ResponseExtractorUsed   string   `json:"response_extractor_used,omitempty"`
}

// workerStats exposes atomic counters for observability.
type workerStats struct {
	resultDropped atomic.Int64
}

// startWorkers launches numWorkers goroutines that process assembled payloads.
// The RedactorChain is shared across all workers (it is stateless and safe
// for concurrent use). Workers exit when input is closed. Returns a function
// that blocks until all workers have exited.
func startWorkers(numWorkers int, input <-chan *assembledPayload, output chan<- *TelemetryLog, chain *RedactorChain, stats *workerStats) (wait func()) {
	var wg sync.WaitGroup
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for payload := range input {
				entry := processPayload(payload, chain)
				// Non-blocking send: if output is full, drop the entry.
				select {
				case output <- entry:
				default:
					stats.resultDropped.Add(1)
				}
			}
		}()
	}
	return wg.Wait
}

// processPayload is the core per-request processing function.
// It extracts content fields, redacts PII via the chain, computes hashes,
// and builds the telemetry log entry.
func processPayload(p *assembledPayload, chain *RedactorChain) *TelemetryLog {
	now := time.Now()
	originalHash := hashPayload(p.data)

	// For response-direction payloads, check the extractor registry first
	// (populated by enterprise module via RegisterExtractor). Fall back to
	// the built-in API-type extractor which already handles SSE.
	var ext PayloadExtractor
	if p.direction == DirectionResponse {
		ext = ExtractorForDirection(DirectionResponse, extractorName(p.apiType))
	}
	if ext == nil {
		ext = extractorForAPI(p.apiType)
	}
	var fields []ContentField
	if ext != nil {
		fields = ext.ExtractContent(p.data)
	}

	var allCategories []PIICategory
	var redactedFields []ContentField
	for _, f := range fields {
		text, cats := chain.Redact(f.Text)
		redactedFields = append(redactedFields, ContentField{
			Text:  text,
			Path:  f.Path,
			Index: f.Index,
		})
		allCategories = append(allCategories, cats...)
	}

	redactedPayload := buildRedactedPayload(redactedFields)
	redactedHash := hashRedacted(redactedPayload, now)

	// Deduplicate categories across all fields.
	catSet := make(map[PIICategory]struct{})
	for _, c := range allCategories {
		catSet[c] = struct{}{}
	}
	cats := make([]string, 0, len(catSet))
	for c := range catSet {
		cats = append(cats, string(c))
	}
	sort.Strings(cats)

	return &TelemetryLog{
		RequestID:          p.requestID,
		Timestamp:          now,
		Version:            Version,
		OriginalHash:       originalHash,
		RedactedPayload:    redactedPayload,
		RedactedHash:       redactedHash,
		RedactionLevel:     chain.Name(),
		PIICategoriesFound: cats,
		ExtractorUsed:      extractorName(p.apiType),
		FieldsScanned:      len(fields),
		CaptureComplete:    p.complete,
	}
}

// hashPayload computes the SHA-256 hex digest of a byte slice.
func hashPayload(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// hashRedacted computes the SHA-256 hex digest of the redacted payload
// concatenated with the nanosecond-precision timestamp.
func hashRedacted(payload string, ts time.Time) string {
	h := sha256.New()
	h.Write([]byte(payload))
	h.Write([]byte(ts.Format(time.RFC3339Nano)))
	return hex.EncodeToString(h.Sum(nil))
}

// buildRedactedPayload serializes extracted-and-redacted content fields
// as a JSON object mapping path -> redacted text.
func buildRedactedPayload(fields []ContentField) string {
	if len(fields) == 0 {
		return "{}"
	}
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f.Path] = f.Text
	}
	b, err := json.Marshal(m)
	if err != nil {
		log.Printf("buildRedactedPayload: marshal error: %v", err)
		return "{}"
	}
	return string(b)
}
