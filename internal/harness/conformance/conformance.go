package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
)

const (
	defaultProbeTimeout      = 30 * time.Second
	cleanupProbeTimeout      = 10 * time.Second
	postTerminalDrainTimeout = 100 * time.Millisecond
	maxProbeFrames           = 256
	maxProbeFrameBytes       = 4 << 20
)

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

	// ProbeBrokeredRead starts a brokered read-profile turn, waits for a tool call,
	// sends a synthetic Orka tool result through /continue, and requires terminal completion.
	ProbeBrokeredRead bool

	// ProbeBrokeredWrite starts a brokered write-profile turn, waits for a tool call,
	// sends a synthetic approved Orka tool result through /continue, and requires terminal completion.
	ProbeBrokeredWrite bool

	// ProbeBrokeredCoordination starts a brokered coordination-profile turn and
	// verifies the same tool-request/continue/result/terminal contract.
	ProbeBrokeredCoordination bool

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

	if len(result.Failures) == 0 {
		if target.ProbeBrokeredRead {
			runBrokeredProbe(ctx, target, &result, baseURL, controlTimeout, harness.BrokeredToolClassRead)
		} else if target.ProbeBrokeredWrite {
			runBrokeredProbe(ctx, target, &result, baseURL, controlTimeout, harness.BrokeredToolClassWrite)
		} else if target.ProbeBrokeredCoordination {
			runBrokeredProbe(ctx, target, &result, baseURL, controlTimeout, harness.BrokeredToolClassCoordination)
		} else if target.ProbeTurn {
			runTurnProbe(ctx, target, &result, baseURL, controlTimeout)
		} else if target.RequireAuth {
			runAuthProbe(ctx, target, &result, baseURL, controlTimeout)
		}
	}
	result.finalize()
	return result
}

func runAuthProbe(ctx context.Context, target Target, result *Result, baseURL string, controlTimeout time.Duration) {
	if strings.TrimSpace(target.BearerToken) == "" {
		result.addFailure("bearer token is required for authenticated harness conformance")
		return
	}
	request := defaultStartTurnRequest("conformance-auth")
	if result.ObservedCapabilities != nil && strings.TrimSpace(result.ObservedCapabilities.RuntimeName) != "" {
		request.Metadata["runtime"] = strings.TrimSpace(result.ObservedCapabilities.RuntimeName)
	}
	if target.StartTurnRequest != nil {
		request = *target.StartTurnRequest
	}
	assertUnauthenticatedStartRejected(ctx, target, result, baseURL, controlTimeout, request)
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
	if target.RequireAuth && !assertUnauthenticatedStartRejected(ctx, target, result, baseURL, controlTimeout, request) {
		return
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
	if !assertDuplicateStartRejected(ctx, client, result, request) {
		return
	}
	if target.RequireAuth {
		assertUnauthenticatedTurnResourcesRejected(ctx, target, result, baseURL, controlTimeout, request)
	}
	frames := []harness.HarnessEventFrame{}
	var frameBytes int
	streamCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	terminalSeen := false
	terminalDrainScheduled := false
	if err := client.StreamFrames(streamCtx, request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		if len(frames) >= maxProbeFrames {
			return fmt.Errorf("conformance probe frame count exceeded %d", maxProbeFrames)
		}
		frameBytes += approximateProbeFrameBytes(frame)
		if frameBytes > maxProbeFrameBytes {
			return fmt.Errorf("conformance probe frame bytes exceeded %d", maxProbeFrameBytes)
		}
		frames = append(frames, frame)
		if isProbeTerminalFrame(frame.Type) {
			terminalSeen = true
			if !terminalDrainScheduled {
				terminalDrainScheduled = true
				time.AfterFunc(postTerminalDrainTimeout, cancel)
			}
		}
		return nil
	}); err != nil && (!terminalSeen || !probeStreamStoppedByContext(err)) {
		cancelProbeTurn(ctx, client, result, request, "conformance stream failed")
		result.addFailure(fmt.Sprintf("stream frames failed: %v", err))
		return
	}
	if !validateProbeFrames(result, request, frames) {
		cancelProbeTurn(ctx, client, result, request, "conformance probe did not complete")
	}
}

