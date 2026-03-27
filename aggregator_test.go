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
	"testing"
	"time"
)

func TestAggregatorIngest(t *testing.T) {
	agg := NewAggregator("pattern-based")
	batch := []*telemetryLog{
		{PIICategoriesFound: []string{"SSN", "EMAIL"}},
		{PIICategoriesFound: []string{"SSN"}},
		{PIICategoriesFound: []string{"EMAIL", "PHONE"}},
	}
	agg.Ingest(batch)

	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.TotalRequestsAudited != 3 {
		t.Fatalf("audited: got %d, want 3", snap.TotalRequestsAudited)
	}
	if snap.TotalPIIDetected != 5 {
		t.Fatalf("pii detected: got %d, want 5", snap.TotalPIIDetected)
	}
	if snap.RedactionEventsByCategory["SSN"] != 2 {
		t.Fatalf("SSN count: got %d, want 2", snap.RedactionEventsByCategory["SSN"])
	}
	if snap.RedactionEventsByCategory["EMAIL"] != 2 {
		t.Fatalf("EMAIL count: got %d, want 2", snap.RedactionEventsByCategory["EMAIL"])
	}
	if snap.RedactionEventsByCategory["PHONE"] != 1 {
		t.Fatalf("PHONE count: got %d, want 1", snap.RedactionEventsByCategory["PHONE"])
	}
}

func TestAggregatorRecordLatency(t *testing.T) {
	agg := NewAggregator("test")

	agg.RecordLatency(1 * time.Millisecond)
	agg.RecordLatency(1 * time.Millisecond)
	agg.RecordLatency(50 * time.Millisecond)
	agg.RecordLatency(99 * time.Millisecond)

	// TotalRequestsProcessed = latencySamples = all proxied requests.
	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.TotalRequestsProcessed != 4 {
		t.Fatalf("processed: got %d, want 4", snap.TotalRequestsProcessed)
	}

	// After snapshot, counters should be zero.
	snap2 := agg.SnapshotAndReset(0, 0, false, "test")
	if snap2.TotalRequestsProcessed != 0 {
		t.Fatalf("after reset: got %d, want 0", snap2.TotalRequestsProcessed)
	}
}

func TestAggregatorComputePercentile(t *testing.T) {
	agg := NewAggregator("test")

	// 100 requests at 1ms each.
	for i := 0; i < 100; i++ {
		agg.RecordLatency(1 * time.Millisecond)
	}
	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.ProxyP50LatencyMS != 1 {
		t.Fatalf("p50: got %d, want 1", snap.ProxyP50LatencyMS)
	}
	if snap.ProxyP99LatencyMS != 1 {
		t.Fatalf("p99: got %d, want 1", snap.ProxyP99LatencyMS)
	}
}

func TestAggregatorComputePercentileMixed(t *testing.T) {
	agg := NewAggregator("test")

	// 90 requests at 1ms, 9 at 5ms, 1 at 50ms.
	for i := 0; i < 90; i++ {
		agg.RecordLatency(1 * time.Millisecond)
	}
	for i := 0; i < 9; i++ {
		agg.RecordLatency(5 * time.Millisecond)
	}
	agg.RecordLatency(50 * time.Millisecond)

	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.ProxyP50LatencyMS != 1 {
		t.Fatalf("p50: got %d, want 1", snap.ProxyP50LatencyMS)
	}
	if snap.ProxyP95LatencyMS != 5 {
		t.Fatalf("p95: got %d, want 5", snap.ProxyP95LatencyMS)
	}
	// p99 = 99th of 100 = 99th request, which is the 9th at 5ms bucket.
	// Cumulative at bucket 1: 90, at bucket 5: 99. target = 99.
	if snap.ProxyP99LatencyMS != 5 {
		t.Fatalf("p99: got %d, want 5", snap.ProxyP99LatencyMS)
	}
}

