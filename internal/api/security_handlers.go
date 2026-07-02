package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
)

type CreateRepositoryScanRequest struct {
	Name      string                          `json:"name"`
	Namespace string                          `json:"namespace"`
	Metadata  MetadataRequest                 `json:"metadata"`
	Spec      corev1alpha1.RepositoryScanSpec `json:"spec"`
}

type UpdateRepositoryScanRequest struct {
	Spec corev1alpha1.RepositoryScanSpec `json:"spec"`
}

type UpdateThreatModelRequest struct {
	Content string `json:"content"`
	Source  string `json:"source,omitempty"`
}

const sourceProviderGitHub = "github"

func (h *Handlers) normalizeRepositoryScanSpec(spec *corev1alpha1.RepositoryScanSpec) {
	if spec.Provider == "" {
		spec.Provider = sourceProviderGitHub
	}
	if spec.ValidationMode == "" {
		spec.ValidationMode = "light"
	}
	if spec.Owner == "" || spec.Repository == "" {
		owner, repo := security.ParseRepositoryURL(spec.RepoURL)
		if spec.Owner == "" {
			spec.Owner = owner
		}
		if spec.Repository == "" {
			spec.Repository = repo
		}
	}
	if spec.PRBaseBranch == "" && spec.Branch != "" {
		spec.PRBaseBranch = spec.Branch
	}
}

func (h *Handlers) ensureSecurityStore() error {
	if h.securityStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "security store not configured")
	}
	return nil
}

func (h *Handlers) fetchRepositoryScan(ctx context.Context, namespace, name string) (*corev1alpha1.RepositoryScan, error) {
	scan := &corev1alpha1.RepositoryScan{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, scan); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "repository scan not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get repository scan: %v", err))
	}
	return scan, nil
}

func (h *Handlers) ownerRefForRepositoryScan(scan *corev1alpha1.RepositoryScan) metav1.OwnerReference {
	return *metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))
}

func (h *Handlers) hasActiveSecurityScanPipelineTask(ctx context.Context, scan *corev1alpha1.RepositoryScan) (bool, error) {
	var tasks corev1alpha1.TaskList
	if err := h.client.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{labels.LabelSecurityTarget: labels.SelectorValue(scan.Name)}),
	); err != nil {
		return false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list scan tasks: %v", err))
	}

	for i := range tasks.Items {
		task := &tasks.Items[i]
		stage := strings.TrimSpace(task.Labels[labels.LabelSecurityStage])
		if stage == security.StagePatch || stage == security.StageValidation {
			continue
		}
		switch task.Status.Phase {
		case corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseScheduled:
			return true, nil
		}
	}
	return false, nil
}

func (h *Handlers) updateRepositoryScanRunStatus(ctx context.Context, scan *corev1alpha1.RepositoryScan, scanID, taskName string) error {
	patch := scan.DeepCopy()
	patch.Status.Phase = "Scanning"
	patch.Status.LastScanID = scanID
	patch.Status.LastScanTaskName = taskName
	meta.SetStatusCondition(&patch.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Scanning",
		Message:            "Security scan is running",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: patch.Generation,
	})
	return h.client.Status().Patch(ctx, patch, client.MergeFrom(scan))
}

func (h *Handlers) authorizeContextTokenRepositoryScanPolicyRefs(
	c fiber.Ctx,
	action string,
	namespace string,
	spec corev1alpha1.RepositoryScanSpec,
) error {
	if spec.CustomScanInstructionsRef != nil {
		if err := h.authorizeContextTokenPolicyConfigMapName(c, action+"CustomScanPolicy", namespace, spec.CustomScanInstructionsRef.Name); err != nil {
			return err
		}
	}
	if spec.FalsePositivePolicyRef != nil {
		if err := h.authorizeContextTokenPolicyConfigMapName(c, action+"FalsePositivePolicy", namespace, spec.FalsePositivePolicyRef.Name); err != nil {
			return err
		}
	}
	return nil
}

