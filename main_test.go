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
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// proxyTestEnv sets up an upstream server and a proxy pointed at it.
// Caller must defer env.close().
type proxyTestEnv struct {
	Upstream    *httptest.Server
	Proxy       *httptest.Server
	UpstreamURL *url.URL
}

func (e *proxyTestEnv) close() {
	e.Proxy.Close()
	e.Upstream.Close()
}

func newProxyTestEnv(upstream http.Handler) *proxyTestEnv {
	us := httptest.NewServer(upstream)
	usURL, _ := url.Parse(us.URL)
	p := newProxy(proxyConfig{targetURL: usURL})
	ps := httptest.NewServer(p)
	return &proxyTestEnv{Upstream: us, Proxy: ps, UpstreamURL: usURL}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100.0)
	return sorted[idx]
}

// ---------------------------------------------------------------------------
// Functional Tests: Header Integrity
// ---------------------------------------------------------------------------

func TestHeaderTransparency(t *testing.T) {
	headersCh := make(chan http.Header, 1)
	env := newProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersCh <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer env.close()

	t.Run("no_user_agent_leak", func(t *testing.T) {
		req, _ := http.NewRequest("GET", env.Proxy.URL+"/v1/chat/completions", nil)
		// Setting an explicitly empty User-Agent suppresses Go's default
		// "Go-http-client/1.1" in Request.write() -- this is the only way
		// to send a request with no User-Agent header from Go's HTTP stack.
		req.Header["User-Agent"] = []string{""}

		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		got := <-headersCh
		if ua := got.Get("User-Agent"); ua != "" {
			t.Errorf("upstream received User-Agent %q, want empty", ua)
		}
	})

	t.Run("no_x_forwarded_leak", func(t *testing.T) {
		req, _ := http.NewRequest("GET", env.Proxy.URL+"/v1/models", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		got := <-headersCh
		for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host"} {
			if v := got.Get(h); v != "" {
				t.Errorf("upstream received %s=%q, want absent", h, v)
			}
		}
	})

	t.Run("auth_preservation", func(t *testing.T) {
		req, _ := http.NewRequest("GET", env.Proxy.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer sk-test123")
		req.Header.Set("Anthropic-Version", "2023-06-01")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		got := <-headersCh
		if v := got.Get("Authorization"); v != "Bearer sk-test123" {
			t.Errorf("Authorization = %q, want %q", v, "Bearer sk-test123")
		}
		if v := got.Get("Anthropic-Version"); v != "2023-06-01" {
			t.Errorf("Anthropic-Version = %q, want %q", v, "2023-06-01")
		}
	})

	t.Run("hop_by_hop_suppression", func(t *testing.T) {
		req, _ := http.NewRequest("GET", env.Proxy.URL+"/v1/models", nil)
		req.Header.Set("Connection", "close")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		got := <-headersCh
		if v := got.Get("Connection"); v != "" {
			t.Errorf("upstream received Connection=%q, want absent (hop-by-hop)", v)
		}
	})
}

// ---------------------------------------------------------------------------
// Functional Tests: Protocol Fidelity
// ---------------------------------------------------------------------------

func TestSSEBoundaryPreservation(t *testing.T) {
	frames := []string{
		"data: {\"chunk\": 1}\n\n",
		"data: {\"chunk\": 2}\n\n",
		"data: {\"chunk\": 3}\n\n",
	}

	env := newProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not implement Flusher")
			return
		}
		for _, frame := range frames {
			fmt.Fprint(w, frame)
			flusher.Flush()
		}
	}))
	defer env.close()

	resp, err := http.Get(env.Proxy.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Join(frames, "")
	if string(body) != want {
		t.Errorf("SSE body mismatch:\ngot:  %q\nwant: %q", body, want)
	}

	// Verify individual frame boundaries by splitting on \n\n
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
			return i + 2, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})

	var count int
	for scanner.Scan() {
		count++
	}
	if count != 3 {
		t.Errorf("got %d SSE frames, want 3", count)
	}
}

