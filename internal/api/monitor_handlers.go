package api

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/metrics"
	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/workerenv"
)

type CreateRepositoryMonitorRequest struct {
	Name      string                             `json:"name"`
	Namespace string                             `json:"namespace"`
	Metadata  MetadataRequest                    `json:"metadata"`
	Spec      corev1alpha1.RepositoryMonitorSpec `json:"spec"`
}

type UpdateRepositoryMonitorRequest struct {
	Spec corev1alpha1.RepositoryMonitorSpec `json:"spec"`
}

type CreateRepositoryMonitorRunRequest struct {
	TargetKind   string `json:"targetKind,omitempty"`
	TargetNumber int64  `json:"targetNumber,omitempty"`
	TargetSHA    string `json:"targetSHA,omitempty"`
}

// CreateRepositoryMonitorCommandRequest records an explicit monitor command from the Orka API.
type CreateRepositoryMonitorCommandRequest struct {
	Kind      string `json:"kind"`
	Number    int64  `json:"number"`
	Intent    string `json:"intent"`
	TargetSHA string `json:"targetSHA,omitempty"`
}

const (
	repositoryMonitorRunRequestAnnotation  = "orka.ai/repository-monitor-run-requested-at"
	repositoryMonitorTargetKindPullRequest = "pull_request"
	repositoryMonitorTargetKindIssue       = "issue"
	repositoryMonitorRunPhaseQueued        = "queued"
	repositoryMonitorRunPhaseRunning       = "running"
	repositoryMonitorRunPhaseFailed        = "failed"
)

func (h *Handlers) ensureRepositoryMonitorStore() error {
	if h.repositoryMonitorStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "repository monitor store not configured")
	}
	return nil
}

func (h *Handlers) normalizeRepositoryMonitorSpec(spec *corev1alpha1.RepositoryMonitorSpec) {
	if spec.Provider == "" {
		spec.Provider = sourceProviderGitHub
	}
	if spec.Branch == "" {
		spec.Branch = "main"
	}
	if owner, repo, err := parseRepositoryMonitorGitHubURL(spec.RepoURL); err == nil {
		spec.Owner = owner
		spec.Repository = repo
	} else if spec.Owner == "" || spec.Repository == "" {
		owner, repo := security.ParseRepositoryURL(spec.RepoURL)
		if spec.Owner == "" {
			spec.Owner = owner
		}
		if spec.Repository == "" {
			spec.Repository = repo
		}
	}
	if spec.Targets.PullRequests.Enabled == nil && !spec.Targets.Issues.Enabled && !spec.Targets.Commits.Enabled {
		enabled := true
		spec.Targets.PullRequests.Enabled = &enabled
	}
	if spec.Targets.PullRequests.MaxPerRun == nil {
		maxPerRun := int32(20)
		spec.Targets.PullRequests.MaxPerRun = &maxPerRun
	}
	if spec.Review.Event == "" {
		spec.Review.Event = "COMMENT"
	}
	if spec.Validation.Mode == "" {
		spec.Validation.Mode = "changed"
	}
}

func validateRepositoryMonitorSpec(spec corev1alpha1.RepositoryMonitorSpec) error {
	if strings.TrimSpace(spec.RepoURL) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.repoURL is required")
	}
	if spec.Provider != "" && spec.Provider != sourceProviderGitHub {
		return fiber.NewError(fiber.StatusBadRequest, "spec.provider must be github")
	}
	if _, _, err := parseRepositoryMonitorGitHubURL(spec.RepoURL); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if err := validateRepositoryMonitorSupportedTargets(spec); err != nil {
		return err
	}
	if err := validateRepositoryMonitorReviewPublishSpec(spec.Review.Publish); err != nil {
		return err
	}
	if repositoryMonitorPullRequestsEnabled(spec) && (spec.Agents.Reviewer == nil || strings.TrimSpace(spec.Agents.Reviewer.Name) == "") {
		return fiber.NewError(fiber.StatusBadRequest, "spec.agents.reviewer.name is required when pull request monitoring is enabled")
	}
	return nil
}

