package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

const (
	githubEventIssueComment            = "issue_comment"
	githubWebhookActionCreated         = "created"
	githubCommandEventSourceComment    = "github_comment"
	githubWebhookUserTypeBot           = "Bot"
	githubCommentCommandMaxCommandWord = 32
)

var repositoryMonitorCommentIntents = []string{
	githubActionTriage,
	githubActionResearch,
	commandIntentPlan,
	githubActionImplement,
	commandIntentDecompose,
	commandIntentStop,
	commandIntentResume,
}

type githubIssueCommentWebhookPayload struct {
	Action     string                  `json:"action"`
	Issue      *githubWebhookIssue     `json:"issue,omitempty"`
	Comment    githubWebhookComment    `json:"comment"`
	Repository githubWebhookRepository `json:"repository"`
	Sender     githubWebhookUser       `json:"sender"`
}

type githubWebhookComment struct {
	ID                    int64             `json:"id"`
	HTMLURL               string            `json:"html_url"`
	Body                  string            `json:"body"`
	User                  githubWebhookUser `json:"user"`
	AuthorAssociation     string            `json:"author_association"`
	PerformedViaGitHubApp json.RawMessage   `json:"performed_via_github_app,omitempty"`
}

type repositoryMonitorCommentCommand struct {
	Word    string
	Context string
}

func (h *Handlers) handleGitHubIssueCommentWebhook(c fiber.Ctx, body []byte) error {
	var payload githubIssueCommentWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid GitHub webhook payload")
	}
	if payload.Action != githubWebhookActionCreated {
		return githubWebhookIgnored(c, fmt.Sprintf("ignored issue_comment action %q", payload.Action))
	}
	if payload.Issue == nil || payload.Comment.ID <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "GitHub issue_comment payload has no issue or comment")
	}
	if payload.Issue.PullRequest != nil {
		return githubWebhookIgnored(c, "comment commands on pull requests are not supported")
	}
	if githubCommentFromBot(payload) {
		return githubWebhookIgnored(c, "comment author is a bot")
	}
	if h.repositoryMonitorStore == nil {
		return githubWebhookIgnored(c, "repository monitor store is not configured")
	}
	target, _ := githubLabelWebhookPayload{Repository: payload.Repository, Issue: payload.Issue}.target()
	delivery := strings.TrimSpace(c.Get(githubDeliveryHeader))
	if delivery == "" {
		delivery = githubWebhookReplayKey(body)
	}

	monitors := &corev1alpha1.RepositoryMonitorList{}
	if err := h.client.List(c.Context(), monitors, h.githubWebhookMonitorListOptions()...); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list repository monitors: %v", err))
	}
	var result githubRepositoryMonitorEventResult
	ignoredReason := "comment is not a configured repository monitor command"
	for i := range monitors.Items {
		monitor := &monitors.Items[i]
		if !monitor.Spec.Triggers.GitHub.Comments.Enabled || repositoryMonitorWebhookSuspended(monitor) || !repositoryMonitorWebhookRepositoryMatches(monitor, payload.Repository) {
			continue
		}
		parsed, ok := parseRepositoryMonitorCommentCommand(payload.Comment.Body, monitor.Spec.Triggers.GitHub.Comments.Prefix)
		if !ok {
			continue
		}
		intent, ok := repositoryMonitorCommentIntent(monitor, parsed.Word)
		if !ok {
			ignoredReason = fmt.Sprintf("unknown comment command %q", parsed.Word)
			continue
		}
		if !repositoryMonitorAcceptsCommandTarget(monitor, payload.Repository, target, intent) {
			continue
		}
		self, err := h.repositoryMonitorCommentFromMonitorIdentity(c.Context(), monitor, payload.Comment.User.Login)
		if err != nil {
			return fiber.NewError(fiber.StatusServiceUnavailable, fmt.Sprintf("failed to verify repository monitor identity: %v", err))
		}
		if self {
			ignoredReason = "comment author is the repository monitor's own identity"
			continue
		}
		commentResult, err := h.recordRepositoryMonitorCommentCommand(c, monitor, payload, target, parsed, intent, delivery)
		if err != nil {
			return err
		}
		result = mergeGitHubRepositoryMonitorEventResults(result, commentResult)
	}
	if result.Matched > 0 {
		return githubRepositoryMonitorEventResponse(c, result)
	}
	return githubWebhookIgnored(c, ignoredReason)
}

