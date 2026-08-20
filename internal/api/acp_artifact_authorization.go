package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

const acpArtifactAuthorizationPath = "/internal/v2/acp/artifact-authorizations"

type acpArtifactAuthorizationRequest struct {
	Namespace string                      `json:"namespace"`
	Metadata  harnessv2.MutationMetadata  `json:"metadata"`
	Artifact  harnessv2.ArtifactReference `json:"artifact"`
}

type acpArtifactAuthorizationResponse struct {
	Capability    string `json:"capability"`
	RequestDigest string `json:"requestDigest"`
}

func (s *Server) installACPArtifactAuthorizationBroker() {
	s.app.Post(acpArtifactAuthorizationPath, s.issueACPArtifactAuthorization)
	s.app.Post(publisherservice.ArtifactAuthorizationBrokerPath, s.issuePublisherArtifactAuthorization)
	s.app.Post(publisherservice.CredentialBrokerPath, s.issuePublisherCredential)
}

func (s *Server) issueACPArtifactAuthorization(c fiber.Ctx) error {
	// Authenticate on the pool bearer resolved from the non-secret pool
	// namespace/UID headers before consuming the body: runtime Pods can reach
	// this endpoint, so an unauthenticated peer must be rejected without being
	// allowed to stream — or drip-feed a declared-length — request body and
	// occupy controller handlers.
	poolNamespace := strings.TrimSpace(string(c.Request().Header.Peek(harnessv2.MCPBrokerPoolNamespaceHeader)))
	poolUID := strings.TrimSpace(string(c.Request().Header.Peek(harnessv2.MCPBrokerPoolUIDHeader)))
	if poolNamespace == "" || poolUID == "" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	pool, secret, err := s.resolveArtifactRuntimePoolByIdentity(c.Context(), poolNamespace, poolUID)
	if err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(string(c.Request().Header.Peek("Authorization")), "Bearer "))
	expectedBearer := strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKeyAPI]))
	if !constantAPIStringEqual(bearer, expectedBearer) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	var request acpArtifactAuthorizationRequest
	if err := json.Unmarshal(c.Body(), &request); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request"})
	}
	// The body's pool identity must match the pre-authenticated headers so a
	// valid bearer for one pool cannot authorize an artifact for another.
	if request.Namespace != poolNamespace || string(request.Metadata.Fence.RuntimePoolUID) != poolUID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	now := time.Now().UTC()
	if request.Namespace == "" || request.Metadata.PromptID == "" || request.Metadata.TaskUID == "" ||
		request.Artifact.MediaType != artifactcap.MediaTypeWorkspaceDelta || request.Artifact.Validate() != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request"})
	}
	if got, err := harnessv2.CanonicalRequestDigest(request); err != nil || got != request.Metadata.RequestDigest {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request"})
	}
	capabilitySecret := secret.Data[runtimePoolCapabilitySecretKeyAPI]
	// Verify with a fresh timestamp: runtime-pool resolution above performs
	// Kubernetes I/O, and a capability that expired while it ran must not be
	// accepted against the stale pre-resolution clock.
	if err := harnessv2.VerifyOperationCapability(capabilitySecret, string(c.Request().Header.Peek(harnessv2.OperationCapabilityHeader)), request.Metadata, true, time.Now().UTC()); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	if pool.Status.ActiveInstance == nil || pool.Status.ActiveInstance.RuntimeInstanceID != string(request.Metadata.Fence.RuntimeInstanceID) {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "stale_runtime"})
	}
	task, err := findTaskByUIDWithReader(c.Context(), s.authorizationReader(), request.Namespace, string(request.Metadata.TaskUID))
	if err != nil || task.Status.Execution == nil || task.Status.Execution.PromptID != string(request.Metadata.PromptID) ||
		task.Status.Execution.RuntimeSessionUID != string(request.Metadata.Fence.RuntimeSessionUID) ||
		task.Status.Execution.RuntimeInstanceID != string(request.Metadata.Fence.RuntimeInstanceID) ||
		(task.Status.Execution.State != corev1alpha1.TaskExecutionStateRunning && task.Status.Execution.State != corev1alpha1.TaskExecutionStateSettling) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	artifactSecret, err := readACPArtifactCapabilitySecret()
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
	}
	binding := artifactcap.OperationRequest{
		Operation: artifactcap.OperationUpload, ObjectDigest: request.Artifact.Digest,
		Identity:      artifactcap.Identity{Namespace: request.Namespace, TaskID: string(request.Metadata.TaskUID)},
		ContentLength: request.Artifact.SizeBytes, MediaType: request.Artifact.MediaType,
		OperationID: "runtime-delta-upload-" + string(request.Metadata.OperationID),
	}
	const capabilityTTL = 2 * time.Minute
	authorization, err := artifactcap.Issue(artifactSecret, binding, now, capabilityTTL)
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
	}
	if s.config.ArtifactReservations == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
	}
	if err := s.config.ArtifactReservations.Reserve(c.Context(), binding, now.Add(capabilityTTL+artifactcap.MaxClockSkew)); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
	}
	return c.JSON(acpArtifactAuthorizationResponse{Capability: authorization.Capability, RequestDigest: authorization.RequestDigest})
}

