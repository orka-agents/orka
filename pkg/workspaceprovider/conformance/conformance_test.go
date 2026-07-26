package conformance

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceagent"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

type attachmentLifecycleDriver struct {
	active            bool
	everAttached      bool
	initialized       bool
	suspended         bool
	initialPolls      int
	servicePolls      int
	detachPolls       int
	suspendPolls      int
	resumePolls       int
	deletePolls       int
	activateCalls     int
	revocationCalls   int
	revocationPending bool
	missingDeadline   bool
	metadata          *workspaceprovider.AdapterMetadata
	connectionRef     *workspacev1alpha1.SecretReference
}

func (d *attachmentLifecycleDriver) Metadata() workspaceprovider.AdapterMetadata {
	if d.metadata != nil {
		return *d.metadata
	}
	return workspaceprovider.AdapterMetadata{
		ControllerName: "conformance.test/v1",
		Version:        "1.0.0",
		Contracts:      []string{workspacev1alpha1.ContractVersionV1},
		Features:       []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureSuspend},
	}
}

func (d *attachmentLifecycleDriver) ObserveProvider(
	context.Context,
	*workspacev1alpha1.ExecutionWorkspaceProvider,
) (workspaceprovider.ProviderObservation, error) {
	return d.providerObservation(), nil
}

func (d *attachmentLifecycleDriver) providerObservation() workspaceprovider.ProviderObservation {
	metadata := d.Metadata()
	return workspaceprovider.ProviderObservation{
		Adapter: workspacev1alpha1.ExecutionWorkspaceAdapterStatus{
			Version: metadata.Version,
			Digest:  metadata.Digest,
		},
		Backend: workspacev1alpha1.ExecutionWorkspaceBackendStatus{
			Version:     "2026.07",
			APIVersions: []string{"conformance.test/v1"},
		},
		SupportedFeatures: append([]workspacev1alpha1.ExecutionWorkspaceFeature(nil), metadata.Features...),
	}
}

func (d *attachmentLifecycleDriver) providerBinding() *workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus {
	metadata := d.Metadata()
	return &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
		ContractVersion:   metadata.Contracts[0],
		AdapterVersion:    metadata.Version,
		AdapterDigest:     metadata.Digest,
		BackendAPIVersion: d.providerObservation().Backend.APIVersions[0],
	}
}

func (d *attachmentLifecycleDriver) withProviderBinding(
	observation workspaceprovider.WorkspaceObservation,
) workspaceprovider.WorkspaceObservation {
	observation.ProviderBinding = d.providerBinding()
	return observation
}

func (d *attachmentLifecycleDriver) ReconcilePool(
	context.Context,
	*workspacev1alpha1.ExecutionWorkspacePool,
) (workspaceprovider.PoolObservation, error) {
	return workspaceprovider.PoolObservation{}, nil
}

