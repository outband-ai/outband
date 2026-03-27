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
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type bufferPool struct {
	pool sync.Pool
}

func (b *bufferPool) Get() []byte {
	if v := b.pool.Get(); v != nil {
		return v.([]byte)
	}
	return make([]byte, 32*1024)
}

func (b *bufferPool) Put(buf []byte) {
	b.pool.Put(buf)
}

type proxyConfig struct {
	targetURL      *url.URL
	auditPool      blockAllocator        // nil = audit disabled
	auditQueue     chan<- *auditBlock     // global audit queue
	drops          dropRecorder          // nil = no drop tracking
	nextReqID      *atomic.Uint64        // monotonic request ID generator
	recordOverhead func(time.Duration)   // nil = latency recording disabled
}

func newProxy(cfg proxyConfig) *httputil.ReverseProxy {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(cfg.targetURL)
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			// Go's Request.write() injects "User-Agent: Go-http-client/1.1"
			// whenever the header is absent. To preserve transparency when
			// the client sent no User-Agent, set an explicitly empty value
			// which suppresses the default without sending a header on the wire.
			if _, ok := r.In.Header["User-Agent"]; !ok {
				r.Out.Header["User-Agent"] = []string{""}
			}
			if r.Out.Body != nil && cfg.auditPool != nil {
				reqID := cfg.nextReqID.Add(1)
				r.Out.Body = newAuditReader(r.Out.Body, cfg.auditPool, cfg.drops, cfg.auditQueue, reqID)
			}
			// Record proxy ingress overhead: time from request arrival to
			// upstream dial initiation. This is the last code before
			// http.Transport dials. Does NOT measure SSE stream duration.
			if cfg.recordOverhead != nil {
				if start, ok := requestStart(r.In.Context()); ok {
					cfg.recordOverhead(time.Since(start))
				}
			}
		},
		Transport:  transport,
		BufferPool: &bufferPool{},
	}
}

// version is set at build time via ldflags:
//
//	go build -ldflags "-X main.version=v1.2.3" -o outband .
var version = "dev"

// validateFlags checks all numeric and duration flags for invalid values.
// Returns a slice of human-readable error strings (empty if valid).
func validateFlags(
	auditCapacity, auditBlockSize, auditQueueSize int,
	maxPayloadSize, workerQueueSize, numWorkers int,
	collectorQueueSize, batchSize int,
	dropPollInterval, staleTimeout, flushInterval time.Duration,
	logMaxSize int64, logMaxFiles int, logMaxAge, summaryInterval, shutdownDrain time.Duration,
	metricsPort int,
) []string {
	var errs []string
	if auditCapacity < 0 {
		errs = append(errs, "--audit-capacity must be >= 0")
	}
	if auditBlockSize <= 0 {
		errs = append(errs, "--audit-block-size must be > 0")
	}
	if auditQueueSize <= 0 {
		errs = append(errs, "--audit-queue-size must be > 0")
	}
	if maxPayloadSize <= 0 {
		errs = append(errs, "--max-payload-size must be > 0")
	}
	if workerQueueSize <= 0 {
		errs = append(errs, "--worker-queue-size must be > 0")
	}
	if numWorkers <= 0 {
		errs = append(errs, "--workers must be > 0")
	}
	if collectorQueueSize <= 0 {
		errs = append(errs, "--collector-queue-size must be > 0")
	}
	if batchSize <= 0 {
		errs = append(errs, "--batch-size must be > 0")
	}
	if dropPollInterval <= 0 {
		errs = append(errs, "--drop-poll-interval must be > 0")
	}
	if staleTimeout <= 0 {
		errs = append(errs, "--stale-timeout must be > 0")
	}
	if flushInterval <= 0 {
		errs = append(errs, "--flush-interval must be > 0")
	}
	if auditCapacity > 0 && auditBlockSize > 0 && auditBlockSize > auditCapacity {
		errs = append(errs, "--audit-block-size must be <= --audit-capacity")
	}
	if logMaxSize <= 0 {
		errs = append(errs, "--log-max-size must be > 0")
	}
	if logMaxFiles <= 0 {
		errs = append(errs, "--log-max-files must be > 0")
	}
	if logMaxAge <= 0 {
		errs = append(errs, "--log-max-age must be > 0")
	}
	if summaryInterval <= 0 {
		errs = append(errs, "--summary-interval must be > 0")
	}
	if shutdownDrain <= 0 {
		errs = append(errs, "--shutdown-drain must be > 0")
	}
	if metricsPort < 1 || metricsPort > 65535 {
		errs = append(errs, "--metrics-port must be between 1 and 65535")
	}
	return errs
}