func TestAggregatorLatencyOverflow(t *testing.T) {
	agg := NewAggregator("test")

	agg.RecordLatency(1 * time.Millisecond)
	agg.RecordLatency(100 * time.Millisecond) // overflow
	agg.RecordLatency(200 * time.Millisecond) // overflow

	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.TotalRequestsProcessed != 3 {
		t.Fatalf("processed: got %d, want 3", snap.TotalRequestsProcessed)
	}
	if snap.LatencyOverflowCount != 2 {
		t.Fatalf("overflow: got %d, want 2", snap.LatencyOverflowCount)
	}
}

func TestAggregatorSnapshotAndReset(t *testing.T) {
	agg := NewAggregator("pattern-based")

	agg.RecordLatency(2 * time.Millisecond)
	agg.Ingest([]*telemetryLog{
		{PIICategoriesFound: []string{"SSN"}},
	})

	snap := agg.SnapshotAndReset(3, 5, true, "v1.0.0")

	// processed = latencySamples = 1 (one RecordLatency call)
	if snap.TotalRequestsProcessed != 1 {
		t.Fatalf("processed: got %d, want 1", snap.TotalRequestsProcessed)
	}
	if snap.TotalRequestsAudited != 1 {
		t.Fatalf("audited: got %d, want 1", snap.TotalRequestsAudited)
	}
	if snap.TotalRequestsDropped != 5 {
		t.Fatalf("dropped: got %d, want 5", snap.TotalRequestsDropped)
	}
	if snap.IOErrors != 3 {
		t.Fatalf("io errors: got %d, want 3", snap.IOErrors)
	}
	if !snap.PartialWindow {
		t.Fatal("expected partial_window=true")
	}
	if snap.SidecarVersion != "v1.0.0" {
		t.Fatalf("version: got %q, want %q", snap.SidecarVersion, "v1.0.0")
	}
	if snap.RedactionLevel != "pattern-based" {
		t.Fatalf("redaction level: got %q, want %q", snap.RedactionLevel, "pattern-based")
	}
	if snap.SchemaVersion != "1.0.0" {
		t.Fatalf("schema version: got %q", snap.SchemaVersion)
	}
	if len(snap.SOC2ControlsSatisfied) != 3 {
		t.Fatalf("soc2 controls: got %d, want 3", len(snap.SOC2ControlsSatisfied))
	}
	if len(snap.PIICategoriesNotCovered) != 4 {
		t.Fatalf("not covered: got %d, want 4", len(snap.PIICategoriesNotCovered))
	}

	// Verify reset.
	snap2 := agg.SnapshotAndReset(0, 0, false, "v1.0.0")
	if snap2.TotalRequestsProcessed != 0 {
		t.Fatalf("after reset, processed: got %d", snap2.TotalRequestsProcessed)
	}
	if snap2.TotalRequestsAudited != 0 {
		t.Fatalf("after reset, audited: got %d", snap2.TotalRequestsAudited)
	}
	if snap2.TotalPIIDetected != 0 {
		t.Fatalf("after reset, pii: got %d", snap2.TotalPIIDetected)
	}
	if len(snap2.RedactionEventsByCategory) != 0 {
		t.Fatalf("after reset, categories: got %d", len(snap2.RedactionEventsByCategory))
	}
	if snap2.LatencyOverflowCount != 0 {
		t.Fatalf("after reset, overflow: got %d", snap2.LatencyOverflowCount)
	}
}

func TestAggregatorDeepCopy(t *testing.T) {
	agg := NewAggregator("test")
	agg.Ingest([]*telemetryLog{
		{PIICategoriesFound: []string{"SSN"}},
	})

	snap := agg.SnapshotAndReset(0, 0, false, "test")

	// Mutate the returned map.
	snap.RedactionEventsByCategory["SSN"] = 999
	snap.RedactionEventsByCategory["INJECTED"] = 1

	// Ingest new data and snapshot again — should be clean.
	agg.Ingest([]*telemetryLog{
		{PIICategoriesFound: []string{"EMAIL"}},
	})
	snap2 := agg.SnapshotAndReset(0, 0, false, "test")

	if snap2.RedactionEventsByCategory["SSN"] != 0 {
		t.Fatalf("SSN leaked from mutated snapshot: got %d", snap2.RedactionEventsByCategory["SSN"])
	}
	if snap2.RedactionEventsByCategory["INJECTED"] != 0 {
		t.Fatal("INJECTED key leaked from mutated snapshot")
	}
	if snap2.RedactionEventsByCategory["EMAIL"] != 1 {
		t.Fatalf("EMAIL: got %d, want 1", snap2.RedactionEventsByCategory["EMAIL"])
	}
}

