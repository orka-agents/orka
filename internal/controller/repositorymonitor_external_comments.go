package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	repositoryMonitorMaxExternalThreads        = 20
	repositoryMonitorMaxCommentsPerThread      = 6
	repositoryMonitorMaxExternalCommentRunes   = 2000
	repositoryMonitorMaxExternalTotalRunes     = 16000
	repositoryMonitorMaxExternalReplyNoteRunes = 500
	repositoryMonitorExternalReplyOperation    = "reply_external_review_thread"
	repositoryMonitorExternalResolveOperation  = "resolve_external_review_thread"
	repositoryMonitorExternalThreadStatusFixed = "fixed"
	repositoryMonitorExternalThreadStatusWont  = "wont_fix"
	repositoryMonitorExternalThreadStatusHuman = "needs_human"
	repositoryMonitorGraphQLOwnerVariable      = "owner"
	repositoryMonitorGraphQLNameVariable       = "name"
	repositoryMonitorGraphQLBodyVariable       = "body"
	githubPermissionWrite                      = "write"
)

// repositoryMonitorExternalReviewThread is one trust-gated, sanitized, bounded
// unresolved pull request review thread carried in repair prompts.
type repositoryMonitorExternalReviewThread struct {
	ThreadID string                                   `json:"threadId"`
	Path     string                                   `json:"path,omitempty"`
	Line     int                                      `json:"line,omitempty"`
	Author   string                                   `json:"author"`
	IsBot    bool                                     `json:"isBot,omitempty"`
	Revision string                                   `json:"revision,omitempty"`
	Comments []repositoryMonitorExternalReviewComment `json:"comments"`
}

type repositoryMonitorExternalReviewComment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// repositoryMonitorRepairExternalResult is the optional structured summary a
// repair agent returns in its task result when addressing external review threads.
type repositoryMonitorRepairExternalResult struct {
	AddressedThreads []repositoryMonitorAddressedThread `json:"addressedThreads"`
}

type repositoryMonitorAddressedThread struct {
	ThreadID string `json:"threadId"`
	Status   string `json:"status"`
	Note     string `json:"note"`
}

func repositoryMonitorExternalCommentsEnabled(monitor *corev1alpha1.RepositoryMonitor) bool {
	return monitor != nil && monitor.Spec.Repair.Enabled && monitor.Spec.Repair.ExternalComments.Enabled
}

func repositoryMonitorExternalCommentsReplyOnFix(spec corev1alpha1.RepositoryMonitorRepairExternalCommentsSpec) bool {
	return spec.ReplyOnFix == nil || *spec.ReplyOnFix
}

func repositoryMonitorExternalBotAllowed(spec corev1alpha1.RepositoryMonitorRepairExternalCommentsSpec, login string, isBotActor bool) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	candidates := []string{login}
	if isBotActor && !strings.HasSuffix(strings.ToLower(login), "[bot]") {
		candidates = append(candidates, login+"[bot]")
	}
	return slices.ContainsFunc(spec.AllowedBots, func(allowed string) bool {
		allowed = strings.TrimSpace(allowed)
		for _, candidate := range candidates {
			if strings.EqualFold(allowed, candidate) {
				return true
			}
		}
		return false
	})
}

func repositoryMonitorExternalAuthorPermissionAllowed(spec corev1alpha1.RepositoryMonitorSpec, permission string) bool {
	allowed := []string{githubPermissionWrite, githubPermissionMaintain, githubPermissionAdmin}
	switch strings.ToLower(strings.TrimSpace(spec.Repair.ExternalComments.RequireAuthorPermission)) {
	case githubPermissionAdmin:
		allowed = []string{githubPermissionAdmin}
	case githubPermissionMaintain:
		allowed = []string{githubPermissionMaintain, githubPermissionAdmin}
	}
	if !repositoryMonitorPermissionListed(allowed, permission) {
		return false
	}
	policy := spec.Policy.AllowedRepositoryPermissions
	return len(policy) == 0 || repositoryMonitorPermissionListed(policy, permission)
}

type repositoryMonitorRawExternalThread struct {
	ID         string
	IsResolved bool
	IsOutdated bool
	Path       string
	Line       int
	Comments   []repositoryMonitorRawExternalComment
}

type repositoryMonitorRawExternalComment struct {
	ID         string
	UpdatedAt  string
	Author     string
	AuthorType string
	Body       string
}

