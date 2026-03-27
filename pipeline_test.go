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
	"strings"
	"sync"
	"testing"
	"time"
)

// collectTelemetry runs the full pipeline (assembler → workers → collector)
// and returns the collected telemetry logs.
func collectTelemetry(t *testing.T, blocks []*auditBlock, apiType apiType) []*telemetryLog {
	t.Helper()

	pool := newBlockPool(64*1024*20, 64*1024)
	auditQueue := make(chan *auditBlock, 64)
	workerInput := make(chan *assembledPayload, 16)
	resultsChannel := make(chan *telemetryLog, 64)

	asmStats := &assemblerStats{}
	assemblerDone := make(chan struct{})
	go func() {
		defer close(assemblerDone)
		startAssembler(auditQueue, assemblerConfig{
			maxPayloadSize: defaultMaxPayloadSize,
			staleTimeout:   defaultStaleTimeout,
			workerInput:    workerInput,
			pool:           pool,
			apiType:        apiType,
		}, asmStats)
	}()

	wkStats := &workerStats{}
	waitWorkers := startWorkers(2, workerInput, resultsChannel, testChain(), wkStats)

	var mu sync.Mutex
	var collected []*telemetryLog
	cStats := &collectorStats{}
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		startCollector(collectorConfig{
			batchSize:     1000,
			flushInterval: 50 * time.Millisecond,
			input:         resultsChannel,
			flush: func(batch []*telemetryLog) {
				mu.Lock()
				collected = append(collected, batch...)
				mu.Unlock()
			},
		}, cStats)
	}()

	// Feed blocks into the queue.
	for _, blk := range blocks {
		auditQueue <- blk
	}
	close(auditQueue)

	// Cascade shutdown.
	<-assemblerDone
	waitWorkers()
	close(resultsChannel)
	<-collectorDone

	mu.Lock()
	defer mu.Unlock()
	return collected
}

// TestEndToEndPipeline verifies the complete flow: blocks with PII go through
// assembler → worker → collector and produce correct telemetry logs.
func TestEndToEndPipeline(t *testing.T) {
	pool := newBlockPool(64*1024*10, 64*1024)

	payload := []byte(`{"messages":[{"role":"user","content":"My SSN is 123-45-6789 and email test@example.com"}]}`)

	blocks := []*auditBlock{
		makeBlock(pool, 1, 0, payload, true, false),
	}

	logs := collectTelemetry(t, blocks, apiTypeOpenAI)

	if len(logs) != 1 {
		t.Fatalf("got %d logs, want 1", len(logs))
	}

	entry := logs[0]
	if entry.RequestID != 1 {
		t.Errorf("RequestID = %d, want 1", entry.RequestID)
	}
	if !entry.CaptureComplete {
		t.Error("CaptureComplete should be true")
	}
	if entry.RedactionLevel != "pattern-based" {
		t.Errorf("RedactionLevel = %q", entry.RedactionLevel)
	}
	if entry.ExtractorUsed != "openai" {
		t.Errorf("ExtractorUsed = %q", entry.ExtractorUsed)
	}
	if entry.FieldsScanned != 1 {
		t.Errorf("FieldsScanned = %d, want 1", entry.FieldsScanned)
	}
	if !strings.Contains(entry.RedactedPayload, "[REDACTED:SSN:CC6.1]") {
		t.Errorf("SSN not redacted: %s", entry.RedactedPayload)
	}
	if !strings.Contains(entry.RedactedPayload, "[REDACTED:EMAIL:CC6.1]") {
		t.Errorf("email not redacted: %s", entry.RedactedPayload)
	}
	if entry.OriginalHash == "" {
		t.Error("OriginalHash is empty")
	}
	if entry.RedactedHash == "" {
		t.Error("RedactedHash is empty")
	}

	catSet := make(map[string]struct{})
	for _, c := range entry.PIICategoriesFound {
		catSet[c] = struct{}{}
	}
	if _, ok := catSet["SSN"]; !ok {
		t.Error("SSN not in categories")
	}
	if _, ok := catSet["EMAIL"]; !ok {
		t.Error("EMAIL not in categories")
	}
}

// TestBlockBoundarySSNRedaction verifies that a SSN split across 3+ ring
// buffer blocks is correctly reassembled and the SSN is detected and redacted.
func TestBlockBoundarySSNRedaction(t *testing.T) {
	pool := newBlockPool(64*1024*10, 64*1024)

	// Split the payload so the SSN "123-45-6789" spans block boundaries.
	part1 := []byte(`{"messages":[{"role":"user","content":"SSN: 123-`)
	part2 := []byte(`45-67`)
	part3 := []byte(`89 end"}]}`)

	blocks := []*auditBlock{
		makeBlock(pool, 1, 0, part1, false, false),
		makeBlock(pool, 1, 1, part2, false, false),
		makeBlock(pool, 1, 2, part3, true, false),
	}

	logs := collectTelemetry(t, blocks, apiTypeOpenAI)

	if len(logs) != 1 {
		t.Fatalf("got %d logs, want 1", len(logs))
	}

	entry := logs[0]
	if !strings.Contains(entry.RedactedPayload, "[REDACTED:SSN:CC6.1]") {
		t.Errorf("SSN not redacted after block boundary reassembly: %s", entry.RedactedPayload)
	}
}