func validateRepositoryMonitorReviewPublishSpec(publish corev1alpha1.RepositoryMonitorReviewPublishSpec) error {
	if mode := strings.TrimSpace(publish.Mode); mode != "" && mode != "summary_only" && mode != "summary_with_inline_findings" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.review.publish.mode must be summary_only or summary_with_inline_findings")
	}
	if event := strings.TrimSpace(publish.Event); event != "" && event != "COMMENT" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.review.publish.event only supports COMMENT in v1")
	}
	if policy := strings.TrimSpace(publish.SameHeadPolicy); policy != "" && policy != "skip" {
		return fiber.NewError(fiber.StatusBadRequest, "spec.review.publish.sameHeadPolicy only supports skip in v1")
	}
	if priority := strings.TrimSpace(publish.Inline.MinPriority); priority != "" {
		switch priority {
		case "P0", "P1", "P2", "P3":
		default:
			return fiber.NewError(fiber.StatusBadRequest, "spec.review.publish.inline.minPriority must be one of P0, P1, P2, or P3")
		}
	}
	if publish.Inline.MaxComments != nil {
		if *publish.Inline.MaxComments < 0 || *publish.Inline.MaxComments > 50 {
			return fiber.NewError(fiber.StatusBadRequest, "spec.review.publish.inline.maxComments must be between 0 and 50")
		}
	}
	return nil
}

func (h *Handlers) validateRepositoryMonitorReviewerAgent(c fiber.Ctx, namespace string, spec corev1alpha1.RepositoryMonitorSpec) error {
	if !repositoryMonitorPullRequestsEnabled(spec) {
		return nil
	}
	reviewer := spec.Agents.Reviewer
	if reviewer == nil || strings.TrimSpace(reviewer.Name) == "" {
		return nil
	}
	agentNamespace := reviewer.Namespace
	if agentNamespace == "" {
		agentNamespace = namespace
	}
	if h.enforceNamespaceIsolation && agentNamespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.agents.reviewer namespace %q must match monitor namespace %q when namespace isolation is enforced", agentNamespace, namespace))
	}
	var agent corev1alpha1.Agent
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: reviewer.Name, Namespace: agentNamespace}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.agents.reviewer %q not found in namespace %q", reviewer.Name, agentNamespace))
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get reviewer agent %q: %v", reviewer.Name, err))
	}
	if agent.Spec.Runtime == nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.agents.reviewer %q must use the claude runtime for read-only repository monitor reviews", reviewer.Name))
	}
	if agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeClaude {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.agents.reviewer %q runtime %q is not supported for read-only repository monitor reviews; use claude", reviewer.Name, agent.Spec.Runtime.Type))
	}
	if agent.Spec.SecretRef == nil || strings.TrimSpace(agent.Spec.SecretRef.Name) == "" {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.agents.reviewer %q must reference a Secret with Claude credentials for read-only repository monitor reviews", reviewer.Name))
	}
	secretName := strings.TrimSpace(agent.Spec.SecretRef.Name)
	var secret corev1.Secret
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: secretName, Namespace: namespace}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.agents.reviewer %q credential Secret %q not found in monitor namespace %q", reviewer.Name, secretName, namespace))
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get spec.agents.reviewer %q credential Secret %q: %v", reviewer.Name, secretName, err))
	}
	if !repositoryMonitorClaudeSecretHasCredential(&secret) {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.agents.reviewer %q credential Secret %q must contain a supported Claude auth key", reviewer.Name, secretName))
	}
	return nil
}

func (h *Handlers) validateRepositoryMonitorGitSecret(c fiber.Ctx, namespace string, spec corev1alpha1.RepositoryMonitorSpec) error {
	if spec.GitSecretRef == nil || strings.TrimSpace(spec.GitSecretRef.Name) == "" {
		return nil
	}
	secretName := strings.TrimSpace(spec.GitSecretRef.Name)
	var secret corev1.Secret
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: secretName, Namespace: namespace}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.gitSecretRef %q not found in namespace %q", secretName, namespace))
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get spec.gitSecretRef %q: %v", secretName, err))
	}
	if !repositoryMonitorGitSecretHasToken(&secret) {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("spec.gitSecretRef %q must contain a non-empty token, password, or %s key", secretName, workerenv.GitHubToken))
	}
	return nil
}