const (
	runtimePoolControllerTokenKeyAPI  = "controller-token"
	runtimePoolCapabilitySecretKeyAPI = "capability-secret"
)

func (s *Server) authorizationReader() client.Reader {
	if s.config.APIReader != nil {
		return s.config.APIReader
	}
	return s.client
}

// resolveArtifactRuntimePoolByIdentity resolves the RuntimePool and its exact
// active-instance controller-auth Secret from a non-secret pool namespace and
// UID. It is called with header-supplied identity so the caller's bearer can be
// authenticated before the request body is read.
func (s *Server) resolveArtifactRuntimePoolByIdentity(ctx context.Context, poolNamespace, poolUID string) (*corev1alpha1.RuntimePool, *corev1.Secret, error) {
	reader := s.authorizationReader()
	var pools corev1alpha1.RuntimePoolList
	if err := reader.List(ctx, &pools, client.InNamespace(poolNamespace)); err != nil {
		return nil, nil, err
	}
	var pool *corev1alpha1.RuntimePool
	for i := range pools.Items {
		candidate := &pools.Items[i]
		if string(candidate.UID) == poolUID {
			pool = candidate.DeepCopy()
			break
		}
	}
	if pool == nil || pool.Status.ActiveInstance == nil {
		return nil, nil, fmt.Errorf("runtime pool not found")
	}
	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(pool.Status.ActiveInstance.PodNamespace), client.MatchingLabels{
		"orka.ai/runtime-pool-auth": "true", "orka.ai/runtime-pool-uid": string(pool.UID),
	}); err != nil {
		return nil, nil, err
	}
	// During graceful epoch replacement both the draining instance's Secret
	// and the next epoch's Secret exist; select the one mounted by the
	// pool's exact active instance instead of requiring one Secret globally.
	suffix := "auth-e" + strconv.FormatInt(pool.Status.ActiveInstance.ControllerEpoch, 10)
	var matched []*corev1.Secret
	for i := range secrets.Items {
		if strings.HasSuffix(secrets.Items[i].Name, suffix) {
			matched = append(matched, &secrets.Items[i])
		}
	}
	if len(matched) != 1 {
		return nil, nil, fmt.Errorf("runtime pool auth secret is ambiguous for controller epoch %d", pool.Status.ActiveInstance.ControllerEpoch)
	}
	return pool, matched[0].DeepCopy(), nil
}

func (s *Server) findTaskByUID(ctx context.Context, namespace, uid string) (*corev1alpha1.Task, error) {
	return findTaskByUIDWithReader(ctx, s.authorizationReader(), namespace, uid)
}