func (d *attachmentLifecycleDriver) ReconcileWorkspace(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (workspaceprovider.WorkspaceObservation, error) {
	if _, ok := ctx.Deadline(); !ok {
		d.missingDeadline = true
	}
	lifecycleMaintenance := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	if lifecycleMaintenance && !workspaceCurrentlyCoreAdmitted(workspace) {
		state := workspacev1alpha1.ExecutionWorkspaceStateQuarantined
		observation := workspaceprovider.WorkspaceObservation{State: state}
		if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
			observation.State = workspacev1alpha1.ExecutionWorkspaceStateDeleted
			observation.Disposition = validConformanceDisposition()
		}
		return d.withProviderBinding(observation), nil
	}
	if !lifecycleMaintenance && !d.revocationPending && !workspaceCurrentlyCoreAdmitted(workspace) {
		return workspaceprovider.WorkspaceObservation{}, nil
	}
	if workspace.Spec.Mode == workspacev1alpha1.ExecutionWorkspaceModeService {
		if d.servicePolls == 0 {
			d.servicePolls++
			return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateProvisioning,
				ExternalID: "service-1",
			}), nil
		}
		return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
			State:      workspacev1alpha1.ExecutionWorkspaceStateReady,
			ExternalID: "service-1",
			Endpoints: []workspacev1alpha1.ExecutionWorkspaceEndpoint{{
				Name: "mcp", URL: "https://service.example/mcp", Protocol: "HTTPS",
			}},
		}), nil
	}
	if !d.initialized {
		if d.initialPolls == 0 {
			d.initialPolls++
			return workspaceprovider.WorkspaceObservation{
				State: workspacev1alpha1.ExecutionWorkspaceStateProvisioning,
			}, nil
		}
		d.initialized = true
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		if d.deletePolls == 0 {
			d.deletePolls++
			return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateDeleting,
				ExternalID: "workspace-1",
			}), nil
		}
		return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
			State:       workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			ExternalID:  "workspace-1",
			Disposition: validConformanceDisposition(),
		}), nil
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		if d.suspendPolls == 0 {
			d.suspendPolls++
			return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateSuspending,
				ExternalID: "workspace-1",
			}), nil
		}
		d.suspended = true
		return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
			State:      workspacev1alpha1.ExecutionWorkspaceStateSuspended,
			ExternalID: "workspace-1",
		}), nil
	}
	if workspace.Spec.Attachment != nil {
		if !d.active {
			return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateAttaching,
				ExternalID: "workspace-1",
			}), nil
		}
		return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
			State:         workspacev1alpha1.ExecutionWorkspaceStateAttached,
			ExternalID:    "workspace-1",
			AttachedEpoch: workspace.Spec.Attachment.Epoch,
		}), nil
	}
	if d.everAttached && d.detachPolls == 0 {
		d.detachPolls++
		return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
			State:         workspacev1alpha1.ExecutionWorkspaceStateDetaching,
			ExternalID:    "workspace-1",
			AttachedEpoch: 1,
		}), nil
	}
	if d.suspended {
		if d.resumePolls == 0 {
			d.resumePolls++
			return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateProvisioning,
				ExternalID: "workspace-1",
			}), nil
		}
		d.suspended = false
	}
	if d.revocationPending && d.detachPolls > 0 {
		d.revocationPending = false
	}
	return d.withProviderBinding(workspaceprovider.WorkspaceObservation{
		State:         workspacev1alpha1.ExecutionWorkspaceStateReady,
		ExternalID:    "workspace-1",
		ConnectionRef: d.connectionRef,
	}), nil
}

func (d *attachmentLifecycleDriver) ActivateAttachment(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	if _, ok := ctx.Deadline(); !ok {
		d.missingDeadline = true
	}
	d.activateCalls++
	d.active = true
	d.everAttached = workspace.Spec.Attachment != nil
	return nil
}

func (d *attachmentLifecycleDriver) RevokeAttachment(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	epoch int64,
) error {
	if _, ok := ctx.Deadline(); !ok {
		d.missingDeadline = true
	}
	d.revocationCalls++
	d.revocationPending = true
	if workspace.Spec.Attachment != nil || epoch != 1 {
		return context.Canceled
	}
	d.active = false
	return nil
}

func conformanceProviderFixture(
	metadata workspaceprovider.AdapterMetadata,
	advertised workspaceprovider.ProviderObservation,
) *workspacev1alpha1.ExecutionWorkspaceProvider {
	adapter := advertised.Adapter
	backend := advertised.Backend
	backend.APIVersions = append([]string(nil), advertised.Backend.APIVersions...)
	return &workspacev1alpha1.ExecutionWorkspaceProvider{
		Spec: workspacev1alpha1.ExecutionWorkspaceProviderSpec{
			ControllerName:    metadata.ControllerName,
			RequiredContracts: append([]string(nil), metadata.Contracts...),
		},
		Status: workspacev1alpha1.ExecutionWorkspaceProviderStatus{
			Adapter:            &adapter,
			Backend:            &backend,
			SupportedContracts: append([]string(nil), metadata.Contracts...),
		},
	}
}

