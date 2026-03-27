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
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeBatch(n int) []*telemetryLog {
	batch := make([]*telemetryLog, n)
	for i := range batch {
		batch[i] = &telemetryLog{
			RequestID:          uint64(i),
			Timestamp:          time.Now(),
			OriginalHash:       "abc123",
			RedactedPayload:    `{"field":"value"}`,
			RedactedHash:       "def456",
			RedactionLevel:     "pattern-based",
			PIICategoriesFound: []string{"SSN"},
			ExtractorUsed:      "openai",
			FieldsScanned:      3,
			CaptureComplete:    true,
		}
	}
	return batch
}

func countJSONLFiles(dir string) (active int, rotated int) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if name == activeFileName {
			active++
		} else if strings.HasPrefix(name, rotatedPrefix) && strings.HasSuffix(name, rotatedSuffix) {
			rotated++
		}
	}
	return
}

func TestJSONLWriterBasicWrite(t *testing.T) {
	dir := t.TempDir()
	w, err := NewJSONLWriter(dir, 100*1024*1024, 24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	batch := makeBatch(5)
	if err := w.Flush(batch); err != nil {
		t.Fatal(err)
	}

	// Read back and verify each line is valid JSON.
	path := filepath.Join(dir, activeFileName)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		var entry telemetryLog
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", count, err)
		}
		count++
	}
	if count != 5 {
		t.Fatalf("got %d lines, want 5", count)
	}
}

func TestJSONLWriterSizeRotation(t *testing.T) {
	dir := t.TempDir()
	// Very small max size to trigger rotation.
	w, err := NewJSONLWriter(dir, 200, 24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Each batch should exceed 200 bytes total, triggering rotation.
	for i := 0; i < 5; i++ {
		if err := w.Flush(makeBatch(3)); err != nil {
			t.Fatal(err)
		}
	}

	active, rotated := countJSONLFiles(dir)
	if active != 1 {
		t.Fatalf("expected 1 active file, got %d", active)
	}
	if rotated < 1 {
		t.Fatal("expected at least 1 rotated file")
	}
}

func TestJSONLWriterAgeRotation(t *testing.T) {
	dir := t.TempDir()
	// 1ms max age — practically instant rotation.
	w, err := NewJSONLWriter(dir, 100*1024*1024, 1*time.Millisecond, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Flush(makeBatch(1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := w.Flush(makeBatch(1)); err != nil {
		t.Fatal(err)
	}

	_, rotated := countJSONLFiles(dir)
	if rotated < 1 {
		t.Fatal("expected at least 1 rotated file from age-based rotation")
	}
}

func TestJSONLWriterRetention(t *testing.T) {
	dir := t.TempDir()
	w, err := NewJSONLWriter(dir, 100, 24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Force multiple rotations.
	for i := 0; i < 10; i++ {
		if err := w.Flush(makeBatch(3)); err != nil {
			t.Fatal(err)
		}
		// Small sleep to ensure unique timestamps in filenames.
		time.Sleep(1 * time.Millisecond)
	}

	_, rotated := countJSONLFiles(dir)
	if rotated > 2 {
		t.Fatalf("retention failed: got %d rotated files, want <= 2", rotated)
	}
}

func TestJSONLWriterNoSplitAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	// Small enough that each batch triggers rotation.
	w, err := NewJSONLWriter(dir, 100, 24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Flush(makeBatch(3)); err != nil {
		t.Fatal(err)
	}
	// This batch should trigger rotation; all 3 records go to the new file.
	if err := w.Flush(makeBatch(3)); err != nil {
		t.Fatal(err)
	}

	// Verify the active file has exactly 3 lines (from the second batch).
	path := filepath.Join(dir, activeFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("active file has %d lines, want 3 (no split)", len(lines))
	}

	// Verify each line is valid JSON.
	for i, line := range lines {
		var entry telemetryLog
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("active file line %d: invalid JSON: %v", i, err)
		}
	}
}

func TestJSONLWriterEmptyBatch(t *testing.T) {
	dir := t.TempDir()
	w, err := NewJSONLWriter(dir, 100*1024*1024, 24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Flush(nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush([]*telemetryLog{}); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLWriterResume(t *testing.T) {
	dir := t.TempDir()

	// First writer: write a batch, close.
	w1, err := NewJSONLWriter(dir, 100*1024*1024, 24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.Flush(makeBatch(2)); err != nil {
		t.Fatal(err)
	}
	w1.Close()

	// Second writer: opens the same dir, resumes.
	w2, err := NewJSONLWriter(dir, 100*1024*1024, 24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Flush(makeBatch(3)); err != nil {
		t.Fatal(err)
	}
	w2.Close()

	// Should have 5 lines total in the active file.
	path := filepath.Join(dir, activeFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines after resume, want 5", len(lines))
	}
}

func TestJSONLWriterDiskError(t *testing.T) {
	// Write to a non-existent nested path that can't be created.
	_, err := NewJSONLWriter("/dev/null/impossible", 1024, 24*time.Hour, 10)
	if err == nil {
		t.Fatal("expected error for invalid directory")
	}
}
