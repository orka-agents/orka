package supervisor

import (
	"strings"
	"testing"
)

func TestSSETerminalErrorScannerDetectsInStreamFailures(t *testing.T) {
	t.Parallel()
	failures := []string{
		"event: message_start\ndata: {}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n",
		"data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{}}\n\n",
		"data: {\"error\": {\"message\": \"rate limited\"}}\n\n",
		"data: {\"type\": \"response.failed\", \"response\": {}}\n\n",
		"data: { \"type\" : \"error\" , \"error\": {} }\n\n",
		"data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.failed\"}",
		"data: {\"padding\":\"" + strings.Repeat("x", 2048) + "\",\"type\":\"response.failed\"}\n\n",
	}
	// One failure stream deliberately ends without a trailing newline:
	// the flush at end-of-stream must scan the residual line.
	for _, stream := range failures {
		scanner := &sseTerminalErrorScanner{}
		// Feed byte-by-byte to prove chunk boundaries cannot hide a marker.
		for i := 0; i < len(stream); i++ {
			if _, err := scanner.Write([]byte{stream[i]}); err != nil {
				t.Fatal(err)
			}
		}
		scanner.flush()
		if !scanner.failed {
			t.Fatalf("scanner missed terminal error in %q", stream[:40])
		}
	}
	clean := []string{
		"event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"discussing event: response.failed and \\\"type\\\":\\\"error\\\" handling\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{}}\n\n",
		"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n",
	}
	for _, stream := range clean {
		scanner := &sseTerminalErrorScanner{}
		if _, err := scanner.Write([]byte(stream)); err != nil {
			t.Fatal(err)
		}
		if scanner.failed {
			t.Fatalf("scanner false-failed clean stream %q (detail %q)", stream[:40], scanner.detail)
		}
		if !scanner.completed {
			t.Fatalf("scanner missed terminal success in %q", stream[:40])
		}
	}
	scanner := &sseTerminalErrorScanner{}
	if _, err := scanner.Write([]byte("event: response.created\ndata: {}\n\n")); err != nil {
		t.Fatal(err)
	}
	if scanner.failed || scanner.completed {
		t.Fatalf("incomplete stream verdict = failed:%t completed:%t, want neither", scanner.failed, scanner.completed)
	}
}

func TestSSETerminalErrorScannerCapturesFullErrorDetail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stream string
		want   string
	}{
		{
			name:   "responses failed payload keeps the message after the marker",
			stream: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"insufficient quota\"}}}\n\n",
			want:   `{"type":"response.failed","response":{"error":{"message":"insufficient quota"}}}`,
		},
		{
			name:   "error object message is extracted",
			stream: "data: {\"error\": {\"message\": \"rate limited\", \"type\": \"rate_limit_error\"}}\n\n",
			want:   "rate limited",
		},
		{
			name:   "anthropic error event uses the following data line",
			stream: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n",
			want:   "Overloaded",
		},
		{
			name:   "error event without data falls back to the marker",
			stream: "event: error\n\n",
			want:   "event:error",
		},
		{
			name:   "unterminated error line settles on flush",
			stream: "data: {\"type\":\"error\",\"error\":{\"message\":\"boom\"}}",
			want:   "boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scanner := &sseTerminalErrorScanner{}
			for i := 0; i < len(tc.stream); i++ {
				if _, err := scanner.Write([]byte{tc.stream[i]}); err != nil {
					t.Fatal(err)
				}
			}
			scanner.flush()
			if !scanner.failed {
				t.Fatalf("scanner did not fail on %q", tc.stream)
			}
			if scanner.detail != tc.want {
				t.Fatalf("detail = %q, want %q", scanner.detail, tc.want)
			}
		})
	}
}

// testAllocateInferenceSeq mirrors admission-time sequence allocation for
// fixtures that record outcomes without an HTTP request.
func testAllocateInferenceSeq(s *providerProxySession) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issuedInference++
	return s.issuedInference
}

func TestSanitizeProviderUpstreamDetailReassemblesWrappedCredential(t *testing.T) {
	t.Parallel()
	const key = "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"
	got := sanitizeProviderUpstreamDetail("quota error for " + key[:11] + "\n" + key[11:] + " on model")
	if strings.Contains(got, key[:11]) || strings.Contains(got, key[11:]) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("sanitizeProviderUpstreamDetail() = %q, want line-wrapped credential redacted", got)
	}
}

