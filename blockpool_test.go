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
	"runtime"
	"sync"
	"testing"
)

func TestBlockPoolBasic(t *testing.T) {
	const blockSize = 1024
	const totalBytes = 4 * blockSize // 4 blocks
	bp := NewBlockPool(totalBytes, blockSize)

	// Should be able to get exactly 4 blocks.
	blocks := make([]*AuditBlock, 0, 4)
	for range 4 {
		blk := bp.Get()
		if blk == nil {
			t.Fatal("expected non-nil block")
		}
		if len(blk.Data) != blockSize {
			t.Errorf("block data len = %d, want %d", len(blk.Data), blockSize)
		}
		blocks = append(blocks, blk)
	}

	// 5th get should return nil (pool exhausted).
	if blk := bp.Get(); blk != nil {
		t.Error("expected nil from exhausted pool")
	}

	// Return all blocks.
	for _, blk := range blocks {
		bp.Put(blk)
	}
}

func TestBlockPoolPutGet(t *testing.T) {
	bp := NewBlockPool(1024, 1024) // 1 block

	blk := bp.Get()
	if blk == nil {
		t.Fatal("expected non-nil block")
	}

	// Write some data, return, get again — should be reset.
	blk.Write([]byte("hello"))
	blk.RequestID = 42
	blk.Seq = 7
	blk.Final = true
	blk.Direction = DirectionResponse
	bp.Put(blk)

	blk2 := bp.Get()
	if blk2 == nil {
		t.Fatal("expected non-nil block after put")
	}
	if blk2.Used != 0 || blk2.RequestID != 0 || blk2.Seq != 0 || blk2.Final || blk2.Direction != DirectionRequest {
		t.Errorf("block not reset: n=%d, requestID=%d, seq=%d, final=%v, direction=%d",
			blk2.Used, blk2.RequestID, blk2.Seq, blk2.Final, blk2.Direction)
	}
}

func TestBlockPoolExhaustionCounter(t *testing.T) {
	bp := NewBlockPool(1024, 1024) // 1 block

	blk := bp.Get()
	if blk == nil {
		t.Fatal("expected non-nil block")
	}

	// Exhaust the pool 3 times.
	for range 3 {
		if got := bp.Get(); got != nil {
			t.Fatal("expected nil from exhausted pool")
		}
	}

	if got := bp.Exhaustions.Load(); got != 3 {
		t.Errorf("exhaustions = %d, want 3", got)
	}

	bp.Put(blk)
}

func TestBlockPoolConcurrent(t *testing.T) {
	const blockSize = 512
	const numBlocks = 100
	bp := NewBlockPool(numBlocks*blockSize, blockSize)

	var wg sync.WaitGroup
	const goroutines = 50
	const opsPerGoroutine = 200

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range opsPerGoroutine {
				blk := bp.Get()
				if blk != nil {
					blk.Write([]byte("x"))
					bp.Put(blk)
				}
			}
		}()
	}
	wg.Wait()

	// All blocks should be back in the pool.
	var count int
	for {
		if bp.Get() == nil {
			break
		}
		count++
	}
	if count != numBlocks {
		t.Errorf("recovered %d blocks, want %d", count, numBlocks)
	}
}

func TestBlockPoolGCResilience(t *testing.T) {
	const blockSize = 1024
	const numBlocks = 10
	bp := NewBlockPool(numBlocks*blockSize, blockSize)

	// Force multiple GC cycles.
	for range 5 {
		runtime.GC()
	}

	// All blocks should still be available (unlike sync.Pool which evicts).
	var count int
	for {
		if bp.Get() == nil {
			break
		}
		count++
	}
	if count != numBlocks {
		t.Errorf("after GC: recovered %d blocks, want %d", count, numBlocks)
	}
}

func TestAuditBlockWrite(t *testing.T) {
	blk := &AuditBlock{Data: make([]byte, 16)}

	n := blk.Write([]byte("hello"))
	if n != 5 {
		t.Errorf("write returned %d, want 5", n)
	}
	if blk.Used != 5 {
		t.Errorf("blk.Used = %d, want 5", blk.Used)
	}
	if blk.Remaining() != 11 {
		t.Errorf("remaining = %d, want 11", blk.Remaining())
	}

	// Write more than remaining — capped to available space.
	n = blk.Write([]byte("this is a long string"))
	if n != 11 {
		t.Errorf("write returned %d, want 11 (capped to remaining)", n)
	}
	if blk.Remaining() != 0 {
		t.Errorf("remaining = %d, want 0", blk.Remaining())
	}

	// Verify content: "hello" (5) + "this is a l" (11) = 16 bytes.
	want := "hellothis is a l"
	if got := string(blk.Data[:blk.Used]); got != want {
		t.Errorf("block data = %q, want %q", got, want)
	}
}
