/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

const (
	githubWebhookSecretEnv                = "ORKA_GITHUB_WEBHOOK_SECRET"
	githubLabelTriggerAgentEnv            = "ORKA_GITHUB_LABEL_TRIGGER_AGENT"
	githubLabelTriggerNamespaceEnv        = "ORKA_GITHUB_LABEL_TRIGGER_NAMESPACE"
	githubLabelTriggerGitSecretEnv        = "ORKA_GITHUB_LABEL_TRIGGER_GIT_SECRET"
	githubLabelTriggerPrefixEnv           = "ORKA_GITHUB_LABEL_TRIGGER_PREFIX"
	githubLabelTriggerTimeoutEnv          = "ORKA_GITHUB_LABEL_TRIGGER_TIMEOUT"
	githubLabelTriggerMaxTurnsEnv         = "ORKA_GITHUB_LABEL_TRIGGER_MAX_TURNS"
	githubLabelTriggerDefaultPrefix       = "agent:"
	githubDeliveryHeader                  = "X-GitHub-Delivery"
	githubEventHeader                     = "X-GitHub-Event"
	githubSignature256Header              = "X-Hub-Signature-256"
	githubSignature256Prefix              = "sha256="
	githubEventIssues                     = "issues"
	githubEventPullRequest                = "pull_request"
	githubEventPing                       = "ping"
	githubWebhookCreatedBy                = "github-label"
	githubActionImplement                 = "implement"
	githubActionReview                    = "review"
	githubActionToIssues                  = "to-issues"
	githubActionUpdateBranch              = "update-branch"
	githubWebhookDefaultTimeout           = 30 * time.Minute
	githubWebhookDefaultMaxTurns    int32 = 100
)

var nonDNSNameCharRE = regexp.MustCompile(`[^a-z0-9-]+`)

type githubLabelWebhookPayload struct {
	Action      string                    `json:"action"`
	Label       githubWebhookLabel        `json:"label"`
	Repository  githubWebhookRepository   `json:"repository"`
	Issue       *githubWebhookIssue       `json:"issue,omitempty"`
	PullRequest *githubWebhookPullRequest `json:"pull_request,omitempty"`
	Sender      githubWebhookUser         `json:"sender"`
}

type githubWebhookLabel struct {
	Name string `json:"name"`
}

type githubWebhookUser struct {
	Login string `json:"login"`
}

type githubWebhookRepository struct {
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

type githubWebhookIssue struct {
	Number      int                       `json:"number"`
	Title       string                    `json:"title"`
	Body        string                    `json:"body"`
	HTMLURL     string                    `json:"html_url"`
	PullRequest *githubIssuePullRequestID `json:"pull_request,omitempty"`
}

type githubIssuePullRequestID struct {
	HTMLURL string `json:"html_url"`
}

type githubWebhookPullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	Base    struct {
		Ref  string                  `json:"ref"`
		SHA  string                  `json:"sha"`
		Repo githubWebhookRepository `json:"repo"`
	} `json:"base"`
	Head struct {
		Ref  string                  `json:"ref"`
		SHA  string                  `json:"sha"`
		Repo githubWebhookRepository `json:"repo"`
	} `json:"head"`
}

type githubLabelTarget struct {
	Kind         string
	Number       int
	Title        string
	Body         string
	HTMLURL      string
	IsPR         bool
	IncompletePR bool
	Draft        bool
	BaseBranch   string
	BaseSHA      string
	HeadBranch   string
	HeadSHA      string
	Repo         githubWebhookRepository
	BaseRepo     githubWebhookRepository
	HeadRepo     githubWebhookRepository
}

