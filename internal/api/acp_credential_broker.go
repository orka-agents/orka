package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

const (
	defaultWorkspaceCredentialKey = "token"
	maxBrokeredCredentialBytes    = 32 << 10
)

func (s *Server) issuePublisherCredential(c fiber.Ctx) error {
	// Authenticate from headers before consuming the body: runtime Pods can
	// reach this endpoint, so unauthenticated peers must be rejected without
	// being allowed to stream request bodies.
	expectedBearer, err := readSecretAtEnvPath(envWorkspacePublisherControllerTokenFile, 16)
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "credential_broker_unavailable"})
	}
	bearer := strings.TrimSpace(strings.TrimPrefix(string(c.Request().Header.Peek("Authorization")), "Bearer "))
	if !constantAPIStringEqual(bearer, string(expectedBearer)) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	var request publisherservice.CredentialMaterialRequest
	decoder := json.NewDecoder(strings.NewReader(string(c.Body())))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) == nil || request.Validate() != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_request"})
	}
	task, reference, frozenResourceVersion, credentialRole, err := s.authorizePublisherCredentialRequest(c.Context(), request)
	if err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	if err := s.authorizePublisherParentEffect(c.Context(), request.ParentOperation, request.Metadata); err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	binding, err := s.frozenPromptCredentialBinding(c.Context(), task, credentialRole)
	if err != nil || binding.SecretName != reference.Name || binding.SecretKey != effectiveWorkspaceCredentialKey(reference) ||
		binding.ResourceVersion != frozenResourceVersion {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "authorization_failed"})
	}
	secretReader := client.Reader(s.client)
	if s.config.APIReader != nil {
		secretReader = s.config.APIReader
	}
	secret := &corev1.Secret{}
	if err := secretReader.Get(c.Context(), client.ObjectKey{Namespace: task.Namespace, Name: reference.Name}, secret); err != nil ||
		secret.ResourceVersion == "" || secret.ResourceVersion != frozenResourceVersion || string(secret.UID) != binding.SecretUID {
		return c.Status(fiber.StatusGone).JSON(fiber.Map{"error": "credential_version_changed"})
	}
	key := effectiveWorkspaceCredentialKey(reference)
	value := bytes.TrimSpace(secret.Data[key])
	if len(value) == 0 || len(value) > maxBrokeredCredentialBytes || bytes.ContainsAny(value, "\r\n\x00") {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "credential_material_invalid"})
	}
	material, err := formatPublisherCredential(request.Reference.Kind, value)
	if err != nil {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "credential_material_invalid"})
	}
	return c.JSON(publisherservice.CredentialMaterialResponse{Material: material, ResourceVersion: secret.ResourceVersion})
}

func (s *Server) authorizePublisherCredentialRequest(
	ctx context.Context,
	request publisherservice.CredentialMaterialRequest,
) (*corev1alpha1.Task, *corev1alpha1.WorkspaceCredentialReference, string, string, error) {
	if request.ParentOperation == publisherservice.OperationWorkspaceResolve || request.ParentOperation == publisherservice.OperationWorkspacePrepare {
		return s.authorizeWorkspaceReadCredential(ctx, request)
	}
	task, reference, frozenResourceVersion, credentialRole, err := s.authorizePublicationCredential(ctx, request)
	return task, reference, frozenResourceVersion, credentialRole, err
}

func (s *Server) authorizeWorkspaceReadCredential(
	ctx context.Context,
	request publisherservice.CredentialMaterialRequest,
) (*corev1alpha1.Task, *corev1alpha1.WorkspaceCredentialReference, string, string, error) {
	task, err := s.findTaskByUID(ctx, request.Metadata.Namespace, request.Metadata.TaskID)
	if err != nil || task.Status.Execution == nil || task.Spec.Workspace == nil {
		return nil, nil, "", "", fmt.Errorf("workspace read credential is unavailable")
	}
	execution := task.Status.Execution
	operationPrefix := "workspace-resolve-"
	if request.ParentOperation == publisherservice.OperationWorkspacePrepare {
		operationPrefix = "workspace-prepare-"
	}
	if !taskStateAllowsWorkspacePreparation(execution.State) ||
		request.Metadata.PublicationID != "" ||
		request.Metadata.OperationID != operationPrefix+execution.PromptID || request.Reference.Kind != publisherservice.CredentialHTTPExtraHeader {
		return nil, nil, "", "", fmt.Errorf("workspace read credential request is not bound to the planned task")
	}
	workspace := task.Spec.Workspace
	var reference *corev1alpha1.WorkspaceCredentialReference
	var frozenResourceVersion string
	switch request.Reference.Role {
	case "", publisherservice.CredentialRoleSourceRead:
		reference = workspace.ReadCredentialRef
		frozenResourceVersion = execution.ReadCredentialResourceVersion
	case publisherservice.CredentialRoleTargetRead:
		reference = workspace.PublicationReadCredentialRef
		frozenResourceVersion = execution.PublicationReadCredentialResourceVersion
	default:
		return nil, nil, "", "", fmt.Errorf("workspace read credential role is invalid")
	}
	if reference == nil || request.Reference.Name != reference.Name || frozenResourceVersion == "" {
		return nil, nil, "", "", fmt.Errorf("workspace read credential request is not bound to the planned task")
	}
	role := request.Reference.Role
	if role == "" {
		role = publisherservice.CredentialRoleSourceRead
	}
	return task, reference, frozenResourceVersion, string(role), nil
}