func repositoryMonitorGitSecretHasToken(secret *corev1.Secret) bool {
	if secret == nil {
		return false
	}
	for _, key := range []string{"token", "password", workerenv.GitHubToken} {
		if value := strings.TrimSpace(string(secret.Data[key])); value != "" {
			return true
		}
	}
	return false
}

func repositoryMonitorClaudeSecretHasCredential(secret *corev1.Secret) bool {
	if secret == nil {
		return false
	}
	for _, key := range []string{workerenv.AnthropicAPIKey, "ANTHROPIC_FOUNDRY_API_KEY"} {
		if value := strings.TrimSpace(string(secret.Data[key])); value != "" {
			return true
		}
	}
	return false
}

func validateRepositoryMonitorSupportedTargets(spec corev1alpha1.RepositoryMonitorSpec) error {
	if spec.Targets.Commits.Enabled {
		return fiber.NewError(fiber.StatusBadRequest, "spec.targets.commits is not supported; only pull request and issue monitoring are supported")
	}
	if !repositoryMonitorPullRequestsEnabled(spec) && !spec.Targets.Issues.Enabled {
		return fiber.NewError(fiber.StatusBadRequest, "at least one repository monitor target must be enabled")
	}
	if spec.Review.RequireGreenCI {
		return fiber.NewError(fiber.StatusBadRequest, "spec.review.requireGreenCI is not supported until repository monitor CI state collection is available")
	}
	return nil
}

func validateRepositoryMonitorRunRequest(req CreateRepositoryMonitorRunRequest) error {
	switch strings.TrimSpace(req.TargetKind) {
	case "", repositoryMonitorTargetKindPullRequest, repositoryMonitorTargetKindIssue:
		return nil
	default:
		return fiber.NewError(fiber.StatusBadRequest, "targetKind must be pull_request or issue")
	}
}

func parseRepositoryMonitorGitHubURL(repoURL string) (string, string, error) {
	owner, repo, err := security.ParseGitHubRepositoryURL(repoURL)
	if err != nil {
		return "", "", repositoryMonitorRepoURLError(err)
	}
	return owner, repo, nil
}

func repositoryMonitorRepoURLError(err error) error {
	message := strings.TrimPrefix(err.Error(), "repository URL ")
	return fmt.Errorf("spec.repoURL %s", message)
}

func repositoryMonitorPullRequestsEnabled(spec corev1alpha1.RepositoryMonitorSpec) bool {
	return spec.Targets.PullRequests.Enabled == nil || *spec.Targets.PullRequests.Enabled
}

func (h *Handlers) fetchRepositoryMonitor(ctx fiber.Ctx, namespace, name string) (*corev1alpha1.RepositoryMonitor, error) {
	monitor := &corev1alpha1.RepositoryMonitor{}
	if err := h.client.Get(ctx.Context(), types.NamespacedName{Name: name, Namespace: namespace}, monitor); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "repository monitor not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get repository monitor: %v", err))
	}
	return monitor, nil
}

func effectiveRepositoryMonitorBranch(monitor *corev1alpha1.RepositoryMonitor) string {
	if monitor.Spec.Branch != "" {
		return monitor.Spec.Branch
	}
	return "main"
}

func repositoryMonitorAgentRefs(monitor *corev1alpha1.RepositoryMonitor) []corev1alpha1.AgentReference {
	var refs []corev1alpha1.AgentReference
	if monitor.Spec.Agents.Reviewer != nil && monitor.Spec.Agents.Reviewer.Name != "" {
		refs = append(refs, *monitor.Spec.Agents.Reviewer)
	}
	if monitor.Spec.Agents.Repairer != nil && monitor.Spec.Agents.Repairer.Name != "" {
		refs = append(refs, *monitor.Spec.Agents.Repairer)
	}
	if monitor.Spec.Agents.Implementer != nil && monitor.Spec.Agents.Implementer.Name != "" {
		refs = append(refs, *monitor.Spec.Agents.Implementer)
	}
	return refs
}

