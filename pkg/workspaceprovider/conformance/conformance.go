// Package conformance provides a reusable provider-neutral lifecycle suite for
// out-of-tree workspace adapter drivers.
package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceagent"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

// Fixtures supplies valid generic objects owned by the driver under test.
type Fixtures struct {
	Provider    *workspacev1alpha1.ExecutionWorkspaceProvider
	Pool        *workspacev1alpha1.ExecutionWorkspacePool
	Interactive *workspacev1alpha1.ExecutionWorkspace
	Service     *workspacev1alpha1.ExecutionWorkspace
	DataPlane   *DataPlaneFixture

	// TransitionTimeout bounds each provider call/transition. Zero uses 10 seconds.
	TransitionTimeout time.Duration
	// PollInterval controls retries for asynchronous observations. Zero uses 25 milliseconds.
	PollInterval time.Duration
}

// DataPlaneFixture supplies a disposable ready workspace and raw attachment
// values used to exercise the public workspace-agent client contract.
type DataPlaneFixture struct {
	Workspace          *workspacev1alpha1.ExecutionWorkspace
	Connection         workspaceprovider.ConnectionData
	Bearer             string
	ExecCommand        []string
	ExecVerifyCommand  []string
	ExecExpectedStdout string
	FilePath           string
	FileData           []byte
	ResetPaths         []string
}

type conformanceTiming struct {
	timeout      time.Duration
	pollInterval time.Duration
}

const (
	workspaceTransitionTimeout      = 10 * time.Second
	workspaceTransitionPollInterval = 25 * time.Millisecond
)

// Run executes provider registration, pool, idempotent workspace, attachment,
// suspension, deletion, and service endpoint contract checks.
func Run(t *testing.T, driver workspaceprovider.Driver, fixtures Fixtures) {
	t.Helper()
	if driver == nil {
		t.Fatal("workspace provider driver is nil")
	}
	timing := conformanceTimingFor(fixtures)
	metadata := driver.Metadata()
	if err := validateAdapterMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	if err := validateFixtures(metadata, fixtures); err != nil {
		t.Fatal(err)
	}

	t.Run("provider observation", func(t *testing.T) {
		runProviderObservation(t, driver, fixtures.Provider, metadata, timing)
	})
	t.Run("pool observation", func(t *testing.T) {
		if !slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeaturePools) {
			t.Skip("driver does not advertise pool support")
		}
		runPoolObservation(t, driver, fixtures.Pool, timing)
	})
	t.Run("workspace admission gate", func(t *testing.T) {
		for _, fixture := range workspaceAdmissionFixtures(fixtures) {
			t.Run(fixture.name, func(t *testing.T) {
				runWorkspaceAdmissionGate(t, driver, fixture.name, fixture.workspace, timing)
			})
		}
	})
	t.Run("workspace-agent data plane", func(t *testing.T) {
		if !requiresDataPlane(metadata.Features) {
			t.Skip("driver does not advertise workspace-agent data-plane features")
		}
		providerObservation := runProviderObservation(t, driver, fixtures.Provider, metadata, timing)
		runDataPlaneConformance(
			t, driver, fixtures.Provider, fixtures.DataPlane, metadata, providerObservation, timing,
		)
	})
	t.Run("interactive idempotency and lifecycle", func(t *testing.T) {
		if !slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureExec) {
			t.Skip("driver does not advertise interactive exec support")
		}
		providerObservation := runProviderObservation(t, driver, fixtures.Provider, metadata, timing)
		runInteractiveLifecycle(
			t, driver, fixtures.Provider, fixtures.Interactive, metadata, providerObservation, timing,
		)
	})
	t.Run("service endpoint", func(t *testing.T) {
		if !slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureServicePorts) {
			t.Skip("driver does not advertise service endpoint support")
		}
		providerObservation := runProviderObservation(t, driver, fixtures.Provider, metadata, timing)
		runServiceEndpoint(
			t, driver, fixtures.Provider, fixtures.Service, metadata, providerObservation, timing,
		)
	})
}

type workspaceAdmissionConformanceFixture struct {
	name      string
	workspace *workspacev1alpha1.ExecutionWorkspace
}

func workspaceAdmissionFixtures(fixtures Fixtures) []workspaceAdmissionConformanceFixture {
	var result []workspaceAdmissionConformanceFixture
	if fixtures.DataPlane != nil && fixtures.DataPlane.Workspace != nil {
		result = append(result, workspaceAdmissionConformanceFixture{
			name: "data-plane", workspace: fixtures.DataPlane.Workspace,
		})
	}
	if fixtures.Interactive != nil {
		result = append(result, workspaceAdmissionConformanceFixture{name: "interactive", workspace: fixtures.Interactive})
	}
	if fixtures.Service != nil {
		result = append(result, workspaceAdmissionConformanceFixture{name: "service", workspace: fixtures.Service})
	}
	return result
}

