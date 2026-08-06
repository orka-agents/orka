package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/security"
	securitybundle "github.com/orka-agents/orka/internal/security/bundle"
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

type AppendFindingDecisionRequest struct {
	DecisionID              string                              `json:"decisionId"`
	Scope                   store.FindingDecisionScope          `json:"scope"`
	OccurrenceID            string                              `json:"occurrenceId,omitempty"`
	Action                  store.FindingDecisionAction         `json:"action"`
	ReasonCode              string                              `json:"reasonCode,omitempty"`
	Reason                  string                              `json:"reason,omitempty"`
	EvidenceReceiptIDs      []string                            `json:"evidenceReceiptIds,omitempty"`
	SupersedesDecisionID    string                              `json:"supersedesDecisionId,omitempty"`
	ExpectedDecisionVersion *int64                              `json:"expectedDecisionVersion"`
	Applicability           *store.FindingDecisionApplicability `json:"applicability,omitempty"`
}

const (
	sourceProviderGitHub              = "github"
	securityAssessmentOutcomeDeferred = "deferred"
	securityScanRunPhasePending       = "pending"
	securityScanRunPhaseRunning       = "running"
)

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

func (h *Handlers) ensureSecurityIntegrityStore() error {
	if h.securityIntegrityStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "security integrity store not configured")
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
		if !metav1.IsControlledBy(task, scan) {
			continue
		}
		stage := strings.TrimSpace(task.Labels[labels.LabelSecurityStage])
		if stage == security.StagePatch || stage == security.StageValidation {
			continue
		}
		switch task.Status.Phase {
		case corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing, corev1alpha1.TaskPhaseScheduled:
			return true, nil
		}
	}
	return false, nil
}

func (h *Handlers) updateRepositoryScanRunStatus(
	ctx context.Context,
	expectedScan *corev1alpha1.RepositoryScan,
	expectedRun *store.ScanRun,
	taskName string,
) error {
	if expectedScan == nil || expectedRun == nil {
		return fmt.Errorf("repository scan and scan run are required")
	}
	key := types.NamespacedName{Namespace: expectedScan.Namespace, Name: expectedScan.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentScan := &corev1alpha1.RepositoryScan{}
		if err := h.client.Get(ctx, key, currentScan); err != nil {
			return err
		}
		if currentScan.UID != expectedScan.UID || currentScan.Generation != expectedScan.Generation {
			return nil
		}

		currentRun, err := h.securityStore.GetScanRun(ctx, currentScan.Namespace, expectedRun.ID)
		if err != nil {
			return err
		}
		if currentRun.RepositoryScan != currentScan.Name || currentRun.RepositoryScanUID != string(currentScan.UID) ||
			currentRun.RepositoryScanGeneration != currentScan.Generation || currentRun.TaskName != taskName ||
			!activeSecurityScanRunPhase(currentRun.Phase) || currentRun.CompletedAt != nil {
			return nil
		}
		latestRuns, _, err := h.securityStore.ListScanRuns(ctx, currentScan.Namespace, currentScan.Name, 1, "")
		if err != nil {
			return err
		}
		if len(latestRuns) == 0 || latestRuns[0].ID != currentRun.ID ||
			!activeSecurityScanRunPhase(latestRuns[0].Phase) || latestRuns[0].CompletedAt != nil {
			return nil
		}

		base := currentScan.DeepCopy()
		currentScan.Status.Phase = "Scanning"
		currentScan.Status.LastScanID = currentRun.ID
		currentScan.Status.LastScanTaskName = taskName
		meta.SetStatusCondition(&currentScan.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "Scanning",
			Message:            "Security scan is running",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: currentScan.Generation,
		})
		if h.integrityConfig.QualityStateWritesEnabled {
			currentScan.Status.Quality = repositoryScanQualityStatusForAPIRun(currentRun)
			meta.SetStatusCondition(&currentScan.Status.Conditions, metav1.Condition{
				Type:               "QualityReady",
				Status:             metav1.ConditionUnknown,
				Reason:             "QualityPending",
				Message:            "Scan quality is still being evaluated",
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: currentScan.Generation,
			})
		} else {
			currentScan.Status.Quality = nil
			meta.RemoveStatusCondition(&currentScan.Status.Conditions, "QualityReady")
		}
		return h.client.Status().Patch(ctx, currentScan,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func repositoryScanQualityStatusForAPIRun(run *store.ScanRun) *corev1alpha1.RepositoryScanQualityStatus {
	if run == nil {
		return nil
	}
	return &corev1alpha1.RepositoryScanQualityStatus{
		SchemaVersion:             int32(run.Quality.SchemaVersion),
		ObservedRepositoryScanUID: run.RepositoryScanUID,
		ObservedGeneration:        run.RepositoryScanGeneration,
		InventoryCoverageStatus:   string(run.Quality.InventoryCoverageStatus),
		CandidateCoverageStatus:   string(run.Quality.CandidateCoverageStatus),
		CoverageStatus:            string(run.Quality.CoverageStatus),
		ValidationScope:           string(run.Quality.ValidationScope),
		ValidationExecution:       string(run.Quality.ValidationExecution),
		AttackPathExecution:       string(run.Quality.AttackPathExecution),
		AnalysisAttestationLevel:  string(run.Quality.AnalysisAttestationLevel),
		TargetVerification:        string(run.Quality.TargetVerification),
		BundleStatus:              string(run.Quality.BundleStatus),
		AuthorizationStatus:       string(run.Quality.AuthorizationStatus),
		IsolationStatus:           string(run.Quality.IsolationStatus),
		ReasonCodes:               append([]string(nil), run.Quality.ReasonCodes...),
	}
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

func (h *Handlers) securityAnalysisTaskAnnotations(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
) (map[string]string, error) {
	policy := security.EffectiveAnalysisIsolationPolicy(scan)
	if policy == "legacy" {
		annotations, _, err := security.AnalysisIsolationAnnotations(policy, nil)
		return annotations, err
	}
	if h.client == nil || scan == nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "analysis capability resolution is unavailable")
	}
	namespace := strings.TrimSpace(scan.Spec.AnalysisAgentRef.Namespace)
	if namespace == "" {
		namespace = scan.Namespace
	}
	agent := &corev1alpha1.Agent{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: scan.Spec.AnalysisAgentRef.Name}, agent); err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("failed to resolve analysis agent capability: %v", err))
	}
	annotations, _, err := security.AnalysisIsolationAnnotations(policy, agent)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return annotations, nil
}