func TestInteractiveLifecycleExercisesAttachmentControllerAndWaitsForDetach(t *testing.T) {
	driver := &attachmentLifecycleDriver{}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	workspace.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
		workspacev1alpha1.WorkspaceOnDetachSuspend,
	}
	workspace.Spec.Lifecycle.DeletionPolicy = conformanceDeletionPolicy()

	timing := conformanceTiming{timeout: time.Second, pollInterval: time.Millisecond}
	metadata := driver.Metadata()
	advertised := driver.providerObservation()
	runInteractiveLifecycle(
		t, driver, conformanceProviderFixture(metadata, advertised), workspace, metadata, advertised, timing,
	)
	service := &workspacev1alpha1.ExecutionWorkspace{}
	service.Spec.Mode = workspacev1alpha1.ExecutionWorkspaceModeService
	service.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	service.Spec.Service = &workspacev1alpha1.ExecutionWorkspaceServiceSpec{
		Ports: []workspacev1alpha1.ExecutionWorkspaceServicePort{{
			Name: "mcp", Port: 8080, Protocol: "HTTPS",
		}},
	}
	runServiceEndpoint(
		t, driver, conformanceProviderFixture(metadata, advertised), service, metadata, advertised, timing,
	)

	if driver.missingDeadline {
		t.Fatal("conformance driver call did not receive a deadline-bearing context")
	}
	if driver.activateCalls != 1 || driver.revocationCalls != 1 {
		t.Fatalf(
			"attachment controller calls = activate %d revoke %d, want 1 each",
			driver.activateCalls,
			driver.revocationCalls,
		)
	}
	if driver.initialPolls != 1 || driver.servicePolls != 1 || driver.detachPolls != 1 ||
		driver.suspendPolls != 1 || driver.resumePolls != 1 || driver.deletePolls != 0 {
		t.Fatalf(
			"transition polls = initial %d service %d detach %d suspend %d resume %d delete %d, "+
				"want one poll for non-delete transitions and zero for immediate maintenance deletion",
			driver.initialPolls,
			driver.servicePolls,
			driver.detachPolls,
			driver.suspendPolls,
			driver.resumePolls,
			driver.deletePolls,
		)
	}
}

func TestValidateWorkspaceAdmissionProbeResultRejectsProgressWithAdmissionError(t *testing.T) {
	observation := workspaceprovider.WorkspaceObservation{
		State:      workspacev1alpha1.ExecutionWorkspaceStateReady,
		ExternalID: "already-created",
	}
	if err := validateWorkspaceAdmissionProbeResult(
		observation, workspaceprovider.ErrWorkspaceNotAdmitted, "admission probe",
	); err == nil {
		t.Fatal("progressed observation with ErrWorkspaceNotAdmitted passed admission validation")
	}
}

func TestValidDeletedDispositionRequiresTerminalPolicyCompliantState(t *testing.T) {
	policy := conformanceDeletionPolicy()
	if validDeletedDisposition(&workspacev1alpha1.ExecutionWorkspaceDisposition{}, policy) {
		t.Fatal("empty disposition passed conformance")
	}
	if !validDeletedDisposition(validConformanceDisposition(), policy) {
		t.Fatal("terminal policy-compliant disposition failed conformance")
	}
	invalid := validConformanceDisposition()
	invalid.Compute = workspacev1alpha1.DispositionActive
	if validDeletedDisposition(invalid, policy) {
		t.Fatal("active compute disposition passed deletion conformance")
	}
	invalid = validConformanceDisposition()
	invalid.PersistentVolumes = workspacev1alpha1.DispositionDeleted
	if validDeletedDisposition(invalid, policy) {
		t.Fatal("retained-volume policy mismatch passed deletion conformance")
	}

	notApplicable := validConformanceDisposition()
	notApplicable.AccessCredentials = workspacev1alpha1.DispositionNotApplicable
	if !validDeletedDisposition(notApplicable, policy) {
		t.Fatal("generic deletion disposition rejected not-applicable credentials")
	}
	if validInteractiveDeletedDisposition(notApplicable, policy) {
		t.Fatal("interactive deletion accepted not-applicable attachment credentials")
	}
}