func runWorkspaceAdmissionGate(
	t *testing.T,
	driver workspaceprovider.Driver,
	fixtureName string,
	fixture *workspacev1alpha1.ExecutionWorkspace,
	timing conformanceTiming,
) {
	t.Helper()
	if fixture == nil {
		t.Fatal("workspace fixture is required")
	}
	runID := time.Now().UnixNano()
	tests := []struct {
		name      string
		configure func(*workspacev1alpha1.ExecutionWorkspace)
	}{
		{name: "missing core admission"},
		{
			name: "current marker without condition",
			configure: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				setWorkspaceCoreAdmissionMarker(workspace, workspace.Generation)
			},
		},
		{
			name: "current marker with denied condition",
			configure: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				setWorkspaceCoreAdmissionMarker(workspace, workspace.Generation)
				setWorkspaceAdmissionConditionValue(
					workspace, metav1.ConditionFalse, string(workspacev1alpha1.ReasonReady), workspace.Generation,
				)
			},
		},
		{
			name: "current marker with wrong condition reason",
			configure: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				setWorkspaceCoreAdmissionMarker(workspace, workspace.Generation)
				setWorkspaceAdmissionConditionValue(
					workspace, metav1.ConditionTrue, "NotReady", workspace.Generation,
				)
			},
		},
		{
			name: "current marker with stale condition generation",
			configure: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Generation++
				setWorkspaceCoreAdmissionMarker(workspace, workspace.Generation)
				setWorkspaceAdmissionCondition(workspace, workspace.Generation-1)
			},
		},
		{
			name: "forged adapter status without protected admission",
			configure: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Status.ObservedGeneration = workspace.Generation
				workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
				workspace.Status.ExternalID = "forged/external-id"
				workspace.Status.ProviderBinding = &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
					ContractVersion: workspacev1alpha1.ContractVersionV1,
				}
				setWorkspaceAdmissionCondition(workspace, workspace.Generation)
			},
		},
		{
			name: "stale admitted generation with forged current condition",
			configure: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Generation++
				setWorkspaceCoreAdmissionMarker(workspace, workspace.Generation-1)
				setWorkspaceAdmissionCondition(workspace, workspace.Generation)
			},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := fixture.DeepCopy()
			isolateWorkspaceConformanceIdentity(
				workspace, fmt.Sprintf("%s-%d-admission-%d", fixtureName, runID, index),
			)
			if workspace.Generation == 0 {
				workspace.Generation = 1
			}
			workspace.Spec.CoreAdmission = nil
			removeWorkspaceAdmissionCondition(workspace)
			if test.configure != nil {
				test.configure(workspace)
			}
			probeTimeout := min(timing.timeout, 250*time.Millisecond)
			for attempt := range 3 {
				ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
				observation, err := driver.ReconcileWorkspace(ctx, workspace.DeepCopy())
				cancel()
				if validationErr := validateWorkspaceAdmissionProbeResult(
					observation, err, test.name,
				); validationErr != nil {
					t.Fatal(validationErr)
				}
				if attempt < 2 {
					time.Sleep(timing.pollInterval)
				}
			}
		})
	}
	for index, maintenance := range []struct {
		name        string
		desired     workspacev1alpha1.ExecutionWorkspaceDesiredState
		state       workspacev1alpha1.ExecutionWorkspaceState
		staleMarker bool
	}{
		{
			name:    "quarantine without admission",
			desired: workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined,
			state:   workspacev1alpha1.ExecutionWorkspaceStateQuarantined,
		},
		{
			name:    "deletion without admission",
			desired: workspacev1alpha1.ExecutionWorkspaceDesiredDeleted,
			state:   workspacev1alpha1.ExecutionWorkspaceStateDeleted,
		},
		{
			name:        "quarantine with stale admission",
			desired:     workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined,
			state:       workspacev1alpha1.ExecutionWorkspaceStateQuarantined,
			staleMarker: true,
		},
		{
			name:        "deletion with stale admission",
			desired:     workspacev1alpha1.ExecutionWorkspaceDesiredDeleted,
			state:       workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			staleMarker: true,
		},
	} {
		t.Run(maintenance.name, func(t *testing.T) {
			workspace := fixture.DeepCopy()
			isolateWorkspaceConformanceIdentity(
				workspace, fmt.Sprintf("%s-%d-maintenance-%d", fixtureName, runID, index),
			)
			if workspace.Generation == 0 {
				workspace.Generation = 1
			}
			workspace.Spec.CoreAdmission = nil
			removeWorkspaceAdmissionCondition(workspace)
			workspace.Spec.Attachment = nil
			workspace.Spec.DesiredState = maintenance.desired
			if maintenance.staleMarker {
				workspace.Generation = 2
				setWorkspaceCoreAdmissionMarker(workspace, 1)
			}
			observation := reconcileWorkspaceUntil(
				t, context.Background(), driver, workspace, maintenance.name, timing,
				func(observation workspaceprovider.WorkspaceObservation) bool {
					return observation.State == maintenance.state
				},
			)
			if observation.ConnectionRef != nil || len(observation.Endpoints) > 0 || observation.AttachedEpoch != 0 {
				t.Fatalf("%s exposed normal data-plane state: %#v", maintenance.name, observation)
			}
			if maintenance.state == workspacev1alpha1.ExecutionWorkspaceStateDeleted &&
				!validDeletedDisposition(observation.Disposition, workspace.Spec.Lifecycle.DeletionPolicy) {
				t.Fatalf("%s missing valid cleanup disposition: %#v", maintenance.name, observation)
			}
		})
	}
}

func isolateWorkspaceConformanceIdentity(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	suffix string,
) {
	workspace.Name = "orka-conformance-" + suffix
	workspace.UID = types.UID("orka-conformance-" + suffix)
	workspace.ResourceVersion = ""
	workspace.Generation = 1
	workspace.Labels = nil
	workspace.Annotations = nil
	workspace.Finalizers = nil
	workspace.OwnerReferences = nil
	workspace.DeletionTimestamp = nil
	workspace.Status = workspacev1alpha1.ExecutionWorkspaceStatus{}
}

func removeWorkspaceAdmissionCondition(workspace *workspacev1alpha1.ExecutionWorkspace) {
	conditions := workspace.Status.Conditions[:0]
	for _, condition := range workspace.Status.Conditions {
		if condition.Type != string(workspacev1alpha1.ConditionWorkspaceAdmitted) {
			conditions = append(conditions, condition)
		}
	}
	workspace.Status.Conditions = conditions
}

func setWorkspaceCoreAdmissionMarker(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	admittedGeneration int64,
) {
	var poolBinding *workspacev1alpha1.ImmutableObjectBinding
	if workspace.Spec.CoreAdmission != nil && workspace.Spec.CoreAdmission.PoolBinding != nil {
		poolBinding = workspace.Spec.CoreAdmission.PoolBinding.DeepCopy()
	}
	workspace.Spec.CoreAdmission = &workspacev1alpha1.ExecutionWorkspaceCoreAdmission{
		ClassBinding:       workspace.Spec.ClassBinding,
		ProviderBinding:    workspace.Spec.ProviderBinding,
		PoolBinding:        poolBinding,
		AdmittedGeneration: admittedGeneration,
	}
}

func setWorkspaceAdmissionCondition(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	observedGeneration int64,
) {
	setWorkspaceAdmissionConditionValue(
		workspace, metav1.ConditionTrue, string(workspacev1alpha1.ReasonReady), observedGeneration,
	)
}

func setWorkspaceAdmissionConditionValue(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	status metav1.ConditionStatus,
	reason string,
	observedGeneration int64,
) {
	workspaceprovider.SetCondition(&workspace.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		Status:             status,
		Reason:             reason,
		Message:            "workspace admission condition from conformance core fixture",
		ObservedGeneration: observedGeneration,
	})
}

func validateWorkspaceAdmissionProbeResult(
	observation workspaceprovider.WorkspaceObservation,
	reconcileErr error,
	transition string,
) error {
	if reconcileErr != nil && !errors.Is(reconcileErr, workspaceprovider.ErrWorkspaceNotAdmitted) {
		return fmt.Errorf("%s returned a non-admission error: %w", transition, reconcileErr)
	}
	switch observation.State {
	case "", workspacev1alpha1.ExecutionWorkspaceStateFailed, workspacev1alpha1.ExecutionWorkspaceStateQuarantined:
	default:
		return fmt.Errorf("%s progressed before current core admission: %#v", transition, observation)
	}
	if observation.ExternalID != "" || observation.ConnectionRef != nil ||
		observation.ProviderBinding != nil || observation.AttachedEpoch != 0 || len(observation.Endpoints) > 0 {
		return fmt.Errorf("%s published identity before current core admission: %#v", transition, observation)
	}
	return nil
}

