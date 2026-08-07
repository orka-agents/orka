package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

type harnessV1CandidateErrorReader struct {
	client.Reader
	get func(client.ObjectKey, client.Object) error
}

type harnessV1PolicySequenceReader struct {
	client.Reader
	policyReads int
}

type harnessV1SessionControlSequenceStore struct {
	store.DurableControlStore
	controls []*store.SessionControl
	reads    int
}

func (s *harnessV1SessionControlSequenceStore) GetSessionControl(
	ctx context.Context,
	namespace, sessionName string,
) (*store.SessionControl, error) {
	if s.reads >= len(s.controls) {
		return s.DurableControlStore.GetSessionControl(ctx, namespace, sessionName)
	}
	control := s.controls[s.reads]
	s.reads++
	if control == nil {
		return nil, store.ErrNotFound
	}
	copyControl := *control
	if control.Lease != nil {
		copyLease := *control.Lease
		copyControl.Lease = &copyLease
	}
	return &copyControl, nil
}

func (r *harnessV1PolicySequenceReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if policy, ok := object.(*corev1alpha1.AgentExecutionPolicy); ok {
		r.policyReads++
		if r.policyReads > 1 {
			policy.Generation++
			policy.Spec.AllowNewV1Bindings = false
		}
	}
	return nil
}

type harnessV1BindingResponseLostClient struct {
	client.Client
	policyKey client.ObjectKey
	lost      bool
}

func (c *harnessV1BindingResponseLostClient) Status() client.SubResourceWriter {
	return &harnessV1BindingResponseLostWriter{
		SubResourceWriter: c.Client.Status(),
		parent:            c,
	}
}

type harnessV1BindingResponseLostWriter struct {
	client.SubResourceWriter
	parent *harnessV1BindingResponseLostClient
}

func (w *harnessV1BindingResponseLostWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	if err := w.SubResourceWriter.Patch(ctx, object, patch, options...); err != nil {
		return err
	}
	task, ok := object.(*corev1alpha1.Task)
	if !ok || task.Status.AgentExecutionBinding == nil || w.parent.lost {
		return nil
	}
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := w.parent.Get(ctx, w.parent.policyKey, policy); err != nil {
		return fmt.Errorf("read policy before simulated response loss: %w", err)
	}
	policy.Spec.AllowNewV1Bindings = false
	policy.Generation++
	if err := w.parent.Update(ctx, policy); err != nil {
		return fmt.Errorf("revoke policy before simulated response loss: %w", err)
	}
	w.parent.lost = true
	return errors.New("simulated binding status response loss")
}

func (r *harnessV1CandidateErrorReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if r.get != nil {
		if err := r.get(key, object); err != nil {
			return err
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestEnsureHarnessV1ExecutionBindingRequeuesTransientCandidateErrors(t *testing.T) {
	tests := []struct {
		name      string
		intercept func(client.Object) bool
	}{
		{
			name: "compatibility policy API read",
			intercept: func(object client.Object) bool {
				_, ok := object.(*corev1alpha1.AgentExecutionPolicy)
				return ok
			},
		},
		{
			name: "wrapper auth Secret API read",
			intercept: func(object client.Object) bool {
				_, ok := object.(*corev1.Secret)
				return ok
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newHarnessV1CandidateFixture(t, ctx)
			transientErr := errors.New("temporary Kubernetes API outage")
			fixture.reconciler.APIReader = &harnessV1CandidateErrorReader{
				Reader: fixture.reconciler.Client,
				get: func(_ client.ObjectKey, object client.Object) error {
					if test.intercept(object) {
						return transientErr
					}
					return nil
				},
			}

			result, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(
				ctx, fixture.task.DeepCopy(), fixture.agent,
			)
			if err != nil || !handled || result.RequeueAfter != 5*time.Second {
				t.Fatalf("ensure binding = result=%#v handled=%v err=%v, want five-second requeue", result, handled, err)
			}
			current := &corev1alpha1.Task{}
			if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.task), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase == corev1alpha1.TaskPhaseFailed || current.Status.AgentExecutionBinding != nil {
				t.Fatalf("transient resolution error terminalized or bound Task: %#v", current.Status)
			}
		})
	}
}

func TestEnsureHarnessV1ExecutionBindingFailsPermanentPolicyViolation(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.policy), policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.AllowNewV1Bindings = false
	if err := fixture.reconciler.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}

	_, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(
		ctx, fixture.task.DeepCopy(), fixture.agent,
	)
	if err != nil || !handled {
		t.Fatalf("ensure binding = handled=%v err=%v, want permanent failure", handled, err)
	}
	current := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseFailed ||
		!strings.Contains(current.Status.Message, "does not authorize new bindings") {
		t.Fatalf("permanent policy violation status = %#v", current.Status)
	}
}