func TestValidatePoolObservationRejectsImpossibleCounters(t *testing.T) {
	tests := []struct {
		name        string
		observation workspaceprovider.PoolObservation
		wantError   bool
	}{
		{name: "empty", observation: workspaceprovider.PoolObservation{}},
		{
			name: "available allocated and suspended",
			observation: workspaceprovider.PoolObservation{
				Available: 1,
				Allocated: 1,
				Suspended: 1,
				Total:     3,
			},
		},
		{
			name: "documented disjoint capacity example",
			observation: workspaceprovider.PoolObservation{
				Available: 3, Allocated: 8, Suspended: 20, Total: 31,
			},
		},
		{
			name: "disjoint capacity sum exceeds total",
			observation: workspaceprovider.PoolObservation{
				Available: 3, Allocated: 8, Suspended: 20, Total: 30,
			},
			wantError: true,
		},
		{name: "negative", observation: workspaceprovider.PoolObservation{Available: -1}, wantError: true},
		{
			name:        "available exceeds total",
			observation: workspaceprovider.PoolObservation{Available: 2, Total: 1},
			wantError:   true,
		},
		{
			name:        "allocated exceeds total",
			observation: workspaceprovider.PoolObservation{Allocated: 2, Total: 1},
			wantError:   true,
		},
		{
			name:        "available plus allocated exceeds total",
			observation: workspaceprovider.PoolObservation{Available: 2, Allocated: 2, Total: 3},
			wantError:   true,
		},
		{
			name: "widened counter sum exceeds total without overflow",
			observation: workspaceprovider.PoolObservation{
				Available: 1<<31 - 1,
				Allocated: 1<<31 - 1,
				Suspended: 1<<31 - 1,
				Total:     1<<31 - 1,
			},
			wantError: true,
		},
		{
			name:        "suspended exceeds total",
			observation: workspaceprovider.PoolObservation{Suspended: 2, Total: 1},
			wantError:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePoolObservation(tt.observation)
			if tt.wantError && err == nil {
				t.Fatalf("validatePoolObservation(%#v) succeeded, want error", tt.observation)
			}
			if !tt.wantError && err != nil {
				t.Fatalf("validatePoolObservation(%#v) error = %v", tt.observation, err)
			}
		})
	}
}

func providerBindingConformanceFixture() (
	workspaceprovider.AdapterMetadata,
	workspaceprovider.ProviderObservation,
	*workspacev1alpha1.ExecutionWorkspaceProvider,
	workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus,
) {
	metadata := workspaceprovider.AdapterMetadata{
		ControllerName: "conformance.test/v1",
		Version:        "1.2.3",
		Digest:         "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Contracts:      []string{workspacev1alpha1.ContractVersionV1, "workspace.orka.ai/v2"},
	}
	advertised := workspaceprovider.ProviderObservation{
		Adapter: workspacev1alpha1.ExecutionWorkspaceAdapterStatus{
			Version: metadata.Version,
			Digest:  metadata.Digest,
		},
		Backend: workspacev1alpha1.ExecutionWorkspaceBackendStatus{
			Version:     "2026.07",
			APIVersions: []string{"backend.test/v1", "backend.test/v2"},
		},
	}
	provider := conformanceProviderFixture(metadata, advertised)
	binding := workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
		ContractVersion:   workspacev1alpha1.ContractVersionV1,
		AdapterVersion:    metadata.Version,
		AdapterDigest:     metadata.Digest,
		BackendAPIVersion: "backend.test/v1",
	}
	return metadata, advertised, provider, binding
}

func TestValidateProviderBindingIdentityMatchesProviderFixtureAndAdvertisedMetadata(t *testing.T) {
	metadata, advertised, provider, valid := providerBindingConformanceFixture()
	tests := []struct {
		name      string
		binding   *workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus
		wantError bool
	}{
		{name: "valid", binding: &valid},
		{name: "nil", binding: nil, wantError: true},
		{
			name: "empty selected contract",
			binding: &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
				AdapterVersion:    valid.AdapterVersion,
				AdapterDigest:     valid.AdapterDigest,
				BackendAPIVersion: valid.BackendAPIVersion,
			},
			wantError: true,
		},
		{
			name: "contract mismatch",
			binding: &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
				ContractVersion:   "workspace.orka.ai/v3",
				AdapterVersion:    valid.AdapterVersion,
				AdapterDigest:     valid.AdapterDigest,
				BackendAPIVersion: valid.BackendAPIVersion,
			},
			wantError: true,
		},
		{
			name: "adapter version mismatch",
			binding: &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
				ContractVersion:   valid.ContractVersion,
				AdapterVersion:    "9.9.9",
				AdapterDigest:     valid.AdapterDigest,
				BackendAPIVersion: valid.BackendAPIVersion,
			},
			wantError: true,
		},
		{
			name: "adapter digest mismatch",
			binding: &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
				ContractVersion:   valid.ContractVersion,
				AdapterVersion:    valid.AdapterVersion,
				AdapterDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				BackendAPIVersion: valid.BackendAPIVersion,
			},
			wantError: true,
		},
		{
			name: "backend API mismatch",
			binding: &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
				ContractVersion:   valid.ContractVersion,
				AdapterVersion:    valid.AdapterVersion,
				AdapterDigest:     valid.AdapterDigest,
				BackendAPIVersion: "backend.test/v3",
			},
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateProviderBindingIdentity(tt.binding, provider, metadata, advertised)
			if tt.wantError && err == nil {
				t.Fatalf("validateProviderBindingIdentity(%#v) succeeded, want error", tt.binding)
			}
			if !tt.wantError && err != nil {
				t.Fatalf("validateProviderBindingIdentity(%#v) error = %v", tt.binding, err)
			}
		})
	}
}