func (h *Handlers) HandleGitHubWebhook(c fiber.Ctx) error {
	body := append([]byte(nil), c.Body()...)
	secret := strings.TrimSpace(os.Getenv(githubWebhookSecretEnv))
	if secret == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "GitHub webhook secret is not configured")
	}
	if !validGitHubSignature(body, c.Get(githubSignature256Header), secret) {
		return fiber.NewError(fiber.StatusUnauthorized, "invalid GitHub webhook signature")
	}

	event := c.Get(githubEventHeader)
	if event == githubEventPing {
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"status":  "ok",
			"message": "GitHub webhook signature verified",
		})
	}
	if event != githubEventIssues && event != githubEventPullRequest {
		return githubWebhookIgnored(c, fmt.Sprintf("unsupported GitHub event %q", event))
	}

	var payload githubLabelWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid GitHub webhook payload")
	}
	if payload.Action != "labeled" {
		return githubWebhookIgnored(c, fmt.Sprintf("ignored action %q", payload.Action))
	}

	action, ok := githubLabelAction(payload.Label.Name)
	if !ok {
		return githubWebhookIgnored(c, "label is not an Orka agent trigger")
	}

	target, ok := payload.target()
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "GitHub webhook payload has no issue or pull request target")
	}
	if target.IncompletePR {
		return githubWebhookIgnored(c, "issues webhook payload for pull request lacks base/head details; configure pull_request events for PR labels")
	}
	if actionRequiresPullRequest(action) && !target.IsPR {
		return githubWebhookIgnored(c, fmt.Sprintf("agent:%s requires a pull request target", action))
	}

	namespace := h.githubWebhookNamespace()
	agentName := githubAgentForAction(action)
	if agentName == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "GitHub label trigger agent is not configured")
	}
	if err := h.ensureAgentExists(c, namespace, agentName); err != nil {
		return err
	}

	delivery := c.Get(githubDeliveryHeader)
	if delivery == "" {
		delivery = hex.EncodeToString(githubHash(body))[:12]
	}

	task := buildGitHubLabelTask(namespace, agentName, action, delivery, event, payload, target)
	if err := h.client.Create(c.Context(), task); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
				"status":    "duplicate",
				"message":   "task already exists for this GitHub delivery",
				"taskName":  task.Name,
				"namespace": task.Namespace,
				"action":    action,
			})
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create task: %v", err))
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"status":    "created",
		"taskName":  task.Name,
		"namespace": task.Namespace,
		"action":    action,
		"label":     payload.Label.Name,
	})
}

func validGitHubSignature(body []byte, signatureHeader, secret string) bool {
	if !strings.HasPrefix(signatureHeader, githubSignature256Prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signatureHeader, githubSignature256Prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(got, want)
}

func githubHash(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

func githubWebhookIgnored(c fiber.Ctx, reason string) error {
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"status": "ignored",
		"reason": reason,
	})
}

func githubLabelAction(labelName string) (string, bool) {
	prefix := strings.TrimSpace(os.Getenv(githubLabelTriggerPrefixEnv))
	if prefix == "" {
		prefix = githubLabelTriggerDefaultPrefix
	}

	labelName = strings.TrimSpace(labelName)
	if !strings.HasPrefix(strings.ToLower(labelName), strings.ToLower(prefix)) {
		return "", false
	}
	action := strings.TrimSpace(labelName[len(prefix):])
	action = strings.ToLower(action)
	if action == "" {
		return "", false
	}
	return action, true
}

func actionRequiresPullRequest(action string) bool {
	switch action {
	case githubActionReview, githubActionUpdateBranch:
		return true
	default:
		return false
	}
}

