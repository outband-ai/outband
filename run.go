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
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// parseFlags registers CLI flags and parses them into a ProxyConfig.
func parseFlags(c *ProxyConfig) {
	flag.StringVar(&c.Target, "target", "", "upstream API URL (e.g., https://api.openai.com)")
	flag.StringVar(&c.Listen, "listen", c.Listen, "address to listen on")
	flag.IntVar(&c.AuditCapacity, "audit-capacity", c.AuditCapacity, "audit buffer pool size in bytes (0 to disable)")
	flag.IntVar(&c.AuditBlockSize, "audit-block-size", c.AuditBlockSize, "audit buffer block size in bytes")
	flag.IntVar(&c.AuditQueueSize, "audit-queue-size", c.AuditQueueSize, "audit queue slot count")
	flag.DurationVar(&c.DropPollInterval, "drop-poll-interval", c.DropPollInterval, "interval for polling drop counter")
	flag.StringVar(&c.APIFormat, "api-format", "", "API payload format: openai, anthropic (auto-detected from --target if omitted)")
	flag.IntVar(&c.MaxPayloadSize, "max-payload-size", c.MaxPayloadSize, "per-request payload size limit in bytes")
	flag.DurationVar(&c.StaleTimeout, "stale-timeout", c.StaleTimeout, "staleness timeout for incomplete payloads")
	flag.IntVar(&c.WorkerQueueSize, "worker-queue-size", c.WorkerQueueSize, "channel buffer size between assembler and workers")
	flag.IntVar(&c.NumWorkers, "workers", runtime.GOMAXPROCS(0), "number of redaction worker goroutines")
	flag.IntVar(&c.CollectorQueueSize, "collector-queue-size", c.CollectorQueueSize, "channel buffer size between workers and collector")
	flag.IntVar(&c.BatchSize, "batch-size", c.BatchSize, "collector batch size before flush")
	flag.DurationVar(&c.FlushInterval, "flush-interval", c.FlushInterval, "collector flush interval")
	flag.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	flag.StringVar(&c.LogDir, "log-dir", ".", "directory for JSONL telemetry logs")
	flag.Int64Var(&c.LogMaxSize, "log-max-size", c.LogMaxSize, "max JSONL file size in bytes before rotation")
	flag.DurationVar(&c.LogMaxAge, "log-max-age", c.LogMaxAge, "max JSONL file age before rotation")
	flag.IntVar(&c.LogMaxFiles, "log-max-files", c.LogMaxFiles, "max rotated JSONL files to retain")
	flag.StringVar(&c.EvidenceDir, "evidence-dir", ".", "directory for JSON evidence summaries")
	flag.DurationVar(&c.SummaryInterval, "summary-interval", c.SummaryInterval, "interval between evidence summary reports")
	flag.StringVar(&c.WebhookURL, "webhook-url", "", "URL for evidence summary webhook delivery")
	flag.StringVar(&c.WebhookHeaders, "webhook-headers", "", "comma-separated key=value headers for webhook")
	flag.IntVar(&c.MetricsPort, "metrics-port", c.MetricsPort, "port for Prometheus metrics server")
	flag.DurationVar(&c.ShutdownDrain, "shutdown-drain", c.ShutdownDrain, "HTTP connection drain budget during shutdown")

	flag.Parse()

	if c.ShowVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	if c.Target == "" {
		fmt.Fprintln(os.Stderr, "error: --target is required")
		flag.Usage()
		os.Exit(1)
	}

	targetURL, err := url.Parse(c.Target)
	if err != nil {
		log.Fatalf("invalid target URL: %v", err)
	}
	if targetURL.Scheme != "http" && targetURL.Scheme != "https" {
		log.Fatalf("target URL must have http or https scheme, got %q", targetURL.Scheme)
	}
	c.TargetURL = targetURL

	if c.APIFormat != "" && parseAPIFormat(c.APIFormat) == apiTypeUnknown {
		fmt.Fprintf(os.Stderr, "error: unrecognized --api-format %q (valid: openai, anthropic)\n", c.APIFormat)
		os.Exit(1)
	}

	if errs := validateFlags(
		c.AuditCapacity, c.AuditBlockSize, c.AuditQueueSize,
		c.MaxPayloadSize, c.WorkerQueueSize, c.NumWorkers,
		c.CollectorQueueSize, c.BatchSize,
		c.DropPollInterval, c.StaleTimeout, c.FlushInterval,
		c.LogMaxSize, c.LogMaxFiles, c.LogMaxAge, c.SummaryInterval, c.ShutdownDrain,
		c.MetricsPort,
	); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "error:", e)
		}
		os.Exit(1)
	}
}

