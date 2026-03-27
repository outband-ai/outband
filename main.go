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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
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
	targetURL  *url.URL
	auditPool  blockAllocator    // nil = audit disabled
	auditQueue chan<- *auditBlock // global audit queue
	drops      dropRecorder      // nil = no drop tracking
	nextReqID  *atomic.Uint64    // monotonic request ID generator
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
	return errs
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
	if *auditCapacity > 0 {
		pool := newBlockPool(*auditCapacity, *auditBlockSize)
		auditQueue = make(chan *auditBlock, *auditQueueSize)
		drops := newDropCounter()
		cfg.auditPool = pool
		cfg.auditQueue = auditQueue
		cfg.drops = drops
		cfg.nextReqID = &atomic.Uint64{}

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
		chain := &RedactorChain{redactors: []Redactor{NewRegexRedactor()}}
		var wkStats workerStats
		waitWorkers := startWorkers(*numWorkers, workerInput, resultsChannel, chain, &wkStats)

		// Stage 3: Collector.
		var cStats collectorStats
		collectorDone := make(chan struct{})
		logger := log.Default()
		go func() {
			defer close(collectorDone)
			startCollector(collectorConfig{
				batchSize:     *batchSize,
				flushInterval: *flushInterval,
				input:         resultsChannel,
				flush: func(batch []*telemetryLog) {
					for _, entry := range batch {
						data, _ := json.Marshal(entry)
						logger.Printf("TELEMETRY: %s", data)
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

		stopPoller = startDropPoller(context.Background(), drops, *dropPollInterval, log.Default())
		ready.Store(true)
		log.Printf("audit pipeline enabled: pool=%dMB, block=%dKB, queue=%d, workers=%d",
			*auditCapacity/(1024*1024), *auditBlockSize/1024, *auditQueueSize, *numWorkers)
	} else {
		// No audit pipeline — ready immediately.
		ready.Store(true)
	}

	proxy := newProxy(cfg)
	handler := newHealthHandler(proxy, targetURL, &ready)

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

	log.Println("shutting down")
	ready.Store(false) // reject /readyz immediately so K8s stops routing traffic
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(ctx)
	if shutdownErr != nil {
		log.Printf("server shutdown error: %v", shutdownErr)
	}
	// Always drain the pipeline regardless of Shutdown outcome so the
	// collector flushes any buffered telemetry before exit.
	if auditQueue != nil {
		close(auditQueue)
	}
	if waitPipeline != nil {
		waitPipeline()
	}
	if stopPoller != nil {
		stopPoller()
	}
	if shutdownErr != nil {
		os.Exit(1)
	}
}