func TestEnsureHarnessV1ExecutionBindingRejectsPolicyChangedBeforeCAS(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	fixture.reconciler.APIReader = &harnessV1PolicySequenceReader{Reader: fixture.reconciler.Client}

	_, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(
		ctx, fixture.task.DeepCopy(), fixture.agent,
	)
	if err != nil || !handled {
		t.Fatalf("ensure binding = handled=%v err=%v, want terminal policy rejection", handled, err)
	}
	current := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.AgentExecutionBinding != nil || current.Status.Phase != corev1alpha1.TaskPhaseFailed ||
		!strings.Contains(current.Status.Message, "changed or revoked") {
		t.Fatalf("Task after policy race = %#v", current.Status)
	}
	control := &corev1alpha1.AgentExecutionControl{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}, control); err != nil {
		t.Fatal(err)
	}
	inventory, err := fixture.reconciler.AgentExecutionBindingReservations.ListAgentExecutionBindingReservations(
		ctx,
		store.AgentExecutionControlRevision{
			ControlUID:        string(control.UID),
			ControlGeneration: control.Generation,
			Backend:           store.AgentExecutionBackendV1,
			ModeRevision:      control.Status.Backends.V1.ModeRevision,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Reservations) != 1 || inventory.OpenCount != 0 ||
		inventory.Reservations[0].State != store.AgentExecutionBindingReservationRejected ||
		inventory.Reservations[0].TerminalReason != "policy-invalidated" {
		t.Fatalf("policy-race reservation inventory = %#v", inventory)
	}
}

func TestEnsureHarnessV1ExecutionBindingRecoversCommittedPatchAfterPolicyRevocation(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	baseClient := fixture.reconciler.Client
	lostClient := &harnessV1BindingResponseLostClient{
		Client:    baseClient,
		policyKey: client.ObjectKeyFromObject(fixture.policy),
	}
	fixture.reconciler.Client = lostClient

	_, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(
		ctx, fixture.task.DeepCopy(), fixture.agent,
	)
	if err != nil || handled {
		t.Fatalf("ensure binding ambiguity recovery = handled=%v err=%v", handled, err)
	}
	if !lostClient.lost {
		t.Fatal("test client did not simulate a committed status patch with a lost response")
	}
	bound := &corev1alpha1.Task{}
	if err := baseClient.Get(ctx, client.ObjectKeyFromObject(fixture.task), bound); err != nil {
		t.Fatal(err)
	}
	if bound.Status.AgentExecutionBinding == nil {
		t.Fatal("committed binding was not recovered after the response was lost")
	}
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := baseClient.Get(ctx, client.ObjectKeyFromObject(fixture.policy), policy); err != nil {
		t.Fatal(err)
	}
	if policy.Spec.AllowNewV1Bindings {
		t.Fatal("test policy was not revoked after the binding commit")
	}
	want, err := agentExecutionBindingReservationFor(bound, bound.Status.AgentExecutionBinding)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := fixture.reconciler.AgentExecutionBindingReservations.GetAgentExecutionBindingReservation(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.State != store.AgentExecutionBindingReservationBound {
		t.Fatalf("recovered reservation state = %s, want Bound", reservation.State)
	}
}

func TestResolveHarnessV1ExecutionCandidateMarksSpecViolationPermanent(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	task := fixture.task.DeepCopy()
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{}

	_, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent)
	if err == nil || !isPermanentHarnessV1CandidateError(err) {
		t.Fatalf("workspace violation error = %v, permanent=%v", err, isPermanentHarnessV1CandidateError(err))
	}
}

