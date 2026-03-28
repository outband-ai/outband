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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------------------------------------------------------------------------
// In-process mock upstream handlers
// ---------------------------------------------------------------------------

// syncBodyLog is a thread-safe log of request bodies received by the mock.
type syncBodyLog struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (s *syncBodyLog) append(b []byte) {
	s.mu.Lock()
	s.bodies = append(s.bodies, b)
	s.mu.Unlock()
}

func (s *syncBodyLog) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// defaultMockHandler returns an http.Handler that accepts POST requests,
// logs received bodies, and returns an instant OpenAI-format JSON response.
func defaultMockHandler(bodyLog *syncBodyLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Validate method and path — catch proxy rewrite regressions.
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}

		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodyLog.append(body)

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "mock-1",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": "Mock response.",
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{
				"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
}

// slowStreamingMockHandler returns an http.Handler that responds with an SSE
// stream, emitting chunks with configurable delay. Used by the graceful
// shutdown test to create an active streaming connection.
func slowStreamingMockHandler(bodyLog *syncBodyLog, chunkDelay time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodyLog.append(body)

		// Check if client requested streaming.
		var req struct {
			Stream bool `json:"stream"`
		}
		if len(body) > 0 {
			json.Unmarshal(body, &req)
		}

		if !req.Stream {
			// Non-streaming: instant JSON response.
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   "mock-1",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "Mock response.",
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]int{
					"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Streaming: SSE with configurable delay.
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		tokens := []string{"Hello", " from", " streaming", " mock", "."}
		for i, token := range tokens {
			chunk := map[string]any{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   "mock-1",
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]string{"content": token},
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if i < len(tokens)-1 {
				time.Sleep(chunkDelay)
			}
		}

		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
}

// ---------------------------------------------------------------------------
// HTTP clients for tests
// ---------------------------------------------------------------------------

// defaultTestClient is used for sequential request tests.
var defaultTestClient = &http.Client{Timeout: 10 * time.Second}

// concurrentTestClient bypasses connection pooling to force true parallel
// TCP connections. Without this, Go's default MaxConnsPerHost serializes
// requests through a limited connection pool.
var concurrentTestClient = &http.Client{
	Transport: &http.Transport{
		DisableKeepAlives: true,
		MaxConnsPerHost:   0,
	},
	Timeout: 5 * time.Second,
}

// ---------------------------------------------------------------------------
// Integration test environment
// ---------------------------------------------------------------------------

type integrationConfig struct {
	poolCapacity    int
	blockSize       int
	queueSize       int
	numWorkers      int
	batchSize       int
	flushInterval   time.Duration
	summaryInterval time.Duration
	logMaxSize      int64
	shutdownDrain   time.Duration
	upstreamHandler http.Handler
}

type integrationOption func(*integrationConfig)

func withPoolCapacity(n int) integrationOption    { return func(c *integrationConfig) { c.poolCapacity = n } }
func withBlockSize(n int) integrationOption       { return func(c *integrationConfig) { c.blockSize = n } }
func withQueueSize(n int) integrationOption       { return func(c *integrationConfig) { c.queueSize = n } }
func withNumWorkers(n int) integrationOption      { return func(c *integrationConfig) { c.numWorkers = n } }
func withBatchSize(n int) integrationOption       { return func(c *integrationConfig) { c.batchSize = n } }
func withFlushInterval(d time.Duration) integrationOption {
	return func(c *integrationConfig) { c.flushInterval = d }
}
func withSummaryInterval(d time.Duration) integrationOption {
	return func(c *integrationConfig) { c.summaryInterval = d }
}
func withLogMaxSize(n int64) integrationOption {
	return func(c *integrationConfig) { c.logMaxSize = n }
}
func withShutdownDrain(d time.Duration) integrationOption {
	return func(c *integrationConfig) { c.shutdownDrain = d }
}
func withUpstreamHandler(h http.Handler) integrationOption {
	return func(c *integrationConfig) { c.upstreamHandler = h }
}

type integrationEnv struct {
	upstream      *httptest.Server
	metricsServer *httptest.Server
	server        *http.Server
	proxyURL      string
	metricsURL    string

	auditQueue chan *AuditBlock
	drops      *DropCounter
	aggregator *Aggregator
	jsonlWriter *JSONLWriter
	ioErrors   *atomic.Uint64
	metrics    *outbandMetrics
	ready      *atomic.Bool

	receivedBodies *syncBodyLog

	waitPipeline func()
	tickerCancel context.CancelFunc
	tickerDone   chan struct{}

	logDir      string
	evidenceDir string
	jsonlPath   string

	shutdownOnce       sync.Once
	lastDropsReported  *int64
	lastIOErrsReported *uint64
	cfg                integrationConfig
}

func newIntegrationEnv(t *testing.T, opts ...integrationOption) *integrationEnv {
	t.Helper()

	cfg := integrationConfig{
		poolCapacity:    5 * 1024 * 1024,
		blockSize:       64 * 1024,
		queueSize:       256,
		numWorkers:      2,
		batchSize:       10,
		flushInterval:   50 * time.Millisecond,
		summaryInterval: 1 * time.Hour,
		logMaxSize:      100 * 1024 * 1024,
		shutdownDrain:   10 * time.Second,
	}
	for _, o := range opts {
		o(&cfg)
	}

	bodyLog := &syncBodyLog{}

	if cfg.upstreamHandler == nil {
		cfg.upstreamHandler = defaultMockHandler(bodyLog)
	}

	// Step 1: Temp directories.
	logDir := t.TempDir()
	evidenceDir := t.TempDir()

	// Step 2: Start mock upstream — OS assigns random port.
	upstream := httptest.NewServer(cfg.upstreamHandler)

	// Step 3: Extract actual URL.
	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	// Step 4: Create pipeline components.
	pool := NewBlockPool(cfg.poolCapacity, cfg.blockSize)
	auditQueue := make(chan *AuditBlock, cfg.queueSize)
	drops := NewDropCounter()
	nextReqID := &atomic.Uint64{}

	// Step 5: Isolated Prometheus registry.
	promRegistry := prometheus.NewRegistry()
	metrics := newMetrics(promRegistry, drops)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{}))
	metricsServer := httptest.NewServer(metricsMux)

	// Step 6: Aggregator, JSONL writer, redactor chain.
	chain := &RedactorChain{redactors: []Redactor{NewRegexRedactor()}}
	aggregator := NewAggregator(chain.Name())

	jw, err := NewJSONLWriter(logDir, cfg.logMaxSize, 1*time.Hour, 24)
	if err != nil {
		t.Fatalf("create JSONL writer: %v", err)
	}

	var ioErrors atomic.Uint64

	// Step 7: Wire pipeline goroutines.
	workerInput := make(chan *assembledPayload, 64)
	resultsChannel := make(chan *TelemetryLog, 128)

	var asmStats assemblerStats
	assemblerDone := make(chan struct{})
	go func() {
		defer close(assemblerDone)
		startAssembler(auditQueue, assemblerConfig{
			maxPayloadSize: defaultMaxPayloadSize,
			staleTimeout:   defaultStaleTimeout,
			workerInput:    workerInput,
			pool:           pool,
			apiType:        apiTypeOpenAI,
		}, &asmStats)
	}()

	var wkStats workerStats
	waitWorkers := startWorkers(cfg.numWorkers, workerInput, resultsChannel, chain, &wkStats)

	var cStats collectorStats
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		startCollector(collectorConfig{
			batchSize:     cfg.batchSize,
			flushInterval: cfg.flushInterval,
			input:         resultsChannel,
			flush: func(batch []*TelemetryLog) {
				written, err := jw.Flush(batch)
				if err != nil {
					ioErrors.Add(1)
					metrics.flushErrors.Inc()
				}
				persisted := batch[:written]
				aggregator.Ingest(persisted)
				metrics.requestsAudited.Add(float64(len(persisted)))
				for _, entry := range persisted {
					for _, cat := range entry.PIICategoriesFound {
						metrics.piiDetected.WithLabelValues(cat).Inc()
					}
				}
			},
		}, &cStats)
	}()

	waitPipeline := func() {
		<-assemblerDone
		waitWorkers()
		close(resultsChannel)
		<-collectorDone
	}

	// Step 8: Evidence ticker.
	// Offsets are shared between the ticker goroutine and gracefulShutdown
	// via lastDropsReported/lastIOErrsReported to prevent double-counting.
	var lastDropsReported int64
	var lastIOErrsReported uint64
	localJSON := NewLocalJSONTarget(evidenceDir)

	tickerCtx, tickerCancel := context.WithCancel(context.Background())
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		tick := time.NewTicker(cfg.summaryInterval)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if tickerCtx.Err() != nil {
					return
				}
				currentDrops := drops.Total.Load()
				currentIOErrs := ioErrors.Load()
				windowDrops := uint64(currentDrops - lastDropsReported)
				windowIOErrs := currentIOErrs - lastIOErrsReported
				lastDropsReported = currentDrops
				lastIOErrsReported = currentIOErrs
				summary := aggregator.SnapshotAndReset(windowIOErrs, windowDrops, 0, false, Version)
				if err := localJSON.Push(tickerCtx, summary); err != nil {
					log.Printf("evidence dispatch error: %v", err)
				}
				metrics.evidenceReports.Inc()
			case <-tickerCtx.Done():
				return
			}
		}
	}()

	// Step 9: Build proxy config.
	var ready atomic.Bool
	proxyCfg := ProxyConfig{
		TargetURL:  targetURL,
		AuditPool:  pool,
		AuditQueue: auditQueue,
		Drops:      drops,
		NextReqID:  nextReqID,
		RecordOverhead: func(d time.Duration) {
			aggregator.RecordLatency(d)
			metrics.proxyOverhead.Observe(d.Seconds())
			metrics.requestsTotal.Inc()
		},
	}

	// Step 10–11: Create proxy handler.
	proxy := newProxy(&proxyCfg)
	handler := newLatencyMiddleware(newHealthHandler(proxy, targetURL, &ready))

	// Step 12: Start proxy on random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			// Test may have already finished; don't panic.
		}
	}()

	ready.Store(true)

	env := &integrationEnv{
		upstream:           upstream,
		metricsServer:      metricsServer,
		server:             srv,
		proxyURL:           "http://" + ln.Addr().String(),
		metricsURL:         metricsServer.URL + "/metrics",
		auditQueue:         auditQueue,
		drops:              drops,
		aggregator:         aggregator,
		jsonlWriter:        jw,
		ioErrors:           &ioErrors,
		metrics:            metrics,
		ready:              &ready,
		receivedBodies:     bodyLog,
		waitPipeline:       waitPipeline,
		tickerCancel:       tickerCancel,
		tickerDone:         tickerDone,
		logDir:             logDir,
		evidenceDir:        evidenceDir,
		jsonlPath:          filepath.Join(logDir, activeFileName),
		lastDropsReported:  &lastDropsReported,
		lastIOErrsReported: &lastIOErrsReported,
		cfg:                cfg,
	}

	t.Cleanup(func() { env.close() })
	return env
}