//nolint:gocyclo // Brokered conformance is a compact protocol state-machine probe.
func runBrokeredProbe(
	ctx context.Context,
	target Target,
	result *Result,
	baseURL string,
	controlTimeout time.Duration,
	profile harness.BrokeredToolClass,
) {
	if target.RequireAuth && strings.TrimSpace(target.BearerToken) == "" {
		result.addFailure("bearer token is required for authenticated harness conformance")
		return
	}
	if result.ObservedCapabilities == nil {
		result.addFailure("capabilities are required for brokered conformance")
		return
	}
	if !capabilitiesHaveToolMode(result.ObservedCapabilities, harness.ToolExecutionModeBrokered) {
		result.addFailure(fmt.Sprintf("runtime does not advertise toolExecutionMode %q", harness.ToolExecutionModeBrokered))
		return
	}
	if !capabilitiesHaveBrokeredClass(result.ObservedCapabilities, profile) {
		result.addFailure(fmt.Sprintf("runtime does not advertise brokeredToolClass %q", profile))
		return
	}
	if !result.ObservedCapabilities.SupportsContinuation {
		result.addFailure("runtime does not advertise supportsContinuation")
		return
	}

	request := defaultStartTurnRequest("conformance-brokered-" + string(profile))
	if target.StartTurnRequest != nil {
		request = *target.StartTurnRequest
	}
	request.ToolExecutionMode = harness.ToolExecutionModeBrokered
	if request.Metadata == nil {
		request.Metadata = map[string]string{}
	}
	request.Input.Prompt = fmt.Sprintf("Orka brokered %s conformance probe. Request one brokered tool and complete after the result.", profile)
	request.Input.Tools = []harness.ToolDefinition{{
		Name:          "conformance_" + string(profile),
		Description:   "Synthetic Orka conformance tool schema",
		BrokeredClass: profile,
		Parameters:    json.RawMessage(`{"type":"object","additionalProperties":true}`),
	}}
	if result.ObservedCapabilities != nil && strings.TrimSpace(result.ObservedCapabilities.RuntimeName) != "" {
		request.Metadata["runtime"] = strings.TrimSpace(result.ObservedCapabilities.RuntimeName)
	}
	request.Metadata["brokeredToolClasses"] = string(profile)
	request.Metadata["brokeredToolClass"] = string(profile)
	if target.RequireAuth && !assertUnauthenticatedStartRejected(ctx, target, result, baseURL, controlTimeout, request) {
		return
	}
	client, err := newClient(baseURL, target.BearerToken, target.HTTPClient, controlTimeout)
	if err != nil {
		result.addFailure(fmt.Sprintf("create authenticated client: %v", err))
		return
	}
	started, err := client.StartTurn(ctx, request)
	if err != nil {
		result.addFailure(fmt.Sprintf("start brokered turn failed: %v", err))
		return
	}
	if strings.TrimSpace(started.EventStreamPath) == "" {
		result.addFailure("start turn response eventStreamPath is required")
	}
	if target.RequireAuth {
		assertUnauthenticatedTurnResourcesRejected(ctx, target, result, baseURL, controlTimeout, request)
	}

	streamCtx, streamCancel := context.WithTimeout(ctx, controlTimeout)
	defer streamCancel()
	framesCh := make(chan harness.HarnessEventFrame, maxProbeFrames)
	errCh := make(chan error, 1)
	go func() {
		var frameBytes int
		err := client.StreamFrames(streamCtx, request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
			frameBytes += approximateProbeFrameBytes(frame)
			if frameBytes > maxProbeFrameBytes {
				return fmt.Errorf("conformance probe frame bytes exceeded %d", maxProbeFrameBytes)
			}
			select {
			case framesCh <- frame:
			case <-streamCtx.Done():
				return streamCtx.Err()
			}
			if isProbeTerminalFrame(frame.Type) {
				return nil
			}
			return nil
		})
		errCh <- err
	}()

	frames := []harness.HarnessEventFrame{}
	var requested *harness.HarnessEventFrame
	initialStreamEnded := false
	for requested == nil {
		select {
		case frame := <-framesCh:
			frames = append(frames, frame)
			if err := validateBrokeredProbeFrame(request, frame); err != nil {
				result.addFailure(err.Error())
				cancelProbeTurn(ctx, client, result, request, "invalid brokered conformance frame")
				return
			}
			if frame.Type == harness.FrameToolCallRequested {
				if expected := expectedBrokeredProbeToolName(profile); frame.ToolName != expected {
					result.addFailure(fmt.Sprintf("brokered %s probe requested tool %q, want %q", profile, frame.ToolName, expected))
					return
				}
				copyFrame := frame
				requested = &copyFrame
			}
			if isProbeTerminalFrame(frame.Type) && requested == nil {
				result.addFailure("brokered turn completed before requesting a tool")
				return
			}
		case err := <-errCh:
			for len(framesCh) > 0 && requested == nil {
				frame := <-framesCh
				frames = append(frames, frame)
				if err := validateBrokeredProbeFrame(request, frame); err != nil {
					result.addFailure(err.Error())
					cancelProbeTurn(ctx, client, result, request, "invalid brokered conformance frame")
					return
				}
				if frame.Type == harness.FrameToolCallRequested {
					if expected := expectedBrokeredProbeToolName(profile); frame.ToolName != expected {
						result.addFailure(fmt.Sprintf("brokered %s probe requested tool %q, want %q", profile, frame.ToolName, expected))
						return
					}
					copyFrame := frame
					requested = &copyFrame
					break
				}
				if isProbeTerminalFrame(frame.Type) {
					result.addFailure("brokered turn completed before requesting a tool")
					return
				}
			}
			if requested != nil {
				initialStreamEnded = true
				if err != nil && !probeStreamStoppedByContext(err) {
					result.addFailure(fmt.Sprintf("stream brokered frames failed before continue: %v", err))
					return
				}
				break
			}
			if err != nil {
				result.addFailure(fmt.Sprintf("stream brokered frames failed before tool request: %v", err))
			} else {
				result.addFailure("brokered stream ended before tool request")
			}
			return
		case <-streamCtx.Done():
			cancelProbeTurn(ctx, client, result, request, "brokered conformance timed out waiting for tool request")
			result.addFailure("brokered conformance timed out waiting for tool request")
			return
		}
	}
	if !assertNoBrokeredPreContinueFrames(streamCtx, result, request, framesCh, &frames) {
		cancelProbeTurn(ctx, client, result, request, "brokered runtime emitted result before continue")
		return
	}

	toolResult := harness.ToolCallResult{
		Version:          harness.ProtocolVersion,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		ToolCallID:       requested.ToolCallID,
		IdempotencyKey:   harness.ToolRequestIdempotencyKey(request.RuntimeSessionID, request.TurnID, requested.ToolCallID),
		Approved:         true,
		Output:           json.RawMessage(`{"success":true,"data":{"conformance":"ok"}}`),
	}
	continueRequest := harness.ContinueTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		ToolResults:      []harness.ToolCallResult{toolResult},
	}
	if target.RequireAuth && !assertUnauthenticatedContinueRejected(ctx, target, result, baseURL, controlTimeout, continueRequest) {
		cancelProbeTurn(ctx, client, result, request, "unauthenticated brokered continue was accepted")
		return
	}
	if _, err := client.ContinueTurn(ctx, continueRequest); err != nil {
		cancelProbeTurn(ctx, client, result, request, "brokered conformance continue failed")
		result.addFailure(fmt.Sprintf("continue brokered turn failed: %v", err))
		return
	}
	continueStreamCtx, continueStreamCancel := context.WithTimeout(ctx, controlTimeout)
	defer continueStreamCancel()
	if initialStreamEnded {
		reconnected, sawToolResult, sawTerminal, reconnectErr := streamBrokeredContinuationFrames(
			streamCtx,
			client,
			request,
			*requested,
			maxFrameSeq(frames),
		)
		frames = append(frames, reconnected...)
		if reconnectErr != nil && (!sawTerminal || !probeStreamStoppedByContext(reconnectErr)) {
			result.addFailure(fmt.Sprintf("stream brokered frames after continue failed: %v", reconnectErr))
			return
		}
		if !sawTerminal {
			result.addFailure("brokered stream ended without terminal frame")
		}
		if sawTerminal && !sawToolResult {
			result.addFailure("brokered turn did not acknowledge tool result")
		}
		validateProbeFrames(result, request, frames)
		return
	}

	terminalSeen := false
	toolResultSeen := false
	for !terminalSeen {
		select {
		case frame := <-framesCh:
			frames = append(frames, frame)
			if err := validateBrokeredProbeFrame(request, frame); err != nil {
				result.addFailure(err.Error())
				cancelProbeTurn(ctx, client, result, request, "invalid brokered conformance frame")
				return
			}
			if frame.Type == harness.FrameToolResultReceived && frame.ToolCallID == requested.ToolCallID {
				toolResultSeen = true
			}
			terminalSeen = isProbeTerminalFrame(frame.Type)
		case err := <-errCh:
			for len(framesCh) > 0 && !terminalSeen {
				frame := <-framesCh
				frames = append(frames, frame)
				if frame.Type == harness.FrameToolResultReceived && frame.ToolCallID == requested.ToolCallID {
					toolResultSeen = true
				}
				terminalSeen = isProbeTerminalFrame(frame.Type)
			}
			if err != nil && (!terminalSeen || !probeStreamStoppedByContext(err)) {
				result.addFailure(fmt.Sprintf("stream brokered frames failed: %v", err))
				return
			}
			if !terminalSeen {
				reconnected, sawToolResult, sawTerminal, reconnectErr := streamBrokeredContinuationFrames(
					continueStreamCtx,
					client,
					request,
					*requested,
					maxFrameSeq(frames),
				)
				frames = append(frames, reconnected...)
				toolResultSeen = toolResultSeen || sawToolResult
				terminalSeen = sawTerminal
				if reconnectErr != nil && (!terminalSeen || !probeStreamStoppedByContext(reconnectErr)) {
					result.addFailure(fmt.Sprintf("stream brokered frames after continue failed: %v", reconnectErr))
					return
				}
			}
			if !terminalSeen {
				result.addFailure("brokered stream ended without terminal frame")
			}
			if terminalSeen && !toolResultSeen {
				result.addFailure("brokered turn did not acknowledge tool result")
			}
			validateProbeFrames(result, request, frames)
			return
		case <-streamCtx.Done():
			cancelProbeTurn(ctx, client, result, request, "brokered conformance timed out waiting for terminal frame")
			result.addFailure("brokered conformance timed out waiting for terminal frame")
			return
		}
	}
	if !toolResultSeen {
		result.addFailure("brokered turn did not acknowledge tool result")
	}
	validateProbeFrames(result, request, frames)
}

