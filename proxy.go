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
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is set at build time via ldflags in cmd/outband/main.go.
var Version = "dev"

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

// FlagConfig holds CLI flag values parsed by ProxyConfig.Parse().
// Separated from runtime dependencies for clarity.
type FlagConfig struct {
	Listen             string
	Target             string
	AuditCapacity      int
	AuditBlockSize     int
	AuditQueueSize     int
	DropPollInterval   time.Duration
	APIFormat          string
	MaxPayloadSize     int
	StaleTimeout       time.Duration
	WorkerQueueSize    int
	NumWorkers         int
	CollectorQueueSize int
	BatchSize          int
	FlushInterval      time.Duration
	LogDir             string
	LogMaxSize         int64
	LogMaxAge          time.Duration
	LogMaxFiles        int
	EvidenceDir        string
	SummaryInterval    time.Duration
	WebhookURL         string
	WebhookHeaders     string
	MetricsPort        int
	ShutdownDrain      time.Duration
	ShowVersion        bool
}

// ProxyConfig holds the runtime dependencies and extension points for
// the reverse proxy. Embeds FlagConfig for CLI tuning parameters.
type ProxyConfig struct {
	FlagConfig

	TargetURL      *url.URL
	AuditPool      BlockAllocator    // nil = audit disabled
	AuditQueue     chan *AuditBlock   // global audit queue; bidirectional because Run() both sends (via auditReader) and closes during shutdown
	Drops          DropRecorder      // nil = no drop tracking
	NextReqID      *atomic.Uint64    // monotonic request ID generator
	RecordOverhead func(time.Duration) // nil = latency recording disabled
	Chain          *RedactorChain

	// Enterprise extension points for response capture.
	ResponseCapture       bool
	ResponseWriterFactory func(http.ResponseWriter, uint64) http.ResponseWriter
	ResponseQueue         chan *AuditBlock // bidirectional because Run() both sends and closes during shutdown
	ResponsePool          *BlockPool
	ResponseDrops         *DropCounter
}

func newProxy(cfg *ProxyConfig) *httputil.ReverseProxy {
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
			r.SetURL(cfg.TargetURL)
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			if _, ok := r.In.Header["User-Agent"]; !ok {
				r.Out.Header["User-Agent"] = []string{""}
			}
			if r.Out.Body != nil && cfg.AuditPool != nil {
				// Use requestID from context if set by response capture
				// middleware; otherwise generate a new one.
				var reqID uint64
				if id, ok := r.In.Context().Value(requestIDKey).(uint64); ok {
					reqID = id
				} else {
					reqID = cfg.NextReqID.Add(1)
				}
				r.Out.Body = newAuditReader(r.Out.Body, cfg.AuditPool, cfg.Drops, cfg.AuditQueue, reqID)
			}
			if cfg.RecordOverhead != nil {
				if start, ok := requestStart(r.In.Context()); ok {
					cfg.RecordOverhead(time.Since(start))
				}
			}
		},
		Transport:  transport,
		BufferPool: &bufferPool{},
	}
}

// validateFlags checks all numeric and duration flags for invalid values.
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

// DefaultConfig returns a ProxyConfig with sane defaults.
func DefaultConfig() *ProxyConfig {
	return &ProxyConfig{
		FlagConfig: FlagConfig{
			Listen:             "localhost:8080",
			AuditCapacity:      defaultPoolCapacity,
			AuditBlockSize:     defaultBlockSize,
			AuditQueueSize:     defaultQueueSize,
			DropPollInterval:   defaultDropPollInterval,
			MaxPayloadSize:     defaultMaxPayloadSize,
			StaleTimeout:       defaultStaleTimeout,
			WorkerQueueSize:    defaultWorkerQueueSize,
			NumWorkers:         0, // resolved to GOMAXPROCS in Parse
			CollectorQueueSize: defaultCollectorQueueSize,
			BatchSize:          defaultBatchSize,
			FlushInterval:      defaultFlushInterval,
			LogMaxSize:         100 * 1024 * 1024,
			LogMaxAge:          1 * time.Hour,
			LogMaxFiles:        24,
			SummaryInterval:    60 * time.Minute,
			MetricsPort:        9090,
			ShutdownDrain:      10 * time.Second,
		},
	}
}

// responseCaptureMiddleware wraps the response writer when ResponseCapture is
// enabled. It generates a requestID, stores it in context for the Rewrite
// hook, wraps the response writer via ResponseWriterFactory, and defers
// Close() to dispatch the buffered response to the audit queue.
type responseCaptureMiddleware struct {
	next    http.Handler
	cfg     *ProxyConfig
}

func newResponseCaptureMiddleware(next http.Handler, cfg *ProxyConfig) http.Handler {
	if !cfg.ResponseCapture || cfg.ResponseWriterFactory == nil {
		return next
	}
	return &responseCaptureMiddleware{next: next, cfg: cfg}
}

func (m *responseCaptureMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var reqID uint64
	if m.cfg.NextReqID != nil {
		reqID = m.cfg.NextReqID.Add(1)
	}
	ctx := context.WithValue(r.Context(), requestIDKey, reqID)

	auditWriter := m.cfg.ResponseWriterFactory(w, reqID)
	defer func() {
		if closer, ok := auditWriter.(io.Closer); ok {
			closer.Close()
		}
	}()

	m.next.ServeHTTP(auditWriter, r.WithContext(ctx))
}

// Parse reads CLI flags into the ProxyConfig. Call after DefaultConfig().
func (c *ProxyConfig) Parse() {
	parseFlags(c)
}
