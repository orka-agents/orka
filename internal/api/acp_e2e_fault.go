package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	e2ePromptWriteFaultLabel      = "orka.ai/e2e-prompt-write-ambiguity"
	e2ePromptWriteFaultLabelValue = "true"
	e2eFaultErrorKey              = "error"
	e2eFaultAuthorizationFailed   = "authorization_failed"
	e2eFaultInvalidRequest        = "invalid_request"
	e2eFaultStaleRuntime          = "stale_runtime"
)

func (s *Server) installACPE2EPromptWriteFaultRecorder() {
	if !s.config.E2EPromptFaultEnabled {
		return
	}
	s.app.Post(harnessv2.E2EPromptWriteAmbiguityRecordPath, s.recordACPE2EPromptWriteFault)
}

func (s *Server) recordACPE2EPromptWriteFault(c fiber.Ctx) error {
	poolNamespace := strings.TrimSpace(string(c.Request().Header.Peek(harnessv2.MCPBrokerPoolNamespaceHeader)))
	poolUID := strings.TrimSpace(string(c.Request().Header.Peek(harnessv2.MCPBrokerPoolUIDHeader)))
	if poolNamespace == "" || poolUID == "" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultAuthorizationFailed})
	}
	pool, secret, err := s.resolveArtifactRuntimePoolByIdentity(c.Context(), poolNamespace, poolUID)
	if err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultAuthorizationFailed})
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(string(c.Request().Header.Peek("Authorization")), "Bearer "))
	expectedBearer := strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKeyAPI]))
	if !constantAPIStringEqual(bearer, expectedBearer) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultAuthorizationFailed})
	}

	var request harnessv2.E2EPromptWriteAmbiguityRecordRequest
	if err := json.Unmarshal(c.Body(), &request); err != nil || request.ValidateAt(time.Now().UTC()) != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultInvalidRequest})
	}
	if request.Namespace != poolNamespace || string(request.Metadata.Fence.RuntimePoolUID) != poolUID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultAuthorizationFailed})
	}
	if got, err := harnessv2.CanonicalRequestDigest(request); err != nil || got != request.Metadata.RequestDigest {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultInvalidRequest})
	}
	if err := harnessv2.VerifyOperationCapability(
		secret.Data[runtimePoolCapabilitySecretKeyAPI],
		string(c.Request().Header.Peek(harnessv2.OperationCapabilityHeader)),
		request.Metadata,
		true,
		time.Now().UTC(),
	); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultAuthorizationFailed})
	}
	if pool.Status.ActiveInstance == nil {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultStaleRuntime})
	}
	profileSchemaVersion, err := strconv.ParseUint(pool.Status.ActiveInstance.ProfileDigestSchemaVersion, 10, 32)
	if err != nil {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultStaleRuntime})
	}
	expectedFence := harnessv2.Fence{
		RuntimeInstanceID:          harnessv2.RuntimeInstanceID(pool.Status.ActiveInstance.RuntimeInstanceID),
		SupervisorBootID:           harnessv2.SupervisorBootID(pool.Status.ActiveInstance.BootID),
		ControllerEpoch:            uint64(pool.Status.ActiveInstance.ControllerEpoch),
		RuntimePoolUID:             harnessv2.RuntimePoolUID(pool.UID),
		RuntimePoolGeneration:      uint64(pool.Generation),
		RuntimeProfileDigest:       harnessv2.ProfileDigest(pool.Status.ActiveInstance.ProfileDigest),
		ProfileDigestSchemaVersion: uint32(profileSchemaVersion),
	}
	if harnessv2.CompareFence(expectedFence, request.Metadata.Fence, false) != harnessv2.FenceMatch {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultStaleRuntime})
	}
	task, err := findTaskByUIDWithReader(c.Context(), s.authorizationReader(), request.Namespace, string(request.Metadata.TaskUID))
	expectedOperationID := harnessv2.OperationID("start-prompt-" + string(request.Metadata.PromptID))
	if err != nil || task.Status.Execution == nil ||
		task.Status.Execution.State != corev1alpha1.TaskExecutionStateSubmitting ||
		task.Status.Execution.PromptID != string(request.Metadata.PromptID) ||
		task.Status.Execution.RuntimeInstanceID != string(request.Metadata.Fence.RuntimeInstanceID) ||
		task.Status.Execution.RuntimeSessionUID != string(request.Metadata.Fence.RuntimeSessionUID) ||
		task.Status.Execution.RuntimeSessionGeneration != int64(request.Metadata.Fence.RuntimeSessionGeneration) ||
		request.PromptOperationID != expectedOperationID {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{e2eFaultErrorKey: e2eFaultAuthorizationFailed})
	}

	inject, err := s.consumeACPE2EPromptWriteFault(c.Context(), pool, request.PromptOperationID)
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{e2eFaultErrorKey: "fault_record_unavailable"})
	}
	return c.JSON(harnessv2.E2EPromptWriteAmbiguityRecordResponse{Inject: inject})
}

func (s *Server) consumeACPE2EPromptWriteFault(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	operationID harnessv2.OperationID,
) (bool, error) {
	operationDigest := sha256.Sum256([]byte(operationID))
	operationDigestText := "sha256:" + hex.EncodeToString(operationDigest[:])
	keyDigest := sha256.Sum256([]byte(string(pool.UID) + "\x00" + string(operationID)))
	name := "orka-e2e-prompt-fault-" + hex.EncodeToString(keyDigest[:20])
	immutable := true
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pool.Namespace,
			Name:      name,
			Labels: map[string]string{
				e2ePromptWriteFaultLabel: e2ePromptWriteFaultLabelValue,
				runtimePoolUIDLabelAPI:   string(pool.UID),
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(pool, corev1alpha1.GroupVersion.WithKind("RuntimePool")),
			},
		},
		Immutable: &immutable,
		Data: map[string]string{
			"operationDigest": operationDigestText,
			"runtimePoolUID":  string(pool.UID),
		},
	}
	if err := s.client.Create(ctx, record); err == nil {
		return true, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return false, err
	}

	existing := &corev1.ConfigMap{}
	if err := s.authorizationReader().Get(ctx, client.ObjectKeyFromObject(record), existing); err != nil {
		return false, err
	}
	owner := metav1.GetControllerOf(existing)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.Kind != "RuntimePool" ||
		owner.Name != pool.Name || owner.UID != pool.UID || existing.Immutable == nil || !*existing.Immutable ||
		existing.Labels[e2ePromptWriteFaultLabel] != e2ePromptWriteFaultLabelValue ||
		existing.Labels[runtimePoolUIDLabelAPI] != string(pool.UID) ||
		existing.Data["operationDigest"] != operationDigestText || existing.Data["runtimePoolUID"] != string(pool.UID) {
		return false, apierrors.NewConflict(corev1.Resource("configmaps"), name, errors.New("existing fault record does not match the requested operation"))
	}
	return false, nil
}
