package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/harnesstest"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2conformance "github.com/orka-agents/orka/internal/harness/v2/conformance"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
)

const agentRuntimeV1TestBearer = "0123456789abcdef0123456789abcdef"

func TestAgentRuntimeReconcilerMarksStrictV2RuntimeReady(t *testing.T) {
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{
		ControllerBearerToken:     strings.Repeat("t", 32),
		OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID:         "external-runtime-instance-1",
		SupervisorBootID:          "boot-1",
		RuntimePoolUID:            "external-pool-1",
		Profile:                   profile,
		Limits:                    limits,
		SupportsDrain:             true,
		WorkspaceGovernance:       claims,
	}
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	runtimeObject, secret := testAgentRuntimeAndSecret(t, server.URL(), config)
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if !updated.Status.Ready {
		t.Fatalf("Ready = false, message=%q", updated.Status.Message)
	}
	if updated.Status.ObservedCapabilities == nil {
		t.Fatal("ObservedCapabilities is nil")
	}
	observed := updated.Status.ObservedCapabilities
	if observed.ProtocolVersion != harnessv2.ProtocolVersion || observed.Transport != "http+ndjson" {
		t.Fatalf("observed protocol = %#v", observed)
	}
	if observed.RuntimeInstanceID != string(config.RuntimeInstanceID) || observed.SupervisorBootID != string(config.SupervisorBootID) {
		t.Fatalf("observed exact instance = %#v", observed)
	}
	if observed.RuntimeProfileDigest != runtimeObject.Spec.Capabilities.Profile.Digest || observed.Limits == nil ||
		observed.Limits.MaxConcurrentPrompts != int32(limits.MaxConcurrentPrompts) {
		t.Fatalf("observed profile/limits = %#v", observed)
	}
	if observed.WorkspaceGovernance == nil || !observed.WorkspaceGovernance.Strict() {
		t.Fatalf("observed strict governance = %#v", observed.WorkspaceGovernance)
	}
	if updated.Status.ObservedControllerAuthRefResourceVersion == "" || updated.Status.ObservedOperationCapabilityRefResourceVersion == "" {
		t.Fatalf("observed auth versions = %#v", updated.Status)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, agentRuntimeReadyCondition)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != agentRuntimeReasonReady {
		t.Fatalf("Ready condition = %#v", condition)
	}
	counts := server.Counts()
	if counts.PromptStarts != 1 || counts.PromptCancels != 1 || counts.SessionDeletes != 1 || counts.WorkspaceDeltas != 1 {
		t.Fatalf("hostile conformance counts = %#v", counts)
	}
}

func TestAgentRuntimeReconcilerDoesNotRepeatHostileCycleWhenIdentityIsUnchanged(t *testing.T) {
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{
		ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID: "external-runtime-instance-1", SupervisorBootID: "boot-1", RuntimePoolUID: "external-pool-1",
		Profile: profile, Limits: limits, SupportsDrain: true, WorkspaceGovernance: claims,
	}
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	runtimeObject, secret := testAgentRuntimeAndSecret(t, server.URL(), config)
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	for range 2 {
		if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
			t.Fatal(err)
		}
	}
	counts := server.Counts()
	if counts.PromptStarts != 1 || counts.SessionCreates != 1 {
		t.Fatalf("deep hostile cycle repeated on unchanged ready runtime: %#v", counts)
	}
}

func TestAgentRuntimeReconcilerRechecksHostileCycleAfterAuthenticatedIdentityChange(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*harnessv2.Fence)
	}{
		{name: "supervisor boot", mutate: func(fence *harnessv2.Fence) { fence.SupervisorBootID = "boot-2" }},
		{name: "controller epoch", mutate: func(fence *harnessv2.Fence) { fence.ControllerEpoch++ }},
		{name: "runtime pool UID", mutate: func(fence *harnessv2.Fence) { fence.RuntimePoolUID = "external-pool-2" }},
		{name: "runtime pool generation", mutate: func(fence *harnessv2.Fence) { fence.RuntimePoolGeneration++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
			config := conformancetest.Config{
				ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
				RuntimeInstanceID: "external-runtime-instance-1", SupervisorBootID: "boot-1", RuntimePoolUID: "external-pool-1",
				Profile: profile, Limits: limits, SupportsDrain: true, WorkspaceGovernance: claims,
			}
			server, err := conformancetest.NewServer(config)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			runtimeObject, secret := testAgentRuntimeAndSecret(t, server.URL(), config)
			reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
			allowAgentRuntimeLoopback(t)
			if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
				t.Fatal(err)
			}

			fence := server.Fence()
			test.mutate(&fence)
			if err := server.SetFence(fence); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
				t.Fatal(err)
			}
			updated := getAgentRuntime(t, reconciler, runtimeObject)
			if !updated.Status.Ready {
				t.Fatalf("Ready = false after identity change, message=%q", updated.Status.Message)
			}
			counts := server.Counts()
			if counts.SessionCreates != 2 || counts.PromptStarts != 2 {
				t.Fatalf("authenticated identity change did not rerun full lifecycle: %#v", counts)
			}
		})
	}
}