// Run starts the proxy with the given configuration. It blocks until
// a SIGINT or SIGTERM signal is received.
func Run(cfg *ProxyConfig) {
	var ready atomic.Bool
	var auditQueue chan *AuditBlock
	var stopPoller func()
	var waitPipeline func()
	var tickerCancel context.CancelFunc
	var tickerDone chan struct{}
	var jsonlWriter *JSONLWriter
	var metricsServer *http.Server
	var metrics *outbandMetrics
	var aggregator *Aggregator
	var drops *DropCounter
	var ioErrors atomic.Uint64
	var lastDropsReported int64
	var lastResponseDropsReported int64
	var lastIOErrsReported uint64

	if cfg.AuditCapacity > 0 {
		pool := NewBlockPool(cfg.AuditCapacity, cfg.AuditBlockSize)
		auditQueue = make(chan *AuditBlock, cfg.AuditQueueSize)
		drops = NewDropCounter()
		cfg.AuditPool = pool
		cfg.AuditQueue = auditQueue
		cfg.Drops = drops
		cfg.NextReqID = &atomic.Uint64{}

		// Prometheus metrics.
		promRegistry := prometheus.NewRegistry()
		metrics = newMetrics(promRegistry, drops)
		metricsAddr := fmt.Sprintf(":%d", cfg.MetricsPort)
		var metricsErr error
		metricsServer, metricsErr = startMetricsServer(metricsAddr, promRegistry)
		if metricsErr != nil {
			log.Fatalf("failed to start metrics server on %s: %v", metricsAddr, metricsErr)
		}

		// Aggregator.
		chain := cfg.Chain
		if chain == nil {
			chain = NewRedactorChain(NewRegexRedactor())
			cfg.Chain = chain
		}
		aggregator = NewAggregator(chain.Name())

		// JSONL writer.
		var writerErr error
		jsonlWriter, writerErr = NewJSONLWriter(cfg.LogDir, cfg.LogMaxSize, cfg.LogMaxAge, cfg.LogMaxFiles)
		if writerErr != nil {
			log.Fatalf("failed to initialize JSONL writer: %v", writerErr)
		}

		// Determine API type: explicit flag overrides URL heuristic.
		apiT := parseAPIFormat(cfg.APIFormat)
		if apiT == apiTypeUnknown {
			apiT = detectAPIType(cfg.TargetURL)
		}

		// Pipeline channels.
		workerInput := make(chan *assembledPayload, cfg.WorkerQueueSize)
		requestResults := make(chan *TelemetryLog, cfg.CollectorQueueSize)

		// SessionTracker merges request and response TelemetryLog entries
		// by RequestID before forwarding to the collector. In open source
		// mode (bidir=false), entries pass through immediately.
		collectorInput := make(chan *TelemetryLog, cfg.CollectorQueueSize)
		trackerCtx, trackerCancel := context.WithCancel(context.Background())
		tracker := NewSessionTracker(collectorInput, 60*time.Second, cfg.ResponseCapture)
		trackerDone := tracker.Start(trackerCtx)

		// Stage 1: Request assembler.
		var asmStats assemblerStats
		assemblerDone := make(chan struct{})
		go func() {
			defer close(assemblerDone)
			startAssembler(auditQueue, assemblerConfig{
				maxPayloadSize: cfg.MaxPayloadSize,
				staleTimeout:   cfg.StaleTimeout,
				workerInput:    workerInput,
				pool:           pool,
				apiType:        apiT,
			}, &asmStats)
		}()

		// Stage 2: Request workers.
		var wkStats workerStats
		waitWorkers := startWorkers(cfg.NumWorkers, workerInput, requestResults, chain, &wkStats)

		// Bridge: request worker results → SessionTracker.SubmitRequest.
		requestBridgeDone := make(chan struct{})
		go func() {
			defer close(requestBridgeDone)
			for entry := range requestResults {
				tracker.SubmitRequest(entry)
			}
		}()

		// Response pipeline (enterprise only).
		var respAsmStats assemblerStats
		var respWkStats workerStats
		var respAssemblerDone chan struct{}
		var waitResponseWorkers func()
		var responseBridgeDone chan struct{}
		var responseResults chan *TelemetryLog

		if cfg.ResponseCapture && cfg.ResponseQueue != nil {
			respWorkerInput := make(chan *assembledPayload, cfg.WorkerQueueSize)
			responseResults = make(chan *TelemetryLog, cfg.CollectorQueueSize)

			respAssemblerDone = make(chan struct{})
			go func() {
				defer close(respAssemblerDone)
				startAssembler(cfg.ResponseQueue, assemblerConfig{
					maxPayloadSize: cfg.MaxPayloadSize,
					staleTimeout:   cfg.StaleTimeout,
					workerInput:    respWorkerInput,
					pool:           cfg.ResponsePool,
					apiType:        apiT,
					direction:      DirectionResponse,
				}, &respAsmStats)
			}()

			waitResponseWorkers = startWorkers(cfg.NumWorkers, respWorkerInput, responseResults, chain, &respWkStats)

			responseBridgeDone = make(chan struct{})
			go func() {
				defer close(responseBridgeDone)
				for entry := range responseResults {
					tracker.SubmitResponse(entry)
				}
			}()
		}

		// Stage 3: Collector reads from SessionTracker output.
		var cStats collectorStats
		collectorDone := make(chan struct{})
		go func() {
			defer close(collectorDone)
			startCollector(collectorConfig{
				batchSize:     cfg.BatchSize,
				flushInterval: cfg.FlushInterval,
				input:         collectorInput,
				flush: func(batch []*TelemetryLog) {
					written, err := jsonlWriter.Flush(batch)
					if err != nil {
						log.Printf("JSONL write error: %v", err)
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

		// Pipeline shutdown waiter: assembler → workers → bridge → tracker → collector.
		waitPipeline = func() {
			// Request pipeline.
			<-assemblerDone
			waitWorkers()
			close(requestResults)
			<-requestBridgeDone

			// Response pipeline (if active).
			if respAssemblerDone != nil {
				close(cfg.ResponseQueue)
				<-respAssemblerDone
				waitResponseWorkers()
				close(responseResults) // will be captured by closure
				<-responseBridgeDone
			}

			// Stop tracker sweeper, wait for final sweep to complete
			// before closing the collector input channel. Without this
			// wait, the final sweep's emit() races with close().
			trackerCancel()
			<-trackerDone

			// Close collector input and wait for final flush.
			close(collectorInput)
			<-collectorDone
			log.Printf("pipeline stats: oversized_dropped=%d stale_dropped=%d worker_queue_full=%d budget_exceeded=%d result_dropped=%d flush_backlog=%d",
				asmStats.oversizedDropped.Load(), asmStats.staleDropped.Load(),
				asmStats.workerQueueFull.Load(), asmStats.budgetExceeded.Load(),
				wkStats.resultDropped.Load(), cStats.flushBacklog.Load())
		}

		// Egress dispatcher.
		logger := log.Default()
		var egressTargets []EgressTarget
		egressTargets = append(egressTargets, NewLocalJSONTarget(cfg.EvidenceDir))
		if _, ok := GetTarget("pdf"); !ok {
			logger.Println("PDF evidence reports require outband-enterprise. See outband.dev/pricing.")
		}
		if cfg.WebhookURL != "" {
			hdrs := parseWebhookHeaders(cfg.WebhookHeaders)
			egressTargets = append(egressTargets, NewWebhookTarget(cfg.WebhookURL, hdrs))
		}
		dispatcher := NewEgressDispatcher(logger, egressTargets...)

		// Evidence ticker goroutine.
		const dispatchTimeout = 30 * time.Second
		var tickerCtx context.Context
		tickerCtx, tickerCancel = context.WithCancel(context.Background())
		tickerDone = make(chan struct{})
		go func() {
			defer close(tickerDone)
			tick := time.NewTicker(cfg.SummaryInterval)
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
					var windowResponseDrops uint64
					if cfg.ResponseDrops != nil {
						currentResponseDrops := cfg.ResponseDrops.Total.Load()
						windowResponseDrops = uint64(currentResponseDrops - lastResponseDropsReported)
						lastResponseDropsReported = currentResponseDrops
					}
					lastDropsReported = currentDrops
					lastIOErrsReported = currentIOErrs
					summary := aggregator.SnapshotAndReset(windowIOErrs, windowDrops, windowResponseDrops, false, Version)
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
		cfg.RecordOverhead = func(d time.Duration) {
			aggregator.RecordLatency(d)
			metrics.proxyOverhead.Observe(d.Seconds())
			metrics.requestsTotal.Inc()
		}

		stopPoller = startDropPoller(context.Background(), drops, cfg.DropPollInterval, log.Default())
		ready.Store(true)
		log.Printf("audit pipeline enabled: pool=%dMB, block=%dKB, queue=%d, workers=%d, metrics=:%d",
			cfg.AuditCapacity/(1024*1024), cfg.AuditBlockSize/1024, cfg.AuditQueueSize, cfg.NumWorkers, cfg.MetricsPort)
	} else {
		ready.Store(true)
	}

	proxy := newProxy(cfg)
	var proxyHandler http.Handler = proxy
	proxyHandler = newResponseCaptureMiddleware(proxyHandler, cfg)
	handler := newLatencyMiddleware(newHealthHandler(proxyHandler, cfg.TargetURL, &ready))

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
	}

	log.Printf("proxying %s -> %s", cfg.Listen, cfg.TargetURL)
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
	ready.Store(false)

	httpCtx, httpCancel := context.WithTimeout(context.Background(), cfg.ShutdownDrain)
	defer httpCancel()
	shutdownErr := server.Shutdown(httpCtx)
	if shutdownErr != nil {
		log.Printf("network drain timeout: %v", shutdownErr)
		server.Close()
	}

	if auditQueue != nil {
		close(auditQueue)
	}

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

	if pipelineStopped && tickerStopped {
		if drops != nil && aggregator != nil {
			finalDrops := uint64(drops.Total.Load() - lastDropsReported)
			var finalResponseDrops uint64
			if cfg.ResponseDrops != nil {
				finalResponseDrops = uint64(cfg.ResponseDrops.Total.Load() - lastResponseDropsReported)
			}
			finalIOErrs := ioErrors.Load() - lastIOErrsReported
			summary := aggregator.SnapshotAndReset(finalIOErrs, finalDrops, finalResponseDrops, true, Version)
			localJSON := NewLocalJSONTarget(cfg.EvidenceDir)
			if err := localJSON.Push(context.Background(), summary); err != nil {
				log.Printf("CRITICAL: failed to write final evidence summary: %v", err)
			}
			if metrics != nil {
				metrics.evidenceReports.Inc()
			}
		}
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

// ParseEvidenceSummary reads and parses a JSON evidence summary file.
func ParseEvidenceSummary(path string) (*EvidenceSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s EvidenceSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
