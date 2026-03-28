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
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// OpenAI extractor tests
// ---------------------------------------------------------------------------

func TestOpenAIBasicExtraction(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant"},
			{"role": "user", "content": "Hello world"}
		],
		"temperature": 0.7
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 2 {
		t.Fatalf("got %d fields, want 2", len(fields))
	}
	if fields[0].Text != "You are a helpful assistant" {
		t.Errorf("field[0].Text = %q", fields[0].Text)
	}
	if fields[0].Path != "messages[0].content" {
		t.Errorf("field[0].Path = %q", fields[0].Path)
	}
	if fields[1].Text != "Hello world" {
		t.Errorf("field[1].Text = %q", fields[1].Text)
	}
	if fields[1].Path != "messages[1].content" {
		t.Errorf("field[1].Path = %q", fields[1].Path)
	}
}

func TestOpenAIMultimodalContent(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-4-vision",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "describe this image"},
				{"type": "image_url", "image_url": {"url": "https://example.com/img.png"}}
			]}
		]
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1 (only text parts)", len(fields))
	}
	if fields[0].Text != "describe this image" {
		t.Errorf("field[0].Text = %q", fields[0].Text)
	}
	if fields[0].Path != "messages[0].content[0].text" {
		t.Errorf("field[0].Path = %q", fields[0].Path)
	}
}

func TestOpenAINullContent(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "assistant", "content": null, "tool_calls": []}
		]
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 0 {
		t.Fatalf("got %d fields, want 0", len(fields))
	}
}

func TestOpenAIMissingContent(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "assistant", "tool_calls": []}
		]
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 0 {
		t.Fatalf("got %d fields, want 0", len(fields))
	}
}

