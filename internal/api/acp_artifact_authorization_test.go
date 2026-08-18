package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	types "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

func TestACPArtifactAuthorizationBrokerIssuesExactUploadCapability(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	poolUID := types.UID("pool-uid")
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool", UID: poolUID, Generation: 1},
		Status:     corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{PodNamespace: "orka-runtimes", RuntimeInstanceID: "runtime-1", ControllerEpoch: 1}},
	}
	controllerToken := strings.Repeat("t", 32)
	operationSecret := []byte(strings.Repeat("s", 32))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "pool-auth-e1", Labels: map[string]string{"orka.ai/runtime-pool-auth": "true", "orka.ai/runtime-pool-uid": string(poolUID)}}, Data: map[string][]byte{runtimePoolControllerTokenKeyAPI: []byte(controllerToken), runtimePoolCapabilitySecretKeyAPI: operationSecret}}
	taskUID := types.UID("task-uid")
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: taskUID}, Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateSettling, PromptID: "prompt-1", RuntimeSessionUID: "session-1", RuntimeInstanceID: "runtime-1"}}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, secret, task).Build()
	artifactSecret := []byte(strings.Repeat("a", 32))
	secretFile := filepath.Join(t.TempDir(), "artifact-secret")
	if err := os.WriteFile(secretFile, artifactSecret, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envACPArtifactSecretFile, secretFile)

	artifactData := []byte("delta")
	digest := artifactcap.DigestBytes(artifactData)
	artifactID, _ := artifactcap.ArtifactIDForDigest(digest)
	metadata := harnessv2.MutationMetadata{
		Fence:   harnessv2.Fence{RuntimeInstanceID: "runtime-1", SupervisorBootID: "boot", ControllerEpoch: 1, RuntimePoolUID: harnessv2.RuntimePoolUID(poolUID), RuntimePoolGeneration: 1, RuntimeSessionUID: "session-1", RuntimeSessionGeneration: 1, RuntimeProfileDigest: harnessv2.ProfileDigest("sha256:" + strings.Repeat("b", 64)), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion},
		TaskUID: harnessv2.TaskUID(taskUID), TaskAttempt: 1, PromptID: "prompt-1", OperationID: "authorize-delta-1", RequestDigestSchemaVersion: 1, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	request := acpArtifactAuthorizationRequest{Namespace: "default", Metadata: metadata, Artifact: harnessv2.ArtifactReference{ArtifactID: harnessv2.ArtifactID(artifactID), Digest: digest, SizeBytes: int64(len(artifactData)), MediaType: artifactcap.MediaTypeWorkspaceDelta}}
	requestDigest, err := harnessv2.CanonicalRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Metadata.RequestDigest = requestDigest
	capability, err := harnessv2.SignOperationCapability(operationSecret, harnessv2.ClaimsForMutation(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(request)
	app := fiber.New()
	reservations := &recordingCapabilityReservations{}
	server := &Server{app: app, client: kubeClient, config: ServerConfig{ArtifactReservations: reservations}}
	server.installACPArtifactAuthorizationBroker()
	httpRequest := httptest.NewRequest(http.MethodPost, acpArtifactAuthorizationPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+controllerToken)
	httpRequest.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	httpRequest.Header.Set(harnessv2.MCPBrokerPoolNamespaceHeader, "default")
	httpRequest.Header.Set(harnessv2.MCPBrokerPoolUIDHeader, string(poolUID))
	reservationStart := time.Now().UTC()
	response, err := app.Test(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var issued acpArtifactAuthorizationResponse
	if err := json.NewDecoder(response.Body).Decode(&issued); err != nil {
		t.Fatal(err)
	}
	presented := artifactcap.PresentedRequest{Method: http.MethodPut, Path: mustArtifactPath(t, digest), ObjectDigest: digest, ContentLength: int64(len(artifactData)), MediaType: artifactcap.MediaTypeWorkspaceDelta, RequestDigest: issued.RequestDigest}
	if _, err := artifactcap.Verify(artifactSecret, issued.Capability, presented, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if len(reservations.requests) != 1 || reservations.requests[0] != (artifactcap.OperationRequest{
		Operation: artifactcap.OperationUpload, ObjectDigest: digest,
		Identity:      artifactcap.Identity{Namespace: "default", TaskID: string(taskUID)},
		ContentLength: int64(len(artifactData)), MediaType: artifactcap.MediaTypeWorkspaceDelta,
		OperationID: "runtime-delta-upload-authorize-delta-1",
	}) {
		t.Fatalf("capability reservations = %#v, want exact upload binding", reservations.requests)
	}
	minimumExpiry := reservationStart.Add(2*time.Minute + artifactcap.MaxClockSkew)
	if reservations.expiresAt[0].Before(minimumExpiry) {
		t.Fatalf("capability reservation expiry = %s, want at least %s", reservations.expiresAt[0], minimumExpiry)
	}
}

func TestACPArtifactAuthorizationBrokerAuthenticatesBeforeBody(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	poolUID := types.UID("pool-uid")
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool", UID: poolUID, Generation: 1},
		Status:     corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{PodNamespace: "orka-runtimes", RuntimeInstanceID: "runtime-1", ControllerEpoch: 1}},
	}
	controllerToken := strings.Repeat("t", 32)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "pool-auth-e1", Labels: map[string]string{"orka.ai/runtime-pool-auth": "true", "orka.ai/runtime-pool-uid": string(poolUID)}},
		Data:       map[string][]byte{runtimePoolControllerTokenKeyAPI: []byte(controllerToken), runtimePoolCapabilitySecretKeyAPI: []byte(strings.Repeat("s", 32))},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, secret).Build()
	app := fiber.New()
	server := &Server{app: app, client: kubeClient, config: ServerConfig{ArtifactReservations: &recordingCapabilityReservations{}}}
	server.installACPArtifactAuthorizationBroker()

	cases := []struct {
		name           string
		bearer         string
		setPoolHeaders bool
		body           string
	}{
		{name: "missing pool identity headers", bearer: controllerToken, setPoolHeaders: false, body: "{}"},
		// An invalid-JSON body would yield 400 if the handler parsed it first;
		// rejecting the wrong bearer with 403 proves pre-auth precedes the body.
		{name: "wrong bearer before body parse", bearer: strings.Repeat("x", 32), setPoolHeaders: true, body: "not-json{"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			httpRequest := httptest.NewRequest(http.MethodPost, acpArtifactAuthorizationPath, strings.NewReader(tc.body))
			httpRequest.Header.Set("Content-Type", "application/json")
			httpRequest.Header.Set("Authorization", "Bearer "+tc.bearer)
			if tc.setPoolHeaders {
				httpRequest.Header.Set(harnessv2.MCPBrokerPoolNamespaceHeader, "default")
				httpRequest.Header.Set(harnessv2.MCPBrokerPoolUIDHeader, string(poolUID))
			}
			response, err := app.Test(httpRequest)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != fiber.StatusForbidden {
				t.Fatalf("status = %d, want %d", response.StatusCode, fiber.StatusForbidden)
			}
		})
	}
}