func advanceWorkspaceGenerationAndReadmit(
	t *testing.T,
	driver workspaceprovider.Driver,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	transition string,
	timing conformanceTiming,
) {
	t.Helper()
	workspace.Generation++
	ctx, cancel := context.WithTimeout(context.Background(), timing.timeout)
	observation, err := driver.ReconcileWorkspace(ctx, workspace.DeepCopy())
	cancel()
	if validationErr := validateWorkspaceAdmissionProbeResult(
		observation, err, transition+" stale-admission probe",
	); validationErr != nil {
		t.Fatal(validationErr)
	}
	// Core refreshes spec.coreAdmission in a second spec patch, which advances
	// metadata.generation again before the adapter observes current admission.
	workspace.Generation++
	markWorkspaceCoreAdmitted(workspace)
}

func workspaceCurrentlyCoreAdmitted(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil || workspace.Spec.CoreAdmission == nil ||
		workspace.Spec.CoreAdmission.ClassBinding != workspace.Spec.ClassBinding ||
		workspace.Spec.CoreAdmission.ProviderBinding != workspace.Spec.ProviderBinding ||
		workspace.Spec.CoreAdmission.AdmittedGeneration != workspace.Generation {
		return false
	}
	for _, condition := range workspace.Status.Conditions {
		if condition.Type == string(workspacev1alpha1.ConditionWorkspaceAdmitted) {
			return condition.Status == metav1.ConditionTrue &&
				condition.Reason == string(workspacev1alpha1.ReasonReady) &&
				condition.ObservedGeneration == workspace.Generation
		}
	}
	return false
}

func markWorkspaceCoreAdmitted(workspace *workspacev1alpha1.ExecutionWorkspace) {
	if workspace.Generation == 0 {
		workspace.Generation = 1
	}
	setWorkspaceCoreAdmissionMarker(workspace, workspace.Generation)
	setWorkspaceAdmissionCondition(workspace, workspace.Generation)
}

func validateAdapterMetadata(metadata workspaceprovider.AdapterMetadata) error {
	if metadata.ControllerName == "" || metadata.Version == "" {
		return fmt.Errorf("driver metadata is incomplete: %#v", metadata)
	}
	if err := validateContractList("driver metadata", metadata.Contracts); err != nil {
		return err
	}
	if !contains(metadata.Contracts, workspacev1alpha1.ContractVersionV1) {
		return fmt.Errorf("driver contracts = %v, want %s", metadata.Contracts, workspacev1alpha1.ContractVersionV1)
	}
	if !slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureTLS) {
		return fmt.Errorf("driver metadata must advertise %s", workspacev1alpha1.WorkspaceFeatureTLS)
	}
	if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureReset) &&
		!slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureFiles) {
		return fmt.Errorf("driver metadata advertising reset must also advertise files")
	}
	return nil
}

func validateContractList(kind string, contracts []string) error {
	for _, contract := range contracts {
		if strings.TrimSpace(contract) == "" {
			return fmt.Errorf("%s contains an empty contract", kind)
		}
	}
	return nil
}

func validateProviderObservation(
	metadata workspaceprovider.AdapterMetadata,
	observation workspaceprovider.ProviderObservation,
) error {
	if observation.Adapter.Version != metadata.Version {
		return fmt.Errorf("adapter version = %q, want %q", observation.Adapter.Version, metadata.Version)
	}
	if metadata.Digest != "" && observation.Adapter.Digest != metadata.Digest {
		return fmt.Errorf("adapter digest = %q, want %q", observation.Adapter.Digest, metadata.Digest)
	}
	if !featureSetsEqual(metadata.Features, observation.SupportedFeatures) {
		return fmt.Errorf(
			"provider observed features = %v, want exact metadata features %v",
			observation.SupportedFeatures,
			metadata.Features,
		)
	}
	return nil
}

func featureSetsEqual(
	left, right []workspacev1alpha1.ExecutionWorkspaceFeature,
) bool {
	leftSet := make(map[workspacev1alpha1.ExecutionWorkspaceFeature]struct{}, len(left))
	rightSet := make(map[workspacev1alpha1.ExecutionWorkspaceFeature]struct{}, len(right))
	for _, feature := range left {
		leftSet[feature] = struct{}{}
	}
	for _, feature := range right {
		rightSet[feature] = struct{}{}
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for feature := range leftSet {
		if _, ok := rightSet[feature]; !ok {
			return false
		}
	}
	return true
}

func validateFixtures(metadata workspaceprovider.AdapterMetadata, fixtures Fixtures) error {
	if fixtures.Provider == nil {
		return fmt.Errorf("provider fixture is required")
	}
	if fixtures.Interactive == nil && fixtures.Service == nil &&
		(fixtures.DataPlane == nil || fixtures.DataPlane.Workspace == nil) {
		return fmt.Errorf("at least one workspace fixture is required")
	}
	if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeaturePools) && fixtures.Pool == nil {
		return fmt.Errorf("pool fixture is required when pool support is advertised")
	}
	if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureExec) && fixtures.Interactive == nil {
		return fmt.Errorf("interactive fixture is required when exec support is advertised")
	}
	if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureServicePorts) && fixtures.Service == nil {
		return fmt.Errorf("service fixture is required when service endpoint support is advertised")
	}
	if requiresDataPlane(metadata.Features) && fixtures.DataPlane == nil {
		return fmt.Errorf("data-plane fixture is required when workspace-agent capabilities are advertised")
	}
	return nil
}

func requiresDataPlane(features []workspacev1alpha1.ExecutionWorkspaceFeature) bool {
	return slices.Contains(features, workspacev1alpha1.WorkspaceFeatureExec) ||
		slices.Contains(features, workspacev1alpha1.WorkspaceFeatureFiles) ||
		slices.Contains(features, workspacev1alpha1.WorkspaceFeatureReset)
}

