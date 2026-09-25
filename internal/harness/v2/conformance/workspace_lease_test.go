package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type workspaceLeaseTransport struct {
	state     *lifecycleProbeState
	delay     time.Duration
	tailDelay time.Duration
	fault     string
	mu        sync.Mutex
	start     harnessv2.StartPromptRequest
	lease     harnessv2.PromptLease
	renewals  []harnessv2.RenewPromptLeaseRequest
	starts    int
	cancels   int
	closed    chan struct{}
	writer    chan struct{}
	changed   chan struct{}
	terminal  harnessv2.PromptSettlement
}

type workspaceLeaseBody struct {
	*io.PipeReader
	once   sync.Once
	closed chan struct{}
}

func (b *workspaceLeaseBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return b.PipeReader.Close()
}

func workspaceLeaseJSON(value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(data)))}
}

func (f *workspaceLeaseTransport) validate(r *http.Request, metadata harnessv2.MutationMetadata, value any) error {
	if err := metadata.ValidateDigest(value); err != nil {
		return err
	}
	if r.Header.Get("Authorization") != "Bearer "+f.state.target.ControllerBearerToken {
		return errors.New("missing original controller authorization")
	}
	return harnessv2.VerifyOperationCapability(f.state.target.OperationCapabilitySecret,
		r.Header.Get(harnessv2.OperationCapabilityHeader), metadata, true, time.Now().UTC())
}

func (f *workspaceLeaseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/lease") {
		var request harnessv2.RenewPromptLeaseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if err := f.validate(r, request.Metadata, request); err != nil {
			return nil, err
		}
		limits := f.state.target.Limits
		now := time.Now().UTC()
		if err := request.Lease.ValidateAt(now, time.Duration(limits.MinPromptLeaseMillis)*time.Millisecond,
			time.Duration(limits.MaxPromptLeaseMillis)*time.Millisecond); err != nil {
			return nil, err
		}
		if err := harnessv2.ValidatePromptLeaseRenewal(f.lease, request.Lease, request.ExpectedLeaseGeneration,
			now, time.Duration(limits.MaxPromptLeaseMillis)*time.Millisecond); err != nil {
			return nil, err
		}
		if err := request.MCPAuthorization.ValidateForAt(request.Metadata, request.Lease, now); err != nil {
			return nil, err
		}
		original := f.start.Metadata
		if request.Metadata.Fence != original.Fence || request.Metadata.TaskUID != original.TaskUID ||
			request.Metadata.TaskAttempt != original.TaskAttempt || request.Metadata.PromptID != original.PromptID ||
			!request.Metadata.ExpiresAt.Equal(f.lease.ExpiresAt) || !f.lease.ActiveAt(now) {
			return nil, errors.New("renewal changed original authority or crossed its expiry")
		}
		for _, prior := range f.renewals {
			if prior.Metadata.OperationID == request.Metadata.OperationID {
				return nil, errors.New("renewal operation replayed")
			}
		}
		f.renewals = append(f.renewals, request)
		if f.fault == "stalled" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		if f.fault == "ambiguous" {
			// Apply first, then lose the ACK. The client may not infer it failed
			// or repeat this request under a new operation identity.
			f.lease = request.Lease
			return nil, io.ErrUnexpectedEOF
		}
		f.lease = request.Lease
		f.changed <- struct{}{}
		response := harnessv2.PromptLeaseResponse{Protocol: harnessv2.ProtocolVersion,
			Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh}, Lease: request.Lease}
		if f.fault == "changed-ack" {
			response.Lease.Generation++
		}
		return workspaceLeaseJSON(response), nil
	}
	if strings.HasSuffix(r.URL.Path, "/cancel") {
		var request harnessv2.CancelPromptRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if err := f.validate(r, request.Metadata, request); err != nil {
			return nil, err
		}
		if !request.Metadata.ExpiresAt.Equal(f.lease.ExpiresAt) || f.terminal.TerminalEvent != harnessv2.EventCompleted {
			return nil, errors.New("cancellation did not retain latest confirmed lease and completed stream")
		}
		f.cancels++
		return workspaceLeaseJSON(harnessv2.CancelPromptResponse{Protocol: harnessv2.ProtocolVersion,
			Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
			BarrierState:   harnessv2.CancellationBarrierSettled, SettlementProven: true, Settlement: f.terminal}), nil
	}
	if f.starts != 0 || r.Method != http.MethodPut {
		return nil, errors.New("prompt replay or unexpected operation")
	}
	if err := json.NewDecoder(r.Body).Decode(&f.start); err != nil {
		return nil, err
	}
	if err := f.validate(r, f.start.Metadata, f.start); err != nil {
		return nil, err
	}
	f.starts++
	f.lease = f.start.Lease
	reader, writer := io.Pipe()
	body := &workspaceLeaseBody{PipeReader: reader, closed: f.closed}
	go f.stream(writer)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {harnessv2.NDJSONMediaType}}, Body: body}, nil
}

