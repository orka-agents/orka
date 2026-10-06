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
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	apiFieldStatus   = "status"
	apiFieldTaskName = "taskName"
	apiFieldAction   = "action"
	apiFieldLabel    = "label"
)

const (
	githubWebhookActionLabeled   = "labeled"
	githubWebhookActionUnlabeled = "unlabeled"
)

const (
	githubWebhookSecretEnv                     = "ORKA_GITHUB_WEBHOOK_SECRET"
	githubDeliveryHeader                       = "X-GitHub-Delivery"
	githubEventHeader                          = "X-GitHub-Event"
	githubSignature256Header                   = "X-Hub-Signature-256"
	githubSignature256Prefix                   = "sha256="
	githubEventIssues                          = "issues"
	githubEventPullRequest                     = "pull_request"
	githubEventPing                            = "ping"
	githubWebhookCreatedBy                     = "github-label"
	githubActionImplement                      = "implement"
	githubActionReview                         = "review"
	githubActionToIssues                       = "to-issues"
	githubActionUpdateBranch                   = "update-branch"
	githubMonitorTriggerPullRequestEvent       = "pull_request_event"
	githubMonitorTriggerLabelCommand           = "github_label_command"
	githubCommandEventSourceLabel              = "github_label"
	githubCommandStatusAccepted                = "accepted"
	githubCommandStatusRejected                = "rejected"
	githubCommandStatusBlocked                 = "blocked"
	githubCommandStatusCompleted               = "completed"
	githubCommandStatusProcessed               = "processed"
	githubMonitorEventTypeExactRunQueued       = "exact_event_run_queued"
	githubWebhookDefaultTimeout                = 30 * time.Minute
	githubWebhookDefaultMaxTurns         int32 = 100
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
	State       string                    `json:"state"`
	UpdatedAt   time.Time                 `json:"updated_at"`
	PullRequest *githubIssuePullRequestID `json:"pull_request,omitempty"`
	Labels      []githubWebhookLabel      `json:"labels"`
}

type githubIssuePullRequestID struct {
	HTMLURL string `json:"html_url"`
}

type githubWebhookPullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
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
	Labels []githubWebhookLabel `json:"labels"`
}

type githubLabelTarget struct {
	Kind         string
	Number       int
	Title        string
	Body         string
	HTMLURL      string
	State        string
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
	Labels       []string
	UpdatedAt    time.Time
}

type githubRepositoryMonitorEventResult struct {
	Matched       int
	Queued        int
	Duplicate     int
	SkippedActive int
	RunIDs        []string
	CommandIDs    []string
}

//nolint:gocyclo // GitHub webhook routing is intentionally linear across event families.
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
			apiFieldStatus:  "ok",
			apiFieldMessage: "GitHub webhook signature verified",
		})
	}
	if event != githubEventIssues && event != githubEventPullRequest {
		return githubWebhookIgnored(c, fmt.Sprintf("unsupported GitHub event %q", event))
	}

	var payload githubLabelWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid GitHub webhook payload")
	}

	pauseResult, err := h.handleRepositoryMonitorPauseEvent(c, body, payload)
	if err != nil {
		return err
	}
	monitorResult := pauseResult
	if payload.Action != githubWebhookActionLabeled {
		if event == githubEventPullRequest {
			var err error
			delivery := strings.TrimSpace(c.Get(githubDeliveryHeader))
			prResult, err := h.enqueueRepositoryMonitorPullRequestEventRuns(c, body, delivery, payload)
			if err != nil {
				return err
			}
			monitorResult = mergeGitHubRepositoryMonitorEventResults(monitorResult, prResult)
		}
		if monitorResult.Matched > 0 {
			return githubRepositoryMonitorEventResponse(c, monitorResult)
		}
		return githubWebhookIgnored(c, fmt.Sprintf("ignored action %q", payload.Action))
	}

	target, ok := payload.target()
	if !ok {
		return fiber.NewError(fiber.StatusBadRequest, "GitHub webhook payload has no issue or pull request target")
	}
	if target.IncompletePR {
		return githubWebhookIgnored(c, "issues webhook payload for pull request lacks base/head details; configure pull_request events for PR labels")
	}

	commandResult, handledCommand, err := h.handleRepositoryMonitorLabelCommand(c, body, strings.TrimSpace(c.Get(githubDeliveryHeader)), payload, target)
	if err != nil {
		return err
	}
	if handledCommand {
		if event == githubEventPullRequest {
			var err error
			delivery := strings.TrimSpace(c.Get(githubDeliveryHeader))
			prResult, err := h.enqueueRepositoryMonitorPullRequestEventRuns(c, body, delivery, payload)
			if err != nil {
				return err
			}
			monitorResult = mergeGitHubRepositoryMonitorEventResults(monitorResult, prResult)
		}
		return githubRepositoryMonitorEventResponse(c, mergeGitHubRepositoryMonitorEventResults(monitorResult, commandResult))
	}
	if event == githubEventPullRequest && !handledCommand {
		var err error
		delivery := strings.TrimSpace(c.Get(githubDeliveryHeader))
		prResult, err := h.enqueueRepositoryMonitorPullRequestEventRuns(c, body, delivery, payload)
		if err != nil {
			return err
		}
		monitorResult = mergeGitHubRepositoryMonitorEventResults(monitorResult, prResult)
	}

	if monitorResult.Matched > 0 {
		return githubRepositoryMonitorEventResponse(c, monitorResult)
	}
	return githubWebhookIgnored(c, "label is not a configured repository workflow trigger")
}