func authorizeContextTokenRepositoryScanPolicyRefsForUser(
	ui *UserInfo,
	cfg ContextTokenAuthorizationConfig,
	action string,
	namespace string,
	spec corev1alpha1.RepositoryScanSpec,
) error {
	if spec.CustomScanInstructionsRef != nil {
		if err := authorizeContextTokenPolicyConfigMapForUser(ui, cfg, action+"CustomScanPolicy", namespace, spec.CustomScanInstructionsRef.Name); err != nil {
			return err
		}
	}
	if spec.FalsePositivePolicyRef != nil {
		if err := authorizeContextTokenPolicyConfigMapForUser(ui, cfg, action+"FalsePositivePolicy", namespace, spec.FalsePositivePolicyRef.Name); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handlers) createSecurityScanRun(ctx context.Context, ui *UserInfo, scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit string) (*store.ScanRun, error) {
	if err := h.ensureSecurityStore(); err != nil {
		return nil, err
	}

	var threatModel string
	if model, err := h.securityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name); err == nil {
		threatModel = model.Content
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load threat model: %v", err))
	}

	if err := authorizeContextTokenRepositoryScanPolicyRefsForUser(ui, h.contextTokenAuthorization, "createSecurityScanTaskPolicy", scan.Namespace, scan.Spec); err != nil {
		return nil, err
	}
	policy, err := security.LoadScannerPolicy(ctx, h.client, scan.Namespace, scan.Spec)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid repository scan policy: %v", err))
	}
	taskName := security.ScanStageTaskName(scan.Name, mode, security.StageThreatModel, "")
	scanID := security.ScanRunID(taskName)
	idempotencyKey := security.ScanRunIdempotencyKey(scan.Namespace, scan.Name, mode, baseCommit, headCommit, scan.Spec.SubPath, policy.Digest)
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(700)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:        "true",
				labels.LabelCreatedBy:      "repository-security",
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: scanID,
				labels.LabelSecurityMode:   mode,
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
			OwnerReferences: []metav1.OwnerReference{h.ownerRefForRepositoryScan(scan)},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildThreatModelPrompt(scan, mode, baseCommit, headCommit, threatModel, policy.PromptPolicy()),
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StageThreatModel},
				{Name: security.EnvScanID, Value: scanID},
				{Name: security.EnvScannerPolicyVersion, Value: security.ScannerPolicyVersion},
				{Name: security.EnvPolicyDigest, Value: policy.Digest},
				{Name: security.EnvPolicyProvenance, Value: security.PolicyProvenanceEnv(policy)},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveWorkspaceBranch(scan),
					Ref:          security.EffectiveRef(scan),
					GitSecretRef: scan.Spec.GitSecretRef,
					SubPath:      scan.Spec.SubPath,
					ForkRepo:     scan.Spec.ForkRepo,
					PRBaseBranch: scan.Spec.PRBaseBranch,
				},
			},
		},
	}
	if scan.Spec.GitSecretRef != nil {
		if err := authorizeContextTokenGitCredentialSecretForUser(ui, h.contextTokenAuthorization, "createSecurityScanTaskGitSecret", scan.Namespace, scan.Spec.GitSecretRef.Name); err != nil {
			return nil, err
		}
	}
	if err := authorizeAndStampTaskContext(ctx, h.client, h.clientset, contextTokenFromUserInfo(ui), h.contextTokenAuthorization, "createSecurityScanTask", ui, task); err != nil {
		return nil, err
	}
	if err := h.client.Create(ctx, task); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create scan task: %v", err))
	}

	run := &store.ScanRun{
		ID:                   scanID,
		Namespace:            scan.Namespace,
		RepositoryScan:       scan.Name,
		TaskName:             taskName,
		Mode:                 mode,
		Phase:                "pending",
		BaseCommit:           baseCommit,
		HeadCommit:           headCommit,
		ScannerPolicyVersion: security.ScannerPolicyVersion,
		PolicyDigest:         policy.Digest,
		IdempotencyKey:       idempotencyKey,
		StartedAt:            time.Now(),
	}
	if err := h.securityStore.CreateScanRun(ctx, run); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create scan run: %v", err))
	}
	if err := h.updateRepositoryScanRunStatus(ctx, scan, scanID, taskName); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update repository scan status: %v", err))
	}
	return run, nil
}

