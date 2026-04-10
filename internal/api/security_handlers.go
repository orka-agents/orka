package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/internal/store"
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

func (h *Handlers) normalizeRepositoryScanSpec(spec *corev1alpha1.RepositoryScanSpec) {
	if spec.Provider == "" {
		spec.Provider = "github"
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

func (h *Handlers) createSecurityScanTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit string) (*store.ScanRun, error) {
	if err := h.ensureSecurityStore(); err != nil {
		return nil, err
	}

	var threatModel string
	if model, err := h.securityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name); err == nil {
		threatModel = model.Content
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load threat model: %v", err))
	}

	taskName := security.ScanTaskName(scan.Name, mode)
	scanID := security.ScanRunID(taskName)
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(700)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:        "true",
				labels.LabelCreatedBy:      "repository-security",
				labels.LabelSecurityTarget: scan.Name,
				labels.LabelSecurityScanID: scanID,
				labels.LabelSecurityMode:   mode,
			},
			OwnerReferences: []metav1.OwnerReference{h.ownerRefForRepositoryScan(scan)},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildScanPrompt(scan, mode, baseCommit, headCommit, threatModel),
			Timeout:  &timeout,
			Priority: &priority,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveBranch(scan),
					GitSecretRef: scan.Spec.GitSecretRef,
					SubPath:      scan.Spec.SubPath,
					ForkRepo:     scan.Spec.ForkRepo,
					PRBaseBranch: scan.Spec.PRBaseBranch,
				},
			},
		},
	}
	if err := h.client.Create(ctx, task); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create scan task: %v", err))
	}

	run := &store.ScanRun{
		ID:             scanID,
		Namespace:      scan.Namespace,
		RepositoryScan: scan.Name,
		TaskName:       taskName,
		Mode:           mode,
		Phase:          "pending",
		BaseCommit:     baseCommit,
		HeadCommit:     headCommit,
		StartedAt:      time.Now(),
	}
	if err := h.securityStore.CreateScanRun(ctx, run); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create scan run: %v", err))
	}
	return run, nil
}

func (h *Handlers) createSecurityPatchTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, finding *store.Finding) (*store.PatchProposal, error) {
	if err := h.ensureSecurityStore(); err != nil {
		return nil, err
	}

	agentRef := scan.Spec.AnalysisAgentRef
	if scan.Spec.PatchAgentRef != nil {
		agentRef = *scan.Spec.PatchAgentRef
	}

	taskName := security.PatchTaskName(scan.Name, finding.ID)
	proposalID := security.PatchProposalID(taskName)
	branch := security.PatchBranch(finding.ID)
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(750)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:           "true",
				labels.LabelCreatedBy:         "repository-security",
				labels.LabelSecurityTarget:    scan.Name,
				labels.LabelSecurityScanID:    proposalID,
				labels.LabelSecurityMode:      "patch",
				labels.LabelSecurityFindingID: finding.ID,
			},
			OwnerReferences: []metav1.OwnerReference{h.ownerRefForRepositoryScan(scan)},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &agentRef,
			Prompt:   security.BuildPatchPrompt(scan, finding),
			Timeout:  &timeout,
			Priority: &priority,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveBranch(scan),
					GitSecretRef: scan.Spec.GitSecretRef,
					SubPath:      scan.Spec.SubPath,
					ForkRepo:     scan.Spec.ForkRepo,
					PRBaseBranch: scan.Spec.PRBaseBranch,
					PushBranch:   branch,
				},
			},
		},
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

	return c.JSON(ListResponse{
		Items: list.Items,
		Metadata: ListMeta{
			Continue:           list.Continue,
			RemainingItemCount: list.RemainingItemCount,
		},
	})
}

// GetRepositoryScan returns a repository scan configuration.
func (h *Handlers) GetRepositoryScan(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
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
	if req.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	if req.Spec.RepoURL == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.repoURL is required")
	}
	if req.Spec.AnalysisAgentRef.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.analysisAgentRef.name is required")
	}

	namespace, err := h.resolveNamespace(c, req.Namespace)
	if err != nil {
		return err
	}
	h.normalizeRepositoryScanSpec(&req.Spec)

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: req.Spec,
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
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}

	if req.Spec.RepoURL == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.repoURL is required")
	}
	if req.Spec.AnalysisAgentRef.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.analysisAgentRef.name is required")
	}

	h.normalizeRepositoryScanSpec(&req.Spec)
	scan.Spec = req.Spec
	if err := h.client.Update(c.Context(), scan); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update repository scan: %v", err))
	}
	return c.JSON(scan)
}

// DeleteRepositoryScan deletes a repository scan configuration.
func (h *Handlers) DeleteRepositoryScan(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), scan); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete repository scan: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// GetThreatModel returns the latest threat model for a repository.
func (h *Handlers) GetThreatModel(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if _, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name")); err != nil {
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

// UpdateThreatModel saves a new threat model version.
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
	if _, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name")); err != nil {
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
	if _, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name")); err != nil {
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
	scan, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return err
	}
	run, err := h.createSecurityScanTask(c.Context(), scan, "manual", scan.Status.LastProcessedCommit, "")
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
	if _, err := h.fetchRepositoryScan(c.Context(), namespace, c.Params("name")); err != nil {
		return err
	}

	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	findings, next, err := h.securityStore.ListFindings(c.Context(), store.FindingFilter{
		Namespace:        namespace,
		RepositoryScan:   c.Params("name"),
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

// GetSecurityFinding returns a finding by ID.
func (h *Handlers) GetSecurityFinding(c fiber.Ctx) error {
	if err := h.ensureSecurityStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	finding, err := h.securityStore.GetFinding(c.Context(), namespace, c.Params("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get finding: %v", err))
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
	if err := h.securityStore.UpdateFindingState(c.Context(), namespace, c.Params("id"), "open"); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "finding not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to reopen finding: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
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

	proposal, err := h.createSecurityPatchTask(c.Context(), scan, finding)
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
	proposals, err := h.securityStore.ListPatchProposals(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list patch proposals: %v", err))
	}
	return c.JSON(fiber.Map{"items": proposals})
}

func extractGitToken(secret *corev1.Secret) string {
	for _, key := range []string{"token", "password", "GITHUB_TOKEN"} {
		if value, ok := secret.Data[key]; ok {
			token := strings.TrimSpace(string(value))
			if token != "" {
				return token
			}
		}
	}
	return ""
}

func createGitHubPullRequest(ctx context.Context, token, owner, repo, head, base, title, body string) (string, int, error) {
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, repo)
	payload, _ := json.Marshal(map[string]string{
		"title": title,
		"body":  body,
		"head":  head,
		"base":  base,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close() //nolint:errcheck

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return "", 0, err
	}
	return result.HTMLURL, result.Number, nil
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

	prURL, prNumber, err := createGitHubPullRequest(c.Context(), token, owner, repo, proposal.Branch, baseBranch, title, body)
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
		"status":   "created",
	})
}