func (s *Server) authorizePublicationCredential(
	ctx context.Context,
	request publisherservice.CredentialMaterialRequest,
) (*corev1alpha1.Task, *corev1alpha1.WorkspaceCredentialReference, string, string, error) {
	task, publication, err := s.taskAndPublicationForCredential(ctx, request)
	if err != nil || task.Status.Execution == nil || task.Spec.Workspace == nil {
		return nil, nil, "", "", fmt.Errorf("publication credential is unavailable")
	}
	execution := task.Status.Execution
	if execution.State != corev1alpha1.TaskExecutionStateSucceeded {
		return nil, nil, "", "", fmt.Errorf("publication credential request is not bound to the settled task")
	}
	kind, expectedState, expectedCredentialKind := publicationCredentialOperation(request.ParentOperation)
	credentialRole := request.Reference.Role
	if credentialRole == "" {
		credentialRole = publisherservice.CredentialRole(promptCredentialRoleForOperation(request.ParentOperation))
	}
	if request.ParentOperation == publisherservice.OperationPublicationPrepare {
		if credentialRole != publisherservice.CredentialRoleSourceRead && credentialRole != publisherservice.CredentialRoleTargetRead {
			return nil, nil, "", "", fmt.Errorf("publication credential role is invalid for the requested operation")
		}
	} else if credentialRole != publisherservice.CredentialRole(promptCredentialRoleForOperation(request.ParentOperation)) {
		return nil, nil, "", "", fmt.Errorf("publication credential role is invalid for the requested operation")
	}
	reference, frozenVersion := publicationCredentialBinding(task, request.ParentOperation, credentialRole)
	if kind == "" || reference == nil || frozenVersion == "" || request.Reference.Kind != expectedCredentialKind ||
		request.Reference.Name != reference.Name || request.Metadata.OperationID != controller.ACPPublicationOperationID(kind, task) {
		return nil, nil, "", "", fmt.Errorf("publication credential operation identity is invalid")
	}
	if publication != nil && string(publication.Status.State) != string(expectedState) {
		return nil, nil, "", "", fmt.Errorf("publication credential state is invalid")
	}
	return task, reference, frozenVersion, string(credentialRole), nil
}

func publicationCredentialBinding(
	task *corev1alpha1.Task,
	operation publisherservice.Operation,
	role publisherservice.CredentialRole,
) (*corev1alpha1.WorkspaceCredentialReference, string) {
	if task == nil || task.Status.Execution == nil || task.Spec.Workspace == nil {
		return nil, ""
	}
	workspace := task.Spec.Workspace
	execution := task.Status.Execution
	switch operation {
	case publisherservice.OperationPublicationPreflight, publisherservice.OperationPublicationVerify:
		return workspace.PublicationReadCredentialRef, execution.PublicationReadCredentialResourceVersion
	case publisherservice.OperationPublicationPrepare:
		if role == publisherservice.CredentialRoleTargetRead {
			return workspace.PublicationReadCredentialRef, execution.PublicationReadCredentialResourceVersion
		}
		return workspace.ReadCredentialRef, execution.ReadCredentialResourceVersion
	case publisherservice.OperationPublicationPublish:
		return workspace.PublicationCredentialRef, execution.PublicationCredentialResourceVersion
	case publisherservice.OperationPullRequestReconcile:
		return workspace.ForgeCredentialRef, execution.ForgeCredentialResourceVersion
	default:
		return nil, ""
	}
}

func (s *Server) taskAndPublicationForCredential(
	ctx context.Context,
	request publisherservice.CredentialMaterialRequest,
) (*corev1alpha1.Task, *corev1alpha1.Publication, error) {
	if request.ParentOperation == publisherservice.OperationPublicationPreflight {
		var tasks corev1alpha1.TaskList
		if err := s.authorizationReader().List(ctx, &tasks, client.InNamespace(request.Metadata.Namespace)); err != nil {
			return nil, nil, err
		}
		for i := range tasks.Items {
			candidate := &tasks.Items[i]
			if candidate.Status.Execution != nil && controller.ACPPublicationIDForTask(candidate) == request.Metadata.PublicationID {
				return candidate.DeepCopy(), nil, nil
			}
		}
		return nil, nil, fmt.Errorf("publication Task not found")
	}
	publication, err := s.findPublicationByID(ctx, request.Metadata.Namespace, request.Metadata.PublicationID)
	if err != nil {
		return nil, nil, err
	}
	task, err := s.findTaskByUID(ctx, request.Metadata.Namespace, publication.Spec.TaskUID)
	return task, publication, err
}