func TestAgentRuntimeAuthenticatedIdentityChanged(t *testing.T) {
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-1", SupervisorBootID: "boot-1", ControllerEpoch: 1,
		RuntimePoolUID: "pool-1", RuntimePoolGeneration: 1, RuntimeProfileDigest: harnessv2.ProfileDigest(testControllerDigest("profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	previous := &corev1alpha1.AgentRuntimeObservedCapabilities{
		RuntimeInstanceID: string(fence.RuntimeInstanceID), SupervisorBootID: string(fence.SupervisorBootID),
		ControllerEpoch: int64(fence.ControllerEpoch), RuntimePoolUID: string(fence.RuntimePoolUID),
		RuntimePoolGeneration: int64(fence.RuntimePoolGeneration), RuntimeProfileDigest: string(fence.RuntimeProfileDigest),
		ProfileDigestSchemaVersion: int32(fence.ProfileDigestSchemaVersion),
	}
	if agentRuntimeAuthenticatedIdentityChanged(previous, &harnessv2.StatusResponse{Fence: fence}) {
		t.Fatal("unchanged authenticated identity was reported changed")
	}
	if !agentRuntimeAuthenticatedIdentityChanged(nil, &harnessv2.StatusResponse{Fence: fence}) {
		t.Fatal("missing previous identity must require lifecycle conformance")
	}
	if !agentRuntimeAuthenticatedIdentityChanged(previous, nil) {
		t.Fatal("missing current identity must require lifecycle conformance")
	}

	tests := []struct {
		name   string
		mutate func(*harnessv2.Fence)
	}{
		{name: "runtime instance", mutate: func(f *harnessv2.Fence) { f.RuntimeInstanceID = "runtime-2" }},
		{name: "supervisor boot", mutate: func(f *harnessv2.Fence) { f.SupervisorBootID = "boot-2" }},
		{name: "controller epoch", mutate: func(f *harnessv2.Fence) { f.ControllerEpoch++ }},
		{name: "runtime pool UID", mutate: func(f *harnessv2.Fence) { f.RuntimePoolUID = "pool-2" }},
		{name: "runtime pool generation", mutate: func(f *harnessv2.Fence) { f.RuntimePoolGeneration++ }},
		{name: "runtime profile", mutate: func(f *harnessv2.Fence) {
			f.RuntimeProfileDigest = harnessv2.ProfileDigest(testControllerDigest("profile-2"))
		}},
		{name: "profile schema", mutate: func(f *harnessv2.Fence) { f.ProfileDigestSchemaVersion++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := fence
			test.mutate(&changed)
			if !agentRuntimeAuthenticatedIdentityChanged(previous, &harnessv2.StatusResponse{Fence: changed}) {
				t.Fatalf("%s change did not require lifecycle conformance", test.name)
			}
		})
	}
}

func TestAgentRuntimeReconcilerRechecksHostileCycleAfterAuthRotation(t *testing.T) {
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{
		ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID: "external-runtime-instance-1", SupervisorBootID: "boot-1", RuntimePoolUID: "external-pool-1",
		Profile: profile, Limits: limits, SupportsDrain: true, WorkspaceGovernance: claims,
	}
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	runtimeObject, secret := testAgentRuntimeAndSecret(t, server.URL(), config)
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.Secret{}
	if err := reconciler.Get(t.Context(), client.ObjectKeyFromObject(secret), stored); err != nil {
		t.Fatal(err)
	}
	stored.Data["capability-secret"] = []byte(strings.Repeat("q", 32))
	if err := reconciler.Update(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	// The fake runtime still has the old key, so rotation must force a deep probe
	// and fail closed rather than leaving the prior Ready status in place.
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	// The rotated key now fails closed at the earliest capability-guarded
	// surface: the status probe itself rejects the stale capability secret.
	if updated.Status.Ready ||
		(!strings.Contains(updated.Status.Message, "operation capability") && !strings.Contains(updated.Status.Message, "status authorization failed")) {
		t.Fatalf("rotated mismatched capability key did not fail closed: %#v", updated.Status)
	}
}

func TestAgentRuntimeReconcilerMarksHarnessV1RuntimeReady(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{
		RuntimeName: "external-v1", AuthToken: agentRuntimeV1TestBearer,
	})
	defer server.Close()
	runtimeObject, secret := testHarnessV1AgentRuntimeAndSecret(server.URL())
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if !updated.Status.Ready {
		t.Fatalf("harness v1 runtime Ready = false, message=%q", updated.Status.Message)
	}
	if updated.Status.ObservedCapabilities == nil ||
		updated.Status.ObservedCapabilities.ProtocolVersion != harness.ProtocolVersion ||
		updated.Status.ObservedCapabilities.Transport != harness.HTTPTransport ||
		updated.Status.ObservedCapabilities.RuntimeName != "external-v1" {
		t.Fatalf("observed harness v1 capabilities = %#v", updated.Status.ObservedCapabilities)
	}
	if updated.Status.ObservedAuthRefResourceVersion == "" ||
		updated.Status.ObservedControllerAuthRefResourceVersion != "" ||
		updated.Status.ObservedOperationCapabilityRefResourceVersion != "" {
		t.Fatalf("contract-specific observed auth versions = %#v", updated.Status)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, agentRuntimeReadyCondition)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != agentRuntimeReasonReady {
		t.Fatalf("Ready condition = %#v", condition)
	}
	encodedStatus := updated.Status.Message + updated.Status.ObservedCapabilities.RuntimeName +
		updated.Status.ObservedCapabilities.RuntimeVersion
	if strings.Contains(encodedStatus, agentRuntimeV1TestBearer) {
		t.Fatal("harness v1 status leaked the bearer token")
	}
}

func TestAgentRuntimeReconcilerHarnessV1RejectsWeakBearerToken(t *testing.T) {
	tests := []struct {
		name        string
		token       string
		wantMessage string
	}{
		{name: "short", token: strings.Repeat("t", agentRuntimeMinBearerBytes-1), wantMessage: "at least 32 bytes"},
		{name: "space", token: strings.Repeat("t", 16) + " " + strings.Repeat("t", 16), wantMessage: "invalid HTTP header bytes"},
		{name: "control", token: strings.Repeat("t", 16) + "\n" + strings.Repeat("t", 16), wantMessage: "invalid HTTP header bytes"},
		{name: "non-ASCII", token: strings.Repeat("t", 31) + "é", wantMessage: "invalid HTTP header bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: agentRuntimeV1TestBearer})
			defer server.Close()
			runtimeObject, secret := testHarnessV1AgentRuntimeAndSecret(server.URL())
			secret.Data["token"] = []byte(test.token)
			reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
			allowAgentRuntimeLoopback(t)
			if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
				t.Fatal(err)
			}
			updated := getAgentRuntime(t, reconciler, runtimeObject)
			if updated.Status.Ready || !strings.Contains(updated.Status.Message, test.wantMessage) {
				t.Fatalf("weak harness v1 bearer status = %#v, want message containing %q", updated.Status, test.wantMessage)
			}
			if strings.Contains(updated.Status.Message, test.token) {
				t.Fatal("harness v1 status leaked the rejected bearer token")
			}
		})
	}
}