func (h *Handlers) enqueueRepositoryMonitorPullRequestEventRuns(c fiber.Ctx, body []byte, delivery string, payload githubLabelWebhookPayload) (githubRepositoryMonitorEventResult, error) {
	var result githubRepositoryMonitorEventResult
	if h.repositoryMonitorStore == nil || !repositoryMonitorExactPullRequestAction(payload.Action) || payload.PullRequest == nil {
		return result, nil
	}
	target, ok := payload.target()
	if !ok || !target.IsPR || target.IncompletePR || target.HeadSHA == "" {
		return result, nil
	}

	monitors := &corev1alpha1.RepositoryMonitorList{}
	if err := h.client.List(c.Context(), monitors, h.githubWebhookMonitorListOptions()...); err != nil {
		return result, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list repository monitors: %v", err))
	}

	deliveryKey := strings.TrimSpace(delivery)
	if deliveryKey == "" {
		deliveryKey = githubWebhookReplayKey(body)
	}
	for i := range monitors.Items {
		monitor := &monitors.Items[i]
		if (payload.Action == githubWebhookActionLabeled || payload.Action == githubWebhookActionUnlabeled) && repositoryMonitorWebhookMatchingLabel(repositoryMonitorAPIPauseLabels(monitor), []string{payload.Label.Name}) != "" && repositoryMonitorAcceptsPauseEvent(monitor, payload.Repository, target) {
			continue
		}
		if intent, isCommand := repositoryMonitorCommandIntentForLabel(monitor, target, payload.Label.Name); isCommand && repositoryMonitorAcceptsLabelCommand(monitor, payload.Repository, target, intent) {
			continue
		}
		if !repositoryMonitorAcceptsPullRequestEvent(monitor, payload.Repository, target) {
			continue
		}
		result.Matched++
		runID := githubRepositoryMonitorExactRunID(monitor, deliveryKey)
		run := &store.MonitorRun{
			ID:               runID,
			MonitorNamespace: monitor.Namespace,
			MonitorName:      monitor.Name,
			Trigger:          githubMonitorTriggerPullRequestEvent,
			TargetKind:       repositoryMonitorTargetKindPullRequest,
			TargetNumber:     int64(target.Number),
			TargetSHA:        target.HeadSHA,
			Phase:            repositoryMonitorRunPhaseQueued,
			StartedAt:        time.Now(),
		}
		duplicate, err := h.queuedRepositoryMonitorPullRequestEventRunExists(c, monitor, run)
		if err != nil {
			return result, err
		}
		if duplicate {
			result.Duplicate++
			continue
		}
		if err := h.repositoryMonitorStore.CreateMonitorRun(c.Context(), run); err != nil {
			if errors.Is(err, store.ErrConflict) {
				retryQueued, retryErr := h.requeueFailedRepositoryMonitorEventRun(c, run)
				if retryErr != nil {
					return result, retryErr
				}
				if !retryQueued {
					result.SkippedActive++
					continue
				}
			} else {
				return result, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create repository monitor event run: %v", err))
			}
		}
		if err := h.annotateRepositoryMonitorRunRequest(c, monitor, run); err != nil {
			if failErr := h.markRepositoryMonitorRunSignalFailed(c, run, err); failErr != nil {
				return result, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("%v; additionally failed to mark monitor run failed: %v", err, failErr))
			}
			return result, err
		}
		if err := h.createRepositoryMonitorEventRunAudit(c, monitor, run, payload, target, deliveryKey, githubMonitorEventTypeExactRunQueued); err != nil {
			return result, err
		}
		result.Queued++
		result.RunIDs = append(result.RunIDs, runID)
	}
	return result, nil
}