func (h *Handlers) recordRepositoryMonitorCommentCommand(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, payload githubIssueCommentWebhookPayload, target githubLabelTarget, parsed repositoryMonitorCommentCommand, intent, delivery string) (githubRepositoryMonitorEventResult, error) {
	result := githubRepositoryMonitorEventResult{Matched: 1}
	commentID := strconv.FormatInt(payload.Comment.ID, 10)
	author := strings.TrimSpace(payload.Comment.User.Login)
	metadata := controller.RepositoryMonitorCommandContextMetadata(controller.SanitizeRepositoryMonitorCommandContext(parsed.Context), author, payload.Comment.HTMLURL)
	command, duplicate, err := h.recordRepositoryMonitorCommand(c, monitor, payload.Repository, target, intent, delivery, repositoryMonitorCommandIntake{
		Source:            githubCommandEventSourceComment,
		Command:           parsed.Word,
		CommentID:         commentID,
		CommentURL:        payload.Comment.HTMLURL,
		Author:            author,
		AuthorAssociation: payload.Comment.AuthorAssociation,
		DedupeKey:         repositoryMonitorCommentCommandDedupeKey(monitor, payload.Repository, commentID),
		PermissionAllowed: func(permission string) bool {
			return repositoryMonitorCommentPermissionAllowed(monitor, permission)
		},
		Metadata: metadata,
	})
	if err != nil {
		return result, err
	}
	if duplicate {
		result.Duplicate++
	}
	result.CommandIDs = append(result.CommandIDs, command.ID)
	if command.Status == githubCommandStatusRecording {
		if err := h.annotateRepositoryMonitorRunRequest(c, monitor, &store.MonitorRun{ID: command.ID}); err != nil {
			return result, err
		}
		if err := h.finishRepositoryMonitorCommandRecording(c.Context(), monitor, command, metadata); err != nil {
			return result, err
		}
	}
	if command.Status != githubCommandStatusAccepted {
		return result, h.upsertRepositoryMonitorCommandWorkAction(c.Context(), monitor, command, "")
	}
	run, queued, err := h.queueRepositoryMonitorCommandRunWithMetadata(c, monitor, command, target, metadata)
	if err != nil {
		return result, err
	}
	if queued {
		result.Queued++
		result.RunIDs = append(result.RunIDs, run.ID)
	} else {
		result.SkippedActive++
	}
	return result, nil
}

func (h *Handlers) finishRepositoryMonitorCommandRecording(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, metadata map[string]any) error {
	command.Status = githubCommandStatusAccepted
	if err := h.upsertRepositoryMonitorCommandWorkActionWithMetadata(ctx, monitor, command, repositoryMonitorCommandRunID(command), metadata); err != nil {
		command.Status = githubCommandStatusRecording
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to record repository monitor command context: %v", err))
	}
	if command.Status != githubCommandStatusAccepted {
		return nil
	}
	if err := h.repositoryMonitorStore.UpdateCommandEvent(ctx, command); err != nil {
		command.Status = githubCommandStatusRecording
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to accept repository monitor command: %v", err))
	}
	metrics.RecordRepositoryMonitorCommand(command.Intent, command.Status)
	return nil
}

func parseRepositoryMonitorCommentCommand(body, prefix string) (repositoryMonitorCommentCommand, bool) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || strings.ContainsFunc(prefix, isASCIISpace) {
		return repositoryMonitorCommentCommand{}, false
	}
	body = strings.TrimPrefix(body, "\ufeff")
	firstLine, rest, _ := strings.Cut(body, "\n")
	firstLine = strings.TrimRight(firstLine, "\r")
	line := strings.TrimLeft(firstLine, " \t")
	if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		return repositoryMonitorCommentCommand{}, false
	}
	line = line[len(prefix):]
	if line == "" || (line[0] != ' ' && line[0] != '\t') {
		return repositoryMonitorCommentCommand{}, false
	}
	line = strings.TrimLeft(line, " \t")
	end := strings.IndexFunc(line, isASCIISpace)
	word := line
	remainder := ""
	if end >= 0 {
		word, remainder = line[:end], line[end:]
	}
	if word == "" || len(word) > githubCommentCommandMaxCommandWord {
		return repositoryMonitorCommentCommand{}, false
	}
	commandContext := strings.TrimSpace(remainder)
	if rest != "" {
		commandContext = strings.TrimSpace(commandContext + "\n" + rest)
	}
	return repositoryMonitorCommentCommand{Word: strings.ToLower(word), Context: commandContext}, true
}

func isASCIISpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '\v' || r == '\f'
}

func repositoryMonitorCommentIntent(monitor *corev1alpha1.RepositoryMonitor, word string) (string, bool) {
	word = strings.ToLower(strings.TrimSpace(word))
	if monitor == nil || word == "" {
		return "", false
	}
	commands := monitor.Spec.Triggers.GitHub.Comments.Commands
	if len(commands) == 0 {
		for _, intent := range repositoryMonitorCommentIntents {
			if word == intent {
				return intent, true
			}
		}
		return "", false
	}
	for _, command := range commands {
		if strings.EqualFold(strings.TrimSpace(command.Name), word) {
			intent := strings.TrimSpace(command.Intent)
			return intent, repositoryMonitorCommentIntentSupported(intent)
		}
	}
	return "", false
}

func repositoryMonitorCommentIntentSupported(intent string) bool {
	return slices.Contains(repositoryMonitorCommentIntents, intent)
}

func repositoryMonitorCommentPermissionAllowed(monitor *corev1alpha1.RepositoryMonitor, permission string) bool {
	if monitor == nil {
		return false
	}
	permission = strings.ToLower(strings.TrimSpace(permission))
	minimum := strings.TrimSpace(monitor.Spec.Triggers.GitHub.Comments.RequireActorPermission)
	if minimum == "" {
		minimum = githubPermissionMaintain
	}
	if !repositoryMonitorPermissionInList(permission, repositoryMonitorPermissionsAtLeast(minimum)) {
		return false
	}
	policyAllowed := monitor.Spec.Policy.AllowedRepositoryPermissions
	return len(policyAllowed) == 0 || repositoryMonitorPermissionInList(permission, policyAllowed)
}

func repositoryMonitorCommentCommandDedupeKey(monitor *corev1alpha1.RepositoryMonitor, repo githubWebhookRepository, commentID string) string {
	return strings.Join([]string{monitor.Namespace, monitor.Name, githubCommandEventSourceComment, strings.ToLower(strings.TrimSpace(repo.FullName)), commentID}, "|")
}

func repositoryMonitorWebhookRepositoryMatches(monitor *corev1alpha1.RepositoryMonitor, repo githubWebhookRepository) bool {
	owner, repository, err := parseRepositoryMonitorGitHubURL(monitor.Spec.RepoURL)
	return err == nil && strings.EqualFold(strings.TrimSpace(repo.FullName), owner+"/"+repository)
}

func githubCommentFromBot(payload githubIssueCommentWebhookPayload) bool {
	for _, user := range []githubWebhookUser{payload.Comment.User, payload.Sender} {
		if strings.EqualFold(strings.TrimSpace(user.Type), githubWebhookUserTypeBot) || strings.HasSuffix(strings.ToLower(strings.TrimSpace(user.Login)), "[bot]") {
			return true
		}
	}
	app := strings.TrimSpace(string(payload.Comment.PerformedViaGitHubApp))
	return app != "" && app != "null"
}

func (h *Handlers) repositoryMonitorCommentFromMonitorIdentity(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, login string) (bool, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return false, fmt.Errorf("GitHub comment author is missing")
	}
	token, err := h.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return false, err
	}
	baseURL := strings.TrimRight(osGetenv(githubAPIBaseURLEnv), "/")
	if baseURL == "" {
		baseURL = githubAPIDefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/user", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", strings.Join([]string{githubAuthorizationBearer, token}, " "))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("GitHub identity lookup failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("GitHub identity lookup returned %d", resp.StatusCode)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(data, &user); err != nil || strings.TrimSpace(user.Login) == "" {
		return false, fmt.Errorf("GitHub identity lookup returned no login")
	}
	return strings.EqualFold(strings.TrimSpace(user.Login), login), nil
}