func (f *workspaceLeaseTransport) event(kind harnessv2.EventType, sequence uint64) harnessv2.Event {
	m := f.start.Metadata
	return harnessv2.Event{Protocol: harnessv2.ProtocolVersion, Type: kind, Identity: harnessv2.EventIdentity{
		RuntimeInstanceID: m.Fence.RuntimeInstanceID, SupervisorBootID: m.Fence.SupervisorBootID,
		RuntimeSessionUID: m.Fence.RuntimeSessionUID, RuntimeSessionGeneration: m.Fence.RuntimeSessionGeneration,
		TaskUID: m.TaskUID, TaskAttempt: m.TaskAttempt, PromptID: m.PromptID, RequestDigest: m.RequestDigest,
		Sequence: sequence, Timestamp: time.Now().UTC()}}
}

func (f *workspaceLeaseTransport) stream(writer *io.PipeWriter) {
	defer close(f.writer)
	defer writer.Close() //nolint:errcheck
	encoder := json.NewEncoder(writer)
	accepted := f.event(harnessv2.EventAccepted, 1)
	accepted.Accepted = &harnessv2.AcceptedEvent{AcceptedAt: accepted.Identity.Timestamp, Lease: f.start.Lease, ACPVersion: harnessv2.ACPProfileV1}
	if encoder.Encode(accepted) != nil {
		return
	}
	completion := time.NewTimer(f.delay)
	defer completion.Stop()
	for {
		f.mu.Lock()
		expiry := f.lease.ExpiresAt
		f.mu.Unlock()
		leaseTimer := time.NewTimer(time.Until(expiry))
		select {
		case <-f.closed:
			leaseTimer.Stop()
			return
		case <-f.changed:
			leaseTimer.Stop()
			continue
		case <-completion.C:
			leaseTimer.Stop()
			f.mu.Lock()
			f.terminal = harnessv2.PromptSettlement{TerminalEvent: harnessv2.EventCompleted,
				Outcome: harnessv2.PromptOutcomeSucceeded, StopReason: harnessv2.ACPStopReasonEndTurn, SettledAt: time.Now().UTC()}
			f.mu.Unlock()
			event := f.event(harnessv2.EventCompleted, 2)
			event.Completed = &harnessv2.CompletedEvent{StopReason: harnessv2.ACPStopReasonEndTurn,
				Result: harnessv2.PromptResult{Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "ok"}}}}
			_ = encoder.Encode(event)
			if f.tailDelay > 0 {
				select {
				case <-time.After(f.tailDelay):
				case <-f.closed:
				}
			}
			return
		case <-leaseTimer.C:
			f.mu.Lock()
			f.terminal = harnessv2.PromptSettlement{TerminalEvent: harnessv2.EventFailed, SettledAt: time.Now().UTC()}
			f.mu.Unlock()
			event := f.event(harnessv2.EventFailed, 2)
			event.Failed = &harnessv2.FailedEvent{Code: "lease_expired", Message: "initial lease elapsed during provisioning"}
			_ = encoder.Encode(event)
			return
		}
	}
}

