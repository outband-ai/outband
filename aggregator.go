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

package main

import (
	"sync"
	"time"
)

const maxLatencyBucketMS = 100

// EvidenceSummary is the periodic compliance report payload produced once per
// aggregation window. It is the source data for local JSON evidence files and
// any downstream report generation (e.g., PDF via separate tooling).
type EvidenceSummary struct {
	WindowStart               time.Time         `json:"window_start"`
	WindowEnd                 time.Time         `json:"window_end"`
	TotalRequestsProcessed    uint64            `json:"total_requests_processed"`
	TotalRequestsAudited      uint64            `json:"total_requests_audited"`
	TotalRequestsDropped      uint64            `json:"total_requests_dropped"`
	AuditCoveragePercent      float64           `json:"audit_coverage_percent"`
	TotalPIIDetected          uint64            `json:"total_pii_detected"`
	RedactionEventsByCategory map[string]uint64 `json:"redaction_events_by_category"`
	RedactionLevel            string            `json:"redaction_level"`
	PIICategoriesNotCovered   []string          `json:"pii_categories_not_covered"`
	SystemUptimeSeconds       float64           `json:"system_uptime_seconds"`
	ProxyP50LatencyMS         int               `json:"proxy_p50_latency_ms"`
	ProxyP95LatencyMS         int               `json:"proxy_p95_latency_ms"`
	ProxyP99LatencyMS         int               `json:"proxy_p99_latency_ms"`
	LatencyOverflowCount      uint64            `json:"latency_overflow_count"`
	IOErrors                  uint64            `json:"io_errors"`
	PartialWindow             bool              `json:"partial_window"`
	SchemaVersion             string            `json:"schema_version"`
	SidecarVersion            string            `json:"sidecar_version"`
	SOC2ControlsSatisfied     []string          `json:"soc2_controls_satisfied"`
	ISO42001ControlsSatisfied []string          `json:"iso42001_controls_satisfied"`
}

// Aggregator accumulates per-window statistics from telemetry batches and
// latency measurements. All methods are safe for concurrent use.
//
// Entire internal state is guarded by a single sync.Mutex. No atomic
// counters — they are redundant under mutex and cannot provide the atomic
// read-and-clear semantics that SnapshotAndReset requires across 10+ fields.
//
// Fixed-bucket histogram: [100]uint64 array, 800 bytes, zero heap allocation
// regardless of throughput. Bucket i counts latencies in [i, i+1) milliseconds.
type Aggregator struct {
	mu sync.Mutex

	windowStart       time.Time
	startTime         time.Time
	requestsProcessed uint64
	requestsAudited   uint64
	piiDetected       uint64
	eventsByCategory  map[string]uint64
	redactorName      string

	latencyBuckets  [maxLatencyBucketMS]uint64
	latencyOverflow uint64
}

// NewAggregator creates an Aggregator for the given redactor chain name.
func NewAggregator(redactorName string) *Aggregator {
	return &Aggregator{
		windowStart:      time.Now(),
		startTime:        time.Now(),
		eventsByCategory: make(map[string]uint64),
		redactorName:     redactorName,
	}
}

// RecordProcessed records a single request's proxy ingress overhead latency.
// Called from the Rewrite hook on every request (hot path).
func (a *Aggregator) RecordProcessed(latency time.Duration) {
	ms := int(latency.Milliseconds())
	a.mu.Lock()
	a.requestsProcessed++
	if ms >= 0 && ms < maxLatencyBucketMS {
		a.latencyBuckets[ms]++
	} else {
		a.latencyOverflow++
	}
	a.mu.Unlock()
}

// Ingest processes a batch of telemetry logs from the collector's flushFunc.
func (a *Aggregator) Ingest(batch []*telemetryLog) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, entry := range batch {
		a.requestsAudited++
		a.piiDetected += uint64(len(entry.PIICategoriesFound))
		for _, cat := range entry.PIICategoriesFound {
			a.eventsByCategory[cat]++
		}
	}
}

// computePercentile walks the 100-bucket histogram to find the given
// percentile (0.0–1.0). Called under lock. Returns the bucket index (ms).
func (a *Aggregator) computePercentile(target float64) int {
	if a.requestsProcessed == 0 {
		return 0
	}
	targetCount := uint64(float64(a.requestsProcessed) * target)
	if targetCount == 0 {
		targetCount = 1
	}
	var cumulative uint64
	for i := 0; i < maxLatencyBucketMS; i++ {
		cumulative += a.latencyBuckets[i]
		if cumulative >= targetCount {
			return i
		}
	}
	return maxLatencyBucketMS
}

// SnapshotAndReset atomically captures the current window's statistics and
// resets all counters for the next window. The ioErrors and droppedCount
// parameters are passed in from the orchestrating goroutine (they live
// outside the aggregator as atomic counters).
func (a *Aggregator) SnapshotAndReset(ioErrors uint64, droppedCount uint64, partial bool, sidecarVersion string) *EvidenceSummary {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()

	// Deep copy — non-negotiable. Without it, the next window's Ingest
	// writes to a map that the dispatcher is concurrently reading.
	catCopy := make(map[string]uint64, len(a.eventsByCategory))
	for k, v := range a.eventsByCategory {
		catCopy[k] = v
	}

	var coverage float64
	if a.requestsProcessed > 0 {
		coverage = float64(a.requestsAudited) / float64(a.requestsProcessed) * 100
	}

	summary := &EvidenceSummary{
		WindowStart:            a.windowStart,
		WindowEnd:              now,
		TotalRequestsProcessed: a.requestsProcessed,
		TotalRequestsAudited:   a.requestsAudited,
		TotalRequestsDropped:   droppedCount,
		AuditCoveragePercent:   coverage,
		TotalPIIDetected:       a.piiDetected,
		RedactionEventsByCategory: catCopy,
		RedactionLevel:            a.redactorName,
		PIICategoriesNotCovered:   []string{"NAME", "STREET_ADDRESS", "MEDICAL", "NON_US_ID"},
		SystemUptimeSeconds:       now.Sub(a.startTime).Seconds(),
		ProxyP50LatencyMS:         a.computePercentile(0.50),
		ProxyP95LatencyMS:         a.computePercentile(0.95),
		ProxyP99LatencyMS:         a.computePercentile(0.99),
		LatencyOverflowCount:      a.latencyOverflow,
		IOErrors:                  ioErrors,
		PartialWindow:             partial,
		SchemaVersion:             "1.0.0",
		SidecarVersion:            sidecarVersion,
		SOC2ControlsSatisfied:     []string{"CC6.1", "CC6.6", "CC9.2"},
		ISO42001ControlsSatisfied: []string{"A.10.2", "A.10.3", "A.10.4"},
	}

	// Reset for next window.
	a.windowStart = now
	a.requestsProcessed = 0
	a.requestsAudited = 0
	a.piiDetected = 0
	a.eventsByCategory = make(map[string]uint64)
	a.latencyOverflow = 0
	a.latencyBuckets = [maxLatencyBucketMS]uint64{}

	return summary
}