type recordingCapabilityReservations struct {
	requests  []artifactcap.OperationRequest
	expiresAt []time.Time
	err       error
}

func (r *recordingCapabilityReservations) Reserve(_ context.Context, request artifactcap.OperationRequest, expiresAt time.Time) error {
	if r.err != nil {
		return r.err
	}
	r.requests = append(r.requests, request)
	r.expiresAt = append(r.expiresAt, expiresAt)
	return nil
}

func TestACPArtifactAuthorizationBrokerRejectsStaleCachedRevocationState(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	poolUID := types.UID("pool-uid")
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool", UID: poolUID, Generation: 1},
		Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
			PodNamespace: "orka-runtimes", RuntimeInstanceID: "runtime-1", ControllerEpoch: 1,
		}},
	}
	controllerToken := strings.Repeat("t", 32)
	operationSecret := []byte(strings.Repeat("s", 32))
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "orka-runtimes", Name: "pool-auth-e1", UID: "old-secret-uid",
			Labels: map[string]string{"orka.ai/runtime-pool-auth": "true", "orka.ai/runtime-pool-uid": string(poolUID)},
		},
		Data: map[string][]byte{
			runtimePoolControllerTokenKeyAPI:  []byte(controllerToken),
			runtimePoolCapabilitySecretKeyAPI: operationSecret,
		},
	}
	taskUID := types.UID("task-uid")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: taskUID},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSettling, PromptID: "prompt-1",
			RuntimeSessionUID: "session-1", RuntimeInstanceID: "runtime-1",
		}},
	}
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, authSecret, task).Build()

	artifactSecret := []byte(strings.Repeat("a", 32))
	writeAPISecretFile(t, envACPArtifactSecretFile, "artifact", artifactSecret)
	artifactData := []byte("delta")
	digest := artifactcap.DigestBytes(artifactData)
	artifactID, err := artifactcap.ArtifactIDForDigest(digest)
	if err != nil {
		t.Fatal(err)
	}
	metadata := harnessv2.MutationMetadata{
		Fence: harnessv2.Fence{
			RuntimeInstanceID: "runtime-1", SupervisorBootID: "boot", ControllerEpoch: 1,
			RuntimePoolUID: harnessv2.RuntimePoolUID(poolUID), RuntimePoolGeneration: 1,
			RuntimeSessionUID: "session-1", RuntimeSessionGeneration: 1,
			RuntimeProfileDigest: harnessv2.ProfileDigest("sha256:" + strings.Repeat("b", 64)), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
		},
		TaskUID: harnessv2.TaskUID(taskUID), TaskAttempt: 1, PromptID: "prompt-1", OperationID: "authorize-delta-1",
		RequestDigestSchemaVersion: 1, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	request := acpArtifactAuthorizationRequest{
		Namespace: "default", Metadata: metadata,
		Artifact: harnessv2.ArtifactReference{
			ArtifactID: harnessv2.ArtifactID(artifactID), Digest: digest,
			SizeBytes: int64(len(artifactData)), MediaType: artifactcap.MediaTypeWorkspaceDelta,
		},
	}
	requestDigest, err := harnessv2.CanonicalRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Metadata.RequestDigest = requestDigest
	capability, err := harnessv2.SignOperationCapability(operationSecret, harnessv2.ClaimsForMutation(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		currentObjects func() []client.Object
	}{
		{
			name: "runtime pool replacement",
			currentObjects: func() []client.Object {
				replacementPool := pool.DeepCopy()
				replacementPool.UID = "replacement-pool-uid"
				replacementSecret := authSecret.DeepCopy()
				replacementSecret.UID = "replacement-secret-uid"
				replacementSecret.Labels["orka.ai/runtime-pool-uid"] = string(replacementPool.UID)
				return []client.Object{replacementPool, replacementSecret, task.DeepCopy()}
			},
		},
		{
			name: "runtime pool auth Secret replacement",
			currentObjects: func() []client.Object {
				replacementSecret := authSecret.DeepCopy()
				replacementSecret.UID = "replacement-secret-uid"
				replacementSecret.Data = map[string][]byte{
					runtimePoolControllerTokenKeyAPI:  []byte(strings.Repeat("r", 32)),
					runtimePoolCapabilitySecretKeyAPI: []byte(strings.Repeat("n", 32)),
				}
				return []client.Object{pool.DeepCopy(), replacementSecret, task.DeepCopy()}
			},
		},
		{
			name: "Task cancellation",
			currentObjects: func() []client.Object {
				cancelledTask := task.DeepCopy()
				cancelledTask.Status.Execution.State = corev1alpha1.TaskExecutionStateCancelled
				return []client.Object{pool.DeepCopy(), authSecret.DeepCopy(), cancelledTask}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(test.currentObjects()...).Build()
			app := fiber.New()
			server := &Server{app: app, client: cachedClient, config: ServerConfig{APIReader: apiReader}}
			server.installACPArtifactAuthorizationBroker()
			httpRequest := httptest.NewRequest(http.MethodPost, acpArtifactAuthorizationPath, bytes.NewReader(body))
			httpRequest.Header.Set("Content-Type", "application/json")
			httpRequest.Header.Set("Authorization", "Bearer "+controllerToken)
			httpRequest.Header.Set(harnessv2.OperationCapabilityHeader, capability)
			response, err := app.Test(httpRequest)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("status=%d, want %d", response.StatusCode, http.StatusForbidden)
			}
		})
	}
}