// close tears down the environment without generating a final evidence report.
func (e *integrationEnv) close() {
	e.shutdownOnce.Do(func() {
		e.ready.Store(false)
		e.server.Close()
		close(e.auditQueue)

		done := make(chan struct{})
		go func() {
			e.waitPipeline()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}

		e.tickerCancel()
		select {
		case <-e.tickerDone:
		case <-time.After(2 * time.Second):
		}

		e.jsonlWriter.Close()
		e.metricsServer.Close()
		e.upstream.Close()
	})
}

// gracefulShutdown mirrors main()'s shutdown sequence: graceful HTTP drain,
// pipeline flush, and final partial evidence report.
func (e *integrationEnv) gracefulShutdown(drainBudget time.Duration) {
	e.shutdownOnce.Do(func() {
		e.ready.Store(false)

		// Graceful drain — keeps active connections alive until they
		// complete or the budget expires.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), drainBudget)
		defer drainCancel()
		if err := e.server.Shutdown(drainCtx); err != nil {
			e.server.Close()
		}

		// Safe to close: Shutdown guarantees no active handlers.
		close(e.auditQueue)

		// Flush pipeline.
		done := make(chan struct{})
		go func() {
			e.waitPipeline()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}

		// Stop ticker.
		e.tickerCancel()
		select {
		case <-e.tickerDone:
		case <-time.After(2 * time.Second):
		}

		// Final partial evidence report.
		finalDrops := uint64(e.drops.Total.Load() - *e.lastDropsReported)
		finalIOErrs := e.ioErrors.Load() - *e.lastIOErrsReported
		summary := e.aggregator.SnapshotAndReset(finalIOErrs, finalDrops, 0, true, Version)
		localJSON := NewLocalJSONTarget(e.evidenceDir)
		if err := localJSON.Push(context.Background(), summary); err != nil {
			log.Printf("final evidence write error: %v", err)
		}

		e.jsonlWriter.Close()
		e.metricsServer.Close()
		e.upstream.Close()
	})
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// sendIntegrationRequest sends an OpenAI-format chat completion request
// through the proxy and returns the response. Caller must close the body.
func sendIntegrationRequest(t *testing.T, client *http.Client, proxyURL, content string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"model":"gpt-4","messages":[{"role":"user","content":%q}]}`, content)
	req, err := http.NewRequest("POST", proxyURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	return resp
}

// sendAndDiscard sends a request, asserts a 2xx response, and discards the body.
func sendAndDiscard(t *testing.T, client *http.Client, proxyURL, content string) {
	t.Helper()
	resp := sendIntegrationRequest(t, client, proxyURL, content)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("unexpected status %d from %s: %s", resp.StatusCode, proxyURL, string(tailBytes(body, 200)))
	}
}

// countAllJSONLEntries returns all .jsonl files in dir and their total parseable entry count.
func countAllJSONLEntries(dir string) (files []string, totalEntries int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		files = append(files, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var entry TelemetryLog
			if json.Unmarshal(line, &entry) == nil {
				totalEntries++
			}
		}
	}
	return files, totalEntries
}

// ---------------------------------------------------------------------------
// Integration Tests
// ---------------------------------------------------------------------------

func TestIntegrationCleanTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	env := newIntegrationEnv(t)

	prompts := []string{
		"Explain how photosynthesis works",
		"What is the capital of France",
		"Describe the water cycle",
		"How do computers work",
		"What is gravity",
	}
	for _, p := range prompts {
		sendAndDiscard(t, defaultTestClient, env.proxyURL, p)
	}

	entries, err := waitForJSONLCount(env.jsonlPath, 5, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	for i, entry := range entries {
		if !entry.CaptureComplete {
			t.Errorf("entry %d: CaptureComplete = false", i)
		}
		if len(entry.PIICategoriesFound) != 0 {
			t.Errorf("entry %d: PIICategoriesFound = %v, want empty", i, entry.PIICategoriesFound)
		}
		if entry.RedactionLevel != "pattern-based" {
			t.Errorf("entry %d: RedactionLevel = %q, want %q", i, entry.RedactionLevel, "pattern-based")
		}
		if len(entry.OriginalHash) != 64 {
			t.Errorf("entry %d: OriginalHash length = %d, want 64", i, len(entry.OriginalHash))
		}
		if len(entry.RedactedHash) != 64 {
			t.Errorf("entry %d: RedactedHash length = %d, want 64", i, len(entry.RedactedHash))
		}
		if strings.Contains(entry.RedactedPayload, "[REDACTED") {
			t.Errorf("entry %d: unexpected redaction in clean payload: %s", i, entry.RedactedPayload)
		}
	}
}

func TestIntegrationPIITraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	env := newIntegrationEnv(t)

	type piiTest struct {
		content    string
		wantCats   []string
		wantTags   []string
		absentText []string
	}

	cases := []piiTest{
		{
			content:    "My SSN is 123-45-6789",
			wantCats:   []string{"SSN"},
			wantTags:   []string{"[REDACTED:SSN:CC6.1]"},
			absentText: []string{"123-45-6789"},
		},
		{
			content:    "Card number 4532015112830366",
			wantCats:   []string{"CC_NUMBER"},
			wantTags:   []string{"[REDACTED:CC_NUMBER:CC6.1]"},
			absentText: []string{"4532015112830366"},
		},
		{
			content:    "Email john.smith@acmecorp.com",
			wantCats:   []string{"EMAIL"},
			wantTags:   []string{"[REDACTED:EMAIL:CC6.1]"},
			absentText: []string{"john.smith@acmecorp.com"},
		},
		{
			content:    "Call me at (555) 867-5309",
			wantCats:   []string{"PHONE"},
			wantTags:   []string{"[REDACTED:PHONE:CC6.1]"},
			absentText: []string{"867-5309"},
		},
		{
			content:    "SSN 234-56-7890 email admin@corp.com phone (212) 555-1234",
			wantCats:   []string{"SSN", "EMAIL", "PHONE"},
			wantTags:   []string{"[REDACTED:SSN:CC6.1]", "[REDACTED:EMAIL:CC6.1]", "[REDACTED:PHONE:CC6.1]"},
			absentText: []string{"234-56-7890", "admin@corp.com", "555-1234"},
		},
	}

	for _, tc := range cases {
		sendAndDiscard(t, defaultTestClient, env.proxyURL, tc.content)
	}

	entries, err := waitForJSONLCount(env.jsonlPath, 5, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Match entries by expected redaction tags rather than positional index.
	// Workers process concurrently so JSONL entry order is nondeterministic.
	used := make([]bool, len(entries))
	for i, tc := range cases {
		found := false
		for j, entry := range entries {
			if used[j] {
				continue
			}
			// Match: entry must contain all expected redaction tags.
			match := true
			for _, tag := range tc.wantTags {
				if !strings.Contains(entry.RedactedPayload, tag) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			used[j] = true
			found = true

			catSet := make(map[string]bool)
			for _, c := range entry.PIICategoriesFound {
				catSet[c] = true
			}
			for _, want := range tc.wantCats {
				if !catSet[want] {
					t.Errorf("case %d: missing category %q in %v", i, want, entry.PIICategoriesFound)
				}
			}
			for _, absent := range tc.absentText {
				if strings.Contains(entry.RedactedPayload, absent) {
					t.Errorf("case %d: PII %q not redacted in payload", i, absent)
				}
			}
			break
		}
		if !found {
			t.Errorf("case %d: no matching entry found for tags %v", i, tc.wantTags)
		}
	}

	// Verify upstream received all requests with PII intact.
	if got := env.receivedBodies.count(); got != 5 {
		t.Errorf("upstream received %d requests, want 5", got)
	}
}

func TestIntegrationFalsePositiveResistance(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	env := newIntegrationEnv(t)

	// Workspace ID with 16-digit Luhn-valid number in structural field.
	body1 := `{"model":"gpt-4","workspace_id":"4502123456789012","messages":[{"role":"user","content":"No PII in this message"}]}`
	req1, _ := http.NewRequest("POST", env.proxyURL+"/v1/chat/completions", strings.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := defaultTestClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()

	// 9-digit number that is not in SSN format.
	sendAndDiscard(t, defaultTestClient, env.proxyURL, "the order number is 123456789")

	entries, err := waitForJSONLCount(env.jsonlPath, 2, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	for i, entry := range entries {
		if len(entry.PIICategoriesFound) != 0 {
			t.Errorf("entry %d: false positive — categories = %v", i, entry.PIICategoriesFound)
		}
		if strings.Contains(entry.RedactedPayload, "[REDACTED") {
			t.Errorf("entry %d: false positive — payload contains redaction tag", i)
		}
	}
}

func TestIntegrationBlockBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	// Use small blocks to force the SSN to span a block boundary.
	env := newIntegrationEnv(t, withBlockSize(256))

	// Build a content string with ~1KB of filler so SSN spans blocks.
	filler := strings.Repeat("The quick brown fox jumps. ", 40)
	content := filler + "SSN: 987-65-4321 end"
	sendAndDiscard(t, defaultTestClient, env.proxyURL, content)

	entries, err := waitForJSONLCount(env.jsonlPath, 1, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	entry := entries[0]
	catSet := make(map[string]bool)
	for _, c := range entry.PIICategoriesFound {
		catSet[c] = true
	}
	if !catSet["SSN"] {
		t.Errorf("SSN not detected after block boundary reassembly: categories = %v", entry.PIICategoriesFound)
	}
	if !strings.Contains(entry.RedactedPayload, "[REDACTED:SSN:CC6.1]") {
		t.Errorf("SSN redaction tag missing from payload")
	}
}

func TestIntegrationDropCounter(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	// Choke the pipeline: tiny pool (2 blocks), queue size 1, 1 worker.
	env := newIntegrationEnv(t,
		withPoolCapacity(2*64*1024),
		withQueueSize(1),
		withNumWorkers(1),
	)

	const totalRequests = 50
	// Content larger than one block (>64KB) to exhaust pool.
	bigContent := strings.Repeat("x", 70*1024)

	var wg sync.WaitGroup
	var successCount atomic.Int64
	var sendErrors atomic.Int64
	for i := 0; i < totalRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"gpt-4","messages":[{"role":"user","content":%q}]}`, bigContent)
			req, err := http.NewRequest("POST", env.proxyURL+"/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				sendErrors.Add(1)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := concurrentTestClient.Do(req)
			if err != nil {
				sendErrors.Add(1)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				successCount.Add(1)
			}
		}()
	}

	// wg.Wait() is sufficient: drops are recorded synchronously in the
	// TeeReader hot path before the HTTP response is delivered.
	wg.Wait()

	if errs := sendErrors.Load(); errs > 0 {
		t.Fatalf("%d requests failed to send", errs)
	}

	// All requests must be forwarded (fail-open).
	if got := successCount.Load(); got != totalRequests {
		t.Errorf("successful responses = %d, want %d (fail-open)", got, totalRequests)
	}
	if got := env.receivedBodies.count(); got != totalRequests {
		t.Errorf("upstream received %d requests, want %d", got, totalRequests)
	}

	// Poll until pipeline settles: JSONL entries + drops == total.
	err := waitForCondition(10*time.Second, 100*time.Millisecond, func() bool {
		_, jsonlCount := countAllJSONLEntries(env.logDir)
		drops := int(env.drops.Total.Load())
		return jsonlCount+drops >= totalRequests
	})
	if err != nil {
		_, jsonlCount := countAllJSONLEntries(env.logDir)
		drops := env.drops.Total.Load()
		t.Fatalf("pipeline did not settle: jsonl=%d drops=%d total=%d want=%d",
			jsonlCount, drops, jsonlCount+int(drops), totalRequests)
	}

	// Drops must have occurred.
	if got := env.drops.Total.Load(); got == 0 {
		t.Error("expected drops > 0 due to buffer pressure")
	}
}

func TestIntegrationEvidenceSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	env := newIntegrationEnv(t, withSummaryInterval(1*time.Second))

	// 5 clean + 5 with SSN PII.
	for i := 0; i < 5; i++ {
		sendAndDiscard(t, defaultTestClient, env.proxyURL, "clean request no PII")
	}
	for i := 0; i < 5; i++ {
		sendAndDiscard(t, defaultTestClient, env.proxyURL, fmt.Sprintf("SSN: %d23-45-6789", i))
	}

	summary, err := waitForEvidenceFile(env.evidenceDir, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if summary.TotalRequestsProcessed < 10 {
		t.Errorf("TotalRequestsProcessed = %d, want >= 10", summary.TotalRequestsProcessed)
	}
	if summary.TotalRequestsAudited == 0 {
		t.Error("TotalRequestsAudited = 0")
	}
	if summary.AuditCoveragePercent <= 0 {
		t.Errorf("AuditCoveragePercent = %f, want > 0", summary.AuditCoveragePercent)
	}
	if summary.RedactionLevel != "pattern-based" {
		t.Errorf("RedactionLevel = %q", summary.RedactionLevel)
	}
	if summary.SchemaVersion != "1.0.0" {
		t.Errorf("SchemaVersion = %q", summary.SchemaVersion)
	}
	if summary.PartialWindow {
		t.Error("PartialWindow = true, want false")
	}
	if !summary.WindowStart.Before(summary.WindowEnd) {
		t.Errorf("WindowStart (%v) not before WindowEnd (%v)", summary.WindowStart, summary.WindowEnd)
	}

	// Check SOC 2 controls.
	controlSet := make(map[string]bool)
	for _, c := range summary.SOC2ControlsSatisfied {
		controlSet[c] = true
	}
	for _, want := range []string{"CC6.1", "CC6.6", "CC9.2"} {
		if !controlSet[want] {
			t.Errorf("missing SOC 2 control %q in %v", want, summary.SOC2ControlsSatisfied)
		}
	}

	// SSN detections.
	if summary.RedactionEventsByCategory["SSN"] == 0 {
		t.Error("RedactionEventsByCategory[SSN] = 0, want > 0")
	}
}

func TestIntegrationJSONLRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	// 2KB rotation threshold — each entry is ~200-500 bytes, so rotation
	// fires every ~4-10 entries. 20 requests guarantees multiple rotations.
	env := newIntegrationEnv(t, withLogMaxSize(2048))

	for i := 0; i < 20; i++ {
		sendAndDiscard(t, defaultTestClient, env.proxyURL, fmt.Sprintf("request %d with some content padding text here", i))
	}

	// Wait for all entries to be written across all files AND rotation to occur.
	err := waitForCondition(10*time.Second, 100*time.Millisecond, func() bool {
		files, entries := countAllJSONLEntries(env.logDir)
		return len(files) >= 2 && entries >= 20
	})
	if err != nil {
		files, count := countAllJSONLEntries(env.logDir)
		t.Fatalf("rotation/entry count not met: files=%v entries=%d", files, count)
	}

	// Verify all files parse cleanly and no entries were lost.
	files, totalEntries := countAllJSONLEntries(env.logDir)
	if len(files) < 2 {
		t.Fatalf("expected at least 2 JSONL files, got %d", len(files))
	}
	if totalEntries < 20 {
		t.Errorf("total entries across all files = %d, want >= 20", totalEntries)
	}

	// Verify at least one rotated file exists (name != active).
	hasRotated := false
	for _, f := range files {
		if f != activeFileName {
			hasRotated = true
		}
	}
	if !hasRotated {
		t.Errorf("no rotated files found in %v", files)
	}
}

func TestIntegrationGracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	bodyLog := &syncBodyLog{}

	env := newIntegrationEnv(t,
		withSummaryInterval(1*time.Hour),
		withShutdownDrain(5*time.Second),
		withUpstreamHandler(slowStreamingMockHandler(bodyLog, 200*time.Millisecond)),
	)

	// Send 3 non-streaming requests with PII to confirm pipeline works.
	for i := 0; i < 3; i++ {
		sendAndDiscard(t, defaultTestClient, env.proxyURL,
			fmt.Sprintf("SSN: %d56-78-9012", i))
	}

	_, err := waitForJSONLCount(env.jsonlPath, 3, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Launch a streaming request that will be in-flight during shutdown.
	type streamResult struct {
		body []byte
		err  error
		code int
	}
	resultCh := make(chan streamResult, 1)
	go func() {
		reqBody := `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"stream test"}]}`
		req, _ := http.NewRequest("POST", env.proxyURL+"/v1/chat/completions", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		resp, err := defaultTestClient.Do(req)
		if err != nil {
			resultCh <- streamResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		resultCh <- streamResult{body: body, err: readErr, code: resp.StatusCode}
	}()

	// Wait for the proxy to establish the connection.
	time.Sleep(100 * time.Millisecond)

	// Trigger graceful shutdown while the stream is active.
	env.gracefulShutdown(5 * time.Second)

	// Wait for the streaming response to complete.
	select {
	case result := <-resultCh:
		// Strict body assertions — premature EOF guard.
		if result.err != nil {
			t.Fatalf("streaming response read error (connection severed?): %v", result.err)
		}
		if result.code != http.StatusOK {
			t.Errorf("streaming response status = %d, want 200", result.code)
		}
		if !bytes.HasSuffix(bytes.TrimSpace(result.body), []byte("data: [DONE]")) {
			t.Errorf("streaming response does not end with data: [DONE]; tail: %q",
				tailBytes(result.body, 100))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("streaming response did not complete within 10s")
	}

	// Verify partial evidence summary was generated.
	summary, err := waitForEvidenceFile(env.evidenceDir, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.PartialWindow {
		t.Error("PartialWindow = false, want true")
	}
	if summary.TotalRequestsAudited < 3 {
		t.Errorf("TotalRequestsAudited = %d, want >= 3", summary.TotalRequestsAudited)
	}

	// Verify JSONL file is cleanly terminated.
	data, err := os.ReadFile(env.jsonlPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read JSONL: %v", err)
	}
	if len(data) > 0 {
		lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
		lastLine := lines[len(lines)-1]
		var entry TelemetryLog
		if err := json.Unmarshal(lastLine, &entry); err != nil {
			t.Errorf("last JSONL line is not valid JSON: %v", err)
		}
	}
}

func TestIntegrationPrometheusMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}

	env := newIntegrationEnv(t)

	// 5 clean requests.
	for i := 0; i < 5; i++ {
		sendAndDiscard(t, defaultTestClient, env.proxyURL, "clean request")
	}
	// 5 requests with SSN.
	for i := 0; i < 5; i++ {
		sendAndDiscard(t, defaultTestClient, env.proxyURL, fmt.Sprintf("SSN %d34-56-7890", i))
	}

	// Wait for JSONL to confirm pipeline processed everything.
	_, err := waitForJSONLCount(env.jsonlPath, 10, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Poll Prometheus metrics.
	if err := waitForPrometheusMetric(env.metricsURL, "outband_requests_total", 10, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForPrometheusMetric(env.metricsURL, "outband_requests_audited_total", 10, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForPrometheusMetric(env.metricsURL, `outband_pii_detected_total{category="SSN"}`, 5, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	// Drops should be zero.
	resp, err := http.Get(env.metricsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	dropsVal := parseMetricValue(string(body), "outband_requests_dropped_total")
	if dropsVal > 0 {
		t.Errorf("outband_requests_dropped_total = %g, want 0", dropsVal)
	}

	// Verify overhead histogram exists.
	if !strings.Contains(string(body), "outband_proxy_overhead_seconds") {
		t.Error("outband_proxy_overhead_seconds histogram not found in /metrics output")
	}
}

// tailBytes returns the last n bytes of b, or all of b if len(b) <= n.
func tailBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
