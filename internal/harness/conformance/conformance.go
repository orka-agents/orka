package conformance

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
)

const defaultProbeTimeout = 30 * time.Second

// Target identifies a harness endpoint to probe. BearerToken is used only for
// authenticated control-plane endpoints and is never included in Result.
type Target struct {
	BaseURL        string
	BearerToken    string
	HTTPClient     *http.Client
	ControlTimeout time.Duration

	// RequireAuth verifies mutating/streaming endpoints reject unauthenticated
	// requests. It is intended for conformance tests and admission/readiness
	// checks for untrusted endpoints.
	RequireAuth bool

	// ProbeTurn starts and streams one synthetic observed-mode turn. Readiness
	// checks may leave this false when they only need condition-ready health and
	// capabilities data.
	ProbeTurn bool

	// StartTurnRequest overrides the synthetic turn request used when ProbeTurn is true.
	StartTurnRequest *harness.StartTurnRequest
}

// Result is condition-ready conformance output. It intentionally contains only
// sanitized endpoint facts and human-actionable failure messages.
type Result struct {
	Passed               bool
	ObservedCapabilities *harness.CapabilitiesResponse
	Failures             []string
	Message              string
}

// CheckReadiness probes unauthenticated health/capabilities and validates the
// endpoint enough for an AgentRuntime Ready condition.
func CheckReadiness(ctx context.Context, target Target) Result {
	target.ProbeTurn = true
	target.RequireAuth = true
	return Check(ctx, target)
}

// Check runs the configured harness conformance probes.
func Check(ctx context.Context, target Target) Result {
	result := Result{Passed: true}
	baseURL := strings.TrimSpace(target.BaseURL)
	if baseURL == "" {
		return failed("base URL is required")
	}
	controlTimeout := target.ControlTimeout
	if controlTimeout <= 0 {
		controlTimeout = defaultProbeTimeout
	}

	unauth, err := newClient(baseURL, "", target.HTTPClient, controlTimeout)
	if err != nil {
		return failed(fmt.Sprintf("invalid harness endpoint: %v", err))
	}
	if health, err := unauth.Health(ctx); err != nil {
		result.addFailure(fmt.Sprintf("health check failed: %v", err))
	} else if !health.Ready || health.Status != harness.HealthStatusOK {
		result.addFailure(fmt.Sprintf("health not ready: status=%s ready=%t", health.Status, health.Ready))
	}

	caps, err := unauth.Capabilities(ctx)
	if err != nil {
		result.addFailure(fmt.Sprintf("capabilities check failed: %v", err))
	} else {
		result.ObservedCapabilities = caps
	}

	if target.ProbeTurn {
		runTurnProbe(ctx, target, &result, baseURL, controlTimeout)
	}
	result.finalize()
	return result
}

func runTurnProbe(ctx context.Context, target Target, result *Result, baseURL string, controlTimeout time.Duration) {
	if target.RequireAuth && strings.TrimSpace(target.BearerToken) == "" {
		result.addFailure("bearer token is required for authenticated harness conformance")
		return
	}
	request := defaultStartTurnRequest("conformance-turn")
	if result.ObservedCapabilities != nil && strings.TrimSpace(result.ObservedCapabilities.RuntimeName) != "" {
		request.Metadata["runtime"] = strings.TrimSpace(result.ObservedCapabilities.RuntimeName)
	}
	if target.StartTurnRequest != nil {
		request = *target.StartTurnRequest
	}
	if target.RequireAuth {
		assertUnauthenticatedStartRejected(ctx, target, result, baseURL, controlTimeout, request)
	}

	client, err := newClient(baseURL, target.BearerToken, target.HTTPClient, controlTimeout)
	if err != nil {
		result.addFailure(fmt.Sprintf("create authenticated client: %v", err))
		return
	}
	started, err := client.StartTurn(ctx, request)
	if err != nil {
		result.addFailure(fmt.Sprintf("start turn failed: %v", err))
		return
	}
	if strings.TrimSpace(started.EventStreamPath) == "" {
		result.addFailure("start turn response eventStreamPath is required")
	}
	if target.RequireAuth {
		assertUnauthenticatedTurnResourcesRejected(ctx, target, result, baseURL, controlTimeout, request)
	}
	frames := []harness.HarnessEventFrame{}
	if err := client.StreamFrames(ctx, request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		result.addFailure(fmt.Sprintf("stream frames failed: %v", err))
		return
	}
	validateProbeFrames(result, request, frames)
}

func assertUnauthenticatedStartRejected(
	ctx context.Context,
	target Target,
	result *Result,
	baseURL string,
	controlTimeout time.Duration,
	request harness.StartTurnRequest,
) {
	unauth, err := newClient(baseURL, "", target.HTTPClient, controlTimeout)
	if err != nil {
		result.addFailure(fmt.Sprintf("create unauthenticated client: %v", err))
		return
	}
	probe := request
	probe.TurnID = harness.HarnessTurnID(string(request.TurnID) + "-unauth")
	probe.CorrelationID = request.CorrelationID + "-unauth"
	if _, err := unauth.StartTurn(ctx, probe); err == nil {
		result.addFailure("unauthenticated start turn was accepted")
	} else if !isAuthRequiredError(err) {
		result.addFailure(fmt.Sprintf("unauthenticated start turn returned %v, want 401/403", err))
	}
}

