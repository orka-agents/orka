//go:build linux

package supervisor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func testPinnedCodexRejectedContinuation(t *testing.T, data []byte, threadID string) {
	rejectSecond := func(w http.ResponseWriter, n int) bool {
		if n != 2 {
			return false
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, "scripted rejection", http.StatusTooManyRequests)
		return true
	}
	snapshot, posts := runPinnedNativeTurn(t, data, threadID, "current-retry-fixture", "NATIVE_ORka_RETRY", "", harnessv2.WorkspaceIntentWrite, "gpt-5.4", "high", rejectSecond)
	if len(posts) != 3 {
		t.Fatalf("POST /v1/responses = %d, want 3 (tool, rejected continuation, successful continuation)", len(posts))
	}
	gap := posts[2].at.Sub(posts[1].at)
	if gap < time.Second || gap > 30*time.Second {
		t.Errorf("provider retry after %s; want >= Retry-After: 1 and within the retry budget", gap)
	}
	payload := func(body []byte) map[string]any {
		t.Helper()
		var request map[string]any
		d := json.NewDecoder(bytes.NewReader(body))
		d.UseNumber()
		if err := d.Decode(&request); err != nil || len(request) == 0 {
			t.Fatalf("invalid Responses body: %v", err)
		}
		return request
	}
	if !reflect.DeepEqual(payload(posts[1].body), payload(posts[2].body)) {
		t.Error("retry changed the rejected Responses payload")
	}
	assertOneNativeToolTurn(t, data, snapshot.Data)
	t.Logf("POST /v1/responses=3; rejected continuation retried after %s", gap)
}

func assertOneNativeToolTurn(t *testing.T, before, after []byte) {
	t.Helper()
	count := func(snapshot []byte) (prompts, toolResults int) {
		t.Helper()
		var bundle struct{ Rollout []byte }
		if err := json.Unmarshal(snapshot, &bundle); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(bundle.Rollout))
		for {
			var record struct {
				Type    string
				Payload struct{ Type string }
			}
			if err := decoder.Decode(&record); err == io.EOF {
				return prompts, toolResults
			} else if err != nil {
				t.Fatal(err)
			}
			if record.Type == "event_msg" && record.Payload.Type == "task_started" {
				prompts++
			}
			if record.Type == "response_item" && record.Payload.Type == "function_call_output" {
				toolResults++
			}
		}
	}
	p0, r0 := count(before)
	p1, r1 := count(after)
	if p1-p0 != 1 || r1-r0 != 1 {
		t.Fatalf("native rollout added %d prompts, %d tool results; want 1 of each", p1-p0, r1-r0)
	}
	t.Log("native rollout: 1 new prompt, 1 new tool result; uploaded file: 1 appended line")
}

