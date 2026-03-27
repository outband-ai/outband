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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollectorBatchFlush(t *testing.T) {
	input := make(chan *telemetryLog, 100)
	var mu sync.Mutex
	var flushed []*telemetryLog

	stats := &collectorStats{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		startCollector(collectorConfig{
			batchSize:     5,
			flushInterval: 10 * time.Second, // long timer — batch size triggers flush
			input:         input,
			flush: func(batch []*telemetryLog) {
				mu.Lock()
				flushed = append(flushed, batch...)
				mu.Unlock()
			},
		}, stats)
	}()

	// Send exactly batchSize entries.
	for i := range 5 {
		input <- &telemetryLog{RequestID: uint64(i)}
	}

	// Give the async flush goroutine time to complete.
	time.Sleep(100 * time.Millisecond)
	close(input)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 5 {
		t.Errorf("flushed %d entries, want 5", len(flushed))
	}
}

func TestCollectorTimerFlush(t *testing.T) {
	input := make(chan *telemetryLog, 100)
	var mu sync.Mutex
	var flushed []*telemetryLog

	stats := &collectorStats{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		startCollector(collectorConfig{
			batchSize:     1000, // large — timer triggers flush
			flushInterval: 50 * time.Millisecond,
			input:         input,
			flush: func(batch []*telemetryLog) {
				mu.Lock()
				flushed = append(flushed, batch...)
				mu.Unlock()
			},
		}, stats)
	}()

	// Send fewer than batchSize entries.
	for i := range 3 {
		input <- &telemetryLog{RequestID: uint64(i)}
	}

	// Wait for timer to tick and flush.
	time.Sleep(200 * time.Millisecond)
	close(input)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 3 {
		t.Errorf("flushed %d entries, want 3", len(flushed))
	}
}

func TestCollectorShutdown(t *testing.T) {
	input := make(chan *telemetryLog, 100)
	var mu sync.Mutex
	var flushed []*telemetryLog

	stats := &collectorStats{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		startCollector(collectorConfig{
			batchSize:     1000,
			flushInterval: 10 * time.Second,
			input:         input,
			flush: func(batch []*telemetryLog) {
				mu.Lock()
				flushed = append(flushed, batch...)
				mu.Unlock()
			},
		}, stats)
	}()

	// Send entries and immediately close — final flush should catch them.
	for i := range 7 {
		input <- &telemetryLog{RequestID: uint64(i)}
	}
	close(input)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not exit after input closed")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(flushed) != 7 {
		t.Errorf("flushed %d entries, want 7", len(flushed))
	}
}

func TestCollectorDoubleBuffer(t *testing.T) {
	input := make(chan *telemetryLog, 100)
	gate := make(chan struct{}) // blocks flush until released
	var flushCount atomic.Int64

	stats := &collectorStats{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		startCollector(collectorConfig{
			batchSize:     3,
			flushInterval: 50 * time.Millisecond,
			input:         input,
			flush: func(batch []*telemetryLog) {
				flushCount.Add(1)
				<-gate // block until test releases
			},
		}, stats)
	}()

	// Send first batch — triggers async flush that blocks on gate.
	for i := range 3 {
		input <- &telemetryLog{RequestID: uint64(i)}
	}
	time.Sleep(100 * time.Millisecond)

	// Send more entries while flush is blocked — they accumulate in activeBatch.
	for i := range 3 {
		input <- &telemetryLog{RequestID: uint64(10 + i)}
	}
	time.Sleep(100 * time.Millisecond)

	// The second batch should have triggered a tryFlush that was skipped
	// because the first flush is still running.
	if stats.flushBacklog.Load() == 0 {
		t.Log("note: flush backlog not yet triggered (timing-dependent)")
	}

	// Release the gate — allow flushes to complete.
	close(gate)
	close(input)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not exit")
	}
}

func TestCollectorFlushBackpressure(t *testing.T) {
	input := make(chan *telemetryLog, 200)
	gate := make(chan struct{}) // blocks flush

	stats := &collectorStats{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		startCollector(collectorConfig{
			batchSize:     2,
			flushInterval: 20 * time.Millisecond,
			input:         input,
			flush: func(batch []*telemetryLog) {
				<-gate
			},
		}, stats)
	}()

	// Send first batch to trigger a blocking flush.
	input <- &telemetryLog{RequestID: 1}
	input <- &telemetryLog{RequestID: 2}
	time.Sleep(50 * time.Millisecond)

	// Send more batches while flush is blocked.
	for i := range 10 {
		input <- &telemetryLog{RequestID: uint64(10 + i)}
	}
	// Wait for timer ticks to attempt flushes.
	time.Sleep(200 * time.Millisecond)

	if stats.flushBacklog.Load() == 0 {
		t.Error("expected flushBacklog > 0")
	}

	close(gate)
	close(input)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not exit")
	}
}