func TestAggregatorCoverage(t *testing.T) {
	agg := NewAggregator("test")

	// 10 total requests (latencySamples), 7 audited = 70% coverage.
	for i := 0; i < 10; i++ {
		agg.RecordLatency(1 * time.Millisecond)
	}
	batch := make([]*telemetryLog, 7)
	for i := range batch {
		batch[i] = &telemetryLog{}
	}
	agg.Ingest(batch)

	snap := agg.SnapshotAndReset(0, 3, false, "test")
	if snap.TotalRequestsProcessed != 10 {
		t.Fatalf("processed: got %d, want 10", snap.TotalRequestsProcessed)
	}
	if snap.AuditCoveragePercent != 70 {
		t.Fatalf("coverage: got %f, want 70", snap.AuditCoveragePercent)
	}
}

func TestAggregatorCoverageFullAudit(t *testing.T) {
	agg := NewAggregator("test")

	// 5 total requests, 5 audited = 100% coverage.
	for i := 0; i < 5; i++ {
		agg.RecordLatency(1 * time.Millisecond)
	}
	batch := make([]*telemetryLog, 5)
	for i := range batch {
		batch[i] = &telemetryLog{}
	}
	agg.Ingest(batch)

	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.AuditCoveragePercent != 100 {
		t.Fatalf("coverage: got %f, want 100", snap.AuditCoveragePercent)
	}
}

func TestAggregatorCoverageZeroProcessed(t *testing.T) {
	agg := NewAggregator("test")
	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.AuditCoveragePercent != 0 {
		t.Fatalf("coverage with zero processed: got %f, want 0", snap.AuditCoveragePercent)
	}
}

func TestAggregatorConcurrency(t *testing.T) {
	agg := NewAggregator("test")
	var wg sync.WaitGroup

	// 50 goroutines calling Ingest.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := make([]*telemetryLog, 100)
			for j := range batch {
				batch[j] = &telemetryLog{PIICategoriesFound: []string{"SSN"}}
			}
			agg.Ingest(batch)
		}()
	}

	// 100 goroutines calling RecordLatency.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agg.RecordLatency(time.Duration(1) * time.Millisecond)
		}()
	}

	// 10 goroutines calling SnapshotAndReset.
	var totalAudited uint64
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap := agg.SnapshotAndReset(0, 0, false, "test")
			mu.Lock()
			totalAudited += snap.TotalRequestsAudited
			mu.Unlock()
		}()
	}

	wg.Wait()

	// Final snapshot to collect anything remaining.
	finalSnap := agg.SnapshotAndReset(0, 0, false, "test")
	mu.Lock()
	totalAudited += finalSnap.TotalRequestsAudited
	mu.Unlock()

	// 50 goroutines × 100 entries = 5000 total.
	if totalAudited != 5000 {
		t.Fatalf("total audited across all snapshots: got %d, want 5000", totalAudited)
	}
}

func TestAggregatorPercentileEmpty(t *testing.T) {
	agg := NewAggregator("test")
	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.ProxyP50LatencyMS != 0 {
		t.Fatalf("p50 with no data: got %d, want 0", snap.ProxyP50LatencyMS)
	}
	if snap.ProxyP99LatencyMS != 0 {
		t.Fatalf("p99 with no data: got %d, want 0", snap.ProxyP99LatencyMS)
	}
}

func TestAggregatorUptimeIncreases(t *testing.T) {
	agg := NewAggregator("test")
	time.Sleep(10 * time.Millisecond)
	snap := agg.SnapshotAndReset(0, 0, false, "test")
	if snap.SystemUptimeSeconds < 0.01 {
		t.Fatalf("uptime too low: %f", snap.SystemUptimeSeconds)
	}
}