func (h *Handlers) createSecurityValidationTask(ctx context.Context, ui *UserInfo, scan *corev1alpha1.RepositoryScan, finding *store.Finding) error {
	if err := authorizeContextTokenRepositoryScanPolicyRefsForUser(ui, h.contextTokenAuthorization, "createSecurityValidationTaskPolicy", scan.Namespace, scan.Spec); err != nil {
		return err
	}
	policy, err := security.LoadScannerPolicy(ctx, h.client, scan.Namespace, scan.Spec)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid repository scan policy: %v", err))
	}
	if h.securityStore != nil && strings.TrimSpace(finding.ScanRunID) != "" {
		run, err := h.securityStore.GetScanRun(ctx, scan.Namespace, finding.ScanRunID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load scan run: %v", err))
		}
		if run != nil && (run.Phase == "pending" || run.Phase == "running") && run.PolicyDigest != "" && policy.Digest != "" && run.PolicyDigest != policy.Digest {
			return fiber.NewError(fiber.StatusConflict, "scanner policy changed during active scan run")
		}
	}
	timeout := metav1.Duration{Duration: 90 * time.Minute}
	priority := int32(725)
	taskName := security.ScanStageTaskName(scan.Name, "validation", security.StageValidation, finding.ID)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:           "true",
				labels.LabelCreatedBy:         "repository-security",
				labels.LabelSecurityTarget:    labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID:    finding.ScanRunID,
				labels.LabelSecurityMode:      security.StageValidation,
				labels.LabelSecurityStage:     security.StageValidation,
				labels.LabelSecurityFindingID: finding.ID,
			},
			OwnerReferences: []metav1.OwnerReference{h.ownerRefForRepositoryScan(scan)},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildValidationPrompt(scan, finding, policy.PromptPolicy()),
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StageValidation},
				{Name: security.EnvScanID, Value: finding.ScanRunID},
				{Name: security.EnvPolicyDigest, Value: policy.Digest},
				{Name: security.EnvPolicyProvenance, Value: security.PolicyProvenanceEnv(policy)},
				{Name: security.EnvFindingID, Value: finding.ID},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveWorkspaceBranch(scan),
					Ref:          security.EffectiveRef(scan),
					GitSecretRef: scan.Spec.GitSecretRef,
					SubPath:      scan.Spec.SubPath,
					ForkRepo:     scan.Spec.ForkRepo,
					PRBaseBranch: scan.Spec.PRBaseBranch,
				},
			},
		},
	}
	if scan.Spec.GitSecretRef != nil {
		if err := authorizeContextTokenGitCredentialSecretForUser(ui, h.contextTokenAuthorization, "createSecurityValidationTaskGitSecret", scan.Namespace, scan.Spec.GitSecretRef.Name); err != nil {
			return err
		}
	}
	if err := authorizeAndStampTaskContext(ctx, h.client, h.clientset, contextTokenFromUserInfo(ui), h.contextTokenAuthorization, "createSecurityValidationTask", ui, task); err != nil {
		return err
	}
	if err := h.client.Create(ctx, task); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create validation task: %v", err))
	}
	finding.ValidationStatus = "pending"
	if err := h.securityStore.UpsertFinding(ctx, finding); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update finding: %v", err))
	}
	return nil
}

func securityPatchAgentRef(scan *corev1alpha1.RepositoryScan) corev1alpha1.AgentReference {
	agentRef := scan.Spec.AnalysisAgentRef
	if scan.Spec.PatchAgentRef != nil {
		agentRef = *scan.Spec.PatchAgentRef
	}
	return agentRef
}

