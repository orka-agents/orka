/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package protocol

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxExtensions bounds the number of advertised extension identifiers.
const MaxExtensions = 16

// SanitizeExtensions returns a fresh, bounded list of unique safe identifiers.
// Identifiers changed by the message redaction policy are dropped, never rewritten:
// normalization must not turn untrusted input into a supported extension.
func SanitizeExtensions(values []string) []string {
	var result []string
	seen := make(map[string]bool)
	for _, value := range values {
		// JSON decoding repairs malformed Unicode to RuneError; never retain that
		// ambiguous identity, even when its original bytes are no longer available.
		if value == "" || len(value) > MaxIdentityBytes || !utf8.ValidString(value) || strings.ContainsRune(value, utf8.RuneError) ||
			strings.ContainsFunc(value, unicode.IsControl) || strings.Contains(value, "[REDACTED]") ||
			SanitizeMessage(value, MaxIdentityBytes) != value || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
		if len(result) == MaxExtensions {
			break
		}
	}
	return result
}
