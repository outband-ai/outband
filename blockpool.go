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

import "sync/atomic"

const (
	defaultBlockSize    = 64 * 1024          // 64KB per block
	defaultPoolCapacity = 50 * 1024 * 1024   // 50MB total
	defaultQueueSize    = 256                 // audit queue slot count
)

// auditBlock is a fixed-size byte buffer for audit capture. Blocks cycle
// between the blockPool and the audit queue without returning to the heap.
type auditBlock struct {
	data      []byte
	n         int    // bytes written so far
	seq       uint32 // sequence number within a request
	requestID uint64 // monotonic ID assigned per request in Rewrite hook
	final     bool   // last block for this request (complete capture)
	abort     bool   // capture abandoned mid-stream; consumer should discard partial payload
}

func (b *auditBlock) remaining() int { return len(b.data) - b.n }

// write copies from p into the block. Returns bytes written.
func (b *auditBlock) write(p []byte) int {
	n := copy(b.data[b.n:], p)
	b.n += n
	return n
}

func (b *auditBlock) reset() {
	b.n = 0
	b.seq = 0
	b.requestID = 0
	b.final = false
	b.abort = false
}

// blockPool manages a fixed set of pre-allocated auditBlocks using a buffered
// channel. Blocks are pinned in memory (channel holds references), preventing
// GC collection. The pool never grows beyond its initial capacity.
type blockPool struct {
	ch          chan *auditBlock
	blockSize   int
	exhaustions atomic.Int64
}

// newBlockPool pre-allocates totalBytes/blockSize blocks and returns a pool.
// All blocks are created once at startup and recycled indefinitely.
func newBlockPool(totalBytes, blockSize int) *blockPool {
	capacity := totalBytes / blockSize
	if capacity < 1 {
		capacity = 1
	}
	bp := &blockPool{
		ch:        make(chan *auditBlock, capacity),
		blockSize: blockSize,
	}
	for range capacity {
		bp.ch <- &auditBlock{data: make([]byte, blockSize)}
	}
	return bp
}

// get acquires a block from the pool. Returns nil if the pool is exhausted.
// Non-blocking: never waits for a block to become available.
func (bp *blockPool) get() *auditBlock {
	select {
	case blk := <-bp.ch:
		blk.reset()
		return blk
	default:
		bp.exhaustions.Add(1)
		return nil
	}
}

// put returns a block to the pool. Non-blocking.
func (bp *blockPool) put(blk *auditBlock) {
	select {
	case bp.ch <- blk:
	default:
		// Pool is full; should not happen if accounting is correct.
	}
}