func runProviderObservation(
	t *testing.T,
	driver workspaceprovider.Driver,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	metadata workspaceprovider.AdapterMetadata,
	timing conformanceTiming,
) workspaceprovider.ProviderObservation {
	t.Helper()
	if provider == nil {
		t.Fatal("provider fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timing.timeout)
	defer cancel()
	observation, err := driver.ObserveProvider(ctx, provider.DeepCopy())
	if err != nil {
		t.Fatalf("ObserveProvider: %v", err)
	}
	if err := validateProviderObservation(metadata, observation); err != nil {
		t.Fatalf("provider observation: %v", err)
	}
	return observation
}

func runPoolObservation(
	t *testing.T,
	driver workspaceprovider.Driver,
	pool *workspacev1alpha1.ExecutionWorkspacePool,
	timing conformanceTiming,
) {
	t.Helper()
	if pool == nil {
		t.Fatal("pool fixture is required when pool support is advertised")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timing.timeout)
	defer cancel()
	observation, err := driver.ReconcilePool(ctx, pool.DeepCopy())
	if err != nil {
		t.Fatalf("ReconcilePool: %v", err)
	}
	if err := validatePoolObservation(observation); err != nil {
		t.Fatalf("invalid pool observation: %v: %#v", err, observation)
	}
}

func validatePoolObservation(observation workspaceprovider.PoolObservation) error {
	if observation.Available < 0 || observation.Allocated < 0 ||
		observation.Suspended < 0 || observation.Total < 0 {
		return fmt.Errorf("pool counters must be non-negative")
	}
	accounted := int64(observation.Available) + int64(observation.Allocated) + int64(observation.Suspended)
	if accounted > int64(observation.Total) {
		return fmt.Errorf("available plus allocated plus suspended capacity must not exceed total capacity")
	}
	return nil
}

//nolint:gocyclo // The conformance probe intentionally exercises each advertised data-plane capability.
func runDataPlaneConformance(
	t *testing.T,
	driver workspaceprovider.Driver,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	fixture *DataPlaneFixture,
	metadata workspaceprovider.AdapterMetadata,
	advertised workspaceprovider.ProviderObservation,
	timing conformanceTiming,
) {
	t.Helper()
	if fixture == nil || fixture.Workspace == nil {
		t.Fatal("data-plane workspace fixture is required")
	}
	workspace := fixture.Workspace.DeepCopy()
	markWorkspaceCoreAdmitted(workspace)
	observation := reconcileWorkspaceUntil(
		t,
		context.Background(),
		driver,
		workspace,
		"data-plane provisioning",
		timing,
		func(observation workspaceprovider.WorkspaceObservation) bool {
			return observation.State == workspacev1alpha1.ExecutionWorkspaceStateReady &&
				observation.ExternalID != "" && observation.ConnectionRef != nil &&
				observation.ConnectionRef.Name != ""
		},
	)
	if observation.ConnectionRef == nil || observation.ConnectionRef.Name == "" {
		t.Fatalf("data-plane observation = %#v", observation)
	}
	bindingValidator := &interactiveProviderBindingConformance{
		provider: provider, metadata: metadata, advertised: advertised,
	}
	if err := bindingValidator.validate(observation); err != nil {
		t.Fatalf("data-plane provider binding: %v", err)
	}
	second := reconcileWorkspaceOnce(t, context.Background(), driver, workspace, "data-plane idempotency", timing)
	if err := bindingValidator.validate(second); err != nil {
		t.Fatalf("data-plane idempotent provider binding: %v", err)
	}
	if second.State != workspacev1alpha1.ExecutionWorkspaceStateReady ||
		second.ExternalID != observation.ExternalID || second.ConnectionRef == nil ||
		second.ConnectionRef.Name != observation.ConnectionRef.Name {
		t.Fatalf("data-plane idempotent observation changed identity: first=%#v second=%#v", observation, second)
	}
	if err := validateConformanceConnection(fixture.Connection); err != nil {
		t.Fatalf("connection data: %v", err)
	}
	encoded, err := workspaceprovider.EncodeConnectionData(fixture.Connection)
	if err != nil {
		t.Fatalf("encode connection data: %v", err)
	}
	connection, err := workspaceprovider.ParseConnectionData(encoded)
	if err != nil {
		t.Fatalf("parse connection data: %v", err)
	}
	config := connection.ClientConfig()
	config.Timeout = timing.timeout
	client, err := workspaceagent.NewClient(config)
	if err != nil {
		t.Fatalf("create workspace-agent client: %v", err)
	}
	operationCtx, cancel := context.WithTimeout(context.Background(), timing.timeout)
	health, err := client.Health(operationCtx)
	cancel()
	if err != nil || health.Status != "ok" {
		t.Fatalf("workspace-agent health = %#v, err=%v", health, err)
	}
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	capabilities, err := client.Capabilities(operationCtx)
	cancel()
	if err != nil {
		t.Fatalf("workspace-agent capabilities: %v", err)
	}
	validateDataPlaneCapabilities(t, metadata.Features, capabilities.Features)
	if workspace.UID == "" || strings.TrimSpace(fixture.Bearer) == "" || capabilities.BindingGeneration == "" {
		t.Fatal("data-plane fixture requires workspace UID, bearer, and binding generation")
	}
	workspaceUID := string(workspace.UID)
	bindingGeneration := capabilities.BindingGeneration
	if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureReset) {
		operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
		startupReset, resetErr := client.Reset(operationCtx, workspaceagent.ResetRequest{
			OperationID:       "conformance-startup-reset",
			WorkspaceUID:      workspaceUID,
			BindingGeneration: bindingGeneration,
			Paths:             append([]string(nil), fixture.ResetPaths...),
		})
		cancel()
		if resetErr != nil || !startupReset.Reset || startupReset.BindingGeneration == "" {
			t.Fatalf("startup reset workspace = %#v, err=%v", startupReset, resetErr)
		}
		bindingGeneration = startupReset.BindingGeneration
	}
	epoch := int64(1)
	tokenDigest := sha256.Sum256([]byte(fixture.Bearer))
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	activationRequest := workspaceagent.AttachmentControlRequest{
		WorkspaceUID:      workspaceUID,
		BindingGeneration: bindingGeneration,
		TaskUID:           "conformance-task-uid",
		Epoch:             epoch,
		ExpiresAt:         time.Now().Add(4 * timing.timeout),
	}
	activationRequest.SetTokenDigest(fmt.Sprintf("sha256:%x", tokenDigest))
	activation, err := client.ActivateAttachment(operationCtx, activationRequest)
	cancel()
	if err != nil || !activation.Active || activation.ActiveEpoch != epoch {
		t.Fatalf("activate attachment = %#v, err=%v", activation, err)
	}
	credentials := workspaceagent.AttachmentCredentials{
		WorkspaceUID: workspaceUID,
		Epoch:        epoch,
		Bearer:       fixture.Bearer,
	}
	if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureExec) {
		runExecConformance(t, client, credentials, fixture, timing)
	}
	if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureFiles) {
		runFileConformance(t, client, credentials, fixture, timing)
	}
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	revoked, err := client.RevokeAttachment(
		operationCtx, workspaceUID, bindingGeneration, epoch,
	)
	cancel()
	if err != nil || revoked.Active {
		t.Fatalf("revoke attachment = %#v, err=%v", revoked, err)
	}
	verifyRevokedCredentials(t, client, credentials, fixture, timing)
	if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureReset) {
		operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
		reset, resetErr := client.Reset(operationCtx, workspaceagent.ResetRequest{
			OperationID:       "conformance-reset",
			WorkspaceUID:      workspaceUID,
			BindingGeneration: bindingGeneration,
			Paths:             append([]string(nil), fixture.ResetPaths...),
		})
		cancel()
		if resetErr != nil || !reset.Reset || reset.BindingGeneration == "" {
			t.Fatalf("reset workspace = %#v, err=%v", reset, resetErr)
		}
		operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
		_, staleErr := client.Reset(operationCtx, workspaceagent.ResetRequest{
			OperationID:       "conformance-reset-stale",
			WorkspaceUID:      workspaceUID,
			BindingGeneration: bindingGeneration,
			Paths:             append([]string(nil), fixture.ResetPaths...),
		})
		cancel()
		requireClientStatus(t, staleErr, 409, "stale reset binding")
		verifyFileRemovedAfterCleanup(
			t, client, workspaceUID, reset.BindingGeneration, 1, fixture, timing,
		)
	} else if slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureFiles) {
		operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
		_, scrubErr := client.Scrub(operationCtx, workspaceagent.ScrubRequest{
			WorkspaceUID:      workspaceUID,
			BindingGeneration: bindingGeneration,
			Paths:             []string{fixture.FilePath},
		})
		cancel()
		if scrubErr != nil {
			t.Fatalf("scrub uploaded file: %v", scrubErr)
		}
		verifyFileRemovedAfterCleanup(
			t, client, workspaceUID, bindingGeneration, 2, fixture, timing,
		)
	}
}