func TestResolveHarnessV1ExecutionCandidateFreezesBoundedSessionTranscript(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	const sessionName = "continued-session"
	seedHarnessV1BindingTranscript(t, ctx, fixture, sessionName, defaultACPSessionType, []store.SessionMessage{
		{ID: "message-1", Role: "user", Content: "old request"},
		{ID: "message-2", Role: "assistant", Content: "old response"},
		{ID: "message-3", Role: "user", Content: "recent request"},
	})
	task := fixture.task.DeepCopy()
	task.Spec.SessionRef = &corev1alpha1.SessionReference{
		Name: sessionName, Append: true, MaxMessages: 2,
	}
	control, err := fixture.controls.GetSessionControl(ctx, task.Namespace, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	sequence := &harnessV1SessionControlSequenceStore{
		DurableControlStore: fixture.controls,
		controls:            []*store.SessionControl{control, control},
	}
	fixture.reconciler.DurableControlStore = sequence

	candidate, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	var body agentExecutionSnapshotBody
	if err := json.Unmarshal(candidate.snapshotBody, &body); err != nil {
		t.Fatal(err)
	}
	bootstrap := body.HarnessV1.SessionBootstrap
	if bootstrap == nil || bootstrap.SchemaVersion != harnessV1SessionBootstrapSchemaVersion ||
		bootstrap.SessionUID != control.SessionUID || bootstrap.ControlVersion != control.Version ||
		bootstrap.LeaseGeneration != control.LeaseGeneration || bootstrap.MessageCount != 2 ||
		bootstrap.TotalMessages != 2 || sequence.reads != 2 {
		t.Fatalf("frozen Session bootstrap = %#v, want bounded two-message suffix", bootstrap)
	}
	if body.Prompt != task.Spec.Prompt || strings.Contains(bootstrap.Artifact, "old request") ||
		!strings.Contains(bootstrap.Artifact, "old response") || !strings.Contains(bootstrap.Artifact, "recent request") {
		t.Fatalf("frozen Session input = prompt %q bootstrap %q", body.Prompt, bootstrap.Artifact)
	}
	if err := validateFrozenHarnessV1SessionInput(body); err != nil {
		t.Fatalf("validate frozen Session input: %v", err)
	}
}

func TestResolveHarnessV1ExecutionCandidateRejectsSessionControlRace(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	const sessionName = "racing-session"
	seedHarnessV1BindingTranscript(t, ctx, fixture, sessionName, defaultACPSessionType, []store.SessionMessage{
		{ID: "message-1", Role: "user", Content: "stable request"},
	})
	task := fixture.task.DeepCopy()
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: sessionName, Append: true}
	control, err := fixture.controls.GetSessionControl(ctx, task.Namespace, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	advanced := *control
	advanced.Version++
	fixture.reconciler.DurableControlStore = &harnessV1SessionControlSequenceStore{
		DurableControlStore: fixture.controls,
		controls:            []*store.SessionControl{control, &advanced},
	}

	if _, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent); err == nil ||
		!errors.Is(err, store.ErrConflict) {
		t.Fatalf("Session control race error = %v, want ErrConflict", err)
	}
}

func TestResolveHarnessV1ExecutionCandidateFreezesNewSessionUID(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	task := fixture.task.DeepCopy()
	task.Spec.SessionRef = &corev1alpha1.SessionReference{
		Name: "new-session", Create: true, Append: true,
	}

	candidate, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	var body agentExecutionSnapshotBody
	if err := json.Unmarshal(candidate.snapshotBody, &body); err != nil {
		t.Fatal(err)
	}
	bootstrap := body.HarnessV1.SessionBootstrap
	if bootstrap == nil || bootstrap.SchemaVersion != harnessV1SessionBootstrapSchemaVersion ||
		strings.TrimSpace(bootstrap.SessionUID) == "" || bootstrap.ControlVersion != 0 ||
		bootstrap.LeaseGeneration != 0 || bootstrap.MessageCount != 0 || bootstrap.TotalMessages != 0 ||
		bootstrap.Artifact != "" {
		t.Fatalf("new Session bootstrap = %#v, want frozen UID at V0/G0 with empty transcript", bootstrap)
	}
	if err := validateFrozenHarnessV1SessionInput(body); err != nil {
		t.Fatalf("validate new Session bootstrap: %v", err)
	}
}

func TestResolveHarnessV1ExecutionCandidateRejectsControlLessTranscriptAdoption(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	const sessionName = "legacy-transcript"
	transcripts := fixture.reconciler.SessionManager.store
	now := time.Now().UTC()
	if err := transcripts.CreateSession(ctx, &store.SessionRecord{
		Namespace: fixture.task.Namespace, Name: sessionName, SessionType: defaultACPSessionType,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := transcripts.AppendMessages(ctx, fixture.task.Namespace, sessionName, []store.SessionMessage{
		{ID: "legacy-message", Role: "user", Content: "unclassified history", Timestamp: now},
	}); err != nil {
		t.Fatal(err)
	}
	task := fixture.task.DeepCopy()
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: sessionName, Create: true, Append: true}

	if _, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent); err == nil ||
		!errors.Is(err, store.ErrConflict) {
		t.Fatalf("control-less transcript adoption error = %v, want ErrConflict", err)
	}
}

func TestResolveHarnessV1ExecutionCandidateFreezesPromptIncludedCutoff(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	const (
		sessionName   = "gateway-session"
		currentPrompt = "answer the canonical gateway message"
	)
	throughMessageID := store.GatewayUserMessageID("event-current")
	seedHarnessV1BindingTranscript(t, ctx, fixture, sessionName, store.SessionTypeGateway, []store.SessionMessage{
		{ID: "gateway:prior:user", Role: "user", Content: "earlier request"},
		{ID: "gateway:prior:assistant", Role: "assistant", Content: "earlier response"},
		{ID: throughMessageID, Role: "user", Content: currentPrompt},
		{ID: store.GatewayUserMessageID("event-later"), Role: "user", Content: "later queued request"},
	})
	task := fixture.task.DeepCopy()
	task.Spec.Prompt = ""
	task.Spec.SessionRef = &corev1alpha1.SessionReference{
		Name: sessionName, Append: false, MaxMessages: int32(store.GatewayTranscriptMessageLimit),
		ThroughMessageID: throughMessageID, PromptIncluded: true,
	}

	candidate, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	var body agentExecutionSnapshotBody
	if err := json.Unmarshal(candidate.snapshotBody, &body); err != nil {
		t.Fatal(err)
	}
	bootstrap := body.HarnessV1.SessionBootstrap
	if body.Prompt != currentPrompt || bootstrap == nil || bootstrap.MessageCount != 2 || bootstrap.TotalMessages != 3 {
		t.Fatalf("frozen prompt-included Session input = prompt %q bootstrap %#v", body.Prompt, bootstrap)
	}
	if strings.Contains(bootstrap.Artifact, currentPrompt) || strings.Contains(bootstrap.Artifact, "later queued request") ||
		!strings.Contains(bootstrap.Artifact, "earlier response") {
		t.Fatalf("prompt-included bootstrap crossed its cutoff or duplicated the current prompt: %q", bootstrap.Artifact)
	}
	if err := validateFrozenHarnessV1SessionInput(body); err != nil {
		t.Fatalf("validate frozen prompt-included Session input: %v", err)
	}
}