func TestInteractiveProviderBindingConformanceRequiresStableBinding(t *testing.T) {
	metadata, advertised, provider, stable := providerBindingConformanceFixture()
	alternate := stable
	alternate.ContractVersion = "workspace.orka.ai/v2"
	alternate.BackendAPIVersion = "backend.test/v2"

	states := []workspacev1alpha1.ExecutionWorkspaceState{
		workspacev1alpha1.ExecutionWorkspaceStateReady,
		workspacev1alpha1.ExecutionWorkspaceStateAttaching,
		workspacev1alpha1.ExecutionWorkspaceStateAttached,
		workspacev1alpha1.ExecutionWorkspaceStateDetaching,
		workspacev1alpha1.ExecutionWorkspaceStateReady,
		workspacev1alpha1.ExecutionWorkspaceStateSuspending,
		workspacev1alpha1.ExecutionWorkspaceStateSuspended,
		workspacev1alpha1.ExecutionWorkspaceStateProvisioning,
		workspacev1alpha1.ExecutionWorkspaceStateReady,
		workspacev1alpha1.ExecutionWorkspaceStateDeleting,
		workspacev1alpha1.ExecutionWorkspaceStateDeleted,
	}
	stableObservations := make([]workspaceprovider.WorkspaceObservation, 0, len(states))
	for _, state := range states {
		binding := stable
		stableObservations = append(stableObservations, workspaceprovider.WorkspaceObservation{
			State:           state,
			ProviderBinding: &binding,
		})
	}

	tests := []struct {
		name         string
		observations []workspaceprovider.WorkspaceObservation
		wantErrorAt  int
	}{
		{name: "stable through lifecycle", observations: stableObservations, wantErrorAt: -1},
		{
			name:         "nil at ready",
			observations: []workspaceprovider.WorkspaceObservation{{State: workspacev1alpha1.ExecutionWorkspaceStateReady}},
			wantErrorAt:  0,
		},
		{
			name: "nil after ready",
			observations: []workspaceprovider.WorkspaceObservation{
				{State: workspacev1alpha1.ExecutionWorkspaceStateReady, ProviderBinding: &stable},
				{State: workspacev1alpha1.ExecutionWorkspaceStateAttaching},
			},
			wantErrorAt: 1,
		},
		{
			name: "unstable but independently valid binding",
			observations: []workspaceprovider.WorkspaceObservation{
				{State: workspacev1alpha1.ExecutionWorkspaceStateReady, ProviderBinding: &stable},
				{State: workspacev1alpha1.ExecutionWorkspaceStateAttaching, ProviderBinding: &alternate},
			},
			wantErrorAt: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := &interactiveProviderBindingConformance{
				provider: provider, metadata: metadata, advertised: advertised,
			}
			for index, observation := range tt.observations {
				err := validator.validate(observation)
				if index == tt.wantErrorAt {
					if err == nil {
						t.Fatalf("observation %d (%s) passed, want error", index, observation.State)
					}
					return
				}
				if err != nil {
					t.Fatalf("observation %d (%s) error = %v", index, observation.State, err)
				}
			}
			if tt.wantErrorAt >= 0 {
				t.Fatalf("observations completed, want error at index %d", tt.wantErrorAt)
			}
		})
	}
}