func TestValidateHarnessV1AgentRuntimeEndpointSpecRejectsUserinfo(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		forbidden []string
	}{
		{
			name: "username", endpoint: "https://" + "operator" + "@runtime.example.invalid",
			forbidden: []string{"operator@"},
		},
		{
			name: "username and password", endpoint: "https://" + "operator" + ":" + "passphrase" + "@runtime.example.invalid",
			forbidden: []string{"operator", "passphrase"},
		},
		{
			name: "percent-encoded userinfo", endpoint: "https://" + "%6fperator" + ":" + "p%40ss" + "@runtime.example.invalid",
			forbidden: []string{"%6fperator", "p%40ss", "operator", "p@ss"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHarnessV1AgentRuntimeEndpointSpec(tt.endpoint)
			if err == nil || !strings.Contains(err.Error(), "must not include userinfo") {
				t.Fatalf("validateHarnessV1AgentRuntimeEndpointSpec() error = %v, want userinfo rejection", err)
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("endpoint validation error disclosed URL userinfo: %q", err)
				}
			}
		})
	}
}

func TestValidateHarnessV1AgentRuntimeExecutableCapabilitiesRequiresControllerCompatibleRuntime(t *testing.T) {
	base := harness.CapabilitiesResponse{
		RuntimeName:             "external-v1",
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		MaxOutputBytes:          harness.MaxFetchTurnOutputBytes,
	}
	if err := validateHarnessV1AgentRuntimeExecutableCapabilities(&base); err != nil {
		t.Fatalf("controller-compatible capabilities rejected: %v", err)
	}

	brokeredWithoutCancel := base
	brokeredWithoutCancel.ToolExecutionModes = []harness.ToolExecutionMode{harness.ToolExecutionModeBrokered}
	brokeredWithoutCancel.BrokeredToolClasses = []harness.BrokeredToolClass{harness.BrokeredToolClassRead}
	brokeredWithoutCancel.SupportsContinuation = true
	brokeredWithoutCancel.SupportsCancel = false
	if err := validateHarnessV1AgentRuntimeExecutableCapabilities(&brokeredWithoutCancel); err == nil ||
		!strings.Contains(err.Error(), "supportsCancel") {
		t.Fatalf("brokered runtime without cancellation error = %v, want supportsCancel", err)
	}

	oversizedOutput := base
	oversizedOutput.MaxOutputBytes = harness.MaxFetchTurnOutputBytes + 1
	if err := validateHarnessV1AgentRuntimeExecutableCapabilities(&oversizedOutput); err == nil ||
		!strings.Contains(err.Error(), "fetch limit") {
		t.Fatalf("oversized output capability error = %v, want fetch limit", err)
	}
}

