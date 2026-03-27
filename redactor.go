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
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// piiCategory identifies a type of personally identifiable information.
type piiCategory string

const (
	piiSSN   piiCategory = "SSN"
	piiCC    piiCategory = "CC_NUMBER"
	piiEmail piiCategory = "EMAIL"
	piiPhone piiCategory = "PHONE"
	piiIPv4  piiCategory = "IP_ADDRESS"
)

// piiPattern holds a compiled regex and its metadata for one PII type.
type piiPattern struct {
	re       *regexp.Regexp
	category piiCategory
	validate func(match string) bool // optional post-match validation
	tag      string                  // SOC 2 compliance control
}

// Compiled patterns — initialized once, read-only, safe for concurrent use.
var piiPatterns []piiPattern

func init() {
	piiPatterns = []piiPattern{
		// Email (most specific format — process first).
		{
			re:       regexp.MustCompile(`\b[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b`),
			category: piiEmail,
			tag:      "CC6.1",
		},
		// SSN: NNN-NN-NNNN.
		{
			re:       regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
			category: piiSSN,
			tag:      "CC6.1",
		},
		// Credit card: prefix-constrained with optional separators.
		// Visa (4xxx 16-digit), Mastercard (51-55/2221-2720 16-digit),
		// Amex (34/37 15-digit). Luhn validation eliminates false positives.
		{
			re: regexp.MustCompile(
				`\b(?:` +
					`4[0-9]{3}[ -]?[0-9]{4}[ -]?[0-9]{4}[ -]?[0-9]{4}` + // Visa
					`|5[1-5][0-9]{2}[ -]?[0-9]{4}[ -]?[0-9]{4}[ -]?[0-9]{4}` + // MC 51-55
					`|2[2-7][0-9]{2}[ -]?[0-9]{4}[ -]?[0-9]{4}[ -]?[0-9]{4}` + // MC 2xxx
					`|3[47][0-9]{2}[ -]?[0-9]{6}[ -]?[0-9]{5}` + // Amex
					`)\b`,
			),
			category: piiCC,
			validate: luhnValid,
			tag:      "CC6.1",
		},
		// US phone: two alternatives to prevent matching inside longer digit
		// sequences. With +1 prefix the prefix itself acts as an anchor;
		// without it, a \b is required before the area code.
		{
			re:       regexp.MustCompile(`(?:\+1[-.\s]?\(?[2-9]\d{2}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b|\b\(?[2-9]\d{2}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b)`),
			category: piiPhone,
			tag:      "CC6.1",
		},
		// IPv4: dotted quad, validated for 0-255 range.
		{
			re:       regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
			category: piiIPv4,
			validate: validateIPv4,
			tag:      "CC6.1",
		},
	}
}

// redactionMarker builds the replacement string, e.g. "[REDACTED:SSN:CC6.1]".
func redactionMarker(cat piiCategory, tag string) string {
	return fmt.Sprintf("[REDACTED:%s:%s]", cat, tag)
}

// redactResult holds the output of a redaction pass.
type redactResult struct {
	text       string
	categories []piiCategory // deduplicated categories found
}

// redactText applies all PII patterns to a string, replacing matches with
// tagged redaction markers. Patterns are applied sequentially; earlier
// markers will not accidentally match later patterns because markers contain
// no digits, @, or dots in PII-triggering positions.
func redactText(input string) redactResult {
	catSet := make(map[piiCategory]struct{})
	result := input

	for _, p := range piiPatterns {
		pat := p // capture for closure
		result = pat.re.ReplaceAllStringFunc(result, func(match string) string {
			if pat.validate != nil && !pat.validate(match) {
				return match
			}
			catSet[pat.category] = struct{}{}
			return redactionMarker(pat.category, pat.tag)
		})
	}

	cats := make([]piiCategory, 0, len(catSet))
	for c := range catSet {
		cats = append(cats, c)
	}
	return redactResult{text: result, categories: cats}
}

// ---------------------------------------------------------------------------
// Validation functions
// ---------------------------------------------------------------------------

// luhnValid returns true if the digit string (after stripping spaces and
// dashes) passes the Luhn checksum.
func luhnValid(s string) bool {
	var digits []int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			digits = append(digits, int(c-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}

	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// validateIPv4 checks that each octet is 0-255 with no leading zeros.
func validateIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 {
			return false
		}
		// Reject leading zeros (e.g. "01.02.03.04").
		if len(p) > 1 && p[0] == '0' {
			return false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}
