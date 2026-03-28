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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// waitForCondition polls check every pollInterval until it returns true or
// timeout expires. No time.Sleep — uses time.NewTicker for precise intervals.
func waitForCondition(timeout, pollInterval time.Duration, check func() bool) error {
	deadline := time.Now().Add(timeout)
	// Check immediately before first tick.
	if check() {
		return nil
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if check() {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("condition not met within %s", timeout)
			}
		}
	}
}

// waitForJSONLCount polls the JSONL file at fpath until it contains at least
// expectedCount parseable TelemetryLog entries, or timeout expires.
// Returns all parsed entries on success.
func waitForJSONLCount(fpath string, expectedCount int, timeout time.Duration) ([]*TelemetryLog, error) {
	var entries []*TelemetryLog
	err := waitForCondition(timeout, 50*time.Millisecond, func() bool {
		data, err := os.ReadFile(fpath)
		if err != nil {
			return false
		}
		lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
		entries = nil
		for _, line := range lines {
			if len(line) == 0 {
				continue
			}
			var entry TelemetryLog
			if err := json.Unmarshal(line, &entry); err != nil {
				continue
			}
			entries = append(entries, &entry)
		}
		return len(entries) >= expectedCount
	})
	if err != nil {
		return entries, fmt.Errorf("wanted %d JSONL entries, got %d: %w", expectedCount, len(entries), err)
	}
	return entries, nil
}

// waitForEvidenceFile polls dir until a .json evidence file appears.
// Returns the parsed EvidenceSummary from the first file found.
func waitForEvidenceFile(dir string, timeout time.Duration) (*EvidenceSummary, error) {
	var summary EvidenceSummary
	err := waitForCondition(timeout, 100*time.Millisecond, func() bool {
		files, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".json") {
				data, err := os.ReadFile(filepath.Join(dir, f.Name()))
				if err != nil {
					continue
				}
				if err := json.Unmarshal(data, &summary); err != nil {
					continue
				}
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("no evidence file in %s: %w", dir, err)
	}
	return &summary, nil
}

// waitForEvidenceCount polls dir until at least n .json evidence files exist.
// Returns all parsed summaries.
func waitForEvidenceCount(dir string, n int, timeout time.Duration) ([]*EvidenceSummary, error) {
	var summaries []*EvidenceSummary
	err := waitForCondition(timeout, 100*time.Millisecond, func() bool {
		files, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		summaries = nil
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".json") {
				data, err := os.ReadFile(filepath.Join(dir, f.Name()))
				if err != nil {
					continue
				}
				var s EvidenceSummary
				if err := json.Unmarshal(data, &s); err != nil {
					continue
				}
				summaries = append(summaries, &s)
			}
		}
		return len(summaries) >= n
	})
	if err != nil {
		return summaries, fmt.Errorf("wanted %d evidence files, got %d: %w", n, len(summaries), err)
	}
	return summaries, nil
}

// waitForPrometheusMetric polls the /metrics endpoint at metricsURL until the
// named metric reaches >= expectedValue, or timeout expires.
func waitForPrometheusMetric(metricsURL, metricName string, expectedValue float64, timeout time.Duration) error {
	lastValue := float64(-1)
	err := waitForCondition(timeout, 100*time.Millisecond, func() bool {
		resp, err := http.Get(metricsURL)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false
		}
		lastValue = parseMetricValue(string(body), metricName)
		return lastValue >= expectedValue
	})
	if err != nil {
		return fmt.Errorf("metric %s: wanted >= %g, got %g: %w", metricName, expectedValue, lastValue, err)
	}
	return nil
}

// parseMetricValue extracts a float64 value for the named metric from
// Prometheus exposition format text. Returns -1 if not found.
// Handles both simple metrics ("metric_name VALUE") and labeled metrics
// ("metric_name{label=\"val\"} VALUE").
func parseMetricValue(body, metricName string) float64 {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Match exact metric name at line start (with optional labels).
		if !strings.HasPrefix(line, metricName) {
			continue
		}
		rest := line[len(metricName):]
		// Must be followed by space, '{', or end of metric name.
		if len(rest) == 0 {
			continue
		}
		if rest[0] == '{' {
			// Skip past label block.
			idx := strings.IndexByte(rest, '}')
			if idx < 0 {
				continue
			}
			rest = rest[idx+1:]
		}
		if len(rest) == 0 || rest[0] != ' ' {
			continue
		}
		valStr := strings.TrimSpace(rest)
		// Handle timestamp suffix (value TIMESTAMP).
		if idx := strings.IndexByte(valStr, ' '); idx > 0 {
			valStr = valStr[:idx]
		}
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		return v
	}
	return -1
}