func contextTokenRepositoryMonitorFailures(token *ContextToken, monitor *corev1alpha1.RepositoryMonitor) []string {
	failures := []string{}
	if want, ok := contextString(token.TransactionContext, "namespace"); ok && monitor.Namespace != want {
		failures = append(failures, fmt.Sprintf("namespace %q does not match token context %q", monitor.Namespace, want))
	}
	if want, ok := contextString(token.TransactionContext, "repo"); ok && monitor.Spec.RepoURL != want {
		failures = append(failures, fmt.Sprintf("repository %q does not match token context %q", monitor.Spec.RepoURL, want))
	}
	if want, ok := contextString(token.TransactionContext, "branch"); ok && effectiveRepositoryMonitorBranch(monitor) != want {
		failures = append(failures, fmt.Sprintf("workspace branch %q does not match token context %q", effectiveRepositoryMonitorBranch(monitor), want))
	}

	refs := repositoryMonitorAgentRefs(monitor)
	allowed, hasAllowed := contextStringList(token.TransactionContext, "allowedAgents")
	if want, ok := contextString(token.TransactionContext, "agent"); ok {
		matched := false
		for _, ref := range refs {
			agentNamespace := ref.Namespace
			if agentNamespace == "" {
				agentNamespace = monitor.Namespace
			}
			if agentMatches(ref.Name, agentNamespace, want) {
				matched = true
				continue
			}
			if !hasAllowed {
				failures = append(failures, fmt.Sprintf("agent %q does not match token context %q", namespacedNameString(agentNamespace, ref.Name), want))
			}
		}
		if !matched {
			failures = append(failures, fmt.Sprintf("no repository monitor agent matches token context %q", want))
		}
	}
	if hasAllowed {
		for _, ref := range refs {
			agentNamespace := ref.Namespace
			if agentNamespace == "" {
				agentNamespace = monitor.Namespace
			}
			if !agentAllowed(ref.Name, agentNamespace, allowed) {
				failures = append(failures, fmt.Sprintf("agent %q is not allowed by token context", namespacedNameString(agentNamespace, ref.Name)))
			}
		}
	}
	return failures
}

func (h *Handlers) contextTokenRepositoryMonitorAllowed(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor) bool {
	if !h.contextTokenAuthorization.Enabled() {
		return true
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return true
	}
	failures := contextTokenRepositoryMonitorFailures(ui.ContextToken, monitor)
	if len(failures) == 0 {
		return true
	}
	if h.contextTokenAuthorization.enforcing() {
		return false
	}
	_ = h.handleContextTokenAuthorizationFailures(ui.ContextToken, "listRepositoryMonitors", failures)
	return true
}

func (h *Handlers) authorizeContextTokenRepositoryMonitor(c fiber.Ctx, action string, monitor *corev1alpha1.RepositoryMonitor) error {
	if !h.contextTokenAuthorization.Enabled() {
		return nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil
	}
	failures := contextTokenRepositoryMonitorFailures(ui.ContextToken, monitor)
	if len(failures) == 0 {
		metrics.RecordContextTokenAuthorization(action, "allowed", "ok")
		return nil
	}
	return h.handleContextTokenAuthorizationFailures(ui.ContextToken, action, failures)
}

// CreateRepositoryMonitor creates a new durable repository monitor.
func (h *Handlers) CreateRepositoryMonitor(c fiber.Ctx) error {
	var req CreateRepositoryMonitorRequest
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
	namespaceValue := req.Namespace
	if namespaceValue == "" {
		namespaceValue = req.Metadata.Namespace
	}
	namespace, err := h.resolveNamespace(c, namespaceValue)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createRepositoryMonitor", h.contextTokenAuthorization.MonitorWriteScopes); err != nil {
		return err
	}
	h.normalizeRepositoryMonitorSpec(&req.Spec)
	if err := validateRepositoryMonitorSpec(req.Spec); err != nil {
		return err
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		ObjectMeta: objectMetaFromRequest(name, namespace, req.Metadata),
		Spec:       req.Spec,
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "createRepositoryMonitor", monitor); err != nil {
		return err
	}
	if err := h.validateRepositoryMonitorReviewerAgent(c, namespace, req.Spec); err != nil {
		return err
	}
	if err := h.validateRepositoryMonitorGitSecret(c, namespace, req.Spec); err != nil {
		return err
	}
	if err := h.client.Create(c.Context(), monitor); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "repository monitor already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create repository monitor: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(monitor)
}

