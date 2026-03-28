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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetricsRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	dc := NewDropCounter()
	m := newMetrics(reg, dc)
	if m == nil {
		t.Fatal("newMetrics returned nil")
	}
}

func TestMetricsRegistrationNilDropCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg, nil)
	if m == nil {
		t.Fatal("newMetrics returned nil")
	}
	if m.dropsCollector != nil {
		t.Fatal("dropsCollector should be nil when DropCounter is nil")
	}
}

func TestDropCollectorReadsAtomic(t *testing.T) {
	dc := NewDropCounter()
	dc.Increment()
	dc.Increment()
	dc.Increment()

	reg := prometheus.NewRegistry()
	collector := newDropCollector(dc)
	reg.MustRegister(collector)

	val := testutil.ToFloat64(collector)
	if val != 3 {
		t.Fatalf("drop collector: got %f, want 3", val)
	}

	// Drop more and verify it updates.
	dc.Increment()
	dc.Increment()
	val = testutil.ToFloat64(collector)
	if val != 5 {
		t.Fatalf("drop collector after more drops: got %f, want 5", val)
	}
}

func TestPIICounterLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg, nil)

	m.piiDetected.WithLabelValues("SSN").Inc()
	m.piiDetected.WithLabelValues("SSN").Inc()
	m.piiDetected.WithLabelValues("EMAIL").Inc()

	ssnVal := testutil.ToFloat64(m.piiDetected.WithLabelValues("SSN"))
	if ssnVal != 2 {
		t.Fatalf("SSN: got %f, want 2", ssnVal)
	}
	emailVal := testutil.ToFloat64(m.piiDetected.WithLabelValues("EMAIL"))
	if emailVal != 1 {
		t.Fatalf("EMAIL: got %f, want 1", emailVal)
	}
}

func TestHistogramRecordsSubMs(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg, nil)

	// 0.8ms = 0.0008 seconds — must NOT truncate to 0.
	m.proxyOverhead.Observe(time.Duration(800 * time.Microsecond).Seconds())
	m.proxyOverhead.Observe(time.Duration(200 * time.Microsecond).Seconds())
	m.proxyOverhead.Observe(time.Duration(1500 * time.Microsecond).Seconds())

	// Gather and verify the histogram has 3 observations.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "outband_proxy_overhead_seconds" {
			for _, m := range mf.GetMetric() {
				h := m.GetHistogram()
				if h.GetSampleCount() != 3 {
					t.Fatalf("histogram count: got %d, want 3", h.GetSampleCount())
				}
				// Sum should be 0.0008 + 0.0002 + 0.0015 = 0.0025
				sum := h.GetSampleSum()
				if sum < 0.0024 || sum > 0.0026 {
					t.Fatalf("histogram sum: got %f, want ~0.0025", sum)
				}
				return
			}
		}
	}
	t.Fatal("outband_proxy_overhead_seconds not found in gathered metrics")
}

func TestMetricsEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	dc := NewDropCounter()
	dc.Increment()
	newMetrics(reg, dc)

	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "outband_requests_total") {
		t.Fatal("missing outband_requests_total")
	}
	if !strings.Contains(text, "outband_requests_dropped_total") {
		t.Fatal("missing outband_requests_dropped_total")
	}
	if !strings.Contains(text, "outband_proxy_overhead_seconds") {
		t.Fatal("missing outband_proxy_overhead_seconds")
	}
}

func TestCounterIncrements(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg, nil)

	m.requestsTotal.Inc()
	m.requestsTotal.Inc()
	m.requestsAudited.Add(5)
	m.flushErrors.Inc()
	m.evidenceReports.Inc()

	if v := testutil.ToFloat64(m.requestsTotal); v != 2 {
		t.Fatalf("requestsTotal: got %f", v)
	}
	if v := testutil.ToFloat64(m.requestsAudited); v != 5 {
		t.Fatalf("requestsAudited: got %f", v)
	}
	if v := testutil.ToFloat64(m.flushErrors); v != 1 {
		t.Fatalf("flushErrors: got %f", v)
	}
	if v := testutil.ToFloat64(m.evidenceReports); v != 1 {
		t.Fatalf("evidenceReports: got %f", v)
	}
}
