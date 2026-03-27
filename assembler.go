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
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// apiType identifies the upstream API provider for extractor selection.
type apiType int

const (
	apiTypeUnknown   apiType = iota
	apiTypeOpenAI            // OpenAI chat completions format
	apiTypeAnthropic         // Anthropic messages API format
)

const (
	defaultMaxPayloadSize = 10 * 1024 * 1024 // 10MB per request
	defaultStaleTimeout   = 30 * time.Second
	defaultWorkerQueueSize = 64
	staleSweepInterval    = 5 * time.Second
)

// payloadBuffer accumulates blocks for a single in-flight request.
type payloadBuffer struct {
	data     []byte
	nextSeq  uint32
	lastSeen time.Time
}

// assembledPayload is the unit of work dispatched to the worker pool.
type assembledPayload struct {
	requestID uint64
	data      []byte  // reassembled contiguous request body
	complete  bool    // true = full capture via final; false = truncated or abandoned
	apiType   apiType
}

// assemblerConfig holds tuning parameters for the session assembler.
type assemblerConfig struct {
	maxPayloadSize int
	staleTimeout   time.Duration
	workerInput    chan<- *assembledPayload
	pool           blockAllocator
	apiType        apiType
}

// assemblerStats exposes atomic counters for observability.
type assemblerStats struct {
	oversizedDropped atomic.Int64
	staleDropped     atomic.Int64
}

// startAssembler runs the session assembler goroutine. It reads blocks from
// auditQueue, reassembles them by requestID, and dispatches complete payloads
// to workerInput. It closes workerInput when auditQueue is closed and all
// remaining blocks are drained.
//
// The assembler is the sole owner of the in-flight map — no synchronization
// is needed because only this goroutine accesses it.
func startAssembler(auditQueue <-chan *auditBlock, cfg assemblerConfig, stats *assemblerStats) {
	defer close(cfg.workerInput)

	inflight := make(map[uint64]*payloadBuffer)
	ticker := time.NewTicker(staleSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case blk, ok := <-auditQueue:
			if !ok {
				// Queue closed — evict all remaining buffers.
				for id := range inflight {
					delete(inflight, id)
				}
				return
			}
			processBlock(blk, inflight, cfg, stats)

		case <-ticker.C:
			sweepStale(inflight, cfg.staleTimeout, stats)
		}
	}
}

// processBlock handles a single auditBlock arriving from the queue.
func processBlock(blk *auditBlock, inflight map[uint64]*payloadBuffer, cfg assemblerConfig, stats *assemblerStats) {
	rid := blk.requestID

	// Abort: discard any partial buffer for this request.
	if blk.abort {
		delete(inflight, rid)
		cfg.pool.put(blk)
		return
	}

	buf, exists := inflight[rid]
	if !exists {
		buf = &payloadBuffer{
			lastSeen: time.Now(),
		}
		inflight[rid] = buf
	}

	// Sequence check: if out of order, treat as corrupt and drop.
	if blk.seq != buf.nextSeq {
		delete(inflight, rid)
		cfg.pool.put(blk)
		return
	}

	buf.data = append(buf.data, blk.data[:blk.n]...)
	buf.nextSeq++
	buf.lastSeen = time.Now()

	// Size limit enforcement.
	if len(buf.data) > cfg.maxPayloadSize {
		delete(inflight, rid)
		cfg.pool.put(blk)
		stats.oversizedDropped.Add(1)
		return
	}

	isFinal := blk.final
	cfg.pool.put(blk)

	if isFinal {
		payload := &assembledPayload{
			requestID: rid,
			data:      buf.data,
			complete:  true,
			apiType:   cfg.apiType,
		}
		// Non-blocking send: if worker queue is full, drop the payload.
		select {
		case cfg.workerInput <- payload:
		default:
		}
		delete(inflight, rid)
	}
}

// sweepStale evicts payloads that haven't received a block within the timeout.
func sweepStale(inflight map[uint64]*payloadBuffer, timeout time.Duration, stats *assemblerStats) {
	now := time.Now()
	for id, buf := range inflight {
		if now.Sub(buf.lastSeen) > timeout {
			delete(inflight, id)
			stats.staleDropped.Add(1)
		}
	}
}

// detectAPIType infers the upstream API provider from the target URL hostname.
func detectAPIType(u *url.URL) apiType {
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.Contains(host, "openai"):
		return apiTypeOpenAI
	case strings.Contains(host, "anthropic"):
		return apiTypeAnthropic
	default:
		return apiTypeUnknown
	}
}

// parseAPIFormat converts a --api-format flag value to an apiType.
// Returns apiTypeUnknown for empty or unrecognized values.
func parseAPIFormat(s string) apiType {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "openai":
		return apiTypeOpenAI
	case "anthropic":
		return apiTypeAnthropic
	default:
		return apiTypeUnknown
	}
}
