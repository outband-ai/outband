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
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockDropRecorder counts drop() calls for test assertions.
type mockDropRecorder struct {
	count atomic.Int64
}

func (m *mockDropRecorder) drop() { m.count.Add(1) }

// collectBlocks drains the queue into a slice. It uses a short timeout
// after the initial non-blocking drain to catch blocks from async producers.
func collectBlocks(queue <-chan *auditBlock) []*auditBlock {
	var out []*auditBlock
	timeout := time.After(50 * time.Millisecond)
	for {
		select {
		case blk, ok := <-queue:
			if !ok {
				return out
			}
			out = append(out, blk)
		case <-timeout:
			return out
		}
	}
}

func TestAuditReaderBasic(t *testing.T) {
	pool := newBlockPool(4*1024, 1024) // 4 x 1KB blocks
	queue := make(chan *auditBlock, 16)
	drops := &mockDropRecorder{}

	body := io.NopCloser(strings.NewReader("hello world"))
	ar := newAuditReader(body, pool, drops, queue, 42)

	data, err := io.ReadAll(ar)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("body = %q, want %q", data, "hello world")
	}
	ar.Close()

	blocks := collectBlocks(queue)
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	blk := blocks[0]
	if blk.requestID != 42 {
		t.Errorf("requestID = %d, want 42", blk.requestID)
	}
	if blk.seq != 0 {
		t.Errorf("seq = %d, want 0", blk.seq)
	}
	if !blk.final {
		t.Error("expected final=true")
	}
	if string(blk.data[:blk.n]) != "hello world" {
		t.Errorf("block data = %q, want %q", blk.data[:blk.n], "hello world")
	}
	if drops.count.Load() != 0 {
		t.Errorf("drops = %d, want 0", drops.count.Load())
	}
}

// partialReader returns at most maxN bytes per Read call.
type partialReader struct {
	r    io.Reader
	maxN int
}

func (p *partialReader) Read(buf []byte) (int, error) {
	if len(buf) > p.maxN {
		buf = buf[:p.maxN]
	}
	return p.r.Read(buf)
}

func TestAuditReaderPartialRead(t *testing.T) {
	pool := newBlockPool(4*1024, 1024)
	queue := make(chan *auditBlock, 16)
	drops := &mockDropRecorder{}

	// Reader that returns at most 3 bytes per Read.
	src := &partialReader{r: strings.NewReader("abcdefghij"), maxN: 3}
	body := io.NopCloser(src)
	ar := newAuditReader(body, pool, drops, queue, 1)

	data, err := io.ReadAll(ar)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abcdefghij" {
		t.Errorf("body = %q, want %q", data, "abcdefghij")
	}
	ar.Close()

	// Reassemble captured data from blocks.
	blocks := collectBlocks(queue)
	var captured bytes.Buffer
	for _, blk := range blocks {
		captured.Write(blk.data[:blk.n])
	}
	if captured.String() != "abcdefghij" {
		t.Errorf("captured = %q, want %q", captured.String(), "abcdefghij")
	}
}

func TestAuditReaderEOF(t *testing.T) {
	pool := newBlockPool(4*1024, 1024)
	queue := make(chan *auditBlock, 16)

	body := io.NopCloser(strings.NewReader("data"))
	ar := newAuditReader(body, pool, &mockDropRecorder{}, queue, 99)

	if _, err := io.ReadAll(ar); err != nil {
		t.Fatal(err)
	}
	ar.Close()

	blocks := collectBlocks(queue)
	if len(blocks) == 0 {
		t.Fatal("expected at least one block")
	}
	last := blocks[len(blocks)-1]
	if !last.final {
		t.Error("last block should have final=true")
	}
	if last.requestID != 99 {
		t.Errorf("requestID = %d, want 99", last.requestID)
	}
}