// ListRepositoryMonitors lists configured repository monitors.
func (h *Handlers) ListRepositoryMonitors(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRepositoryMonitors", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}

	pagination, err := ParsePagination(c.Query("limit", "100"), c.Query("continue", ""))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	opts := &client.ListOptions{Namespace: namespace, Limit: pagination.Limit, Continue: pagination.Continue}
	list := &corev1alpha1.RepositoryMonitorList{}
	if err := h.client.List(c.Context(), list, opts); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list repository monitors: %v", err))
	}

	items := list.Items
	filteredList := false
	if h.contextTokenAuthorization.Enabled() {
		filtered := make([]corev1alpha1.RepositoryMonitor, 0, len(items))
		for i := range items {
			if h.contextTokenRepositoryMonitorAllowed(c, &items[i]) {
				filtered = append(filtered, items[i])
			}
		}
		filteredList = len(filtered) != len(items)
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

// GetRepositoryMonitor returns a repository monitor configuration.
func (h *Handlers) GetRepositoryMonitor(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getRepositoryMonitor", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "getRepositoryMonitor", monitor); err != nil {
		return err
	}
	return c.JSON(monitor)
}

// UpdateRepositoryMonitor updates an existing repository monitor.
func (h *Handlers) UpdateRepositoryMonitor(c fiber.Ctx) error {
	var req UpdateRepositoryMonitorRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "updateRepositoryMonitor", h.contextTokenAuthorization.MonitorWriteScopes); err != nil {
		return err
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "updateRepositoryMonitor", monitor); err != nil {
		return err
	}

	h.normalizeRepositoryMonitorSpec(&req.Spec)
	if err := validateRepositoryMonitorSpec(req.Spec); err != nil {
		return err
	}
	updated := monitor.DeepCopy()
	updated.Spec = req.Spec
	if err := h.authorizeContextTokenRepositoryMonitor(c, "updateRepositoryMonitor", updated); err != nil {
		return err
	}
	if err := h.validateRepositoryMonitorReviewerAgent(c, namespace, req.Spec); err != nil {
		return err
	}
	if err := h.validateRepositoryMonitorGitSecret(c, namespace, req.Spec); err != nil {
		return err
	}
	if err := h.client.Update(c.Context(), updated); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update repository monitor: %v", err))
	}
	return c.JSON(updated)
}

// DeleteRepositoryMonitor deletes a repository monitor configuration.
func (h *Handlers) DeleteRepositoryMonitor(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "deleteRepositoryMonitor", h.contextTokenAuthorization.MonitorWriteScopes); err != nil {
		return err
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "deleteRepositoryMonitor", monitor); err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), monitor); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete repository monitor: %v", err))
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// CreateRepositoryMonitorRun records a manual monitor run request.
func (h *Handlers) CreateRepositoryMonitorRun(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	var req CreateRepositoryMonitorRunRequest
	if len(c.Body()) > 0 {
		if err := c.Bind().JSON(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createRepositoryMonitorRun", h.contextTokenAuthorization.MonitorOperateScopes); err != nil {
		return err
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "createRepositoryMonitorRun", monitor); err != nil {
		return err
	}
	if err := validateRepositoryMonitorRunRequest(req); err != nil {
		return err
	}
	if err := h.ensureNoActiveRepositoryMonitorRun(c, namespace, monitor.Name); err != nil {
		return err
	}

	run := &store.MonitorRun{
		ID:               fmt.Sprintf("monrun-%d", time.Now().UTC().UnixNano()),
		MonitorNamespace: namespace,
		MonitorName:      monitor.Name,
		Trigger:          "manual",
		TargetKind:       req.TargetKind,
		TargetNumber:     req.TargetNumber,
		TargetSHA:        req.TargetSHA,
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now(),
	}
	if err := h.repositoryMonitorStore.CreateMonitorRun(c.Context(), run); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fiber.NewError(fiber.StatusConflict, "repository monitor already has an active run")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create monitor run: %v", err))
	}
	if err := h.annotateRepositoryMonitorRunRequest(c, monitor, run); err != nil {
		if failErr := h.markRepositoryMonitorRunSignalFailed(c, run, err); failErr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("%v; additionally failed to mark monitor run failed: %v", err, failErr))
		}
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(run)
}