func mergeSecurityAnnotations(base map[string]string, overlays ...map[string]string) map[string]string {
	out := make(map[string]string)
	maps.Copy(out, base)
	for _, overlay := range overlays {
		maps.Copy(out, overlay)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (h *Handlers) createSecurityScanRun(
	ctx context.Context,
	ui *UserInfo,
	scan *corev1alpha1.RepositoryScan,
	baseCommit string,
) (*store.ScanRun, error) {
	const mode = "manual"
	const headCommit = ""
	if scan == nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "repository scan is required")
	}
	if err := h.integrityConfig.ValidateRepositoryScanSpec(scan.Spec); err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if err := h.ensureSecurityStore(); err != nil {
		return nil, err
	}
	if err := authorizeContextTokenRepositoryScanPolicyRefsForUser(ui, h.contextTokenAuthorization, "createSecurityScanTaskPolicy", scan.Namespace, scan.Spec); err != nil {
		return nil, err
	}
	policy, err := security.LoadScannerPolicy(ctx, h.client, scan.Namespace, scan.Spec)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid repository scan policy: %v", err))
	}
	requestKey := security.RequestIdempotencyKey(scan, mode, baseCommit, headCommit, policy.Digest)
	if err := h.ensureNoUnrelatedActiveSecurityScan(ctx, scan, requestKey); err != nil {
		return nil, err
	}

	runUID, err := security.NewRunUID()
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	scanID := security.PublicScanRunID(runUID)
	firstStage := security.StageThreatModel
	if h.integrityConfig.PinnedScanTargetsEnabled {
		firstStage = security.StageMapper
	}
	taskName := security.ScanStageTaskNameForRun(scan.Name, mode, firstStage, "", runUID)
	run := &store.ScanRun{
		ID:                       scanID,
		RunUID:                   runUID,
		Namespace:                scan.Namespace,
		RepositoryScan:           scan.Name,
		RepositoryScanUID:        string(scan.UID),
		RepositoryScanGeneration: scan.Generation,
		TaskName:                 taskName,
		Mode:                     mode,
		Phase:                    "pending",
		BaseCommit:               baseCommit,
		HeadCommit:               headCommit,
		ScannerPolicyVersion:     security.ScannerPolicyVersion,
		PolicyDigest:             policy.Digest,
		IdempotencyKey:           requestKey,
		RequestIdempotencyKey:    requestKey,
		Quality:                  initialAPIScanQuality(scan, h.integrityConfig.PinnedScanTargetsEnabled),
		StartedAt:                time.Now().UTC(),
	}
	threatModelInput, err := h.captureSecurityRunThreatModelInput(ctx, scan, run)
	if err != nil {
		return nil, err
	}
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
				labels.LabelSecurityStage:  firstStage,
			},
			OwnerReferences: []metav1.OwnerReference{h.ownerRefForRepositoryScan(scan)},
		},
	}
	if h.integrityConfig.PinnedScanTargetsEnabled {
		timeout = metav1.Duration{Duration: 30 * time.Minute}
		priority = 690
		task.Spec = corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Command:  []string{"--security-mapper"},
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StageMapper},
				{Name: security.EnvScanID, Value: scanID},
				{Name: security.EnvScannerPolicyVersion, Value: security.ScannerPolicyVersion},
				{Name: security.EnvPolicyDigest, Value: policy.Digest},
				{Name: security.EnvPolicyProvenance, Value: security.PolicyProvenanceEnv(policy)},
				{Name: security.EnvScanBaseCommit, Value: baseCommit},
				{Name: security.EnvScanHeadCommit, Value: headCommit},
				{Name: security.EnvPinnedScanTargetsEnabled, Value: "true"},
			},
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo: scan.Spec.RepoURL, Branch: security.EffectiveWorkspaceBranch(scan), Ref: security.EffectiveRef(scan),
				GitSecretRef: scan.Spec.GitSecretRef, SubPath: scan.Spec.SubPath,
				ForkRepo: scan.Spec.ForkRepo, PRBaseBranch: scan.Spec.PRBaseBranch,
			},
		}
	} else {
		annotations, annotationErr := h.securityAnalysisTaskAnnotations(ctx, scan)
		if annotationErr != nil {
			return nil, annotationErr
		}
		switch annotations["orka.ai/security-isolation-status"] {
		case security.IsolationStatusHardened:
			run.Quality.IsolationStatus = store.IsolationStatusHardened
		case security.IsolationStatusFallback:
			run.Quality.IsolationStatus = store.IsolationStatusFallback
		case security.IsolationStatusLegacy:
			run.Quality.IsolationStatus = store.IsolationStatusLegacy
		}
		task.Annotations = annotations
		task.Spec = corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildThreatModelPrompt(scan, mode, baseCommit, headCommit, threatModelInput.Content, policy.PromptPolicy()),
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
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo: scan.Spec.RepoURL, Branch: security.EffectiveWorkspaceBranch(scan), Ref: security.EffectiveRef(scan),
				GitSecretRef: scan.Spec.GitSecretRef, SubPath: scan.Spec.SubPath,
				ForkRepo: scan.Spec.ForkRepo, PRBaseBranch: scan.Spec.PRBaseBranch,
			}},
		}
	}
	if scan.Spec.GitSecretRef != nil {
		if err := authorizeContextTokenGitCredentialSecretForUser(ui, h.contextTokenAuthorization, "createSecurityScanTaskGitSecret", scan.Namespace, scan.Spec.GitSecretRef.Name); err != nil {
			return nil, err
		}
	}
	if err := authorizeAndStampTaskContext(ctx, h.client, h.clientset, contextTokenFromUserInfo(ui), h.contextTokenAuthorization, "createSecurityScanTask", ui, task); err != nil {
		return nil, err
	}
	if err := h.securityRunTaskInputStore.CreateScanRunWithTaskInput(ctx, run, threatModelInput); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return nil, securityIntegrityHTTPError(err, "create scan run")
		}
		activeRun, recoveryErr := h.findActiveSecurityScanRunByIdempotencyKey(ctx, scan, requestKey)
		if errors.Is(recoveryErr, store.ErrNotFound) {
			return nil, fiber.NewError(fiber.StatusConflict, "a different security scan became active for this repository")
		}
		if recoveryErr != nil {
			return nil, securityIntegrityHTTPError(recoveryErr, "recover active scan run")
		}
		recoveredTask, recoveryErr := h.recoveredInitialSecurityScanTask(
			task, activeRun, scan, mode, baseCommit, headCommit, policy.Digest, requestKey, firstStage,
		)
		if recoveryErr != nil {
			return nil, fiber.NewError(fiber.StatusConflict, recoveryErr.Error())
		}
		if firstStage == security.StageThreatModel {
			activeInput, inputErr := h.loadSecurityRunThreatModelInput(ctx, activeRun)
			if inputErr != nil {
				return nil, inputErr
			}
			recoveredTask.Spec.Prompt = security.BuildThreatModelPrompt(
				scan, mode, baseCommit, headCommit, activeInput.Content, policy.PromptPolicy(),
			)
		}
		if recoveryErr := h.ensureSecurityScanRunTask(ctx, scan, activeRun, recoveredTask, true); recoveryErr != nil {
			return nil, recoveryErr
		}
		return activeRun, nil
	}
	if err := h.ensureSecurityScanRunTask(ctx, scan, run, task, false); err != nil {
		return nil, err
	}
	return run, nil
}

func (h *Handlers) ensureNoUnrelatedActiveSecurityScan(
	ctx context.Context, scan *corev1alpha1.RepositoryScan, requestKey string,
) error {
	if _, err := h.findActiveSecurityScanRunByIdempotencyKey(ctx, scan, requestKey); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return securityIntegrityHTTPError(err, "find active scan run")
	}

	active, err := h.hasActiveSecurityScanPipelineTask(ctx, scan)
	if err != nil || !active {
		return err
	}
	if _, err := h.findActiveSecurityScanRunByIdempotencyKey(ctx, scan, requestKey); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return securityIntegrityHTTPError(err, "recheck active scan run")
	}
	return fiber.NewError(fiber.StatusConflict, "a security scan is already running for this repository")
}

func activeSecurityScanRunPhase(phase string) bool {
	return phase == securityScanRunPhasePending || phase == securityScanRunPhaseRunning
}

func scanRunMatchesIdempotencyKey(run *store.ScanRun, requestKey string) bool {
	if run == nil || strings.TrimSpace(requestKey) == "" {
		return false
	}
	requestKey = strings.TrimSpace(requestKey)
	currentKey := strings.TrimSpace(run.RequestIdempotencyKey)
	legacyKey := strings.TrimSpace(run.IdempotencyKey)
	if currentKey != "" && legacyKey != "" && currentKey != legacyKey {
		return false
	}
	if currentKey != "" {
		return currentKey == requestKey
	}
	return legacyKey == requestKey
}

func scanRunMatchesOriginalRequest(
	run *store.ScanRun,
	mode, baseCommit, headCommit, policyDigest, requestKey string,
) bool {
	if run == nil || run.Mode != mode || run.BaseCommit != baseCommit ||
		run.ScannerPolicyVersion != security.ScannerPolicyVersion || run.PolicyDigest != policyDigest ||
		!scanRunMatchesIdempotencyKey(run, requestKey) {
		return false
	}
	if strings.TrimSpace(run.RequestIdempotencyKey) != "" {
		// RequestIdempotencyKey is immutable and captures the originally requested
		// head before the mapper resolves and writes ScanRun.HeadCommit.
		return true
	}
	// Legacy rows do not carry the immutable request key, so retain the stricter
	// projection comparison rather than treating a later head as a replay match.
	return run.HeadCommit == headCommit
}

func (h *Handlers) findActiveSecurityScanRunByIdempotencyKey(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	requestKey string,
) (*store.ScanRun, error) {
	if scan == nil || strings.TrimSpace(requestKey) == "" {
		return nil, fmt.Errorf("%w: repository scan and request idempotency key are required", store.ErrValidation)
	}
	const pageSize = 100
	cursor := ""
	var match *store.ScanRun
	for {
		runs, nextCursor, err := h.securityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, pageSize, cursor)
		if err != nil {
			return nil, err
		}
		for i := range runs {
			candidate := &runs[i]
			if !activeSecurityScanRunPhase(candidate.Phase) || !scanRunMatchesIdempotencyKey(candidate, requestKey) {
				continue
			}
			if match != nil && match.ID != candidate.ID {
				return nil, fmt.Errorf("%w: multiple active scan runs match the request idempotency key", store.ErrConflict)
			}
			copy := *candidate
			match = &copy
		}
		if nextCursor == "" {
			break
		}
		if nextCursor == cursor {
			return nil, fmt.Errorf("%w: scan run pagination did not advance", store.ErrConflict)
		}
		cursor = nextCursor
	}
	if match == nil {
		return nil, store.ErrNotFound
	}
	return match, nil
}

func (h *Handlers) recoveredInitialSecurityScanTask(
	template *corev1alpha1.Task,
	run *store.ScanRun,
	scan *corev1alpha1.RepositoryScan,
	mode, baseCommit, headCommit, policyDigest, requestKey, firstStage string,
) (*corev1alpha1.Task, error) {
	if template == nil || run == nil || scan == nil {
		return nil, fmt.Errorf("active scan run recovery requires a task template, run, and repository scan")
	}
	if !activeSecurityScanRunPhase(run.Phase) || run.CompletedAt != nil {
		return nil, fmt.Errorf("active scan run %q is no longer repairable", run.ID)
	}
	if run.Namespace != scan.Namespace || run.RepositoryScan != scan.Name ||
		run.RepositoryScanUID != string(scan.UID) || run.RepositoryScanGeneration != scan.Generation {
		return nil, fmt.Errorf("active scan run %q does not match the current repository scan identity", run.ID)
	}
	if !scanRunMatchesOriginalRequest(run, mode, baseCommit, headCommit, policyDigest, requestKey) {
		return nil, fmt.Errorf("active scan run %q does not match the requested scan inputs", run.ID)
	}
	if run.RunUID != strings.TrimSpace(run.RunUID) || !security.ValidRunUID(run.RunUID) {
		return nil, fmt.Errorf("active scan run %q has an invalid run UID", run.ID)
	}
	expectedScanID := security.PublicScanRunID(run.RunUID)
	if run.ID != expectedScanID {
		return nil, fmt.Errorf("active scan run %q does not match its run UID", run.ID)
	}
	expectedTaskName := security.ScanStageTaskNameForRun(scan.Name, mode, firstStage, "", run.RunUID)
	if run.TaskName != expectedTaskName {
		return nil, fmt.Errorf("active scan run %q does not reference its deterministic initial task", run.ID)
	}

	task := template.DeepCopy()
	task.Name = expectedTaskName
	task.Namespace = scan.Namespace
	if task.Labels == nil {
		task.Labels = map[string]string{}
	}
	task.Labels[labels.LabelManaged] = queryTrue
	task.Labels[labels.LabelCreatedBy] = "repository-security"
	task.Labels[labels.LabelSecurityTarget] = labels.SelectorValue(scan.Name)
	task.Labels[labels.LabelSecurityScanID] = run.ID
	task.Labels[labels.LabelSecurityMode] = mode
	task.Labels[labels.LabelSecurityStage] = firstStage
	task.OwnerReferences = []metav1.OwnerReference{h.ownerRefForRepositoryScan(scan)}

	scanIDEnv := -1
	for i := range task.Spec.Env {
		if task.Spec.Env[i].Name != security.EnvScanID {
			continue
		}
		if scanIDEnv >= 0 {
			return nil, fmt.Errorf("deterministic initial task contains duplicate scan ID environment bindings")
		}
		scanIDEnv = i
		task.Spec.Env[i].Value = run.ID
	}
	if scanIDEnv < 0 {
		return nil, fmt.Errorf("deterministic initial task is missing the scan ID environment binding")
	}
	return task, nil
}

