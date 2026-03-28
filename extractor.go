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
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/buger/jsonparser"
)

// ---------------------------------------------------------------------------
// Extractor registry for enterprise response-side extractors
// ---------------------------------------------------------------------------

var (
	extractorRegistryMu sync.RWMutex
	extractorRegistry   = map[Direction]map[string]PayloadExtractor{}
)

// RegisterExtractor registers a PayloadExtractor for the given direction
// and API format name (e.g., "openai", "anthropic"). Multiple extractors
// can coexist per direction — one per API format.
func RegisterExtractor(dir Direction, name string, ext PayloadExtractor) {
	if ext == nil {
		panic(fmt.Sprintf("RegisterExtractor: nil extractor for direction %d, name %q", dir, name))
	}
	extractorRegistryMu.Lock()
	defer extractorRegistryMu.Unlock()
	if extractorRegistry[dir] == nil {
		extractorRegistry[dir] = make(map[string]PayloadExtractor)
	}
	extractorRegistry[dir][name] = ext
}

// ExtractorForDirection returns the registered extractor for the given
// direction and API format name, or nil if none is registered.
func ExtractorForDirection(dir Direction, name string) PayloadExtractor {
	extractorRegistryMu.RLock()
	defer extractorRegistryMu.RUnlock()
	if m, ok := extractorRegistry[dir]; ok {
		return m[name]
	}
	return nil
}

// ContentField represents a single natural-language text value extracted
// from a structured JSON payload. Only content fields are extracted;
// structural fields (model, IDs, temperatures) are never returned.
type ContentField struct {
	Text  string // extracted natural language content
	Path  string // JSON path for audit trail, e.g. "messages[0].content"
	Index int    // extraction order
}

// PayloadExtractor extracts natural-language content fields from a raw
// JSON (or SSE-framed) payload. Implementations are stateless and safe
// for concurrent use.
type PayloadExtractor interface {
	ExtractContent(data []byte) []ContentField
}

// openAIExtractor extracts content from OpenAI chat completion request
// format and SSE streamed responses.
type openAIExtractor struct{}

// anthropicExtractor extracts content from Anthropic Messages API request
// format and SSE streamed responses.
type anthropicExtractor struct{}

// extractorForAPI returns the appropriate extractor for the given API type.
// Returns nil for unknown types.
func extractorForAPI(t apiType) PayloadExtractor {
	switch t {
	case apiTypeOpenAI:
		return &openAIExtractor{}
	case apiTypeAnthropic:
		return &anthropicExtractor{}
	default:
		return nil
	}
}