func (h *Handlers) githubWebhookMonitorListOptions() []crclient.ListOption {
	if h.watchNamespace == "" {
		return nil
	}
	return []crclient.ListOption{crclient.InNamespace(h.watchNamespace)}
}

func (h *Handlers) requeueFailedRepositoryMonitorEventRun(c fiber.Ctx, next *store.MonitorRun) (bool, error) {
	existing, err := h.repositoryMonitorStore.GetMonitorRun(c.Context(), next.MonitorNamespace, next.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to inspect repository monitor event run: %v", err))
	}
	if existing.Phase != repositoryMonitorRunPhaseFailed {
		return false, nil
	}
	events, _, err := h.repositoryMonitorStore.ListMonitorEvents(c.Context(), store.MonitorEventFilter{
		Namespace:   next.MonitorNamespace,
		MonitorName: next.MonitorName,
		RunID:       next.ID,
		EventType:   githubMonitorEventTypeExactRunQueued,
		Limit:       1,
	})
	if err != nil {
		return false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to inspect repository monitor event run audit: %v", err))
	}
	if len(events) > 0 {
		return false, nil
	}
	existing.Trigger = next.Trigger
	existing.TargetKind = next.TargetKind
	existing.TargetNumber = next.TargetNumber
	existing.TargetSHA = next.TargetSHA
	existing.Phase = repositoryMonitorRunPhaseQueued
	existing.StartedAt = next.StartedAt
	existing.CompletedAt = nil
	existing.Error = ""
	if err := h.repositoryMonitorStore.UpdateMonitorRun(c.Context(), existing); err != nil {
		return false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to requeue repository monitor event run: %v", err))
	}
	return true, nil
}

func (h *Handlers) queuedRepositoryMonitorPullRequestEventRunExists(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, next *store.MonitorRun) (bool, error) {
	runs, _, err := h.repositoryMonitorStore.ListMonitorRuns(c.Context(), store.MonitorRunFilter{
		Namespace:    monitor.Namespace,
		MonitorName:  monitor.Name,
		Trigger:      githubMonitorTriggerPullRequestEvent,
		TargetKind:   repositoryMonitorTargetKindPullRequest,
		TargetNumber: next.TargetNumber,
		TargetSHA:    next.TargetSHA,
		Phase:        repositoryMonitorRunPhaseQueued,
		Limit:        1,
	})
	if err != nil {
		return false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to inspect active repository monitor run: %v", err))
	}
	return len(runs) > 0, nil
}

func repositoryMonitorExactPullRequestAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "opened", "reopened", "synchronize", "ready_for_review", "labeled", "unlabeled":
		return true
	default:
		return false
	}
}

func repositoryMonitorAcceptsPullRequestEvent(monitor *corev1alpha1.RepositoryMonitor, repo githubWebhookRepository, target githubLabelTarget) bool {
	if monitor == nil || repositoryMonitorWebhookSuspended(monitor) || !monitor.Spec.Review.ExactEventEnabled || !repositoryMonitorPullRequestsEnabled(monitor.Spec) {
		return false
	}
	if target.BaseBranch != "" && !strings.EqualFold(target.BaseBranch, effectiveRepositoryMonitorBranch(monitor)) {
		return false
	}
	owner, repository, err := parseRepositoryMonitorGitHubURL(monitor.Spec.RepoURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(repo.FullName), owner+"/"+repository)
}

func repositoryMonitorWebhookSuspended(monitor *corev1alpha1.RepositoryMonitor) bool {
	return monitor.Spec.Suspend != nil && *monitor.Spec.Suspend
}

func githubRepositoryMonitorExactRunID(monitor *corev1alpha1.RepositoryMonitor, delivery string) string {
	monitorKey := monitor.Namespace + "/" + monitor.Name + "/" + delivery
	return fmt.Sprintf("monrun-%s-%s", dnsNamePart(monitor.Name), githubReplayKeySuffix(githubWebhookReplayKey([]byte(monitorKey))))
}