func (h *Handlers) createSecurityPatchTask(ctx context.Context, ui *UserInfo, scan *corev1alpha1.RepositoryScan, finding *store.Finding) (*store.PatchProposal, error) {
	if err := h.ensureSecurityStore(); err != nil {
		return nil, err
	}

	agentRef := securityPatchAgentRef(scan)

	taskName := security.PatchTaskName(scan.Name, finding.ID)
	proposalID := security.PatchProposalID(taskName)
	branch := security.PatchBranch(finding.ID, taskName)
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(750)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:           "true",
				labels.LabelCreatedBy:         "repository-security",
				labels.LabelSecurityTarget:    labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID:    proposalID,
				labels.LabelSecurityMode:      "patch",
				labels.LabelSecurityStage:     security.StagePatch,
				labels.LabelSecurityFindingID: finding.ID,
			},
			OwnerReferences: []metav1.OwnerReference{h.ownerRefForRepositoryScan(scan)},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &agentRef,
			Prompt:   security.BuildPatchPrompt(scan, finding, branch),
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: workerenv.RequirePushBranch, Value: "true"},
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StagePatch},
				{Name: security.EnvScanID, Value: proposalID},
				{Name: security.EnvFindingID, Value: finding.ID},
				{Name: security.EnvPatchBranch, Value: branch},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveWorkspaceBranch(scan),
					Ref:          security.EffectiveRef(scan),
					GitSecretRef: scan.Spec.GitSecretRef,
					SubPath:      scan.Spec.SubPath,
					ForkRepo:     scan.Spec.ForkRepo,
					PRBaseBranch: scan.Spec.PRBaseBranch,
					PushBranch:   branch,
				},
			},
		},
	}
	if scan.Spec.GitSecretRef != nil {
		if err := authorizeContextTokenGitCredentialSecretForUser(ui, h.contextTokenAuthorization, "createSecurityPatchTaskGitSecret", scan.Namespace, scan.Spec.GitSecretRef.Name); err != nil {
			return nil, err
		}
	}
	if err := authorizeAndStampTaskContext(ctx, h.client, h.clientset, contextTokenFromUserInfo(ui), h.contextTokenAuthorization, "createSecurityPatchTask", ui, task); err != nil {
		return nil, err
	}
	if err := h.client.Create(ctx, task); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create patch task: %v", err))
	}

	proposal := &store.PatchProposal{
		ID:             proposalID,
		Namespace:      scan.Namespace,
		RepositoryScan: scan.Name,
		FindingID:      finding.ID,
		TaskName:       taskName,
		Branch:         branch,
		Status:         "pending",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := h.securityStore.CreatePatchProposal(ctx, proposal); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create patch proposal: %v", err))
	}
	return proposal, nil
}

// ListRepositoryScans lists configured repository scans.
func (h *Handlers) ListRepositoryScans(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRepositoryScans", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}

	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")
	opts := &client.ListOptions{Namespace: namespace}
	pagination, err := ParsePagination(limit, continueToken)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	opts.Limit = pagination.Limit
	opts.Continue = pagination.Continue

	list := &corev1alpha1.RepositoryScanList{}
	if err := h.client.List(c.Context(), list, opts); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list repository scans: %v", err))
	}

	items := list.Items
	filteredList := false
	if h.contextTokenAuthorization.Enabled() {
		filtered := make([]corev1alpha1.RepositoryScan, 0, len(list.Items))
		for i := range list.Items {
			scan := &list.Items[i]
			if h.contextTokenSecurityScanAllowed(c, scan, scan.Spec.AnalysisAgentRef) {
				filtered = append(filtered, *scan)
			}
		}
		filteredList = len(filtered) != len(list.Items)
		items = filtered
	}
	remainingItemCount := list.RemainingItemCount
	if filteredList {
		remainingItemCount = nil
	}

	return c.JSON(ListResponse{
		Items: items,
		Metadata: ListMeta{
			Continue:           list.Continue,
			RemainingItemCount: remainingItemCount,
		},
	})
}

// GetRepositoryScan returns a repository scan configuration.
func (h *Handlers) GetRepositoryScan(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getRepositoryScan", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "getRepositoryScan", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	return c.JSON(scan)
}

// CreateRepositoryScan creates a new repository scan configuration.
func (h *Handlers) CreateRepositoryScan(c fiber.Ctx) error {
	var req CreateRepositoryScanRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	name := req.Name
	if name == "" {
		name = req.Metadata.Name
	}
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	if req.Spec.RepoURL == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.repoURL is required")
	}
	if req.Spec.AnalysisAgentRef.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.analysisAgentRef.name is required")
	}

	explicitNamespace := req.Namespace
	if explicitNamespace == "" {
		explicitNamespace = req.Metadata.Namespace
	}
	namespace, err := h.resolveNamespace(c, explicitNamespace)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createRepositoryScan", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	h.normalizeRepositoryScanSpec(&req.Spec)
	if err := h.authorizeContextTokenRepositoryScanPolicyRefs(c, "createRepositoryScanPolicy", namespace, req.Spec); err != nil {
		return err
	}
	if _, err := security.LoadScannerPolicy(c.Context(), h.client, namespace, req.Spec); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid repository scan policy: %v", err))
	}

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: objectMetaFromRequest(name, namespace, req.Metadata),
		Spec:       req.Spec,
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "createRepositoryScan", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	if scan.Spec.GitSecretRef != nil {
		if err := h.authorizeContextTokenGitCredentialSecretName(c, "createRepositoryScanGitSecret", namespace, scan.Spec.GitSecretRef.Name); err != nil {
			return err
		}
	}
	if err := h.client.Create(c.Context(), scan); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "repository scan already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create repository scan: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(scan)
}