func mustArtifactPath(t *testing.T, digest string) string {
	t.Helper()
	value, err := artifactcap.ObjectPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPublisherArtifactAuthorizationBrokerBindsTaskAndPublicationState(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	taskUID := types.UID("publisher-task-uid")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publisher-task", UID: taskUID},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStatePlanned, PromptID: "prompt-publisher",
		}},
	}
	delta := []byte("publisher delta")
	deltaDigest := artifactcap.DigestBytes(delta)
	deltaID, err := artifactcap.ArtifactIDForDigest(deltaDigest)
	if err != nil {
		t.Fatal(err)
	}
	bundle := []byte("prepared git bundle")
	bundleDigest := artifactcap.DigestBytes(bundle)
	bundleID, err := artifactcap.ArtifactIDForDigest(bundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	publication := &corev1alpha1.Publication{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publication-control"},
		Spec: corev1alpha1.PublicationSpec{
			ID: "publication-id", Generation: 1, TaskUID: string(taskUID), Attempt: 1, PromptID: "prompt-publisher",
			BranchClaimID: "branch-claim", BranchClaimGeneration: 1, SourceRepositoryID: "github.com/o/r",
			SourceRef: "refs/heads/main", SourceBaselineSHA: strings.Repeat("1", 40), TargetRepositoryID: "github.com/o/r",
			TargetRef: "refs/heads/change", ArtifactID: deltaID, ArtifactDigest: deltaDigest,
			ArtifactSizeBytes: int64(len(delta)), ArtifactMediaType: artifactcap.MediaTypeWorkspaceDelta,
			PublicationCredentialRef: "credential", CommitIdentity: "task", CommitMessage: "change",
			CommitTimestamp: metav1.NewTime(time.Now().UTC()), RequestDigest: "sha256:" + strings.Repeat("2", 64),
		},
		Status: corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState(store.PublicationPreparing)},
	}
	readyPublication := publication.DeepCopy()
	readyPublication.Name = "publication-ready"
	readyPublication.Spec.ID = "publication-ready-id"
	readyPublication.Status = corev1alpha1.PublicationStatus{
		State: corev1alpha1.PublicationControlState(store.PublicationVerifying),
		PreparedReceipt: &corev1alpha1.PreparedPublicationControlReceipt{
			OperationID: "prepare-ready", RequestDigest: "sha256:" + strings.Repeat("3", 64),
			TreeSHA: strings.Repeat("4", 40), CommitSHA: strings.Repeat("5", 40), ManifestDigest: "sha256:" + strings.Repeat("6", 64),
			BundleArtifactID: bundleID, BundleDigest: bundleDigest, BundleSizeBytes: int64(len(bundle)),
			BundleMediaType: artifactcap.MediaTypeGitBundle, BundleRef: "refs/orka/publications/" + strings.Repeat("7", 64),
			PreparedAt: metav1.NewTime(time.Now().UTC()),
		},
	}
	effects := []client.Object{
		publisherEffectForTest("workspace-effect", "workspace.prepare", string(taskUID), "workspace-prepare-prompt-publisher"),
		publisherEffectForTest("prepare-effect", "publisher.prepare", publication.Spec.ID, "prepare-operation"),
		publisherEffectForTest("verify-effect", "publisher.verify", readyPublication.Spec.ID, "verify-operation"),
	}
	objects := []client.Object{task, publication, readyPublication}
	objects = append(objects, effects...)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	artifactSecret := []byte(strings.Repeat("a", 32))
	publisherToken := strings.Repeat("p", 32)
	writeAPISecretFile(t, envACPArtifactSecretFile, "artifact", artifactSecret)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher", []byte(publisherToken))

	workspace := []byte("workspace tar")
	workspaceDigest := artifactcap.DigestBytes(workspace)
	workspaceID, err := artifactcap.ArtifactIDForDigest(workspaceDigest)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		request publisherservice.ArtifactAuthorizationRequest
	}{
		{
			name: "workspace upload",
			request: publisherservice.ArtifactAuthorizationRequest{
				ParentOperation:   publisherservice.OperationWorkspacePrepare,
				Metadata:          publisherservice.OperationMetadata{Namespace: "default", TaskID: string(taskUID), OperationID: "workspace-prepare-prompt-publisher"},
				ArtifactOperation: artifactcap.OperationUpload,
				Artifact:          harnessv2.ArtifactReference{ArtifactID: harnessv2.ArtifactID(workspaceID), Digest: workspaceDigest, SizeBytes: int64(len(workspace)), MediaType: artifactcap.MediaTypeWorkspaceTar},
				Attempt:           1,
			},
		},
		{
			name: "publication delta download",
			request: publisherservice.ArtifactAuthorizationRequest{
				ParentOperation:   publisherservice.OperationPublicationPrepare,
				Metadata:          publisherservice.OperationMetadata{Namespace: "default", PublicationID: publication.Spec.ID, OperationID: "prepare-operation"},
				ArtifactOperation: artifactcap.OperationDownload,
				Artifact:          harnessv2.ArtifactReference{ArtifactID: harnessv2.ArtifactID(deltaID), Digest: deltaDigest, SizeBytes: int64(len(delta)), MediaType: artifactcap.MediaTypeWorkspaceDelta},
				Attempt:           1,
			},
		},
		{
			name: "prepared bundle upload",
			request: publisherservice.ArtifactAuthorizationRequest{
				ParentOperation:   publisherservice.OperationPublicationPrepare,
				Metadata:          publisherservice.OperationMetadata{Namespace: "default", PublicationID: publication.Spec.ID, OperationID: "prepare-operation"},
				ArtifactOperation: artifactcap.OperationUpload,
				Artifact:          harnessv2.ArtifactReference{ArtifactID: harnessv2.ArtifactID(bundleID), Digest: bundleDigest, SizeBytes: int64(len(bundle)), MediaType: artifactcap.MediaTypeGitBundle},
				Attempt:           1,
			},
		},
		{
			name: "prepared bundle verification download",
			request: publisherservice.ArtifactAuthorizationRequest{
				ParentOperation:   publisherservice.OperationPublicationVerify,
				Metadata:          publisherservice.OperationMetadata{Namespace: "default", PublicationID: readyPublication.Spec.ID, OperationID: "verify-operation"},
				ArtifactOperation: artifactcap.OperationDownload,
				Artifact:          harnessv2.ArtifactReference{ArtifactID: harnessv2.ArtifactID(bundleID), Digest: bundleDigest, SizeBytes: int64(len(bundle)), MediaType: artifactcap.MediaTypeGitBundle},
				Attempt:           1,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := fiber.New()
			server := &Server{app: app, client: kubeClient, config: ServerConfig{ArtifactReservations: &recordingCapabilityReservations{}}}
			server.installACPArtifactAuthorizationBroker()
			body, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}
			httpRequest := httptest.NewRequest(http.MethodPost, publisherservice.ArtifactAuthorizationBrokerPath, bytes.NewReader(body))
			httpRequest.Header.Set("Content-Type", "application/json")
			httpRequest.Header.Set("Authorization", "Bearer "+publisherToken)
			response, err := app.Test(httpRequest)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", response.StatusCode)
			}
			var issued publisherservice.ArtifactAuthorizationResponse
			if err := json.NewDecoder(response.Body).Decode(&issued); err != nil {
				t.Fatal(err)
			}
			binding, err := publisherservice.ArtifactBinding(test.request)
			if err != nil {
				t.Fatal(err)
			}
			path, err := artifactcap.ObjectPath(binding.ObjectDigest)
			if err != nil {
				t.Fatal(err)
			}
			presented := artifactcap.PresentedRequest{
				Method: binding.Method(), Path: path, ObjectDigest: binding.ObjectDigest,
				ContentLength: binding.ContentLength, MediaType: binding.MediaType, RequestDigest: issued.RequestDigest,
			}
			if _, err := artifactcap.Verify(artifactSecret, issued.Capability, presented, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublisherArtifactAuthorizationBrokerUsesFreshTaskState(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	taskUID := types.UID("fresh-publisher-task-uid")
	freshTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publisher-task", UID: taskUID},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStatePlanned, PromptID: "fresh-prompt",
		}},
	}
	staleTask := freshTask.DeepCopy()
	staleTask.Status.Execution.State = corev1alpha1.TaskExecutionStateRunning

	workspace := []byte("fresh workspace tar")
	workspaceDigest := artifactcap.DigestBytes(workspace)
	workspaceID, err := artifactcap.ArtifactIDForDigest(workspaceDigest)
	if err != nil {
		t.Fatal(err)
	}
	request := publisherservice.ArtifactAuthorizationRequest{
		ParentOperation: publisherservice.OperationWorkspacePrepare,
		Metadata: publisherservice.OperationMetadata{
			Namespace: "default", TaskID: string(taskUID), OperationID: "workspace-prepare-fresh-prompt",
		},
		ArtifactOperation: artifactcap.OperationUpload,
		Artifact: harnessv2.ArtifactReference{
			ArtifactID: harnessv2.ArtifactID(workspaceID), Digest: workspaceDigest,
			SizeBytes: int64(len(workspace)), MediaType: artifactcap.MediaTypeWorkspaceTar,
		},
		Attempt: 1,
	}
	effect := publisherEffectForTest(
		"fresh-workspace-effect", "workspace.prepare", string(taskUID), request.Metadata.OperationID,
	)
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(staleTask, effect.DeepCopy()).Build()
	apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(freshTask, effect).Build()

	artifactSecret := []byte(strings.Repeat("a", 32))
	publisherToken := strings.Repeat("p", 32)
	writeAPISecretFile(t, envACPArtifactSecretFile, "artifact", artifactSecret)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher", []byte(publisherToken))

	app := fiber.New()
	server := &Server{
		app: app, client: cachedClient,
		config: ServerConfig{APIReader: apiReader, ArtifactReservations: &recordingCapabilityReservations{}},
	}
	server.installACPArtifactAuthorizationBroker()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, publisherservice.ArtifactAuthorizationBrokerPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+publisherToken)
	response, err := app.Test(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestPublisherArtifactAuthorizationBrokerUsesFreshPublicationState(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	delta := []byte("fresh publication delta")
	deltaDigest := artifactcap.DigestBytes(delta)
	deltaID, err := artifactcap.ArtifactIDForDigest(deltaDigest)
	if err != nil {
		t.Fatal(err)
	}
	freshPublication := &corev1alpha1.Publication{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "fresh-publication"},
		Spec: corev1alpha1.PublicationSpec{
			ID: "fresh-publication-id", ArtifactID: deltaID, ArtifactDigest: deltaDigest,
			ArtifactSizeBytes: int64(len(delta)), ArtifactMediaType: artifactcap.MediaTypeWorkspaceDelta,
		},
		Status: corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState(store.PublicationPreparing)},
	}
	stalePublication := freshPublication.DeepCopy()
	stalePublication.Status.State = corev1alpha1.PublicationControlState(store.PublicationPublishing)
	request := publisherservice.ArtifactAuthorizationRequest{
		ParentOperation: publisherservice.OperationPublicationPrepare,
		Metadata: publisherservice.OperationMetadata{
			Namespace: "default", PublicationID: freshPublication.Spec.ID, OperationID: "prepare-fresh-publication",
		},
		ArtifactOperation: artifactcap.OperationDownload,
		Artifact: harnessv2.ArtifactReference{
			ArtifactID: harnessv2.ArtifactID(deltaID), Digest: deltaDigest,
			SizeBytes: int64(len(delta)), MediaType: artifactcap.MediaTypeWorkspaceDelta,
		},
		Attempt: 1,
	}
	effect := publisherEffectForTest(
		"fresh-publication-effect", "publisher.prepare", freshPublication.Spec.ID, request.Metadata.OperationID,
	)
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stalePublication).Build()
	apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(freshPublication, effect).Build()

	artifactSecret := []byte(strings.Repeat("a", 32))
	publisherToken := strings.Repeat("p", 32)
	writeAPISecretFile(t, envACPArtifactSecretFile, "artifact", artifactSecret)
	writeAPISecretFile(t, envWorkspacePublisherControllerTokenFile, "publisher", []byte(publisherToken))

	app := fiber.New()
	server := &Server{
		app: app, client: cachedClient,
		config: ServerConfig{APIReader: apiReader, ArtifactReservations: &recordingCapabilityReservations{}},
	}
	server.installACPArtifactAuthorizationBroker()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, publisherservice.ArtifactAuthorizationBrokerPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+publisherToken)
	response, err := app.Test(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestAuthorizePublisherParentEffectUsesFreshReaderWithFallback(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	metadata := publisherservice.OperationMetadata{
		Namespace: "default", TaskID: "task-uid", OperationID: "workspace-prepare-prompt",
	}
	inFlightEffect := publisherEffectForTest(
		"workspace-effect", "workspace.prepare", metadata.TaskID, metadata.OperationID,
	)
	settledEffect := inFlightEffect.DeepCopy()
	settledEffect.Status.State = corev1alpha1.ExternalEffectControlState(store.ExternalEffectSucceeded)

	tests := []struct {
		name          string
		cachedObjects []client.Object
		freshObjects  []client.Object
		useAPIReader  bool
		wantErr       bool
	}{
		{
			name:         "fresh reader observes newly created in-flight effect",
			freshObjects: []client.Object{inFlightEffect.DeepCopy()},
			useAPIReader: true,
		},
		{
			name:          "fresh reader rejects effect settled behind stale cache",
			cachedObjects: []client.Object{inFlightEffect.DeepCopy()},
			freshObjects:  []client.Object{settledEffect},
			useAPIReader:  true,
			wantErr:       true,
		},
		{
			name:          "cached client remains fallback when API reader is unset",
			cachedObjects: []client.Object{inFlightEffect.DeepCopy()},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(test.cachedObjects...).Build()
			server := &Server{client: cachedClient}
			if test.useAPIReader {
				server.config.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(test.freshObjects...).Build()
			}

			err := server.authorizePublisherParentEffect(
				context.Background(), publisherservice.OperationWorkspacePrepare, metadata,
			)
			if test.wantErr && err == nil {
				t.Fatal("expected authorization to fail")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("expected authorization to succeed: %v", err)
			}
		})
	}
}

