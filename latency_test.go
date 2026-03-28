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
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestStartRoundTrip(t *testing.T) {
	before := time.Now()
	ctx := context.WithValue(context.Background(), requestStartKey, time.Now())
	after := time.Now()

	got, ok := requestStart(ctx)
	if !ok {
		t.Fatal("requestStart returned false for stamped context")
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("timestamp %v not in [%v, %v]", got, before, after)
	}
}

func TestRequestStartMissing(t *testing.T) {
	_, ok := requestStart(context.Background())
	if ok {
		t.Fatal("requestStart returned true for empty context")
	}
}

func TestLatencyMiddlewareStampsContext(t *testing.T) {
	var gotStart time.Time
	var gotOK bool

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStart, gotOK = requestStart(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := newLatencyMiddleware(inner)
	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	before := time.Now()
	mw.ServeHTTP(rec, req)

	if !gotOK {
		t.Fatal("inner handler did not find requestStart in context")
	}
	if gotStart.Before(before) {
		t.Fatalf("start time %v is before test start %v", gotStart, before)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestLatencyMiddlewarePassthrough(t *testing.T) {
	body := "hello world"
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "test")
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte(body))
	})

	mw := newLatencyMiddleware(inner)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected %d, got %d", http.StatusTeapot, rec.Code)
	}
	if rec.Header().Get("X-Custom") != "test" {
		t.Fatal("custom header not preserved")
	}
	if rec.Body.String() != body {
		t.Fatalf("body mismatch: got %q", rec.Body.String())
	}
}