// UpdateRepositoryScan updates an existing repository scan.
func (h *Handlers) UpdateRepositoryScan(c fiber.Ctx) error {
	var req UpdateRepositoryScanRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "updateRepositoryScan", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "updateRepositoryScan", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}

	if req.Spec.RepoURL == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.repoURL is required")
	}
	if req.Spec.AnalysisAgentRef.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.analysisAgentRef.name is required")
	}

	h.normalizeRepositoryScanSpec(&req.Spec)
	if err := h.authorizeContextTokenRepositoryScanPolicyRefs(c, "updateRepositoryScanPolicy", namespace, req.Spec); err != nil {
		return err
	}
	if _, err := security.LoadScannerPolicy(c.Context(), h.client, namespace, req.Spec); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid repository scan policy: %v", err))
	}
	updated := scan.DeepCopy()
	updated.Spec = req.Spec
	if err := h.authorizeContextTokenSecurityScanTask(c, "updateRepositoryScan", updated, updated.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	if updated.Spec.GitSecretRef != nil {
		if err := h.authorizeContextTokenGitCredentialSecretName(c, "updateRepositoryScanGitSecret", namespace, updated.Spec.GitSecretRef.Name); err != nil {
			return err
		}
	}
	if err := h.client.Update(c.Context(), updated); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update repository scan: %v", err))
	}
	return c.JSON(updated)
}

// DeleteRepositoryScan deletes a repository scan configuration.
func (h *Handlers) DeleteRepositoryScan(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "deleteRepositoryScan", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "deleteRepositoryScan", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), scan); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete repository scan: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// GetThreatModel returns the current threat model for a repository.
func (h *Handlers) GetThreatModel(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getThreatModel", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "getThreatModel", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	model, err := h.securityStore.GetLatestThreatModel(c.Context(), namespace, c.Params("name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "threat model not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get threat model: %v", err))
	}
	return c.JSON(model)
}

// UpdateThreatModel replaces the current threat model.
func (h *Handlers) UpdateThreatModel(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	var req UpdateThreatModelRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.Content) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "content is required")
	}

	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "updateThreatModel", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "updateThreatModel", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}

	model := &store.ThreatModel{
		Namespace:      namespace,
		RepositoryScan: c.Params("name"),
		Content:        req.Content,
		Source:         req.Source,
	}
	if model.Source == "" {
		model.Source = "edited"
	}
	if err := h.securityStore.SaveThreatModel(c.Context(), model); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save threat model: %v", err))
	}
	return c.JSON(model)
}