func TestResolveHarnessV1ExecutionCandidateRejectsMissingPromptCutoff(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	seedHarnessV1BindingTranscript(t, ctx, fixture, "gateway-session", store.SessionTypeGateway, nil)
	task := fixture.task.DeepCopy()
	task.Spec.Prompt = ""
	task.Spec.SessionRef = &corev1alpha1.SessionReference{
		Name: "gateway-session", Append: false, MaxMessages: int32(store.GatewayTranscriptMessageLimit),
		ThroughMessageID: store.GatewayUserMessageID("missing"), PromptIncluded: true,
	}

	if _, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent); err == nil ||
		!errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing canonical prompt cutoff error = %v, want ErrNotFound", err)
	}
}

func TestResolveHarnessV1ExecutionCandidateRejectsCopilotPermanently(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.policy), policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.AllowedBuiltInRuntimeTypes = append(
		policy.Spec.AllowedBuiltInRuntimeTypes,
		corev1alpha1.AgentRuntimeCopilot,
	)
	if err := fixture.reconciler.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	agent := fixture.agent.DeepCopy()
	agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeCopilot

	candidate, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, fixture.task, agent)
	if candidate != nil || err == nil || !isPermanentHarnessV1CandidateError(err) ||
		!strings.Contains(err.Error(), "GitHub mutation-capable credential") {
		t.Fatalf("Copilot candidate = %#v, error = %v, want permanent safe-credential rejection", candidate, err)
	}
}

func TestResolveHarnessV1TargetRequiresObservedMode(t *testing.T) {
	const (
		endpoint   = "https://runtime.example.invalid"
		runtimeKey = "token"
	)
	tests := []struct {
		name         string
		capabilities *corev1alpha1.AgentRuntimeObservedCapabilities
		wantError    bool
	}{
		{name: "missing capabilities", wantError: true},
		{
			name: "brokered only",
			capabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ToolExecutionModes: []corev1alpha1.AgentRuntimeToolExecutionMode{
					corev1alpha1.AgentRuntimeToolExecutionModeBrokered,
				},
			},
			wantError: true,
		},
		{
			name: "observed only",
			capabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ToolExecutionModes: []corev1alpha1.AgentRuntimeToolExecutionMode{
					corev1alpha1.AgentRuntimeToolExecutionModeObserved,
				},
			},
		},
		{
			name: "observed and brokered",
			capabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ToolExecutionModes: []corev1alpha1.AgentRuntimeToolExecutionMode{
					corev1alpha1.AgentRuntimeToolExecutionModeObserved,
					corev1alpha1.AgentRuntimeToolExecutionModeBrokered,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &corev1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: "external-v1", UID: types.UID("external-v1-uid"), Generation: 1,
				},
				Spec: corev1alpha1.AgentRuntimeRegistrySpec{
					ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV1),
					Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
						Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint, Endpoint: endpoint,
					},
					ClientAuth: corev1alpha1.AgentRuntimeClientAuth{
						BearerAuthRef: &corev1alpha1.AgentRuntimeBearerAuthReference{Name: "external-v1-auth", Key: runtimeKey},
					},
				},
				Status: corev1alpha1.AgentRuntimeStatus{
					Ready: true, ObservedGeneration: 1, ObservedAuthRefResourceVersion: "1",
					ObservedCapabilities: test.capabilities,
				},
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: runtime.Namespace, Name: runtime.Spec.ClientAuth.BearerAuthRef.Name,
					UID: types.UID("external-v1-auth-uid"), ResourceVersion: "1",
					Labels: map[string]string{
						agentRuntimeAuthUseLabel: scheduledRunLabelValue, agentRuntimeAuthRefNameLabel: runtime.Name,
					},
					Annotations: map[string]string{agentRuntimeAuthEndpointAnnotation: endpoint},
				},
				Data: map[string][]byte{runtimeKey: []byte("runtime-auth-value")},
			}
			reconciler, _ := newBindingTestReconciler(t, runtime, secret)
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: runtime.Namespace}}
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
				RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: runtime.Name},
			}}}

			target, err := reconciler.resolveHarnessV1Target(t.Context(), reconciler.Client, task, agent)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "required observed tool execution mode") {
					t.Fatalf("resolve target = %#v, error = %v, want observed-mode rejection", target, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if target.runtimeRef == nil || target.runtimeRef.Name != runtime.Name ||
				target.backend != corev1alpha1.AgentExecutionBackendExternalEndpoint {
				t.Fatalf("resolved target = %#v, want external AgentRuntime %q", target, runtime.Name)
			}
		})
	}
}

