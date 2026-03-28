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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func envDuration(key, fallback string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		v = fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("invalid duration %s=%q: %v", key, v, err)
	}
	return d
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("invalid integer %s=%q: %v", key, v, err)
	}
	return n
}

func truncateBody(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

func handler(delay time.Duration, chunks int, chunkDelay time.Duration) http.HandlerFunc {
	type request struct {
		Stream bool `json:"stream"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to read request body: %v", err), http.StatusBadRequest)
			return
		}

		log.Printf("REQ %s %s content_length=%d body_preview=%q",
			r.Method, r.URL.Path, r.ContentLength, truncateBody(body, 200))

		var req request
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
				return
			}
		}

		// X-Mock-Stream header overrides body stream field.
		if v := r.Header.Get("X-Mock-Stream"); v != "" {
			req.Stream = (v == "true")
		}

		// X-Mock-Response-PII injects PII into response content.
		injectPII := r.Header.Get("X-Mock-Response-PII") == "true"

		// X-Mock-Chunk-Delay overrides per-request chunk delay.
		perRequestChunkDelay := chunkDelay
		if v := r.Header.Get("X-Mock-Chunk-Delay"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				perRequestChunkDelay = d
			}
		}

		time.Sleep(delay)

		if req.Stream {
			writeSSE(w, chunks, perRequestChunkDelay, injectPII)
		} else {
			writeJSON(w, injectPII)
		}
	}
}

func writeJSON(w http.ResponseWriter, injectPII bool) {
	w.Header().Set("Content-Type", "application/json")

	content := "This is a mock response from the outband mock LLM endpoint."
	if injectPII {
		content += " Contact: jane.doe@example.com"
	}

	resp := map[string]any{
		"id":      "chatcmpl-outband-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "mock-1",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]string{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     10,
			"completion_tokens": 12,
			"total_tokens":      22,
		},
	}
	json.NewEncoder(w).Encode(resp)
}

func writeSSE(w http.ResponseWriter, chunks int, chunkDelay time.Duration, injectPII bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	tokens := []string{
		"This", " is", " a", " mock", " streaming", " response",
		" from", " the", " outband", " mock", " LLM", " endpoint", ".",
	}

	for i := 0; i < chunks; i++ {
		token := tokens[i%len(tokens)]

		// Inject PII into the last content chunk.
		if injectPII && i == chunks-1 {
			token += " Contact: jane.doe@example.com"
		}

		chunk := map[string]any{
			"id":      "chatcmpl-outband-mock",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "mock-1",
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]string{
					"content": token,
				},
			}},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()

		if i < chunks-1 {
			time.Sleep(chunkDelay)
		}
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-health" {
		listen := os.Getenv("MOCK_LISTEN")
		if listen == "" {
			listen = ":9090"
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://localhost" + listen + "/health")
		if err != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	listen := os.Getenv("MOCK_LISTEN")
	if listen == "" {
		listen = ":9090"
	}
	delay := envDuration("MOCK_DELAY", "200ms")
	chunks := envInt("MOCK_CHUNKS", 5)
	chunkDelay := envDuration("MOCK_CHUNK_DELAY", "50ms")

	server := &http.Server{
		Addr:    listen,
		Handler: handler(delay, chunks, chunkDelay),
	}

	log.Printf("mock LLM listening on %s (delay=%s, chunks=%d, chunk_delay=%s)", listen, delay, chunks, chunkDelay)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
}
