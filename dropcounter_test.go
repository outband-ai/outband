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
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDropCounterIncrement(t *testing.T) {
	dc := newDropCounter()
	for range 100 {
		dc.drop()
	}
	if got := dc.total.Load(); got != 100 {
		t.Errorf("total = %d, want 100", got)
	}
	if got := dc.interval.Load(); got != 100 {
		t.Errorf("interval = %d, want 100", got)
	}
}

func TestDropCounterPoll(t *testing.T) {
	dc := newDropCounter()

	for range 5 {
		dc.drop()
	}
	n, total := dc.poll()
	if n != 5 || total != 5 {
		t.Errorf("poll() = (%d, %d), want (5, 5)", n, total)
	}

	// Interval should be reset; total should be monotonic.
	for range 3 {
		dc.drop()
	}
	n, total = dc.poll()
	if n != 3 || total != 8 {
		t.Errorf("poll() = (%d, %d), want (3, 8)", n, total)
	}

	// No new drops — interval should be 0.
	n, total = dc.poll()
	if n != 0 || total != 8 {
		t.Errorf("poll() = (%d, %d), want (0, 8)", n, total)
	}
}

func TestDropCounterConcurrent(t *testing.T) {
	dc := newDropCounter()
	const goroutines = 100
	const dropsPerGoroutine = 1000

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range dropsPerGoroutine {
				dc.drop()
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * dropsPerGoroutine)
	if got := dc.total.Load(); got != want {
		t.Errorf("total = %d, want %d", got, want)
	}
}

func TestDropPollerEmitsWarning(t *testing.T) {
	dc := newDropCounter()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	stop := startDropPoller(context.Background(), dc, 100*time.Millisecond, logger)

	// Trigger drops.
	for range 10 {
		dc.drop()
	}

	// Wait for poller to fire, then stop before reading buffer to avoid race.
	time.Sleep(250 * time.Millisecond)
	stop()

	output := buf.String()
	if !strings.Contains(output, "Dropped 10 payloads") {
		t.Errorf("expected warning about 10 dropped payloads, got: %q", output)
	}
	if !strings.Contains(output, "total: 10") {
		t.Errorf("expected total: 10 in output, got: %q", output)
	}
}

func TestDropPollerSilentWhenNone(t *testing.T) {
	dc := newDropCounter()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	stop := startDropPoller(context.Background(), dc, 50*time.Millisecond, logger)

	// Wait for a couple poll cycles with no drops.
	time.Sleep(150 * time.Millisecond)
	stop()

	if buf.Len() != 0 {
		t.Errorf("expected no output when no drops, got: %q", buf.String())
	}
}

func TestDropPollerShutdown(t *testing.T) {
	dc := newDropCounter()
	logger := log.New(&bytes.Buffer{}, "", 0)

	stop := startDropPoller(context.Background(), dc, 100*time.Millisecond, logger)

	// stop should return promptly.
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return within 2 seconds")
	}
}