func TestResolveHarnessV1TargetRevalidatesTLSForReadyRuntime(t *testing.T) {
	const (
		endpoint   = "http://runtime.default.svc:8080"
		runtimeKey = "token"
	)
	runtime := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "stale-ready-v1", UID: types.UID("stale-ready-v1-uid"), Generation: 1,
		},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV1),
			Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
				Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint, Endpoint: endpoint,
			},
			ClientAuth: corev1alpha1.AgentRuntimeClientAuth{
				BearerAuthRef: &corev1alpha1.AgentRuntimeBearerAuthReference{Name: "stale-ready-v1-auth", Key: runtimeKey},
			},
		},
		Status: corev1alpha1.AgentRuntimeStatus{
			Ready: true, ObservedGeneration: 1, ObservedAuthRefResourceVersion: "1",
			ObservedCapabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ToolExecutionModes: []corev1alpha1.AgentRuntimeToolExecutionMode{
					corev1alpha1.AgentRuntimeToolExecutionModeObserved,
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: runtime.Namespace, Name: runtime.Spec.ClientAuth.BearerAuthRef.Name,
			UID: types.UID("stale-ready-v1-auth-uid"), ResourceVersion: "1",
		},
		Data: map[string][]byte{runtimeKey: []byte("runtime-auth-value")},
	}
	reconciler, _ := newBindingTestReconciler(t, runtime, secret)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: runtime.Namespace}}
	agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
		RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: runtime.Name},
	}}}

	target, err := reconciler.resolveHarnessV1Target(t.Context(), reconciler.Client, task, agent)
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("resolve target = %#v, error = %v, want stale Ready cleartext rejection", target, err)
	}
}

func TestResolveHarnessV1ExecutionCandidateFreezesRuntimeAuthOnly(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixture.task.Namespace, Name: "runtime-auth-credentials",
			UID: types.UID("runtime-auth-credentials-uid"),
		},
		Data: map[string][]byte{"OPENAI_API_KEY": []byte("runtime-auth-secret-value")},
	}
	if err := fixture.reconciler.Create(ctx, credential); err != nil {
		t.Fatal(err)
	}
	fixture.agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: credential.Name}
	task := fixture.task.DeepCopy()
	task.Annotations = map[string]string{labels.AnnotationAgentRuntimeAuthOnly: scheduledRunLabelValue}

	candidate, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	var body agentExecutionSnapshotBody
	if err := json.Unmarshal(candidate.snapshotBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.HarnessV1 == nil || !body.HarnessV1.RuntimeAuthOnly {
		t.Fatalf("frozen harness v1 metadata = %#v, want runtimeAuthOnly", body.HarnessV1)
	}
	if strings.Contains(string(candidate.snapshotBody), "runtime-auth-secret-value") {
		t.Fatal("encrypted snapshot plaintext body retained a raw provider credential")
	}

	unprotectedTask := task.DeepCopy()
	delete(unprotectedTask.Annotations, labels.AnnotationAgentRuntimeAuthOnly)
	unprotected, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, unprotectedTask, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	if unprotected.binding.Snapshot.Digest == candidate.binding.Snapshot.Digest {
		t.Fatal("runtime-auth-only annotation did not change the immutable snapshot digest")
	}
}

func TestResolveHarnessV1CredentialRefsAllowlistsRuntimeProviderKeys(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	tests := []struct {
		name        string
		runtimeType corev1alpha1.AgentRuntimeType
		data        map[string][]byte
		wantKeys    []string
	}{
		{
			name: "codex", runtimeType: corev1alpha1.AgentRuntimeCodex,
			data: map[string][]byte{
				"OPENAI_API_KEY":  []byte("codex-provider-key"),
				"OPENAI_BASE_URL": []byte("https://provider.example.invalid"),
			},
			wantKeys: []string{"OPENAI_API_KEY", "OPENAI_BASE_URL"},
		},
		{
			name: "claude", runtimeType: corev1alpha1.AgentRuntimeClaude,
			data: map[string][]byte{
				"ANTHROPIC_API_KEY":  []byte("claude-provider-key"),
				"ANTHROPIC_BASE_URL": []byte("https://provider.example.invalid"),
			},
			wantKeys: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: fixture.task.Namespace, Name: "provider-" + tt.name,
					UID: types.UID("provider-" + tt.name + "-uid"),
				},
				Data: tt.data,
			}
			if err := fixture.reconciler.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			agent := fixture.agent.DeepCopy()
			agent.Spec.Runtime.Type = tt.runtimeType
			agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: secret.Name}
			refs, err := resolveHarnessV1CredentialRefs(
				ctx, fixture.reconciler.Client, agent, resolvedHarnessV1Target{}, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(refs) != 1 || !slices.Equal(refs[0].Keys, tt.wantKeys) {
				t.Fatalf("credential refs = %#v, want keys %v", refs, tt.wantKeys)
			}
		})
	}
}