func TestAgentRuntimeReconcilerHarnessV1RequiresDeclaredCapabilitySubset(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: agentRuntimeV1TestBearer})
	defer server.Close()
	runtimeObject, secret := testHarnessV1AgentRuntimeAndSecret(server.URL())
	required := true
	runtimeObject.Spec.Capabilities.SupportsArtifacts = &required
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if updated.Status.Ready || !strings.Contains(updated.Status.Message, "supportsArtifacts") {
		t.Fatalf("missing declared harness v1 capability status = %#v", updated.Status)
	}
}

func TestAgentRuntimeReconcilerHarnessV1AuthRotationForcesConformance(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: agentRuntimeV1TestBearer})
	defer server.Close()
	runtimeObject, secret := testHarnessV1AgentRuntimeAndSecret(server.URL())
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	ready := getAgentRuntime(t, reconciler, runtimeObject)
	if !ready.Status.Ready {
		t.Fatalf("initial harness v1 readiness = %#v", ready.Status)
	}
	stored := &corev1.Secret{}
	if err := reconciler.Get(t.Context(), client.ObjectKeyFromObject(secret), stored); err != nil {
		t.Fatal(err)
	}
	stored.Data["token"] = []byte("rotated-token-not-accepted-by-runtime")
	if err := reconciler.Update(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if updated.Status.Ready ||
		(!strings.Contains(updated.Status.Message, "unauthorized") && !strings.Contains(updated.Status.Message, "401")) {
		t.Fatalf("rotated harness v1 auth did not fail closed = %#v", updated.Status)
	}
	if updated.Status.ObservedAuthRefResourceVersion == ready.Status.ObservedAuthRefResourceVersion {
		t.Fatalf("observed harness v1 auth resourceVersion did not advance: before=%q after=%q",
			ready.Status.ObservedAuthRefResourceVersion, updated.Status.ObservedAuthRefResourceVersion)
	}
}

func TestAgentRuntimeReconcilerHarnessV1ShallowProbeRejectsRuntimeBearerDrift(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{
		RuntimeName: "external-v1", AuthToken: strings.Repeat("r", agentRuntimeMinBearerBytes),
	})
	defer server.Close()
	runtimeObject, secret := testHarnessV1AgentRuntimeAndSecret(server.URL())
	runtimeObject.Status = corev1alpha1.AgentRuntimeStatus{
		Ready:                          true,
		ObservedGeneration:             runtimeObject.Generation,
		ObservedAuthRefResourceVersion: secret.ResourceVersion,
		ObservedCapabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
			ProtocolVersion: harness.ProtocolVersion,
			Transport:       harness.HTTPTransport,
			RuntimeName:     "external-v1",
		},
	}
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if updated.Status.Ready || !strings.Contains(updated.Status.Message, "configured bearer was rejected") {
		t.Fatalf("runtime-side bearer drift did not fail the shallow probe: %#v", updated.Status)
	}
	if strings.Contains(updated.Status.Message, agentRuntimeV1TestBearer) {
		t.Fatal("shallow probe status leaked the configured bearer")
	}
}

func TestAgentRuntimeReconcilerHarnessV1UsesConfiguredTLSClient(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{
		AuthToken: agentRuntimeV1TestBearer,
		TLS:       true,
	})
	defer server.Close()
	runtimeObject, secret := testHarnessV1AgentRuntimeAndSecret(server.URL())
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	reconciler.HarnessV1HTTPClient = server.Client()
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if !updated.Status.Ready {
		t.Fatalf("private-CA harness v1 readiness = %#v", updated.Status)
	}
}

func TestAgentRuntimeReconcilerHarnessV1RejectsSecretIdentityChangeDuringConformance(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*corev1.Secret)
		wantMessage string
	}{
		{
			name:        "UID replacement",
			mutate:      func(secret *corev1.Secret) { secret.UID = types.UID("replacement-secret-uid") },
			wantMessage: "replaced during conformance",
		},
		{
			name:        "resourceVersion rotation",
			mutate:      func(secret *corev1.Secret) { secret.ResourceVersion = "rotated-resource-version" },
			wantMessage: "changed during conformance",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: agentRuntimeV1TestBearer})
			defer server.Close()
			runtimeObject, secret := testHarnessV1AgentRuntimeAndSecret(server.URL())
			reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
			reconciler.APIReader = &agentRuntimeMutateSecondSecretReadClient{
				Client: reconciler.Client, SecretKey: client.ObjectKeyFromObject(secret), Mutate: test.mutate,
			}
			allowAgentRuntimeLoopback(t)
			if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
				t.Fatal(err)
			}
			updated := getAgentRuntime(t, reconciler, runtimeObject)
			if updated.Status.Ready || !strings.Contains(updated.Status.Message, test.wantMessage) {
				t.Fatalf("Secret identity race status = %#v", updated.Status)
			}
		})
	}
}