func (h *Handlers) markRepositoryMonitorRunSignalFailed(c fiber.Ctx, run *store.MonitorRun, signalErr error) error {
	completedAt := time.Now()
	run.Phase = "failed"
	run.CompletedAt = &completedAt
	run.Error = signalErr.Error()
	return h.repositoryMonitorStore.UpdateMonitorRun(c.Context(), run)
}

func (h *Handlers) annotateRepositoryMonitorRunRequest(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun) error {
	updated := monitor.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[repositoryMonitorRunRequestAnnotation] = run.ID
	if err := h.client.Patch(c.Context(), updated, client.MergeFrom(monitor)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to signal repository monitor run: %v", err))
	}
	return nil
}

func (h *Handlers) ensureNoActiveRepositoryMonitorRun(c fiber.Ctx, namespace, monitorName string) error {
	for _, phase := range []string{repositoryMonitorRunPhaseQueued, repositoryMonitorRunPhaseRunning} {
		runs, _, err := h.repositoryMonitorStore.ListMonitorRuns(c.Context(), store.MonitorRunFilter{
			Namespace:   namespace,
			MonitorName: monitorName,
			Phase:       phase,
			Limit:       1,
		})
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list active monitor runs: %v", err))
		}
		if len(runs) > 0 {
			return fiber.NewError(fiber.StatusConflict, "repository monitor already has an active run")
		}
	}
	return nil
}