func (h *Handlers) ensureSecurityScanRunTask(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	task *corev1alpha1.Task,
	verifyExistingFirst bool,
) error {
	if verifyExistingFirst {
		existing := &corev1alpha1.Task{}
		getErr := h.client.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, existing)
		switch {
		case getErr == nil && securityTaskMatchesExpected(existing, task, scan):
			return h.repairRepositoryScanRunStatus(ctx, scan, run, task)
		case getErr == nil:
			admissionErr := fmt.Errorf("existing deterministic scan task does not match the active run binding")
			return h.handleSecurityScanRunTaskAdmissionFailure(ctx, scan, run, task, admissionErr)
		case apierrors.IsNotFound(getErr):
			// Recreate the deterministic initial Task below.
		default:
			// A failed read cannot distinguish an absent Task from an admitted one.
			// Keep the active run pending so a later retry can repair it.
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf(
				"scan task recovery outcome is unknown: verify: %v", getErr,
			))
		}
	}

	if createErr := h.client.Create(ctx, task); createErr != nil {
		existing := &corev1alpha1.Task{}
		getErr := h.client.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, existing)
		switch {
		case getErr == nil && securityTaskMatchesExpected(existing, task, scan):
			// The API server may admit the deterministic Task and still return a
			// timeout or transport error. Treat the matching persisted object as
			// authoritative and repair the RepositoryScan status below.
		case getErr == nil:
			admissionErr := fmt.Errorf("scan task create returned an error and the admitted deterministic task does not match the requested run binding: %w", createErr)
			return h.handleSecurityScanRunTaskAdmissionFailure(ctx, scan, run, task, admissionErr)
		case apierrors.IsNotFound(getErr) && definitiveSecurityTaskCreateRejection(createErr):
			admissionErr := fmt.Errorf("scan task create was definitively rejected: %w", createErr)
			return h.handleSecurityScanRunTaskAdmissionFailure(ctx, scan, run, task, admissionErr)
		case apierrors.IsNotFound(getErr):
			// A transport timeout or other ambiguous create response can race the
			// API server's read path. An immediate NotFound is not proof that the
			// Task was rejected, so keep the run pending for idempotent recovery.
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf(
				"scan task admission outcome is unknown after create error: create: %v; verify: deterministic task not found", createErr,
			))
		default:
			// A failed read cannot distinguish a rejected create from an admitted
			// Task. Keep the run pending so reconciliation or a retry can repair it.
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf(
				"scan task admission outcome is unknown after create error: create: %v; verify: %v", createErr, getErr,
			))
		}
	}
	return h.repairRepositoryScanRunStatus(ctx, scan, run, task)
}

func definitiveSecurityTaskCreateRejection(err error) bool {
	if err == nil {
		return false
	}
	var statusErr apierrors.APIStatus
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.Status().Reason {
	case metav1.StatusReasonNotFound,
		metav1.StatusReasonInvalid,
		metav1.StatusReasonBadRequest,
		metav1.StatusReasonForbidden,
		metav1.StatusReasonUnauthorized,
		metav1.StatusReasonMethodNotAllowed,
		metav1.StatusReasonRequestEntityTooLarge:
		return true
	default:
		return false
	}
}

func (h *Handlers) handleSecurityScanRunTaskAdmissionFailure(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	expectedTask *corev1alpha1.Task,
	admissionErr error,
) error {
	existing := &corev1alpha1.Task{}
	getErr := h.client.Get(ctx, types.NamespacedName{Namespace: expectedTask.Namespace, Name: expectedTask.Name}, existing)
	switch {
	case getErr == nil && securityTaskMatchesExpected(existing, expectedTask, scan):
		currentRun, err := h.securityStore.GetScanRun(ctx, run.Namespace, run.ID)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to reload recovered scan run: %v", err))
		}
		if !activeSecurityScanRunPhase(currentRun.Phase) || currentRun.CompletedAt != nil {
			return fiber.NewError(fiber.StatusConflict, "scan run changed before the admitted task could be recovered")
		}
		return h.repairRepositoryScanRunStatus(ctx, scan, currentRun, existing)
	case getErr == nil, apierrors.IsNotFound(getErr):
		// Task admission and scan-run persistence live in different systems and
		// cannot be committed atomically. Terminalizing from this observation
		// would race a concurrent idempotent caller that admits the same Task
		// immediately afterward. Keep the unique active run pending so the exact
		// deterministic Task can be verified or recreated by a later retry.
	default:
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf(
			"scan task admission could not be revalidated; scan run remains pending for recovery: %v", getErr,
		))
	}
	return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf(
		"%v; scan run remains pending for idempotent task admission recovery", admissionErr,
	))
}

func (h *Handlers) repairRepositoryScanRunStatus(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	task *corev1alpha1.Task,
) error {
	if err := h.updateRepositoryScanRunStatus(ctx, scan, run, task.Name); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update repository scan status: %v", err))
	}
	return nil
}

func (h *Handlers) captureSecurityRunThreatModelInput(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
) (*store.SecurityRunTaskInput, error) {
	if h.securityRunTaskInputStore == nil {
		return nil, fiber.NewError(fiber.StatusNotImplemented, "security run task-input store not configured")
	}
	input := &store.SecurityRunTaskInput{
		RunUID: run.RunUID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan,
		ScanRunID: run.ID, Stage: security.StageThreatModel,
	}
	model, err := h.securityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
	switch {
	case err == nil && threatModelBoundToRepositoryScan(model, scan):
		input.SourceVersion = model.Version
		input.Content = model.Content
	case err == nil, errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to snapshot threat model input: %v", err))
	}
	input.Content = security.NormalizeTaskInputSnapshot(input.Content)
	return input, nil
}

func (h *Handlers) loadSecurityRunThreatModelInput(
	ctx context.Context,
	run *store.ScanRun,
) (*store.SecurityRunTaskInput, error) {
	if h.securityRunTaskInputStore == nil {
		return nil, fiber.NewError(fiber.StatusNotImplemented, "security run task-input store not configured")
	}
	if run == nil || !security.ValidRunUID(run.RunUID) {
		return nil, fiber.NewError(fiber.StatusConflict, "active scan run has no valid task-input binding")
	}
	input, err := h.securityRunTaskInputStore.GetSecurityRunTaskInput(
		ctx, run.Namespace, run.RunUID, security.StageThreatModel,
	)
	if err != nil {
		return nil, securityIntegrityHTTPError(err, "load security run task input")
	}
	if input.RunUID != run.RunUID || input.Namespace != run.Namespace || input.RepositoryScan != run.RepositoryScan ||
		input.ScanRunID != run.ID || input.Stage != security.StageThreatModel {
		return nil, fiber.NewError(fiber.StatusConflict, "security run task-input binding does not match the active scan run")
	}
	return input, nil
}

func initialAPIScanQuality(scan *corev1alpha1.RepositoryScan, targetPending bool) store.ScanQuality {
	quality := store.LegacyScanQuality()
	quality.InventoryCoverageStatus = store.CoverageStatusPending
	quality.CandidateCoverageStatus = store.CoverageStatusPending
	quality.CoverageStatus = store.CoverageStatusPending
	quality.ValidationExecution = store.QualityExecutionNotStarted
	quality.AttackPathExecution = store.QualityExecutionNotStarted
	quality.IsolationStatus = store.IsolationStatusUnverified
	if targetPending {
		quality.TargetVerification = store.TargetVerificationPending
	}
	switch security.EffectiveValidationMode(scan) {
	case "off":
		quality.ValidationScope = store.ValidationScopeOff
	case "full":
		quality.ValidationScope = store.ValidationScopeAll
	default:
		quality.ValidationScope = store.ValidationScopeSampled
	}
	return quality
}