func TestSanitizeProviderUpstreamDetailWithholdsPolicyMatchedCredentials(t *testing.T) {
	t.Parallel()
	// Not a real key: a bare AWS access-key ID shape the redactor does not
	// recognize but the secret policy does.
	const awsKeyShaped = "AKIA" + "IOSFODNN7EXAMPLE"
	got := sanitizeProviderUpstreamDetail("invalid credentials for access key " + awsKeyShaped)
	if strings.Contains(got, awsKeyShaped) {
		t.Fatalf("sanitizeProviderUpstreamDetail() = %q, want the credential-shaped detail withheld", got)
	}
	if got != providerUpstreamDetailWithheld {
		t.Fatalf("sanitizeProviderUpstreamDetail() = %q, want %q", got, providerUpstreamDetailWithheld)
	}
	for _, plain := range []string{"insufficient quota", "model not found: gpt-5.6-sol", "rate limited, retry after 20s"} {
		if got := sanitizeProviderUpstreamDetail(plain); got != plain {
			t.Fatalf("sanitizeProviderUpstreamDetail(%q) = %q, want unchanged", plain, got)
		}
	}
}

func TestSSETerminalErrorScannerCapturesUsageOnlyWhenAsked(t *testing.T) {
	line := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n"
	for _, capture := range []bool{false, true} {
		scanner := &sseTerminalErrorScanner{captureUsage: capture}
		_, _ = scanner.Write([]byte(line))
		if !scanner.completed || (scanner.usage != nil) != capture {
			t.Fatalf("capture=%v: completed=%v usage=%#v", capture, scanner.completed, scanner.usage)
		}
		if capture && (scanner.usage.InputTokens != 7 || scanner.usage.OutputTokens != 3) {
			t.Fatalf("usage = %#v", scanner.usage)
		}
	}
	// An incomplete response is a terminal success, and its tokens count too.
	incomplete := &sseTerminalErrorScanner{captureUsage: true}
	_, _ = incomplete.Write([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":9}}}\n"))
	if !incomplete.completed || incomplete.usage == nil || incomplete.usage.InputTokens != 5 || incomplete.usage.OutputTokens != 9 {
		t.Fatalf("incomplete: completed=%v usage=%#v", incomplete.completed, incomplete.usage)
	}
	// Invalid cache accounting is dropped rather than recorded.
	scanner := &sseTerminalErrorScanner{captureUsage: true}
	_, _ = scanner.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":5}}}}\n"))
	if scanner.usage != nil {
		t.Fatalf("invalid usage recorded: %#v", scanner.usage)
	}
}

func TestSSETerminalErrorScannerCapturesUsageFromLongCompletedLines(t *testing.T) {
	// A real response.completed event carries the whole response object.
	output := strings.Repeat("model output ", 400)
	line := "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"text\":\"" + output +
		"\"}],\"usage\":{\"input_tokens\":70,\"output_tokens\":30}}}\n"
	if len(line) <= sseScannerDetailPrefixBytes {
		t.Fatalf("line is only %d bytes", len(line))
	}
	scanner := &sseTerminalErrorScanner{captureUsage: true}
	_, _ = scanner.Write([]byte(line))
	if !scanner.completed || scanner.usage == nil || scanner.usage.InputTokens != 70 || scanner.usage.OutputTokens != 30 {
		t.Fatalf("completed=%v usage=%#v", scanner.completed, scanner.usage)
	}
	// The buffered line stays bounded.
	oversized := "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"text\":\"" + strings.Repeat("x", sseScannerUsageLineBytes) +
		"\"}],\"usage\":{\"input_tokens\":70,\"output_tokens\":30}}}\n"
	scanner = &sseTerminalErrorScanner{captureUsage: true}
	_, _ = scanner.Write([]byte(oversized))
	if scanner.usage != nil || len(scanner.usageLine) > sseScannerUsageLineBytes {
		t.Fatalf("oversized line recorded usage %#v with a %d-byte buffer", scanner.usage, len(scanner.usageLine))
	}
}
