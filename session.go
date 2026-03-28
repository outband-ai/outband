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
	"sync"
	"time"
)

// SessionTracker merges request-side and response-side TelemetryLog entries
// by RequestID before emitting a unified record to the collector. This
// prevents the split-brain problem where independent request and response
// workers would produce two JSONL lines per request, double-counting
// requestsAudited and inflating AuditCoveragePercent.
//
// In open source mode (bidir=false), entries are emitted immediately from
// SubmitRequest — no map storage, no TTL overhead.
type SessionTracker struct {
	mu       sync.Mutex
	sessions map[uint64]*sessionEntry
	output   chan<- *TelemetryLog
	ttl      time.Duration
	bidir    bool
}

type sessionEntry struct {
	log       *TelemetryLog
	createdAt time.Time
}

// NewSessionTracker creates a tracker that emits merged TelemetryLog entries
// to output. When bidir is false (open source), SubmitRequest emits
// immediately. When bidir is true, entries are held until both request and
// response sides complete, or until TTL expires.
func NewSessionTracker(output chan<- *TelemetryLog, ttl time.Duration, bidir bool) *SessionTracker {
	return &SessionTracker{
		sessions: make(map[uint64]*sessionEntry),
		output:   output,
		ttl:      ttl,
		bidir:    bidir,
	}
}

// SubmitRequest writes request-side data into the tracker. In open source
// mode, the entry is emitted immediately to the output channel.
func (s *SessionTracker) SubmitRequest(entry *TelemetryLog) {
	if !s.bidir {
		// Open source mode — emit immediately, no map storage.
		select {
		case s.output <- entry:
		default:
		}
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.sessions[entry.RequestID]; ok {
		// Response arrived first (unusual but possible). Copy response
		// fields from the stored entry into the request entry, then emit
		// the request entry (which has all request-side fields intact).
		mergeResponse(entry, existing.log)
		s.emit(entry)
		delete(s.sessions, entry.RequestID)
	} else {
		s.sessions[entry.RequestID] = &sessionEntry{
			log:       entry,
			createdAt: time.Now(),
		}
	}
}

// SubmitResponse writes response-side data into the tracker. It looks up
// the existing request entry by RequestID, merges response fields, and
// emits the unified record. If no request entry exists yet, the response
// is stored for later merging.
func (s *SessionTracker) SubmitResponse(entry *TelemetryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.sessions[entry.RequestID]; ok {
		// Request already arrived — merge response fields and emit.
		mergeResponse(existing.log, entry)
		s.emit(existing.log)
		delete(s.sessions, entry.RequestID)
	} else {
		// Response arrived before request — store it. The request entry
		// submission will merge and emit.
		s.sessions[entry.RequestID] = &sessionEntry{
			log:       entry,
			createdAt: time.Now(),
		}
	}
}

// Start launches the background TTL sweeper goroutine. It evicts sessions
// older than TTL and emits them as-is (with ResponseCaptureComplete=false
// if the response side never arrived). Cancel the context to stop.
// Returns a channel that is closed when the sweeper has finished its final
// flush — callers must wait on this before closing the output channel.
func (s *SessionTracker) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if !s.bidir {
		close(done) // no sweeper needed in open source mode
		return done
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.sweep()
			case <-ctx.Done():
				// Final sweep: emit all remaining sessions.
				s.mu.Lock()
				for id, se := range s.sessions {
					s.emit(se.log)
					delete(s.sessions, id)
				}
				s.mu.Unlock()
				return
			}
		}
	}()

	return done
}

// sweep evicts sessions older than TTL, emitting them as-is.
func (s *SessionTracker) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, se := range s.sessions {
		if now.Sub(se.createdAt) > s.ttl {
			s.emit(se.log)
			delete(s.sessions, id)
		}
	}
}

// emit sends an entry to the output channel. Non-blocking.
// Must be called with s.mu held.
func (s *SessionTracker) emit(entry *TelemetryLog) {
	select {
	case s.output <- entry:
	default:
	}
}

// mergeResponse copies response-side fields from src into dst.
func mergeResponse(dst, src *TelemetryLog) {
	if src.ResponseRedactedPayload != "" {
		dst.ResponseRedactedPayload = src.ResponseRedactedPayload
	}
	if src.ResponseHash != "" {
		dst.ResponseHash = src.ResponseHash
	}
	if len(src.ResponsePIICategories) > 0 {
		dst.ResponsePIICategories = src.ResponsePIICategories
	}
	if src.ResponseCaptureComplete {
		dst.ResponseCaptureComplete = src.ResponseCaptureComplete
	}
	if src.ResponseExtractorUsed != "" {
		dst.ResponseExtractorUsed = src.ResponseExtractorUsed
	}
}
