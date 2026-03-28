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
	"net/url"
	"testing"
	"time"
)

// makeBlock creates an AuditBlock with the given fields for testing.
func makeBlock(pool *BlockPool, requestID uint64, seq uint32, data []byte, final, abort bool) *AuditBlock {
	blk := pool.Get()
	if blk == nil {
		panic("pool exhausted in test setup")
	}
	blk.RequestID = requestID
	blk.Seq = seq
	blk.Final = final
	blk.Abort = abort
	copy(blk.Data, data)
	blk.Used = len(data)
	return blk
}

func TestAssemblerSingleRequest(t *testing.T) {
	pool := NewBlockPool(64*1024*10, 64*1024)
	auditQueue := make(chan *AuditBlock, 16)
	workerInput := make(chan *assembledPayload, 16)
	stats := &assemblerStats{}

	go startAssembler(auditQueue, assemblerConfig{
		maxPayloadSize: defaultMaxPayloadSize,
		staleTimeout:   defaultStaleTimeout,
		workerInput:    workerInput,
		pool:           pool,
		apiType:        apiTypeOpenAI,
	}, stats)

	// Send 3 blocks for request 1.
	auditQueue <- makeBlock(pool, 1, 0, []byte("hello "), false, false)
	auditQueue <- makeBlock(pool, 1, 1, []byte("world "), false, false)
	auditQueue <- makeBlock(pool, 1, 2, []byte("done"), true, false)
	close(auditQueue)

	select {
	case p := <-workerInput:
		if string(p.data) != "hello world done" {
			t.Fatalf("got %q, want %q", string(p.data), "hello world done")
		}
		if p.requestID != 1 {
			t.Fatalf("requestID = %d, want 1", p.requestID)
		}
		if !p.complete {
			t.Fatal("expected complete=true")
		}
		if p.apiType != apiTypeOpenAI {
			t.Fatalf("apiType = %d, want %d", p.apiType, apiTypeOpenAI)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for assembled payload")
	}
}

func TestAssemblerMultipleRequests(t *testing.T) {
	pool := NewBlockPool(64*1024*10, 64*1024)
	auditQueue := make(chan *AuditBlock, 32)
	workerInput := make(chan *assembledPayload, 16)
	stats := &assemblerStats{}

	go startAssembler(auditQueue, assemblerConfig{
		maxPayloadSize: defaultMaxPayloadSize,
		staleTimeout:   defaultStaleTimeout,
		workerInput:    workerInput,
		pool:           pool,
		apiType:        apiTypeOpenAI,
	}, stats)

	// Interleave blocks from 3 requests.
	auditQueue <- makeBlock(pool, 1, 0, []byte("req1-a"), false, false)
	auditQueue <- makeBlock(pool, 2, 0, []byte("req2-a"), false, false)
	auditQueue <- makeBlock(pool, 3, 0, []byte("req3-a"), false, false)
	auditQueue <- makeBlock(pool, 1, 1, []byte("req1-b"), true, false)
	auditQueue <- makeBlock(pool, 2, 1, []byte("req2-b"), true, false)
	auditQueue <- makeBlock(pool, 3, 1, []byte("req3-b"), true, false)
	close(auditQueue)

	got := make(map[uint64]string)
	for range 3 {
		select {
		case p := <-workerInput:
			got[p.requestID] = string(p.data)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for assembled payload")
		}
	}

	want := map[uint64]string{
		1: "req1-areq1-b",
		2: "req2-areq2-b",
		3: "req3-areq3-b",
	}
	for id, wantData := range want {
		if got[id] != wantData {
			t.Errorf("request %d: got %q, want %q", id, got[id], wantData)
		}
	}
}

func TestAssemblerAbort(t *testing.T) {
	pool := NewBlockPool(64*1024*10, 64*1024)
	auditQueue := make(chan *AuditBlock, 16)
	workerInput := make(chan *assembledPayload, 16)
	stats := &assemblerStats{}

	go startAssembler(auditQueue, assemblerConfig{
		maxPayloadSize: defaultMaxPayloadSize,
		staleTimeout:   defaultStaleTimeout,
		workerInput:    workerInput,
		pool:           pool,
		apiType:        apiTypeOpenAI,
	}, stats)

	// Send partial blocks, then abort.
	auditQueue <- makeBlock(pool, 1, 0, []byte("partial"), false, false)
	auditQueue <- makeBlock(pool, 1, 0, []byte(""), false, true)
	close(auditQueue)

	// workerInput should be closed with no payloads.
	select {
	case p, ok := <-workerInput:
		if ok {
			t.Fatalf("expected no payload, got requestID=%d data=%q", p.requestID, string(p.data))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for workerInput to close")
	}
}

func TestAssemblerOversizePayload(t *testing.T) {
	pool := NewBlockPool(64*1024*10, 64*1024)
	auditQueue := make(chan *AuditBlock, 16)
	workerInput := make(chan *assembledPayload, 16)
	stats := &assemblerStats{}

	maxSize := 100 // very small limit for testing

	go startAssembler(auditQueue, assemblerConfig{
		maxPayloadSize: maxSize,
		staleTimeout:   defaultStaleTimeout,
		workerInput:    workerInput,
		pool:           pool,
		apiType:        apiTypeOpenAI,
	}, stats)

	// Send data that exceeds the limit.
	bigData := make([]byte, 60)
	for i := range bigData {
		bigData[i] = 'A'
	}
	auditQueue <- makeBlock(pool, 1, 0, bigData, false, false)
	auditQueue <- makeBlock(pool, 1, 1, bigData, true, false) // total 120 > 100
	close(auditQueue)

	// No payload should arrive.
	select {
	case p, ok := <-workerInput:
		if ok {
			t.Fatalf("expected no payload, got requestID=%d len=%d", p.requestID, len(p.data))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for workerInput to close")
	}

	if got := stats.oversizedDropped.Load(); got != 1 {
		t.Fatalf("oversizedDropped = %d, want 1", got)
	}
}

func TestAssemblerStaleEviction(t *testing.T) {
	pool := NewBlockPool(64*1024*10, 64*1024)
	auditQueue := make(chan *AuditBlock, 16)
	workerInput := make(chan *assembledPayload, 16)
	stats := &assemblerStats{}

	go startAssembler(auditQueue, assemblerConfig{
		maxPayloadSize: defaultMaxPayloadSize,
		staleTimeout:   50 * time.Millisecond, // very short for testing
		sweepInterval:  25 * time.Millisecond,  // fast sweep for testing
		workerInput:    workerInput,
		pool:           pool,
		apiType:        apiTypeOpenAI,
	}, stats)

	// Send partial block, then wait for it to go stale.
	auditQueue <- makeBlock(pool, 1, 0, []byte("stale"), false, false)

	// Wait for stale sweep to fire (sweepInterval=25ms, staleTimeout=50ms).
	time.Sleep(200 * time.Millisecond)
	close(auditQueue)

	// No payload should arrive.
	select {
	case p, ok := <-workerInput:
		if ok {
			t.Fatalf("expected no payload, got requestID=%d", p.requestID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for workerInput to close")
	}

	if got := stats.staleDropped.Load(); got != 1 {
		t.Fatalf("staleDropped = %d, want 1", got)
	}
}

func TestAssemblerBlockBoundaryIntegrity(t *testing.T) {
	pool := NewBlockPool(64*1024*10, 64*1024)
	auditQueue := make(chan *AuditBlock, 16)
	workerInput := make(chan *assembledPayload, 16)
	stats := &assemblerStats{}

	go startAssembler(auditQueue, assemblerConfig{
		maxPayloadSize: defaultMaxPayloadSize,
		staleTimeout:   defaultStaleTimeout,
		workerInput:    workerInput,
		pool:           pool,
		apiType:        apiTypeOpenAI,
	}, stats)

	// SSN "123-45-6789" split across two blocks at the dash boundary.
	auditQueue <- makeBlock(pool, 1, 0, []byte("SSN: 123-45"), false, false)
	auditQueue <- makeBlock(pool, 1, 1, []byte("-6789 end"), true, false)
	close(auditQueue)

	select {
	case p := <-workerInput:
		want := "SSN: 123-45-6789 end"
		if string(p.data) != want {
			t.Fatalf("got %q, want %q", string(p.data), want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for assembled payload")
	}
}

func TestAssemblerShutdown(t *testing.T) {
	pool := NewBlockPool(64*1024*10, 64*1024)
	auditQueue := make(chan *AuditBlock, 16)
	workerInput := make(chan *assembledPayload, 16)
	stats := &assemblerStats{}

	go startAssembler(auditQueue, assemblerConfig{
		maxPayloadSize: defaultMaxPayloadSize,
		staleTimeout:   defaultStaleTimeout,
		workerInput:    workerInput,
		pool:           pool,
		apiType:        apiTypeOpenAI,
	}, stats)

	close(auditQueue)

	// workerInput should be closed.
	select {
	case _, ok := <-workerInput:
		if ok {
			t.Fatal("expected workerInput to be closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for workerInput to close")
	}
}

func TestAssemblerBlocksReturnedToPool(t *testing.T) {
	totalBlocks := 10
	pool := NewBlockPool(64*1024*totalBlocks, 64*1024)
	auditQueue := make(chan *AuditBlock, 32)
	workerInput := make(chan *assembledPayload, 16)
	stats := &assemblerStats{}

	go startAssembler(auditQueue, assemblerConfig{
		maxPayloadSize: defaultMaxPayloadSize,
		staleTimeout:   defaultStaleTimeout,
		workerInput:    workerInput,
		pool:           pool,
		apiType:        apiTypeOpenAI,
	}, stats)

	// Use 5 blocks: 3 for a complete request, 2 for an aborted request.
	auditQueue <- makeBlock(pool, 1, 0, []byte("a"), false, false)
	auditQueue <- makeBlock(pool, 1, 1, []byte("b"), false, false)
	auditQueue <- makeBlock(pool, 1, 2, []byte("c"), true, false)
	auditQueue <- makeBlock(pool, 2, 0, []byte("x"), false, false)
	auditQueue <- makeBlock(pool, 2, 0, []byte(""), false, true)
	close(auditQueue)

	// Drain workerInput.
	for range workerInput {
	}

	// All 5 blocks should be back in the pool. We had 10, used 5, so
	// we should be able to get all 10 back.
	available := 0
	for range totalBlocks {
		if pool.Get() != nil {
			available++
		}
	}
	if available != totalBlocks {
		t.Fatalf("pool has %d blocks available, want %d", available, totalBlocks)
	}
}

func TestDetectAPIType(t *testing.T) {
	tests := []struct {
		rawURL string
		want   apiType
	}{
		{"https://api.openai.com/v1/chat/completions", apiTypeOpenAI},
		{"https://api.anthropic.com/v1/messages", apiTypeAnthropic},
		{"https://my-openai-proxy.example.com", apiTypeOpenAI},
		{"https://vllm.internal.corp:8080", apiTypeUnknown},
		{"http://localhost:9090", apiTypeUnknown},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.rawURL, err)
		}
		if got := detectAPIType(u); got != tt.want {
			t.Errorf("detectAPIType(%q) = %d, want %d", tt.rawURL, got, tt.want)
		}
	}
}

func TestParseAPIFormat(t *testing.T) {
	tests := []struct {
		input string
		want  apiType
	}{
		{"openai", apiTypeOpenAI},
		{"OpenAI", apiTypeOpenAI},
		{"anthropic", apiTypeAnthropic},
		{"  Anthropic ", apiTypeAnthropic},
		{"", apiTypeUnknown},
		{"unknown", apiTypeUnknown},
	}
	for _, tt := range tests {
		if got := parseAPIFormat(tt.input); got != tt.want {
			t.Errorf("parseAPIFormat(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