func (p githubLabelWebhookPayload) target() (githubLabelTarget, bool) {
	if p.PullRequest != nil {
		pr := p.PullRequest
		repo := p.Repository
		headRepo := pr.Head.Repo
		if headRepo.CloneURL == "" && headRepo.HTMLURL == "" {
			headRepo = repo
		}
		baseRepo := pr.Base.Repo
		if baseRepo.CloneURL == "" && baseRepo.HTMLURL == "" {
			baseRepo = repo
		}
		return githubLabelTarget{
			Kind:       "pull_request",
			Number:     pr.Number,
			Title:      pr.Title,
			Body:       pr.Body,
			HTMLURL:    pr.HTMLURL,
			IsPR:       true,
			Draft:      pr.Draft,
			BaseBranch: pr.Base.Ref,
			BaseSHA:    pr.Base.SHA,
			HeadBranch: pr.Head.Ref,
			HeadSHA:    pr.Head.SHA,
			Repo:       repo,
			BaseRepo:   baseRepo,
			HeadRepo:   headRepo,
		}, true
	}

	if p.Issue != nil {
		htmlURL := p.Issue.HTMLURL
		if p.Issue.PullRequest != nil {
			if p.Issue.PullRequest.HTMLURL != "" {
				htmlURL = p.Issue.PullRequest.HTMLURL
			}
			return githubLabelTarget{
				Kind:         "pull_request",
				Number:       p.Issue.Number,
				Title:        p.Issue.Title,
				Body:         p.Issue.Body,
				HTMLURL:      htmlURL,
				IsPR:         true,
				IncompletePR: true,
				Repo:         p.Repository,
				BaseRepo:     p.Repository,
				HeadRepo:     p.Repository,
			}, true
		}
		return githubLabelTarget{
			Kind:     "issue",
			Number:   p.Issue.Number,
			Title:    p.Issue.Title,
			Body:     p.Issue.Body,
			HTMLURL:  htmlURL,
			Repo:     p.Repository,
			BaseRepo: p.Repository,
			HeadRepo: p.Repository,
		}, true
	}

	return githubLabelTarget{}, false
}

func (h *Handlers) githubWebhookNamespace() string {
	if ns := strings.TrimSpace(os.Getenv(githubLabelTriggerNamespaceEnv)); ns != "" {
		return ns
	}
	if h.watchNamespace != "" {
		return h.watchNamespace
	}
	return "default"
}

func githubAgentForAction(action string) string {
	if actionAgent := strings.TrimSpace(os.Getenv(githubActionAgentEnv(action))); actionAgent != "" {
		return actionAgent
	}
	return strings.TrimSpace(os.Getenv(githubLabelTriggerAgentEnv))
}

func githubActionAgentEnv(action string) string {
	action = strings.ToUpper(action)
	var b strings.Builder
	for _, r := range action {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "ORKA_GITHUB_LABEL_AGENT_" + strings.Trim(b.String(), "_")
}

func (h *Handlers) ensureAgentExists(c fiber.Ctx, namespace, agentName string) error {
	var agent corev1alpha1.Agent
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: agentName, Namespace: namespace}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("agent %q not found in namespace %q", agentName, namespace))
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get agent: %v", err))
	}
	if agent.Spec.Runtime == nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("agent %q must have runtime configured", agentName))
	}
	return nil
}

func buildGitHubLabelTask(namespace, agentName, action, delivery, event string, payload githubLabelWebhookPayload, target githubLabelTarget) *corev1alpha1.Task {
	workspace := githubWorkspace(action, target)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      githubTaskName(action, target.Number, delivery),
			Namespace: namespace,
			Labels: map[string]string{
				labels.LabelCreatedBy:        githubWebhookCreatedBy,
				labels.LabelGitHubEvent:      labels.SelectorValue(event),
				labels.LabelGitHubAction:     labels.SelectorValue(action),
				labels.LabelGitHubRepository: labels.SelectorValue(payload.Repository.FullName),
				labels.LabelGitHubTarget:     labels.SelectorValue(target.Kind),
			},
			Annotations: map[string]string{
				labels.AnnotationGitHubDelivery:   delivery,
				labels.AnnotationGitHubLabel:      payload.Label.Name,
				labels.AnnotationGitHubAction:     action,
				labels.AnnotationGitHubRepository: payload.Repository.FullName,
				labels.AnnotationGitHubURL:        target.HTMLURL,
				labels.AnnotationGitHubSender:     payload.Sender.Login,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: buildGitHubActionPrompt(action, payload, target, workspace),
			AgentRef: &corev1alpha1.AgentReference{
				Name: agentName,
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				MaxTurns:  githubMaxTurns(),
				Workspace: workspace,
			},
			Timeout: githubTimeout(),
			Env: []corev1.EnvVar{
				{Name: "ORKA_GITHUB_EVENT", Value: event},
				{Name: "ORKA_GITHUB_DELIVERY", Value: delivery},
				{Name: "ORKA_GITHUB_LABEL", Value: payload.Label.Name},
				{Name: "ORKA_GITHUB_ACTION", Value: action},
				{Name: "ORKA_GITHUB_REPOSITORY", Value: payload.Repository.FullName},
				{Name: "ORKA_GITHUB_TARGET_URL", Value: target.HTMLURL},
			},
		},
	}
	if target.Number > 0 {
		task.Labels[labels.LabelGitHubNumber] = labels.SelectorValue(strconv.Itoa(target.Number))
		task.Annotations[labels.AnnotationGitHubNumber] = strconv.Itoa(target.Number)
	}
	return task
}