func TestResolveHarnessV1CredentialRefsRejectsFoundryOnlyForRuntimeAuthOnly(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	tests := []struct {
		name string
		data map[string][]byte
	}{
		{
			name: "Foundry flag",
			data: map[string][]byte{
				"CLAUDE_CODE_USE_FOUNDRY": []byte("true"),
				"ANTHROPIC_API_KEY":       []byte("direct-provider-key"),
			},
		},
		{
			name: "Foundry key",
			data: map[string][]byte{
				"ANTHROPIC_FOUNDRY_API_KEY": []byte("foundry-provider-key"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: fixture.task.Namespace, Name: "provider-" + strings.ReplaceAll(strings.ToLower(test.name), " ", "-"),
					UID: types.UID("provider-" + strings.ReplaceAll(strings.ToLower(test.name), " ", "-") + "-uid"),
				},
				Data: test.data,
			}
			if err := fixture.reconciler.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			agent := fixture.agent.DeepCopy()
			agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude
			agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: secret.Name}

			if _, err := resolveHarnessV1CredentialRefs(
				ctx, fixture.reconciler.Client, agent, resolvedHarnessV1Target{}, true,
			); err == nil || !isPermanentHarnessV1CandidateError(err) || !strings.Contains(err.Error(), "does not support Azure AI Foundry") {
				t.Fatalf("runtime-auth-only Foundry error = %v, want permanent early rejection", err)
			}
			refs, err := resolveHarnessV1CredentialRefs(
				ctx, fixture.reconciler.Client, agent, resolvedHarnessV1Target{}, false,
			)
			if err != nil || len(refs) != 1 {
				t.Fatalf("non-proxied Foundry refs = %#v, error = %v, want preserved support", refs, err)
			}
		})
	}
}

func TestResolveHarnessV1CredentialRefsRejectsUnrelatedKeys(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	for _, prohibitedKey := range []string{"GITHUB_TOKEN", "GIT_TOKEN", "KONTXT_TXN_TOKEN"} {
		t.Run(prohibitedKey, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: fixture.task.Namespace, Name: "provider-" + strings.ToLower(prohibitedKey),
					UID: types.UID("provider-" + strings.ToLower(prohibitedKey) + "-uid"),
				},
				Data: map[string][]byte{
					"OPENAI_API_KEY": []byte("provider-key"),
					prohibitedKey:    []byte("unrelated-sensitive-value"),
				},
			}
			if err := fixture.reconciler.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			agent := fixture.agent.DeepCopy()
			agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: secret.Name}
			_, err := resolveHarnessV1CredentialRefs(
				ctx, fixture.reconciler.Client, agent, resolvedHarnessV1Target{}, false,
			)
			if err == nil || !isPermanentHarnessV1CandidateError(err) || !strings.Contains(err.Error(), prohibitedKey) {
				t.Fatalf("unrelated credential key error = %v, want permanent rejection mentioning %s", err, prohibitedKey)
			}
		})
	}
}

func TestHarnessV1SessionLineageDigestExcludesTurnSnapshot(t *testing.T) {
	binding := &corev1alpha1.AgentExecutionBinding{
		ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
		Backend:         corev1alpha1.AgentExecutionBackendHarnessWrapper,
		RuntimeType:     corev1alpha1.AgentRuntimeCodex,
		Agent: &corev1alpha1.AgentExecutionAgentRef{
			Namespace: "default", Name: "agent", UID: types.UID("agent-uid"), Generation: 1,
		},
		Policy: &corev1alpha1.AgentExecutionPolicyRef{
			Name: "compatibility", UID: types.UID("policy-uid"), Generation: 1,
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
		Snapshot: corev1alpha1.AgentExecutionSnapshotRef{Digest: "sha256:" + strings.Repeat("b", 64)},
	}
	first, err := agentExecutionLineageConfigDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	secondTurn := *binding
	secondTurn.Snapshot.Digest = "sha256:" + strings.Repeat("c", 64)
	second, err := agentExecutionLineageConfigDigest(&secondTurn)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("turn-specific snapshots changed v1 Session lineage digest: %s != %s", first, second)
	}
	otherRuntime := secondTurn
	otherRuntime.RuntimeType = corev1alpha1.AgentRuntimeClaude
	third, err := agentExecutionLineageConfigDigest(&otherRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("runtime identity change did not change v1 Session lineage digest")
	}
}

