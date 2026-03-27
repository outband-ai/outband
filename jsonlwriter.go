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
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	activeFileName  = "outband-telemetry-current.jsonl"
	rotatedPrefix   = "outband-telemetry-"
	rotatedSuffix   = ".jsonl"
	initialAvgEntry = 512
)

// JSONLWriter writes telemetry logs as one JSON object per line to a file
// with size-based and age-based rotation. Rotation executes synchronously
// inside Flush before any bytes from the new batch are written — closing
// the file descriptor mid-write causes fs.ErrClosed panics.
type JSONLWriter struct {
	dir          string
	maxSize      int64
	maxAge       time.Duration
	maxFiles     int
	file         *os.File
	currentBytes int64
	fileCreated  time.Time
	avgEntrySize int64
}

// NewJSONLWriter creates a JSONL writer that outputs to dir. The directory
// is created if it does not exist. If a current file already exists, it is
// resumed (appended to) with the existing size tracked.
func NewJSONLWriter(dir string, maxSize int64, maxAge time.Duration, maxFiles int) (*JSONLWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	path := filepath.Join(dir, activeFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	created := info.ModTime()
	if info.Size() == 0 {
		created = time.Now()
	}

	return &JSONLWriter{
		dir:          dir,
		maxSize:      maxSize,
		maxAge:       maxAge,
		maxFiles:     maxFiles,
		file:         f,
		currentBytes: info.Size(),
		fileCreated:  created,
		avgEntrySize: initialAvgEntry,
	}, nil
}

// Flush serializes and appends a batch of telemetry logs. It performs
// synchronous pre-write rotation if needed: the entire triggering batch
// goes to the new file, never split across files.
func (w *JSONLWriter) Flush(batch []*telemetryLog) error {
	if len(batch) == 0 {
		return nil
	}

	batchEstimate := int64(len(batch)) * w.avgEntrySize

	if w.needsRotation(batchEstimate) {
		if err := w.rotate(); err != nil {
			return err
		}
	}

	var totalWritten int64
	for _, entry := range batch {
		line, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		n, err := w.file.Write(line)
		totalWritten += int64(n)
		if err != nil {
			// Update currentBytes with partial writes so needsRotation
			// sees the correct on-disk size on the next call.
			w.currentBytes += totalWritten
			return err
		}
	}

	if len(batch) > 0 && totalWritten > 0 {
		actualAvg := totalWritten / int64(len(batch))
		w.avgEntrySize = (w.avgEntrySize + actualAvg) / 2
	}
	w.currentBytes += totalWritten
	return nil
}

// Close flushes and closes the current file.
func (w *JSONLWriter) Close() error {
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

func (w *JSONLWriter) needsRotation(batchEstimate int64) bool {
	if w.currentBytes+batchEstimate > w.maxSize {
		return true
	}
	if time.Since(w.fileCreated) > w.maxAge {
		return true
	}
	return false
}

func (w *JSONLWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}

	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	oldPath := filepath.Join(w.dir, activeFileName)
	newPath := filepath.Join(w.dir, rotatedPrefix+ts+rotatedSuffix)
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}

	f, err := os.OpenFile(
		filepath.Join(w.dir, activeFileName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0o600,
	)
	if err != nil {
		return err
	}

	w.file = f
	w.currentBytes = 0
	w.fileCreated = time.Now()

	return w.enforceRetention()
}

func (w *JSONLWriter) enforceRetention() error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}

	var rotated []string
	for _, e := range entries {
		name := e.Name()
		if name == activeFileName {
			continue
		}
		if strings.HasPrefix(name, rotatedPrefix) && strings.HasSuffix(name, rotatedSuffix) {
			rotated = append(rotated, name)
		}
	}

	if len(rotated) <= w.maxFiles {
		return nil
	}

	sort.Strings(rotated) // timestamp in name → chronological order
	toDelete := rotated[:len(rotated)-w.maxFiles]
	for _, name := range toDelete {
		if err := os.Remove(filepath.Join(w.dir, name)); err != nil {
			return err
		}
	}
	return nil
}
