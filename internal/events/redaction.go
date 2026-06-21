package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/sozercan/orka/internal/redact"
)

const (
	ExecutionEventRedactedValue = "[REDACTED]"

	MaxExecutionEventSummaryChars     = 4 * 1024
	MaxExecutionEventContentTextChars = 32 * 1024
	MaxExecutionEventContentJSONBytes = 64 * 1024
)

var (
	executionEventAuthorizationHeaderRe = regexp.MustCompile(`(?i)\b(authorization\s*:\s*)[^\r\n]+`)
	executionEventTransactionHeaderRe   = regexp.MustCompile(`(?i)\b((?:txn-token|transaction-token)\s*:\s*)[A-Za-z0-9._~+/=-]+`)
	executionEventCookieHeaderRe        = regexp.MustCompile(`(?i)\b((?:cookie|set-cookie)\s*:\s*)[^\r\n]+`)
)

// ExecutionEventTruncation records whether public event payload fields were truncated.
// Lengths are measured after redaction so metadata cannot expose raw secret sizes.
type ExecutionEventTruncation struct {
	SummaryTruncated         bool `json:"summaryTruncated,omitempty"`
	SummaryOriginalChars     int  `json:"summaryOriginalChars,omitempty"`
	ContentTextTruncated     bool `json:"contentTextTruncated,omitempty"`
	ContentTextOriginalChars int  `json:"contentTextOriginalChars,omitempty"`
	ContentJSONTruncated     bool `json:"contentJsonTruncated,omitempty"`
	ContentJSONOriginalBytes int  `json:"contentJsonOriginalBytes,omitempty"`
}

// Empty reports whether no truncation metadata was set.
func (m ExecutionEventTruncation) Empty() bool {
	return !m.SummaryTruncated && m.SummaryOriginalChars == 0 &&
		!m.ContentTextTruncated && m.ContentTextOriginalChars == 0 &&
		!m.ContentJSONTruncated && m.ContentJSONOriginalBytes == 0
}

// SanitizedExecutionEventPayload contains redacted and size-bounded public payload fields.
type SanitizedExecutionEventPayload struct {
	Summary     string
	Content     json.RawMessage
	ContentText string
	Truncation  *ExecutionEventTruncation
}

// RedactExecutionEventText redacts credential-shaped text using the shared Orka redaction rules.
func RedactExecutionEventText(value string) string {
	redacted := redact.SensitiveText(value)
	redacted = executionEventAuthorizationHeaderRe.ReplaceAllString(redacted, `${1}`+ExecutionEventRedactedValue)
	redacted = executionEventTransactionHeaderRe.ReplaceAllString(redacted, `${1}`+ExecutionEventRedactedValue)
	redacted = executionEventCookieHeaderRe.ReplaceAllString(redacted, `${1}`+ExecutionEventRedactedValue)
	return redacted
}

// RedactAndTruncateExecutionEventText redacts value and bounds it to maxChars runes.
func RedactAndTruncateExecutionEventText(value string, maxChars int) (string, bool, int) {
	redacted := RedactExecutionEventText(value)
	return truncateRunes(redacted, maxChars)
}

// SanitizeExecutionEventPayload redacts and truncates event payload fields for API/worker boundaries.
func SanitizeExecutionEventPayload(summary string, content json.RawMessage, contentText string) (SanitizedExecutionEventPayload, error) {
	var metadata ExecutionEventTruncation

	summary, truncated, originalChars := RedactAndTruncateExecutionEventText(summary, MaxExecutionEventSummaryChars)
	if truncated {
		metadata.SummaryTruncated = true
		metadata.SummaryOriginalChars = originalChars
	}

	contentText, truncated, originalChars = RedactAndTruncateExecutionEventText(contentText, MaxExecutionEventContentTextChars)
	if truncated {
		metadata.ContentTextTruncated = true
		metadata.ContentTextOriginalChars = originalChars
	}

	sanitizedContent, contentMetadata, err := SanitizeExecutionEventJSON(content)
	if err != nil {
		return SanitizedExecutionEventPayload{}, err
	}
	if contentMetadata != nil {
		metadata.ContentJSONTruncated = contentMetadata.ContentJSONTruncated
		metadata.ContentJSONOriginalBytes = contentMetadata.ContentJSONOriginalBytes
	}

	var truncation *ExecutionEventTruncation
	if !metadata.Empty() {
		truncation = &metadata
	}

	return SanitizedExecutionEventPayload{
		Summary:     summary,
		Content:     sanitizedContent,
		ContentText: contentText,
		Truncation:  truncation,
	}, nil
}

