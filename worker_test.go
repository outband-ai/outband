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
	"testing"
	"time"
)

func TestProcessPayloadWithPII(t *testing.T) {
	payload := &assembledPayload{
		requestID: 42,
		data: []byte(`{
			"messages": [
				{"role": "user", "content": "My SSN is 123-45-6789 and email is test@example.com"}
			]
		}`),
		complete: true,
		apiType:  apiTypeOpenAI,
	}

	entry := processPayload(payload, testChain())

	if entry.RequestID != 42 {
		t.Errorf("RequestID = %d, want 42", entry.RequestID)
	}
	if !entry.CaptureComplete {
		t.Error("CaptureComplete should be true")
	}
	if entry.RedactionLevel != "pattern-based" {
		t.Errorf("RedactionLevel = %q, want %q", entry.RedactionLevel, "pattern-based")
	}
	if entry.ExtractorUsed != "openai" {
		t.Errorf("ExtractorUsed = %q, want %q", entry.ExtractorUsed, "openai")
	}
	if entry.FieldsScanned != 1 {
		t.Errorf("FieldsScanned = %d, want 1", entry.FieldsScanned)
	}
	if !strings.Contains(entry.RedactedPayload, "[REDACTED:SSN:CC6.1]") {
		t.Errorf("SSN not redacted in payload: %s", entry.RedactedPayload)
	}
	if !strings.Contains(entry.RedactedPayload, "[REDACTED:EMAIL:CC6.1]") {
		t.Errorf("email not redacted in payload: %s", entry.RedactedPayload)
	}
	if entry.OriginalHash == "" {
		t.Error("OriginalHash is empty")
	}
	if entry.RedactedHash == "" {
		t.Error("RedactedHash is empty")
	}
	if entry.OriginalHash == entry.RedactedHash {
		t.Error("OriginalHash should differ from RedactedHash")
	}

	// Check categories.
	catSet := make(map[string]struct{})
	for _, c := range entry.PIICategoriesFound {
		catSet[c] = struct{}{}
	}
	if _, ok := catSet["SSN"]; !ok {
		t.Error("SSN not in PIICategoriesFound")
	}
	if _, ok := catSet["EMAIL"]; !ok {
		t.Error("EMAIL not in PIICategoriesFound")
	}
}

func TestProcessPayloadNoPII(t *testing.T) {
	payload := &assembledPayload{
		requestID: 1,
		data: []byte(`{
			"messages": [
				{"role": "user", "content": "Hello world"}
			]
		}`),
		complete: true,
		apiType:  apiTypeOpenAI,
	}

	entry := processPayload(payload, testChain())

	if strings.Contains(entry.RedactedPayload, "REDACTED") {
		t.Errorf("clean payload was redacted: %s", entry.RedactedPayload)
	}
	if len(entry.PIICategoriesFound) != 0 {
		t.Errorf("got %d categories, want 0", len(entry.PIICategoriesFound))
	}
}

func TestProcessPayloadUnknownAPI(t *testing.T) {
	payload := &assembledPayload{
		requestID: 1,
		data:      []byte(`{"messages":[{"content":"hello"}]}`),
		complete:  true,
		apiType:   apiTypeUnknown,
	}

	entry := processPayload(payload, testChain())

	if entry.FieldsScanned != 0 {
		t.Errorf("FieldsScanned = %d, want 0 (no extractor)", entry.FieldsScanned)
	}
	if entry.ExtractorUsed != "unknown" {
		t.Errorf("ExtractorUsed = %q, want %q", entry.ExtractorUsed, "unknown")
	}
}

func TestProcessPayloadIncomplete(t *testing.T) {
	payload := &assembledPayload{
		requestID: 1,
		data:      []byte(`{"messages":[{"role":"user","content":"test"}]}`),
		complete:  false,
		apiType:   apiTypeOpenAI,
	}

	entry := processPayload(payload, testChain())

	if entry.CaptureComplete {
		t.Error("CaptureComplete should be false")
	}
}

func TestHashPayloadDeterministic(t *testing.T) {
	data := []byte("hello world")
	h1 := hashPayload(data)
	h2 := hashPayload(data)
	if h1 != h2 {
		t.Errorf("non-deterministic: %q != %q", h1, h2)
	}
}

func TestHashPayloadDifferentInputs(t *testing.T) {
	h1 := hashPayload([]byte("hello"))
	h2 := hashPayload([]byte("world"))
	if h1 == h2 {
		t.Error("different inputs produced same hash")
	}
}

func TestHashRedactedIncludesTimestamp(t *testing.T) {
	payload := "test payload"
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC) // 1 nanosecond later

	h1 := hashRedacted(payload, t1)
	h2 := hashRedacted(payload, t2)

	if h1 == h2 {
		t.Error("same content with different timestamps produced same hash")
	}
}

func TestWorkerPoolThroughput(t *testing.T) {
	input := make(chan *assembledPayload, 100)
	output := make(chan *telemetryLog, 100)
	stats := &workerStats{}

	wait := startWorkers(4, input, output, testChain(), stats)

	n := 50
	for i := range n {
		input <- &assembledPayload{
			requestID: uint64(i),
			data:      []byte(`{"messages":[{"role":"user","content":"test"}]}`),
			complete:  true,
			apiType:   apiTypeOpenAI,
		}
	}
	close(input)
	wait()
	close(output)

	count := 0
	for range output {
		count++
	}
	if count != n {
		t.Errorf("got %d results, want %d", count, n)
	}
}

func TestWorkerPoolShutdown(t *testing.T) {
	input := make(chan *assembledPayload)
	output := make(chan *telemetryLog, 16)
	stats := &workerStats{}

	wait := startWorkers(2, input, output, testChain(), stats)

	close(input)
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not exit after input closed")
	}
}

func TestWorkerNonBlockingOutput(t *testing.T) {
	input := make(chan *assembledPayload, 10)
	output := make(chan *telemetryLog, 1) // very small buffer
	stats := &workerStats{}

	wait := startWorkers(1, input, output, testChain(), stats)

	// Send more payloads than the output buffer can hold.
	for i := range 5 {
		input <- &assembledPayload{
			requestID: uint64(i),
			data:      []byte(`{"messages":[{"role":"user","content":"test"}]}`),
			complete:  true,
			apiType:   apiTypeOpenAI,
		}
	}
	close(input)

	// Worker should not block — it drops results when output is full.
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker blocked on full output channel")
	}

	if stats.resultDropped.Load() == 0 {
		t.Error("expected some dropped results")
	}
}