// ListRepositoryMonitorRuns lists durable runs for a repository monitor.
func (h *Handlers) ListRepositoryMonitorRuns(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRepositoryMonitorRuns", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "listRepositoryMonitorRuns", monitor); err != nil {
		return err
	}
	limit, err := strconv.Atoi(c.Query("limit", "20"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	runs, next, err := h.repositoryMonitorStore.ListMonitorRuns(c.Context(), store.MonitorRunFilter{
		Namespace:   namespace,
		MonitorName: monitor.Name,
		Trigger:     c.Query("trigger"),
		TargetKind:  c.Query("targetKind"),
		Limit:       limit,
		Cursor:      monitorListCursor(c),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list monitor runs: %v", err))
	}
	return c.JSON(fiber.Map{"items": runs, "metadata": fiber.Map{"continue": next}})
}

// ListRepositoryMonitorItems lists current items for a repository monitor.
func (h *Handlers) ListRepositoryMonitorItems(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRepositoryMonitorItems", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "listRepositoryMonitorItems", monitor); err != nil {
		return err
	}
	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	items, next, err := h.repositoryMonitorStore.ListMonitorItems(c.Context(), store.MonitorItemFilter{
		Namespace:      namespace,
		MonitorName:    monitor.Name,
		Kind:           c.Query("kind"),
		State:          c.Query("state"),
		ReviewVerdict:  c.Query("verdict"),
		RepairState:    c.Query("repairState"),
		AutomergeState: c.Query("automergeState"),
		Limit:          limit,
		Cursor:         monitorListCursor(c),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list monitor items: %v", err))
	}
	return c.JSON(fiber.Map{"items": items, "metadata": fiber.Map{"continue": next}})
}

// ListRepositoryMonitorEvents lists audit events for a repository monitor.
func (h *Handlers) ListRepositoryMonitorEvents(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRepositoryMonitorEvents", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	monitorName := c.Query("name")
	if monitorName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name query parameter is required")
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, monitorName)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "listRepositoryMonitorEvents", monitor); err != nil {
		return err
	}
	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	itemNumber, err := parseOptionalInt64Query(c.Query("itemNumber"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid itemNumber")
	}
	events, next, err := h.repositoryMonitorStore.ListMonitorEvents(c.Context(), store.MonitorEventFilter{
		Namespace:   namespace,
		MonitorName: monitor.Name,
		RunID:       c.Query("runID"),
		ItemKind:    c.Query("itemKind"),
		ItemNumber:  itemNumber,
		EventType:   c.Query("eventType"),
		Limit:       limit,
		Cursor:      monitorListCursor(c),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list monitor events: %v", err))
	}
	return c.JSON(fiber.Map{"items": events, "metadata": fiber.Map{"continue": next}})
}

func monitorListCursor(c fiber.Ctx) string {
	if cursor := c.Query("continue"); cursor != "" {
		return cursor
	}
	return c.Query("cursor")
}

func parseOptionalInt64Query(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	return strconv.ParseInt(value, 10, 64)
}

// CreateRepositoryMonitorCommandEvent records an API-created monitor command and best-effort queues its run.
func (h *Handlers) CreateRepositoryMonitorCommandEvent(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	var req CreateRepositoryMonitorCommandRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createRepositoryMonitorCommandEvent", h.contextTokenAuthorization.MonitorOperateScopes); err != nil {
		return err
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, c.Params("name"))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "createRepositoryMonitorCommandEvent", monitor); err != nil {
		return err
	}
	req.Intent = strings.TrimSpace(req.Intent)
	if err := validateRepositoryMonitorCommandRequest(req); err != nil {
		return err
	}
	if err := validateRepositoryMonitorCommandTargetEnabled(monitor.Spec, req.Kind); err != nil {
		return err
	}
	item, _ := h.repositoryMonitorStore.GetMonitorItem(c.Context(), namespace, monitor.Name, req.Kind, strconv.FormatInt(req.Number, 10))
	if req.TargetSHA == "" && item != nil && req.Kind == repositoryMonitorTargetKindPullRequest {
		req.TargetSHA = item.HeadSHA
	}
	snapshot := ""
	if item != nil && req.Kind == repositoryMonitorTargetKindIssue {
		snapshot = item.SnapshotDigest
	}
	now := time.Now()
	id := fmt.Sprintf("cmd-api-%d", now.UTC().UnixNano())
	event := &store.CommandEvent{
		ID:                  id,
		MonitorNamespace:    namespace,
		MonitorName:         monitor.Name,
		Repo:                monitor.Spec.Owner + "/" + monitor.Spec.Repository,
		Kind:                req.Kind,
		Number:              req.Number,
		Source:              "api",
		MonitorGeneration:   monitor.Generation,
		DedupeKey:           id,
		IdempotencyKey:      id,
		Author:              "orka-api",
		Permission:          "orka:monitors:operate",
		Command:             req.Intent,
		Intent:              req.Intent,
		HeadSHA:             req.TargetSHA,
		IssueSnapshotDigest: snapshot,
		Status:              "accepted",
		CreatedAt:           now,
		ProcessedAt:         &now,
	}
	if err := h.repositoryMonitorStore.CreateCommandEvent(c.Context(), event); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create monitor command: %v", err))
	}
	run := &store.MonitorRun{ID: "monrun-" + githubReplayKeySuffix(githubWebhookReplayKey([]byte(event.ID+"|run"))), MonitorNamespace: namespace, MonitorName: monitor.Name, Trigger: githubMonitorTriggerLabelCommand, TargetKind: req.Kind, TargetNumber: req.Number, TargetSHA: req.TargetSHA, CommandEventID: event.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: now}
	if err := h.repositoryMonitorStore.CreateMonitorRun(c.Context(), run); err == nil {
		if err := h.annotateRepositoryMonitorRunRequest(c, monitor, run); err != nil {
			if failErr := h.markRepositoryMonitorRunSignalFailed(c, run, err); failErr != nil {
				return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("%v; additionally failed to mark monitor run failed: %v", err, failErr))
			}
			return err
		}
	} else if !errors.Is(err, store.ErrConflict) {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to queue monitor command run: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(event)
}

func validateRepositoryMonitorCommandTargetEnabled(spec corev1alpha1.RepositoryMonitorSpec, kind string) error {
	switch kind {
	case repositoryMonitorTargetKindIssue:
		if spec.Targets.Issues.Enabled {
			return nil
		}
	case repositoryMonitorTargetKindPullRequest:
		if repositoryMonitorPullRequestsEnabled(spec) {
			return nil
		}
	}
	return fiber.NewError(fiber.StatusBadRequest, "command target kind is not enabled for this monitor")
}