func expectedBrokeredProbeToolName(profile harness.BrokeredToolClass) string {
	return "conformance_" + string(profile)
}

func streamBrokeredContinuationFrames(
	ctx context.Context,
	client *harness.Client,
	request harness.StartTurnRequest,
	requested harness.HarnessEventFrame,
	afterSeq int64,
) ([]harness.HarnessEventFrame, bool, bool, error) {
	frames := []harness.HarnessEventFrame{}
	toolResultSeen := false
	terminalSeen := false
	err := client.StreamFrames(ctx, request.TurnID, afterSeq, func(frame harness.HarnessEventFrame) error {
		if err := validateBrokeredProbeFrame(request, frame); err != nil {
			return err
		}
		frames = append(frames, frame)
		if frame.Type == harness.FrameToolResultReceived && frame.ToolCallID == requested.ToolCallID {
			toolResultSeen = true
		}
		terminalSeen = isProbeTerminalFrame(frame.Type)
		return nil
	})
	return frames, toolResultSeen, terminalSeen, err
}

func maxFrameSeq(frames []harness.HarnessEventFrame) int64 {
	var maxSeq int64
	for _, frame := range frames {
		if frame.Seq > maxSeq {
			maxSeq = frame.Seq
		}
	}
	return maxSeq
}

func assertNoBrokeredPreContinueFrames(
	ctx context.Context,
	result *Result,
	request harness.StartTurnRequest,
	framesCh <-chan harness.HarnessEventFrame,
	frames *[]harness.HarnessEventFrame,
) bool {
	timer := time.NewTimer(postTerminalDrainTimeout)
	defer timer.Stop()
	for {
		select {
		case frame := <-framesCh:
			*frames = append(*frames, frame)
			if err := validateBrokeredProbeFrame(request, frame); err != nil {
				result.addFailure(err.Error())
				return false
			}
			if frame.Type == harness.FrameToolResultReceived || isProbeTerminalFrame(frame.Type) {
				result.addFailure("brokered runtime emitted tool result or terminal frame before continue")
				return false
			}
		case <-timer.C:
			return true
		case <-ctx.Done():
			result.addFailure("brokered conformance context ended before continue")
			return false
		}
	}
}

