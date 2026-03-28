package outband

import (
	"context"
	"testing"
	"time"
)

func TestSessionTracker_OpenSourceMode(t *testing.T) {
	output := make(chan *TelemetryLog, 10)
	st := NewSessionTracker(output, time.Minute, false)

	entry := &TelemetryLog{RequestID: 1, RedactedPayload: "test"}
	st.SubmitRequest(entry)

	select {
	case got := <-output:
		if got.RequestID != 1 {
			t.Fatalf("expected request ID 1, got %d", got.RequestID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for emitted entry")
	}
}

func TestSessionTracker_BidirMerge(t *testing.T) {
	output := make(chan *TelemetryLog, 10)
	st := NewSessionTracker(output, time.Minute, true)

	reqEntry := &TelemetryLog{
		RequestID:       42,
		RedactedPayload: "request-data",
	}
	respEntry := &TelemetryLog{
		RequestID:               42,
		ResponseRedactedPayload: "response-data",
		ResponseCaptureComplete: true,
		ResponseExtractorUsed:   "openai-sse",
		ResponsePIICategories:   []string{"SSN"},
	}

	st.SubmitRequest(reqEntry)
	st.SubmitResponse(respEntry)

	select {
	case got := <-output:
		if got.RequestID != 42 {
			t.Fatalf("expected request ID 42, got %d", got.RequestID)
		}
		if got.RedactedPayload != "request-data" {
			t.Fatalf("expected request payload preserved, got %q", got.RedactedPayload)
		}
		if got.ResponseRedactedPayload != "response-data" {
			t.Fatalf("expected response payload merged, got %q", got.ResponseRedactedPayload)
		}
		if !got.ResponseCaptureComplete {
			t.Fatal("expected ResponseCaptureComplete = true")
		}
		if got.ResponseExtractorUsed != "openai-sse" {
			t.Fatalf("expected response extractor, got %q", got.ResponseExtractorUsed)
		}
		if len(got.ResponsePIICategories) != 1 || got.ResponsePIICategories[0] != "SSN" {
			t.Fatalf("expected response PII categories [SSN], got %v", got.ResponsePIICategories)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for merged entry")
	}

	// Verify only one entry was emitted (not two).
	select {
	case extra := <-output:
		t.Fatalf("unexpected extra entry: %+v", extra)
	default:
	}
}

func TestSessionTracker_ResponseFirst(t *testing.T) {
	output := make(chan *TelemetryLog, 10)
	st := NewSessionTracker(output, time.Minute, true)

	// Submit response before request.
	respEntry := &TelemetryLog{
		RequestID:               7,
		ResponseRedactedPayload: "resp",
		ResponseCaptureComplete: true,
	}
	st.SubmitResponse(respEntry)

	// No emission yet.
	select {
	case <-output:
		t.Fatal("should not emit until request arrives")
	default:
	}

	// Now submit request.
	reqEntry := &TelemetryLog{RequestID: 7, RedactedPayload: "req"}
	st.SubmitRequest(reqEntry)

	select {
	case got := <-output:
		if got.RedactedPayload != "req" {
			t.Fatalf("expected request payload 'req', got %q", got.RedactedPayload)
		}
		if got.ResponseRedactedPayload != "resp" {
			t.Fatalf("expected response payload 'resp', got %q", got.ResponseRedactedPayload)
		}
		if !got.ResponseCaptureComplete {
			t.Fatal("expected ResponseCaptureComplete=true after merge")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestSessionTracker_TTLEviction(t *testing.T) {
	output := make(chan *TelemetryLog, 10)
	st := NewSessionTracker(output, 50*time.Millisecond, true)

	ctx, cancel := context.WithCancel(context.Background())
	_ = st.Start(ctx)

	st.SubmitRequest(&TelemetryLog{RequestID: 99, RedactedPayload: "stale"})

	// Wait for TTL + sweep interval.
	select {
	case got := <-output:
		if got.RequestID != 99 {
			t.Fatalf("expected evicted request 99, got %d", got.RequestID)
		}
		if got.ResponseCaptureComplete {
			t.Fatal("evicted entry should have ResponseCaptureComplete=false")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for TTL eviction")
	}

	cancel()
}

func TestSessionTracker_NoDuplicateCount(t *testing.T) {
	output := make(chan *TelemetryLog, 100)
	st := NewSessionTracker(output, time.Minute, true)

	// Submit 10 request+response pairs.
	for i := uint64(1); i <= 10; i++ {
		st.SubmitRequest(&TelemetryLog{RequestID: i})
		st.SubmitResponse(&TelemetryLog{
			RequestID:               i,
			ResponseRedactedPayload: "resp",
			ResponseCaptureComplete: true,
		})
	}

	// Should get exactly 10 entries, not 20.
	count := 0
	for {
		select {
		case <-output:
			count++
		default:
			if count != 10 {
				t.Fatalf("expected 10 entries, got %d", count)
			}
			return
		}
	}
}