func findTaskByUIDWithReader(ctx context.Context, reader client.Reader, namespace, uid string) (*corev1alpha1.Task, error) {
	var tasks corev1alpha1.TaskList
	if err := reader.List(ctx, &tasks, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range tasks.Items {
		if string(tasks.Items[i].UID) == uid {
			return tasks.Items[i].DeepCopy(), nil
		}
	}
	return nil, fmt.Errorf("task not found")
}

func readACPArtifactCapabilitySecret() ([]byte, error) {
	path := strings.TrimSpace(os.Getenv(envACPArtifactSecretFile))
	if path == "" {
		return nil, fmt.Errorf("artifact capability secret is not configured")
	}
	return readACPArtifactCapabilitySecretFile(path)
}

func readACPArtifactCapabilitySecretFile(path string) ([]byte, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("artifact capability secret is unavailable")
	}
	value = []byte(strings.TrimSpace(string(value)))
	if len(value) < artifactcap.MinSecretBytes {
		return nil, fmt.Errorf("artifact capability secret is unavailable")
	}
	return value, nil
}

func constantAPIStringEqual(left, right string) bool {
	if len(left) != len(right) || left == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

const envWorkspacePublisherControllerTokenFile = "ORKA_WORKSPACE_PUBLISHER_CONTROLLER_TOKEN_FILE"

func (s *Server) issuePublisherArtifactAuthorization(c fiber.Ctx) error {
	// Authenticate from headers before consuming the body: runtime Pods can
	// reach this endpoint, so unauthenticated peers must be rejected without
	// being allowed to stream request bodies.
	expectedBearer, err := readSecretAtEnvPath(envWorkspacePublisherControllerTokenFile, 16)
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_authorization_unavailable"})
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(string(c.Request().Header.Peek("Authorization")), "Bearer "))
	if !constantAPIStringEqual(bearer, string(expectedBearer)) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	var request publisherservice.ArtifactAuthorizationRequest
	decoder := json.NewDecoder(strings.NewReader(string(c.Body())))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) == nil || request.Validate() != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request"})
	}
	if err := s.authorizePublisherArtifactRequest(c.Context(), request); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	if err := s.authorizePublisherParentEffect(c.Context(), request.ParentOperation, request.Metadata); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	artifactSecret, err := readACPArtifactCapabilitySecret()
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
	}
	binding, err := publisherservice.ArtifactBinding(request)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request"})
	}
	now := time.Now().UTC()
	const capabilityTTL = 2 * time.Minute
	authorization, err := artifactcap.Issue(artifactSecret, binding, now, capabilityTTL)
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
	}
	if s.config.ArtifactReservations == nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
	}
	if err := s.config.ArtifactReservations.Reserve(c.Context(), binding, now.Add(capabilityTTL+artifactcap.MaxClockSkew)); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
	}
	return c.JSON(publisherservice.ArtifactAuthorizationResponse{
		Capability: authorization.Capability, RequestDigest: authorization.RequestDigest,
	})
}

func (s *Server) authorizePublisherArtifactRequest(ctx context.Context, request publisherservice.ArtifactAuthorizationRequest) error {
	switch request.ParentOperation {
	case publisherservice.OperationWorkspacePrepare:
		return s.authorizePublisherWorkspaceUpload(ctx, request)
	case publisherservice.OperationPublicationPrepare:
		if request.ArtifactOperation == artifactcap.OperationUpload {
			return s.authorizePublisherBundleUpload(ctx, request)
		}
		return s.authorizePublisherDeltaDownload(ctx, request)
	case publisherservice.OperationPublicationPublish, publisherservice.OperationPublicationVerify:
		return s.authorizePublisherBundleDownload(ctx, request)
	default:
		return fmt.Errorf("unsupported publisher artifact operation")
	}
}