func githubTaskName(action string, number int, delivery string) string {
	action = dnsNamePart(action)
	if action == "" {
		action = "action"
	}
	deliveryHash := hex.EncodeToString(githubHash([]byte(delivery)))[:12]
	base := fmt.Sprintf("github-%s-%d", action, number)
	maxBaseLen := 63 - len(deliveryHash) - 1
	if len(base) > maxBaseLen {
		base = strings.Trim(base[:maxBaseLen], "-")
	}
	if base == "" {
		base = "github"
	}
	return base + "-" + deliveryHash
}

func dnsNamePart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, ":", "-")
	value = nonDNSNameCharRE.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if len(value) > 40 {
		value = strings.Trim(value[:40], "-")
	}
	return value
}

func githubWorkspace(action string, target githubLabelTarget) *corev1alpha1.WorkspaceConfig {
	repo := target.Repo
	if target.IsPR {
		repo = target.HeadRepo
	}
	ws := &corev1alpha1.WorkspaceConfig{
		GitRepo: repoURL(repo),
	}

	switch {
	case target.IsPR && target.HeadBranch != "":
		ws.Branch = target.HeadBranch
		if target.HeadSHA != "" {
			ws.Ref = target.HeadSHA
		}
	case target.Repo.DefaultBranch != "":
		ws.Branch = target.Repo.DefaultBranch
	}

	if action == githubActionImplement {
		ws.PushBranch = fmt.Sprintf("orka/implement-%s-%d", target.Kind, target.Number)
		if target.IsPR && target.HeadBranch != "" {
			ws.PushBranch = target.HeadBranch
		}
	}
	if action == githubActionUpdateBranch && target.HeadBranch != "" {
		ws.PushBranch = target.HeadBranch
	}
	if action != githubActionReview && action != githubActionToIssues && action != githubActionImplement && action != githubActionUpdateBranch {
		ws.PushBranch = fmt.Sprintf("orka/%s-%s-%d", dnsNamePart(action), target.Kind, target.Number)
	}

	if gitSecret := strings.TrimSpace(os.Getenv(githubLabelTriggerGitSecretEnv)); gitSecret != "" {
		ws.GitSecretRef = &corev1.LocalObjectReference{Name: gitSecret}
	}
	if target.IsPR && target.BaseBranch != "" {
		ws.PRBaseBranch = target.BaseBranch
	}
	return ws
}

func repoURL(repo githubWebhookRepository) string {
	if repo.CloneURL != "" {
		return repo.CloneURL
	}
	if repo.HTMLURL != "" {
		return strings.TrimSuffix(repo.HTMLURL, ".git") + ".git"
	}
	return ""
}

func githubMaxTurns() *int32 {
	maxTurns := githubWebhookDefaultMaxTurns
	if value := strings.TrimSpace(os.Getenv(githubLabelTriggerMaxTurnsEnv)); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 32); err == nil && parsed > 0 {
			maxTurns = int32(parsed)
		}
	}
	return &maxTurns
}