func TestLargePayloadStreaming(t *testing.T) {
	const payloadSize = 50 * 1024 * 1024 // 50MB

	firstByteReceived := make(chan time.Time, 1)
	allBytesReceived := make(chan int64, 1)

	env := newProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64*1024)
		var total int64
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				total += int64(n)
				if total == int64(n) {
					firstByteReceived <- time.Now()
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("upstream read error: %v", err)
				break
			}
		}
		allBytesReceived <- total
		w.WriteHeader(http.StatusOK)
	}))
	defer env.close()

	// Slow reader that produces 50MB in 64KB chunks
	pr, pw := io.Pipe()
	uploadStart := time.Now()
	go func() {
		chunk := make([]byte, 64*1024)
		var written int64
		for written < payloadSize {
			n := int64(len(chunk))
			if written+n > payloadSize {
				n = payloadSize - written
			}
			pw.Write(chunk[:n])
			written += n
			// Small delay to simulate slow upload -- enough to prove streaming
			if written < payloadSize {
				time.Sleep(1 * time.Millisecond)
			}
		}
		pw.Close()
	}()

	req, _ := http.NewRequest("POST", env.Proxy.URL+"/v1/chat/completions", pr)
	req.ContentLength = -1 // Force chunked transfer
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	firstByte := <-firstByteReceived
	total := <-allBytesReceived

	// Upstream must start receiving bytes well before the entire upload completes
	streamingDelay := firstByte.Sub(uploadStart)
	if streamingDelay > 5*time.Second {
		t.Errorf("upstream received first byte after %v, proxy may be buffering the entire request", streamingDelay)
	}
	if total != payloadSize {
		t.Errorf("upstream received %d bytes, want %d", total, payloadSize)
	}
}

func TestQueryStringVerbatim(t *testing.T) {
	rawQueryCh := make(chan string, 1)
	env := newProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQueryCh <- r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer env.close()

	wantQuery := "model=gpt-4&stream=true&special=%2F%2B"
	resp, err := http.Get(env.Proxy.URL + "/v1/chat/completions?" + wantQuery)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := <-rawQueryCh
	if got != wantQuery {
		t.Errorf("query string mismatch:\ngot:  %q\nwant: %q", got, wantQuery)
	}
}

// ---------------------------------------------------------------------------
// Functional Tests: Lifecycle & Error Handling
// ---------------------------------------------------------------------------

func TestUpstreamTimeout(t *testing.T) {
	// Channel to unblock the hanging handler on test cleanup
	done := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-done // Block until test signals cleanup
	}))
	defer func() {
		close(done)
		upstream.Close()
	}()

	usURL, _ := url.Parse(upstream.URL)
	transport := &http.Transport{
		ResponseHeaderTimeout: 500 * time.Millisecond,
		DisableCompression:    true,
	}

	p := newProxy(proxyConfig{targetURL: usURL})
	p.Transport = transport
	timeoutProxy := httptest.NewServer(p)
	defer timeoutProxy.Close()

	resp, err := http.Get(timeoutProxy.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d (502 Bad Gateway on upstream timeout)", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestUpstreamDisconnectMidStream(t *testing.T) {
	env := newProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write partial response then abort
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"data": "par`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hijack and close to simulate abrupt disconnect
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}))
	defer env.close()

	resp, err := http.Get(env.Proxy.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read should encounter an error or return truncated data -- the proxy
	// must propagate the disconnect, not hang or fabricate a complete response.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// Read error means the proxy correctly propagated the disconnect.
		return
	}
	// No read error -- verify we got the truncated data, not something fabricated.
	if string(body) != `{"data": "par` {
		t.Errorf("expected truncated body %q, got %q", `{"data": "par`, string(body))
	}
}

