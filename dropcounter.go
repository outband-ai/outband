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
	"context"
	"log"
	"sync/atomic"
	"time"
)

const defaultDropPollInterval = 5 * time.Second

// dropCounter tracks audit capture abandonment events.
// The hot path (TeeReader) only touches atomic counters — zero I/O.
// A background goroutine periodically polls and logs aggregate warnings.
type dropCounter struct {
	total    atomic.Int64 // monotonically increasing; included in compliance reports
	interval atomic.Int64 // drops since last poll; reset by poller
}

func newDropCounter() *dropCounter {
	return &dropCounter{}
}

// drop increments both counters. Called from the TeeReader hot path.
// Zero allocations, zero I/O.
func (d *dropCounter) drop() {
	d.total.Add(1)
	d.interval.Add(1)
}

// poll returns the interval count (resetting it) and the monotonic total.
// Called only by the background poller goroutine.
func (d *dropCounter) poll() (intervalDrops, totalDrops int64) {
	intervalDrops = d.interval.Swap(0)
	totalDrops = d.total.Load()
	return
}

// startDropPoller launches a background goroutine that logs aggregate
// drop warnings. Returns a stop function for graceful shutdown.
func startDropPoller(ctx context.Context, dc *dropCounter, interval time.Duration, logger *log.Logger) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				n, total := dc.poll()
				if n > 0 {
					logger.Printf("WARN: Dropped %d payloads in last %s due to audit buffer pressure (total: %d)", n, interval, total)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return func() {
		cancel()
		<-done
		// Flush any drops that accumulated since the last tick.
		n, total := dc.poll()
		if n > 0 {
			logger.Printf("WARN: Dropped %d payloads in last %s due to audit buffer pressure (total: %d)", n, interval, total)
		}
	}
}
