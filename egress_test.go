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
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testSummary() *EvidenceSummary {
	return &EvidenceSummary{
		WindowStart:               time.Date(2026, 3, 27, 14, 0, 0, 0, time.UTC),
		WindowEnd:                 time.Date(2026, 3, 27, 15, 0, 0, 0, time.UTC),
		TotalRequestsProcessed:    1000,
		TotalRequestsAudited:      998,
		AuditCoveragePercent:      99.8,
		RedactionEventsByCategory: map[string]uint64{"SSN": 5, "EMAIL": 10},
		RedactionLevel:            "pattern-based",
		SchemaVersion:             "1.0.0",
		SidecarVersion:            "v0.4.0",
		SOC2ControlsSatisfied:     []string{"CC6.1", "CC6.6", "CC9.2"},
	}
}

func TestLocalJSONTargetWritesFile(t *testing.T) {
	dir := t.TempDir()
	target := NewLocalJSONTarget(dir)
	summary := testSummary()

	if err := target.Push(context.Background(), summary); err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(dir, "evidence-2026-03-27T14-00-00.json")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}

	var got EvidenceSummary
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.TotalRequestsProcessed != 1000 {
		t.Fatalf("processed: got %d, want 1000", got.TotalRequestsProcessed)
	}
	if got.RedactionEventsByCategory["SSN"] != 5 {
		t.Fatalf("SSN: got %d, want 5", got.RedactionEventsByCategory["SSN"])
	}
}

func TestLocalJSONTargetName(t *testing.T) {
	target := NewLocalJSONTarget(".")
	if target.Name() != "local-json" {
		t.Fatalf("name: got %q", target.Name())
	}
}

func TestWebhookTargetSuccess(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := NewWebhookTarget(srv.URL, map[string]string{"X-Auth": "token123"})
	if err := target.Push(context.Background(), testSummary()); err != nil {
		t.Fatal(err)
	}

	var got EvidenceSummary
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatalf("invalid JSON received: %v", err)
	}
	if got.TotalRequestsProcessed != 1000 {
		t.Fatalf("processed: got %d", got.TotalRequestsProcessed)
	}
}

func TestWebhookTargetRetryOn500(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := NewWebhookTarget(srv.URL, nil)
	if err := target.Push(context.Background(), testSummary()); err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts: got %d, want 3", attempts.Load())
	}
}

func TestWebhookTargetNoRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	target := NewWebhookTarget(srv.URL, nil)
	err := target.Push(context.Background(), testSummary())
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts: got %d, want 1 (no retry on 4xx)", attempts.Load())
	}
}

func TestWebhookTargetContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	target := NewWebhookTarget(srv.URL, nil)
	err := target.Push(ctx, testSummary())
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestWebhookTargetName(t *testing.T) {
	target := NewWebhookTarget("http://example.com", nil)
	if target.Name() != "webhook" {
		t.Fatalf("name: got %q", target.Name())
	}
}

func TestWebhookTargetHeaders(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := NewWebhookTarget(srv.URL, map[string]string{"Authorization": "Bearer xyz"})
	if err := target.Push(context.Background(), testSummary()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer xyz" {
		t.Fatalf("auth header: got %q, want %q", gotAuth, "Bearer xyz")
	}
}

// mockTarget records calls for testing dispatcher ordering.
type mockTarget struct {
	name   string
	calls  int
	err    error
}

func (m *mockTarget) Push(_ context.Context, _ *EvidenceSummary) error {
	m.calls++
	return m.err
}

func (m *mockTarget) Name() string { return m.name }

func TestEgressDispatcherCallsInOrder(t *testing.T) {
	t1 := &mockTarget{name: "first"}
	t2 := &mockTarget{name: "second"}
	t3 := &mockTarget{name: "third"}

	logger := log.New(io.Discard, "", 0)
	d := NewEgressDispatcher(logger, t1, t2, t3)

	if err := d.Dispatch(context.Background(), testSummary()); err != nil {
		t.Fatal(err)
	}

	if t1.calls != 1 || t2.calls != 1 || t3.calls != 1 {
		t.Fatalf("calls: first=%d second=%d third=%d", t1.calls, t2.calls, t3.calls)
	}
}

func TestEgressDispatcherContinuesOnError(t *testing.T) {
	t1 := &mockTarget{name: "first", err: io.ErrUnexpectedEOF}
	t2 := &mockTarget{name: "second"}

	logger := log.New(io.Discard, "", 0)
	d := NewEgressDispatcher(logger, t1, t2)

	err := d.Dispatch(context.Background(), testSummary())
	if err == nil {
		t.Fatal("expected error from first target")
	}
	if t2.calls != 1 {
		t.Fatal("second target not called after first target error")
	}
}

func TestEgressDispatcherStopsOnContextCancel(t *testing.T) {
	t1 := &mockTarget{name: "first"}
	t2 := &mockTarget{name: "second"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	logger := log.New(io.Discard, "", 0)
	d := NewEgressDispatcher(logger, t1, t2)
	_ = d.Dispatch(ctx, testSummary())

	// With cancelled context, dispatcher should break before calling any target.
	if t1.calls != 0 {
		t.Fatal("first target should not be called with cancelled context")
	}
}

func TestRegisterAndGetTarget(t *testing.T) {
	RegisterTarget("test-target", func(config map[string]string) EgressTarget {
		return &mockTarget{name: "registered"}
	})

	factory, ok := GetTarget("test-target")
	if !ok {
		t.Fatal("registered target not found")
	}
	target := factory(nil)
	if target.Name() != "registered" {
		t.Fatalf("name: got %q", target.Name())
	}

	_, ok = GetTarget("nonexistent")
	if ok {
		t.Fatal("found nonexistent target")
	}

	// Cleanup.
	registryMu.Lock()
	delete(registeredTargets, "test-target")
	registryMu.Unlock()
}