// SanitizeExecutionEventJSON redacts sensitive JSON keys and string values while preserving valid JSON.
// Oversized JSON is replaced by a small preview object and marked in truncation metadata.
func SanitizeExecutionEventJSON(content json.RawMessage) (json.RawMessage, *ExecutionEventTruncation, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil, nil, nil
	}

	if len(trimmed) > MaxExecutionEventContentJSONBytes {
		return truncateExecutionEventJSON(trimmed, false)
	}

	var value any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, nil, fmt.Errorf("invalid execution event content JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, nil, fmt.Errorf("invalid execution event content JSON: trailing data")
	}

	sanitized := sanitizeExecutionEventJSONValue(value)
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal sanitized execution event content JSON: %w", err)
	}
	if len(encoded) <= MaxExecutionEventContentJSONBytes {
		return json.RawMessage(encoded), nil, nil
	}

	return truncateExecutionEventJSON(encoded, true)
}

func truncateExecutionEventJSON(content []byte, includeSanitizedLength bool) (json.RawMessage, *ExecutionEventTruncation, error) {
	body := map[string]any{
		"truncated": true,
		"preview":   "[truncated oversized JSON content]",
	}
	metadata := &ExecutionEventTruncation{ContentJSONTruncated: true}
	if includeSanitizedLength {
		body["originalBytes"] = len(content)
		metadata.ContentJSONOriginalBytes = len(content)
	}
	truncated, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal truncated execution event content JSON: %w", err)
	}
	return json.RawMessage(truncated), metadata, nil
}

// IsSensitiveExecutionEventKey reports whether a JSON key should have its value replaced.
func IsSensitiveExecutionEventKey(key string) bool {
	normalized := strings.ToLower(key)
	normalized = strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(normalized)
	if isExecutionEventTokenUsageKey(normalized) {
		return false
	}

	for _, marker := range []string{
		"apikey",
		"token",
		"secret",
		"password",
		"passwd",
		"pwd",
		"credential",
		"privatekey",
		"clientsecret",
		"accesstoken",
		"refreshtoken",
		"authorization",
		"cookie",
		"txntoken",
		"transactiontoken",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func isExecutionEventTokenUsageKey(normalized string) bool {
	switch normalized {
	case "prompttokens",
		"completiontokens",
		"inputtokens",
		"outputtokens",
		"totaltokens",
		"totaltokencount",
		"tokencount",
		"cachedtokens",
		"reasoningtokens":
		return true
	default:
		return false
	}
}

func sanitizeExecutionEventJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if IsSensitiveExecutionEventKey(key) {
				out[key] = ExecutionEventRedactedValue
				continue
			}
			out[key] = sanitizeExecutionEventJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = sanitizeExecutionEventJSONValue(child)
		}
		return out
	case string:
		return RedactExecutionEventText(typed)
	default:
		return typed
	}
}

func truncateRunes(value string, maxChars int) (string, bool, int) {
	if maxChars <= 0 {
		return "", value != "", len([]rune(value))
	}
	runes := []rune(value)
	if len(runes) <= maxChars {
		return value, false, len(runes)
	}
	if maxChars == 1 {
		return "…", true, len(runes)
	}
	return string(runes[:maxChars-1]) + "…", true, len(runes)
}