func (s *Server) authorizePublisherWorkspaceUpload(ctx context.Context, request publisherservice.ArtifactAuthorizationRequest) error {
	if request.ArtifactOperation != artifactcap.OperationUpload || request.Metadata.TaskID == "" || request.Metadata.PublicationID != "" {
		return fmt.Errorf("workspace artifact identity is invalid")
	}
	task, err := s.findTaskByUID(ctx, request.Metadata.Namespace, request.Metadata.TaskID)
	if err != nil || task.Status.Execution == nil {
		return fmt.Errorf("workspace Task is unavailable")
	}
	execution := task.Status.Execution
	// Task status and the external-effect lease are committed through separate
	// Kubernetes objects. Under concurrent workspace preparation, a fresh LIST
	// can briefly observe the preceding Task state after the exact parent effect
	// is already durably InFlight. Bind the immutable prompt identity here and
	// let authorizePublisherParentEffect enforce the live current-epoch lease.
	if execution.PromptID == "" || request.Metadata.OperationID != "workspace-prepare-"+execution.PromptID {
		return fmt.Errorf("workspace Task is not bound to the exact preparation operation")
	}
	return nil
}

func (s *Server) authorizePublisherDeltaDownload(ctx context.Context, request publisherservice.ArtifactAuthorizationRequest) error {
	if request.ArtifactOperation != artifactcap.OperationDownload || request.Metadata.PublicationID == "" || request.Metadata.TaskID != "" {
		return fmt.Errorf("publication artifact identity is invalid")
	}
	publication, err := s.findPublicationByID(ctx, request.Metadata.Namespace, request.Metadata.PublicationID)
	if err != nil || string(publication.Status.State) != string(store.PublicationPreparing) {
		return fmt.Errorf("publication is not preparing")
	}
	if publication.Spec.ArtifactID != string(request.Artifact.ArtifactID) ||
		publication.Spec.ArtifactDigest != request.Artifact.Digest ||
		publication.Spec.ArtifactSizeBytes != request.Artifact.SizeBytes ||
		publication.Spec.ArtifactMediaType != request.Artifact.MediaType {
		return fmt.Errorf("publication artifact identity drifted")
	}
	return nil
}

func (s *Server) authorizePublisherBundleUpload(ctx context.Context, request publisherservice.ArtifactAuthorizationRequest) error {
	if request.ArtifactOperation != artifactcap.OperationUpload || request.Artifact.MediaType != artifactcap.MediaTypeGitBundle ||
		request.Metadata.PublicationID == "" || request.Metadata.TaskID != "" {
		return fmt.Errorf("prepared bundle upload identity is invalid")
	}
	publication, err := s.findPublicationByID(ctx, request.Metadata.Namespace, request.Metadata.PublicationID)
	if err != nil || string(publication.Status.State) != string(store.PublicationPreparing) || publication.Status.PreparedReceipt != nil {
		return fmt.Errorf("publication is not accepting a prepared bundle")
	}
	return nil
}

func (s *Server) authorizePublisherBundleDownload(ctx context.Context, request publisherservice.ArtifactAuthorizationRequest) error {
	if request.ArtifactOperation != artifactcap.OperationDownload || request.Artifact.MediaType != artifactcap.MediaTypeGitBundle ||
		request.Metadata.PublicationID == "" || request.Metadata.TaskID != "" {
		return fmt.Errorf("prepared bundle download identity is invalid")
	}
	publication, err := s.findPublicationByID(ctx, request.Metadata.Namespace, request.Metadata.PublicationID)
	if err != nil || publication.Status.PreparedReceipt == nil {
		return fmt.Errorf("publication prepared receipt is unavailable")
	}
	expectedState := store.PublicationPublishing
	if request.ParentOperation == publisherservice.OperationPublicationVerify {
		expectedState = store.PublicationVerifying
	}
	receipt := publication.Status.PreparedReceipt
	if string(publication.Status.State) != string(expectedState) || receipt.BundleArtifactID != string(request.Artifact.ArtifactID) ||
		receipt.BundleDigest != request.Artifact.Digest || receipt.BundleSizeBytes != request.Artifact.SizeBytes ||
		receipt.BundleMediaType != request.Artifact.MediaType {
		return fmt.Errorf("publication prepared bundle identity drifted")
	}
	return nil
}