func validateBrokeredProbeFrame(request harness.StartTurnRequest, frame harness.HarnessEventFrame) error {
	if err := frame.ValidateRequired(); err != nil {
		return fmt.Errorf("invalid brokered frame: %w", err)
	}
	if frame.RuntimeSessionID != request.RuntimeSessionID || frame.TurnID != request.TurnID || frame.CorrelationID != request.CorrelationID {
		return fmt.Errorf("brokered frame identity does not match requested turn")
	}
	return nil
}

func capabilitiesHaveToolMode(caps *harness.CapabilitiesResponse, want harness.ToolExecutionMode) bool {
	if caps == nil {
		return false
	}
	return slices.Contains(caps.ToolExecutionModes, want)
}

func capabilitiesHaveBrokeredClass(caps *harness.CapabilitiesResponse, want harness.BrokeredToolClass) bool {
	if caps == nil {
		return false
	}
	return slices.Contains(caps.BrokeredToolClasses, want)
}

func assertDuplicateStartRejected(ctx context.Context, client *harness.Client, result *Result, request harness.StartTurnRequest) bool {
	started, err := client.StartTurn(ctx, request)
	if err == nil {
		expectedPath, pathErr := harness.EventStreamPath(request.TurnID)
		if pathErr == nil &&
			started.RuntimeSessionID == request.RuntimeSessionID &&
			started.TurnID == request.TurnID &&
			started.CorrelationID == request.CorrelationID &&
			strings.TrimSpace(started.EventStreamPath) == expectedPath {
			return true
		}
		result.addFailure("duplicate start turn was accepted with mismatched identity")
		cancelProbeTurn(ctx, client, result, request, "duplicate conformance start identity mismatch")
		return false
	}
	if !isDuplicateStartRejectedError(err) {
		result.addFailure(fmt.Sprintf("duplicate start turn returned %v, want deterministic already-started rejection or identical response", err))
		cancelProbeTurn(ctx, client, result, request, "duplicate conformance start returned an unexpected error")
		return false
	}
	return true
}

