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

// testChain returns a RedactorChain with only the RegexRedactor, matching
// the open-source binary configuration.
func testChain() *RedactorChain {
	return &RedactorChain{redactors: []Redactor{NewRegexRedactor()}}
}

func TestRedactSSN(t *testing.T) {
	text, cats := testChain().Redact("My SSN is 123-45-6789")
	want := "My SSN is [REDACTED:SSN:CC6.1]"
	if text != want {
		t.Errorf("got %q, want %q", text, want)
	}
	assertCategory(t, cats, PIISSN)
}

func TestRedactCreditCardVisa(t *testing.T) {
	// 4532015112830366 is Luhn-valid.
	text, cats := testChain().Redact("Card: 4532015112830366")
	if !strings.Contains(text, "[REDACTED:CC_NUMBER:CC6.1]") {
		t.Errorf("Visa not redacted: %q", text)
	}
	assertCategory(t, cats, PIICC)
}

func TestRedactCreditCardVisaWithSeparators(t *testing.T) {
	text, _ := testChain().Redact("Card: 4532-0151-1283-0366")
	if !strings.Contains(text, "[REDACTED:CC_NUMBER:CC6.1]") {
		t.Errorf("Visa with dashes not redacted: %q", text)
	}
}

func TestRedactCreditCardAmex(t *testing.T) {
	// 378282246310005 is a known Amex test number (Luhn-valid).
	text, _ := testChain().Redact("Card: 378282246310005")
	if !strings.Contains(text, "[REDACTED:CC_NUMBER:CC6.1]") {
		t.Errorf("Amex not redacted: %q", text)
	}
}

func TestRedactCreditCardFailsLuhn(t *testing.T) {
	// Change last digit to make Luhn fail.
	text, _ := testChain().Redact("Number: 4532015112830367")
	if strings.Contains(text, "REDACTED") {
		t.Errorf("false positive: Luhn-invalid number was redacted: %q", text)
	}
}

func TestRedactEmail(t *testing.T) {
	text, cats := testChain().Redact("Email me at test@example.com please")
	want := "Email me at [REDACTED:EMAIL:CC6.1] please"
	if text != want {
		t.Errorf("got %q, want %q", text, want)
	}
	assertCategory(t, cats, PIIEmail)
}

func TestRedactEmailSubdomain(t *testing.T) {
	text, _ := testChain().Redact("Contact: user@mail.corp.example.com")
	if !strings.Contains(text, "[REDACTED:EMAIL:CC6.1]") {
		t.Errorf("email with subdomain not redacted: %q", text)
	}
}

func TestRedactPhoneFormats(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"parens-dash", "Call (555) 123-4567"},
		{"dashes", "Call 555-123-4567"},
		{"dots", "Call 555.123.4567"},
		{"country-code", "Call +1 555 123 4567"},
		{"country-code-dashes", "Call +1-555-123-4567"},
	}
	chain := testChain()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, _ := chain.Redact(tt.input)
			if !strings.Contains(text, "[REDACTED:PHONE:CC6.1]") {
				t.Errorf("phone not redacted: %q", text)
			}
		})
	}
}

func TestRedactIPv4(t *testing.T) {
	text, cats := testChain().Redact("Server at 192.168.1.1 is running")
	want := "Server at [REDACTED:IP_ADDRESS:CC6.1] is running"
	if text != want {
		t.Errorf("got %q, want %q", text, want)
	}
	assertCategory(t, cats, PIIIPv4)
}

func TestRedactIPv4InvalidOctet(t *testing.T) {
	text, _ := testChain().Redact("Not IP: 999.999.999.999")
	if strings.Contains(text, "REDACTED") {
		t.Errorf("invalid IP was redacted: %q", text)
	}
}

func TestRedactIPv4LeadingZero(t *testing.T) {
	text, _ := testChain().Redact("Not IP: 01.02.03.04")
	if strings.Contains(text, "REDACTED") {
		t.Errorf("leading-zero IP was redacted: %q", text)
	}
}

func TestRedactMultiplePII(t *testing.T) {
	input := "SSN: 123-45-6789, email: test@example.com, call (555) 123-4567"
	text, cats := testChain().Redact(input)

	if !strings.Contains(text, "[REDACTED:SSN:CC6.1]") {
		t.Errorf("SSN not redacted: %q", text)
	}
	if !strings.Contains(text, "[REDACTED:EMAIL:CC6.1]") {
		t.Errorf("email not redacted: %q", text)
	}
	if !strings.Contains(text, "[REDACTED:PHONE:CC6.1]") {
		t.Errorf("phone not redacted: %q", text)
	}
	if len(cats) != 3 {
		t.Errorf("got %d categories, want 3", len(cats))
	}
}

func TestRedactNoPII(t *testing.T) {
	input := "The quick brown fox jumps over the lazy dog"
	text, cats := testChain().Redact(input)
	if text != input {
		t.Errorf("clean text was modified: %q", text)
	}
	if len(cats) != 0 {
		t.Errorf("got %d categories, want 0", len(cats))
	}
}

func TestRedactEmptyString(t *testing.T) {
	text, _ := testChain().Redact("")
	if text != "" {
		t.Errorf("empty string was modified: %q", text)
	}
}

// ---------------------------------------------------------------------------
// Redactor interface tests
// ---------------------------------------------------------------------------