// ListSecurityScanRuns lists stored scan runs for a repository.
func (h *Handlers) ListSecurityScanRuns(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSecurityScanRuns", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "listSecurityScanRuns", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}

	limit, err := strconv.Atoi(c.Query("limit", "20"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	runs, next, err := h.securityStore.ListScanRuns(c.Context(), namespace, c.Params("name"), limit, c.Query("cursor"))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list scan runs: %v", err))
	}
	return c.JSON(fiber.Map{"items": runs, "metadata": fiber.Map{"continue": next}})
}

// CreateManualSecurityScan creates and starts a manual scan task.
func (h *Handlers) CreateManualSecurityScan(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createManualSecurityScan", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "createManualSecurityScan", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	active, err := h.hasActiveSecurityScanPipelineTask(c.Context(), scan)
	if err != nil {
		return err
	}
	if active {
		return fiber.NewError(fiber.StatusConflict, "a security scan is already running for this repository")
	}
	run, err := h.createSecurityScanRun(c.Context(), GetUserInfo(c), scan, "manual", scan.Status.LastProcessedCommit, "")
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(run)
}

// ListSecurityFindings lists findings for a repository.
func (h *Handlers) ListSecurityFindings(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSecurityFindings", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "listSecurityFindings", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}

	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	findings, next, err := h.securityStore.ListFindings(c.Context(), store.FindingFilter{
		Namespace:        namespace,
		RepositoryScan:   c.Params("name"),
		SliceID:          c.Query("sliceID"),
		Category:         c.Query("category"),
		Severity:         c.Query("severity"),
		ValidationStatus: c.Query("validationStatus"),
		State:            c.Query("state"),
		Recommended:      c.Query("recommended") == queryTrue,
		Limit:            limit,
		Cursor:           c.Query("cursor"),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list findings: %v", err))
	}
	if findings == nil {
		findings = []store.Finding{}
	}

	for i := range findings {
		if findings[i].ScanRunID == "" {
			continue
		}
		if run, err := h.securityStore.GetScanRun(c.Context(), namespace, findings[i].ScanRunID); err == nil {
			findings[i].ScanTaskName = run.TaskName
		}
	}

	return c.JSON(fiber.Map{"items": findings, "metadata": fiber.Map{"continue": next}})
}

// ListSecurityReviewSlices lists deterministic review slices for a repository.
func (h *Handlers) ListSecurityReviewSlices(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSecurityReviewSlices", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "listSecurityReviewSlices", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	limit, err := strconv.Atoi(c.Query("limit", "100"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	slices, next, err := h.securityStore.ListReviewSlices(c.Context(), store.ReviewSliceFilter{
		Namespace:      namespace,
		RepositoryScan: c.Params("name"),
		Status:         c.Query("status"),
		Limit:          limit,
		Cursor:         c.Query("cursor"),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list review slices: %v", err))
	}
	if slices == nil {
		slices = []store.ReviewSlice{}
	}
	return c.JSON(fiber.Map{"items": slices, "metadata": fiber.Map{"continue": next}})
}

// GetSecurityReviewSlice returns one deterministic review slice.
func (h *Handlers) GetSecurityReviewSlice(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getSecurityReviewSlice", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "getSecurityReviewSlice", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	slice, err := h.securityStore.GetReviewSlice(c.Context(), namespace, c.Params("name"), c.Params("sliceID"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "review slice not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get review slice: %v", err))
	}
	return c.JSON(slice)
}

// ListSecurityDroppedFindings lists diagnostics for v2 findings rejected during ingestion.
func (h *Handlers) ListSecurityDroppedFindings(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSecurityDroppedFindings", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "listSecurityDroppedFindings", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	reason := c.Query("reason")
	reasonContains := ""
	if after, ok := strings.CutPrefix(reason, "contains="); ok {
		reasonContains = after
		reason = ""
	}
	dropped, next, err := h.securityStore.ListDroppedFindings(c.Context(), store.DroppedFindingFilter{
		Namespace:      namespace,
		RepositoryScan: c.Params("name"),
		ScanRunID:      c.Query("scanRunID"),
		SliceID:        c.Query("sliceID"),
		Layer:          c.Query("layer"),
		Reason:         reason,
		ReasonContains: reasonContains,
		Limit:          limit,
		Cursor:         c.Query("cursor"),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list dropped findings: %v", err))
	}
	if dropped == nil {
		dropped = []store.DroppedFinding{}
	}
	return c.JSON(fiber.Map{"items": dropped, "metadata": fiber.Map{"continue": next}})
}

// GetSecurityFinding returns a finding by ID.
func (h *Handlers) GetSecurityFinding(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getSecurityFinding", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, finding.RepositoryScan)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "getSecurityFinding", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	if finding.ScanRunID != "" {
		if run, err := h.securityStore.GetScanRun(c.Context(), namespace, finding.ScanRunID); err == nil {
			finding.ScanTaskName = run.TaskName
		}
	}
	return c.JSON(finding)
}

// DismissSecurityFinding marks a finding as dismissed.
func (h *Handlers) DismissSecurityFinding(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "dismissSecurityFinding", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, finding.RepositoryScan)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "dismissSecurityFinding", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	if err := h.securityStore.UpdateFindingState(c.Context(), namespace, c.Params("id"), "dismissed"); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to dismiss finding: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ReopenSecurityFinding reopens a dismissed finding.
func (h *Handlers) ReopenSecurityFinding(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "reopenSecurityFinding", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, finding.RepositoryScan)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "reopenSecurityFinding", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	if err := h.securityStore.UpdateFindingState(c.Context(), namespace, c.Params("id"), "open"); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to reopen finding: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ValidateSecurityFinding creates a validator/repro task for a finding.
func (h *Handlers) ValidateSecurityFinding(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "validateSecurityFinding", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, finding.RepositoryScan)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "validateSecurityFinding", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}

	if err := h.createSecurityValidationTask(c.Context(), GetUserInfo(c), scan, finding); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusAccepted)
}