//nolint:gocyclo // Validation admission keeps authorization, immutable binding, deduplication, and seal fencing ordered fail-closed.
func (h *Handlers) createSecurityValidationTask(ctx context.Context, ui *UserInfo, scan *corev1alpha1.RepositoryScan, finding *store.Finding) error {
	if strings.TrimSpace(finding.ScanRunID) == "" {
		return fiber.NewError(fiber.StatusConflict, "finding has no verifiable source scan run")
	}
	if err := authorizeContextTokenRepositoryScanPolicyRefsForUser(ui, h.contextTokenAuthorization, "createSecurityValidationTaskPolicy", scan.Namespace, scan.Spec); err != nil {
		return err
	}
	policy, err := security.LoadScannerPolicy(ctx, h.client, scan.Namespace, scan.Spec)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid repository scan policy: %v", err))
	}
	sourceRun, err := h.securityStore.GetScanRun(ctx, scan.Namespace, finding.ScanRunID)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusConflict, "source scan run is unavailable; validation cannot safely determine bundle state")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load scan run: %v", err))
	}
	if sourceRun.RepositoryScan != scan.Name || sourceRun.RepositoryScanUID == "" || sourceRun.RepositoryScanGeneration <= 0 ||
		sourceRun.RepositoryScanUID != string(scan.UID) || sourceRun.RepositoryScanGeneration != scan.Generation {
		return fiber.NewError(fiber.StatusConflict, "source scan run is not bound to the current RepositoryScan generation")
	}
	if security.ValidRunUID(sourceRun.RunUID) && strings.TrimSpace(sourceRun.HeadCommit) == "" {
		return fiber.NewError(fiber.StatusConflict, "source scan run has no resolved immutable target commit")
	}
	if sourceRun.Quality.BundleStatus == store.BundleStatusSealing || sourceRun.Quality.BundleStatus == store.BundleStatusSealed {
		return fiber.NewError(fiber.StatusConflict, "source scan bundle is sealing or sealed; supplemental validation is not yet supported")
	}
	if h.securityBundleStore != nil {
		if _, bundleErr := h.securityBundleStore.GetSecurityScanBundle(ctx, scan.Namespace, finding.ScanRunID); bundleErr == nil {
			return fiber.NewError(fiber.StatusConflict, "source scan bundle is sealed; supplemental validation is not yet supported")
		} else if !errors.Is(bundleErr, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load source scan bundle: %v", bundleErr))
		}
	}
	if sourceRun.PolicyDigest != "" && policy.Digest != "" && sourceRun.PolicyDigest != policy.Digest {
		return fiber.NewError(fiber.StatusConflict, "scanner policy changed during active scan run")
	}
	if strings.TrimSpace(finding.CurrentOccurrenceID) != "" {
		if h.securityIntegrityStore == nil {
			return fiber.NewError(fiber.StatusConflict, "finding occurrence store is unavailable")
		}
		occurrence, occurrenceErr := h.securityIntegrityStore.GetFindingOccurrence(ctx, scan.Namespace, finding.CurrentOccurrenceID)
		if occurrenceErr != nil {
			return securityIntegrityHTTPError(occurrenceErr, "load finding occurrence")
		}
		if occurrence.RepositoryScan != scan.Name || occurrence.ScanRunID != sourceRun.ID || occurrence.RunUID != sourceRun.RunUID ||
			occurrence.PublicFindingID != finding.ID || occurrence.TargetReceiptID != sourceRun.TargetReceiptID ||
			occurrence.TargetSHA != sourceRun.HeadCommit {
			return fiber.NewError(fiber.StatusConflict, "finding occurrence does not match the source scan run")
		}
	}
	if h.securityIntegrityStore != nil && strings.TrimSpace(finding.CurrentOccurrenceID) != "" {
		assessments, _, err := h.securityIntegrityStore.ListFindingAssessments(ctx, store.FindingAssessmentFilter{
			Namespace: scan.Namespace, RepositoryScan: finding.RepositoryScan, OccurrenceID: finding.CurrentOccurrenceID,
			Kind: store.FindingAssessmentValidation, Limit: 1,
		})
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to check existing validation assessment: %v", err))
		}
		if slices.ContainsFunc(assessments, validationAssessmentBlocksManualRequest) {
			return fiber.NewError(fiber.StatusConflict, "finding occurrence already has a validator assessment")
		}
	}
	timeout := metav1.Duration{Duration: 90 * time.Minute}
	priority := int32(725)
	validationScopeID := finding.CurrentOccurrenceID
	if validationScopeID == "" {
		validationScopeID = finding.ID
	}
	runIdentity := sourceRun.RunUID
	if !security.ValidRunUID(runIdentity) {
		runIdentity = "legacy:" + sourceRun.ID
	}
	taskName := security.ScanStageTaskNameForRun(
		scan.Name, "validation", security.StageValidation, validationScopeID, runIdentity,
	)
	analysisAnnotations, err := h.securityAnalysisTaskAnnotations(ctx, scan)
	if err != nil {
		return err
	}

	bindingAnnotations := map[string]string{}
	if security.ValidRunUID(sourceRun.RunUID) {
		bindingAnnotations[security.AnnotationValidationBindingVersion] = security.ValidationBindingVersion
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:        taskName,
			Namespace:   scan.Namespace,
			Annotations: mergeSecurityAnnotations(analysisAnnotations, bindingAnnotations),
			Labels: map[string]string{
				labels.LabelManaged:              "true",
				labels.LabelCreatedBy:            "repository-security",
				labels.LabelSecurityTarget:       labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID:       finding.ScanRunID,
				labels.LabelSecurityMode:         security.StageValidation,
				labels.LabelSecurityStage:        security.StageValidation,
				labels.LabelSecurityFindingID:    labels.SelectorValue(finding.ID),
				labels.LabelSecurityOccurrenceID: labels.SelectorValue(finding.CurrentOccurrenceID),
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
				{Name: security.EnvOccurrenceID, Value: finding.CurrentOccurrenceID},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveWorkspaceBranch(scan),
					Ref:          pinnedSourceRef(scan, sourceRun),
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
	if err := h.authorizeAndStampPinnedSecurityTask(ctx, ui, scan, task, "createSecurityValidationTask"); err != nil {
		return err
	}
	created := true
	if err := h.client.Create(ctx, task); err != nil {
		if apierrors.IsAlreadyExists(err) {
			created = false
			existing := &corev1alpha1.Task{}
			if getErr := h.client.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, existing); getErr != nil {
				return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load existing validation task: %v", getErr))
			}
			if !securityTaskMatchesExpected(existing, task, scan) {
				return fiber.NewError(fiber.StatusConflict, "existing validation task does not match the requested occurrence")
			}
			if existing.Status.Phase == corev1alpha1.TaskPhaseSucceeded || existing.Status.Phase == corev1alpha1.TaskPhaseFailed ||
				existing.Status.Phase == corev1alpha1.TaskPhaseCancelled {
				return fiber.NewError(fiber.StatusConflict, "validation task already reached a terminal state")
			}
		} else {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create validation task: %v", err))
		}
	}
	latestRun, err := h.securityStore.GetScanRun(ctx, scan.Namespace, finding.ScanRunID)
	if err != nil {
		if created {
			_ = h.client.Delete(ctx, task)
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to revalidate source scan run: %v", err))
	}
	if latestRun.Quality.BundleStatus == store.BundleStatusSealing || latestRun.Quality.BundleStatus == store.BundleStatusSealed {
		if created {
			_ = h.client.Delete(ctx, task)
		}
		return fiber.NewError(fiber.StatusConflict, "source scan bundle began sealing before validation task admission completed")
	}
	if h.securityBundleStore != nil {
		if _, bundleErr := h.securityBundleStore.GetSecurityScanBundle(ctx, scan.Namespace, finding.ScanRunID); bundleErr == nil {
			if created {
				_ = h.client.Delete(ctx, task)
			}
			return fiber.NewError(fiber.StatusConflict, "source scan bundle sealed before validation task admission completed")
		} else if !errors.Is(bundleErr, store.ErrNotFound) {
			if created {
				_ = h.client.Delete(ctx, task)
			}
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to revalidate source scan bundle: %v", bundleErr))
		}
	}
	finding.ValidationStatus = "pending"
	if err := h.securityStore.UpsertFinding(ctx, finding); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update finding: %v", err))
	}
	return nil
}

func securityTaskMatchesExpected(existing, expected *corev1alpha1.Task, scan *corev1alpha1.RepositoryScan) bool {
	if existing == nil || expected == nil || scan == nil || existing.Name != expected.Name || existing.Namespace != expected.Namespace ||
		!metav1.IsControlledBy(existing, scan) ||
		!apiequality.Semantic.DeepEqual(deterministicSecurityTaskSpec(existing), deterministicSecurityTaskSpec(expected)) {
		return false
	}
	for key, value := range expected.Labels {
		if securityTaskRequestSpecificLabel(key) {
			continue
		}
		if existing.Labels[key] != value {
			return false
		}
	}
	for key, value := range expected.Annotations {
		if securityTaskRequestSpecificAnnotation(key) {
			continue
		}
		if existing.Annotations[key] != value {
			return false
		}
	}
	for key, value := range existing.Labels {
		if !securityTaskBindingMetadataKey(key) {
			continue
		}
		expectedValue, ok := expected.Labels[key]
		if !ok || expectedValue != value {
			return false
		}
	}
	for key, value := range existing.Annotations {
		if !securityTaskBindingMetadataKey(key) {
			continue
		}
		expectedValue, ok := expected.Annotations[key]
		if !ok || expectedValue != value {
			return false
		}
	}
	return true
}

func securityTaskBindingMetadataKey(key string) bool {
	return strings.HasPrefix(key, "orka.ai/security-") ||
		strings.HasPrefix(key, "orka.ai/harness-wrapper-")
}

func deterministicSecurityTaskSpec(task *corev1alpha1.Task) corev1alpha1.TaskSpec {
	copy := task.DeepCopy()
	copy.Spec.RequestedBy = nil
	copy.Spec.Transaction = nil
	return copy.Spec
}

func securityTaskRequestSpecificLabel(key string) bool {
	switch key {
	case labels.LabelTransactionID, labels.LabelAuthProfile:
		return true
	default:
		return false
	}
}

func securityTaskRequestSpecificAnnotation(key string) bool {
	switch key {
	case labels.AnnotationTransactionID,
		labels.AnnotationContextTokenProfile,
		labels.AnnotationTransactionIssuer,
		labels.AnnotationTransactionSubject,
		labels.AnnotationTransactionRequestingWorkload,
		labels.AnnotationTransactionScope,
		labels.AnnotationTransactionContextDigest,
		labels.AnnotationRequesterContextDigest,
		labels.AnnotationTraceParent,
		labels.AnnotationTraceState,
		labels.AnnotationTraceBaggage:
		return true
	default:
		return false
	}
}