func TestErrorPassthrough(t *testing.T) {
	wantBody := `{"error": "rate_limited", "retry_after": 30}`
	env := newProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, wantBody)
	}))
	defer env.close()

	resp, err := http.Get(env.Proxy.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

// ---------------------------------------------------------------------------
// Audit Integration Tests
// ---------------------------------------------------------------------------

// auditProxyTestEnv extends proxyTestEnv with audit capture components.
type auditProxyTestEnv struct {
	proxyTestEnv
	Pool  *blockPool
	Queue chan *auditBlock
	Drops *mockDropRecorder
}

func (e *auditProxyTestEnv) close() {
	e.proxyTestEnv.close()
}

func newAuditProxyTestEnv(upstream http.Handler, poolBytes, blockSize int) *auditProxyTestEnv {
	us := httptest.NewServer(upstream)
	usURL, _ := url.Parse(us.URL)

	pool := newBlockPool(poolBytes, blockSize)
	queue := make(chan *auditBlock, 256)
	drops := &mockDropRecorder{}

	p := newProxy(proxyConfig{
		targetURL:  usURL,
		auditPool:  pool,
		auditQueue: queue,
		drops:      drops,
		nextReqID:  &atomic.Uint64{},
	})
	ps := httptest.NewServer(p)

	return &auditProxyTestEnv{
		proxyTestEnv: proxyTestEnv{Upstream: us, Proxy: ps, UpstreamURL: usURL},
		Pool:         pool,
		Queue:        queue,
		Drops:        drops,
	}
}

func TestAuditLargePayloadStreaming(t *testing.T) {
	const payloadSize = 5 * 1024 * 1024 // 5MB

	firstByteReceived := make(chan time.Time, 1)
	allBytesReceived := make(chan int64, 1)

	env := newAuditProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64*1024)
		var total int64
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				total += int64(n)
				if total == int64(n) {
					firstByteReceived <- time.Now()
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("upstream read error: %v", err)
				break
			}
		}
		allBytesReceived <- total
		w.WriteHeader(http.StatusOK)
	}), 10*1024*1024, 64*1024) // 10MB pool, 64KB blocks
	defer env.close()

	pr, pw := io.Pipe()
	uploadStart := time.Now()
	go func() {
		chunk := make([]byte, 64*1024)
		var written int64
		for written < payloadSize {
			n := int64(len(chunk))
			if written+n > payloadSize {
				n = payloadSize - written
			}
			pw.Write(chunk[:n])
			written += n
			if written < payloadSize {
				time.Sleep(1 * time.Millisecond)
			}
		}
		pw.Close()
	}()

	req, _ := http.NewRequest("POST", env.Proxy.URL+"/v1/chat/completions", pr)
	req.ContentLength = -1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	firstByte := <-firstByteReceived
	total := <-allBytesReceived

	// Upstream must start receiving bytes well before the entire upload completes.
	streamingDelay := firstByte.Sub(uploadStart)
	if streamingDelay > 5*time.Second {
		t.Errorf("upstream received first byte after %v, proxy may be buffering", streamingDelay)
	}
	if total != payloadSize {
		t.Errorf("upstream received %d bytes, want %d", total, payloadSize)
	}
	if env.Drops.count.Load() != 0 {
		t.Errorf("unexpected drops: %d", env.Drops.count.Load())
	}
}