func (r *RepositoryMonitorReconciler) fetchRepositoryMonitorExternalReviewThreads(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	owner, repository string,
	pr repositoryMonitorPullRequest,
) ([]repositoryMonitorExternalReviewThread, error) {
	if !repositoryMonitorExternalCommentsEnabled(monitor) || pr.Number <= 0 || strings.TrimSpace(pr.HeadSHA) == "" {
		return nil, nil
	}
	token, err := r.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return nil, err
	}
	rawThreads, err := r.listRepositoryMonitorExternalReviewThreadsRaw(ctx, owner, repository, token, pr.Number)
	if err != nil {
		return nil, err
	}
	if len(rawThreads) == 0 {
		return nil, nil
	}
	baseURL := repositoryMonitorGitHubBaseURL(r)
	permCache := map[string]bool{}
	spec := monitor.Spec.Repair.ExternalComments
	var out []repositoryMonitorExternalReviewThread
	totalRunes := 0

	for _, raw := range rawThreads {
		if len(out) >= repositoryMonitorMaxExternalThreads || totalRunes >= repositoryMonitorMaxExternalTotalRunes {
			break
		}
		if raw.IsResolved || raw.IsOutdated || strings.TrimSpace(raw.ID) == "" || len(raw.Comments) == 0 {
			continue
		}
		// Skip Orka's own inline review threads.
		if repositoryMonitorInlineFindingMarker.MatchString(raw.Comments[0].Body) {
			continue
		}
		// Trust-gate on the thread's opening author; subsequent comments in the
		// thread are included only if their author is also trusted.
		rootAuthor := strings.TrimSpace(raw.Comments[0].Author)
		rootBotActor := strings.EqualFold(strings.TrimSpace(raw.Comments[0].AuthorType), "Bot")
		rootAllowed, rootIsBot, err := r.repositoryMonitorExternalCommentAuthorAllowed(ctx, monitor, baseURL, owner, repository, token, rootAuthor, rootBotActor, permCache)
		if err != nil {
			return nil, err
		}
		if !rootAllowed {
			continue
		}

		comments := make([]repositoryMonitorExternalReviewComment, 0, min(len(raw.Comments), repositoryMonitorMaxCommentsPerThread))
		var revParts []string
		for _, c := range raw.Comments {
			if len(comments) >= repositoryMonitorMaxCommentsPerThread || totalRunes >= repositoryMonitorMaxExternalTotalRunes {
				break
			}
			author := strings.TrimSpace(c.Author)
			isBotActor := strings.EqualFold(strings.TrimSpace(c.AuthorType), "Bot")
			allowed, _, err := r.repositoryMonitorExternalCommentAuthorAllowed(ctx, monitor, baseURL, owner, repository, token, author, isBotActor, permCache)
			if err != nil {
				return nil, err
			}
			if !allowed {
				continue
			}
			remaining := repositoryMonitorMaxExternalTotalRunes - totalRunes
			limit := min(repositoryMonitorMaxExternalCommentRunes, remaining)
			if limit <= 0 {
				break
			}
			body := sanitizeRepositoryMonitorReviewText(c.Body, limit)
			if body == "" {
				continue
			}
			totalRunes += len([]rune(body))
			comments = append(comments, repositoryMonitorExternalReviewComment{
				Author: author,
				Body:   body,
			})
			revParts = append(revParts, strings.TrimSpace(c.ID)+"|"+strings.TrimSpace(c.UpdatedAt)+"|"+author+"|"+repositoryMonitorShortHash(body))
		}
		if len(comments) == 0 {
			continue
		}
		revision := repositoryMonitorShortHash(strings.Join(revParts, "\n"))
		// Skip threads whose exact trusted comment revision was already handled
		// by Orka (independent of head SHA so an unresolved thread doesn't loop
		// after a repair push, while a new trusted comment changes revision and
		// becomes eligible again).
		if r.Store != nil {
			ackID := repositoryMonitorExternalAckMutationID(monitor, pr.Number, raw.ID, revision)
			if existing, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, ackID); err == nil && existing.Status == repositoryMonitorRunPhaseSucceeded {
				continue
			}
			// Backward-compatible check for head-keyed reply records.
			mutID := repositoryMonitorExternalReplyMutationID(monitor, pr.Number, raw.ID, pr.HeadSHA)
			if existing, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, mutID); err == nil && existing.Status == repositoryMonitorRunPhaseSucceeded && existing.Reason == raw.ID {
				continue
			}
		}
		out = append(out, repositoryMonitorExternalReviewThread{
			ThreadID: strings.TrimSpace(raw.ID),
			Path:     sanitizeRepositoryMonitorReviewText(raw.Path, 256),
			Line:     raw.Line,
			Author:   rootAuthor,
			IsBot:    rootIsBot || repositoryMonitorExternalBotAllowed(spec, rootAuthor, rootBotActor),
			Revision: revision,
			Comments: comments,
		})
	}
	return out, nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorExternalCommentAuthorAllowed(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	baseURL, owner, repository, token, login string,
	isBotActor bool,
	cache map[string]bool,
) (allowed bool, isBot bool, err error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return false, false, nil
	}
	if repositoryMonitorExternalBotAllowed(monitor.Spec.Repair.ExternalComments, login, isBotActor) {
		return true, true, nil
	}
	key := strings.ToLower(login)
	if cached, ok := cache[key]; ok {
		return cached, false, nil
	}
	permission, err := r.repositoryMonitorCollaboratorPermission(ctx, baseURL, owner, repository, token, login)
	if err != nil {
		return false, false, err
	}
	ok := repositoryMonitorExternalAuthorPermissionAllowed(monitor.Spec, permission)
	cache[key] = ok
	return ok, false, nil
}