func TestAgentRuntimeReconcilerHarnessV1RequiresSecretUID(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: agentRuntimeV1TestBearer})
	defer server.Close()
	runtimeObject, secret := testHarnessV1AgentRuntimeAndSecret(server.URL())
	secret.UID = ""
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if updated.Status.Ready || !strings.Contains(updated.Status.Message, "UID is required") {
		t.Fatalf("missing bearer Secret UID status = %#v", updated.Status)
	}
}

func TestAgentRuntimeReconcilerUnclassifiedContractNotReady(t *testing.T) {
	runtimeObject := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unclassified", Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint, Endpoint: "https://runtime.example.com"},
		},
	}
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if updated.Status.Ready || !strings.Contains(updated.Status.Message, "unclassified") {
		t.Fatalf("unclassified runtime status = %#v", updated.Status)
	}
}

func TestValidateAgentRuntimeSpecRequiresExactCanonicalProfileDigest(t *testing.T) {
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{RuntimeInstanceID: "runtime-1", Profile: profile, Limits: limits, WorkspaceGovernance: claims}
	runtimeObject, _ := testAgentRuntimeAndSecret(t, "https://runtime.example.com", config)
	runtimeObject.Spec.Capabilities.Profile.Digest = testControllerDigest("wrong")
	if err := validateAgentRuntimeSpec(runtimeObject); err == nil || !strings.Contains(err.Error(), "canonical digest") {
		t.Fatalf("validateAgentRuntimeSpec() = %v, want canonical digest mismatch", err)
	}
}

func TestValidateAgentRuntimeSpecRejectsTrustedRuntimeStrictClaims(t *testing.T) {
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	claims.Mode = v2conformance.WorkspaceGovernanceTrusted
	claims.Trusted = true
	config := conformancetest.Config{RuntimeInstanceID: "runtime-1", Profile: profile, Limits: limits, WorkspaceGovernance: claims}
	runtimeObject, _ := testAgentRuntimeAndSecret(t, "https://runtime.example.com", config)
	if err := validateAgentRuntimeSpec(runtimeObject); err == nil || !strings.Contains(err.Error(), "must not claim strict") {
		t.Fatalf("validateAgentRuntimeSpec() = %v, want trusted strict-claim rejection", err)
	}
}

func TestAgentRuntimeTrustedNonGovernedRegistrationIsExplicitAndNotStrictEligible(t *testing.T) {
	profile, _, limits := testAgentRuntimeProfileClaimsAndLimits()
	claims := v2conformance.WorkspaceGovernanceClaims{Mode: v2conformance.WorkspaceGovernanceTrusted, Trusted: true}
	config := conformancetest.Config{
		ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID: "trusted-runtime-1", SupervisorBootID: "boot-1", RuntimePoolUID: "external-pool-1",
		Profile: profile, Limits: limits, WorkspaceGovernance: claims,
	}
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	runtimeObject, secret := testAgentRuntimeAndSecret(t, server.URL(), config)
	if runtimeObject.Spec.Capabilities.SupportsStrictWorkspaceIntent(corev1alpha1.WorkspaceIntentRead) ||
		runtimeObject.Spec.Capabilities.SupportsStrictWorkspaceIntent(corev1alpha1.WorkspaceIntentWrite) {
		t.Fatal("trusted non-governed runtime was eligible for strict workspace intent")
	}
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if !updated.Status.Ready || updated.Status.ObservedCapabilities == nil ||
		updated.Status.ObservedCapabilities.WorkspaceGovernance == nil ||
		!updated.Status.ObservedCapabilities.WorkspaceGovernance.Trusted {
		t.Fatalf("trusted runtime registration = %#v", updated.Status)
	}
	if server.Counts().WorkspaceDeltas != 0 {
		t.Fatal("trusted non-governed conformance unexpectedly relied on strict workspace delta production")
	}
}

func TestAgentRuntimeReconcilerFailsClosedWhenStatusIsUnauthenticated(t *testing.T) {
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{
		ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID: "runtime-1", SupervisorBootID: "boot-1", RuntimePoolUID: "pool-1",
		Profile: profile, Limits: limits, WorkspaceGovernance: claims, AllowUnauthenticatedStatus: true,
	}
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	runtimeObject, secret := testAgentRuntimeAndSecret(t, server.URL(), config)
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if updated.Status.Ready || !strings.Contains(updated.Status.Message, "status negative probe") {
		t.Fatalf("unauthenticated status exposure = %#v", updated.Status)
	}
}

func TestAgentRuntimeReconcilerRejectsMissingCapabilitySecretKey(t *testing.T) {
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{
		ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID: "runtime-1", Profile: profile, Limits: limits, WorkspaceGovernance: claims,
	}
	runtimeObject, secret := testAgentRuntimeAndSecret(t, "https://runtime.example.com", config)
	delete(secret.Data, "capability-secret")
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err != nil {
		t.Fatal(err)
	}
	updated := getAgentRuntime(t, reconciler, runtimeObject)
	if updated.Status.Ready || !strings.Contains(updated.Status.Message, "at least 32 bytes") {
		t.Fatalf("missing capability key status = %#v", updated.Status)
	}
}