func isDuplicateStartRejectedError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "turn already exists") || strings.Contains(message, "turn already completed")
}

func approximateProbeFrameBytes(frame harness.HarnessEventFrame) int {
	encoded, err := json.Marshal(frame)
	if err == nil {
		return len(encoded)
	}
	size := len(frame.Version) + len(frame.Type) + len(frame.RuntimeSessionID) + len(frame.TurnID) + len(frame.CorrelationID) +
		len(frame.Severity) + len(frame.Summary) + len(frame.Content) + len(frame.ContentText) + len(frame.ToolName) +
		len(frame.ToolCallID) + len(frame.ApprovalID)
	for key, value := range frame.Metadata {
		size += len(key) + len(value)
	}
	return size
}

func assertUnauthenticatedContinueRejected(
	ctx context.Context,
	target Target,
	result *Result,
	baseURL string,
	controlTimeout time.Duration,
	request harness.ContinueTurnRequest,
) bool {
	unauth, err := newClient(baseURL, "", target.HTTPClient, controlTimeout)
	if err != nil {
		result.addFailure(fmt.Sprintf("create unauthenticated client: %v", err))
		return false
	}
	if _, err := unauth.ContinueTurn(ctx, request); err == nil {
		result.addFailure("unauthenticated continue turn was accepted")
		return false
	} else if !isAuthRequiredError(err) {
		result.addFailure(fmt.Sprintf("unauthenticated continue turn returned %v, want 401/403", err))
		return false
	}
	return true
}