// GenerateSecurityPatch creates a patch proposal task for a finding.
func (h *Handlers) GenerateSecurityPatch(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "generateSecurityPatch", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, finding.RepositoryScan)
	if err != nil {
		return err
	}
	agentRef := securityPatchAgentRef(scan)
	if err := h.authorizeContextTokenSecurityScanTask(c, "generateSecurityPatch", scan, agentRef); err != nil {
		return err
	}

	proposal, err := h.createSecurityPatchTask(c.Context(), GetUserInfo(c), scan, finding)
	if err != nil {
		return err
	}
	finding.PatchProposalID = proposal.ID
	finding.State = "patch_pending"
	if err := h.securityStore.UpsertFinding(c.Context(), finding); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update finding: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(proposal)
}

// ListSecurityPatchProposals lists patch proposals for a finding.
func (h *Handlers) ListSecurityPatchProposals(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSecurityPatchProposals", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, finding.RepositoryScan)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "listSecurityPatchProposals", scan, securityPatchAgentRef(scan)); err != nil {
		return err
	}
	proposals, err := h.securityStore.ListPatchProposals(c.Context(), namespace, finding.ID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list patch proposals: %v", err))
	}
	return c.JSON(fiber.Map{"items": proposals})
}

func contextTokenSecurityScanFailures(token *ContextToken, scan *corev1alpha1.RepositoryScan, agentRef corev1alpha1.AgentReference) []string {
	failures := []string{}
	if want, ok := contextString(token.TransactionContext, "namespace"); ok && scan.Namespace != want {
		failures = append(failures, fmt.Sprintf("namespace %q does not match token context %q", scan.Namespace, want))
	}
	if want, ok := contextString(token.TransactionContext, "repo"); ok && scan.Spec.RepoURL != want {
		failures = append(failures, fmt.Sprintf("repository %q does not match token context %q", scan.Spec.RepoURL, want))
	}
	ref := security.EffectiveRef(scan)
	wantRef, hasWantRef := contextString(token.TransactionContext, "ref")
	if want, ok := contextString(token.TransactionContext, "branch"); ok {
		refOnlyScanMatches := scan.Spec.Branch == "" && ref != "" && hasWantRef && ref == wantRef
		if !refOnlyScanMatches && security.EffectiveBranch(scan) != want {
			failures = append(failures, fmt.Sprintf("workspace branch %q does not match token context %q", security.EffectiveBranch(scan), want))
		}
	}
	if hasWantRef && ref != wantRef {
		failures = append(failures, fmt.Sprintf("workspace ref %q does not match token context %q", ref, wantRef))
	} else if _, branchScoped := contextString(token.TransactionContext, "branch"); !hasWantRef && branchScoped && ref != "" {
		failures = append(failures, fmt.Sprintf("workspace ref %q is not allowed by branch-only token context", ref))
	}

	agentNamespace := agentRef.Namespace
	if agentNamespace == "" {
		agentNamespace = scan.Namespace
	}
	if want, ok := contextString(token.TransactionContext, "agent"); ok && !agentMatches(agentRef.Name, agentNamespace, want) {
		failures = append(failures, fmt.Sprintf("agent %q does not match token context %q", namespacedNameString(agentNamespace, agentRef.Name), want))
	}
	if allowed, ok := contextStringList(token.TransactionContext, "allowedAgents"); ok && !agentAllowed(agentRef.Name, agentNamespace, allowed) {
		failures = append(failures, fmt.Sprintf("agent %q is not allowed by token context", namespacedNameString(agentNamespace, agentRef.Name)))
	}
	return failures
}

func (h *Handlers) contextTokenSecurityScanAllowed(c fiber.Ctx, scan *corev1alpha1.RepositoryScan, agentRef corev1alpha1.AgentReference) bool {
	if !h.contextTokenAuthorization.Enabled() {
		return true
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return true
	}
	failures := contextTokenSecurityScanFailures(ui.ContextToken, scan, agentRef)
	if len(failures) == 0 {
		return true
	}
	if h.contextTokenAuthorization.enforcing() {
		return false
	}
	_ = h.handleContextTokenAuthorizationFailures(ui.ContextToken, "listRepositoryScans", failures)
	return true
}