func TestRegexRedactorName(t *testing.T) {
	r := NewRegexRedactor()
	if r.Name() != "pattern-based" {
		t.Errorf("Name() = %q, want %q", r.Name(), "pattern-based")
	}
}

func TestRedactorChainName(t *testing.T) {
	chain := &RedactorChain{redactors: []Redactor{NewRegexRedactor()}}
	if chain.Name() != "pattern-based" {
		t.Errorf("single-redactor chain Name() = %q, want %q", chain.Name(), "pattern-based")
	}
}

func TestRedactorChainNameMultiple(t *testing.T) {
	chain := &RedactorChain{redactors: []Redactor{
		NewRegexRedactor(),
		&mockRedactor{name: "nlp-enhanced"},
	}}
	want := "pattern-based, nlp-enhanced"
	if chain.Name() != want {
		t.Errorf("multi-redactor chain Name() = %q, want %q", chain.Name(), want)
	}
}

func TestRedactorChainDeduplicatesCategories(t *testing.T) {
	// Both redactors find EMAIL — the category should appear once.
	chain := &RedactorChain{redactors: []Redactor{
		NewRegexRedactor(),
		&mockRedactor{
			name: "nlp-enhanced",
			redactFunc: func(input string) (string, []PIICategory) {
				// Simulate NLP also flagging EMAIL on already-redacted text.
				return input, []PIICategory{PIIEmail}
			},
		},
	}}

	_, cats := chain.Redact("Email me at test@example.com")

	emailCount := 0
	for _, c := range cats {
		if c == PIIEmail {
			emailCount++
		}
	}
	if emailCount != 1 {
		t.Errorf("EMAIL appears %d times in categories, want 1", emailCount)
	}
}

func TestRedactorChainSequential(t *testing.T) {
	// Second redactor should see the output of the first (markers, not raw PII).
	var secondSaw string
	chain := &RedactorChain{redactors: []Redactor{
		NewRegexRedactor(),
		&mockRedactor{
			name: "inspector",
			redactFunc: func(input string) (string, []PIICategory) {
				secondSaw = input
				return input, nil
			},
		},
	}}

	chain.Redact("My SSN is 123-45-6789")

	if !strings.Contains(secondSaw, "[REDACTED:SSN:CC6.1]") {
		t.Errorf("second redactor saw raw PII instead of marker: %q", secondSaw)
	}
	if strings.Contains(secondSaw, "123-45-6789") {
		t.Errorf("second redactor saw raw SSN: %q", secondSaw)
	}
}

// mockRedactor is a test double that implements the Redactor interface.
type mockRedactor struct {
	name       string
	redactFunc func(input string) (string, []PIICategory)
}

func (m *mockRedactor) Redact(input string) (string, []PIICategory) {
	if m.redactFunc != nil {
		return m.redactFunc(input)
	}
	return input, nil
}

func (m *mockRedactor) Name() string { return m.name }

// ---------------------------------------------------------------------------
// Luhn validation tests
// ---------------------------------------------------------------------------

func TestLuhnValid(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"4532015112830366", true},       // Visa
		{"5425233430109903", true},       // Mastercard
		{"378282246310005", true},        // Amex
		{"4532-0151-1283-0366", true},    // with dashes
		{"4532 0151 1283 0366", true},    // with spaces
		{"4532015112830367", false},      // last digit wrong
		{"1234567890", false},            // too short
		{"12345678901234567890", false},  // too long
		{"", false},                      // empty
		{"abcdefghijklmnop", false},      // no digits
	}
	for _, tt := range tests {
		if got := luhnValid(tt.input); got != tt.valid {
			t.Errorf("luhnValid(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}

// ---------------------------------------------------------------------------
// IPv4 validation tests
// ---------------------------------------------------------------------------

func TestValidateIPv4(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"192.168.1.1", true},
		{"0.0.0.0", true},
		{"255.255.255.255", true},
		{"10.0.0.1", true},
		{"256.1.1.1", false},      // octet > 255
		{"01.02.03.04", false},    // leading zeros
		{"1.2.3", false},          // only 3 octets
		{"1.2.3.4.5", false},     // 5 octets
		{"999.999.999.999", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := validateIPv4(tt.input); got != tt.valid {
			t.Errorf("validateIPv4(%q) = %v, want %v", tt.input, got, tt.valid)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: workspace_id must NOT trigger CC redaction
// ---------------------------------------------------------------------------

func TestWorkspaceIDNotRedacted(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-4",
		"workspace_id": "4532015112830366",
		"messages": [
			{"role": "user", "content": "Hello, no PII here"}
		]
	}`)

	ext := &openAIExtractor{}
	fields := ext.ExtractContent(payload)

	chain := testChain()
	for _, f := range fields {
		text, _ := chain.Redact(f.Text)
		if strings.Contains(text, "REDACTED") {
			t.Errorf("field %q was redacted but should not be: %s", f.Path, text)
		}
	}

	// Also verify the workspace_id was never extracted.
	for _, f := range fields {
		if strings.Contains(f.Text, "4532015112830366") {
			t.Errorf("workspace_id leaked into content field %q", f.Path)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertCategory(t *testing.T, cats []PIICategory, want PIICategory) {
	t.Helper()
	for _, c := range cats {
		if c == want {
			return
		}
	}
	t.Errorf("category %q not found in %v", want, cats)
}