func TestAuditReaderMultipleBlocks(t *testing.T) {
	const blockSize = 64
	pool := newBlockPool(10*blockSize, blockSize)
	queue := make(chan *auditBlock, 16)

	// Payload larger than one block.
	payload := strings.Repeat("x", 3*blockSize+10)
	body := io.NopCloser(strings.NewReader(payload))
	ar := newAuditReader(body, pool, &mockDropRecorder{}, queue, 7)

	data, err := io.ReadAll(ar)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Errorf("body length = %d, want %d", len(data), len(payload))
	}
	ar.Close()

	blocks := collectBlocks(queue)
	if len(blocks) != 4 {
		t.Fatalf("got %d blocks, want 4 (3 full + 1 partial)", len(blocks))
	}

	// Verify sequence numbers and requestID.
	var captured bytes.Buffer
	for i, blk := range blocks {
		if blk.requestID != 7 {
			t.Errorf("block %d: requestID = %d, want 7", i, blk.requestID)
		}
		if blk.seq != uint32(i) {
			t.Errorf("block %d: seq = %d, want %d", i, blk.seq, i)
		}
		captured.Write(blk.data[:blk.n])
	}
	if captured.String() != payload {
		t.Errorf("captured length = %d, want %d", captured.Len(), len(payload))
	}

	// Only last block is final.
	for i, blk := range blocks {
		if i < len(blocks)-1 && blk.final {
			t.Errorf("block %d: unexpected final=true", i)
		}
	}
	if !blocks[len(blocks)-1].final {
		t.Error("last block should have final=true")
	}
}

func TestAuditReaderExactFill(t *testing.T) {
	const blockSize = 64
	pool := newBlockPool(10*blockSize, blockSize)
	queue := make(chan *auditBlock, 16)

	// Payload exactly fills 2 blocks — no leftover bytes.
	payload := strings.Repeat("x", 2*blockSize)
	body := io.NopCloser(strings.NewReader(payload))
	ar := newAuditReader(body, pool, &mockDropRecorder{}, queue, 8)

	data, err := io.ReadAll(ar)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Errorf("body length = %d, want %d", len(data), len(payload))
	}
	ar.Close()

	blocks := collectBlocks(queue)
	if len(blocks) < 2 {
		t.Fatalf("got %d blocks, want at least 2", len(blocks))
	}

	// Reassemble and verify data integrity.
	var captured bytes.Buffer
	for _, blk := range blocks {
		captured.Write(blk.data[:blk.n])
	}
	if captured.String() != payload {
		t.Errorf("captured length = %d, want %d", captured.Len(), len(payload))
	}

	// Last block must have final=true (sentinel or data block).
	last := blocks[len(blocks)-1]
	if !last.final {
		t.Error("last block should have final=true")
	}
}

func TestAuditReaderAbandonPoolExhausted(t *testing.T) {
	const blockSize = 64
	pool := newBlockPool(2*blockSize, blockSize) // 2 blocks: one for data, one for abort
	queue := make(chan *auditBlock, 16)
	drops := &mockDropRecorder{}

	// Payload needs 3+ blocks but pool only has 2, so after filling and
	// sending block 0, acquiring block 1 succeeds (used mid-capture),
	// but acquiring block 2 fails → abandon with abort signal on block 1.
	payload := strings.Repeat("y", 3*blockSize)
	body := io.NopCloser(strings.NewReader(payload))
	ar := newAuditReader(body, pool, drops, queue, 5)

	// Upstream must still receive the full payload.
	data, err := io.ReadAll(ar)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Errorf("body length = %d, want %d", len(data), len(payload))
	}
	ar.Close()

	if drops.count.Load() != 1 {
		t.Errorf("drops = %d, want 1", drops.count.Load())
	}

	// Should have blocks on the queue, at least one with abort=true.
	blocks := collectBlocks(queue)
	var hasAbort bool
	for _, blk := range blocks {
		if blk.requestID != 5 {
			t.Errorf("block requestID = %d, want 5", blk.requestID)
		}
		if blk.abort {
			hasAbort = true
		}
	}
	if !hasAbort {
		t.Error("expected an abort block on the queue")
	}
}