func validateConformanceConnection(connection workspaceprovider.ConnectionData) error {
	if connection.AllowInsecure {
		return fmt.Errorf("conformance connection must not enable insecure transport")
	}
	parsed, err := url.Parse(strings.TrimSpace(connection.Endpoint))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("conformance connection endpoint must use HTTPS")
	}
	return nil
}

func validateDataPlaneCapabilities(
	t *testing.T,
	features []workspacev1alpha1.ExecutionWorkspaceFeature,
	capabilities []string,
) {
	t.Helper()
	required := []string{"attachment-fencing"}
	if slices.Contains(features, workspacev1alpha1.WorkspaceFeatureExec) {
		required = append(required, "exec", "exec-idempotency", "exec-cancel")
	}
	if slices.Contains(features, workspacev1alpha1.WorkspaceFeatureFiles) {
		required = append(required, "files")
	}
	if slices.Contains(features, workspacev1alpha1.WorkspaceFeatureReset) {
		required = append(required, "reset")
	}
	for _, capability := range required {
		if !slices.Contains(capabilities, capability) {
			t.Fatalf("workspace-agent capabilities %v missing %q", capabilities, capability)
		}
	}
}

func runExecConformance(
	t *testing.T,
	client *workspaceagent.Client,
	credentials workspaceagent.AttachmentCredentials,
	fixture *DataPlaneFixture,
	timing conformanceTiming,
) {
	t.Helper()
	if len(fixture.ExecCommand) == 0 || len(fixture.ExecVerifyCommand) == 0 {
		t.Fatal("data-plane exec and verification commands are required")
	}
	request := workspaceagent.ExecRequest{
		OperationID: "conformance-exec",
		Command:     append([]string(nil), fixture.ExecCommand...),
	}
	operationCtx, cancel := context.WithTimeout(context.Background(), timing.timeout)
	started, err := client.Exec(operationCtx, credentials, request)
	cancel()
	if err != nil {
		t.Fatalf("start conformance exec: %v", err)
	}
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	duplicate, err := client.Exec(operationCtx, credentials, request)
	cancel()
	if err != nil || duplicate.OperationID != started.OperationID {
		t.Fatalf("idempotent conformance exec = %#v, err=%v", duplicate, err)
	}
	result := waitForExecResult(t, client, credentials, request.OperationID, started, timing)
	if result.State != workspaceagent.OperationStateSucceeded || result.ExitCode != 0 || result.IsolationFailed {
		t.Fatalf("conformance exec result = %#v", result)
	}
	verifyRequest := workspaceagent.ExecRequest{
		OperationID: "conformance-exec-verify",
		Command:     append([]string(nil), fixture.ExecVerifyCommand...),
	}
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	verification, err := client.Exec(operationCtx, credentials, verifyRequest)
	cancel()
	if err != nil {
		t.Fatalf("start conformance exec verification: %v", err)
	}
	verification = waitForExecResult(
		t, client, credentials, verifyRequest.OperationID, verification, timing,
	)
	if verification.State != workspaceagent.OperationStateSucceeded ||
		verification.Stdout != fixture.ExecExpectedStdout {
		t.Fatalf("exactly-once verification result = %#v, want stdout %q", verification, fixture.ExecExpectedStdout)
	}
}

func waitForExecResult(
	t *testing.T,
	client *workspaceagent.Client,
	credentials workspaceagent.AttachmentCredentials,
	operationID string,
	result *workspaceagent.ExecResponse,
	timing conformanceTiming,
) *workspaceagent.ExecResponse {
	t.Helper()
	deadline := time.Now().Add(timing.timeout)
	var err error
	for result.Running {
		if time.Now().After(deadline) {
			t.Fatalf("conformance exec %q did not complete", operationID)
		}
		time.Sleep(timing.pollInterval)
		operationCtx, cancel := context.WithTimeout(context.Background(), timing.timeout)
		result, err = client.ExecStatus(operationCtx, credentials, operationID)
		cancel()
		if err != nil {
			t.Fatalf("poll conformance exec %q: %v", operationID, err)
		}
	}
	return result
}

func verifyRevokedCredentials(
	t *testing.T,
	client *workspaceagent.Client,
	credentials workspaceagent.AttachmentCredentials,
	fixture *DataPlaneFixture,
	timing conformanceTiming,
) {
	t.Helper()
	command := fixture.ExecVerifyCommand
	if len(command) == 0 {
		command = fixture.ExecCommand
	}
	operationCtx, cancel := context.WithTimeout(context.Background(), timing.timeout)
	_, err := client.Exec(operationCtx, credentials, workspaceagent.ExecRequest{
		OperationID: "conformance-revoked-credentials",
		Command:     append([]string(nil), command...),
	})
	cancel()
	requireClientStatus(t, err, 401, "revoked attachment credentials")
}

