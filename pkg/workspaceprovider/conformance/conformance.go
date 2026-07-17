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
	t.Run("workspace-agent data plane", func(t *testing.T) {
		if !requiresDataPlane(metadata.Features) {
			t.Skip("driver does not advertise workspace-agent data-plane features")
		}
		runDataPlaneConformance(t, driver, fixtures.DataPlane, metadata, timing)
	})
	t.Run("interactive idempotency and lifecycle", func(t *testing.T) {
		if !slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureExec) {
			t.Skip("driver does not advertise interactive exec support")
		}
		runInteractiveLifecycle(t, driver, fixtures.Interactive, metadata, timing)
	})
	t.Run("service endpoint", func(t *testing.T) {
		if !slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureServicePorts) {
			t.Skip("driver does not advertise service endpoint support")
		}
		runServiceEndpoint(t, driver, fixtures.Service, timing)
	})
}

func validateAdapterMetadata(metadata workspaceprovider.AdapterMetadata) error {
	if metadata.ControllerName == "" || metadata.Version == "" {
		return fmt.Errorf("driver metadata is incomplete: %#v", metadata)
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
) {
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
	if observation.Total < observation.Available || observation.Total < observation.Allocated ||
		observation.Total < observation.Suspended {
		return fmt.Errorf("total capacity must cover every reported pool counter")
	}
	return nil
}

//nolint:gocyclo // The conformance probe intentionally exercises each advertised data-plane capability.
func runDataPlaneConformance(
	t *testing.T,
	driver workspaceprovider.Driver,
	fixture *DataPlaneFixture,
	metadata workspaceprovider.AdapterMetadata,
	timing conformanceTiming,
) {
	t.Helper()
	if fixture == nil || fixture.Workspace == nil {
		t.Fatal("data-plane workspace fixture is required")
	}
	workspace := fixture.Workspace.DeepCopy()
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

func runInteractiveLifecycle(
	t *testing.T,
	driver workspaceprovider.Driver,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	metadata workspaceprovider.AdapterMetadata,
	timing conformanceTiming,
) {
	t.Helper()
	if workspace == nil {
		t.Fatal("interactive fixture is required when exec support is advertised")
	}
	ctx := context.Background()
	workspace = workspace.DeepCopy()
	first := reconcileWorkspaceUntil(t, ctx, driver, workspace, "initial provisioning", timing, func(
		observation workspaceprovider.WorkspaceObservation,
	) bool {
		return observation.State == workspacev1alpha1.ExecutionWorkspaceStateReady &&
			observation.ExternalID != ""
	})
	second := reconcileWorkspaceOnce(t, ctx, driver, workspace, "idempotency", timing)
	if first.ExternalID == "" || first.ExternalID != second.ExternalID {
		t.Fatalf("idempotent external IDs = %q and %q", first.ExternalID, second.ExternalID)
	}

	workspace.Spec.Attachment = conformanceAttachment()
	attachmentController, controlsAttachment := driver.(workspaceprovider.AttachmentController)
	if controlsAttachment {
		operationCtx, cancel := context.WithTimeout(ctx, timing.timeout)
		err := attachmentController.ActivateAttachment(operationCtx, workspace.DeepCopy())
		cancel()
		if err != nil {
			t.Fatalf("ActivateAttachment: %v", err)
		}
	}
	attached := reconcileWorkspaceUntil(t, ctx, driver, workspace, "attachment activation", timing, func(
		observation workspaceprovider.WorkspaceObservation,
	) bool {
		return observation.State == workspacev1alpha1.ExecutionWorkspaceStateAttached &&
			observation.AttachedEpoch == workspace.Spec.Attachment.Epoch
	})
	if attached.State != workspacev1alpha1.ExecutionWorkspaceStateAttached || attached.AttachedEpoch != 1 {
		t.Fatalf("attached observation = %#v", attached)
	}

	detachedEpoch := workspace.Spec.Attachment.Epoch
	workspace.Spec.Attachment = nil
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	if controlsAttachment {
		operationCtx, cancel := context.WithTimeout(ctx, timing.timeout)
		err := attachmentController.RevokeAttachment(operationCtx, workspace.DeepCopy(), detachedEpoch)
		cancel()
		if err != nil {
			t.Fatalf("RevokeAttachment: %v", err)
		}
	}
	reconcileWorkspaceUntil(t, ctx, driver, workspace, "attachment revocation", timing, func(
		observation workspaceprovider.WorkspaceObservation,
	) bool {
		return observation.State == workspacev1alpha1.ExecutionWorkspaceStateReady &&
			observation.AttachedEpoch == 0
	})
	suspendAllowed := slices.Contains(
		workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	if suspendAllowed && slices.Contains(metadata.Features, workspacev1alpha1.WorkspaceFeatureSuspend) {
		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		reconcileWorkspaceUntil(t, ctx, driver, workspace, "suspension", timing, func(
			observation workspaceprovider.WorkspaceObservation,
		) bool {
			return observation.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended &&
				observation.AttachedEpoch == 0
		})

		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
		reconcileWorkspaceUntil(t, ctx, driver, workspace, "resume", timing, func(
			observation workspaceprovider.WorkspaceObservation,
		) bool {
			return observation.State == workspacev1alpha1.ExecutionWorkspaceStateReady &&
				observation.AttachedEpoch == 0
		})
	}

	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	deleted := reconcileWorkspaceUntil(t, ctx, driver, workspace, "deletion", timing, func(
		observation workspaceprovider.WorkspaceObservation,
	) bool {
		return observation.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted
	})
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
	workspace *workspacev1alpha1.ExecutionWorkspace,
	timing conformanceTiming,
) {
	t.Helper()
	if workspace == nil {
		t.Fatal("service fixture is required when service endpoint support is advertised")
	}
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
	if disposition == nil {
		return false
	}
	terminal := func(
		state workspacev1alpha1.ExecutionWorkspaceDispositionState,
		allowed ...workspacev1alpha1.ExecutionWorkspaceDispositionState,
	) bool {
		return slices.Contains(allowed, state)
	}
	policySatisfied := func(
		state workspacev1alpha1.ExecutionWorkspaceDispositionState,
		action workspacev1alpha1.WorkspaceDeletionAction,
	) bool {
		if state == workspacev1alpha1.DispositionNotApplicable {
			return true
		}
		if action == workspacev1alpha1.WorkspaceDeletionActionRetain {
			return state == workspacev1alpha1.DispositionRetained
		}
		return action == workspacev1alpha1.WorkspaceDeletionActionDelete &&
			state == workspacev1alpha1.DispositionDeleted
	}
	return terminal(
		disposition.Compute,
		workspacev1alpha1.DispositionDeleted,
		workspacev1alpha1.DispositionNotApplicable,
	) && terminal(
		disposition.AccessCredentials,
		workspacev1alpha1.DispositionRevoked,
		workspacev1alpha1.DispositionDeleted,
		workspacev1alpha1.DispositionNotApplicable,
	) && terminal(
		disposition.EphemeralSecrets,
		workspacev1alpha1.DispositionDeleted,
		workspacev1alpha1.DispositionNotApplicable,
	) && terminal(
		disposition.WorkspaceData,
		workspacev1alpha1.DispositionDeleted,
		workspacev1alpha1.DispositionRetained,
		workspacev1alpha1.DispositionNotApplicable,
	) && policySatisfied(disposition.PersistentVolumes, policy.PersistentVolumes) &&
		policySatisfied(disposition.Checkpoints, policy.Checkpoints) &&
		policySatisfied(disposition.ProviderResources, policy.ProviderResources)
}

func validInteractiveDeletedDisposition(
	disposition *workspacev1alpha1.ExecutionWorkspaceDisposition,
	policy workspacev1alpha1.ExecutionWorkspaceDeletionPolicy,
) bool {
	if !validDeletedDisposition(disposition, policy) {
		return false
	}
	return disposition.AccessCredentials == workspacev1alpha1.DispositionRevoked ||
		disposition.AccessCredentials == workspacev1alpha1.DispositionDeleted
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
