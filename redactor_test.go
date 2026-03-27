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
	"strings"
	"testing"
)

func TestRedactSSN(t *testing.T) {
	r := redactText("My SSN is 123-45-6789")
	want := "My SSN is [REDACTED:SSN:CC6.1]"
	if r.text != want {
		t.Errorf("got %q, want %q", r.text, want)
	}
	assertCategory(t, r.categories, piiSSN)
}

func TestRedactCreditCardVisa(t *testing.T) {
	// 4532015112830366 is Luhn-valid.
	r := redactText("Card: 4532015112830366")
	if !strings.Contains(r.text, "[REDACTED:CC_NUMBER:CC6.1]") {
		t.Errorf("Visa not redacted: %q", r.text)
	}
	assertCategory(t, r.categories, piiCC)
}

func TestRedactCreditCardVisaWithSeparators(t *testing.T) {
	r := redactText("Card: 4532-0151-1283-0366")
	if !strings.Contains(r.text, "[REDACTED:CC_NUMBER:CC6.1]") {
		t.Errorf("Visa with dashes not redacted: %q", r.text)
	}
}

func TestRedactCreditCardAmex(t *testing.T) {
	// 378282246310005 is a known Amex test number (Luhn-valid).
	r := redactText("Card: 378282246310005")
	if !strings.Contains(r.text, "[REDACTED:CC_NUMBER:CC6.1]") {
		t.Errorf("Amex not redacted: %q", r.text)
	}
}

func TestRedactCreditCardFailsLuhn(t *testing.T) {
	// Change last digit to make Luhn fail.
	r := redactText("Number: 4532015112830367")
	if strings.Contains(r.text, "REDACTED") {
		t.Errorf("false positive: Luhn-invalid number was redacted: %q", r.text)
	}
}

func TestRedactEmail(t *testing.T) {
	r := redactText("Email me at test@example.com please")
	want := "Email me at [REDACTED:EMAIL:CC6.1] please"
	if r.text != want {
		t.Errorf("got %q, want %q", r.text, want)
	}
	assertCategory(t, r.categories, piiEmail)
}

func TestRedactEmailSubdomain(t *testing.T) {
	r := redactText("Contact: user@mail.corp.example.com")
	if !strings.Contains(r.text, "[REDACTED:EMAIL:CC6.1]") {
		t.Errorf("email with subdomain not redacted: %q", r.text)
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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := redactText(tt.input)
			if !strings.Contains(r.text, "[REDACTED:PHONE:CC6.1]") {
				t.Errorf("phone not redacted: %q", r.text)
			}
		})
	}
}

func TestRedactIPv4(t *testing.T) {
	r := redactText("Server at 192.168.1.1 is running")
	want := "Server at [REDACTED:IP_ADDRESS:CC6.1] is running"
	if r.text != want {
		t.Errorf("got %q, want %q", r.text, want)
	}
	assertCategory(t, r.categories, piiIPv4)
}

func TestRedactIPv4InvalidOctet(t *testing.T) {
	r := redactText("Not IP: 999.999.999.999")
	if strings.Contains(r.text, "REDACTED") {
		t.Errorf("invalid IP was redacted: %q", r.text)
	}
}

func TestRedactIPv4LeadingZero(t *testing.T) {
	r := redactText("Not IP: 01.02.03.04")
	if strings.Contains(r.text, "REDACTED") {
		t.Errorf("leading-zero IP was redacted: %q", r.text)
	}
}

func TestRedactMultiplePII(t *testing.T) {
	input := "SSN: 123-45-6789, email: test@example.com, call (555) 123-4567"
	r := redactText(input)

	if !strings.Contains(r.text, "[REDACTED:SSN:CC6.1]") {
		t.Errorf("SSN not redacted: %q", r.text)
	}
	if !strings.Contains(r.text, "[REDACTED:EMAIL:CC6.1]") {
		t.Errorf("email not redacted: %q", r.text)
	}
	if !strings.Contains(r.text, "[REDACTED:PHONE:CC6.1]") {
		t.Errorf("phone not redacted: %q", r.text)
	}
	if len(r.categories) != 3 {
		t.Errorf("got %d categories, want 3", len(r.categories))
	}
}

func TestRedactNoPII(t *testing.T) {
	input := "The quick brown fox jumps over the lazy dog"
	r := redactText(input)
	if r.text != input {
		t.Errorf("clean text was modified: %q", r.text)
	}
	if len(r.categories) != 0 {
		t.Errorf("got %d categories, want 0", len(r.categories))
	}
}

func TestRedactEmptyString(t *testing.T) {
	r := redactText("")
	if r.text != "" {
		t.Errorf("empty string was modified: %q", r.text)
	}
}

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

	for _, f := range fields {
		r := redactText(f.Text)
		if strings.Contains(r.text, "REDACTED") {
			t.Errorf("field %q was redacted but should not be: %s", f.Path, r.text)
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

func assertCategory(t *testing.T, cats []piiCategory, want piiCategory) {
	t.Helper()
	for _, c := range cats {
		if c == want {
			return
		}
	}
	t.Errorf("category %q not found in %v", want, cats)
}