func validConformanceDisposition() *workspacev1alpha1.ExecutionWorkspaceDisposition {
	const payload = `{
		"compute":"Deleted",
		"accessCredentials":"Revoked",
		"ephemeralSecrets":"Deleted",
		"workspaceData":"Deleted",
		"persistentVolumes":"Retained",
		"checkpoints":"Deleted",
		"providerResources":"Deleted"
	}`
	disposition := &workspacev1alpha1.ExecutionWorkspaceDisposition{}
	if err := json.Unmarshal([]byte(payload), disposition); err != nil {
		panic(err)
	}
	return disposition
}

func conformanceDeletionPolicy() workspacev1alpha1.ExecutionWorkspaceDeletionPolicy {
	return workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
		ProviderResources: workspacev1alpha1.WorkspaceDeletionActionDelete,
		PersistentVolumes: workspacev1alpha1.WorkspaceDeletionActionRetain,
		Checkpoints:       workspacev1alpha1.WorkspaceDeletionActionDelete,
	}
}

func TestValidateFixturesRequiresAdvertisedCapabilities(t *testing.T) {
	metadata := workspaceprovider.AdapterMetadata{Features: []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeaturePools,
		workspacev1alpha1.WorkspaceFeatureServicePorts,
	}}
	fixtures := Fixtures{}
	if err := validateFixtures(metadata, fixtures); err == nil {
		t.Fatal("missing provider fixture passed validation")
	}
	fixtures.Provider = &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := validateFixtures(metadata, fixtures); err == nil {
		t.Fatal("missing advertised capability fixtures passed validation")
	}
	fixtures.Pool = &workspacev1alpha1.ExecutionWorkspacePool{}
	fixtures.Interactive = &workspacev1alpha1.ExecutionWorkspace{}
	fixtures.Service = &workspacev1alpha1.ExecutionWorkspace{}
	fixtures.DataPlane = &DataPlaneFixture{}
	if err := validateFixtures(metadata, fixtures); err != nil {
		t.Fatalf("complete fixtures failed validation: %v", err)
	}
}

func TestProviderObservationMatchesAdvertisedMetadata(t *testing.T) {
	metadata := workspaceprovider.AdapterMetadata{
		ControllerName: "test.workspace.orka.ai/v1",
		Version:        "1.2.3",
		Digest:         "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Contracts:      []string{workspacev1alpha1.ContractVersionV1},
		Features: []workspacev1alpha1.ExecutionWorkspaceFeature{
			workspacev1alpha1.WorkspaceFeatureTLS,
			workspacev1alpha1.WorkspaceFeatureExec,
		},
	}
	if err := validateAdapterMetadata(metadata); err != nil {
		t.Fatalf("valid metadata: %v", err)
	}
	invalidContracts := metadata
	invalidContracts.Contracts = append(append([]string(nil), metadata.Contracts...), "")
	if err := validateAdapterMetadata(invalidContracts); err == nil {
		t.Fatal("metadata with an empty contract passed validation")
	}
	observation := workspaceprovider.ProviderObservation{
		Adapter: workspacev1alpha1.ExecutionWorkspaceAdapterStatus{
			Version: metadata.Version,
			Digest:  metadata.Digest,
		},
		SupportedFeatures: append([]workspacev1alpha1.ExecutionWorkspaceFeature(nil), metadata.Features...),
	}
	if err := validateProviderObservation(metadata, observation); err != nil {
		t.Fatalf("matching observation: %v", err)
	}
	observation.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	if err := validateProviderObservation(metadata, observation); err == nil {
		t.Fatal("observation missing advertised exec feature passed validation")
	}
	observation.SupportedFeatures = append(
		append([]workspacev1alpha1.ExecutionWorkspaceFeature(nil), metadata.Features...),
		workspacev1alpha1.WorkspaceFeatureReset,
	)
	if err := validateProviderObservation(metadata, observation); err == nil {
		t.Fatal("observation with unadvertised reset feature passed validation")
	}
	metadata.Features = []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureExec}
	if err := validateAdapterMetadata(metadata); err == nil {
		t.Fatal("metadata without TLS passed validation")
	}
}