func TestAgentRuntimeEndpointPolicy(t *testing.T) {
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{RuntimeInstanceID: "runtime-1", Profile: profile, Limits: limits, WorkspaceGovernance: claims}
	runtimeObject, _ := testAgentRuntimeAndSecret(t, "http://runtime.default.svc.cluster.local:8080", config)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "runtime"}}
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, service)
	if err := reconciler.validateAgentRuntimeEndpointPolicy(t.Context(), runtimeObject); err != nil {
		t.Fatalf("same-namespace Service endpoint: %v", err)
	}
	runtimeObject.Spec.Deployment.Endpoint = "http://runtime.other.svc.cluster.local:8080"
	if err := reconciler.validateAgentRuntimeEndpointPolicy(t.Context(), runtimeObject); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("cross-namespace endpoint error = %v", err)
	}
	runtimeObject.Spec.Deployment.Endpoint = "http://runtime.example.com"
	if err := reconciler.validateAgentRuntimeEndpointPolicy(t.Context(), runtimeObject); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("external HTTP endpoint error = %v", err)
	}
	for _, endpoint := range []string{
		"https://100.64.0.1", "https://198.18.0.5", "https://192.0.2.9", "https://[2002::1]",
	} {
		runtimeObject.Spec.Deployment.Endpoint = endpoint
		if err := reconciler.validateAgentRuntimeEndpointPolicy(t.Context(), runtimeObject); err == nil || !strings.Contains(err.Error(), "non-public IP") {
			t.Fatalf("special-use endpoint %s error = %v", endpoint, err)
		}
	}
	service.Spec.Type = corev1.ServiceTypeExternalName
	service.Spec.ExternalName = "internal.other-namespace.svc.cluster.local"
	if err := reconciler.Update(t.Context(), service); err != nil {
		t.Fatal(err)
	}
	runtimeObject.Spec.Deployment.Endpoint = "http://runtime.default.svc.cluster.local:8080"
	if err := reconciler.validateAgentRuntimeEndpointPolicy(t.Context(), runtimeObject); err == nil || !strings.Contains(err.Error(), "ExternalName") {
		t.Fatalf("ExternalName Service endpoint error = %v", err)
	}
}

func TestValidateHarnessV1AgentRuntimeEndpointSpecRequiresTLS(t *testing.T) {
	for _, endpoint := range []string{
		"http://runtime.default.svc:8080",
		"http://runtime.default.svc.cluster.local:8080",
		"http://runtime.example.invalid",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if err := validateHarnessV1AgentRuntimeEndpointSpec(endpoint); err == nil || !strings.Contains(err.Error(), "must use https") {
				t.Fatalf("validateHarnessV1AgentRuntimeEndpointSpec(%q) error = %v, want TLS rejection", endpoint, err)
			}
		})
	}
	if err := validateHarnessV1AgentRuntimeEndpointSpec("https://runtime.default.svc:8443"); err != nil {
		t.Fatalf("TLS Service endpoint rejected: %v", err)
	}
}

func TestAgentRuntimeProfilePreservesModelLimits(t *testing.T) {
	base, _, _ := testAgentRuntimeProfileClaimsAndLimits()
	base.ProviderKind = runtimePoolProviderOpencode
	base.Model = "openai/gpt-test"
	base.AdapterDigests = map[string]string{"opencode": testControllerDigest("opencode-adapter")}
	base.ProxyCredentialScope = "model:openai/gpt-test"
	base.ModelLimits = &harnessv2.ModelTokenLimits{Context: 32768, Output: 4096}
	spec := corev1alpha1.AgentRuntimeProfileSpec{
		DigestSchemaVersion:      int32(harnessv2.ProfileDigestSchemaVersion),
		ACPProfile:               base.ACPProfile,
		AdapterName:              "opencode",
		AdapterDigest:            base.AdapterDigests["opencode"],
		ProviderKind:             base.ProviderKind,
		Model:                    base.Model,
		ModelLimits:              &corev1alpha1.ModelTokenLimits{Context: 32768, Output: 4096},
		AgentConfigurationDigest: base.AgentConfigurationDigest,
		ToolPolicyDigest:         base.ToolPolicyDigest,
		ApprovalPolicyDigest:     base.ApprovalPolicyDigest,
		MCPConfigurationDigest:   base.MCPConfigurationDigest,
		WorkspaceIntent:          corev1alpha1.WorkspaceIntent(base.WorkspaceIntent),
		ProxyCredentialRole:      base.ProxyCredentialRole,
		ProxyCredentialScope:     base.ProxyCredentialScope,
		ResourceClass:            base.ResourceClass,
	}
	profile, err := agentRuntimeProfile(spec)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ModelLimits == nil || profile.ModelLimits.Context != 32768 || profile.ModelLimits.Output != 4096 {
		t.Fatalf("model limits = %#v", profile.ModelLimits)
	}
}