func verifyFileRemovedAfterCleanup(
	t *testing.T,
	client *workspaceagent.Client,
	workspaceUID string,
	bindingGeneration string,
	epoch int64,
	fixture *DataPlaneFixture,
	timing conformanceTiming,
) {
	t.Helper()
	tokenDigest := sha256.Sum256([]byte(fixture.Bearer))
	request := workspaceagent.AttachmentControlRequest{
		WorkspaceUID:      workspaceUID,
		BindingGeneration: bindingGeneration,
		TaskUID:           "conformance-cleanup-verification",
		Epoch:             epoch,
		ExpiresAt:         time.Now().Add(4 * timing.timeout),
	}
	request.SetTokenDigest(fmt.Sprintf("sha256:%x", tokenDigest))
	operationCtx, cancel := context.WithTimeout(context.Background(), timing.timeout)
	_, err := client.ActivateAttachment(operationCtx, request)
	cancel()
	if err != nil {
		t.Fatalf("activate cleanup verification attachment: %v", err)
	}
	credentials := workspaceagent.AttachmentCredentials{
		WorkspaceUID: workspaceUID,
		Epoch:        epoch,
		Bearer:       fixture.Bearer,
	}
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	_, downloadErr := client.Download(operationCtx, credentials, workspaceagent.DownloadRequest{
		Paths: []string{fixture.FilePath},
	})
	cancel()
	requireClientStatus(t, downloadErr, 404, "cleanup verification download")
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	_, listErr := client.Download(operationCtx, credentials, workspaceagent.DownloadRequest{})
	cancel()
	if listErr != nil {
		t.Fatalf("list workspace files after cleanup verification download: %v", listErr)
	}
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	_, revokeErr := client.RevokeAttachment(operationCtx, workspaceUID, bindingGeneration, epoch)
	cancel()
	if revokeErr != nil {
		t.Fatalf("revoke cleanup verification attachment: %v", revokeErr)
	}
}

func requireClientStatus(t *testing.T, err error, status int, operation string) {
	t.Helper()
	var clientErr *workspaceagent.Error
	if !errors.As(err, &clientErr) || clientErr.StatusCode != status {
		t.Fatalf("%s error = %v, want status %d", operation, err, status)
	}
}

func runFileConformance(
	t *testing.T,
	client *workspaceagent.Client,
	credentials workspaceagent.AttachmentCredentials,
	fixture *DataPlaneFixture,
	timing conformanceTiming,
) {
	t.Helper()
	if strings.TrimSpace(fixture.FilePath) == "" {
		t.Fatal("data-plane file path is required")
	}
	data := fixture.FileData
	if len(data) == 0 {
		data = []byte("workspace-provider-conformance")
	}
	operationCtx, cancel := context.WithTimeout(context.Background(), timing.timeout)
	_, err := client.Upload(operationCtx, credentials, workspaceagent.UploadRequest{
		Files: []workspaceagent.UploadFile{{Path: fixture.FilePath, Data: append([]byte(nil), data...)}},
	})
	cancel()
	if err != nil {
		t.Fatalf("upload conformance file: %v", err)
	}
	operationCtx, cancel = context.WithTimeout(context.Background(), timing.timeout)
	downloaded, err := client.Download(operationCtx, credentials, workspaceagent.DownloadRequest{
		Paths: []string{fixture.FilePath},
	})
	cancel()
	if err != nil || len(downloaded.Artifacts) != 1 || !bytes.Equal(downloaded.Artifacts[0].Data, data) {
		t.Fatalf("download conformance file = %#v, err=%v", downloaded, err)
	}
}

type interactiveProviderBindingConformance struct {
	provider   *workspacev1alpha1.ExecutionWorkspaceProvider
	metadata   workspaceprovider.AdapterMetadata
	advertised workspaceprovider.ProviderObservation
	stable     *workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus
}

func (c *interactiveProviderBindingConformance) validate(
	observation workspaceprovider.WorkspaceObservation,
) error {
	binding := observation.ProviderBinding
	if binding == nil {
		return fmt.Errorf("provider binding is required after the workspace reaches Ready")
	}
	if err := validateProviderBindingIdentity(binding, c.provider, c.metadata, c.advertised); err != nil {
		return err
	}
	if c.stable == nil {
		stable := *binding
		c.stable = &stable
		return nil
	}
	if *binding != *c.stable {
		return fmt.Errorf("provider binding changed from %#v to %#v", *c.stable, *binding)
	}
	return nil
}

func validateProviderBindingIdentity(
	binding *workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	metadata workspaceprovider.AdapterMetadata,
	advertised workspaceprovider.ProviderObservation,
) error {
	if binding == nil {
		return fmt.Errorf("provider binding is required")
	}
	if strings.TrimSpace(binding.ContractVersion) == "" {
		return fmt.Errorf("provider binding contract version is required")
	}
	if err := validateContractList("adapter metadata", metadata.Contracts); err != nil {
		return err
	}
	if !contains(metadata.Contracts, binding.ContractVersion) {
		return fmt.Errorf(
			"provider binding contract %q is not advertised by adapter metadata %v",
			binding.ContractVersion,
			metadata.Contracts,
		)
	}
	if provider != nil {
		if err := validateContractList("provider required contracts", provider.Spec.RequiredContracts); err != nil {
			return err
		}
		if err := validateContractList("provider supported contracts", provider.Status.SupportedContracts); err != nil {
			return err
		}
		if len(provider.Spec.RequiredContracts) > 0 &&
			!contains(provider.Spec.RequiredContracts, binding.ContractVersion) {
			return fmt.Errorf(
				"provider binding contract %q is not required by provider fixture %v",
				binding.ContractVersion,
				provider.Spec.RequiredContracts,
			)
		}
		if len(provider.Status.SupportedContracts) > 0 &&
			!contains(provider.Status.SupportedContracts, binding.ContractVersion) {
			return fmt.Errorf(
				"provider binding contract %q is not supported by provider fixture %v",
				binding.ContractVersion,
				provider.Status.SupportedContracts,
			)
		}
	}

	adapterVersions := []string{metadata.Version, advertised.Adapter.Version}
	adapterDigests := []string{metadata.Digest, advertised.Adapter.Digest}
	if provider != nil && provider.Status.Adapter != nil {
		adapterVersions = append(adapterVersions, provider.Status.Adapter.Version)
		adapterDigests = append(adapterDigests, provider.Status.Adapter.Digest)
	}
	expectedVersion, err := consistentAdvertisedValue("adapter version", adapterVersions...)
	if err != nil {
		return err
	}
	if binding.AdapterVersion != expectedVersion {
		return fmt.Errorf(
			"provider binding adapter version = %q, want advertised %q",
			binding.AdapterVersion,
			expectedVersion,
		)
	}
	expectedDigest, err := consistentAdvertisedValue("adapter digest", adapterDigests...)
	if err != nil {
		return err
	}
	if binding.AdapterDigest != expectedDigest {
		return fmt.Errorf(
			"provider binding adapter digest = %q, want advertised %q",
			binding.AdapterDigest,
			expectedDigest,
		)
	}

	backendAPIVersions := make([][]string, 0, 2)
	if len(advertised.Backend.APIVersions) > 0 {
		backendAPIVersions = append(backendAPIVersions, advertised.Backend.APIVersions)
	}
	if provider != nil && provider.Status.Backend != nil && len(provider.Status.Backend.APIVersions) > 0 {
		backendAPIVersions = append(backendAPIVersions, provider.Status.Backend.APIVersions)
	}
	if len(backendAPIVersions) == 0 {
		if binding.BackendAPIVersion != "" {
			return fmt.Errorf(
				"provider binding backend API version %q was not advertised",
				binding.BackendAPIVersion,
			)
		}
		return nil
	}
	if binding.BackendAPIVersion == "" {
		return fmt.Errorf("provider binding backend API version is required when the provider advertises one")
	}
	for _, advertisedVersions := range backendAPIVersions {
		if !contains(advertisedVersions, binding.BackendAPIVersion) {
			return fmt.Errorf(
				"provider binding backend API version %q is not advertised in %v",
				binding.BackendAPIVersion,
				advertisedVersions,
			)
		}
	}
	return nil
}

