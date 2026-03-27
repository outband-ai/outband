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
	"io"
	"sync"
)

// blockAllocator abstracts the block pool for testability.
type blockAllocator interface {
	get() *auditBlock
	put(*auditBlock)
}

// dropRecorder abstracts the drop counter for testability.
type dropRecorder interface {
	drop()
}

// auditReader wraps an io.ReadCloser and copies bytes into audit blocks
// as the reverse proxy streams the request body to upstream. Filled blocks
// are sent to a global audit queue via non-blocking channel sends.
//
// If the pool is exhausted or the queue is full, capture is abandoned for
// that request. The upstream connection is never blocked by the audit path.
type auditReader struct {
	src       io.ReadCloser
	pool      blockAllocator
	drops     dropRecorder
	queue     chan<- *auditBlock
	current   *auditBlock
	abandoned bool
	finalized bool
	seq       uint32
	requestID uint64
	closeOnce sync.Once
	closed    bool
}

func newAuditReader(src io.ReadCloser, pool blockAllocator, drops dropRecorder, queue chan<- *auditBlock, requestID uint64) *auditReader {
	return &auditReader{
		src:       src,
		pool:      pool,
		drops:     drops,
		queue:     queue,
		requestID: requestID,
	}
}

// Read implements io.Reader. It reads from the underlying body and copies
// bytes into audit blocks on the side. The return value always reflects
// the underlying read — audit failures never affect the proxy path.
func (a *auditReader) Read(p []byte) (int, error) {
	n, err := a.src.Read(p)

	if n > 0 && !a.abandoned {
		a.capture(p[:n])
	}

	if err == io.EOF && !a.abandoned {
		a.finish()
	}

	return n, err
}

// capture copies data into audit blocks, acquiring new blocks as needed.
func (a *auditReader) capture(data []byte) {
	for len(data) > 0 {
		if a.current == nil {
			blk := a.pool.get()
			if blk == nil {
				a.abandon()
				return
			}
			blk.requestID = a.requestID
			blk.seq = a.seq
			a.seq++
			a.current = blk
		}

		written := a.current.write(data)
		data = data[written:]

		if a.current.remaining() == 0 {
			if len(data) > 0 {
				// More data to capture — check if we can continue before
				// sending the full block. If not, mark it as abort so the
				// consumer knows this request's capture is incomplete.
				next := a.pool.get()
				if next == nil {
					a.current.abort = true
					if !a.sendBlock(a.current) {
						a.pool.put(a.current)
					}
					a.current = nil
					if a.drops != nil {
						a.drops.drop()
					}
					a.abandoned = true
					return
				}
				if !a.sendBlock(a.current) {
					a.pool.put(a.current)
					a.pool.put(next)
					a.current = nil
					if a.drops != nil {
						a.drops.drop()
					}
					a.abandoned = true
					return
				}
				next.requestID = a.requestID
				next.seq = a.seq
				a.seq++
				a.current = next
			} else {
				// Block is full and no more data in this Read call.
				// Send it; next Read will acquire a new block if needed.
				if !a.sendBlock(a.current) {
					a.abandon()
					return
				}
				a.current = nil
			}
		}
	}
}

// sendBlock performs a non-blocking send of a block to the audit queue.
// It recovers from panics caused by sending on a closed channel during
// server shutdown, treating them as a failed send.
func (a *auditReader) sendBlock(blk *auditBlock) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	select {
	case a.queue <- blk:
		return true
	default:
		return false
	}
}

// finish sends the current block (if any) as the final block.
// If the last block was already sent (payload exactly filled it),
// a zero-length sentinel block carries the final flag.
func (a *auditReader) finish() {
	if a.finalized {
		return
	}
	a.finalized = true
	if a.current == nil {
		if a.seq == 0 {
			return // empty body — nothing to finalize
		}
		// Previous block was full and already sent; need a sentinel.
		blk := a.pool.get()
		if blk == nil {
			if a.drops != nil {
				a.drops.drop()
			}
			a.abandoned = true
			return
		}
		blk.requestID = a.requestID
		blk.seq = a.seq
		blk.final = true
		if !a.sendBlock(blk) {
			a.pool.put(blk)
			if a.drops != nil {
				a.drops.drop()
			}
			a.abandoned = true
		}
		return
	}
	a.current.final = true
	if !a.sendBlock(a.current) {
		a.pool.put(a.current)
		if a.drops != nil {
			a.drops.drop()
		}
	}
	a.current = nil
}

// abandon stops audit capture for this request. It attempts to send an
// abort signal block so the consumer knows to discard partial data.
func (a *auditReader) abandon() {
	if a.current != nil {
		a.current.abort = true
		if !a.sendBlock(a.current) {
			a.pool.put(a.current)
		}
		a.current = nil
	}
	if a.drops != nil {
		a.drops.drop()
	}
	a.abandoned = true
}

// Close implements io.Closer. It finalizes or abandons audit capture
// and closes the underlying body. Safe to call multiple times.
func (a *auditReader) Close() error {
	var err error
	a.closeOnce.Do(func() {
		a.closed = true
		if !a.abandoned {
			a.finish()
		} else if a.current != nil {
			a.pool.put(a.current)
			a.current = nil
		}
		err = a.src.Close()
	})
	return err
}