func TestAuditPoolExhaustion(t *testing.T) {
	const blockSize = 64 * 1024
	const poolBytes = 2 * blockSize // only 2 blocks = 128KB
	const payloadSize = 1024 * 1024 // 1MB

	var upstreamTotal atomic.Int64

	env := newAuditProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		upstreamTotal.Store(int64(len(data)))
		w.WriteHeader(http.StatusOK)
	}), poolBytes, blockSize)
	defer env.close()

	payload := strings.Repeat("x", payloadSize)
	req, _ := http.NewRequest("POST", env.Proxy.URL+"/v1/chat/completions",
		strings.NewReader(payload))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Fail-open: upstream must receive the full payload.
	if got := upstreamTotal.Load(); got != payloadSize {
		t.Errorf("upstream received %d bytes, want %d", got, payloadSize)
	}

	// Drops must have occurred.
	if got := env.Drops.count.Load(); got == 0 {
		t.Error("expected drops > 0 due to pool exhaustion")
	}

	// Pool exhaustion counter must have incremented.
	if got := env.Pool.exhaustions.Load(); got == 0 {
		t.Error("expected pool exhaustions > 0")
	}
}

func TestAuditRequestIDGrouping(t *testing.T) {
	const blockSize = 256
	const poolBytes = 100 * blockSize

	env := newAuditProxyTestEnv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}), poolBytes, blockSize)
	defer env.close()

	// Send 3 concurrent requests with payloads larger than one block.
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := strings.Repeat("a", 3*blockSize+50)
			req, _ := http.NewRequest("POST", env.Proxy.URL+"/v1/chat/completions",
				strings.NewReader(payload))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	// Small delay to let queue drain in-flight blocks.
	time.Sleep(50 * time.Millisecond)

	// Collect all blocks from the queue.
	blocks := collectBlocks(env.Queue)

	// Group blocks by requestID.
	grouped := make(map[uint64][]*auditBlock)
	for _, blk := range blocks {
		grouped[blk.requestID] = append(grouped[blk.requestID], blk)
	}

	if len(grouped) != 3 {
		t.Errorf("expected 3 distinct requestIDs, got %d", len(grouped))
	}

	// For each request, verify sequential ordering and a terminal block.
	for reqID, reqBlocks := range grouped {
		// Sort by seq to verify ordering.
		slices.SortFunc(reqBlocks, func(a, b *auditBlock) int {
			return int(a.seq) - int(b.seq)
		})

		for i, blk := range reqBlocks {
			if blk.seq != uint32(i) {
				t.Errorf("requestID %d: block %d has seq=%d, want %d", reqID, i, blk.seq, i)
			}
		}

		// Last block must be final or abort.
		last := reqBlocks[len(reqBlocks)-1]
		if !last.final && !last.abort {
			t.Errorf("requestID %d: last block has neither final nor abort set", reqID)
		}
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func gcStats() runtime.MemStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m
}

func logGCDelta(b *testing.B, label string, before, after runtime.MemStats, requests int) {
	gcCycles := after.NumGC - before.NumGC
	pauseNs := after.PauseTotalNs - before.PauseTotalNs
	mallocs := after.Mallocs - before.Mallocs
	totalAlloc := after.TotalAlloc - before.TotalAlloc
	b.Logf("%s: GC cycles=%d, STW pause=%v, mallocs=%d (%.1f/req), alloc=%dMB",
		label, gcCycles,
		time.Duration(pauseNs),
		mallocs, float64(mallocs)/float64(requests),
		totalAlloc/(1024*1024))
}