func testAgentRuntimeAndSecret(t *testing.T, endpoint string, config conformancetest.Config) (*corev1alpha1.AgentRuntime, *corev1.Secret) {
	t.Helper()
	profileDigest, err := harnessv2.CanonicalProfileDigest(config.Profile)
	if err != nil {
		t.Fatal(err)
	}
	adapterName, adapterDigest := "", ""
	for adapterName, adapterDigest = range config.Profile.AdapterDigests {
		break
	}
	var apiModelLimits *corev1alpha1.ModelTokenLimits
	if config.Profile.ModelLimits != nil {
		apiModelLimits = &corev1alpha1.ModelTokenLimits{
			Context: config.Profile.ModelLimits.Context,
			Output:  config.Profile.ModelLimits.Output,
		}
	}
	runtimeObject := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "runtime", Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV2),
			Deployment:      corev1alpha1.AgentRuntimeDeploymentSpec{Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint, Endpoint: endpoint},
			ClientAuth: corev1alpha1.AgentRuntimeClientAuth{
				ControllerBearerTokenSecretRef: &corev1alpha1.AgentRuntimeSecretKeyReference{Name: "runtime-auth", Key: "controller-token"},
				OperationCapabilitySecretRef:   &corev1alpha1.AgentRuntimeSecretKeyReference{Name: "runtime-auth", Key: "capability-secret"},
			},
			Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{
				RuntimeInstanceID: string(config.RuntimeInstanceID),
				Profile: &corev1alpha1.AgentRuntimeProfileSpec{
					Digest: string(profileDigest), DigestSchemaVersion: int32(harnessv2.ProfileDigestSchemaVersion),
					ACPProfile: config.Profile.ACPProfile, AdapterName: adapterName, AdapterDigest: adapterDigest,
					ProviderKind: config.Profile.ProviderKind, Model: config.Profile.Model, ModelLimits: apiModelLimits,
					AgentConfigurationDigest: config.Profile.AgentConfigurationDigest,
					ToolPolicyDigest:         config.Profile.ToolPolicyDigest, ApprovalPolicyDigest: config.Profile.ApprovalPolicyDigest,
					MCPConfigurationDigest: config.Profile.MCPConfigurationDigest,
					WorkspaceIntent:        corev1alpha1.WorkspaceIntent(config.Profile.WorkspaceIntent),
					ProxyCredentialRole:    config.Profile.ProxyCredentialRole, ProxyCredentialScope: config.Profile.ProxyCredentialScope,
					ResourceClass: config.Profile.ResourceClass,
				},
				Limits: &corev1alpha1.AgentRuntimeProtocolLimits{
					MaxResidentSessions: int32(config.Limits.MaxResidentSessions), MaxConcurrentPrompts: int32(config.Limits.MaxConcurrentPrompts),
					MaxRequestBytes: int32(config.Limits.MaxRequestBytes), MaxEventLineBytes: int32(config.Limits.MaxEventLineBytes),
					MaxTerminalResultBytes: int32(config.Limits.MaxTerminalResultBytes), MaxBufferedEvents: int32(config.Limits.MaxBufferedEvents),
					MaxUpdateEventsPerSecond: int32(config.Limits.MaxUpdateEventsPerSecond), MinPromptLeaseMillis: config.Limits.MinPromptLeaseMillis,
					MaxPromptLeaseMillis: config.Limits.MaxPromptLeaseMillis, MaxPendingPermissions: int32(config.Limits.MaxPendingPermissions),
					MaxWorkspaceDeltaBytes: config.Limits.MaxWorkspaceDeltaBytes,
				},
				SupportsDrain: config.SupportsDrain,
				WorkspaceGovernance: &corev1alpha1.AgentRuntimeWorkspaceGovernanceCapabilities{
					Mode: corev1alpha1.AgentRuntimeWorkspaceGovernanceMode(config.WorkspaceGovernance.Mode), Trusted: config.WorkspaceGovernance.Trusted,
					OrkaOwnedWorkspaceDeltas:        config.WorkspaceGovernance.OrkaOwnedWorkspaceDeltas,
					PromptScopedBrokerAuthorization: config.WorkspaceGovernance.PromptScopedBrokerAuthorization,
					NoDirectSCMPublication:          config.WorkspaceGovernance.NoDirectSCMPublication,
					OrkaOwnedCleanRoomPublication:   config.WorkspaceGovernance.OrkaOwnedCleanRoomPublication,
					ExactInstanceFencing:            config.WorkspaceGovernance.ExactInstanceFencing,
					DuplicateSafeMutations:          config.WorkspaceGovernance.DuplicateSafeMutations,
					CancellationSettlement:          config.WorkspaceGovernance.CancellationSettlement,
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "runtime-auth",
			Labels:      map[string]string{agentRuntimeAuthUseLabel: "true", agentRuntimeAuthRefNameLabel: runtimeObject.Name},
			Annotations: map[string]string{agentRuntimeAuthEndpointAnnotation: endpoint},
		},
		Data: map[string][]byte{
			"controller-token":  []byte(config.ControllerBearerToken),
			"capability-secret": append([]byte(nil), config.OperationCapabilitySecret...),
		},
	}
	return runtimeObject, secret
}