func consistentAdvertisedValue(kind string, values ...string) (string, error) {
	expected := ""
	for _, value := range values {
		if value == "" {
			continue
		}
		if expected == "" {
			expected = value
			continue
		}
		if value != expected {
			return "", fmt.Errorf("provider fixture advertises conflicting %s values %q and %q", kind, expected, value)
		}
	}
	return expected, nil
}

func runInteractiveLifecycle(
	t *testing.T,
	driver workspaceprovider.Driver,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	metadata workspaceprovider.AdapterMetadata,
	advertised workspaceprovider.ProviderObservation,
	timing conformanceTiming,
) {
	t.Helper()
	if workspace == nil {
		t.Fatal("interactive fixture is required when exec support is advertised")
	}
	ctx := context.Background()
	workspace = workspace.DeepCopy()
	markWorkspaceCoreAdmitted(workspace)
	bindingConformance := &interactiveProviderBindingConformance{
		provider: provider, metadata: metadata, advertised: advertised,
	}
	requireStableBinding := func(transition string, observation workspaceprovider.WorkspaceObservation) {
		t.Helper()
		if err := bindingConformance.validate(observation); err != nil {
			t.Fatalf("%s provider binding: %v; observation = %#v", transition, err, observation)
		}
	}
	withStableBinding := func(
		transition string,
		complete func(workspaceprovider.WorkspaceObservation) bool,
	) func(workspaceprovider.WorkspaceObservation) bool {
		return func(observation workspaceprovider.WorkspaceObservation) bool {
			requireStableBinding(transition, observation)
			return complete(observation)
		}
	}

	first := reconcileWorkspaceUntil(t, ctx, driver, workspace, "initial provisioning", timing, func(
		observation workspaceprovider.WorkspaceObservation,
	) bool {
		if observation.State != workspacev1alpha1.ExecutionWorkspaceStateReady {
			return false
		}
		requireStableBinding("initial provisioning", observation)
		return observation.ExternalID != ""
	})
	second := reconcileWorkspaceOnce(t, ctx, driver, workspace, "idempotency", timing)
	requireStableBinding("idempotency", second)
	if first.ExternalID == "" || first.ExternalID != second.ExternalID {
		t.Fatalf("idempotent external IDs = %q and %q", first.ExternalID, second.ExternalID)
	}

	workspace.Spec.Attachment = conformanceAttachment()
	advanceWorkspaceGenerationAndReadmit(t, driver, workspace, "attachment activation", timing)
	attachmentController, controlsAttachment := driver.(workspaceprovider.AttachmentController)
	if controlsAttachment {
		operationCtx, cancel := context.WithTimeout(ctx, timing.timeout)
		err := attachmentController.ActivateAttachment(operationCtx, workspace.DeepCopy())
		cancel()
		if err != nil {
			t.Fatalf("ActivateAttachment: %v", err)
		}
	}
	attached := reconcileWorkspaceUntil(
		t, ctx, driver, workspace, "attachment activation", timing,
		withStableBinding("attachment activation", func(observation workspaceprovider.WorkspaceObservation) bool {
			return observation.State == workspacev1alpha1.ExecutionWorkspaceStateAttached &&
				observation.AttachedEpoch == workspace.Spec.Attachment.Epoch
		}),
	)
	if attached.State != workspacev1alpha1.ExecutionWorkspaceStateAttached || attached.AttachedEpoch != 1 {
		t.Fatalf("attached observation = %#v", attached)
	}

	detachedEpoch := workspace.Spec.Attachment.Epoch
	workspace.Spec.Attachment = nil
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	workspace.Generation++
	if controlsAttachment {
		operationCtx, cancel := context.WithTimeout(ctx, timing.timeout)
		err := attachmentController.RevokeAttachment(operationCtx, workspace.DeepCopy(), detachedEpoch)
		cancel()
		if err != nil {
			t.Fatalf("RevokeAttachment: %v", err)
		}
	}
	reconcileWorkspaceUntil(
		t, ctx, driver, workspace, "attachment revocation", timing,
		withStableBinding("attachment revocation", func(observation workspaceprovider.WorkspaceObservation) bool {
			return observation.State == workspacev1alpha1.ExecutionWorkspaceStateReady &&
				observation.AttachedEpoch == 0
		}),
	)
	// Core refreshes admission only after fail-closed revocation is observed.
	workspace.Generation++
	markWorkspaceCoreAdmitted(workspace)
	suspendAllowed := slices.Contains(
		workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	if suspendAllowed && slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureSuspend) {
		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		advanceWorkspaceGenerationAndReadmit(t, driver, workspace, "suspension", timing)
		reconcileWorkspaceUntil(
			t, ctx, driver, workspace, "suspension", timing,
			withStableBinding("suspension", func(observation workspaceprovider.WorkspaceObservation) bool {
				return observation.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended &&
					observation.AttachedEpoch == 0
			}),
		)

		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
		advanceWorkspaceGenerationAndReadmit(t, driver, workspace, "resume", timing)
		reconcileWorkspaceUntil(
			t, ctx, driver, workspace, "resume", timing,
			withStableBinding("resume", func(observation workspaceprovider.WorkspaceObservation) bool {
				return observation.State == workspacev1alpha1.ExecutionWorkspaceStateReady &&
					observation.AttachedEpoch == 0
			}),
		)
	}

	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	workspace.Generation++
	deleted := reconcileWorkspaceUntil(
		t, ctx, driver, workspace, "deletion", timing,
		withStableBinding("deletion", func(observation workspaceprovider.WorkspaceObservation) bool {
			return observation.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted
		}),
	)
	if !validInteractiveDeletedDisposition(deleted.Disposition, workspace.Spec.Lifecycle.DeletionPolicy) {
		t.Fatalf("deleted observation = %#v", deleted)
	}
}

