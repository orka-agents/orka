package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
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
	maxProbeFrameBytes       = 8 << 20
)

var errProbeStreamShutdownTimeout = errors.New("timed out waiting for brokered stream shutdown")

// Target identifies a harness endpoint to probe. BearerToken is used only for
// authenticated control-plane endpoints and is never included in Result.
type Target struct {
	BaseURL        string
	BearerToken    string
	HTTPClient     *http.Client
	ControlTimeout time.Duration

	probeReadiness bool

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
	target.probeReadiness = true
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
	if err := validateProbeSelection(target); err != nil {
		return failed(err.Error())
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

	if len(result.Failures) == 0 && target.probeReadiness && !hasTurnProbe(target) {
		runReadinessProbes(ctx, target, &result, baseURL, controlTimeout)
	} else if len(result.Failures) == 0 {
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

func validateProbeSelection(target Target) error {
	count := 0
	for _, selected := range []bool{
		target.ProbeTurn,
		target.ProbeBrokeredRead,
		target.ProbeBrokeredWrite,
		target.ProbeBrokeredCoordination,
	} {
		if selected {
			count++
		}
	}
	if count > 1 {
		return fmt.Errorf("only one turn conformance probe may be selected")
	}
	return nil
}

func hasTurnProbe(target Target) bool {
	return target.ProbeTurn || target.ProbeBrokeredRead || target.ProbeBrokeredWrite ||
		target.ProbeBrokeredCoordination
}

func runReadinessProbes(
	ctx context.Context,
	target Target,
	result *Result,
	baseURL string,
	controlTimeout time.Duration,
) {
	capabilities := result.ObservedCapabilities
	if capabilities == nil {
		return
	}
	if slices.Contains(capabilities.ToolExecutionModes, harness.ToolExecutionModeObserved) {
		runTurnProbe(ctx, target, result, baseURL, controlTimeout)
		return
	}
	if !slices.Contains(capabilities.ToolExecutionModes, harness.ToolExecutionModeBrokered) {
		result.addFailure("runtime does not advertise a probeable tool execution mode")
		return
	}
	seen := make(map[harness.BrokeredToolClass]struct{}, len(capabilities.BrokeredToolClasses))
	for _, class := range capabilities.BrokeredToolClasses {
		if _, duplicate := seen[class]; duplicate {
			continue
		}
		seen[class] = struct{}{}
		probeTarget := brokeredReadinessTarget(target, class)
		runBrokeredProbe(ctx, probeTarget, result, baseURL, controlTimeout, class)
	}
}

func brokeredReadinessTarget(target Target, class harness.BrokeredToolClass) Target {
	if target.StartTurnRequest == nil {
		return target
	}
	request := cloneStartTurnRequest(*target.StartTurnRequest)
	suffix := "-" + string(class) + "-" + uuid.NewString()
	request.SessionName += suffix
	request.RuntimeSessionID = harness.RuntimeSessionID(string(request.RuntimeSessionID) + suffix)
	request.TurnID = harness.HarnessTurnID(string(request.TurnID) + suffix)
	request.CorrelationID += suffix
	target.StartTurnRequest = &request
	return target
}

func probeStreamContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline.Add(postTerminalDrainTimeout)
	}
	streamCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	stopParentCancel := context.AfterFunc(ctx, func() {
		if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			cancel()
		}
	})
	return streamCtx, func() {
		stopParentCancel()
		cancel()
	}
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
		request = cloneStartTurnRequest(*target.StartTurnRequest)
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
		request = cloneStartTurnRequest(*target.StartTurnRequest)
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
		if result.ObservedCapabilities != nil && result.ObservedCapabilities.SupportsCancel &&
			startTurnMayHaveBeenAccepted(err) {
			bestEffortCancelProbeTurn(ctx, client, request, "start turn response validation failed")
		}
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
		request = cloneStartTurnRequest(*target.StartTurnRequest)
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
		Parameters:    json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
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
		if result.ObservedCapabilities.SupportsCancel && startTurnMayHaveBeenAccepted(err) {
			bestEffortCancelProbeTurn(ctx, client, request, "brokered start response validation failed")
		}
		result.addFailure(fmt.Sprintf("start brokered turn failed: %v", err))
		return
	}
	frames := []harness.HarnessEventFrame{}
	defer func() {
		if result.ObservedCapabilities.SupportsCancel && !probeFramesHaveValidTerminal(request, frames) {
			cancelProbeTurn(ctx, client, result, request, "brokered conformance probe cleanup")
		}
	}()
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
	emitFrame := newBrokeredProbeFrameEmitter(framesCh)
	go func() {
		errCh <- client.StreamFrames(streamCtx, request.TurnID, 0, emitFrame)
	}()

	var requested *harness.HarnessEventFrame
	initialStreamEnded := false
	for requested == nil {
		select {
		case frame := <-framesCh:
			frames = append(frames, frame)
			if err := validateBrokeredProbeFrame(request, frame); err != nil {
				result.addFailure(err.Error())
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
			result.addFailure("brokered conformance timed out waiting for tool request")
			return
		}
	}
	if !assertNoBrokeredPreContinueFrames(streamCtx, result, request, framesCh, &frames) {
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
		return
	}
	if _, err := client.ContinueTurn(ctx, continueRequest); err != nil {
		result.addFailure(fmt.Sprintf("continue brokered turn failed: %v", err))
		return
	}
	continueStreamCtx, continueStreamCancel := probeStreamContext(ctx, controlTimeout)
	defer continueStreamCancel()
	if initialStreamEnded {
		reconnected, sawToolResult, sawTerminal, reconnectErr := streamBrokeredContinuationFrames(
			continueStreamCtx,
			client,
			request,
			*requested,
			maxFrameSeq(frames),
			len(frames),
			probeFramesBytes(frames),
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
	var terminalDrainTimer *time.Timer
	var terminalDrain <-chan time.Time
	recordFrame := func(frame harness.HarnessEventFrame) bool {
		frames = append(frames, frame)
		if err := validateBrokeredProbeFrame(request, frame); err != nil {
			result.addFailure(err.Error())
			return false
		}
		if frame.Type == harness.FrameToolResultReceived && frame.ToolCallID == requested.ToolCallID {
			toolResultSeen = true
		}
		if isProbeTerminalFrame(frame.Type) {
			terminalSeen = true
			if terminalDrainTimer == nil {
				terminalDrainTimer = time.NewTimer(postTerminalDrainTimeout)
				terminalDrain = terminalDrainTimer.C
			}
		}
		return true
	}
	defer func() {
		if terminalDrainTimer != nil {
			terminalDrainTimer.Stop()
		}
	}()
	finish := func() {
		if !terminalSeen {
			result.addFailure("brokered stream ended without terminal frame")
		}
		if terminalSeen && !toolResultSeen {
			result.addFailure("brokered turn did not acknowledge tool result")
		}
		validateProbeFrames(result, request, frames)
	}
	for {
		select {
		case frame := <-framesCh:
			if !recordFrame(frame) {
				return
			}
		case err := <-errCh:
			for len(framesCh) > 0 {
				if !recordFrame(<-framesCh) {
					return
				}
			}
			if err != nil && (!terminalSeen || !probeStreamStoppedByContext(err)) {
				result.addFailure(fmt.Sprintf("stream brokered frames failed: %v", err))
				return
			}
			if terminalSeen {
				finish()
				return
			}
			reconnected, sawToolResult, sawTerminal, reconnectErr := streamBrokeredContinuationFrames(
				continueStreamCtx,
				client,
				request,
				*requested,
				maxFrameSeq(frames),
				len(frames),
				probeFramesBytes(frames),
			)
			frames = append(frames, reconnected...)
			toolResultSeen = toolResultSeen || sawToolResult
			terminalSeen = sawTerminal
			if reconnectErr != nil && (!terminalSeen || !probeStreamStoppedByContext(reconnectErr)) {
				result.addFailure(fmt.Sprintf("stream brokered frames after continue failed: %v", reconnectErr))
				return
			}
			finish()
			return
		case <-terminalDrain:
			streamErr, drained := stopProbeStreamAndDrainFrames(streamCancel, framesCh, errCh, recordFrame)
			if !drained {
				return
			}
			if streamErr != nil && !probeStreamStoppedByContext(streamErr) {
				result.addFailure(fmt.Sprintf("stream brokered frames failed: %v", streamErr))
				return
			}
			finish()
			return
		case <-continueStreamCtx.Done():
			streamErr, drained := stopProbeStreamAndDrainFrames(streamCancel, framesCh, errCh, recordFrame)
			if !drained {
				return
			}
			if streamErr != nil && (!terminalSeen || !probeStreamStoppedByContext(streamErr)) {
				result.addFailure(fmt.Sprintf("stream brokered frames failed: %v", streamErr))
				return
			}
			if terminalSeen {
				finish()
				return
			}
			result.addFailure("brokered conformance timed out waiting for terminal frame")
			return
		}
	}

}

func newBrokeredProbeFrameEmitter(
	framesCh chan<- harness.HarnessEventFrame,
) func(harness.HarnessEventFrame) error {
	frameCount := 0
	frameBytes := 0
	return func(frame harness.HarnessEventFrame) error {
		if frameCount >= maxProbeFrames {
			return fmt.Errorf("conformance probe frame count exceeded %d", maxProbeFrames)
		}
		frameCount++
		frameBytes += approximateProbeFrameBytes(frame)
		if frameBytes > maxProbeFrameBytes {
			return fmt.Errorf("conformance probe frame bytes exceeded %d", maxProbeFrameBytes)
		}
		// The production channel can hold every permitted frame, so publish an
		// already-decoded frame before StreamFrames observes cancellation. Selecting
		// on the stream context here could discard a terminal frame at the deadline.
		framesCh <- frame
		return nil
	}
}

func stopProbeStreamAndDrainFrames(
	streamCancel context.CancelFunc,
	framesCh <-chan harness.HarnessEventFrame,
	errCh <-chan error,
	recordFrame func(harness.HarnessEventFrame) bool,
) (error, bool) {
	streamCancel()
	shutdownTimer := time.NewTimer(postTerminalDrainTimeout)
	defer shutdownTimer.Stop()
	for {
		select {
		case frame := <-framesCh:
			if !recordFrame(frame) {
				return nil, false
			}
		case streamErr := <-errCh:
			for len(framesCh) > 0 {
				if !recordFrame(<-framesCh) {
					return streamErr, false
				}
			}
			return streamErr, true
		case <-shutdownTimer.C:
			for {
				select {
				case frame := <-framesCh:
					if !recordFrame(frame) {
						return errProbeStreamShutdownTimeout, false
					}
				case streamErr := <-errCh:
					for len(framesCh) > 0 {
						if !recordFrame(<-framesCh) {
							return streamErr, false
						}
					}
					return streamErr, true
				default:
					return errProbeStreamShutdownTimeout, true
				}
			}
		}
	}
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
	existingFrameCount int,
	existingFrameBytes int,
) ([]harness.HarnessEventFrame, bool, bool, error) {
	frames := []harness.HarnessEventFrame{}
	toolResultSeen := false
	terminalSeen := false
	frameBytes := 0
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	var terminalDrainOnce sync.Once
	err := client.StreamFrames(streamCtx, request.TurnID, afterSeq, func(frame harness.HarnessEventFrame) error {
		if existingFrameCount+len(frames) >= maxProbeFrames {
			return fmt.Errorf("conformance probe frame count exceeded %d", maxProbeFrames)
		}
		frameBytes += approximateProbeFrameBytes(frame)
		if existingFrameBytes+frameBytes > maxProbeFrameBytes {
			return fmt.Errorf("conformance probe frame bytes exceeded %d", maxProbeFrameBytes)
		}
		if err := validateBrokeredProbeFrame(request, frame); err != nil {
			return err
		}
		frames = append(frames, frame)
		if frame.Type == harness.FrameToolResultReceived && frame.ToolCallID == requested.ToolCallID {
			toolResultSeen = true
		}
		if isProbeTerminalFrame(frame.Type) {
			terminalSeen = true
			terminalDrainOnce.Do(func() {
				time.AfterFunc(postTerminalDrainTimeout, streamCancel)
			})
		}
		return nil
	})
	if terminalSeen && probeStreamStoppedByContext(err) {
		err = nil
	}
	return frames, toolResultSeen, terminalSeen, err
}

func probeFramesBytes(frames []harness.HarnessEventFrame) int {
	total := 0
	for _, frame := range frames {
		total += approximateProbeFrameBytes(frame)
	}
	return total
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

func startTurnMayHaveBeenAccepted(err error) bool {
	var clientErr harness.ClientError
	if !errors.As(err, &clientErr) {
		return false
	}
	return clientErr.RemoteAccepted || clientErr.StatusCode >= http.StatusOK && clientErr.StatusCode < http.StatusMultipleChoices
}

func bestEffortCancelProbeTurn(
	ctx context.Context,
	client *harness.Client,
	request harness.StartTurnRequest,
	reason string,
) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupProbeTimeout)
	defer cancel()
	_, _ = client.CancelTurn(cleanupCtx, harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Reason:           reason,
	})
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
	terminalSeen := false
	var previousSeq int64
	for i, frame := range frames {
		if terminalSeen {
			result.addFailure(fmt.Sprintf("frame %d appears after terminal frame", i))
		}
		if err := frame.ValidateRequired(); err != nil {
			result.addFailure(fmt.Sprintf("frame %d is invalid: %v", i, err))
		}
		if i > 0 && frame.Seq <= previousSeq {
			result.addFailure(fmt.Sprintf("frame %d seq %d is not strictly greater than %d", i, frame.Seq, previousSeq))
		}
		previousSeq = frame.Seq
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
			terminalSeen = true
		case harness.FrameTurnFailed, harness.FrameTurnCancelled:
			terminal++
			terminalSeen = true
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

func cloneStartTurnRequest(request harness.StartTurnRequest) harness.StartTurnRequest {
	cloned := request
	cloned.Metadata = maps.Clone(request.Metadata)
	cloned.Input.Env = slices.Clone(request.Input.Env)
	cloned.Input.ContextRefs = slices.Clone(request.Input.ContextRefs)
	cloned.Input.Tools = slices.Clone(request.Input.Tools)
	return cloned
}

func probeFramesHaveValidTerminal(
	request harness.StartTurnRequest,
	frames []harness.HarnessEventFrame,
) bool {
	for _, frame := range frames {
		if !isProbeTerminalFrame(frame.Type) || frame.ValidateRequired() != nil || !harness.IsKnownFrameType(frame.Type) {
			continue
		}
		if frame.RuntimeSessionID == request.RuntimeSessionID && frame.TurnID == request.TurnID &&
			frame.CorrelationID == request.CorrelationID {
			return true
		}
	}
	return false
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
	var clientErr harness.ClientError
	if !errors.As(err, &clientErr) {
		return false
	}
	return clientErr.StatusCode == http.StatusUnauthorized || clientErr.StatusCode == http.StatusForbidden
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