func TestValidateHarnessV1RuntimeAuthOnlyRejectsUnsupportedRoute(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{labels.AnnotationAgentRuntimeAuthOnly: scheduledRunLabelValue},
	}}
	tests := []struct {
		name       string
		runtime    corev1alpha1.AgentRuntimeType
		target     resolvedHarnessV1Target
		wantReason string
	}{
		{
			name: "external endpoint", runtime: corev1alpha1.AgentRuntimeCodex,
			target: resolvedHarnessV1Target{
				backend:    corev1alpha1.AgentExecutionBackendExternalEndpoint,
				runtimeRef: &corev1alpha1.AgentRuntime{},
			},
			wantReason: "built-in wrapper",
		},
		{
			name: "unsupported built-in runtime", runtime: corev1alpha1.AgentRuntimeCopilot,
			target:     resolvedHarnessV1Target{backend: corev1alpha1.AgentExecutionBackendHarnessWrapper},
			wantReason: "does not support runtime",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
				Runtime: &corev1alpha1.AgentCLIRuntime{Type: test.runtime},
			}}
			_, err := validateHarnessV1RuntimeAuthOnly(task, agent, test.target)
			if err == nil || !isPermanentHarnessV1CandidateError(err) || !strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("validate runtime-auth-only route error = %v, want permanent %q failure", err, test.wantReason)
			}
		})
	}
}

func TestLoadVerifiedHarnessV1ExecutionForRecoveryControlRevision(t *testing.T) {
	tests := []struct {
		name          string
		mutateControl func(*corev1alpha1.AgentExecutionControl, *corev1alpha1.AgentExecutionBackendControlRef)
		wantError     bool
	}{
		{
			name: "same enabled generation and revision",
			mutateControl: func(
				_ *corev1alpha1.AgentExecutionControl,
				_ *corev1alpha1.AgentExecutionBackendControlRef,
			) {
			},
		},
		{
			name: "closing transition",
			mutateControl: func(
				control *corev1alpha1.AgentExecutionControl,
				ref *corev1alpha1.AgentExecutionBackendControlRef,
			) {
				control.Generation = ref.Generation + 1
				control.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeDisabled
				control.Status.ObservedGeneration = ref.Generation
				control.Status.Backends.V1 = corev1alpha1.AgentExecutionBackendStatus{
					EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeClosing,
					ModeRevision:  ref.ModeRevision + 1,
				}
			},
		},
		{
			name: "regressed mode revision",
			mutateControl: func(
				control *corev1alpha1.AgentExecutionControl,
				ref *corev1alpha1.AgentExecutionBackendControlRef,
			) {
				control.Status.Backends.V1.ModeRevision = ref.ModeRevision - 1
			},
			wantError: true,
		},
		{
			name: "observed generation below binding generation",
			mutateControl: func(
				control *corev1alpha1.AgentExecutionControl,
				ref *corev1alpha1.AgentExecutionBackendControlRef,
			) {
				control.Status.ObservedGeneration = ref.Generation - 1
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			reconciler, task := newHarnessV1RecoveryBindingFixture(t, ctx)
			binding := task.Status.AgentExecutionBinding
			if binding == nil || binding.BackendControl == nil {
				t.Fatal("fixture did not persist a harness v1 backend control binding")
			}

			updateHarnessV1RecoveryControl(t, ctx, reconciler, func(control *corev1alpha1.AgentExecutionControl) {
				test.mutateControl(control, binding.BackendControl)
			})
			_, err := reconciler.loadVerifiedHarnessV1ExecutionForRecovery(
				ctx, task, binding, false,
			)
			if test.wantError && err == nil {
				t.Fatal("recovery verification unexpectedly accepted regressed backend control")
			}
			if !test.wantError && err != nil {
				t.Fatalf("recovery verification rejected admitted work: %v", err)
			}
		})
	}
}

type harnessV1CandidateFixture struct {
	reconciler *TaskReconciler
	controls   store.DurableControlStore
	fence      store.ControllerEpochFence
	task       *corev1alpha1.Task
	agent      *corev1alpha1.Agent
	policy     *corev1alpha1.AgentExecutionPolicy
}