func (h *Handlers) authorizeAndStampPinnedSecurityTask(
	ctx context.Context,
	ui *UserInfo,
	scan *corev1alpha1.RepositoryScan,
	task *corev1alpha1.Task,
	action string,
) error {
	authorizationTask := task.DeepCopy()
	if authorizationTask.Spec.AgentRuntime != nil && authorizationTask.Spec.AgentRuntime.Workspace != nil {
		authorizationTask.Spec.AgentRuntime.Workspace.Branch = security.EffectiveWorkspaceBranch(scan)
		authorizationTask.Spec.AgentRuntime.Workspace.Ref = security.EffectiveRef(scan)
	}
	if err := authorizeAndStampTaskContext(ctx, h.client, h.clientset, contextTokenFromUserInfo(ui),
		h.contextTokenAuthorization, action, ui, authorizationTask); err != nil {
		return err
	}
	task.Labels = maps.Clone(authorizationTask.Labels)
	task.Annotations = maps.Clone(authorizationTask.Annotations)
	task.Spec.RequestedBy = authorizationTask.Spec.RequestedBy
	task.Spec.Transaction = authorizationTask.Spec.Transaction
	return nil
}

func securityPatchAgentRef(scan *corev1alpha1.RepositoryScan) corev1alpha1.AgentReference {
	agentRef := scan.Spec.AnalysisAgentRef
	if scan.Spec.PatchAgentRef != nil {
		agentRef = *scan.Spec.PatchAgentRef
	}
	return agentRef
}

func pinnedSourceRef(scan *corev1alpha1.RepositoryScan, run *store.ScanRun) string {
	if run != nil && strings.TrimSpace(run.HeadCommit) != "" {
		return strings.TrimSpace(run.HeadCommit)
	}
	return security.EffectiveRef(scan)
}

func (h *Handlers) createSecurityPatchTask(ctx context.Context, ui *UserInfo, scan *corev1alpha1.RepositoryScan, finding *store.Finding) (*store.PatchProposal, error) {
	if err := h.ensureSecurityStore(); err != nil {
		return nil, err
	}

	sourceRunID := strings.TrimSpace(finding.ScanRunID)
	if sourceRunID == "" {
		return nil, fiber.NewError(fiber.StatusConflict, "patch generation requires a verifiable source scan run")
	}
	sourceRun, err := h.securityStore.GetScanRun(ctx, scan.Namespace, sourceRunID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusConflict, "patch generation source scan run is unavailable")
	}
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load source scan run: %v", err))
	}
	if sourceRun.RepositoryScan != scan.Name || !security.ValidRunUID(sourceRun.RunUID) ||
		sourceRun.RepositoryScanUID == "" || sourceRun.RepositoryScanUID != string(scan.UID) ||
		sourceRun.RepositoryScanGeneration <= 0 || sourceRun.RepositoryScanGeneration != scan.Generation {
		return nil, fiber.NewError(fiber.StatusConflict, "patch generation source scan run is not verifiably bound to the current RepositoryScan generation")
	}
	sourceHead, ok := security.NormalizeFullGitObjectID(sourceRun.HeadCommit)
	if !ok {
		return nil, fiber.NewError(fiber.StatusConflict, "patch generation requires a full 40- or 64-hex source head commit")
	}
	agentRef := securityPatchAgentRef(scan)

	patchScopeID := strings.TrimSpace(finding.CurrentOccurrenceID)
	if patchScopeID == "" {
		patchScopeID = strings.TrimSpace(finding.ID)
	}
	taskName := security.ScanStageTaskNameForRun(
		scan.Name, "patch", security.StagePatch, patchScopeID, sourceRun.RunUID,
	)
	proposalID := security.PatchProposalIDForOccurrence(sourceRun.RunUID, patchScopeID)
	branch := security.PatchBranch(finding.ID, taskName)
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(750)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:              "true",
				labels.LabelCreatedBy:            "repository-security",
				labels.LabelSecurityTarget:       labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID:       finding.ScanRunID,
				labels.LabelSecurityMode:         "patch",
				labels.LabelSecurityStage:        security.StagePatch,
				labels.LabelSecurityFindingID:    labels.SelectorValue(finding.ID),
				labels.LabelSecurityOccurrenceID: labels.SelectorValue(finding.CurrentOccurrenceID),
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
				{Name: security.EnvScanID, Value: finding.ScanRunID},
				{Name: security.EnvFindingID, Value: finding.ID},
				{Name: security.EnvOccurrenceID, Value: finding.CurrentOccurrenceID},
				{Name: security.EnvPatchBranch, Value: branch},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveWorkspaceBranch(scan),
					Ref:          sourceHead,
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
	if err := h.authorizeAndStampPinnedSecurityTask(ctx, ui, scan, task, "createSecurityPatchTask"); err != nil {
		return nil, err
	}
	if err := h.client.Create(ctx, task); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create patch task: %v", err))
	}

	proposal := &store.PatchProposal{
		ID:              proposalID,
		Namespace:       scan.Namespace,
		RepositoryScan:  scan.Name,
		FindingID:       finding.ID,
		OccurrenceID:    finding.CurrentOccurrenceID,
		SourceScanRunID: finding.ScanRunID,
		SourceHeadSHA:   sourceHead,
		TaskName:        taskName,
		Branch:          branch,
		Status:          "pending",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	if err := h.securityStore.CreatePatchProposal(ctx, proposal); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create patch proposal: %v", err))
	}
	return proposal, nil
}