func TestOpenAIEmptyMessages(t *testing.T) {
	payload := []byte(`{"model": "gpt-4", "messages": []}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 0 {
		t.Fatalf("got %d fields, want 0", len(fields))
	}
}

func TestOpenAIMalformedJSON(t *testing.T) {
	payload := []byte(`{not valid json`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 0 {
		t.Fatalf("got %d fields, want 0 (malformed JSON)", len(fields))
	}
}

func TestOpenAIEscapedContent(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": "line1\nline2\t\"quoted\""}
		]
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1", len(fields))
	}
	if fields[0].Text != "line1\nline2\t\"quoted\"" {
		t.Errorf("field[0].Text = %q, want escaped content", fields[0].Text)
	}
}

func TestOpenAIEmptyStringContent(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": ""}
		]
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 0 {
		t.Fatalf("got %d fields, want 0 (empty string)", len(fields))
	}
}

func TestOpenAIContentArrayNoText(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "image_url", "image_url": {"url": "https://example.com/img.png"}}
			]}
		]
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 0 {
		t.Fatalf("got %d fields, want 0", len(fields))
	}
}

// ---------------------------------------------------------------------------
// Anthropic extractor tests
// ---------------------------------------------------------------------------

func TestAnthropicBasicExtraction(t *testing.T) {
	payload := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"system": "You are helpful",
		"messages": [
			{"role": "user", "content": "My email is test@example.com"}
		],
		"max_tokens": 1024
	}`)

	ext := &anthropicExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 2 {
		t.Fatalf("got %d fields, want 2", len(fields))
	}
	if fields[0].Text != "You are helpful" {
		t.Errorf("field[0].Text = %q", fields[0].Text)
	}
	if fields[0].Path != "system" {
		t.Errorf("field[0].Path = %q", fields[0].Path)
	}
	if fields[1].Text != "My email is test@example.com" {
		t.Errorf("field[1].Text = %q", fields[1].Text)
	}
}

func TestAnthropicSystemArray(t *testing.T) {
	payload := []byte(`{
		"system": [
			{"type": "text", "text": "Be helpful"},
			{"type": "text", "text": "Be concise"}
		],
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	ext := &anthropicExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(fields))
	}
	if fields[0].Text != "Be helpful" || fields[0].Path != "system[0].text" {
		t.Errorf("field[0] = %+v", fields[0])
	}
	if fields[1].Text != "Be concise" || fields[1].Path != "system[1].text" {
		t.Errorf("field[1] = %+v", fields[1])
	}
	if fields[2].Text != "Hello" || fields[2].Path != "messages[0].content" {
		t.Errorf("field[2] = %+v", fields[2])
	}
}

func TestAnthropicNoSystem(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	ext := &anthropicExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1", len(fields))
	}
	if fields[0].Text != "Hello" {
		t.Errorf("field[0].Text = %q", fields[0].Text)
	}
}

func TestAnthropicNullSystem(t *testing.T) {
	payload := []byte(`{
		"system": null,
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	ext := &anthropicExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1", len(fields))
	}
}

func TestAnthropicContentArray(t *testing.T) {
	payload := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "describe this"},
				{"type": "image", "source": {"type": "base64"}}
			]}
		]
	}`)

	ext := &anthropicExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1 (only text blocks)", len(fields))
	}
	if fields[0].Text != "describe this" {
		t.Errorf("field[0].Text = %q", fields[0].Text)
	}
}

// ---------------------------------------------------------------------------
// Structural field exclusion test (critical: workspace_id must NOT be extracted)
// ---------------------------------------------------------------------------

func TestExtractorContentOnlyNotStructural(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-4",
		"workspace_id": "4532015112830366",
		"temperature": 0.7,
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "Hello, no PII here"}
		]
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	for _, f := range fields {
		if strings.Contains(f.Text, "4532015112830366") {
			t.Errorf("workspace_id leaked into content field %q: %s", f.Path, f.Text)
		}
		if strings.Contains(f.Text, "gpt-4") {
			t.Errorf("model name leaked into content field %q: %s", f.Path, f.Text)
		}
	}
}

// ---------------------------------------------------------------------------
// SSE tests
// ---------------------------------------------------------------------------

func TestSplitSSEChunks(t *testing.T) {
	data := []byte("data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n")
	chunks := splitSSEChunks(data)

	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if string(chunks[0]) != `{"a":1}` {
		t.Errorf("chunk[0] = %q", string(chunks[0]))
	}
	if string(chunks[1]) != `{"b":2}` {
		t.Errorf("chunk[1] = %q", string(chunks[1]))
	}
}

func TestOpenAIExtractorSSEStream(t *testing.T) {
	data := []byte(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(data)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1 (concatenated deltas)", len(fields))
	}
	if fields[0].Text != "Hello world" {
		t.Errorf("field[0].Text = %q, want %q", fields[0].Text, "Hello world")
	}
}

func TestAnthropicExtractorSSEStream(t *testing.T) {
	data := []byte(
		"data: {\"type\":\"content_block_start\",\"index\":0}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"text\":\"Hello\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"text\":\" there\"}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)

	ext := &anthropicExtractor{}
	fields := ext.ExtractContent(data)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1", len(fields))
	}
	if fields[0].Text != "Hello there" {
		t.Errorf("field[0].Text = %q, want %q", fields[0].Text, "Hello there")
	}
}

func TestExtractorSSEWithPIIAcrossChunks(t *testing.T) {
	// SSN "123-45-6789" split across two SSE data chunks.
	data := []byte(
		"data: {\"choices\":[{\"delta\":{\"content\":\"SSN: 123-45-\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"6789 end\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(data)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1", len(fields))
	}
	// The concatenated text must contain the full SSN for regex to catch it.
	if fields[0].Text != "SSN: 123-45-6789 end" {
		t.Errorf("field[0].Text = %q, want %q", fields[0].Text, "SSN: 123-45-6789 end")
	}
}

func TestExtractorNonSSEPayload(t *testing.T) {
	// Standard JSON request body — not SSE.
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1", len(fields))
	}
	if fields[0].Text != "hello" {
		t.Errorf("field[0].Text = %q", fields[0].Text)
	}
}

func TestIsSSE(t *testing.T) {
	tests := []struct {
		input []byte
		want  bool
	}{
		{[]byte("data: {}\n\n"), true},
		{[]byte("\ndata: {}\n\n"), true},
		{[]byte(`{"messages":[]}`), false},
		{[]byte(""), false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := isSSE(tt.input); got != tt.want {
			t.Errorf("isSSE(%q) = %v, want %v", string(tt.input), got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// extractorForAPI
// ---------------------------------------------------------------------------

func TestExtractorForAPI(t *testing.T) {
	if extractorForAPI(apiTypeOpenAI) == nil {
		t.Error("expected non-nil for apiTypeOpenAI")
	}
	if extractorForAPI(apiTypeAnthropic) == nil {
		t.Error("expected non-nil for apiTypeAnthropic")
	}
	if extractorForAPI(apiTypeUnknown) != nil {
		t.Error("expected nil for apiTypeUnknown")
	}
}