func (s *Server) findPublicationByID(ctx context.Context, namespace, id string) (*corev1alpha1.Publication, error) {
	var publications corev1alpha1.PublicationList
	if err := s.authorizationReader().List(ctx, &publications, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range publications.Items {
		if publications.Items[i].Spec.ID == id {
			return publications.Items[i].DeepCopy(), nil
		}
	}
	return nil, fmt.Errorf("publication not found")
}

func readSecretAtEnvPath(name string, minimum int) ([]byte, error) {
	path := strings.TrimSpace(os.Getenv(name))
	if path == "" {
		return nil, fmt.Errorf("%s is not configured", name)
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	value = []byte(strings.TrimSpace(string(value)))
	if len(value) < minimum {
		return nil, fmt.Errorf("%s is unavailable", name)
	}
	return value, nil
}

func (s *Server) authorizePublisherParentEffect(
	ctx context.Context,
	operation publisherservice.Operation,
	metadata publisherservice.OperationMetadata,
) error {
	if s.config.ControllerEpochs == nil || s.config.ExternalEffects == nil {
		return fmt.Errorf("publisher controller epoch authority is unavailable")
	}
	currentFence, err := s.config.ControllerEpochs.CurrentFence(ctx)
	if err != nil || strings.TrimSpace(currentFence.Name) == "" || currentFence.Epoch <= 0 || strings.TrimSpace(currentFence.HolderID) == "" {
		return fmt.Errorf("publisher controller epoch authority is unavailable")
	}
	kind := ""
	aggregateID := metadata.PublicationID
	switch operation {
	case publisherservice.OperationWorkspaceResolve:
		kind, aggregateID = "workspace.resolve", metadata.TaskID
	case publisherservice.OperationWorkspacePrepare:
		kind, aggregateID = "workspace.prepare", metadata.TaskID
	case publisherservice.OperationPublicationPreflight:
		kind = "publisher.preflight"
	case publisherservice.OperationPublicationPrepare:
		kind = "publisher.prepare"
	case publisherservice.OperationPublicationPublish:
		kind = "publisher.publish"
	case publisherservice.OperationPublicationVerify:
		kind = "publisher.verify"
	case publisherservice.OperationPullRequestReconcile:
		kind = "publisher.pull-request"
	default:
		return fmt.Errorf("unsupported Publisher parent operation")
	}
	if aggregateID == "" || metadata.OperationID == "" {
		return fmt.Errorf("publisher parent effect identity is incomplete")
	}
	identity := store.ExternalEffectIdentity{
		Kind: kind, Namespace: metadata.Namespace, AggregateID: aggregateID, OperationID: metadata.OperationID,
	}
	effect, err := s.config.ExternalEffects.GetExternalEffectByIdentity(ctx, identity)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if effect.Identity != identity || effect.State != store.ExternalEffectInFlight ||
		!externalEffectLeaseActive(effect, currentFence, now) {
		return fmt.Errorf("publisher parent effect is not exactly in flight")
	}
	return nil
}

// externalEffectLeaseActive reports whether an in-flight external effect still
// holds a live lease under the controller's current durable fence. The
// publisher broker paths authenticate on a shared bearer with no per-request
// epoch capability, so a lease from a superseded controller epoch must stop
// authorizing broker access immediately even when its wall-clock expiry has not
// elapsed yet.
func externalEffectLeaseActive(effect *store.ExternalEffect, fence store.ControllerEpochFence, now time.Time) bool {
	if effect == nil || strings.TrimSpace(effect.LeaseOwner) == "" || effect.ControllerEpoch <= 0 {
		return false
	}
	return effect.ControllerEpochName == fence.Name && effect.ControllerEpoch == fence.Epoch &&
		effect.LeaseExpiresAt != nil && effect.LeaseExpiresAt.After(now)
}