func TestValidateServiceEndpointCorrespondence(t *testing.T) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	workspace.Spec.Service = &workspacev1alpha1.ExecutionWorkspaceServiceSpec{
		Ports: []workspacev1alpha1.ExecutionWorkspaceServicePort{{
			Name: "mcp", Port: 8080, Protocol: "HTTPS",
		}},
	}
	valid := []workspacev1alpha1.ExecutionWorkspaceEndpoint{{
		Name: "mcp", URL: "https://service.example/mcp", Protocol: "HTTPS",
	}}
	if err := workspaceprovider.ValidateEndpoints(valid); err != nil {
		t.Fatalf("valid endpoint URL: %v", err)
	}
	if err := validateServiceEndpointCorrespondence(workspace, valid); err != nil {
		t.Fatalf("valid endpoint correspondence: %v", err)
	}
	wrongProtocol := append([]workspacev1alpha1.ExecutionWorkspaceEndpoint(nil), valid...)
	wrongProtocol[0].Protocol = "HTTP"
	if err := validateServiceEndpointCorrespondence(workspace, wrongProtocol); err == nil {
		t.Fatal("protocol mismatch passed service endpoint validation")
	}
	credentialed := append([]workspacev1alpha1.ExecutionWorkspaceEndpoint(nil), valid...)
	credentialed[0].URL = "https://user:pass@service.example/mcp"
	if err := workspaceprovider.ValidateEndpoints(credentialed); err == nil {
		t.Fatal("credential-bearing service endpoint passed validation")
	}
}