func (h *Handlers) createRepositoryMonitorEventRunAudit(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, payload githubLabelWebhookPayload, target githubLabelTarget, delivery, eventType string) error {
	eventKey := githubWebhookReplayKey([]byte(eventType + "-" + delivery))
	metadataJSON, err := json.Marshal(map[string]any{
		apiFieldAction: payload.Action,
		"delivery":     delivery,
		"repository":   payload.Repository.FullName,
		"sender":       payload.Sender.Login,
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode monitor event metadata: %v", err))
	}
	if err := h.repositoryMonitorStore.CreateMonitorEvent(c.Context(), &store.MonitorEvent{
		ID:               fmt.Sprintf("mevt-%s-%s", run.ID, githubReplayKeySuffix(eventKey)),
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		RunID:            run.ID,
		ItemKind:         target.Kind,
		ItemNumber:       int64(target.Number),
		ItemSHA:          target.HeadSHA,
		EventType:        eventType,
		Actor:            "github-webhook",
		Summary:          fmt.Sprintf("GitHub %s event queued repository monitor run for %s #%d", payload.Action, strings.ReplaceAll(target.Kind, "_", " "), target.Number),
		MetadataJSON:     string(metadataJSON),
	}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to record repository monitor event run audit: %v", err))
	}
	return nil
}

func githubRepositoryMonitorEventResponse(c fiber.Ctx, result githubRepositoryMonitorEventResult) error {
	status := fiber.StatusAccepted
	responseStatus := "accepted"
	if result.Queued > 0 {
		status = fiber.StatusCreated
		responseStatus = "created"
	}
	return c.Status(status).JSON(fiber.Map{
		apiFieldStatus:       responseStatus,
		"monitorRunsQueued":  result.Queued,
		"monitorRunsCached":  result.Duplicate,
		"monitorRunsSkipped": result.SkippedActive,
		"matchedMonitors":    result.Matched,
		"runIDs":             result.RunIDs,
		"commandIDs":         result.CommandIDs,
	})
}

func mergeGitHubRepositoryMonitorEventResults(a, b githubRepositoryMonitorEventResult) githubRepositoryMonitorEventResult {
	return githubRepositoryMonitorEventResult{
		Matched:       a.Matched + b.Matched,
		Queued:        a.Queued + b.Queued,
		Duplicate:     a.Duplicate + b.Duplicate,
		SkippedActive: a.SkippedActive + b.SkippedActive,
		RunIDs:        append(append([]string{}, a.RunIDs...), b.RunIDs...),
		CommandIDs:    append(append([]string{}, a.CommandIDs...), b.CommandIDs...),
	}
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

func githubWebhookReplayKey(body []byte) string {
	return hex.EncodeToString(githubHash(body))
}

func githubWebhookIgnored(c fiber.Ctx, reason string) error {
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		apiFieldStatus: "ignored",
		"reason":       reason,
	})
}

func (p githubLabelWebhookPayload) target() (githubLabelTarget, bool) {
	if p.PullRequest != nil {
		pr := p.PullRequest
		repo := p.Repository
		headRepo := pr.Head.Repo
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
			State:      pr.State,
			IsPR:       true,
			Draft:      pr.Draft,
			BaseBranch: pr.Base.Ref,
			BaseSHA:    pr.Base.SHA,
			HeadBranch: pr.Head.Ref,
			HeadSHA:    pr.Head.SHA,
			Repo:       repo,
			BaseRepo:   baseRepo,
			HeadRepo:   headRepo,
			Labels:     githubWebhookLabelNames(pr.Labels),
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
				State:        p.Issue.State,
				IsPR:         true,
				IncompletePR: true,
				Repo:         p.Repository,
				BaseRepo:     p.Repository,
				HeadRepo:     p.Repository,
				Labels:       githubWebhookLabelNames(p.Issue.Labels),
				UpdatedAt:    p.Issue.UpdatedAt,
			}, true
		}
		return githubLabelTarget{
			Kind:      "issue",
			Number:    p.Issue.Number,
			Title:     p.Issue.Title,
			Body:      p.Issue.Body,
			HTMLURL:   htmlURL,
			State:     p.Issue.State,
			Repo:      p.Repository,
			BaseRepo:  p.Repository,
			HeadRepo:  p.Repository,
			Labels:    githubWebhookLabelNames(p.Issue.Labels),
			UpdatedAt: p.Issue.UpdatedAt,
		}, true
	}

	return githubLabelTarget{}, false
}

func githubWebhookLabelNames(webhookLabels []githubWebhookLabel) []string {
	names := make([]string, 0, len(webhookLabels))
	for _, label := range webhookLabels {
		if strings.TrimSpace(label.Name) != "" {
			names = append(names, label.Name)
		}
	}
	return names
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

func githubSameRepository(a, b githubWebhookRepository) bool {
	return a.FullName != "" && b.FullName != "" && strings.EqualFold(a.FullName, b.FullName)
}

func githubReplayKeySuffix(replayKey string) string {
	replayKey = strings.TrimSpace(replayKey)
	if len(replayKey) >= 12 {
		return replayKey[:12]
	}
	return hex.EncodeToString(githubHash([]byte(replayKey)))[:12]
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
