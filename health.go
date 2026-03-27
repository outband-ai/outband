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
	"log"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

const upstreamDialTimeout = 2 * time.Second

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

// handleHealthz is a process-level liveness probe. It returns 200 if the
// server goroutine is running and can serve requests. No external checks —
// if this handler executes, the process is alive.
func (h *healthHandler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// handleReadyz returns 200 only after the audit pipeline is initialized AND
// the upstream target is reachable. If audit is disabled, readiness only
// requires upstream reachability.
//
// Connection errors are logged server-side but never exposed to clients.
func (h *healthHandler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !h.ready.Load() {
		log.Print("readyz: pipeline not ready")
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}

	addr := hostPort(h.targetURL)
	conn, err := net.DialTimeout("tcp", addr, upstreamDialTimeout)
	if err != nil {
		log.Printf("readyz: upstream unreachable: %v", err)
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	defer conn.Close()

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