func TestAuditReaderAbandonPoolFullyExhausted(t *testing.T) {
	const blockSize = 64
	pool := newBlockPool(blockSize, blockSize) // only 1 block — no spare for abort
	queue := make(chan *auditBlock, 16)
	drops := &mockDropRecorder{}

	// With only 1 block, after filling and sending it, there's no block
	// available for the abort signal. Drop is still counted.
	payload := strings.Repeat("y", 2*blockSize)
	body := io.NopCloser(strings.NewReader(payload))
	ar := newAuditReader(body, pool, drops, queue, 5)

	data, err := io.ReadAll(ar)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Errorf("body length = %d, want %d", len(data), len(payload))
	}
	ar.Close()

	if drops.count.Load() != 1 {
		t.Errorf("drops = %d, want 1", drops.count.Load())
	}
}

func TestAuditReaderAbandonQueueFull(t *testing.T) {
	const blockSize = 64
	pool := newBlockPool(10*blockSize, blockSize)
	queue := make(chan *auditBlock, 1) // queue can only hold 1 block
	drops := &mockDropRecorder{}

	// Payload needs 3+ blocks but queue can only hold 1.
	payload := strings.Repeat("z", 3*blockSize)
	body := io.NopCloser(strings.NewReader(payload))
	ar := newAuditReader(body, pool, drops, queue, 10)

	data, err := io.ReadAll(ar)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Errorf("body length = %d, want %d", len(data), len(payload))
	}
	ar.Close()

	if drops.count.Load() == 0 {
		t.Error("expected at least 1 drop due to queue full")
	}
}

func TestAuditReaderRequestIDUnique(t *testing.T) {
	pool := newBlockPool(8*1024, 1024)
	queue := make(chan *auditBlock, 16)
	drops := &mockDropRecorder{}

	body1 := io.NopCloser(strings.NewReader("request one"))
	ar1 := newAuditReader(body1, pool, drops, queue, 100)
	if _, err := io.ReadAll(ar1); err != nil {
		t.Fatalf("io.ReadAll(ar1) failed: %v", err)
	}
	ar1.Close()

	body2 := io.NopCloser(strings.NewReader("request two"))
	ar2 := newAuditReader(body2, pool, drops, queue, 101)
	if _, err := io.ReadAll(ar2); err != nil {
		t.Fatalf("io.ReadAll(ar2) failed: %v", err)
	}
	ar2.Close()

	blocks := collectBlocks(queue)
	ids := make(map[uint64]bool)
	for _, blk := range blocks {
		ids[blk.requestID] = true
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 distinct requestIDs, got %d", len(ids))
	}
	if !ids[100] || !ids[101] {
		t.Errorf("expected requestIDs 100 and 101, got %v", ids)
	}
}

func TestAuditReaderCloseWithoutRead(t *testing.T) {
	pool := newBlockPool(4*1024, 1024)
	queue := make(chan *auditBlock, 16)

	body := io.NopCloser(strings.NewReader("data"))
	ar := newAuditReader(body, pool, &mockDropRecorder{}, queue, 1)

	// Close without reading — should not panic.
	if err := ar.Close(); err != nil {
		t.Fatal(err)
	}

	// No blocks expected (nothing was read).
	blocks := collectBlocks(queue)
	if len(blocks) != 0 {
		t.Errorf("expected 0 blocks, got %d", len(blocks))
	}
}

func TestAuditReaderEmptyBody(t *testing.T) {
	pool := newBlockPool(4*1024, 1024)
	queue := make(chan *auditBlock, 16)

	body := io.NopCloser(strings.NewReader(""))
	ar := newAuditReader(body, pool, &mockDropRecorder{}, queue, 1)

	data, err := io.ReadAll(ar)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("body = %q, want empty", data)
	}
	ar.Close()

	// Empty body should produce no blocks.
	blocks := collectBlocks(queue)
	if len(blocks) != 0 {
		t.Errorf("expected 0 blocks for empty body, got %d", len(blocks))
	}
}