func conformanceTimingFor(fixtures Fixtures) conformanceTiming {
	timeout := fixtures.TransitionTimeout
	if timeout <= 0 {
		timeout = workspaceTransitionTimeout
	}
	pollInterval := fixtures.PollInterval
	if pollInterval <= 0 {
		pollInterval = workspaceTransitionPollInterval
	}
	return conformanceTiming{timeout: timeout, pollInterval: pollInterval}
}

func reconcileWorkspaceOnce(
	t *testing.T,
	ctx context.Context,
	driver workspaceprovider.Driver,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	operation string,
	timing conformanceTiming,
) workspaceprovider.WorkspaceObservation {
	t.Helper()
	operationCtx, cancel := context.WithTimeout(ctx, timing.timeout)
	defer cancel()
	observation, err := driver.ReconcileWorkspace(operationCtx, workspace.DeepCopy())
	if err != nil {
		t.Fatalf("%s ReconcileWorkspace: %v", operation, err)
	}
	return observation
}

func reconcileWorkspaceUntil(
	t *testing.T,
	ctx context.Context,
	driver workspaceprovider.Driver,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	transition string,
	timing conformanceTiming,
	complete func(workspaceprovider.WorkspaceObservation) bool,
) workspaceprovider.WorkspaceObservation {
	t.Helper()
	transitionCtx, cancel := context.WithTimeout(ctx, timing.timeout)
	defer cancel()
	ticker := time.NewTicker(timing.pollInterval)
	defer ticker.Stop()
	var last workspaceprovider.WorkspaceObservation
	for {
		observation, err := driver.ReconcileWorkspace(transitionCtx, workspace.DeepCopy())
		if err != nil {
			t.Fatalf("%s ReconcileWorkspace: %v", transition, err)
		}
		last = observation
		if complete(observation) {
			return observation
		}
		select {
		case <-transitionCtx.Done():
			t.Fatalf("%s did not complete: %v; last observation = %#v", transition, transitionCtx.Err(), last)
			return last
		case <-ticker.C:
		}
	}
}

func runServiceEndpoint(
	t *testing.T,
	driver workspaceprovider.Driver,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	metadata workspaceprovider.AdapterMetadata,
	advertised workspaceprovider.ProviderObservation,
	timing conformanceTiming,
) {
	t.Helper()
	if workspace == nil {
		t.Fatal("service fixture is required when service endpoint support is advertised")
	}
	workspace = workspace.DeepCopy()
	markWorkspaceCoreAdmitted(workspace)
	observation := reconcileWorkspaceUntil(
		t,
		context.Background(),
		driver,
		workspace,
		"service provisioning",
		timing,
		func(observation workspaceprovider.WorkspaceObservation) bool {
			return observation.State == workspacev1alpha1.ExecutionWorkspaceStateReady &&
				len(observation.Endpoints) > 0
		},
	)
	if observation.ExternalID == "" {
		t.Fatalf("service observation = %#v", observation)
	}
	bindingValidator := &interactiveProviderBindingConformance{
		provider: provider, metadata: metadata, advertised: advertised,
	}
	if err := bindingValidator.validate(observation); err != nil {
		t.Fatalf("service provider binding: %v", err)
	}
	second := reconcileWorkspaceOnce(t, context.Background(), driver, workspace, "service idempotency", timing)
	if err := bindingValidator.validate(second); err != nil {
		t.Fatalf("service idempotent provider binding: %v", err)
	}
	if second.State != workspacev1alpha1.ExecutionWorkspaceStateReady || second.ExternalID != observation.ExternalID {
		t.Fatalf("service idempotent observation changed: first=%#v second=%#v", observation, second)
	}
	if err := workspaceprovider.ValidateEndpoints(second.Endpoints); err != nil {
		t.Fatalf("service idempotent endpoints: %v", err)
	}
	if err := validateServiceEndpointCorrespondence(workspace, second.Endpoints); err != nil {
		t.Fatalf("service idempotent endpoints: %v", err)
	}
	if err := workspaceprovider.ValidateEndpoints(observation.Endpoints); err != nil {
		t.Fatalf("service endpoints: %v", err)
	}
	if err := validateServiceEndpointCorrespondence(workspace, observation.Endpoints); err != nil {
		t.Fatalf("service endpoints: %v", err)
	}
}

func validateServiceEndpointCorrespondence(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	endpoints []workspacev1alpha1.ExecutionWorkspaceEndpoint,
) error {
	if workspace.Spec.Service == nil {
		return fmt.Errorf("service fixture is missing requested ports")
	}
	if len(endpoints) != len(workspace.Spec.Service.Ports) {
		return fmt.Errorf("endpoint count = %d, want %d", len(endpoints), len(workspace.Spec.Service.Ports))
	}
	byName := make(map[string]workspacev1alpha1.ExecutionWorkspaceEndpoint, len(endpoints))
	for _, endpoint := range endpoints {
		if _, exists := byName[endpoint.Name]; exists {
			return fmt.Errorf("duplicate endpoint name %q", endpoint.Name)
		}
		byName[endpoint.Name] = endpoint
	}
	for _, port := range workspace.Spec.Service.Ports {
		endpoint, ok := byName[port.Name]
		if !ok {
			return fmt.Errorf("requested service port %q has no endpoint", port.Name)
		}
		if endpoint.Protocol != port.Protocol {
			return fmt.Errorf("endpoint %q protocol = %q, want %q", port.Name, endpoint.Protocol, port.Protocol)
		}
		parsed, err := url.Parse(endpoint.URL)
		if err != nil || !strings.EqualFold(parsed.Scheme, port.Protocol) {
			return fmt.Errorf("endpoint %q URL scheme does not match protocol %q", port.Name, port.Protocol)
		}
	}
	return nil
}

func conformanceAttachment() *workspacev1alpha1.ExecutionWorkspaceAttachment {
	attachment := &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef:     workspacev1alpha1.ObjectIdentityReference{Name: "conformance-task", UID: "conformance-task-uid"},
		Epoch:       1,
		TokenSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ExpiresAt:   metav1.NewTime(time.Now().Add(time.Minute)),
	}
	attachment.TokenSecretRef.Name = "conformance-attachment"
	return attachment
}

func validDeletedDisposition(
	disposition *workspacev1alpha1.ExecutionWorkspaceDisposition,
	policy workspacev1alpha1.ExecutionWorkspaceDeletionPolicy,
) bool {
	return workspaceprovider.ValidateDeletedDisposition(disposition, policy) == nil
}

func validInteractiveDeletedDisposition(
	disposition *workspacev1alpha1.ExecutionWorkspaceDisposition,
	policy workspacev1alpha1.ExecutionWorkspaceDeletionPolicy,
) bool {
	return workspaceprovider.ValidateInteractiveDeletedDisposition(disposition, policy) == nil
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
