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
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultBatchSize       = 1000
	defaultFlushInterval   = 5 * time.Second
	defaultCollectorQueueSize = 128
)

// flushFunc is the callback invoked with a batch of telemetry logs.
// Implementations must be safe to call from a goroutine.
type flushFunc func(batch []*TelemetryLog)

// collectorConfig holds tuning parameters for the collector.
type collectorConfig struct {
	batchSize     int
	flushInterval time.Duration
	input         <-chan *TelemetryLog
	flush         flushFunc
}

// collectorStats exposes atomic counters for observability.
type collectorStats struct {
	flushBacklog atomic.Int64 // incremented when a flush is skipped because a prior flush is still running
}

// startCollector runs the collector goroutine. It reads telemetry logs from
// input, accumulates them into batches, and flushes via the configured
// flushFunc. Uses a double-buffer pattern: activeBatch receives new entries
// while flushBatch is being processed by an async flush goroutine.
//
// Returns when input is closed and all remaining entries are flushed.
func startCollector(cfg collectorConfig, stats *collectorStats) {
	activeBatch := make([]*TelemetryLog, 0, cfg.batchSize)
	var flushing atomic.Bool
	var flushWg sync.WaitGroup

	ticker := time.NewTicker(cfg.flushInterval)
	defer ticker.Stop()

	tryFlush := func() {
		if len(activeBatch) == 0 {
			return
		}
		if flushing.Load() {
			stats.flushBacklog.Add(1)
			return
		}
		// Swap: hand off activeBatch to the flush goroutine.
		flushBatch := activeBatch
		activeBatch = make([]*TelemetryLog, 0, cfg.batchSize)
		flushing.Store(true)
		flushWg.Add(1)
		go func() {
			defer flushWg.Done()
			defer flushing.Store(false)
			cfg.flush(flushBatch)
		}()
	}

	for {
		select {
		case entry, ok := <-cfg.input:
			if !ok {
				// Input closed — wait for any in-flight flush, then final flush.
				flushWg.Wait()
				if len(activeBatch) > 0 {
					cfg.flush(activeBatch)
				}
				return
			}
			activeBatch = append(activeBatch, entry)
			if len(activeBatch) >= cfg.batchSize {
				tryFlush()
			}

		case <-ticker.C:
			tryFlush()
		}
	}
}