type repositoryScanListResponse struct {
	Items          []corev1alpha1.RepositoryScan `json:"items"`
	LatestScanRuns *[]store.ScanRun              `json:"latestScanRuns,omitempty"`
	Metadata       ListMeta                      `json:"metadata"`
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

	var latestScanRuns *[]store.ScanRun
	if c.Query("includeLatestRuns") == queryTrue {
		if err := h.ensureSecurityStore(); err != nil {
			return err
		}
		identities := make([]store.RepositoryScanIdentity, 0, len(items))
		for i := range items {
			if items[i].UID == "" || items[i].Generation <= 0 {
				continue
			}
			identities = append(identities, store.RepositoryScanIdentity{
				Name: items[i].Name, UID: string(items[i].UID), Generation: items[i].Generation,
			})
		}
		runs, err := h.securityStore.ListLatestScanRuns(c.Context(), namespace, identities)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list latest security scan runs: %v", err))
		}
		if runs == nil {
			runs = []store.ScanRun{}
		}
		latestScanRuns = &runs
	}

	return c.JSON(repositoryScanListResponse{
		Items:          items,
		LatestScanRuns: latestScanRuns,
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
	if err := h.integrityConfig.ValidateRepositoryScanSpec(req.Spec); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
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

	preserveRepositoryScanIntegrityPolicy(&req.Spec, scan.Spec)
	h.normalizeRepositoryScanSpec(&req.Spec)
	if err := h.integrityConfig.ValidateRepositoryScanSpec(req.Spec); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
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

func preserveRepositoryScanIntegrityPolicy(next *corev1alpha1.RepositoryScanSpec, current corev1alpha1.RepositoryScanSpec) {
	if next == nil {
		return
	}
	if strings.TrimSpace(next.AnalysisIsolationPolicy) == "" {
		next.AnalysisIsolationPolicy = current.AnalysisIsolationPolicy
	}
	if strings.TrimSpace(next.CompletionPolicy) == "" {
		next.CompletionPolicy = current.CompletionPolicy
	}
	if strings.TrimSpace(next.IncrementalBaselinePolicy) == "" {
		next.IncrementalBaselinePolicy = current.IncrementalBaselinePolicy
	}
	if next.DeepScan == nil && current.DeepScan != nil {
		preserved := *current.DeepScan
		if current.DeepScan.Deadline != nil {
			deadline := *current.DeepScan.Deadline
			preserved.Deadline = &deadline
		}
		next.DeepScan = &preserved
	}
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
	if !threatModelBoundToRepositoryScan(model, scan) {
		return fiber.NewError(fiber.StatusNotFound, "threat model not found")
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
		Namespace:                namespace,
		RepositoryScan:           c.Params("name"),
		RepositoryScanUID:        string(scan.UID),
		RepositoryScanGeneration: scan.Generation,
		Content:                  req.Content,
		Source:                   req.Source,
	}
	if model.Source == "" {
		model.Source = "edited"
	}
	if err := h.securityStore.SaveThreatModel(c.Context(), model); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save threat model: %v", err))
	}
	return c.JSON(model)
}

type securityBundleReadManifest struct {
	Repository securitybundle.RepositoryIdentity `json:"repository"`
	Target     securitybundle.TargetSnapshot     `json:"target"`
	Metadata   map[string]string                 `json:"metadata"`
	Run        struct {
		RunUID                   string  `json:"runUid"`
		PublicRunID              *string `json:"publicRunId"`
		Namespace                string  `json:"namespace"`
		RepositoryScanName       string  `json:"repositoryScanName"`
		RepositoryScanUID        string  `json:"repositoryScanUid"`
		RepositoryScanGeneration int64   `json:"repositoryScanGeneration"`
	} `json:"run"`
}

func securityBundleReadAuthorizationScan(
	current *corev1alpha1.RepositoryScan,
	sealed *store.SecurityScanBundle,
) (*corev1alpha1.RepositoryScan, error) {
	if current == nil || sealed == nil {
		return nil, store.ErrNotFound
	}
	var evidence []securitybundle.EvidenceBlob
	if err := json.Unmarshal(sealed.EvidenceJSON, &evidence); err != nil {
		return nil, err
	}
	if err := securitybundle.Verify(&securitybundle.Bundle{
		ManifestJSON: sealed.ManifestJSON, FindingsJSON: sealed.FindingsJSON, CoverageJSON: sealed.CoverageJSON,
		Evidence: evidence, Roots: securitybundle.RootDigests{
			ContentDigest: sealed.ContentDigest, RunReceiptDigest: sealed.RunReceiptDigest,
		},
	}, securitybundle.DefaultLimits()); err != nil {
		return nil, err
	}
	var manifest securityBundleReadManifest
	if err := json.Unmarshal(sealed.ManifestJSON, &manifest); err != nil {
		return nil, err
	}
	if sealed.RepositoryScan != current.Name || manifest.Run.Namespace != current.Namespace ||
		manifest.Run.RepositoryScanName != current.Name || manifest.Run.RepositoryScanUID != string(current.UID) {
		return nil, store.ErrNotFound
	}
	publicRunID := ""
	if manifest.Run.PublicRunID != nil {
		publicRunID = *manifest.Run.PublicRunID
	}
	if manifest.Run.RunUID != sealed.RunUID || publicRunID != sealed.ScanRunID ||
		(sealed.RepositoryScanUID != "" && sealed.RepositoryScanUID != manifest.Run.RepositoryScanUID) ||
		(sealed.RepositoryScanGeneration != 0 && sealed.RepositoryScanGeneration != manifest.Run.RepositoryScanGeneration) {
		return nil, store.ErrNotFound
	}
	authorized := current.DeepCopy()
	authorized.Generation = manifest.Run.RepositoryScanGeneration
	authorized.Spec.RepoURL = manifest.Repository.RepoURL
	if manifest.Repository.SubPath != nil {
		authorized.Spec.SubPath = *manifest.Repository.SubPath
	} else {
		authorized.Spec.SubPath = ""
	}
	authorized.Spec.Branch = manifest.Metadata[security.BundleMetadataAuthorizationBranch]
	authorized.Spec.Ref = manifest.Metadata[security.BundleMetadataAuthorizationRef]
	if authorized.Spec.Branch == "" && authorized.Spec.Ref == "" && manifest.Target.OriginalRef != nil {
		authorized.Spec.Ref = *manifest.Target.OriginalRef
	}
	authorized.Spec.AnalysisAgentRef = corev1alpha1.AgentReference{
		Name:      manifest.Metadata[security.BundleMetadataAuthorizationAgentName],
		Namespace: manifest.Metadata[security.BundleMetadataAuthorizationAgentNamespace],
	}
	if authorized.Spec.AnalysisAgentRef.Name == "" && manifest.Run.RepositoryScanGeneration == current.Generation {
		authorized.Spec.AnalysisAgentRef = current.Spec.AnalysisAgentRef
	}
	return authorized, nil
}

// GetSecurityScanBundle returns one sealed canonical bundle.
func (h *Handlers) GetSecurityScanBundle(c fiber.Ctx) error {
	if h.securityBundleStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "security bundle store not configured")
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getSecurityScanBundle", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	sealed, err := h.securityBundleStore.GetSecurityScanBundle(c.Context(), namespace, c.Params("runID"))
	if err != nil {
		return securityIntegrityHTTPError(err, "get security scan bundle")
	}
	authorizationScan, err := securityBundleReadAuthorizationScan(scan, sealed)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "security scan bundle not found")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to verify security scan bundle: %v", err))
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "getSecurityScanBundle", authorizationScan, authorizationScan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"id": sealed.ID, "scanRunID": sealed.ScanRunID, "runUID": sealed.RunUID, "version": sealed.Version,
		"manifest": json.RawMessage(sealed.ManifestJSON), "findings": json.RawMessage(sealed.FindingsJSON),
		"coverage": json.RawMessage(sealed.CoverageJSON), "evidence": json.RawMessage(sealed.EvidenceJSON),
		"contentDigest":    sealed.ContentDigest,
		"runReceiptDigest": sealed.RunReceiptDigest, "sealedAt": sealed.SealedAt,
	})
}

// GetSecurityScanCoverage returns the canonical sealed coverage document.
func (h *Handlers) GetSecurityScanCoverage(c fiber.Ctx) error {
	if h.securityBundleStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "security bundle store not configured")
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getSecurityScanCoverage", h.contextTokenAuthorization.SecurityReadScopes); err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	sealed, err := h.securityBundleStore.GetSecurityScanBundle(c.Context(), namespace, c.Params("runID"))
	if err != nil {
		return securityIntegrityHTTPError(err, "get security scan coverage")
	}
	authorizationScan, err := securityBundleReadAuthorizationScan(scan, sealed)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "security scan coverage not found")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to verify security scan bundle: %v", err))
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "getSecurityScanCoverage", authorizationScan, authorizationScan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	c.Set("Content-Type", "application/json")
	return c.Send(sealed.CoverageJSON)
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
	filteredRuns := runs[:0]
	for _, run := range runs {
		if run.RepositoryScanUID == "" || run.RepositoryScanGeneration <= 0 ||
			run.RepositoryScanUID != string(scan.UID) || run.RepositoryScanGeneration != scan.Generation {
			continue
		}
		filteredRuns = append(filteredRuns, run)
	}
	runs = filteredRuns
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
	run, err := h.createSecurityScanRun(c.Context(), GetUserInfo(c), scan, security.IncrementalBaselineCommit(scan))
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

	filtered := findings[:0]
	for i := range findings {
		if findings[i].ScanRunID == "" {
			continue
		}
		run, runErr := h.securityStore.GetScanRun(c.Context(), namespace, findings[i].ScanRunID)
		if errors.Is(runErr, store.ErrNotFound) {
			continue
		}
		if runErr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load finding scan run: %v", runErr))
		}
		if run.RepositoryScanUID == "" || run.RepositoryScanGeneration <= 0 ||
			run.RepositoryScanUID != string(scan.UID) || run.RepositoryScanGeneration != scan.Generation {
			continue
		}
		findings[i].ScanTaskName = run.TaskName
		filtered = append(filtered, findings[i])
	}
	findings = filtered

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
	reviewSlices, next, err := h.securityStore.ListReviewSlices(c.Context(), store.ReviewSliceFilter{
		Namespace:      namespace,
		RepositoryScan: c.Params("name"),
		Status:         c.Query("status"),
		Limit:          limit,
		Cursor:         c.Query("cursor"),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list review slices: %v", err))
	}
	filtered := make([]store.ReviewSlice, 0, len(reviewSlices))
	for i := range reviewSlices {
		bound, bindErr := h.securityRunBoundToRepositoryScan(c.Context(), scan, reviewSlices[i].LastScanRunID)
		if bindErr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to verify review slice scan run: %v", bindErr))
		}
		if bound {
			filtered = append(filtered, reviewSlices[i])
		}
	}
	return c.JSON(fiber.Map{"items": filtered, "metadata": fiber.Map{"continue": next}})
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
	bound, err := h.securityRunBoundToRepositoryScan(c.Context(), scan, slice.LastScanRunID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to verify review slice scan run: %v", err))
	}
	if !bound {
		return fiber.NewError(fiber.StatusNotFound, "review slice not found")
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
	filtered := make([]store.DroppedFinding, 0, len(dropped))
	for i := range dropped {
		bound, bindErr := h.securityRunBoundToRepositoryScan(c.Context(), scan, dropped[i].ScanRunID)
		if bindErr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to verify dropped finding scan run: %v", bindErr))
		}
		if bound {
			filtered = append(filtered, dropped[i])
		}
	}
	return c.JSON(fiber.Map{"items": filtered, "metadata": fiber.Map{"continue": next}})
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
	if finding.ScanRunID == "" {
		return fiber.NewError(fiber.StatusNotFound, "finding not found")
	}
	run, err := h.securityStore.GetScanRun(c.Context(), namespace, finding.ScanRunID)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "finding not found")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load finding scan run: %v", err))
	}
	if run.RepositoryScanUID == "" || run.RepositoryScanGeneration <= 0 ||
		run.RepositoryScanUID != string(scan.UID) || run.RepositoryScanGeneration != scan.Generation {
		return fiber.NewError(fiber.StatusNotFound, "finding not found")
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, "getSecurityFinding", scan, scan.Spec.AnalysisAgentRef); err != nil {
		return err
	}
	finding.ScanTaskName = run.TaskName
	return c.JSON(finding)
}

func authenticatedDecisionActor(c fiber.Ctx) (subject, issuer, source string) {
	ui := GetUserInfo(c)
	if ui == nil {
		return contextTokenAuthorizationReasonUnknown, "", contextTokenAuthorizationReasonUnknown
	}
	subject = strings.TrimSpace(ui.Subject)
	if subject == "" {
		subject = strings.TrimSpace(ui.UID)
	}
	if subject == "" {
		subject = strings.TrimSpace(ui.Username)
	}
	if subject == "" {
		subject = contextTokenAuthorizationReasonUnknown
	}
	source = strings.TrimSpace(ui.AuthType)
	if source == "" {
		source = contextTokenAuthorizationReasonUnknown
	}
	return subject, strings.TrimSpace(ui.Issuer), source
}

type securityFindingAuthorization struct {
	finding *store.Finding
	scan    *corev1alpha1.RepositoryScan
	run     *store.ScanRun
}