func (r *RepositoryMonitorReconciler) listRepositoryMonitorExternalReviewThreadsRaw(
	ctx context.Context,
	owner, repository, token string,
	number int64,
) ([]repositoryMonitorRawExternalThread, error) {
	const query = `query($owner:String!,$name:String!,$number:Int!,$after:String) {
 repository(owner:$owner,name:$name) { pullRequest(number:$number) {
  reviewThreads(first:100, after:$after) {
   nodes {
    id
    isResolved
    isOutdated
    path
    line
    originalLine
    comments(first:50) {
     nodes {
      id
      updatedAt
      body
      author { __typename login }
     }
    }
   }
   pageInfo { hasNextPage endCursor }
  }
 } }
}`
	var threads []repositoryMonitorRawExternalThread
	var after any
	for {
		var response struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID           string `json:"id"`
							IsResolved   bool   `json:"isResolved"`
							IsOutdated   bool   `json:"isOutdated"`
							Path         string `json:"path"`
							Line         *int   `json:"line"`
							OriginalLine *int   `json:"originalLine"`
							Comments     struct {
								Nodes []struct {
									ID        string `json:"id"`
									UpdatedAt string `json:"updatedAt"`
									Body      string `json:"body"`
									Author    *struct {
										TypeName string `json:"__typename"`
										Login    string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := r.repositoryMonitorGitHubGraphQL(ctx, token, query, map[string]any{
			repositoryMonitorGraphQLOwnerVariable:  owner,
			repositoryMonitorGraphQLNameVariable:   repository,
			repositoryMonitorGraphQLNumberVariable: number,
			"after":                                after,
		}, &response); err != nil {
			return nil, err
		}
		page := response.Repository.PullRequest.ReviewThreads
		for _, node := range page.Nodes {
			line := 0
			if node.Line != nil {
				line = *node.Line
			} else if node.OriginalLine != nil {
				line = *node.OriginalLine
			}
			item := repositoryMonitorRawExternalThread{
				ID:         node.ID,
				IsResolved: node.IsResolved,
				IsOutdated: node.IsOutdated,
				Path:       node.Path,
				Line:       line,
			}
			for _, c := range node.Comments.Nodes {
				login := ""
				authorType := ""
				if c.Author != nil {
					login = c.Author.Login
					authorType = c.Author.TypeName
				}
				item.Comments = append(item.Comments, repositoryMonitorRawExternalComment{
					ID:         c.ID,
					UpdatedAt:  c.UpdatedAt,
					Author:     login,
					AuthorType: authorType,
					Body:       c.Body,
				})
			}
			threads = append(threads, item)
		}
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" {
			return threads, nil
		}
		after = page.PageInfo.EndCursor
	}
}

func repositoryMonitorExternalThreadsFromPrompt(prompt string) []repositoryMonitorExternalReviewThread {
	decoded, ok := repositoryMonitorRepairPromptInput(prompt)
	if !ok {
		return nil
	}
	raw, ok := decoded["externalReviewThreads"]
	if !ok {
		return nil
	}
	var threads []repositoryMonitorExternalReviewThread
	if err := json.Unmarshal(raw, &threads); err != nil {
		return nil
	}
	return threads
}

func parseRepositoryMonitorRepairExternalResult(raw []byte) repositoryMonitorRepairExternalResult {
	var parsed repositoryMonitorRepairExternalResult
	if len(raw) == 0 {
		return parsed
	}
	// Try direct JSON first.
	if err := json.Unmarshal(raw, &parsed); err == nil && len(parsed.AddressedThreads) > 0 {
		return parsed
	}
	// Fallback: extract embedded JSON object (e.g., inside a markdown fenced block or prose).
	text := string(raw)
	for start := strings.IndexByte(text, '{'); start >= 0; {
		dec := json.NewDecoder(strings.NewReader(text[start:]))
		var candidate repositoryMonitorRepairExternalResult
		if err := dec.Decode(&candidate); err == nil && len(candidate.AddressedThreads) > 0 {
			return candidate
		}
		next := strings.IndexByte(text[start+1:], '{')
		if next < 0 {
			break
		}
		start += next + 1
	}
	return repositoryMonitorRepairExternalResult{}
}

func (r *RepositoryMonitorReconciler) replyExternalReviewThreadsAfterRepairBestEffort(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	job *store.RepairJob,
	task *corev1alpha1.Task,
) {
	if !repositoryMonitorExternalCommentsEnabled(monitor) || job == nil || task == nil {
		return
	}
	if job.Intent != repositoryMonitorCommandIntentFix || job.Phase != repositoryMonitorRepairPhaseSucceeded || strings.TrimSpace(job.PushedSHA) == "" {
		return
	}
	spec := monitor.Spec.Repair.ExternalComments
	if !repositoryMonitorExternalCommentsReplyOnFix(spec) && !spec.ResolveBots && !spec.ResolveOnFix {
		return
	}
	promptThreads := repositoryMonitorExternalThreadsFromPrompt(task.Spec.Prompt)
	if len(promptThreads) == 0 {
		return
	}
	if err := r.replyExternalReviewThreadsAfterRepair(ctx, monitor, job, task, promptThreads); err != nil {
		log.FromContext(ctx).Info("could not reply to external review threads after repair", "pr", job.PRNumber, "task", task.Name, "error", err.Error())
	}
}

func (r *RepositoryMonitorReconciler) replyExternalReviewThreadsAfterRepair(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	job *store.RepairJob,
	task *corev1alpha1.Task,
	promptThreads []repositoryMonitorExternalReviewThread,
) error {
	allowedThreads := make(map[string]repositoryMonitorExternalReviewThread, len(promptThreads))
	for _, thread := range promptThreads {
		if strings.TrimSpace(thread.ThreadID) != "" {
			allowedThreads[thread.ThreadID] = thread
		}
	}
	if len(allowedThreads) == 0 {
		return nil
	}

	var addressed []repositoryMonitorAddressedThread
	if r.ResultStore != nil {
		if raw, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name); err == nil {
			addressed = parseRepositoryMonitorRepairExternalResult(raw).AddressedThreads
		}
	}
	// Fail closed: if the repair agent did not return explicit structured
	// addressedThreads confirmation (or the result store is unavailable), do
	// not fabricate "fixed" replies or resolve any review threads.
	if len(addressed) == 0 {
		return nil
	}

	token, err := r.repositoryMonitorForgeToken(ctx, monitor)
	if err != nil {
		// Fall back to read token only when tests omit ForgeCredentialRef.
		token, err = r.repositoryMonitorGitHubToken(ctx, monitor)
		if err != nil {
			return err
		}
	}

	spec := monitor.Spec.Repair.ExternalComments
	replyOnFix := repositoryMonitorExternalCommentsReplyOnFix(spec)
	shortPushed := shortRepositoryMonitorHead(job.PushedSHA)
	marker := fmt.Sprintf("<!-- orka:repo-monitor-reply head=%s -->", strings.ToLower(strings.TrimSpace(job.HeadSHA)))
	seen := map[string]bool{}

	// Before resolving any thread, verify the PR head still matches the repair's
	// pushed SHA so a concurrent force-push never gets stale threads resolved.
	headStillMatchesPushed := true
	if spec.ResolveBots || spec.ResolveOnFix {
		if owner, repo, ownerErr := repositoryMonitorOwnerRepository(monitor); ownerErr == nil {
			if livePR, prErr := r.fetchRepositoryMonitorPullRequest(ctx, owner, repo, token, job.PRNumber); prErr == nil && livePR != nil {
				headStillMatchesPushed = strings.EqualFold(strings.TrimSpace(livePR.HeadSHA), strings.TrimSpace(job.PushedSHA))
			}
		}
	}

	var errs []error
	for _, entry := range addressed {
		threadID := strings.TrimSpace(entry.ThreadID)
		thread, ok := allowedThreads[threadID]
		if !ok || seen[threadID] {
			continue
		}
		seen[threadID] = true

		status := strings.ToLower(strings.TrimSpace(entry.Status))
		switch status {
		case repositoryMonitorExternalThreadStatusFixed, repositoryMonitorExternalThreadStatusWont, repositoryMonitorExternalThreadStatusHuman:
		default:
			// Unrecognized status must never be promoted to "fixed" or resolve a thread.
			status = repositoryMonitorExternalThreadStatusHuman
		}
		note := sanitizeRepositoryMonitorReviewText(entry.Note, repositoryMonitorMaxExternalReplyNoteRunes)
		if security.LooksLikeSecret(note) {
			note = ""
		}

		if replyOnFix {
			body := formatRepositoryMonitorExternalReplyBody(status, shortPushed, note, marker)
			if err := r.postRepositoryMonitorExternalThreadReply(ctx, monitor, job, threadID, body, token); err != nil {
				errs = append(errs, err)
			}
		}

		// Record the handled thread revision so unchanged unresolved threads
		// are not re-queued on the pushed head, while new trusted comments on
		// the same thread produce a different revision and remain eligible.
		if thread.Revision != "" {
			if err := r.recordRepositoryMonitorExternalThreadAck(ctx, monitor, job, threadID, thread.Revision); err != nil {
				errs = append(errs, err)
			}
		}

		shouldResolve := headStillMatchesPushed &&
			status == repositoryMonitorExternalThreadStatusFixed &&
			((thread.IsBot && spec.ResolveBots) || (!thread.IsBot && spec.ResolveOnFix))
		if shouldResolve {
			if err := r.resolveRepositoryMonitorExternalThreadOnce(ctx, monitor, job, threadID, token); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func formatRepositoryMonitorExternalReplyBody(status, shortPushedSHA, note, marker string) string {
	var prefix string
	switch status {
	case repositoryMonitorExternalThreadStatusWont:
		prefix = fmt.Sprintf("Reviewed in `%s` (no code change required).", shortPushedSHA)
	case repositoryMonitorExternalThreadStatusHuman:
		prefix = fmt.Sprintf("Needs human follow-up after `%s`.", shortPushedSHA)
	default:
		prefix = fmt.Sprintf("Addressed in `%s`.", shortPushedSHA)
	}
	if note != "" {
		return prefix + "\n\n" + note + "\n\n" + marker
	}
	return prefix + "\n\n" + marker
}

func (r *RepositoryMonitorReconciler) postRepositoryMonitorExternalThreadReply(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	job *store.RepairJob,
	threadID, body, token string,
) error {
	mutID := repositoryMonitorExternalReplyMutationID(monitor, job.PRNumber, threadID, job.HeadSHA)
	var mutation *store.GitHubMutationRecord
	if r.Store != nil {
		existing, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, mutID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			mutation = &store.GitHubMutationRecord{
				ID:           mutID,
				Operation:    repositoryMonitorExternalReplyOperation,
				TargetKind:   repositoryMonitorPullRequestKind,
				TargetNumber: job.PRNumber,
				TargetSHA:    job.HeadSHA,
				Reason:       threadID,
				ExternalID:   job.PushedSHA,
				Status:       repositoryMonitorAutomergeStateStarted,
			}
			if err := r.recordRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
				return err
			}
		case err != nil:
			return err
		case existing.Status == repositoryMonitorRunPhaseSucceeded:
			return nil
		default:
			mutation = existing
		}
	}

	replyErr := r.addRepositoryMonitorReviewThreadReply(ctx, token, threadID, body)
	if mutation != nil {
		mutation.Status, mutation.Error = repositoryMonitorRunPhaseSucceeded, ""
		if replyErr != nil {
			mutation.Status, mutation.Error = repositoryMonitorRunPhaseFailed, replyErr.Error()
		}
		if err := r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
			return err
		}
	}
	return replyErr
}

func (r *RepositoryMonitorReconciler) resolveRepositoryMonitorExternalThreadOnce(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	job *store.RepairJob,
	threadID, token string,
) error {
	mutID := repositoryMonitorExternalResolveMutationID(monitor, job.PRNumber, threadID, job.HeadSHA)
	var mutation *store.GitHubMutationRecord
	if r.Store != nil {
		existing, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, mutID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			mutation = &store.GitHubMutationRecord{
				ID:           mutID,
				Operation:    repositoryMonitorExternalResolveOperation,
				TargetKind:   repositoryMonitorPullRequestKind,
				TargetNumber: job.PRNumber,
				TargetSHA:    job.HeadSHA,
				Reason:       threadID,
				ExternalID:   job.PushedSHA,
				Status:       repositoryMonitorAutomergeStateStarted,
			}
			if err := r.recordRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
				return err
			}
		case err != nil:
			return err
		case existing.Status == repositoryMonitorRunPhaseSucceeded:
			return nil
		default:
			mutation = existing
		}
	}

	resolveErr := r.resolveRepositoryMonitorReviewThread(ctx, token, threadID)
	if mutation != nil {
		mutation.Status, mutation.Error = repositoryMonitorRunPhaseSucceeded, ""
		if resolveErr != nil {
			mutation.Status, mutation.Error = repositoryMonitorRunPhaseFailed, resolveErr.Error()
		}
		if err := r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
			return err
		}
	}
	return resolveErr
}