func assertUnauthenticatedTurnResourcesRejected(
	ctx context.Context,
	target Target,
	result *Result,
	baseURL string,
	controlTimeout time.Duration,
	request harness.StartTurnRequest,
) {
	unauth, err := newClient(baseURL, "", target.HTTPClient, controlTimeout)
	if err != nil {
		result.addFailure(fmt.Sprintf("create unauthenticated client: %v", err))
		return
	}
	if err := unauth.StreamFrames(ctx, request.TurnID, 0, func(harness.HarnessEventFrame) error { return nil }); err == nil {
		result.addFailure("unauthenticated event stream was accepted")
	} else if !isAuthRequiredError(err) {
		result.addFailure(fmt.Sprintf("unauthenticated event stream returned %v, want 401/403", err))
	}
	if result.ObservedCapabilities == nil || !result.ObservedCapabilities.SupportsCancel {
		return
	}
	_, err = unauth.CancelTurn(ctx, harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Reason:           "conformance unauthenticated cancel probe",
	})
	if err == nil {
		result.addFailure("unauthenticated cancel turn was accepted")
	} else if !isAuthRequiredError(err) {
		result.addFailure(fmt.Sprintf("unauthenticated cancel turn returned %v, want 401/403", err))
	}
}

func validateProbeFrames(result *Result, request harness.StartTurnRequest, frames []harness.HarnessEventFrame) {
	if len(frames) == 0 {
		result.addFailure("stream returned no frames")
		return
	}
	terminal := 0
	for i, frame := range frames {
		if err := frame.ValidateRequired(); err != nil {
			result.addFailure(fmt.Sprintf("frame %d is invalid: %v", i, err))
		}
		if !harness.IsKnownFrameType(frame.Type) {
			result.addFailure(fmt.Sprintf("frame %d type %q is unknown", i, frame.Type))
		}
		if frame.RuntimeSessionID != request.RuntimeSessionID || frame.TurnID != request.TurnID || frame.CorrelationID != request.CorrelationID {
			result.addFailure(fmt.Sprintf("frame %d identity does not match requested turn", i))
		}
		switch frame.Type {
		case harness.FrameTurnCompleted, harness.FrameTurnFailed, harness.FrameTurnCancelled:
			terminal++
		}
	}
	if terminal != 1 {
		result.addFailure(fmt.Sprintf("terminal frame count = %d, want exactly 1", terminal))
	}
}

func newClient(baseURL, token string, httpClient *http.Client, timeout time.Duration) (*harness.Client, error) {
	opts := []harness.ClientOption{harness.WithControlTimeout(timeout), harness.WithBearerToken(token)}
	if httpClient != nil {
		opts = append(opts, harness.WithHTTPClient(httpClient))
	}
	return harness.NewClient(baseURL, opts...)
}

func defaultStartTurnRequest(turnID string) harness.StartTurnRequest {
	suffix := uuid.NewString()
	if strings.TrimSpace(turnID) == "" {
		turnID = "conformance-turn"
	}
	return harness.StartTurnRequest{
		Version:           harness.ProtocolVersion,
		Namespace:         "orka-conformance",
		TaskName:          "probe",
		SessionName:       "probe-session-" + suffix,
		RuntimeSessionID:  harness.RuntimeSessionID("probe-runtime-session-" + suffix),
		TurnID:            harness.HarnessTurnID(turnID + "-" + suffix),
		CorrelationID:     "probe-correlation-" + suffix,
		Deadline:          time.Now().UTC().Add(time.Minute),
		AuthIdentity:      harness.AuthIdentity{Subject: "system:orka-conformance"},
		Input:             harness.TurnInput{Prompt: "Orka harness conformance probe. Reply with a short success result."},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
		Metadata: map[string]string{
			"runtime": "conformance",
			"probe":   "true",
		},
	}
}

func isAuthRequiredError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "(401)") || strings.Contains(message, "(403)") || strings.Contains(message, "unauthorized") || strings.Contains(message, "forbidden")
}

func failed(message string) Result {
	message = sanitizeMessage(message)
	return Result{Passed: false, Failures: []string{message}, Message: message}
}

func (r *Result) addFailure(message string) {
	message = sanitizeMessage(message)
	if strings.TrimSpace(message) == "" {
		return
	}
	r.Passed = false
	r.Failures = append(r.Failures, message)
}

func sanitizeMessage(message string) string {
	return events.RedactExecutionEventText(strings.TrimSpace(message))
}

func (r *Result) finalize() {
	if len(r.Failures) == 0 {
		r.Passed = true
		r.Message = "harness conformance checks passed"
		return
	}
	r.Passed = false
	r.Message = strings.Join(r.Failures, "; ")
}