func newWorkspaceLeaseTest(t *testing.T, delay time.Duration, fault string, limits harnessv2.ProtocolLimits) (*lifecycleProbeState, *workspaceLeaseTransport) {
	t.Helper()
	policy := harnessv2.MCPToolPolicy{AllowedToolNames: []string{}, Tools: []harnessv2.MCPToolDescriptor{}}
	policy.DescriptorDigest, _ = harnessv2.CanonicalMCPToolDescriptorDigest(policy.Tools)
	toolDigest, _ := harnessv2.CanonicalRuntimeToolPolicyDigest(policy.AllowedToolNames, policy.DisallowedToolNames, false)
	approvalDigest, _ := harnessv2.CanonicalMCPApprovalPolicyDigest(harnessv2.MCPApprovalPolicy{})
	configurationDigest, _ := harnessv2.CanonicalMCPConfigurationDigest(policy.AllowedToolNames)
	target := Target{ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		Limits: limits, ToolPolicy: policy,
		Profile: harnessv2.RuntimeProfile{ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest,
			MCPConfigurationDigest: configurationDigest, WorkspaceIntent: harnessv2.WorkspaceIntentRead}}
	fence := harnessv2.Fence{RuntimeInstanceID: "original-instance", SupervisorBootID: "original-boot", ControllerEpoch: 1,
		RuntimePoolUID: "original-pool", RuntimePoolGeneration: 1, RuntimeProfileDigest: harnessv2.ProfileDigest(digestString("profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion}
	f := &workspaceLeaseTransport{delay: delay, fault: fault,
		closed: make(chan struct{}), writer: make(chan struct{}), changed: make(chan struct{}, 8)}
	httpClient := &http.Client{Transport: f}
	client, err := harnessv2.NewClient("http://conformance.invalid", harnessv2.WithHTTPClient(httpClient),
		harnessv2.WithControllerBearerToken(target.ControllerBearerToken), harnessv2.WithOperationCapabilitySecret(target.OperationCapabilitySecret),
		harnessv2.WithProtocolLimits(target.Limits))
	if err != nil {
		t.Fatal(err)
	}
	state := newLifecycleProbeState(httpClient, client, target, fence, "workspace-renewal")
	f.state = state
	return state, f
}

func TestWorkspaceConformanceRenewsOriginalLeaseDuringDelayedCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, transport := newWorkspaceLeaseTest(t, 35*time.Second, "", harnessv2.DefaultProtocolLimits())
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		request, settlement, err := state.probeCompletedPromptLifecycle(ctx, "workspace-renewal")
		if err != nil {
			t.Fatalf("workspace prompt failed while waiting for provisioning: %v", err)
		}
		if settlement.TerminalEvent != harnessv2.EventCompleted || !settlement.SettledAt.After(request.Lease.ExpiresAt) ||
			transport.starts != 1 || len(transport.renewals) < 2 || transport.cancels != 1 || state.lease.Generation != 3 {
			t.Fatal("original prompt did not survive initial expiry with exact lease renewal and settlement")
		}
		before := len(transport.renewals)
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(transport.renewals) != before {
			t.Fatal("renewer remained active after settlement")
		}
		select {
		case <-transport.writer:
		default:
			t.Fatal("original stream was not joined before return")
		}
	})
}