func (h *Handlers) authorizeContextTokenSecurityScanTask(c fiber.Ctx, action string, scan *corev1alpha1.RepositoryScan, agentRef corev1alpha1.AgentReference) error {
	if !h.contextTokenAuthorization.Enabled() {
		return nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil
	}

	token := ui.ContextToken
	failures := contextTokenSecurityScanFailures(token, scan, agentRef)
	if len(failures) == 0 {
		metrics.RecordContextTokenAuthorization(action, "allowed", "ok")
		return nil
	}
	return h.handleContextTokenAuthorizationFailures(token, action, failures)
}

func extractGitToken(secret *corev1.Secret) string {
	for _, key := range []string{"token", "password", workerenv.GitHubToken} {
		if value, ok := secret.Data[key]; ok {
			token := strings.TrimSpace(string(value))
			if token != "" {
				return token
			}
		}
	}
	return ""
}

var githubPullRequestAPIBaseURL = "https://api.github.com"

func createGitHubPullRequest(ctx context.Context, token, owner, repo, head, base, title, body string) (string, int, string, error) {
	pr, err := tools.CreateOrGetGitHubPullRequest(ctx, token, owner, repo, head, base, title, body, githubPullRequestAPIBaseURL)
	if err != nil {
		return "", 0, "", err
	}
	return pr.HTMLURL, pr.Number, pr.Status, nil
}

// CreateSecurityPullRequest opens a pull request from the latest successful patch proposal.
func (h *Handlers) CreateSecurityPullRequest(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createSecurityPullRequest", h.contextTokenAuthorization.SecurityWriteScopes); err != nil {
		return err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, finding.RepositoryScan)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "createSecurityPullRequest", scan, securityPatchAgentRef(scan)); err != nil {
		return err
	}

	proposals, err := h.securityStore.ListPatchProposals(c.Context(), namespace, finding.ID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list patch proposals: %v", err))
	}
	var proposal *store.PatchProposal
	for i := range proposals {
		if proposals[i].Status == "succeeded" {
			proposal = &proposals[i]
			break
		}
	}
	if proposal == nil {
		return fiber.NewError(fiber.StatusBadRequest, "no successful patch proposal available")
	}
	if proposal.Branch == "" {
		return fiber.NewError(fiber.StatusBadRequest, "patch proposal does not have branch metadata")
	}
	if scan.Spec.GitSecretRef == nil {
		return fiber.NewError(fiber.StatusBadRequest, "repository scan does not have git credentials configured")
	}
	if err := h.authorizeContextTokenGitCredentialSecretName(c, "createSecurityPullRequestGitSecret", namespace, scan.Spec.GitSecretRef.Name); err != nil {
		return err
	}

	secret := &corev1.Secret{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: scan.Spec.GitSecretRef.Name, Namespace: namespace}, secret); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get git secret: %v", err))
	}
	token := extractGitToken(secret)
	if token == "" {
		return fiber.NewError(fiber.StatusBadRequest, "git secret does not contain a GitHub token")
	}

	owner, repo := security.ParseRepositoryURL(scan.Spec.RepoURL)
	if owner == "" || repo == "" {
		return fiber.NewError(fiber.StatusBadRequest, "repository URL must be a GitHub repository")
	}

	baseBranch := scan.Spec.PRBaseBranch
	if baseBranch == "" {
		baseBranch = security.EffectiveBranch(scan)
	}
	title := fmt.Sprintf("fix(security): %s", finding.Title)
	body := fmt.Sprintf("Security remediation for finding `%s`.\n\nSummary:\n%s\n\nRoot cause:\n%s\n\nRemediation guidance:\n%s\n",
		finding.ID, finding.Summary, finding.RootCause, finding.Remediation)

	prURL, prNumber, prStatus, err := createGitHubPullRequest(c.Context(), token, owner, repo, proposal.Branch, baseBranch, title, body)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create pull request: %v", err))
	}

	proposal.Status = "pr_opened"
	proposal.PRNumber = &prNumber
	proposal.PRURL = prURL
	if err := h.securityStore.UpdatePatchProposal(c.Context(), proposal); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update patch proposal: %v", err))
	}

	finding.State = "pr_open"
	finding.PRNumber = &prNumber
	finding.PRURL = prURL
	finding.PatchProposalID = proposal.ID
	if err := h.securityStore.UpsertFinding(c.Context(), finding); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update finding: %v", err))
	}

	return c.JSON(fiber.Map{
		"prNumber": prNumber,
		"prURL":    prURL,
		"status":   prStatus,
	})
}
