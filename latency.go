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
	"net/http"
	"time"
)

type contextKey int

const requestStartKey contextKey = iota

// latencyMiddleware stamps request arrival time into the request context.
// It does NOT measure elapsed time — that happens in the Rewrite hook,
// which is the last code that executes before http.Transport dials upstream.
//
// Wrapping ServeHTTP with time.Since(start) would measure the full SSE
// stream duration (5–20 seconds), not proxy overhead.
type latencyMiddleware struct {
	next http.Handler
}

func newLatencyMiddleware(next http.Handler) *latencyMiddleware {
	return &latencyMiddleware{next: next}
}

func (m *latencyMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), requestStartKey, time.Now())
	m.next.ServeHTTP(w, r.WithContext(ctx))
}

// requestStart extracts the arrival timestamp from the request context.
// Returns the zero time and false if the middleware did not stamp the context.
func requestStart(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(requestStartKey).(time.Time)
	return t, ok
}