// chatCompletionResponse is a realistic ~200B JSON response body.
var chatCompletionResponse = []byte(`{"id":"chatcmpl-abc123","object":"chat.completion","created":1711000000,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)

// largerResponseBody is a ~2KB response body simulating a larger completion.
var largerResponseBody = bytes.Repeat([]byte(`{"id":"chatcmpl-abc123","object":"chat.completion","choices":[]}`), 32)

func BenchmarkProxySequential(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(chatCompletionResponse)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)

	b.Run("direct", func(b *testing.B) {
		b.SetBytes(int64(len(chatCompletionResponse)))

		// Warmup
		resp, _ := http.Get(upstream.URL + "/v1/chat/completions")
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		before := gcStats()
		latencies := make([]time.Duration, b.N)

		b.ResetTimer()
		for i := range b.N {
			start := time.Now()
			resp, err := http.Get(upstream.URL + "/v1/chat/completions")
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			latencies[i] = time.Since(start)
		}
		b.StopTimer()

		after := gcStats()
		logGCDelta(b, "direct-sequential", before, after, b.N)

		slices.Sort(latencies)
		for _, p := range []float64{50, 95, 99} {
			b.Logf("p%.0f: %v (n=%d)", p, percentile(latencies, p), b.N)
		}
	})

	b.Run("proxied", func(b *testing.B) {
		proxy := newProxy(proxyConfig{targetURL: upstreamURL})
		proxyServer := httptest.NewServer(proxy)
		defer proxyServer.Close()

		b.SetBytes(int64(len(chatCompletionResponse)))

		// Warmup
		resp, _ := http.Get(proxyServer.URL + "/v1/chat/completions")
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		before := gcStats()
		latencies := make([]time.Duration, b.N)

		b.ResetTimer()
		for i := range b.N {
			start := time.Now()
			resp, err := http.Get(proxyServer.URL + "/v1/chat/completions")
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			latencies[i] = time.Since(start)
		}
		b.StopTimer()

		after := gcStats()
		logGCDelta(b, "proxied-sequential", before, after, b.N)

		slices.Sort(latencies)
		for _, p := range []float64{50, 95, 99} {
			b.Logf("p%.0f: %v (n=%d)", p, percentile(latencies, p), b.N)
		}
	})
}

func BenchmarkProxyConcurrent(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulated TTFB delay -- mimics AI provider thinking time
		time.Sleep(75 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write(largerResponseBody)
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)

	// Test both direct and proxied with b.RunParallel for a fair comparison.
	b.Run("direct", func(b *testing.B) {
		b.SetBytes(int64(len(largerResponseBody)))

		var mu sync.Mutex
		var allLatencies []time.Duration

		before := gcStats()

		b.RunParallel(func(pb *testing.PB) {
			local := make([]time.Duration, 0, b.N/runtime.GOMAXPROCS(0)+1)
			for pb.Next() {
				start := time.Now()
				resp, err := http.Get(upstream.URL + "/v1/chat/completions")
				if err != nil {
					b.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				local = append(local, time.Since(start))
			}
			mu.Lock()
			allLatencies = append(allLatencies, local...)
			mu.Unlock()
		})

		after := gcStats()
		logGCDelta(b, "direct-concurrent", before, after, len(allLatencies))

		slices.Sort(allLatencies)
		for _, p := range []float64{50, 95, 99} {
			b.Logf("p%.0f: %v (n=%d)", p, percentile(allLatencies, p), len(allLatencies))
		}
	})

	b.Run("proxied", func(b *testing.B) {
		proxy := newProxy(proxyConfig{targetURL: upstreamURL})
		proxyServer := httptest.NewServer(proxy)
		defer proxyServer.Close()

		b.SetBytes(int64(len(largerResponseBody)))

		var mu sync.Mutex
		var allLatencies []time.Duration

		before := gcStats()

		b.RunParallel(func(pb *testing.PB) {
			local := make([]time.Duration, 0, b.N/runtime.GOMAXPROCS(0)+1)
			for pb.Next() {
				start := time.Now()
				resp, err := http.Get(proxyServer.URL + "/v1/chat/completions")
				if err != nil {
					b.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				local = append(local, time.Since(start))
			}
			mu.Lock()
			allLatencies = append(allLatencies, local...)
			mu.Unlock()
		})

		after := gcStats()
		logGCDelta(b, "proxied-concurrent", before, after, len(allLatencies))

		slices.Sort(allLatencies)
		for _, p := range []float64{50, 95, 99} {
			b.Logf("p%.0f: %v (n=%d)", p, percentile(allLatencies, p), len(allLatencies))
		}
	})
}

func BenchmarkProxyStreaming(b *testing.B) {
	const (
		numChunks     = 50
		chunkInterval = 5 * time.Millisecond
		initialDelay  = 75 * time.Millisecond
	)

	// Build a realistic SSE chunk
	sseChunk := []byte("data: {\"id\":\"chatcmpl-abc\",\"choices\":[{\"delta\":{\"content\":\"tok\"}}]}\n\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		time.Sleep(initialDelay)

		for range numChunks {
			w.Write(sseChunk)
			flusher.Flush()
			time.Sleep(chunkInterval)
		}
	}))
	defer upstream.Close()

	upstreamURL, _ := url.Parse(upstream.URL)

	// SSE scanner split function: split on \n\n boundary
	sseSplit := func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
			return i + 2, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	}

	// Measure inter-chunk latencies for a single SSE stream
	measureStream := func(b *testing.B, targetURL string) []time.Duration {
		resp, err := http.Get(targetURL + "/v1/chat/completions")
		if err != nil {
			b.Error(err)
			return nil
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Split(sseSplit)

		var deltas []time.Duration
		lastChunk := time.Now()
		for scanner.Scan() {
			now := time.Now()
			deltas = append(deltas, now.Sub(lastChunk))
			lastChunk = now
		}
		return deltas
	}

	// Run for both direct and proxied
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"direct", upstream.URL},
		{"proxied", ""},
	} {
		b.Run(tc.name, func(b *testing.B) {
			targetURL := tc.url
			if tc.name == "proxied" {
				proxy := newProxy(proxyConfig{targetURL: upstreamURL})
				proxyServer := httptest.NewServer(proxy)
				defer proxyServer.Close()
				targetURL = proxyServer.URL
			}

			before := gcStats()

			var mu sync.Mutex
			var allDeltas []time.Duration
			var wg sync.WaitGroup

			for range b.N {
				wg.Add(1)
				go func() {
					defer wg.Done()
					deltas := measureStream(b, targetURL)
					mu.Lock()
					allDeltas = append(allDeltas, deltas...)
					mu.Unlock()
				}()
			}
			wg.Wait()

			after := gcStats()
			logGCDelta(b, tc.name+"-streaming", before, after, b.N)

			slices.Sort(allDeltas)
			for _, p := range []float64{50, 95, 99} {
				b.Logf("inter-chunk p%.0f: %v (n=%d)", p, percentile(allDeltas, p), len(allDeltas))
			}
		})
	}
}

func BenchmarkAuditGCImpact(b *testing.B) {
	const payloadSize = 10 * 1024 // 10KB per request

	requestBody := bytes.Repeat([]byte("x"), payloadSize)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(chatCompletionResponse)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	b.Run("audit-disabled", func(b *testing.B) {
		proxy := newProxy(proxyConfig{targetURL: upstreamURL})
		proxyServer := httptest.NewServer(proxy)
		defer proxyServer.Close()

		b.SetBytes(payloadSize)
		before := gcStats()

		for range b.N {
			req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/chat/completions",
				bytes.NewReader(requestBody))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		after := gcStats()
		logGCDelta(b, "audit-disabled", before, after, b.N)
	})

	b.Run("audit-enabled", func(b *testing.B) {
		pool := newBlockPool(10*1024*1024, 64*1024) // 10MB pool
		queue := make(chan *auditBlock, 256)
		go func() {
			for blk := range queue {
				pool.put(blk)
			}
		}()
		defer close(queue)

		proxy := newProxy(proxyConfig{
			targetURL:  upstreamURL,
			auditPool:  pool,
			auditQueue: queue,
			drops:      newDropCounter(),
			nextReqID:  &atomic.Uint64{},
		})
		proxyServer := httptest.NewServer(proxy)
		defer proxyServer.Close()

		b.SetBytes(payloadSize)
		before := gcStats()

		for range b.N {
			req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/chat/completions",
				bytes.NewReader(requestBody))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		after := gcStats()
		logGCDelta(b, "audit-enabled", before, after, b.N)
	})
}
