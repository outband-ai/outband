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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EgressTarget receives evidence summaries for external delivery.
type EgressTarget interface {
	Push(ctx context.Context, summary *EvidenceSummary) error
	Name() string
}

// EgressDispatcher sends summaries to registered targets in order.
type EgressDispatcher struct {
	targets []EgressTarget
	logger  *log.Logger
}

// NewEgressDispatcher creates a dispatcher that sends to targets sequentially.
func NewEgressDispatcher(logger *log.Logger, targets ...EgressTarget) *EgressDispatcher {
	return &EgressDispatcher{targets: targets, logger: logger}
}

// Dispatch sends the summary to each target sequentially. Logs errors but
// does not stop on failure (best-effort delivery for all targets). Respects
// context cancellation between targets.
func (d *EgressDispatcher) Dispatch(ctx context.Context, summary *EvidenceSummary) error {
	var errs []error
	for _, target := range d.targets {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", target.Name(), err))
			break
		}
		if err := target.Push(ctx, summary); err != nil {
			d.logger.Printf("egress %s: %v", target.Name(), err)
			errs = append(errs, fmt.Errorf("%s: %w", target.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Plugin registry for enterprise targets
// ---------------------------------------------------------------------------

var (
	registryMu        sync.Mutex
	registeredTargets = map[string]func(config map[string]string) EgressTarget{}
)

// RegisterTarget registers a named target factory. Called from init() in
// enterprise-only files.
func RegisterTarget(name string, factory func(config map[string]string) EgressTarget) {
	registryMu.Lock()
	registeredTargets[name] = factory
	registryMu.Unlock()
}

// GetTarget retrieves a registered target factory by name.
func GetTarget(name string) (func(config map[string]string) EgressTarget, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	f, ok := registeredTargets[name]
	return f, ok
}

// ---------------------------------------------------------------------------
// LocalJSONTarget — always active, free tier
// ---------------------------------------------------------------------------

// LocalJSONTarget writes the evidence summary as a formatted JSON file.
type LocalJSONTarget struct {
	outputDir string
}

// NewLocalJSONTarget creates a target that writes JSON summaries to outputDir.
func NewLocalJSONTarget(outputDir string) *LocalJSONTarget {
	return &LocalJSONTarget{outputDir: outputDir}
}

func (t *LocalJSONTarget) Push(_ context.Context, summary *EvidenceSummary) error {
	if err := os.MkdirAll(t.outputDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	filename := fmt.Sprintf("evidence-%s.json",
		summary.WindowStart.UTC().Format("2006-01-02T15-04-05.000000000"))
	path := filepath.Join(t.outputDir, filename)
	return os.WriteFile(path, data, 0o600)
}

func (t *LocalJSONTarget) Name() string { return "local-json" }

// ---------------------------------------------------------------------------
// WebhookTarget — free tier, optional
// ---------------------------------------------------------------------------

// WebhookTarget posts the summary as JSON to an HTTP endpoint with
// exponential backoff retry on server errors.
type WebhookTarget struct {
	url     string
	headers map[string]string
	client  *http.Client
}

// NewWebhookTarget creates a target that POSTs summaries to the given URL.
func NewWebhookTarget(url string, headers map[string]string) *WebhookTarget {
	return &WebhookTarget{
		url:     url,
		headers: headers,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Push sends the summary as JSON. Retries up to 3 times on 5xx errors with
// exponential backoff (500ms, 1s, 2s). 4xx errors are not retried. All HTTP
// calls and backoff sleeps respect context cancellation.
func (t *WebhookTarget) Push(ctx context.Context, summary *EvidenceSummary) error {
	body, err := json.Marshal(summary)
	if err != nil {
		return err
	}

	const maxRetries = 3
	backoff := 500 * time.Millisecond

	// Generate a single idempotency key so all retry attempts are
	// deduplicated by the receiving server.
	var idempotencyKey string
	var keyBuf [16]byte
	if _, err := rand.Read(keyBuf[:]); err == nil {
		idempotencyKey = hex.EncodeToString(keyBuf[:])
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
				backoff *= 2
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		// Apply user headers first, then set reserved headers after so
		// users cannot override Content-Type or Idempotency-Key.
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}

		resp, err := t.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode >= 500 {
			continue
		}
		return fmt.Errorf("webhook %s: HTTP %d", t.url, resp.StatusCode)
	}

	return fmt.Errorf("webhook %s: exhausted %d retries", t.url, maxRetries)
}

func (t *WebhookTarget) Name() string { return "webhook" }