func publicationCredentialOperation(operation publisherservice.Operation) (string, store.PublicationState, publisherservice.CredentialKind) {
	switch operation {
	case publisherservice.OperationPublicationPreflight:
		return "preflight", "", publisherservice.CredentialHTTPExtraHeader
	case publisherservice.OperationPublicationPrepare:
		return "prepare", store.PublicationPreparing, publisherservice.CredentialHTTPExtraHeader
	case publisherservice.OperationPublicationPublish:
		return "publish", store.PublicationPublishing, publisherservice.CredentialHTTPExtraHeader
	case publisherservice.OperationPublicationVerify:
		return "verify", store.PublicationVerifying, publisherservice.CredentialHTTPExtraHeader
	case publisherservice.OperationPullRequestReconcile:
		return "pr-reconcile", store.PublicationVerifying, publisherservice.CredentialForgeToken
	default:
		return "", "", ""
	}
}

func promptCredentialRoleForOperation(operation publisherservice.Operation) string {
	switch operation {
	case publisherservice.OperationWorkspaceResolve, publisherservice.OperationWorkspacePrepare, publisherservice.OperationPublicationPrepare:
		return string(store.PromptCredentialSourceRead)
	case publisherservice.OperationPublicationPreflight, publisherservice.OperationPublicationVerify:
		return string(store.PromptCredentialTargetRead)
	case publisherservice.OperationPublicationPublish:
		return string(store.PromptCredentialTargetWrite)
	case publisherservice.OperationPullRequestReconcile:
		return string(store.PromptCredentialForge)
	default:
		return ""
	}
}

func (s *Server) frozenPromptCredentialBinding(
	ctx context.Context,
	task *corev1alpha1.Task,
	role string,
) (*corev1alpha1.PromptCredentialBinding, error) {
	if task == nil || task.Status.Execution == nil || role == "" {
		return nil, fmt.Errorf("prompt credential binding identity is incomplete")
	}
	var attempts corev1alpha1.PromptAttemptList
	if err := s.authorizationReader().List(ctx, &attempts, client.InNamespace(task.Namespace)); err != nil {
		return nil, err
	}
	var match *corev1alpha1.PromptCredentialBinding
	for i := range attempts.Items {
		attempt := &attempts.Items[i]
		if attempt.Spec.TaskUID != string(task.UID) || attempt.Spec.Attempt != int64(task.Status.Execution.Attempt) ||
			attempt.Spec.PromptID != task.Status.Execution.PromptID {
			continue
		}
		for j := range attempt.Spec.CredentialBindings {
			binding := &attempt.Spec.CredentialBindings[j]
			if binding.Role != role {
				continue
			}
			if match != nil {
				return nil, fmt.Errorf("prompt credential role is ambiguous")
			}
			copy := *binding
			match = &copy
		}
	}
	if match == nil {
		return nil, fmt.Errorf("prompt credential binding was not found")
	}
	return match, nil
}

func effectiveWorkspaceCredentialKey(reference *corev1alpha1.WorkspaceCredentialReference) string {
	if reference == nil || strings.TrimSpace(reference.Key) == "" {
		return defaultWorkspaceCredentialKey
	}
	return strings.TrimSpace(reference.Key)
}

func formatPublisherCredential(kind publisherservice.CredentialKind, value []byte) (string, error) {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return "", fmt.Errorf("credential is empty")
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "authorization:") {
		parts := strings.Fields(strings.TrimSpace(trimmed[len("Authorization:"):]))
		if len(parts) != 2 || parts[1] == "" {
			return "", fmt.Errorf("authorization credential is invalid")
		}
		if kind == publisherservice.CredentialForgeToken {
			if !strings.EqualFold(parts[0], "bearer") {
				return "", fmt.Errorf("forge authorization credential must use Bearer")
			}
			return parts[1], nil
		}
		switch {
		case strings.EqualFold(parts[0], "bearer"):
			return "Authorization: Bearer " + parts[1], nil
		case strings.EqualFold(parts[0], "basic"):
			decoded, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil || !bytes.Contains(decoded, []byte(":")) {
				return "", fmt.Errorf("basic authorization credential is invalid")
			}
			return "Authorization: Basic " + parts[1], nil
		default:
			return "", fmt.Errorf("authorization credential scheme is unsupported")
		}
	}
	if kind == publisherservice.CredentialForgeToken {
		return trimmed, nil
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + trimmed))
	return "Authorization: Basic " + encoded, nil
}
