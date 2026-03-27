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

func handler(delay time.Duration, chunks int, chunkDelay time.Duration) http.HandlerFunc {
	type request struct {
		Stream bool `json:"stream"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()

		preview := string(body)
		if len(preview) > 512 {
			preview = preview[:512] + "..."
		}
		log.Printf("%s %s Content-Length:%d body=%s", r.Method, r.URL.Path, r.ContentLength, preview)

		var req request
		json.Unmarshal(body, &req)

		time.Sleep(delay)

		if req.Stream {
			writeSSE(w, chunks, chunkDelay)
		} else {
			writeJSON(w)
		}
	}
}

func writeJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"id":      "chatcmpl-outband-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "mock-1",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]string{
				"role":    "assistant",
				"content": "This is a mock response from the outband mock LLM endpoint.",
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

func writeSSE(w http.ResponseWriter, chunks int, chunkDelay time.Duration) {
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
		resp, err := http.Get("http://localhost" + listen + "/health")
		if err != nil || resp.StatusCode != http.StatusOK {
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