func assertUnauthenticatedStartRejected(
	ctx context.Context,
	target Target,
	result *Result,
	baseURL string,
	controlTimeout time.Duration,
	request harness.StartTurnRequest,
) bool {
	unauth, err := newClient(baseURL, "", target.HTTPClient, controlTimeout)
	if err != nil {
		result.addFailure(fmt.Sprintf("create unauthenticated client: %v", err))
		return false
	}
	probe := request
	probe.TurnID = harness.HarnessTurnID(string(request.TurnID) + "-unauth")
	probe.CorrelationID = request.CorrelationID + "-unauth"
	if _, err := unauth.StartTurn(ctx, probe); err == nil {
		if client, clientErr := newClient(baseURL, target.BearerToken, target.HTTPClient, controlTimeout); clientErr == nil {
			cancelProbeTurn(ctx, client, result, probe, "unauthenticated conformance start was accepted")
		}
		result.addFailure("unauthenticated start turn was accepted")
		return false
	} else if !isAuthRequiredError(err) {
		result.addFailure(fmt.Sprintf("unauthenticated start turn returned %v, want 401/403", err))
		return false
	}
	return true
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
	streamCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	if err := unauth.StreamFrames(streamCtx, request.TurnID, 0, func(harness.HarnessEventFrame) error { return nil }); err == nil {
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

func cancelProbeTurn(ctx context.Context, client *harness.Client, result *Result, request harness.StartTurnRequest, reason string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupProbeTimeout)
	defer cancel()
	_, err := client.CancelTurn(cleanupCtx, harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Reason:           reason,
	})
	if err != nil {
		result.addFailure(fmt.Sprintf("cancel probe turn failed: %v", err))
	}
}

func isProbeTerminalFrame(frameType harness.FrameType) bool {
	switch frameType {
	case harness.FrameTurnCompleted, harness.FrameTurnFailed, harness.FrameTurnCancelled:
		return true
	default:
		return false
	}
}

func probeStreamStoppedByContext(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "context canceled") || strings.Contains(message, "context deadline exceeded")
}

func validateProbeFrames(result *Result, request harness.StartTurnRequest, frames []harness.HarnessEventFrame) bool {
	if len(frames) == 0 {
		result.addFailure("stream returned no frames")
		return false
	}
	terminal := 0
	completed := 0
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
		case harness.FrameTurnCompleted:
			terminal++
			completed++
		case harness.FrameTurnFailed, harness.FrameTurnCancelled:
			terminal++
		}
	}
	if terminal != 1 {
		result.addFailure(fmt.Sprintf("terminal frame count = %d, want exactly 1", terminal))
	}
	if completed != 1 {
		result.addFailure(fmt.Sprintf("completed terminal frame count = %d, want exactly 1", completed))
	}
	return terminal == 1 && completed == 1
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