func seedHarnessV1BindingTranscript(
	t *testing.T,
	ctx context.Context,
	fixture *harnessV1CandidateFixture,
	sessionName string,
	sessionType string,
	messages []store.SessionMessage,
) {
	t.Helper()
	if fixture == nil || fixture.reconciler == nil || fixture.reconciler.SessionManager == nil {
		t.Fatal("harness v1 binding fixture is missing its Session transcript store")
	}
	transcripts := fixture.reconciler.SessionManager.store
	if err := transcripts.CreateSession(ctx, &store.SessionRecord{
		Namespace: fixture.task.Namespace, Name: sessionName, SessionType: sessionType,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := fixture.controls.CreateSessionControl(ctx, &store.SessionControl{
		Namespace: fixture.task.Namespace, SessionName: sessionName,
		SessionUID:    "frozen-" + sessionName + "-uid",
		RequestDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte("session-control:" + sessionName)),
		Availability:  store.SessionAvailable, CreatedAt: now, UpdatedAt: now,
	}, fixture.fence); err != nil {
		t.Fatal(err)
	}
	if len(messages) == 0 {
		return
	}
	for index := range messages {
		if messages[index].Timestamp.IsZero() {
			messages[index].Timestamp = now.Add(time.Duration(index) * time.Millisecond)
		}
	}
	if err := transcripts.AppendMessages(ctx, fixture.task.Namespace, sessionName, messages); err != nil {
		t.Fatal(err)
	}
}

func newHarnessV1CandidateFixture(
	t *testing.T,
	ctx context.Context,
) *harnessV1CandidateFixture {
	t.Helper()
	const authSecretKey = "harness-auth"
	control := bindingTestControl()
	control.Generation = 3
	control.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeEnabled
	control.Status.ObservedGeneration = control.Generation
	control.Status.Backends.V1 = corev1alpha1.AgentExecutionBackendStatus{
		EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeEnabled,
		ModeRevision:  5,
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "harness-v1-recovery", UID: types.UID("harness-v1-recovery-task-uid"), Generation: 1,
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "continue admitted work",
			AgentRef: &corev1alpha1.AgentReference{Name: "harness-v1-agent"},
		},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: task.Namespace, UID: types.UID("harness-v1-recovery-namespace-uid"),
	}}
	policy := &corev1alpha1.AgentExecutionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: defaultHarnessV1PolicyName,
			UID: types.UID("harness-v1-recovery-policy-uid"), Generation: 1,
		},
		Spec: corev1alpha1.AgentExecutionPolicySpec{
			AllowNewV1Bindings:         true,
			AllowedBuiltInRuntimeTypes: []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex},
			RetryEligibility:           corev1alpha1.AgentExecutionRetryNone,
			ProhibitedFields: []corev1alpha1.AgentExecutionProhibitedField{
				corev1alpha1.AgentExecutionProhibitWorkspaceCredentials,
				corev1alpha1.AgentExecutionProhibitForgeCredentials,
				corev1alpha1.AgentExecutionProhibitDirectPublication,
				corev1alpha1.AgentExecutionProhibitTransactionTokens,
			},
			NetworkIsolationProfile: corev1alpha1.AgentExecutionNetworkIsolationDefaultDeny,
		},
	}
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: "harness-v1-auth",
			UID: types.UID("harness-v1-auth-secret-uid"), ResourceVersion: "1",
		},
		Data: map[string][]byte{authSecretKey: []byte(strings.Repeat("t", 32))},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: task.Spec.AgentRef.Name,
			UID: types.UID("harness-v1-recovery-agent-uid"), Generation: 1,
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: "test-model"},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV1),
				DefaultAllowedTools: []string{}, DefaultAllowBash: new(false),
			},
		},
	}

	reconciler, durable := newBindingTestReconciler(t, control, task, namespace, policy, authSecret)
	fence := seedHarnessV1AttemptEpoch(t, durable)
	configureAgentExecutionBindingTestGate(
		t, ctx, durable, control, store.AgentExecutionBackendV1,
	)
	reconciler.HarnessV1Endpoint = "http://harness-v1.default.svc:8080"
	reconciler.HarnessV1AuthSecretNamespace = authSecret.Namespace
	reconciler.HarnessV1AuthSecretName = authSecret.Name
	reconciler.HarnessV1AuthSecretKey = authSecretKey
	reconciler.SessionManager = NewSessionManager(durable)
	reconciler.DurableControlStore = durable
	return &harnessV1CandidateFixture{
		reconciler: reconciler,
		controls:   durable,
		fence:      fence,
		task:       task,
		agent:      agent,
		policy:     policy,
	}
}

func newHarnessV1RecoveryBindingFixture(
	t *testing.T,
	ctx context.Context,
) (*TaskReconciler, *corev1alpha1.Task) {
	t.Helper()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	if result, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(
		ctx, fixture.task.DeepCopy(), fixture.agent,
	); err != nil || handled {
		t.Fatalf("establish harness v1 recovery fixture binding: result=%#v handled=%v err=%v", result, handled, err)
	}
	bound := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.task), bound); err != nil {
		t.Fatal(err)
	}
	return fixture.reconciler, bound
}

func updateHarnessV1RecoveryControl(
	t *testing.T,
	ctx context.Context,
	reconciler *TaskReconciler,
	mutate func(*corev1alpha1.AgentExecutionControl),
) {
	t.Helper()
	current := &corev1alpha1.AgentExecutionControl{}
	key := client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}
	if err := reconciler.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	desired := current.DeepCopy()
	mutate(desired)
	desiredStatus := desired.Status
	desired.Status = current.Status
	if err := reconciler.Update(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	current.Status = desiredStatus
	if err := reconciler.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
}