func testPinnedCodexInferenceFailures(t *testing.T, data []byte, threadID string) {
	t.Run("stalled_final_429", func(t *testing.T) {
		testPinnedCodexStalledRetryError(t, data, threadID, http.StatusTooManyRequests, 3)
	})
	t.Run("stalled_503_after_429", func(t *testing.T) {
		testPinnedCodexStalledRetryError(t, data, threadID, http.StatusServiceUnavailable, 2)
	})
	const fixtureCredential = "only-for-local-rejection-fixture"
	rateLimit := func(w http.ResponseWriter, header, value string) {
		w.Header().Set(header, value)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w, `{"error":{"code":"rate_limit_exceeded","message":"fixture quota window; Authorization: Bearer %s"}}`, fixtureCredential)
	}
	for _, tc := range []struct {
		name        string
		writeFault  func(http.ResponseWriter, int)
		rejections  int
		terminal429 bool
	}{
		{"persistent_429", func(w http.ResponseWriter, _ int) { rateLimit(w, "Retry-After-Ms", "1250") }, 0, true},
		{"remaining_budget", func(w http.ResponseWriter, n int) {
			delay := "1250"
			if n == 2 {
				delay = "29500"
			}
			rateLimit(w, "Retry-After-Ms", delay)
		}, 2, true},
		{"excessive_ms", func(w http.ResponseWriter, _ int) {
			w.Header().Set("Retry-After", "0")
			rateLimit(w, "X-Ms-Retry-After-Ms", "45000")
		}, 1, true},
		{"excessive_fractional_ms", func(w http.ResponseWriter, _ int) {
			rateLimit(w, "Retry-After-Ms", "45000.5")
		}, 1, true},
		{"overflow", func(w http.ResponseWriter, _ int) {
			rateLimit(w, "Retry-After", "18446744073709551616")
		}, 1, true},
		{"excessive_http_date", func(w http.ResponseWriter, _ int) {
			rateLimit(w, "Retry-After", time.Now().Add(45*time.Second).UTC().Format(http.TimeFormat))
		}, 1, true},
		{"503_is_not_429", func(w http.ResponseWriter, _ int) {
			http.Error(w, "HTTP 429 in text only", http.StatusServiceUnavailable)
		}, 1, false},
		{"ambiguous_delivery", func(http.ResponseWriter, int) {
			panic(http.ErrAbortHandler)
		}, 1, false},
		{"429_then_ambiguous", func(w http.ResponseWriter, n int) {
			if n == 1 {
				rateLimit(w, "Retry-After-Ms", "1250")
				return
			}
			panic(http.ErrAbortHandler)
		}, 2, false},
		{"stream_rate_limit_is_not_http_429", func(w http.ResponseWriter, _ int) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"accepted\",\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"HTTP 429 in SSE\"}}}\n\n")
		}, 1, false},
		{"interrupted_stream", func(w http.ResponseWriter, _ int) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"committed\"}}\n\n"+
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial text\"}\n\n")
			w.(http.Flusher).Flush()
		}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, captured := newPinnedNativeUpstream(t, fixtureCredential, "UNEXPECTED_RECOVERY", harnessv2.WorkspaceIntentRead, 1, func(w http.ResponseWriter, n int) bool {
				if tc.rejections != 0 && n > tc.rejections {
					return false
				}
				tc.writeFault(w, n)
				return true
			})
			defer upstream.Close()
			session := preparePinnedNativeSession(t, data, threadID, fixtureCredential, "", harnessv2.WorkspaceIntentRead, "gpt-5.4", "high", upstream.URL)
			result, terminal := session.runPrompt(t)
			posts := captured()
			elapsed := time.Since(posts[0].at)
			t.Logf("POST /v1/responses=%d; first POST to settlement=%s; terminal=%s", len(posts), elapsed, terminal.Type)
			if elapsed > 32*time.Second {
				t.Errorf("did not settle within the retry budget: %s", elapsed)
			}
			wantPosts := tc.rejections
			if wantPosts == 0 {
				wantPosts = 3
			}
			if len(posts) != wantPosts {
				t.Errorf("%d physical POSTs across all retry layers; want %d", len(posts), wantPosts)
			}
			if wantPosts > 1 {
				for i := 1; i < len(posts); i++ {
					if gap := posts[i].at.Sub(posts[i-1].at); gap < 1250*time.Millisecond {
						t.Errorf("retry %d was early: %s < Retry-After-Ms: 1250", i, gap)
					}
				}
			}
			if tc.terminal429 {
				if terminal.Failed == nil || terminal.Failed.Code != providerUpstreamErrorCode ||
					terminal.Failed.StopReason != harnessv2.ACPStopReasonRefusal || terminal.Failed.Retryable {
					t.Fatalf("exhausted or excessive 429 should fail the prompt, not invite prompt replay: terminal=%s", terminal.Type)
				}
				message := terminal.Failed.Message
				if !strings.Contains(message, "HTTP 429") || !strings.Contains(message, "fixture quota window") {
					t.Error("terminal failure lost HTTP 429 or the safe part of the provider detail")
				}
				if bytes.Contains(result.Body.Bytes(), []byte(fixtureCredential)) {
					t.Error("prompt event stream exposed the synthetic provider credential")
				}
			} else if terminal.Failed == nil && terminal.OutcomeUnknown == nil {
				t.Errorf("terminal=%s after an unsuccessful provider request", terminal.Type)
			}
		})
	}

}

