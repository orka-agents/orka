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
	active          bool
	everAttached    bool
	initialized     bool
	suspended       bool
	initialPolls    int
	servicePolls    int
	detachPolls     int
	suspendPolls    int
	resumePolls     int
	deletePolls     int
	activateCalls   int
	revocationCalls int
	missingDeadline bool
	metadata        *workspaceprovider.AdapterMetadata
	connectionRef   *workspacev1alpha1.SecretReference
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
	return workspaceprovider.ProviderObservation{}, nil
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
	if workspace.Spec.Mode == workspacev1alpha1.ExecutionWorkspaceModeService {
		if d.servicePolls == 0 {
			d.servicePolls++
			return workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateProvisioning,
				ExternalID: "service-1",
			}, nil
		}
		return workspaceprovider.WorkspaceObservation{
			State:      workspacev1alpha1.ExecutionWorkspaceStateReady,
			ExternalID: "service-1",
			Endpoints: []workspacev1alpha1.ExecutionWorkspaceEndpoint{{
				Name: "mcp", URL: "https://service.example/mcp", Protocol: "HTTPS",
			}},
		}, nil
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
			return workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateDeleting,
				ExternalID: "workspace-1",
			}, nil
		}
		return workspaceprovider.WorkspaceObservation{
			State:       workspacev1alpha1.ExecutionWorkspaceStateDeleted,
			ExternalID:  "workspace-1",
			Disposition: validConformanceDisposition(),
		}, nil
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		if d.suspendPolls == 0 {
			d.suspendPolls++
			return workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateSuspending,
				ExternalID: "workspace-1",
			}, nil
		}
		d.suspended = true
		return workspaceprovider.WorkspaceObservation{
			State:      workspacev1alpha1.ExecutionWorkspaceStateSuspended,
			ExternalID: "workspace-1",
		}, nil
	}
	if workspace.Spec.Attachment != nil {
		if !d.active {
			return workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateAttaching,
				ExternalID: "workspace-1",
			}, nil
		}
		return workspaceprovider.WorkspaceObservation{
			State:         workspacev1alpha1.ExecutionWorkspaceStateAttached,
			ExternalID:    "workspace-1",
			AttachedEpoch: workspace.Spec.Attachment.Epoch,
		}, nil
	}
	if d.everAttached && d.detachPolls == 0 {
		d.detachPolls++
		return workspaceprovider.WorkspaceObservation{
			State:         workspacev1alpha1.ExecutionWorkspaceStateDetaching,
			ExternalID:    "workspace-1",
			AttachedEpoch: 1,
		}, nil
	}
	if d.suspended {
		if d.resumePolls == 0 {
			d.resumePolls++
			return workspaceprovider.WorkspaceObservation{
				State:      workspacev1alpha1.ExecutionWorkspaceStateProvisioning,
				ExternalID: "workspace-1",
			}, nil
		}
		d.suspended = false
	}
	return workspaceprovider.WorkspaceObservation{
		State:         workspacev1alpha1.ExecutionWorkspaceStateReady,
		ExternalID:    "workspace-1",
		ConnectionRef: d.connectionRef,
	}, nil
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
	if workspace.Spec.Attachment != nil || epoch != 1 {
		return context.Canceled
	}
	d.active = false
	return nil
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
	runInteractiveLifecycle(t, driver, workspace, driver.Metadata(), timing)
	service := &workspacev1alpha1.ExecutionWorkspace{}
	service.Spec.Mode = workspacev1alpha1.ExecutionWorkspaceModeService
	service.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	service.Spec.Service = &workspacev1alpha1.ExecutionWorkspaceServiceSpec{
		Ports: []workspacev1alpha1.ExecutionWorkspaceServicePort{{
			Name: "mcp", Port: 8080, Protocol: "HTTPS",
		}},
	}
	runServiceEndpoint(t, driver, service, timing)

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
		driver.suspendPolls != 1 || driver.resumePolls != 1 || driver.deletePolls != 1 {
		t.Fatalf(
			"transition polls = initial %d service %d detach %d suspend %d resume %d delete %d, want 1 each",
			driver.initialPolls,
			driver.servicePolls,
			driver.detachPolls,
			driver.suspendPolls,
			driver.resumePolls,
			driver.deletePolls,
		)
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
		{name: "valid", observation: workspaceprovider.PoolObservation{Available: 2, Allocated: 3, Suspended: 1, Total: 3}},
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
	runDataPlaneConformance(t, driver, &DataPlaneFixture{
		Workspace:          workspace,
		Connection:         workspaceprovider.ConnectionData{Endpoint: server.URL, CAData: caData, ControlAuth: controlValue},
		Bearer:             bearerValue,
		ExecCommand:        []string{"mutate"},
		ExecVerifyCommand:  []string{"verify"},
		ExecExpectedStdout: "1",
		FilePath:           "/workspace/conformance.txt",
		FileData:           []byte("conformance-data"),
	}, metadata, conformanceTiming{timeout: time.Second, pollInterval: time.Millisecond})
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