func (r *RepositoryMonitorReconciler) addRepositoryMonitorReviewThreadReply(ctx context.Context, token, threadID, body string) error {
	const mutation = `mutation($thread:ID!,$body:String!) { addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$thread,body:$body}) { comment { id } } }`
	var response struct {
		AddPullRequestReviewThreadReply struct {
			Comment struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"addPullRequestReviewThreadReply"`
	}
	if err := r.repositoryMonitorGitHubGraphQL(ctx, token, mutation, map[string]any{"thread": threadID, repositoryMonitorGraphQLBodyVariable: body}, &response); err != nil {
		return err
	}
	if strings.TrimSpace(response.AddPullRequestReviewThreadReply.Comment.ID) == "" {
		return fmt.Errorf("GitHub did not return a reply comment ID for review thread %s", threadID)
	}
	return nil
}

func (r *RepositoryMonitorReconciler) recordRepositoryMonitorExternalThreadAck(
	ctx context.Context,
	monitor *corev1alpha1.RepositoryMonitor,
	job *store.RepairJob,
	threadID, revision string,
) error {
	if r.Store == nil || strings.TrimSpace(revision) == "" {
		return nil
	}
	ackID := repositoryMonitorExternalAckMutationID(monitor, job.PRNumber, threadID, revision)
	existing, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, ackID)
	if err == nil && existing.Status == repositoryMonitorRunPhaseSucceeded {
		return nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	rec := &store.GitHubMutationRecord{
		ID:           ackID,
		Operation:    "ack_external_review_thread",
		TargetKind:   repositoryMonitorPullRequestKind,
		TargetNumber: job.PRNumber,
		TargetSHA:    job.PushedSHA,
		Reason:       threadID,
		ExternalID:   revision,
		Status:       repositoryMonitorRunPhaseSucceeded,
	}
	if errors.Is(err, store.ErrNotFound) {
		return r.recordRepositoryMonitorGitHubMutation(ctx, monitor, rec)
	}
	return r.updateRepositoryMonitorGitHubMutation(ctx, monitor, rec)
}

func repositoryMonitorExternalAckMutationID(monitor *corev1alpha1.RepositoryMonitor, prNumber int64, threadID, revision string) string {
	key := strings.Join([]string{monitor.Namespace, monitor.Name, strconv.FormatInt(prNumber, 10), threadID, revision, "ext-ack"}, "|")
	return "ghmut-" + repositoryMonitorShortHash(key)
}

func repositoryMonitorExternalReplyMutationID(monitor *corev1alpha1.RepositoryMonitor, prNumber int64, threadID, headSHA string) string {
	key := strings.Join([]string{monitor.Namespace, monitor.Name, strconv.FormatInt(prNumber, 10), threadID, headSHA, "ext-reply"}, "|")
	return "ghmut-" + repositoryMonitorShortHash(key)
}

func repositoryMonitorExternalResolveMutationID(monitor *corev1alpha1.RepositoryMonitor, prNumber int64, threadID, headSHA string) string {
	key := strings.Join([]string{monitor.Namespace, monitor.Name, strconv.FormatInt(prNumber, 10), threadID, headSHA, "ext-resolve"}, "|")
	return "ghmut-" + repositoryMonitorShortHash(key)
}