func testPinnedCodexStalledRetryError(t *testing.T, data []byte, threadID string, finalStatus, wantPosts int) {
	t.Helper()
	const credential = "only-for-local-stall-fixture"
	detail := "final " + http.StatusText(finalStatus)
	first, release := make(chan struct{}), make(chan struct{})
	upstream, captured := newPinnedNativeUpstream(t, credential, "UNEXPECTED_RECOVERY", harnessv2.WorkspaceIntentRead, 1, func(w http.ResponseWriter, n int) bool {
		if n == 1 {
			close(first)
		}
		if n > wantPosts {
			return false
		}
		w.Header().Set("Retry-After", "0")
		w.Header().Set("Content-Type", "application/json")
		if n == wantPosts {
			w.WriteHeader(finalStatus)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"%s; Authorization: Bearer %s"}}`, detail, credential)
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Error("could not flush terminal rejection")
			}
			<-release
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"earlier rejection"}}`)
		}
		return true
	})
	defer upstream.Close()
	defer close(release)
	s := preparePinnedNativeSession(t, data, threadID, credential, "", harnessv2.WorkspaceIntentRead, "gpt-5.4", "high", upstream.URL)
	s.prompt.Metadata.ExpiresAt = time.Now().UTC().Add(50 * time.Second)
	s.prompt.MCPAuthorization.ExpiresAt = s.prompt.Metadata.ExpiresAt
	sealRequest(t, &s.prompt.Metadata.RequestDigest, s.prompt)
	done := serveMutationAsync(s.server.Handler(), mutationHTTPRequest(t, http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", s.prompt, s.cfg))
	awaitSignal(t, first, "provider never received the initial POST")
	start := captured()[0].at
	select {
	case resp := <-done:
		f := s.terminal(t, resp).Failed
		if len(captured()) != wantPosts || f == nil || f.Code != providerUpstreamErrorCode || f.StopReason != harnessv2.ACPStopReasonRefusal ||
			f.Retryable || !strings.Contains(f.Message, fmt.Sprintf("HTTP %d", finalStatus)) || !strings.Contains(f.Message, detail) ||
			bytes.Contains(resp.Body.Bytes(), []byte(credential)) {
			t.Errorf("stalled terminal HTTP %d lost its bounded, sanitized failure: POSTs=%d, want %d", finalStatus, len(captured()), wantPosts)
		}
	case <-time.After(time.Until(start.Add(32 * time.Second))):
		t.Fatalf("no terminal prompt after %s; final HTTP %d body still open, POSTs=%d", time.Since(start), finalStatus, len(captured()))
	}
}

func testPinnedCodexCancelAfterRejection(t *testing.T, data []byte, threadID string) {
	const credential = "local-cancel-fixture"
	const promptPath = "/v2/runtime-sessions/session-1/prompts/prompt-1"
	rejected := make(chan struct{})
	replayed := make(chan struct{}, 1)
	upstream, captured := newPinnedNativeUpstream(t, credential, "UNEXPECTED_RECOVERY", harnessv2.WorkspaceIntentRead, 1, func(w http.ResponseWriter, n int) bool {
		if n != 1 {
			select {
			case replayed <- struct{}{}:
			default:
			}
			return false
		}
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"cancel fixture quota window"}}`)
		w.(http.Flusher).Flush()
		close(rejected)
		return true
	})
	defer upstream.Close()
	s := preparePinnedNativeSession(t, data, threadID, credential, "", harnessv2.WorkspaceIntentRead, "gpt-5.4", "high", upstream.URL)
	prompt := serveMutationAsync(s.server.Handler(), mutationHTTPRequest(t, http.MethodPut, promptPath, s.prompt, s.cfg))
	awaitSignal(t, rejected, "no HTTP 429 reached the native prompt")
	due := captured()[0].at.Add(2 * time.Second)
	select {
	case <-prompt:
		t.Fatalf("prompt already settled instead of waiting for Retry-After: 2; POSTs=%d", len(captured()))
	case <-time.After(150 * time.Millisecond):
	}
	cancel := testLateCancellation(t, s.create.Metadata.Fence)
	response := performMutation(t, s.server.Handler(), http.MethodPut, promptPath+"/cancel", cancel, s.cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("cancel returned HTTP %d", response.Code)
	}
	event := s.terminal(t, awaitRecorder(t, prompt, "cancel did not settle the native prompt"))
	var receipt harnessv2.CancelPromptResponse
	decodeResponse(t, response, &receipt)
	if err := receipt.Validate(); err != nil || !receipt.SettlementProven || receipt.Settlement.TerminalEvent != event.Type {
		t.Errorf("cancel receipt did not confirm the prompt's terminal event: stream=%s receipt=%s proven=%t err=%v",
			event.Type, receipt.Settlement.TerminalEvent, receipt.SettlementProven, err)
	}
	if event.Cancelled == nil {
		f := event.Failed
		if f == nil || f.Code != providerUpstreamErrorCode || f.StopReason != harnessv2.ACPStopReasonRefusal ||
			f.Retryable || !strings.Contains(f.Message, "HTTP 429") || !strings.Contains(f.Message, "cancel fixture quota window") {
			t.Errorf("terminal=%s; want cancellation or the observed provider 429/refusal", event.Type)
		}
	}
	if margin := time.Until(due); margin < 250*time.Millisecond {
		t.Errorf("cancellation settled only %s before the retry was due; want at least 250ms", margin)
	}
	select {
	case <-replayed:
		t.Fatal("sent a second provider request after cancellation")
	case <-time.After(time.Until(due.Add(250 * time.Millisecond))):
	}
	if n := len(captured()); n != 1 {
		t.Errorf("Task timeout cancellation: POST /v1/responses=%d, want 1", n)
	}
	t.Logf("Task timeout cancellation: terminal=%s", event.Type)
}
