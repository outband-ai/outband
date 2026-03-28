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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// unreachableURL returns an http URL pointing at a port that is guaranteed
// to have nothing listening. It binds to :0 to get an OS-assigned port,
// then immediately closes the listener so the port is free but unused.
func unreachableURL(t *testing.T) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for ephemeral port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	return u
}

func TestHealthzAlwaysOK(t *testing.T) {
	// /healthz is a process-level liveness probe — always 200.
	unreachable := unreachableURL(t)
	proxy := newProxy(&ProxyConfig{TargetURL: unreachable})
	ready := &atomic.Bool{} // not ready, doesn't matter for liveness

	h := newHealthHandler(proxy, unreachable, ready)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /healthz = %d, want 200; body: %s", resp.StatusCode, body)
	}
}

func TestReadyzReady(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newProxy(&ProxyConfig{TargetURL: upstreamURL})
	ready := &atomic.Bool{}
	ready.Store(true)

	h := newHealthHandler(proxy, upstreamURL, ready)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want 200", resp.StatusCode)
	}
}

func TestReadyzNotReadyPipelineUninitialized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newProxy(&ProxyConfig{TargetURL: upstreamURL})
	ready := &atomic.Bool{} // not stored — defaults to false

	h := newHealthHandler(proxy, upstreamURL, ready)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want 503", resp.StatusCode)
	}

	// Error details must NOT be exposed to clients.
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "pipeline") {
		t.Errorf("internal details leaked in response body: %s", body)
	}
}

func TestReadyzNotReadyUpstreamUnreachable(t *testing.T) {
	unreachable := unreachableURL(t)
	proxy := newProxy(&ProxyConfig{TargetURL: unreachable})
	ready := &atomic.Bool{}
	ready.Store(true) // pipeline ready, but upstream down

	h := newHealthHandler(proxy, unreachable, ready)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want 503", resp.StatusCode)
	}

	// Must not leak connection error details to clients.
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "dial") || strings.Contains(string(body), "connection") {
		t.Errorf("internal error details leaked in response body: %s", body)
	}
}

func TestReadyzTransitionsToReady(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newProxy(&ProxyConfig{TargetURL: upstreamURL})
	ready := &atomic.Bool{}

	h := newHealthHandler(proxy, upstreamURL, ready)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Initially not ready.
	resp, _ := http.Get(srv.URL + "/readyz")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("before: GET /readyz = %d, want 503", resp.StatusCode)
	}

	// Simulate pipeline initialization completing.
	ready.Store(true)

	resp, _ = http.Get(srv.URL + "/readyz")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after: GET /readyz = %d, want 200", resp.StatusCode)
	}
}

func TestHealthHandlerProxiesNonHealthPaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newProxy(&ProxyConfig{TargetURL: upstreamURL})
	ready := &atomic.Bool{}
	ready.Store(true)

	h := newHealthHandler(proxy, upstreamURL, ready)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/chat/completions = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Upstream-Path"); got != "/v1/chat/completions" {
		t.Errorf("upstream saw path %q, want /v1/chat/completions", got)
	}
}

func TestHostPort(t *testing.T) {
	tests := []struct {
		rawURL string
		want   string
	}{
		{"https://api.openai.com", "api.openai.com:443"},
		{"http://api.openai.com", "api.openai.com:80"},
		{"http://localhost:9090", "localhost:9090"},
		{"https://example.com:8443/path", "example.com:8443"},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.rawURL, err)
		}
		if got := hostPort(u); got != tt.want {
			t.Errorf("hostPort(%q) = %q, want %q", tt.rawURL, got, tt.want)
		}
	}
}