func githubTimeout() *metav1.Duration {
	if value := strings.TrimSpace(os.Getenv(githubLabelTriggerTimeoutEnv)); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			return &metav1.Duration{Duration: d}
		}
	}
	return &metav1.Duration{Duration: githubWebhookDefaultTimeout}
}

func buildGitHubActionPrompt(action string, payload githubLabelWebhookPayload, target githubLabelTarget, workspace *corev1alpha1.WorkspaceConfig) string {
	var b strings.Builder
	b.WriteString("You are an Orka agent task triggered by a GitHub label.\n\n")
	b.WriteString("Trigger details:\n")
	b.WriteString(fmt.Sprintf("- Label: %s\n", payload.Label.Name))
	b.WriteString(fmt.Sprintf("- Action: %s\n", action))
	b.WriteString(fmt.Sprintf("- Repository: %s\n", payload.Repository.FullName))
	b.WriteString(fmt.Sprintf("- Target: %s #%d\n", target.Kind, target.Number))
	b.WriteString(fmt.Sprintf("- URL: %s\n", target.HTMLURL))
	if payload.Sender.Login != "" {
		b.WriteString(fmt.Sprintf("- Triggered by: %s\n", payload.Sender.Login))
	}
	if target.IsPR {
		b.WriteString(fmt.Sprintf("- Base branch: %s\n", target.BaseBranch))
		b.WriteString(fmt.Sprintf("- Head branch: %s\n", target.HeadBranch))
		if target.HeadSHA != "" {
			b.WriteString(fmt.Sprintf("- Head SHA: %s\n", target.HeadSHA))
		}
	}
	if workspace != nil {
		b.WriteString(fmt.Sprintf("- Workspace repo: %s\n", workspace.GitRepo))
		if workspace.Branch != "" {
			b.WriteString(fmt.Sprintf("- Workspace branch: %s\n", workspace.Branch))
		}
		if workspace.PushBranch != "" {
			b.WriteString(fmt.Sprintf("- Push branch: %s\n", workspace.PushBranch))
			b.WriteString("- Push handling: do not commit or push yourself; leave final workspace changes uncommitted so Orka can commit and push them.\n")
		}
	}

	b.WriteString("\nTitle:\n")
	b.WriteString(target.Title)
	b.WriteString("\n\nBody:\n")
	body := strings.TrimSpace(target.Body)
	if body == "" {
		body = "(empty)"
	}
	b.WriteString(body)
	b.WriteString("\n\nInstructions:\n")

	switch action {
	case githubActionImplement:
		b.WriteString("Implement the requested change. Keep the scope limited to the GitHub issue or PR request. Run relevant tests. Leave final changes uncommitted for Orka to commit and push. Summarize changes and test results.\n")
	case githubActionUpdateBranch:
		b.WriteString("Update the pull request branch with the latest base branch changes using a no-commit merge/rebase workflow. Resolve conflicts if needed, run relevant tests, and leave final changes uncommitted for Orka to commit and push. Do not merge the pull request.\n")
	case githubActionReview:
		b.WriteString("Review the pull request for correctness, tests, security, maintainability, and regressions. Do not change code. Produce a concise review with blocking findings first and include file/line references when available.\n")
	case githubActionToIssues:
		b.WriteString("Break the request into small, independently implementable GitHub issues. Prefer tracer-bullet vertical slices with acceptance criteria. If you can create issues with available GitHub credentials, do so; otherwise return issue drafts with titles, bodies, and labels.\n")
	default:
		b.WriteString(fmt.Sprintf("Perform the requested %q action for this GitHub target. Keep changes scoped, run relevant verification, and summarize the outcome.\n", action))
	}

	b.WriteString("\nSafety constraints:\n")
	b.WriteString("- Do not print or commit secrets or credentials.\n")
	b.WriteString("- Do not merge pull requests unless the prompt explicitly says to merge.\n")
	b.WriteString("- If required credentials or permissions are missing, explain exactly what is missing.\n")
	return b.String()
}
