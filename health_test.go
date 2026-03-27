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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHealthzUpstreamReachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newProxy(proxyConfig{targetURL: upstreamURL})
	ready := &atomic.Bool{}
	ready.Store(true)

	h := newHealthHandler(proxy, upstreamURL, ready)
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

func TestHealthzUpstreamUnreachable(t *testing.T) {
	// Point at a port where nothing is listening.
	unreachable, _ := url.Parse("http://127.0.0.1:1")
	proxy := newProxy(proxyConfig{targetURL: unreachable})
	ready := &atomic.Bool{}
	ready.Store(true)

	h := newHealthHandler(proxy, unreachable, ready)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz = %d, want 503", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "upstream unreachable") {
		t.Errorf("expected 'upstream unreachable' in body, got: %s", body)
	}
}

func TestReadyzReady(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newProxy(proxyConfig{targetURL: upstreamURL})
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

func TestReadyzNotReady(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newProxy(proxyConfig{targetURL: upstreamURL})
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

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pipeline not ready") {
		t.Errorf("expected 'pipeline not ready' in body, got: %s", body)
	}
}

func TestReadyzTransitionsToReady(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := newProxy(proxyConfig{targetURL: upstreamURL})
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
	proxy := newProxy(proxyConfig{targetURL: upstreamURL})
	ready := &atomic.Bool{}
	ready.Store(true)

	h := newHealthHandler(proxy, upstreamURL, ready)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Non-health paths should be forwarded to upstream.
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