func TestRunDataPlaneConformance(t *testing.T) {
	const (
		controlValue = "conformance-control-value"
		bearerValue  = "conformance-bearer-value"
	)
	var uploaded []byte
	fileExists := false
	active := false
	bindingGeneration := "binding-1"
	bindingSequence := 1
	execEffects := 0
	execResults := map[string]workspaceagent.ExecResponse{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == workspaceagent.HealthPath:
			_ = json.NewEncoder(w).Encode(workspaceagent.HealthResponse{
				Versioned: workspaceagent.NewVersioned(), Status: "ok",
			})
		case r.URL.Path == workspaceagent.CapabilitiesPath:
			_ = json.NewEncoder(w).Encode(workspaceagent.CapabilitiesResponse{
				Versioned: workspaceagent.NewVersioned(),
				Features: []string{
					"attachment-fencing", "exec", "exec-idempotency", "exec-cancel", "files", "reset",
				},
				BindingGeneration: bindingGeneration,
			})
		case r.URL.Path == workspaceagent.AttachmentControlPath && r.Method == http.MethodPut:
			var request workspaceagent.AttachmentControlRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.BindingGeneration != bindingGeneration {
				http.Error(w, "stale binding", http.StatusConflict)
				return
			}
			active = true
			_ = json.NewEncoder(w).Encode(workspaceagent.AttachmentControlResponse{
				Versioned: workspaceagent.NewVersioned(), WorkspaceUID: request.WorkspaceUID,
				BindingGeneration: bindingGeneration, ActiveEpoch: request.Epoch, Active: true,
			})
		case len(r.URL.Path) > len(workspaceagent.AttachmentControlPrefix) &&
			r.URL.Path[:len(workspaceagent.AttachmentControlPrefix)] == workspaceagent.AttachmentControlPrefix:
			active = false
			_ = json.NewEncoder(w).Encode(workspaceagent.AttachmentControlResponse{
				Versioned: workspaceagent.NewVersioned(), WorkspaceUID: "probe-uid",
				BindingGeneration: bindingGeneration, ActiveEpoch: 1, Active: false,
			})
		case r.URL.Path == workspaceagent.ExecPath:
			if !active {
				http.Error(w, "no active attachment", http.StatusUnauthorized)
				return
			}
			var request workspaceagent.ExecRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if result, ok := execResults[request.OperationID]; ok {
				_ = json.NewEncoder(w).Encode(result)
				return
			}
			result := workspaceagent.ExecResponse{
				Versioned: workspaceagent.NewVersioned(), OperationID: request.OperationID,
				State: workspaceagent.OperationStateSucceeded, ExitCode: 0,
			}
			switch request.OperationID {
			case "conformance-exec":
				execEffects++
			case "conformance-exec-verify":
				result.Stdout = strconv.Itoa(execEffects)
			}
			execResults[request.OperationID] = result
			_ = json.NewEncoder(w).Encode(result)
		case r.URL.Path == workspaceagent.FilesPath:
			if !active {
				http.Error(w, "no active attachment", http.StatusUnauthorized)
				return
			}
			var request workspaceagent.UploadRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if len(request.Files) > 0 {
				uploaded = append([]byte(nil), request.Files[0].Data...)
				fileExists = true
			}
			_ = json.NewEncoder(w).Encode(workspaceagent.UploadResponse{
				Versioned: workspaceagent.NewVersioned(),
			})
		case r.URL.Path == workspaceagent.FilesDownloadPath:
			if !active {
				http.Error(w, "no active attachment", http.StatusUnauthorized)
				return
			}
			var request workspaceagent.DownloadRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if len(request.Paths) == 0 {
				_ = json.NewEncoder(w).Encode(workspaceagent.DownloadResponse{
					Versioned: workspaceagent.NewVersioned(),
				})
				return
			}
			if !fileExists {
				http.Error(w, "file not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(workspaceagent.DownloadResponse{
				Versioned: workspaceagent.NewVersioned(),
				Artifacts: []workspaceagent.DownloadedArtifact{{Data: append([]byte(nil), uploaded...)}},
			})
		case r.URL.Path == workspaceagent.ResetPath:
			var request workspaceagent.ResetRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			if active || request.BindingGeneration != bindingGeneration {
				http.Error(w, "stale binding", http.StatusConflict)
				return
			}
			fileExists = false
			uploaded = nil
			bindingSequence++
			bindingGeneration = fmt.Sprintf("binding-%d", bindingSequence)
			_ = json.NewEncoder(w).Encode(workspaceagent.ResetResponse{
				Versioned: workspaceagent.NewVersioned(), Reset: true, BindingGeneration: bindingGeneration,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	certificate, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	caData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	metadata := workspaceprovider.AdapterMetadata{
		ControllerName: "conformance.test/v1",
		Version:        "1.0.0",
		Contracts:      []string{workspacev1alpha1.ContractVersionV1},
		Features: []workspacev1alpha1.ExecutionWorkspaceFeature{
			workspacev1alpha1.WorkspaceFeatureTLS,
			workspacev1alpha1.WorkspaceFeatureExec,
			workspacev1alpha1.WorkspaceFeatureFiles,
			workspacev1alpha1.WorkspaceFeatureReset,
		},
	}
	driver := &attachmentLifecycleDriver{
		metadata:      &metadata,
		connectionRef: &workspacev1alpha1.SecretReference{Name: "probe-connection"},
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	workspace.UID = types.UID("probe-uid")
	workspace.Spec.Mode = workspacev1alpha1.ExecutionWorkspaceModeInteractive
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	advertised := driver.providerObservation()
	runDataPlaneConformance(t, driver, conformanceProviderFixture(metadata, advertised), &DataPlaneFixture{
		Workspace:          workspace,
		Connection:         workspaceprovider.ConnectionData{Endpoint: server.URL, CAData: caData, ControlAuth: controlValue},
		Bearer:             bearerValue,
		ExecCommand:        []string{"mutate"},
		ExecVerifyCommand:  []string{"verify"},
		ExecExpectedStdout: "1",
		FilePath:           "/workspace/conformance.txt",
		FileData:           []byte("conformance-data"),
	}, metadata, advertised, conformanceTiming{timeout: time.Second, pollInterval: time.Millisecond})
}

func TestConformanceConnectionRequiresTLS(t *testing.T) {
	if err := validateConformanceConnection(workspaceprovider.ConnectionData{
		Endpoint: "http://workspace-agent.example", AllowInsecure: true,
	}); err == nil {
		t.Fatal("insecure conformance connection passed validation")
	}
	if err := validateConformanceConnection(workspaceprovider.ConnectionData{
		Endpoint: "https://workspace-agent.example",
	}); err != nil {
		t.Fatalf("HTTPS conformance connection: %v", err)
	}
}
