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

import "sync/atomic"

// Direction indicates whether an audit block carries request or response data.
type Direction int

const (
	DirectionRequest  Direction = iota
	DirectionResponse
)

const (
	defaultBlockSize    = 64 * 1024        // 64KB per block
	defaultPoolCapacity = 50 * 1024 * 1024 // 50MB total
	defaultQueueSize    = 256              // audit queue slot count
)

// AuditBlock is a fixed-size byte buffer for audit capture. Blocks cycle
// between the BlockPool and the audit queue without returning to the heap.
type AuditBlock struct {
	Data      []byte
	Used      int    // bytes written so far
	Seq       uint32 // sequence number within a request
	RequestID uint64 // monotonic ID assigned per request in Rewrite hook
	Final     bool   // last block for this request (complete capture)
	Abort     bool      // capture abandoned mid-stream; consumer should discard partial payload
	Direction Direction // request or response
}

func (b *AuditBlock) Remaining() int { return len(b.Data) - b.Used }

// Write copies from p into the block. Returns bytes written.
func (b *AuditBlock) Write(p []byte) int {
	n := copy(b.Data[b.Used:], p)
	b.Used += n
	return n
}

func (b *AuditBlock) Reset() {
	b.Used = 0
	b.Seq = 0
	b.RequestID = 0
	b.Final = false
	b.Abort = false
	b.Direction = DirectionRequest
}

// BlockPool manages a fixed set of pre-allocated AuditBlocks using a buffered
// channel. Blocks are pinned in memory (channel holds references), preventing
// GC collection. The pool never grows beyond its initial capacity.
type BlockPool struct {
	ch          chan *AuditBlock
	blockSize   int
	Exhaustions atomic.Int64
}

// NewBlockPool pre-allocates totalBytes/blockSize blocks and returns a pool.
// All blocks are created once at startup and recycled indefinitely.
func NewBlockPool(totalBytes, blockSize int) *BlockPool {
	capacity := totalBytes / blockSize
	if capacity < 1 {
		capacity = 1
	}
	bp := &BlockPool{
		ch:        make(chan *AuditBlock, capacity),
		blockSize: blockSize,
	}
	for range capacity {
		bp.ch <- &AuditBlock{Data: make([]byte, blockSize)}
	}
	return bp
}

// Get acquires a block from the pool. Returns nil if the pool is exhausted.
// Non-blocking: never waits for a block to become available.
func (bp *BlockPool) Get() *AuditBlock {
	select {
	case blk := <-bp.ch:
		blk.Reset()
		return blk
	default:
		bp.Exhaustions.Add(1)
		return nil
	}
}

// Put returns a block to the pool. Non-blocking.
func (bp *BlockPool) Put(blk *AuditBlock) {
	select {
	case bp.ch <- blk:
	default:
		// Pool is full; should not happen if accounting is correct.
	}
}