func testHarnessV1AgentRuntimeAndSecret(endpoint string) (*corev1alpha1.AgentRuntime, *corev1.Secret) {
	supportsCancel := true
	supportsRuntimeSessions := true
	runtimeObject := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "runtime-v1", Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV1),
			Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
				Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint, Endpoint: endpoint,
			},
			ClientAuth: corev1alpha1.AgentRuntimeClientAuth{
				BearerAuthRef: &corev1alpha1.AgentRuntimeBearerAuthReference{Name: "runtime-v1-auth", Key: "token"},
			},
			Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{
				ToolExecutionModes: []corev1alpha1.AgentRuntimeToolExecutionMode{
					corev1alpha1.AgentRuntimeToolExecutionModeObserved,
				},
				SupportsCancel:          &supportsCancel,
				SupportsRuntimeSessions: &supportsRuntimeSessions,
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "runtime-v1-auth", UID: types.UID("runtime-v1-auth-uid"), ResourceVersion: "1",
			Labels: map[string]string{
				agentRuntimeAuthUseLabel: scheduledRunLabelValue, agentRuntimeAuthRefNameLabel: runtimeObject.Name,
			},
			Annotations: map[string]string{agentRuntimeAuthEndpointAnnotation: endpoint},
		},
		Data: map[string][]byte{"token": []byte(agentRuntimeV1TestBearer)},
	}
	return runtimeObject, secret
}

func testAgentRuntimeProfileClaimsAndLimits() (harnessv2.RuntimeProfile, v2conformance.WorkspaceGovernanceClaims, harnessv2.ProtocolLimits) {
	toolPolicy := harnessv2.MCPToolPolicy{AllowedToolNames: []string{}, Tools: []harnessv2.MCPToolDescriptor{}}
	toolPolicyDigest, _ := harnessv2.CanonicalRuntimeToolPolicyDigest(toolPolicy.AllowedToolNames, toolPolicy.DisallowedToolNames, toolPolicy.AllowBash)
	approvalPolicyDigest, _ := harnessv2.CanonicalMCPApprovalPolicyDigest(harnessv2.MCPApprovalPolicy{})
	mcpConfigurationDigest, _ := harnessv2.CanonicalMCPConfigurationDigest(toolPolicy.AllowedToolNames)
	profile := harnessv2.RuntimeProfile{
		ACPProfile:     harnessv2.ACPProfileV1,
		AdapterDigests: map[string]string{"codex": testControllerDigest("adapter")},
		ProviderKind:   "codex", Model: acpTestModel,
		AgentConfigurationDigest: testControllerDigest("agent"), ToolPolicyDigest: toolPolicyDigest,
		ApprovalPolicyDigest: approvalPolicyDigest, MCPConfigurationDigest: mcpConfigurationDigest,
		WorkspaceIntent:     harnessv2.WorkspaceIntentRead,
		ProxyCredentialRole: "provider-proxy", ProxyCredentialScope: "session-and-prompt", ResourceClass: "standard",
	}
	claims := v2conformance.WorkspaceGovernanceClaims{
		Mode:                     v2conformance.WorkspaceGovernanceStrict,
		OrkaOwnedWorkspaceDeltas: true, PromptScopedBrokerAuthorization: true,
		NoDirectSCMPublication: true, OrkaOwnedCleanRoomPublication: true,
		ExactInstanceFencing: true, DuplicateSafeMutations: true, CancellationSettlement: true,
	}
	return profile, claims, harnessv2.DefaultProtocolLimits()
}

func testControllerDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func reconcileRequestFor(object client.Object) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}
}

func newAgentRuntimeUnitReconciler(t *testing.T, objects ...client.Object) *AgentRuntimeReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.AgentRuntime{}).WithObjects(objects...).Build()
	return &AgentRuntimeReconciler{Client: fakeClient, APIReader: fakeClient, Scheme: scheme}
}

func getAgentRuntime(t *testing.T, reconciler *AgentRuntimeReconciler, object *corev1alpha1.AgentRuntime) corev1alpha1.AgentRuntime {
	t.Helper()
	var updated corev1alpha1.AgentRuntime
	if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(object), &updated); err != nil {
		t.Fatal(err)
	}
	return updated
}

func allowAgentRuntimeLoopback(t *testing.T) {
	t.Helper()
	agentRuntimeAllowInsecureLoopbackForTests = true
	t.Cleanup(func() { agentRuntimeAllowInsecureLoopbackForTests = false })
}

type agentRuntimeMutateSecondSecretReadClient struct {
	client.Client
	SecretKey client.ObjectKey
	Mutate    func(*corev1.Secret)
	reads     int
}

func (c *agentRuntimeMutateSecondSecretReadClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if err := c.Client.Get(ctx, key, object, options...); err != nil {
		return err
	}
	secret, ok := object.(*corev1.Secret)
	if !ok || key != c.SecretKey {
		return nil
	}
	c.reads++
	if c.reads == 2 && c.Mutate != nil {
		c.Mutate(secret)
	}
	return nil
}
