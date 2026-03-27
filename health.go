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
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

const healthDialTimeout = 2 * time.Second

// healthHandler intercepts /healthz and /readyz requests before they reach
// the reverse proxy. All other paths are forwarded to the proxy untouched.
type healthHandler struct {
	proxy     http.Handler
	targetURL *url.URL
	ready     *atomic.Bool
}

func newHealthHandler(proxy http.Handler, targetURL *url.URL, ready *atomic.Bool) *healthHandler {
	return &healthHandler{
		proxy:     proxy,
		targetURL: targetURL,
		ready:     ready,
	}
}

func (h *healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		h.handleHealthz(w, r)
	case "/readyz":
		h.handleReadyz(w, r)
	default:
		h.proxy.ServeHTTP(w, r)
	}
}

// handleHealthz returns 200 if the proxy can establish a TCP connection to
// the upstream target. A successful TCP dial proves network reachability
// without sending an HTTP request the upstream might not expect.
func (h *healthHandler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	addr := hostPort(h.targetURL)
	conn, err := net.DialTimeout("tcp", addr, healthDialTimeout)
	if err != nil {
		http.Error(w, "upstream unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	conn.Close()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// handleReadyz returns 200 only after the audit pipeline (ring buffer,
// worker pool, collector) is fully initialized. If audit is disabled
// (--audit-capacity=0), readiness is immediate since there is no pipeline
// to wait for.
func (h *healthHandler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !h.ready.Load() {
		http.Error(w, "pipeline not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// hostPort extracts the host:port from a URL, defaulting to port 443 for
// https and port 80 for http when no explicit port is present.
func hostPort(u *url.URL) string {
	if h := u.Host; portIncluded(h) {
		return h
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
		return net.JoinHostPort(host, "443")
	default:
		return net.JoinHostPort(host, "80")
	}
}

// portIncluded reports whether the host string already includes a port.
func portIncluded(host string) bool {
	_, _, err := net.SplitHostPort(host)
	return err == nil
}