func extractorName(t apiType) string {
	switch t {
	case apiTypeOpenAI:
		return "openai"
	case apiTypeAnthropic:
		return "anthropic"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// SSE handling
// ---------------------------------------------------------------------------

// sseDataPrefix is the Server-Sent Events data line prefix.
var sseDataPrefix = []byte("data: ")

// isSSE detects whether a payload is SSE-framed by checking if it begins
// with "data: " or contains the pattern after a leading newline.
func isSSE(data []byte) bool {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	return bytes.HasPrefix(trimmed, sseDataPrefix)
}

// splitSSEChunks extracts the JSON body from each "data: " line in an SSE
// stream. Lines containing "[DONE]" are skipped.
func splitSSEChunks(data []byte) [][]byte {
	var chunks [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, sseDataPrefix) {
			continue
		}
		payload := line[len(sseDataPrefix):]
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		chunks = append(chunks, payload)
	}
	return chunks
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// extractContentParts extracts text from a JSON content array (multimodal
// format). Only objects with "type":"text" are returned. pathPrefix is used
// to construct the Path field (e.g. "messages[2].content").
func extractContentParts(contentArray []byte, pathPrefix string, startIndex int) []ContentField {
	var fields []ContentField
	partIdx := 0
	jsonparser.ArrayEach(contentArray, func(part []byte, dataType jsonparser.ValueType, _ int, err error) {
		if err != nil {
			partIdx++
			return
		}
		partType, _ := jsonparser.GetString(part, "type")
		if partType == "text" {
			text, tErr := jsonparser.GetString(part, "text")
			if tErr == nil && text != "" {
				fields = append(fields, ContentField{
					Text:  text,
					Path:  fmt.Sprintf("%s[%d].text", pathPrefix, partIdx),
					Index: startIndex + len(fields),
				})
			}
		}
		partIdx++
	})
	return fields
}

// extractMessageContent extracts content from a single message object.
// Handles both string content and array-of-content-blocks.
func extractMessageContent(msg []byte, msgIdx int, startIndex int) []ContentField {
	content, contentType, _, contentErr := jsonparser.Get(msg, "content")
	if contentErr != nil || contentType == jsonparser.Null || contentType == jsonparser.NotExist {
		return nil
	}

	path := fmt.Sprintf("messages[%d].content", msgIdx)

	switch contentType {
	case jsonparser.String:
		text, err := jsonparser.GetString(msg, "content")
		if err == nil && text != "" {
			return []ContentField{{
				Text:  text,
				Path:  path,
				Index: startIndex,
			}}
		}
	case jsonparser.Array:
		return extractContentParts(content, path, startIndex)
	}
	return nil
}

// extractMessagesArray iterates over messages[*] and extracts content fields.
func extractMessagesArray(data []byte, startIndex int) []ContentField {
	var fields []ContentField
	msgIdx := 0
	jsonparser.ArrayEach(data, func(msg []byte, dataType jsonparser.ValueType, _ int, err error) {
		if err != nil {
			msgIdx++
			return
		}
		extracted := extractMessageContent(msg, msgIdx, startIndex+len(fields))
		fields = append(fields, extracted...)
		msgIdx++
	}, "messages")
	return fields
}

// ---------------------------------------------------------------------------
// OpenAI extractor
// ---------------------------------------------------------------------------

func (e *openAIExtractor) ExtractContent(data []byte) []ContentField {
	if isSSE(data) {
		return e.extractSSE(data)
	}
	return extractMessagesArray(data, 0)
}

// extractSSE processes an OpenAI SSE stream. It concatenates all delta
// content strings from choices[*].delta.content across all chunks into a
// single ContentField per choice index. This ensures PII spanning chunk
// boundaries is captured as a contiguous string.
func (e *openAIExtractor) extractSSE(data []byte) []ContentField {
	chunks := splitSSEChunks(data)
	// Accumulate delta text per choice index.
	deltas := make(map[int]*strings.Builder)

	for _, chunk := range chunks {
		arrayPos := 0
		jsonparser.ArrayEach(chunk, func(choice []byte, _ jsonparser.ValueType, _ int, err error) {
			if err != nil {
				arrayPos++
				return
			}
			// Use the explicit "index" field if present; fall back to array position.
			idx := arrayPos
			if explicitIdx, idxErr := jsonparser.GetInt(choice, "index"); idxErr == nil {
				idx = int(explicitIdx)
			}
			delta, dErr := jsonparser.GetString(choice, "delta", "content")
			if dErr == nil && delta != "" {
				b, ok := deltas[idx]
				if !ok {
					b = &strings.Builder{}
					deltas[idx] = b
				}
				b.WriteString(delta)
			}
			arrayPos++
		}, "choices")
	}

	keys := make([]int, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	var fields []ContentField
	for _, k := range keys {
		if b := deltas[k]; b.Len() > 0 {
			fields = append(fields, ContentField{
				Text:  b.String(),
				Path:  fmt.Sprintf("choices[%d].delta.content", k),
				Index: len(fields),
			})
		}
	}
	return fields
}

// ---------------------------------------------------------------------------
// Anthropic extractor
// ---------------------------------------------------------------------------

func (e *anthropicExtractor) ExtractContent(data []byte) []ContentField {
	if isSSE(data) {
		return e.extractSSE(data)
	}
	var fields []ContentField
	fields = append(fields, e.extractSystem(data, 0)...)
	fields = append(fields, extractMessagesArray(data, len(fields))...)
	return fields
}

// extractSystem extracts the top-level "system" field, which can be a
// string or an array of content blocks.
func (e *anthropicExtractor) extractSystem(data []byte, startIndex int) []ContentField {
	systemVal, systemType, _, systemErr := jsonparser.Get(data, "system")
	if systemErr != nil || systemType == jsonparser.Null || systemType == jsonparser.NotExist {
		return nil
	}

	switch systemType {
	case jsonparser.String:
		text, err := jsonparser.GetString(data, "system")
		if err == nil && text != "" {
			return []ContentField{{
				Text:  text,
				Path:  "system",
				Index: startIndex,
			}}
		}
	case jsonparser.Array:
		return extractContentParts(systemVal, "system", startIndex)
	}
	return nil
}

// extractSSE processes an Anthropic SSE stream. Concatenates all
// content_block_delta text deltas per content block index.
func (e *anthropicExtractor) extractSSE(data []byte) []ContentField {
	chunks := splitSSEChunks(data)
	deltas := make(map[int]*strings.Builder)

	for _, chunk := range chunks {
		eventType, _ := jsonparser.GetString(chunk, "type")
		if eventType != "content_block_delta" {
			continue
		}
		idx, err := jsonparser.GetInt(chunk, "index")
		if err != nil {
			continue
		}
		deltaText, err := jsonparser.GetString(chunk, "delta", "text")
		if err != nil || deltaText == "" {
			continue
		}
		b, ok := deltas[int(idx)]
		if !ok {
			b = &strings.Builder{}
			deltas[int(idx)] = b
		}
		b.WriteString(deltaText)
	}

	keys := make([]int, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	var fields []ContentField
	for _, k := range keys {
		if b := deltas[k]; b.Len() > 0 {
			fields = append(fields, ContentField{
				Text:  b.String(),
				Path:  fmt.Sprintf("content_block[%d].text", k),
				Index: len(fields),
			})
		}
	}
	return fields
}