func (h *Handlers) authorizedSecurityFindingWithAgent(
	c fiber.Ctx,
	action string,
	write bool,
	agentRefForScan func(*corev1alpha1.RepositoryScan) corev1alpha1.AgentReference,
) (*securityFindingAuthorization, error) {
	if err := h.ensureSecurityStore(); err != nil {
		return nil, err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return nil, err
	}
	scopes := h.contextTokenAuthorization.SecurityReadScopes
	if write {
		scopes = h.contextTokenAuthorization.SecurityWriteScopes
	}
	if err := h.authorizeContextTokenAction(c, action, scopes); err != nil {
		return nil, err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, finding.RepositoryScan)
	if err != nil {
		return nil, err
	}
	if finding.ScanRunID == "" {
		return nil, fiber.NewError(fiber.StatusConflict, "legacy finding has no verifiable scan run")
	}
	run, runErr := h.securityStore.GetScanRun(c.Context(), namespace, finding.ScanRunID)
	if runErr != nil && !errors.Is(runErr, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load finding scan run: %v", runErr))
	}
	if errors.Is(runErr, store.ErrNotFound) || run.RepositoryScanUID == "" {
		if write {
			return nil, fiber.NewError(fiber.StatusConflict, "legacy finding has no verifiable RepositoryScan UID")
		}
		return nil, fiber.NewError(fiber.StatusNotFound, "finding history not found")
	}
	if run.RepositoryScanUID != string(scan.UID) {
		return nil, fiber.NewError(fiber.StatusNotFound, "finding not found")
	}
	if run.RepositoryScanGeneration == 0 || run.RepositoryScanGeneration != scan.Generation {
		if write {
			return nil, fiber.NewError(fiber.StatusConflict, "historical finding target has no available authorization snapshot")
		}
		return nil, fiber.NewError(fiber.StatusNotFound, "finding history not found")
	}
	agentRef := scan.Spec.AnalysisAgentRef
	if agentRefForScan != nil {
		agentRef = agentRefForScan(scan)
	}
	if err := h.authorizeContextTokenSecurityScanTask(c, action, scan, agentRef); err != nil {
		return nil, err
	}
	return &securityFindingAuthorization{finding: finding, scan: scan, run: run}, nil
}

func (h *Handlers) authorizedSecurityFinding(c fiber.Ctx, action string, write bool) (*store.Finding, error) {
	authorized, err := h.authorizedSecurityFindingWithAgent(c, action, write, nil)
	if err != nil {
		return nil, err
	}
	return authorized.finding, nil
}

func (h *Handlers) authorizedSecurityFindingHistory(c fiber.Ctx, action string) (*securityFindingAuthorization, error) {
	// Historical authorization receipts are not available yet. Fail closed to
	// the current RepositoryScan generation instead of authorizing old targets
	// with the current mutable spec and actor context.
	return h.authorizedSecurityFindingWithAgent(c, action, false, nil)
}

func patchProposalMatchesAuthorizedFinding(proposal *store.PatchProposal, authorized *securityFindingAuthorization) bool {
	if proposal == nil || authorized == nil || authorized.finding == nil || authorized.scan == nil || authorized.run == nil {
		return false
	}
	finding := authorized.finding
	if finding.ScanRunID == "" {
		return false
	}
	expectedHead, ok := security.NormalizeFullGitObjectID(authorized.run.HeadCommit)
	if !ok {
		return false
	}
	proposalHead, ok := security.NormalizeFullGitObjectID(proposal.SourceHeadSHA)
	if !ok {
		return false
	}
	occurrenceMatches := proposal.OccurrenceID == finding.CurrentOccurrenceID
	if finding.CurrentOccurrenceID == "" {
		// Gate-off findings predate immutable occurrences. Keep their existing
		// patch flow only when both sides are explicitly legacy-unbound and the
		// proposal is still pinned to the current source run and full head SHA.
		occurrenceMatches = proposal.OccurrenceID == ""
	}
	return proposal.Namespace == finding.Namespace &&
		proposal.RepositoryScan == finding.RepositoryScan &&
		proposal.FindingID == finding.ID && occurrenceMatches &&
		proposal.SourceScanRunID == finding.ScanRunID &&
		proposalHead == expectedHead
}

// AppendSecurityFindingDecision appends one authenticated, immutable lifecycle decision.
func (h *Handlers) AppendSecurityFindingDecision(c fiber.Ctx) error {
	if err := h.ensureSecurityIntegrityStore(); err != nil {
		return err
	}
	finding, err := h.authorizedSecurityFinding(c, "appendSecurityFindingDecision", true)
	if err != nil {
		return err
	}
	var req AppendFindingDecisionRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if strings.TrimSpace(req.DecisionID) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "decisionId is required")
	}
	if req.ExpectedDecisionVersion == nil {
		return fiber.NewError(fiber.StatusBadRequest, "expectedDecisionVersion is required")
	}
	if req.Action == store.FindingDecisionSuppress {
		return fiber.NewError(fiber.StatusForbidden, "suppression decisions require a dedicated authorization policy")
	}
	subject, issuer, authSource := authenticatedDecisionActor(c)
	decision := &store.FindingDecision{
		ID:                      strings.TrimSpace(req.DecisionID),
		Namespace:               finding.Namespace,
		RepositoryScan:          finding.RepositoryScan,
		PublicFindingID:         finding.ID,
		Scope:                   req.Scope,
		OccurrenceID:            strings.TrimSpace(req.OccurrenceID),
		Action:                  req.Action,
		ReasonCode:              strings.TrimSpace(req.ReasonCode),
		Reason:                  strings.TrimSpace(req.Reason),
		EvidenceReceiptIDs:      append([]string(nil), req.EvidenceReceiptIDs...),
		SupersedesDecisionID:    strings.TrimSpace(req.SupersedesDecisionID),
		ExpectedDecisionVersion: *req.ExpectedDecisionVersion,
		Applicability:           req.Applicability,
		ActorSubject:            subject,
		ActorIssuer:             issuer,
		AuthenticationSource:    authSource,
		Source:                  "api",
	}
	appended, err := h.securityIntegrityStore.AppendFindingDecision(c.Context(), decision)
	if err != nil {
		return securityIntegrityHTTPError(err, "append finding decision")
	}
	return c.Status(fiber.StatusCreated).JSON(appended)
}

const (
	findingHistoryDefaultLimit    = "50"
	findingHistoryFilterBatchSize = 100
	findingHistoryScanBudget      = 2000
)

type findingHistoryPagination struct {
	Limit    int
	Continue string
}

func parseFindingHistoryPagination(c fiber.Ctx) (*findingHistoryPagination, error) {
	rawLimit := c.Query("limit", findingHistoryDefaultLimit)
	limit, err := strconv.Atoi(rawLimit)
	if err != nil {
		return nil, fmt.Errorf("invalid limit parameter: %w", err)
	}
	if limit < 1 {
		return nil, fmt.Errorf("limit must be at least 1")
	}
	if limit > MaxLimit {
		return nil, fmt.Errorf("limit must not exceed %d", MaxLimit)
	}
	return &findingHistoryPagination{Limit: limit, Continue: c.Query("cursor")}, nil
}

// listAuthorizedFindingHistory returns up to limit authorized records while
// scanning at most findingHistoryScanBudget raw records per request. Pages are
// bounded by the remaining output capacity, so every returned continuation
// cursor points after all records examined by this response and cannot skip an
// unexamined authorized record.
func listAuthorizedFindingHistory[T any](
	limit int,
	cursor string,
	list func(limit int, cursor string) ([]T, string, error),
	authorized func(*T) (bool, error),
) ([]T, string, error) {
	items := make([]T, 0, limit)
	next := cursor
	scanned := 0
	for len(items) < limit && scanned < findingHistoryScanBudget {
		remainingOutput := limit - len(items)
		remainingBudget := findingHistoryScanBudget - scanned
		batchLimit := min(findingHistoryFilterBatchSize, remainingOutput, remainingBudget)
		before := next
		page, after, err := list(batchLimit, before)
		if err != nil {
			return nil, "", err
		}
		if len(page) > batchLimit {
			return nil, "", fmt.Errorf("finding history store returned %d records for limit %d", len(page), batchLimit)
		}
		if len(page) == 0 {
			if after == "" {
				return items, "", nil
			}
			if after == before {
				return nil, "", fmt.Errorf("finding history cursor did not advance")
			}
			next = after
			continue
		}
		for i := range page {
			allowed, err := authorized(&page[i])
			if err != nil {
				return nil, "", err
			}
			if allowed {
				items = append(items, page[i])
			}
		}
		scanned += len(page)
		if after == "" {
			return items, "", nil
		}
		if after == before {
			return nil, "", fmt.Errorf("finding history cursor did not advance")
		}
		next = after
	}
	return items, next, nil
}

func (h *Handlers) findingHistoryRunAuthorized(
	ctx context.Context,
	authorized *securityFindingAuthorization,
	runID string,
	cache map[string]bool,
) (bool, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || authorized == nil || authorized.scan == nil || authorized.finding == nil {
		return false, nil
	}
	if allowed, ok := cache[runID]; ok {
		return allowed, nil
	}
	run, err := h.securityStore.GetScanRun(ctx, authorized.finding.Namespace, runID)
	if errors.Is(err, store.ErrNotFound) {
		cache[runID] = false
		return false, nil
	}
	if err != nil {
		return false, err
	}
	allowed := run.RepositoryScan == authorized.scan.Name && run.RepositoryScanUID != "" &&
		run.RepositoryScanUID == string(authorized.scan.UID) &&
		run.RepositoryScanGeneration > 0 && run.RepositoryScanGeneration == authorized.scan.Generation
	cache[runID] = allowed
	return allowed, nil
}