func TestWorkspaceConformanceRenewalFailureClosesOriginalStreamWithoutReplay(t *testing.T) {
	for _, fault := range []string{"ambiguous", "changed-ack", "stalled"} {
		t.Run(fault, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state, transport := newWorkspaceLeaseTest(t, time.Minute, fault, harnessv2.DefaultProtocolLimits())
				started := time.Now()
				ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				defer cancel()
				_, _, err := state.probeCompletedPromptLifecycle(ctx, "workspace-renewal")
				if err == nil || !strings.Contains(err.Error(), "renew workspace prompt lease") {
					t.Fatalf("renewal failure was not retained: %v", err)
				}
				synctest.Wait()
				if transport.starts != 1 || len(transport.renewals) != 1 || transport.cancels != 0 || state.lease.Generation != 1 {
					t.Fatal("uncertain renewal was replayed, adopted, or settled as success")
				}
				if fault == "stalled" && time.Since(started) != 30*time.Second {
					t.Fatal("uncertain renewal outlived the last confirmed lease")
				}
				select {
				case <-transport.writer:
				default:
					t.Fatal("failed renewal left original stream active")
				}
			})
		})
	}
}

func TestWorkspaceConformanceRenewalRemainsBoundedByProbeContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, transport := newWorkspaceLeaseTest(t, 2*time.Minute, "", harnessv2.DefaultProtocolLimits())
		started := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
		defer cancel()
		_, _, err := state.probeCompletedPromptLifecycle(ctx, "workspace-renewal")
		synctest.Wait()
		deadline, _ := ctx.Deadline()
		// The lease terminal and caller deadline become ready at the same
		// instant. Either may win, but only the original expiry at that exact
		// deadline may substitute for context.DeadlineExceeded.
		leaseExpiredAtDeadline := err != nil && err.Error() == "workspace probe prompt did not complete successfully" &&
			transport.terminal.TerminalEvent == harnessv2.EventFailed && transport.terminal.SettledAt.Equal(deadline) &&
			state.lease.ExpiresAt.Equal(deadline)
		if (!errors.Is(err, context.DeadlineExceeded) && !leaseExpiredAtDeadline) || time.Since(started) != 50*time.Second {
			t.Fatalf("probe did not stop at its bounded context: %v", err)
		}
		for _, request := range transport.renewals {
			if request.Lease.ExpiresAt.After(deadline) {
				t.Fatal("renewal outlived the probe context")
			}
		}
		if transport.starts != 1 || len(transport.renewals) != 2 || transport.cancels != 0 {
			t.Fatal("bounded probe replayed or settled its expired operation")
		}
	})
}

func TestWorkspaceConformanceRenewalHonorsAdvertisedLeaseBounds(t *testing.T) {
	for _, limits := range []struct{ minimum, maximum, delay int64 }{{2, 8, 10}, {40, 45, 50}} {
		t.Run(time.Duration(limits.maximum*int64(time.Second)).String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				protocolLimits := harnessv2.DefaultProtocolLimits()
				protocolLimits.MinPromptLeaseMillis, protocolLimits.MaxPromptLeaseMillis = limits.minimum*1000, limits.maximum*1000
				state, transport := newWorkspaceLeaseTest(t, time.Duration(limits.delay)*time.Second, "", protocolLimits)
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				request, settlement, err := state.probeCompletedPromptLifecycle(ctx, "workspace-renewal")
				if err != nil {
					t.Fatal(err)
				}
				if len(transport.renewals) == 0 || !settlement.SettledAt.After(request.Lease.ExpiresAt) {
					t.Fatal("bounded lease did not extend original ownership across initial expiry")
				}
			})
		})
	}
}

func TestWorkspaceConformanceStopsRenewingAfterTerminalBeforeCleanEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, transport := newWorkspaceLeaseTest(t, 35*time.Second, "", harnessv2.DefaultProtocolLimits())
		transport.tailDelay = 20 * time.Second
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		started := time.Now()
		_, _, err := state.probeCompletedPromptLifecycle(ctx, "workspace-renewal")
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(started) != 55*time.Second || len(transport.renewals) != 2 || transport.cancels != 1 {
			t.Fatal("terminal stream was renewed or settled before clean EOF")
		}
	})
}