// TestWorkspaceIDNotRedactedE2E is the end-to-end version: a 16-digit Luhn-valid
// workspace_id in a structural JSON field must NOT trigger credit card redaction.
func TestWorkspaceIDNotRedactedE2E(t *testing.T) {
	pool := newBlockPool(64*1024*10, 64*1024)

	// 4532015112830366 is Luhn-valid. It must NOT be redacted because it's
	// in a structural field, not a content field.
	payload := []byte(`{
		"model": "gpt-4",
		"workspace_id": "4532015112830366",
		"messages": [
			{"role": "user", "content": "Hello, no PII here"}
		]
	}`)

	blocks := []*auditBlock{
		makeBlock(pool, 1, 0, payload, true, false),
	}

	logs := collectTelemetry(t, blocks, apiTypeOpenAI)

	if len(logs) != 1 {
		t.Fatalf("got %d logs, want 1", len(logs))
	}

	entry := logs[0]
	if strings.Contains(entry.RedactedPayload, "REDACTED") {
		t.Errorf("workspace_id triggered redaction: %s", entry.RedactedPayload)
	}
}

// TestCollectorNonBlockingDuringFlush verifies that the double-buffered
// collector continues accepting results during an active flush operation.
func TestCollectorNonBlockingDuringFlush(t *testing.T) {
	input := make(chan *telemetryLog, 200)
	gate := make(chan struct{})        // blocks the first flush
	flushStarted := make(chan struct{}) // signals when flush callback begins
	var mu sync.Mutex
	var totalFlushed int

	stats := &collectorStats{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		startCollector(collectorConfig{
			batchSize:     5,
			flushInterval: 20 * time.Millisecond,
			input:         input,
			flush: func(batch []*telemetryLog) {
				select {
				case flushStarted <- struct{}{}:
				default:
				}
				<-gate // block until test releases
				mu.Lock()
				totalFlushed += len(batch)
				mu.Unlock()
			},
		}, stats)
	}()

	// Send first batch to trigger a flush.
	for i := range 5 {
		input <- &telemetryLog{RequestID: uint64(i)}
	}

	// Wait for the flush callback to actually start executing.
	select {
	case <-flushStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("flush did not start")
	}

	// Send more while flush is blocked — these should NOT be dropped.
	for i := range 20 {
		input <- &telemetryLog{RequestID: uint64(100 + i)}
	}

	// Release the blocked flush and close input.
	close(gate)
	close(input)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("collector did not exit")
	}

	mu.Lock()
	defer mu.Unlock()
	if totalFlushed != 25 {
		t.Errorf("flushed %d entries, want 25 (no drops)", totalFlushed)
	}
}

// TestSSEStreamPIIRedaction verifies that SSE-framed responses with PII in
// delta content are correctly extracted and redacted.
func TestSSEStreamPIIRedaction(t *testing.T) {
	pool := newBlockPool(64*1024*10, 64*1024)

	// SSE stream where an email is split across two delta chunks.
	ssePayload := []byte(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Contact us at user@\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"example.com for help\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)

	blocks := []*auditBlock{
		makeBlock(pool, 1, 0, ssePayload, true, false),
	}

	logs := collectTelemetry(t, blocks, apiTypeOpenAI)

	if len(logs) != 1 {
		t.Fatalf("got %d logs, want 1", len(logs))
	}

	entry := logs[0]
	if !strings.Contains(entry.RedactedPayload, "[REDACTED:EMAIL:CC6.1]") {
		t.Errorf("email not redacted in SSE stream: %s", entry.RedactedPayload)
	}
	// Verify the original email is NOT present in the redacted payload.
	if strings.Contains(entry.RedactedPayload, "user@example.com") {
		t.Errorf("email not fully redacted: %s", entry.RedactedPayload)
	}
}

// TestMultipleRequestsPipeline verifies that multiple concurrent requests
// are all processed correctly through the full pipeline.
func TestMultipleRequestsPipeline(t *testing.T) {
	pool := newBlockPool(64*1024*20, 64*1024)

	blocks := []*auditBlock{
		makeBlock(pool, 1, 0, []byte(`{"messages":[{"role":"user","content":"SSN 123-45-6789"}]}`), true, false),
		makeBlock(pool, 2, 0, []byte(`{"messages":[{"role":"user","content":"email test@test.com"}]}`), true, false),
		makeBlock(pool, 3, 0, []byte(`{"messages":[{"role":"user","content":"no PII here"}]}`), true, false),
	}

	logs := collectTelemetry(t, blocks, apiTypeOpenAI)

	if len(logs) != 3 {
		t.Fatalf("got %d logs, want 3", len(logs))
	}

	// Build a map for easier assertion.
	byID := make(map[uint64]*telemetryLog)
	for _, l := range logs {
		byID[l.RequestID] = l
	}

	// Request 1: SSN redacted.
	if l, ok := byID[1]; ok {
		if !strings.Contains(l.RedactedPayload, "[REDACTED:SSN:CC6.1]") {
			t.Errorf("request 1: SSN not redacted: %s", l.RedactedPayload)
		}
	} else {
		t.Error("request 1 not found in logs")
	}

	// Request 2: email redacted.
	if l, ok := byID[2]; ok {
		if !strings.Contains(l.RedactedPayload, "[REDACTED:EMAIL:CC6.1]") {
			t.Errorf("request 2: email not redacted: %s", l.RedactedPayload)
		}
	} else {
		t.Error("request 2 not found in logs")
	}

	// Request 3: no PII.
	if l, ok := byID[3]; ok {
		if strings.Contains(l.RedactedPayload, "REDACTED") {
			t.Errorf("request 3: false positive: %s", l.RedactedPayload)
		}
	} else {
		t.Error("request 3 not found in logs")
	}
}