func TestAuthorizePublisherParentEffectRequiresLiveLease(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	metadata := publisherservice.OperationMetadata{
		Namespace: "default", TaskID: "task-uid", OperationID: "workspace-prepare-prompt",
	}
	base := func() *corev1alpha1.ExternalEffect {
		return publisherEffectForTest("workspace-effect", "workspace.prepare", metadata.TaskID, metadata.OperationID)
	}
	tests := []struct {
		name    string
		mutate  func(*corev1alpha1.ExternalEffect)
		wantErr bool
	}{
		{name: "live lease authorizes", mutate: func(*corev1alpha1.ExternalEffect) {}},
		{
			name: "expired lease is rejected",
			mutate: func(e *corev1alpha1.ExternalEffect) {
				e.Status.LeaseExpiresAt = &metav1.Time{Time: time.Now().UTC().Add(-time.Minute)}
			},
			wantErr: true,
		},
		{
			name:    "missing lease expiry is rejected",
			mutate:  func(e *corev1alpha1.ExternalEffect) { e.Status.LeaseExpiresAt = nil },
			wantErr: true,
		},
		{
			name:    "missing lease owner is rejected",
			mutate:  func(e *corev1alpha1.ExternalEffect) { e.Status.LeaseOwner = "" },
			wantErr: true,
		},
		{
			name:    "missing controller epoch is rejected",
			mutate:  func(e *corev1alpha1.ExternalEffect) { e.Status.ControllerEpoch = 0 },
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			effect := base()
			test.mutate(effect)
			server := &Server{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(effect).Build()}
			err := server.authorizePublisherParentEffect(
				context.Background(), publisherservice.OperationWorkspacePrepare, metadata,
			)
			if test.wantErr && err == nil {
				t.Fatal("expected authorization to fail for a non-live lease")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("expected authorization to succeed: %v", err)
			}
		})
	}
}

func publisherEffectForTest(name, kind, aggregateID, operationID string) *corev1alpha1.ExternalEffect {
	return &corev1alpha1.ExternalEffect{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: corev1alpha1.ExternalEffectSpec{
			ID: name, Kind: kind, IdentityNamespace: "default", AggregateID: aggregateID,
			OperationID: operationID, RequestDigest: "sha256:" + strings.Repeat("9", 64),
		},
		Status: corev1alpha1.ExternalEffectStatus{
			State:                       corev1alpha1.ExternalEffectControlState(store.ExternalEffectInFlight),
			LeaseOwner:                  "controller-epoch-1",
			LeaseExpiresAt:              &metav1.Time{Time: time.Now().UTC().Add(2 * time.Minute)},
			ControlRecordMutationStatus: corev1alpha1.ControlRecordMutationStatus{ControllerEpoch: 1},
		},
	}
}

func writeAPISecretFile(t *testing.T, env, name string, value []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, value, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(env, path)
}