func validateRepositoryMonitorCommandRequest(req CreateRepositoryMonitorCommandRequest) error {
	if req.Kind != repositoryMonitorTargetKindIssue && req.Kind != repositoryMonitorTargetKindPullRequest {
		return fiber.NewError(fiber.StatusBadRequest, "kind must be issue or pull_request")
	}
	if req.Number <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "number is required")
	}
	intent := strings.TrimSpace(req.Intent)
	switch req.Kind {
	case repositoryMonitorTargetKindIssue:
		switch intent {
		case "triage", "research", "plan", "approve_plan", "implement", "decompose", finishReasonStop, "resume":
			return nil
		}
	case repositoryMonitorTargetKindPullRequest:
		switch intent {
		case "review", "fix", "fix_ci", "update_branch", "automerge", finishReasonStop, "resume":
			return nil
		}
	}
	return fiber.NewError(fiber.StatusBadRequest, "unsupported command intent for target kind")
}

// ListRepositoryMonitorCommandEvents lists durable label/API command events.
func (h *Handlers) ListRepositoryMonitorCommandEvents(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRepositoryMonitorCommandEvents", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	monitorName := c.Query("name")
	if monitorName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name query parameter is required")
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, monitorName)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "listRepositoryMonitorCommandEvents", monitor); err != nil {
		return err
	}
	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	number, err := parseOptionalInt64Query(c.Query("number"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid number")
	}
	events, next, err := h.repositoryMonitorStore.ListCommandEvents(c.Context(), store.CommandEventFilter{
		Namespace:   namespace,
		MonitorName: monitor.Name,
		Kind:        c.Query("kind"),
		Number:      number,
		Intent:      c.Query("intent"),
		Status:      c.Query("status"),
		Limit:       limit,
		Cursor:      monitorListCursor(c),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list monitor commands: %v", err))
	}
	return c.JSON(fiber.Map{"items": events, "metadata": fiber.Map{"continue": next}})
}

// GetRepositoryMonitorCommandEvent fetches one durable command event.
func (h *Handlers) GetRepositoryMonitorCommandEvent(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getRepositoryMonitorCommandEvent", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	event, err := h.repositoryMonitorStore.GetCommandEvent(c.Context(), namespace, c.Params("id"))
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "monitor command not found")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get monitor command: %v", err))
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, event.MonitorName)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "getRepositoryMonitorCommandEvent", monitor); err != nil {
		return err
	}
	return c.JSON(event)
}

// ListRepositoryMonitorActionRecords lists durable monitor action records.
func (h *Handlers) ListRepositoryMonitorActionRecords(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listRepositoryMonitorActionRecords", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	monitorName := c.Query("name")
	if monitorName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name query parameter is required")
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, monitorName)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "listRepositoryMonitorActionRecords", monitor); err != nil {
		return err
	}
	limit, err := strconv.Atoi(c.Query("limit", "50"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	number, err := parseOptionalInt64Query(c.Query("number"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid number")
	}
	records, next, err := h.repositoryMonitorStore.ListActionRecords(c.Context(), store.ActionRecordFilter{
		Namespace:   namespace,
		MonitorName: monitor.Name,
		Kind:        c.Query("kind"),
		Number:      number,
		ActionKind:  c.Query("actionKind"),
		TaskName:    c.Query("taskName"),
		Limit:       limit,
		Cursor:      monitorListCursor(c),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list monitor actions: %v", err))
	}
	return c.JSON(fiber.Map{"items": records, "metadata": fiber.Map{"continue": next}})
}

// GetRepositoryMonitorActionRecord fetches one durable action record.
func (h *Handlers) GetRepositoryMonitorActionRecord(c fiber.Ctx) error {
	if err := h.ensureRepositoryMonitorStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getRepositoryMonitorActionRecord", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		return err
	}
	record, err := h.repositoryMonitorStore.GetActionRecord(c.Context(), namespace, c.Params("id"))
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "monitor action not found")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get monitor action: %v", err))
	}
	monitor, err := h.fetchRepositoryMonitor(c, namespace, record.MonitorName)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenRepositoryMonitor(c, "getRepositoryMonitorActionRecord", monitor); err != nil {
		return err
	}
	return c.JSON(record)
}
