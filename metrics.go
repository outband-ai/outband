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
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// outbandMetrics holds all Prometheus metric descriptors for the sidecar.
// Prometheus counters never reset. Aggregator counters reset each window.
// Both exist for different consumers (Prometheus scraper vs. evidence
// summary) with different semantics (cumulative vs. windowed).
type outbandMetrics struct {
	requestsTotal    prometheus.Counter
	requestsAudited  prometheus.Counter
	piiDetected      *prometheus.CounterVec
	flushErrors      prometheus.Counter
	proxyOverhead    prometheus.Histogram
	evidenceReports  prometheus.Counter
	dropsCollector   *dropCollector
}

// newMetrics creates and registers all Prometheus metrics. The dropCounter
// is read via a custom collector to avoid duplicate counting.
func newMetrics(reg prometheus.Registerer, dc *DropCounter) *outbandMetrics {
	m := &outbandMetrics{
		requestsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outband_requests_total",
			Help: "Total HTTP requests that transited the proxy.",
		}),
		requestsAudited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outband_requests_audited_total",
			Help: "Requests that completed the full audit pipeline.",
		}),
		piiDetected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outband_pii_detected_total",
			Help: "PII instances detected, labeled by category.",
		}, []string{"category"}),
		flushErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outband_flush_errors_total",
			Help: "JSONL write failures.",
		}),
		proxyOverhead: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "outband_proxy_overhead_seconds",
			Help: "Proxy ingress overhead in seconds (request arrival to upstream dial).",
			Buckets: []float64{
				0.0001, 0.0005, 0.001, 0.002, 0.005,
				0.010, 0.025, 0.050, 0.100,
			},
		}),
		evidenceReports: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outband_evidence_reports_generated_total",
			Help: "Evidence summary reports generated.",
		}),
	}

	reg.MustRegister(
		m.requestsTotal,
		m.requestsAudited,
		m.piiDetected,
		m.flushErrors,
		m.proxyOverhead,
		m.evidenceReports,
	)

	if dc != nil {
		m.dropsCollector = newDropCollector(dc)
		reg.MustRegister(m.dropsCollector)
	}

	return m
}

// ---------------------------------------------------------------------------
// dropCollector — custom Prometheus collector that reads the atomic counter
// ---------------------------------------------------------------------------

// dropCollector implements prometheus.Collector by reading directly from the
// dropCounter's atomic total. No duplicate counting, no drift.
type dropCollector struct {
	desc *prometheus.Desc
	dc   *DropCounter
}

func newDropCollector(dc *DropCounter) *dropCollector {
	return &dropCollector{
		desc: prometheus.NewDesc(
			"outband_requests_dropped_total",
			"Requests where audit capture was abandoned due to buffer pressure.",
			nil, nil,
		),
		dc: dc,
	}
}

func (c *dropCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *dropCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(
		c.desc,
		prometheus.CounterValue,
		float64(c.dc.Total.Load()),
	)
}

// ---------------------------------------------------------------------------
// Metrics HTTP server
// ---------------------------------------------------------------------------

// startMetricsServer starts an HTTP server serving /metrics on the given
// address. Pre-binds the port so callers fail fast on address conflicts.
// Returns the server for graceful shutdown.
func startMetricsServer(addr string, gatherer prometheus.Gatherer) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics server error: %v", err)
		}
	}()
	return srv, nil
}