// parseWebhookHeaders parses comma-separated key=value pairs into a map.
func parseWebhookHeaders(s string) map[string]string {
	headers := make(map[string]string)
	if s == "" {
		return headers
	}
	for _, pair := range strings.Split(s, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 {
			headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return headers
}

func main() {
	target := flag.String("target", "", "upstream API URL (e.g., https://api.openai.com)")
	listen := flag.String("listen", "localhost:8080", "address to listen on")
	auditCapacity := flag.Int("audit-capacity", defaultPoolCapacity, "audit buffer pool size in bytes (0 to disable)")
	auditBlockSize := flag.Int("audit-block-size", defaultBlockSize, "audit buffer block size in bytes")
	auditQueueSize := flag.Int("audit-queue-size", defaultQueueSize, "audit queue slot count")
	dropPollInterval := flag.Duration("drop-poll-interval", defaultDropPollInterval, "interval for polling drop counter")
	apiFormat := flag.String("api-format", "", "API payload format: openai, anthropic (auto-detected from --target if omitted)")
	maxPayloadSize := flag.Int("max-payload-size", defaultMaxPayloadSize, "per-request payload size limit in bytes")
	staleTimeout := flag.Duration("stale-timeout", defaultStaleTimeout, "staleness timeout for incomplete payloads")
	workerQueueSize := flag.Int("worker-queue-size", defaultWorkerQueueSize, "channel buffer size between assembler and workers")
	numWorkers := flag.Int("workers", runtime.GOMAXPROCS(0), "number of redaction worker goroutines")
	collectorQueueSize := flag.Int("collector-queue-size", defaultCollectorQueueSize, "channel buffer size between workers and collector")
	batchSize := flag.Int("batch-size", defaultBatchSize, "collector batch size before flush")
	flushInterval := flag.Duration("flush-interval", defaultFlushInterval, "collector flush interval")
	showVersion := flag.Bool("version", false, "print version and exit")

	// Evidence pipeline flags.
	logDir := flag.String("log-dir", ".", "directory for JSONL telemetry logs")
	logMaxSize := flag.Int64("log-max-size", 100*1024*1024, "max JSONL file size in bytes before rotation")
	logMaxAge := flag.Duration("log-max-age", 1*time.Hour, "max JSONL file age before rotation")
	logMaxFiles := flag.Int("log-max-files", 24, "max rotated JSONL files to retain")
	evidenceDir := flag.String("evidence-dir", ".", "directory for JSON evidence summaries")
	summaryInterval := flag.Duration("summary-interval", 60*time.Minute, "interval between evidence summary reports")
	webhookURL := flag.String("webhook-url", "", "URL for evidence summary webhook delivery")
	webhookHeaders := flag.String("webhook-headers", "", "comma-separated key=value headers for webhook")
	metricsPort := flag.Int("metrics-port", 9090, "port for Prometheus metrics server")
	shutdownDrain := flag.Duration("shutdown-drain", 10*time.Second, "HTTP connection drain budget during shutdown")

	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *target == "" {
		fmt.Fprintln(os.Stderr, "error: --target is required")
		flag.Usage()
		os.Exit(1)
	}

	targetURL, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("invalid target URL: %v", err)
	}

	if targetURL.Scheme != "http" && targetURL.Scheme != "https" {
		log.Fatalf("target URL must have http or https scheme, got %q", targetURL.Scheme)
	}

	if *apiFormat != "" && parseAPIFormat(*apiFormat) == apiTypeUnknown {
		fmt.Fprintf(os.Stderr, "error: unrecognized --api-format %q (valid: openai, anthropic)\n", *apiFormat)
		os.Exit(1)
	}

	if errs := validateFlags(
		*auditCapacity, *auditBlockSize, *auditQueueSize,
		*maxPayloadSize, *workerQueueSize, *numWorkers,
		*collectorQueueSize, *batchSize,
		*dropPollInterval, *staleTimeout, *flushInterval,
		*logMaxSize, *logMaxFiles, *logMaxAge, *summaryInterval, *shutdownDrain,
		*metricsPort,
	); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "error:", e)
		}
		os.Exit(1)
	}

	cfg := proxyConfig{targetURL: targetURL}

	var ready atomic.Bool
	var auditQueue chan *auditBlock
	var stopPoller func()
	var waitPipeline func()
	var tickerCancel context.CancelFunc
	var tickerDone chan struct{}
	var jsonlWriter *JSONLWriter
	var metricsServer *http.Server
	var metrics *outbandMetrics
	var aggregator *Aggregator
	var drops *dropCounter
	var ioErrors atomic.Uint64
	var lastDropsReported int64
	var lastIOErrsReported uint64

	if *auditCapacity > 0 {
		pool := newBlockPool(*auditCapacity, *auditBlockSize)
		auditQueue = make(chan *auditBlock, *auditQueueSize)
		drops = newDropCounter()
		cfg.auditPool = pool
		cfg.auditQueue = auditQueue
		cfg.drops = drops
		cfg.nextReqID = &atomic.Uint64{}

		// Prometheus metrics.
		promRegistry := prometheus.NewRegistry()
		metrics = newMetrics(promRegistry, drops)
		metricsAddr := fmt.Sprintf(":%d", *metricsPort)
		var metricsErr error
		metricsServer, metricsErr = startMetricsServer(metricsAddr, promRegistry)
		if metricsErr != nil {
			log.Fatalf("failed to start metrics server on %s: %v", metricsAddr, metricsErr)
		}

		// Aggregator.
		chain := &RedactorChain{redactors: []Redactor{NewRegexRedactor()}}
		aggregator = NewAggregator(chain.Name())

		// JSONL writer.
		var writerErr error
		jsonlWriter, writerErr = NewJSONLWriter(*logDir, *logMaxSize, *logMaxAge, *logMaxFiles)
		if writerErr != nil {
			log.Fatalf("failed to initialize JSONL writer: %v", writerErr)
		}

		// Determine API type: explicit flag overrides URL heuristic.
		apiT := parseAPIFormat(*apiFormat)
		if apiT == apiTypeUnknown {
			apiT = detectAPIType(targetURL)
		}

		// Pipeline channels.
		workerInput := make(chan *assembledPayload, *workerQueueSize)
		resultsChannel := make(chan *telemetryLog, *collectorQueueSize)

		// Stage 1: Assembler.
		var asmStats assemblerStats
		assemblerDone := make(chan struct{})
		go func() {
			defer close(assemblerDone)
			startAssembler(auditQueue, assemblerConfig{
				maxPayloadSize: *maxPayloadSize,
				staleTimeout:   *staleTimeout,
				workerInput:    workerInput,
				pool:           pool,
				apiType:        apiT,
			}, &asmStats)
		}()

		// Stage 2: Workers.
		var wkStats workerStats
		waitWorkers := startWorkers(*numWorkers, workerInput, resultsChannel, chain, &wkStats)

		// Stage 3: Collector with composite flushFunc.
		var cStats collectorStats
		collectorDone := make(chan struct{})
		go func() {
			defer close(collectorDone)
			startCollector(collectorConfig{
				batchSize:     *batchSize,
				flushInterval: *flushInterval,
				input:         resultsChannel,
				flush: func(batch []*telemetryLog) {
					written, err := jsonlWriter.Flush(batch)
					if err != nil {
						log.Printf("JSONL write error: %v", err)
						ioErrors.Add(1)
						metrics.flushErrors.Inc()
					}
					// Only ingest the entries that were actually persisted.
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

		// Pipeline shutdown waiter: assembler → workers → collector.
		waitPipeline = func() {
			<-assemblerDone       // assembler finished, workerInput closed
			waitWorkers()         // all workers finished
			close(resultsChannel) // signal collector
			<-collectorDone       // collector flushed and exited
			log.Printf("pipeline stats: oversized_dropped=%d stale_dropped=%d worker_queue_full=%d budget_exceeded=%d result_dropped=%d flush_backlog=%d",
				asmStats.oversizedDropped.Load(), asmStats.staleDropped.Load(),
				asmStats.workerQueueFull.Load(), asmStats.budgetExceeded.Load(),
				wkStats.resultDropped.Load(), cStats.flushBacklog.Load())
		}

		// Egress dispatcher.
		logger := log.Default()
		var egressTargets []EgressTarget
		egressTargets = append(egressTargets, NewLocalJSONTarget(*evidenceDir))
		if _, ok := GetTarget("pdf"); !ok {
			logger.Println("PDF evidence reports require outband-enterprise. See outband.dev/pricing.")
		}
		if *webhookURL != "" {
			hdrs := parseWebhookHeaders(*webhookHeaders)
			egressTargets = append(egressTargets, NewWebhookTarget(*webhookURL, hdrs))
		}
		dispatcher := NewEgressDispatcher(logger, egressTargets...)

		// Evidence ticker goroutine.
		const dispatchTimeout = 30 * time.Second
		var tickerCtx context.Context
		tickerCtx, tickerCancel = context.WithCancel(context.Background())
		tickerDone = make(chan struct{})
		go func() {
			defer close(tickerDone)
			tick := time.NewTicker(*summaryInterval)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					if tickerCtx.Err() != nil {
						return
					}
					currentDrops := drops.total.Load()
					currentIOErrs := ioErrors.Load()
					windowDrops := uint64(currentDrops - lastDropsReported)
					windowIOErrs := currentIOErrs - lastIOErrsReported
					lastDropsReported = currentDrops
					lastIOErrsReported = currentIOErrs
					summary := aggregator.SnapshotAndReset(windowIOErrs, windowDrops, false, version)
					// Per-dispatch timeout prevents a slow webhook from
					// blocking subsequent SnapshotAndReset windows.
					dCtx, dCancel := context.WithTimeout(tickerCtx, dispatchTimeout)
					if err := dispatcher.Dispatch(dCtx, summary); err != nil {
						log.Printf("egress dispatch error: %v", err)
					}
					dCancel()
					metrics.evidenceReports.Inc()
				case <-tickerCtx.Done():
					return
				}
			}
		}()

		// Wire latency recording into proxy config.
		cfg.recordOverhead = func(d time.Duration) {
			aggregator.RecordLatency(d)
			metrics.proxyOverhead.Observe(d.Seconds())
			metrics.requestsTotal.Inc()
		}

		stopPoller = startDropPoller(context.Background(), drops, *dropPollInterval, log.Default())
		ready.Store(true)
		log.Printf("audit pipeline enabled: pool=%dMB, block=%dKB, queue=%d, workers=%d, metrics=:%d",
			*auditCapacity/(1024*1024), *auditBlockSize/1024, *auditQueueSize, *numWorkers, *metricsPort)
	} else {
		// No audit pipeline — ready immediately.
		ready.Store(true)
	}

	proxy := newProxy(cfg)
	handler := newLatencyMiddleware(newHealthHandler(proxy, targetURL, &ready))

	server := &http.Server{
		Addr:    *listen,
		Handler: handler,
	}

	log.Printf("proxying %s -> %s", *listen, targetURL)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	// --- Time-budgeted, panic-safe shutdown ---

	log.Println("shutting down")

	// 1. Signal readiness failure to load balancers immediately.
	ready.Store(false)

	// 2. Budget the network drain — separate context, separate deadline.
	// server.Shutdown blocks until all active connections finish or context
	// expires. With 20s SSE streams and a short deadline, K8s sends SIGKILL.
	httpCtx, httpCancel := context.WithTimeout(context.Background(), *shutdownDrain)
	defer httpCancel()
	shutdownErr := server.Shutdown(httpCtx)
	if shutdownErr != nil {
		log.Printf("network drain timeout: %v", shutdownErr)
		// Force-close remaining connections so no TeeReaders survive.
		server.Close()
	}

	// 3. HTTP server is fully stopped. No active TeeReaders remain.
	// Safe to close the queue without send-on-closed-channel panics.
	if auditQueue != nil {
		close(auditQueue)
	}

	// 4. Budget the internal flush — 5-second deadline enforced via goroutine.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer flushCancel()
	pipelineStopped := true
	if waitPipeline != nil {
		pipelineDone := make(chan struct{})
		go func() {
			waitPipeline()
			close(pipelineDone)
		}()
		select {
		case <-pipelineDone:
		case <-flushCtx.Done():
			log.Println("warning: pipeline flush exceeded 5s budget")
			pipelineStopped = false
		}
	}

	// 5. Stop the evidence ticker.
	tickerStopped := true
	if tickerCancel != nil {
		tickerCancel()
		select {
		case <-tickerDone:
		case <-flushCtx.Done():
			log.Println("warning: ticker drain exceeded budget")
			tickerStopped = false
		}
	}

	// 6. Final partial evidence report and cleanup — only safe if both the
	// pipeline and ticker have fully stopped. If either timed out, the
	// collector may still be writing to jsonlWriter and reading aggregator
	// counters. Proceeding would race.
	if pipelineStopped && tickerStopped {
		if drops != nil && aggregator != nil {
			finalDrops := uint64(drops.total.Load() - lastDropsReported)
			finalIOErrs := ioErrors.Load() - lastIOErrsReported
			summary := aggregator.SnapshotAndReset(finalIOErrs, finalDrops, true, version)
			localJSON := NewLocalJSONTarget(*evidenceDir)
			if err := localJSON.Push(context.Background(), summary); err != nil {
				log.Printf("CRITICAL: failed to write final evidence summary: %v", err)
			}
			if metrics != nil {
				metrics.evidenceReports.Inc()
			}
		}

		// 7. Cleanup remaining resources.
		if jsonlWriter != nil {
			jsonlWriter.Close()
		}
		if metricsServer != nil {
			metricsServer.Shutdown(flushCtx)
		}
	} else {
		log.Println("warning: skipping final evidence report and cleanup — background drains still running")
		if metricsServer != nil {
			metricsServer.Shutdown(flushCtx)
		}
	}
	if stopPoller != nil {
		stopPoller()
	}
	log.Println("sidecar termination complete")
}