// ListSecurityFindingOccurrences returns immutable occurrences for a public finding.
func (h *Handlers) ListSecurityFindingOccurrences(c fiber.Ctx) error {
	if err := h.ensureSecurityIntegrityStore(); err != nil {
		return err
	}
	authorized, err := h.authorizedSecurityFindingHistory(c, "listSecurityFindingOccurrences")
	if err != nil {
		return err
	}
	pagination, err := parseFindingHistoryPagination(c)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	finding := authorized.finding
	runAuthorization := make(map[string]bool)
	items, next, err := listAuthorizedFindingHistory(
		pagination.Limit,
		pagination.Continue,
		func(limit int, cursor string) ([]store.FindingOccurrence, string, error) {
			return h.securityIntegrityStore.ListFindingOccurrences(c.Context(), store.FindingOccurrenceFilter{
				Namespace: finding.Namespace, RepositoryScan: finding.RepositoryScan, PublicFindingID: finding.ID,
				Limit: limit, Cursor: cursor,
			})
		},
		func(occurrence *store.FindingOccurrence) (bool, error) {
			return h.findingHistoryRunAuthorized(c.Context(), authorized, occurrence.ScanRunID, runAuthorization)
		},
	)
	if err != nil {
		return securityIntegrityHTTPError(err, "list finding occurrences")
	}
	return c.JSON(fiber.Map{"items": items, "metadata": fiber.Map{"continue": next}})
}

// ListSecurityFindingDecisions returns immutable lifecycle decisions.
func (h *Handlers) ListSecurityFindingDecisions(c fiber.Ctx) error {
	if err := h.ensureSecurityIntegrityStore(); err != nil {
		return err
	}
	authorized, err := h.authorizedSecurityFindingHistory(c, "listSecurityFindingDecisions")
	if err != nil {
		return err
	}
	pagination, err := parseFindingHistoryPagination(c)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	finding := authorized.finding
	runAuthorization := make(map[string]bool)
	occurrences := make(map[string]*store.FindingOccurrence)
	items, next, err := listAuthorizedFindingHistory(
		pagination.Limit,
		pagination.Continue,
		func(limit int, cursor string) ([]store.FindingDecision, string, error) {
			return h.securityIntegrityStore.ListFindingDecisions(c.Context(), store.FindingDecisionFilter{
				Namespace: finding.Namespace, RepositoryScan: finding.RepositoryScan, PublicFindingID: finding.ID,
				Limit: limit, Cursor: cursor,
			})
		},
		func(decision *store.FindingDecision) (bool, error) {
			occurrenceID := strings.TrimSpace(decision.OccurrenceID)
			if occurrenceID == "" {
				return false, nil
			}
			occurrence, ok := occurrences[occurrenceID]
			if !ok {
				loaded, getErr := h.securityIntegrityStore.GetFindingOccurrence(c.Context(), finding.Namespace, occurrenceID)
				if errors.Is(getErr, store.ErrNotFound) {
					occurrences[occurrenceID] = nil
					return false, nil
				}
				if getErr != nil {
					return false, getErr
				}
				occurrence = loaded
				occurrences[occurrenceID] = occurrence
			}
			if occurrence == nil || occurrence.RepositoryScan != finding.RepositoryScan ||
				occurrence.PublicFindingID != finding.ID {
				return false, nil
			}
			return h.findingHistoryRunAuthorized(c.Context(), authorized, occurrence.ScanRunID, runAuthorization)
		},
	)
	if err != nil {
		return securityIntegrityHTTPError(err, "list finding decisions")
	}
	return c.JSON(fiber.Map{"items": items, "metadata": fiber.Map{"continue": next}})
}

// ListSecurityFindingAssessments returns immutable validation/attack-path assessments.
func (h *Handlers) ListSecurityFindingAssessments(c fiber.Ctx) error {
	if err := h.ensureSecurityIntegrityStore(); err != nil {
		return err
	}
	authorized, err := h.authorizedSecurityFindingHistory(c, "listSecurityFindingAssessments")
	if err != nil {
		return err
	}
	pagination, err := parseFindingHistoryPagination(c)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	finding := authorized.finding
	runAuthorization := make(map[string]bool)
	items, next, err := listAuthorizedFindingHistory(
		pagination.Limit,
		pagination.Continue,
		func(limit int, cursor string) ([]store.FindingAssessment, string, error) {
			return h.securityIntegrityStore.ListFindingAssessments(c.Context(), store.FindingAssessmentFilter{
				Namespace: finding.Namespace, RepositoryScan: finding.RepositoryScan, PublicFindingID: finding.ID,
				Kind: store.FindingAssessmentKind(c.Query("kind")), Limit: limit, Cursor: cursor,
			})
		},
		func(assessment *store.FindingAssessment) (bool, error) {
			return h.findingHistoryRunAuthorized(c.Context(), authorized, assessment.ScanRunID, runAuthorization)
		},
	)
	if err != nil {
		return securityIntegrityHTTPError(err, "list finding assessments")
	}
	return c.JSON(fiber.Map{"items": items, "metadata": fiber.Map{"continue": next}})
}

func securityIntegrityHTTPError(err error, action string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrDuplicateMismatch):
		return fiber.NewError(fiber.StatusConflict, err.Error())
	case errors.Is(err, store.ErrValidation):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	default:
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to %s: %v", action, err))
	}
}

func legacyDecisionID(finding *store.Finding, action store.FindingDecisionAction) string {
	input := fmt.Sprintf("legacy-api\x00%s\x00%s\x00%s\x00%d", finding.Namespace, finding.ID, action, finding.DecisionVersion)
	digest := sha256.Sum256([]byte(input))
	return "decision_" + hex.EncodeToString(digest[:])
}

func (h *Handlers) appendLegacyFindingDecision(c fiber.Ctx, finding *store.Finding, action store.FindingDecisionAction) error {
	if h.securityIntegrityStore == nil || strings.TrimSpace(finding.CurrentOccurrenceID) == "" {
		return store.ErrNotReady
	}
	subject, issuer, authSource := authenticatedDecisionActor(c)
	_, err := h.securityIntegrityStore.AppendFindingDecision(c.Context(), &store.FindingDecision{
		ID:                      legacyDecisionID(finding, action),
		Namespace:               finding.Namespace,
		RepositoryScan:          finding.RepositoryScan,
		PublicFindingID:         finding.ID,
		Scope:                   store.FindingDecisionOccurrence,
		OccurrenceID:            finding.CurrentOccurrenceID,
		Action:                  action,
		ExpectedDecisionVersion: finding.DecisionVersion,
		ActorSubject:            subject,
		ActorIssuer:             issuer,
		AuthenticationSource:    authSource,
		Source:                  "legacy-api",
	})
	return err
}

// DismissSecurityFinding marks a finding as dismissed.
func (h *Handlers) DismissSecurityFinding(c fiber.Ctx) error {
	finding, err := h.authorizedSecurityFinding(c, "dismissSecurityFinding", true)
	if err != nil {
		return err
	}
	namespace := finding.Namespace
	if finding.State == "dismissed" {
		return c.SendStatus(fiber.StatusNoContent)
	}
	if h.securityIntegrityStore != nil {
		if err := h.appendLegacyFindingDecision(c, finding, store.FindingDecisionCloseWontFix); err == nil {
			return c.SendStatus(fiber.StatusNoContent)
		} else if !errors.Is(err, store.ErrNotReady) {
			return securityIntegrityHTTPError(err, "dismiss finding")
		}
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
	finding, err := h.authorizedSecurityFinding(c, "reopenSecurityFinding", true)
	if err != nil {
		return err
	}
	namespace := finding.Namespace
	if finding.State == "open" {
		return c.SendStatus(fiber.StatusNoContent)
	}
	if h.securityIntegrityStore != nil {
		if err := h.appendLegacyFindingDecision(c, finding, store.FindingDecisionReopen); err == nil {
			return c.SendStatus(fiber.StatusNoContent)
		} else if !errors.Is(err, store.ErrNotReady) {
			return securityIntegrityHTTPError(err, "reopen finding")
		}
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
func validationAssessmentBlocksManualRequest(assessment store.FindingAssessment) bool {
	return assessment.Method != "policy" || assessment.Outcome != securityAssessmentOutcomeDeferred
}

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
	authorized, err := h.authorizedSecurityFindingWithAgent(
		c, "listSecurityPatchProposals", false, securityPatchAgentRef,
	)
	if err != nil {
		return err
	}
	proposals, err := h.securityStore.ListPatchProposals(c.Context(), authorized.finding.Namespace, authorized.finding.ID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list patch proposals: %v", err))
	}
	current := make([]store.PatchProposal, 0, len(proposals))
	for i := range proposals {
		if patchProposalMatchesAuthorizedFinding(&proposals[i], authorized) {
			current = append(current, proposals[i])
		}
	}
	return c.JSON(fiber.Map{"items": current})
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
	authorized, err := h.authorizedSecurityFindingWithAgent(
		c, "createSecurityPullRequest", true, securityPatchAgentRef,
	)
	if err != nil {
		return err
	}
	finding := authorized.finding
	scan := authorized.scan
	namespace := finding.Namespace

	proposals, err := h.securityStore.ListPatchProposals(c.Context(), namespace, finding.ID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list patch proposals: %v", err))
	}
	var proposal *store.PatchProposal
	hasUnboundSuccessfulProposal := false
	for i := range proposals {
		if proposals[i].Status != githubMutationStatusSucceeded {
			continue
		}
		if !patchProposalMatchesAuthorizedFinding(&proposals[i], authorized) {
			hasUnboundSuccessfulProposal = true
			continue
		}
		proposal = &proposals[i]
		break
	}
	if proposal == nil {
		if hasUnboundSuccessfulProposal {
			return fiber.NewError(fiber.StatusConflict, "no successful patch proposal is bound to the finding's current occurrence and source target")
		}
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
