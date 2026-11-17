package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

// Review feedback evidence is what people (and allowlisted review bots) said
// about one pull request head: its unresolved review threads and the
// reviewers' current changes-requested reviews. It is untrusted reviewer text,
// sanitized and bounded with the CI evidence sanitizer before it enters a
// repair prompt.
//
// Evidence and authority are separate. Evidence includes outside reviewers,
// the pull request author, and comments whose author's permission could not be
// read, each marked with the permission GitHub reported. Only an author whose
// repository permission meets the policy minimum, or an allowlisted bot, can
// start a repair; the pull request author never can. Orka never replies to,
// resolves, or dismisses these threads and reviews.
const (
	repositoryMonitorReviewFeedbackSchemaVersion = "orka.prRepair.reviewFeedback.v1"

	repositoryMonitorFeedbackKindThread  = "review_thread"
	repositoryMonitorFeedbackKindComment = "review_comment"
	repositoryMonitorFeedbackKindReview  = "review"

	repositoryMonitorFeedbackAuthorUser    = "user"
	repositoryMonitorFeedbackAuthorBot     = "bot"
	repositoryMonitorFeedbackAuthorDeleted = "deleted"

	// Permission values beyond GitHub's roles. none: not a collaborator.
	// unknown: the lookup failed or was not made. bot: an allowlisted bot,
	// which the collaborator API cannot describe.
	repositoryMonitorFeedbackPermissionNone    = "none"
	repositoryMonitorFeedbackPermissionUnknown = "unknown"
	repositoryMonitorFeedbackPermissionBot     = "bot"

	// Why a feedback entry can or cannot start a repair.
	repositoryMonitorFeedbackAuthorityAuthorized        = "authorized"
	repositoryMonitorFeedbackAuthorityBotAllowlisted    = "bot_allowlisted"
	repositoryMonitorFeedbackAuthorityBelowMinimum      = "permission_below_minimum"
	repositoryMonitorFeedbackAuthorityLookupFailed      = "permission_lookup_failed"
	repositoryMonitorFeedbackAuthorityLookupLimit       = "permission_lookup_limit"
	repositoryMonitorFeedbackAuthorityPRAuthor          = "pull_request_author"
	repositoryMonitorFeedbackAuthorityAuthorUnavailable = "author_unavailable"
	repositoryMonitorFeedbackAuthorityOutdated          = "thread_outdated"
	repositoryMonitorFeedbackAuthorityOrkaThread        = "orka_finding_thread"
	repositoryMonitorFeedbackAuthorityEmptyBody         = "empty_body"
	repositoryMonitorFeedbackAuthorityReviewsDisabled   = "changes_requested_reviews_disabled"

	// Snapshot reasons that are not a GitHub error class.
	repositoryMonitorFeedbackReasonHeadMoved       = "pull_request_head_moved"
	repositoryMonitorFeedbackReasonThreadPageLimit = "review_thread_page_limit"
	repositoryMonitorFeedbackReasonReviewPageLimit = "review_page_limit"
	repositoryMonitorFeedbackReasonMalformed       = "malformed_response"
	// repositoryMonitorFeedbackReasonResponseTooLarge stops a read whose
	// smallest page still exceeds the GitHub response limit.
	repositoryMonitorFeedbackReasonResponseTooLarge = "review_feedback_response_too_large"

	// Bounds. Text limits are UTF-8 bytes after sanitizing; encoded limits
	// are bytes of the JSON encoding.
	repositoryMonitorFeedbackThreadsPerPage       = 50
	repositoryMonitorFeedbackThreadPages          = 4
	repositoryMonitorFeedbackReviewsPerPage       = 50
	repositoryMonitorFeedbackReviewPages          = 4
	repositoryMonitorFeedbackRecentComments       = 20
	repositoryMonitorFeedbackMaxPermissionLookups = 20
	repositoryMonitorFeedbackBodyBytes            = 4 << 10
	repositoryMonitorFeedbackBodyLines            = 80
	repositoryMonitorFeedbackThreadEncodedBytes   = 8 << 10
	repositoryMonitorFeedbackMinCutBody           = 64
	repositoryMonitorFeedbackTotalEncodedBytes    = 32 << 10
	repositoryMonitorFeedbackFetchBudget          = 20 * time.Second

	repositoryMonitorFeedbackTruncatedBody = "body"
)

// repositoryMonitorReviewFeedbackPolicy decides which feedback can start a
// repair. Evidence inclusion does not depend on it, except that only
// allowlisted bots' comments are evidence at all.
type repositoryMonitorReviewFeedbackPolicy struct {
	// MinPermission is write, maintain, or admin.
	MinPermission string
	// AllowedPermissions, when set, also limits the permitted roles.
	AllowedPermissions []string
	// BotLogins lists the bots whose inline comments are evidence and can
	// start a repair.
	BotLogins []string
	// ChangesRequestedReviews lets a changes-requested review start a repair.
	ChangesRequestedReviews bool
}

// repositoryMonitorDefaultReviewFeedbackPolicy is the policy for a monitor
// that configures no human feedback settings: the label command permission
// minimum, no bots, and changes-requested reviews included.
func repositoryMonitorDefaultReviewFeedbackPolicy(spec corev1alpha1.RepositoryMonitorSpec) repositoryMonitorReviewFeedbackPolicy {
	return repositoryMonitorReviewFeedbackPolicy{
		MinPermission:           spec.Triggers.GitHub.Labels.RequireActorPermission,
		AllowedPermissions:      spec.Policy.AllowedRepositoryPermissions,
		ChangesRequestedReviews: true,
	}
}

func (p repositoryMonitorReviewFeedbackPolicy) botAllowlisted(login string) bool {
	login = repositoryMonitorFeedbackBotKey(login)
	return login != "" && slices.ContainsFunc(p.BotLogins, func(candidate string) bool {
		return repositoryMonitorFeedbackBotKey(candidate) == login
	})
}

// repositoryMonitorFeedbackBotKey normalizes a bot login: GraphQL names a
// GitHub App bot without the "[bot]" suffix REST uses.
func repositoryMonitorFeedbackBotKey(login string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]")
}

// repositoryMonitorReviewFeedbackSnapshot is the normalized review feedback
// for one pull request head.
type repositoryMonitorReviewFeedbackSnapshot struct {
	SchemaVersion  string `json:"schemaVersion"`
	Repo           string `json:"repo"`
	PRNumber       int64  `json:"prNumber"`
	HeadSHA        string `json:"headSHA"`
	ReviewDecision string `json:"reviewDecision,omitempty"`
	// Availability uses the CI evidence values; reason names the cause.
	Availability string `json:"availability"`
	Reason       string `json:"reason,omitempty"`
	// Partial means GitHub had more threads or reviews than were read, or a
	// later page failed: feedback may be missing, and none of it starts a
	// repair.
	Partial        bool                                  `json:"partial,omitempty"`
	Threads        []repositoryMonitorFeedbackThread     `json:"threads"`
	OmittedThreads int                                   `json:"omittedThreads,omitempty"`
	Reviews        []repositoryMonitorFeedbackReview     `json:"changesRequestedReviews"`
	OmittedReviews int                                   `json:"omittedReviews,omitempty"`
	authorized     []repositoryMonitorFeedbackAuthorized `json:"-"`
	// unverified maps the IDs of included entries whose author's permission
	// was not read to why: the lookup failed, or the lookup limit was
	// reached. Whether they may start a repair is unknown, not refused.
	unverified map[string]string
	// omittedWithAuthority counts the threads and reviews left out of the
	// snapshot, by its budget or because nothing in them could be shown,
	// that hold feedback by an author with the authority to start a repair,
	// or a thread whose own omitted comments may hide such feedback.
	omittedWithAuthority int
}

// repositoryMonitorFeedbackThread is one unresolved review thread.
type repositoryMonitorFeedbackThread struct {
	ID   string `json:"id"`
	Path string `json:"path,omitempty"`
	// Line is the thread's line in the current diff. An outdated thread has
	// none: its comments were made on code this head changed, so only
	// originalLine, a line in an earlier diff, is kept.
	Line         int    `json:"line,omitempty"`
	OriginalLine int    `json:"originalLine,omitempty"`
	Side         string `json:"side,omitempty"`
	Outdated     bool   `json:"outdated,omitempty"`
	// OrkaFindingThread marks a thread opened by one of Orka's own review
	// comments. Replies there are evidence about Orka's finding only.
	OrkaFindingThread bool                               `json:"orkaFindingThread,omitempty"`
	Comments          []repositoryMonitorFeedbackComment `json:"comments"`
	OmittedComments   int                                `json:"omittedComments,omitempty"`
}

// repositoryMonitorFeedbackComment is one bounded review comment.
type repositoryMonitorFeedbackComment struct {
	ID         string `json:"id"`
	Author     string `json:"author"`
	AuthorType string `json:"authorType"`
	// Permission is the author's repository permission as GitHub reported it.
	Permission   string     `json:"permission"`
	Body         string     `json:"body"`
	URL          string     `json:"url,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastEditedAt *time.Time `json:"lastEditedAt,omitempty"`
	// HiddenContentRemoved means HTML comments, which GitHub does not
	// render, were removed from the body.
	HiddenContentRemoved bool     `json:"hiddenContentRemoved,omitempty"`
	Truncated            []string `json:"truncated,omitempty"`
}

// repositoryMonitorFeedbackReview is a reviewer's current changes-requested
// review.
type repositoryMonitorFeedbackReview struct {
	ID         string `json:"id"`
	Author     string `json:"author"`
	AuthorType string `json:"authorType"`
	Permission string `json:"permission"`
	// CommitSHA is the commit the review was submitted on; EarlierHead marks
	// a review of an earlier head that still requests changes.
	CommitSHA            string     `json:"commitSHA,omitempty"`
	EarlierHead          bool       `json:"earlierHead,omitempty"`
	Body                 string     `json:"body"`
	URL                  string     `json:"url,omitempty"`
	SubmittedAt          time.Time  `json:"submittedAt"`
	LastEditedAt         *time.Time `json:"lastEditedAt,omitempty"`
	HiddenContentRemoved bool       `json:"hiddenContentRemoved,omitempty"`
	Truncated            []string   `json:"truncated,omitempty"`
}

// repositoryMonitorFeedbackAuthorized is one included comment or review by
// an author with the authority to start a repair, with the activity time a
// quiet period counts from. Current entries can start one now; the others
// (an outdated thread, a changes-requested review with no body, or one the
// policy does not act on) are open requests automation will not act on.
type repositoryMonitorFeedbackAuthorized struct {
	ID         string
	Author     string
	ActivityAt time.Time
	Current    bool
	// Reason is why a non-current entry cannot start a repair.
	Reason string
}

// repositoryMonitorFeedbackAuthority records whether one entry can start a
// repair and why, for the controller only.
type repositoryMonitorFeedbackAuthority struct {
	canStart bool
	reason   string
}

// digest identifies the exact snapshot bytes a repair attempt received.
func (s repositoryMonitorReviewFeedbackSnapshot) digest() string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// evidenceIDs lists the thread, comment, and review IDs the snapshot holds.
func (s repositoryMonitorReviewFeedbackSnapshot) evidenceIDs() []string {
	var ids []string
	for _, thread := range s.Threads {
		ids = append(ids, thread.ID)
		for _, comment := range thread.Comments {
			ids = append(ids, comment.ID)
		}
	}
	for _, review := range s.Reviews {
		ids = append(ids, review.ID)
	}
	return ids
}

// canStartRepair reports whether the snapshot may start a repair at all: it
// must be complete and current. Budget truncation is allowed, since only
// included entries are startable.
func (s repositoryMonitorReviewFeedbackSnapshot) canStartRepair() bool {
	return !s.Partial && (s.Availability == repositoryMonitorEvidenceAvailable || s.Availability == repositoryMonitorEvidenceTruncated)
}

// startableFeedback returns the included entries that can start a repair,
// or none when the snapshot cannot.
func (s repositoryMonitorReviewFeedbackSnapshot) startableFeedback() []repositoryMonitorFeedbackAuthorized {
	if !s.canStartRepair() {
		return nil
	}
	var startable []repositoryMonitorFeedbackAuthorized
	for _, entry := range s.authorized {
		if entry.Current {
			startable = append(startable, entry)
		}
	}
	return startable
}

// authorizedFeedback returns every included entry by an author with the
// authority to start a repair, current or not.
func (s repositoryMonitorReviewFeedbackSnapshot) authorizedFeedback() []repositoryMonitorFeedbackAuthorized {
	return slices.Clone(s.authorized)
}

// newestActivity is the latest creation or edit time of any included
// comment or review, which a quiet period counts from.
func (s repositoryMonitorReviewFeedbackSnapshot) newestActivity() time.Time {
	var newest time.Time
	see := func(created time.Time, edited *time.Time) {
		if activity := repositoryMonitorFeedbackActivity(created, edited); activity.After(newest) {
			newest = activity
		}
	}
	for _, thread := range s.Threads {
		for _, comment := range thread.Comments {
			see(comment.CreatedAt, comment.LastEditedAt)
		}
	}
	for _, review := range s.Reviews {
		see(review.SubmittedAt, review.LastEditedAt)
	}
	return newest
}

func newRepositoryMonitorReviewFeedbackSnapshot(repo string, pr repositoryMonitorPullRequest) repositoryMonitorReviewFeedbackSnapshot {
	return repositoryMonitorReviewFeedbackSnapshot{
		SchemaVersion: repositoryMonitorReviewFeedbackSchemaVersion,
		Repo:          repo,
		PRNumber:      pr.Number,
		HeadSHA:       pr.HeadSHA,
		Availability:  repositoryMonitorEvidenceAvailable,
		Threads:       []repositoryMonitorFeedbackThread{},
		Reviews:       []repositoryMonitorFeedbackReview{},
	}
}

// GitHub GraphQL response shapes.

type repositoryMonitorFeedbackActor struct {
	Login    string `json:"login"`
	TypeName string `json:"__typename"`
}

type repositoryMonitorFeedbackCommentNode struct {
	FullDatabaseID    string                                  `json:"fullDatabaseId"`
	Author            *repositoryMonitorFeedbackActor         `json:"author"`
	Body              string                                  `json:"body"`
	URL               string                                  `json:"url"`
	CreatedAt         time.Time                               `json:"createdAt"`
	LastEditedAt      *time.Time                              `json:"lastEditedAt"`
	IsMinimized       bool                                    `json:"isMinimized"`
	PullRequestReview *repositoryMonitorFeedbackCommentReview `json:"pullRequestReview"`
}

// repositoryMonitorFeedbackCommentReview is the review a thread comment
// belongs to.
type repositoryMonitorFeedbackCommentReview struct {
	FullDatabaseID string `json:"fullDatabaseId"`
	State          string `json:"state"`
}

// repositoryMonitorFeedbackPendingComment reports a comment in a review its
// author has not submitted. GitHub shows such drafts only to their author,
// which may be the account behind the monitor's token, so they are never
// feedback.
func repositoryMonitorFeedbackPendingComment(comment repositoryMonitorFeedbackCommentNode) bool {
	return comment.PullRequestReview != nil && strings.EqualFold(strings.TrimSpace(comment.PullRequestReview.State), "PENDING")
}

type repositoryMonitorFeedbackThreadNode struct {
	ID           string `json:"id"`
	IsResolved   bool   `json:"isResolved"`
	IsOutdated   bool   `json:"isOutdated"`
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"originalLine"`
	DiffSide     string `json:"diffSide"`
	Root         struct {
		TotalCount int                                    `json:"totalCount"`
		Nodes      []repositoryMonitorFeedbackCommentNode `json:"nodes"`
	} `json:"root"`
	Recent struct {
		Nodes []repositoryMonitorFeedbackCommentNode `json:"nodes"`
	} `json:"recent"`
}

type repositoryMonitorFeedbackReviewNode struct {
	FullDatabaseID string                          `json:"fullDatabaseId"`
	State          string                          `json:"state"`
	Author         *repositoryMonitorFeedbackActor `json:"author"`
	Body           string                          `json:"body"`
	URL            string                          `json:"url"`
	SubmittedAt    *time.Time                      `json:"submittedAt"`
	LastEditedAt   *time.Time                      `json:"lastEditedAt"`
	IsMinimized    bool                            `json:"isMinimized"`
	Commit         *struct {
		OID string `json:"oid"`
	} `json:"commit"`
}

type repositoryMonitorFeedbackPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// repositoryMonitorFeedbackBackwardPageInfo is the page info of a connection
// read backward, from its newest page.
type repositoryMonitorFeedbackBackwardPageInfo struct {
	HasPreviousPage bool   `json:"hasPreviousPage"`
	StartCursor     string `json:"startCursor"`
}

// repositoryMonitorRawReviewFeedback is what the GitHub reads returned.
type repositoryMonitorRawReviewFeedback struct {
	headSHA        string
	prAuthor       string
	reviewDecision string
	threads        []repositoryMonitorFeedbackThreadNode
	reviews        []repositoryMonitorFeedbackReviewNode
	// partial and reason describe reads that stopped early.
	partial      bool
	availability string
	reason       string
}

const repositoryMonitorFeedbackCommentFields = `fullDatabaseId author { login __typename } body url createdAt lastEditedAt isMinimized pullRequestReview { fullDatabaseId state }`

// repositoryMonitorFeedbackThreadPageSize is the review threads requested
// per page. Each thread carries up to 21 comment bodies, so a page of very
// long comments can exceed the GitHub response limit; the read then asks
// for that page again at half the size, down to one thread.
const repositoryMonitorFeedbackThreadPageSize = 50

const repositoryMonitorFeedbackThreadsQuery = `query($owner:String!,$name:String!,$number:Int!,$after:String,$first:Int!) {
 repository(owner:$owner,name:$name) { pullRequest(number:$number) {
  headRefOid reviewDecision author { login __typename }
  reviewThreads(first:$first, after:$after) {
   pageInfo { hasNextPage endCursor }
   nodes { id isResolved isOutdated path line originalLine diffSide
    root: comments(first:1) { totalCount nodes { ` + repositoryMonitorFeedbackCommentFields + ` } }
    recent: comments(last:20) { nodes { ` + repositoryMonitorFeedbackCommentFields + ` } }
   }
  }
 } }
}`

// repositoryMonitorFeedbackReviewsQuery reads reviews newest page first, so
// a read cut at its page limit still holds the latest verdict of every
// reviewer it holds. Each page lists its own reviews oldest first.
const repositoryMonitorFeedbackReviewsQuery = `query($owner:String!,$name:String!,$number:Int!,$before:String) {
 repository(owner:$owner,name:$name) { pullRequest(number:$number) {
  headRefOid
  reviews(last:50, before:$before) {
   pageInfo { hasPreviousPage startCursor }
   nodes { fullDatabaseId state body url submittedAt lastEditedAt isMinimized author { login __typename } commit { oid } }
  }
 } }
}`

// fetchRepositoryMonitorReviewFeedback reads a bounded number of review
// thread and review pages. It never fails: a read that stops early keeps
// the pages already read and reports why, with partial set.
func (r *RepositoryMonitorReconciler) fetchRepositoryMonitorReviewFeedback(ctx context.Context, token, owner, repository string, number int64) repositoryMonitorRawReviewFeedback {
	raw := repositoryMonitorRawReviewFeedback{availability: repositoryMonitorEvidenceAvailable}
	stop := func(err error) {
		raw.partial = true
		raw.availability, raw.reason = repositoryMonitorEvidenceErrorAvailability(err)
		if errors.Is(err, errRepositoryMonitorGitHubResponseTooLarge) {
			// A thread page is already as small as it gets, and reviews are
			// not read in smaller pages, so reading again cannot help: like
			// a page limit, this is more feedback than one read holds.
			raw.availability, raw.reason = repositoryMonitorEvidenceAvailable, repositoryMonitorFeedbackReasonResponseTooLarge
		}
	}
	// checkHead records the head each page reports and reports false, with
	// the reason set, for a missing head or pages that disagree, which mean
	// the head moved during the read.
	checkHead := func(head string) bool {
		head = strings.TrimSpace(head)
		switch {
		case head == "":
			raw.partial, raw.availability, raw.reason = true, repositoryMonitorEvidenceUnavailable, repositoryMonitorFeedbackReasonMalformed
			return false
		case raw.headSHA == "":
			raw.headSHA = head
		case head != raw.headSHA:
			raw.availability, raw.reason = repositoryMonitorEvidenceStale, repositoryMonitorFeedbackReasonHeadMoved
			return false
		}
		return true
	}
	var after any
	pageSize := repositoryMonitorFeedbackThreadPageSize
	for page := 1; ; page++ {
		var response struct {
			Repository *struct {
				PullRequest *struct {
					HeadRefOID     string                          `json:"headRefOid"`
					ReviewDecision *string                         `json:"reviewDecision"`
					Author         *repositoryMonitorFeedbackActor `json:"author"`
					ReviewThreads  struct {
						PageInfo repositoryMonitorFeedbackPageInfo     `json:"pageInfo"`
						Nodes    []repositoryMonitorFeedbackThreadNode `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		variables := repositoryMonitorPullRequestGraphQLVariables(owner, repository, number, after)
		variables["first"] = pageSize
		if err := r.repositoryMonitorGitHubGraphQL(ctx, token, repositoryMonitorFeedbackThreadsQuery, variables, &response); err != nil {
			if errors.Is(err, errRepositoryMonitorGitHubResponseTooLarge) && pageSize > 1 {
				// Ask for the same page again, smaller; it does not count
				// against the page limit.
				pageSize = max(1, pageSize/2)
				page--
				continue
			}
			stop(err)
			return raw
		}
		if response.Repository == nil || response.Repository.PullRequest == nil {
			raw.partial, raw.availability, raw.reason = true, repositoryMonitorEvidenceUnavailable, repositoryMonitorFeedbackReasonMalformed
			return raw
		}
		pr := response.Repository.PullRequest
		if !checkHead(pr.HeadRefOID) {
			return raw
		}
		if page == 1 {
			if pr.Author != nil {
				raw.prAuthor = pr.Author.Login
			}
			if pr.ReviewDecision != nil {
				raw.reviewDecision = *pr.ReviewDecision
			}
		}
		raw.threads = append(raw.threads, pr.ReviewThreads.Nodes...)
		info := pr.ReviewThreads.PageInfo
		if !info.HasNextPage {
			break
		}
		if page == repositoryMonitorFeedbackThreadPages || info.EndCursor == "" {
			raw.partial, raw.reason = true, repositoryMonitorFeedbackReasonThreadPageLimit
			break
		}
		after = info.EndCursor
	}
	// Reviews are read newest page first and kept oldest first, so the
	// reviews read are always the newest ones: a reviewer whose latest
	// review is older than the pages read is absent, never shown with an
	// earlier verdict.
	var before any
	for page := 1; ; page++ {
		var response struct {
			Repository *struct {
				PullRequest *struct {
					HeadRefOID string `json:"headRefOid"`
					Reviews    struct {
						PageInfo repositoryMonitorFeedbackBackwardPageInfo `json:"pageInfo"`
						Nodes    []repositoryMonitorFeedbackReviewNode     `json:"nodes"`
					} `json:"reviews"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		// The reviews query declares before, not after.
		variables := repositoryMonitorPullRequestGraphQLVariables(owner, repository, number, nil)
		delete(variables, "after")
		variables["before"] = before
		if err := r.repositoryMonitorGitHubGraphQL(ctx, token, repositoryMonitorFeedbackReviewsQuery, variables, &response); err != nil {
			stop(err)
			return raw
		}
		if response.Repository == nil || response.Repository.PullRequest == nil {
			raw.partial, raw.availability, raw.reason = true, repositoryMonitorEvidenceUnavailable, repositoryMonitorFeedbackReasonMalformed
			return raw
		}
		pr := response.Repository.PullRequest
		if !checkHead(pr.HeadRefOID) {
			return raw
		}
		raw.reviews = append(slices.Clone(pr.Reviews.Nodes), raw.reviews...)
		info := pr.Reviews.PageInfo
		if !info.HasPreviousPage {
			break
		}
		if page == repositoryMonitorFeedbackReviewPages || info.StartCursor == "" {
			raw.partial = true
			if raw.reason == "" {
				raw.reason = repositoryMonitorFeedbackReasonReviewPageLimit
			}
			break
		}
		before = info.StartCursor
	}
	return raw
}

// buildRepositoryMonitorReviewFeedback reads and normalizes the review
// feedback of pr's head. GitHub failures degrade to a snapshot that says why;
// only an invalid repository URL or a store failure is returned.
func (r *RepositoryMonitorReconciler) buildRepositoryMonitorReviewFeedback(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, policy repositoryMonitorReviewFeedbackPolicy) (repositoryMonitorReviewFeedbackSnapshot, error) {
	owner, repository, err := security.ParseGitHubRepositoryURL(monitor.Spec.RepoURL)
	if err != nil {
		return repositoryMonitorReviewFeedbackSnapshot{}, err
	}
	snapshot := newRepositoryMonitorReviewFeedbackSnapshot(owner+"/"+repository, pr)
	orkaReviews, err := r.repositoryMonitorPublishedReviewIDs(ctx, monitor, pr.Number)
	if err != nil {
		return snapshot, err
	}
	ctx, cancel := context.WithTimeout(ctx, repositoryMonitorFeedbackFetchBudget)
	defer cancel()
	token, err := r.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		snapshot.Availability, snapshot.Reason = repositoryMonitorEvidenceUnavailable, repositoryMonitorEvidenceReasonCredentialMissing
		return snapshot, nil
	}
	raw := r.fetchRepositoryMonitorReviewFeedback(ctx, token, owner, repository, pr.Number)
	baseURL := repositoryMonitorGitHubBaseURL(r)
	permissionOf := func(login string) (string, error) {
		return r.repositoryMonitorCollaboratorPermission(ctx, baseURL, owner, repository, token, login)
	}
	return normalizeRepositoryMonitorReviewFeedback(snapshot, pr, raw, orkaReviews, policy, permissionOf), nil
}

// repositoryMonitorPublishedReviewIDs returns the GitHub review IDs Orka
// published on the pull request. A comment is Orka's own only when it
// belongs to one of them; a copied hidden marker does not make it so.
//
// Every monitor in the namespace counts, not only this one: another monitor
// of the same repository, or a deleted or renamed one, publishes Orka's
// reviews too, and its reviews must not read as human feedback. Publish
// records do not name the repository, so records of the same pull request
// number in other repositories are included as well; GitHub review IDs are
// unique across repositories, so those never match a review here.
func (r *RepositoryMonitorReconciler) repositoryMonitorPublishedReviewIDs(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, number int64) (map[string]bool, error) {
	ids := map[string]bool{}
	if r.Store == nil {
		return ids, nil
	}
	for cursor := ""; ; {
		page, next, err := r.Store.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{
			Namespace: monitor.Namespace, ItemKind: repositoryMonitorPullRequestKind, ItemNumber: number, Limit: 100, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, publish := range page {
			// A publication that failed after GitHub accepted it still
			// records the review ID it created.
			if id := strings.TrimSpace(publish.GitHubReviewID); id != "" {
				ids[id] = true
			}
		}
		if next == "" {
			return ids, nil
		}
		cursor = next
	}
}

// repositoryMonitorFeedbackPermissions looks up and caches the repository
// permission of a bounded number of authors.
type repositoryMonitorFeedbackPermissions struct {
	lookup  func(login string) (string, error)
	known   map[string]string
	failed  map[string]bool
	lookups int
}

// permission returns the author's permission and the authority reason that
// applies when it falls short. A failed or skipped lookup is unknown, which
// never authorizes.
func (p *repositoryMonitorFeedbackPermissions) permission(login string) (string, string) {
	key := strings.ToLower(login)
	if permission, ok := p.known[key]; ok {
		return permission, repositoryMonitorFeedbackAuthorityBelowMinimum
	}
	if p.failed[key] {
		return repositoryMonitorFeedbackPermissionUnknown, repositoryMonitorFeedbackAuthorityLookupFailed
	}
	if p.lookups >= repositoryMonitorFeedbackMaxPermissionLookups {
		return repositoryMonitorFeedbackPermissionUnknown, repositoryMonitorFeedbackAuthorityLookupLimit
	}
	p.lookups++
	permission, err := p.lookup(login)
	if err != nil {
		p.failed[key] = true
		return repositoryMonitorFeedbackPermissionUnknown, repositoryMonitorFeedbackAuthorityLookupFailed
	}
	permission = repositoryMonitorEvidenceToken(permission)
	if permission == "" {
		permission = repositoryMonitorFeedbackPermissionNone
	}
	p.known[key] = permission
	return permission, repositoryMonitorFeedbackAuthorityBelowMinimum
}

// repositoryMonitorFeedbackLoginPattern matches a GitHub user or bot login.
var repositoryMonitorFeedbackLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:\[bot\])?$`)

// repositoryMonitorFeedbackAuthor is one normalized comment or review author.
type repositoryMonitorFeedbackAuthor struct {
	login, kind, permission string
	authority               repositoryMonitorFeedbackAuthority
	// evidence is false for a bot that is not allowlisted, whose text is
	// never evidence.
	evidence bool
}

// repositoryMonitorFeedbackNormalizer holds what normalizing one snapshot
// needs to classify authors.
type repositoryMonitorFeedbackNormalizer struct {
	policy      repositoryMonitorReviewFeedbackPolicy
	prAuthor    string
	permissions *repositoryMonitorFeedbackPermissions
	unverified  map[string]string
}

// noteUnverified records an entry whose author's authority is unknown
// because their permission lookup failed or was not made.
func (n *repositoryMonitorFeedbackNormalizer) noteUnverified(id string, author repositoryMonitorFeedbackAuthor) {
	switch author.authority.reason {
	case repositoryMonitorFeedbackAuthorityLookupFailed, repositoryMonitorFeedbackAuthorityLookupLimit:
		n.unverified[id] = author.authority.reason
	}
}

func (n *repositoryMonitorFeedbackNormalizer) author(actor *repositoryMonitorFeedbackActor) repositoryMonitorFeedbackAuthor {
	if actor == nil || !repositoryMonitorFeedbackLoginPattern.MatchString(strings.TrimSpace(actor.Login)) {
		// GitHub reports a deleted account as a null author.
		return repositoryMonitorFeedbackAuthor{login: "ghost", kind: repositoryMonitorFeedbackAuthorDeleted, permission: repositoryMonitorFeedbackPermissionUnknown,
			authority: repositoryMonitorFeedbackAuthority{reason: repositoryMonitorFeedbackAuthorityAuthorUnavailable}, evidence: true}
	}
	login := strings.TrimSpace(actor.Login)
	prAuthor := n.isPRAuthor(login)
	if repositoryMonitorFeedbackIsBot(actor) {
		// The collaborator API cannot describe a GitHub App bot; the
		// allowlist alone authorizes one.
		if !n.policy.botAllowlisted(login) {
			return repositoryMonitorFeedbackAuthor{login: login, kind: repositoryMonitorFeedbackAuthorBot}
		}
		author := repositoryMonitorFeedbackAuthor{login: login, kind: repositoryMonitorFeedbackAuthorBot, permission: repositoryMonitorFeedbackPermissionBot, evidence: true,
			authority: repositoryMonitorFeedbackAuthority{canStart: true, reason: repositoryMonitorFeedbackAuthorityBotAllowlisted}}
		if prAuthor {
			author.authority = repositoryMonitorFeedbackAuthority{reason: repositoryMonitorFeedbackAuthorityPRAuthor}
		}
		return author
	}
	author := repositoryMonitorFeedbackAuthor{login: login, kind: repositoryMonitorFeedbackAuthorUser, evidence: true}
	permission, shortfall := n.permissions.permission(login)
	author.permission = permission
	switch {
	case prAuthor:
		author.authority.reason = repositoryMonitorFeedbackAuthorityPRAuthor
	case repositoryMonitorPermissionMeets(n.policy.MinPermission, n.policy.AllowedPermissions, permission):
		author.authority = repositoryMonitorFeedbackAuthority{canStart: true, reason: repositoryMonitorFeedbackAuthorityAuthorized}
	default:
		author.authority.reason = shortfall
	}
	return author
}

// isPRAuthor reports whether login is the pull request author, whose own
// comments never start a repair of their pull request, so a repair cannot
// loop on itself. Bots are matched with and without the "[bot]" suffix REST
// uses.
func (n *repositoryMonitorFeedbackNormalizer) isPRAuthor(login string) bool {
	return n.prAuthor != "" && (strings.EqualFold(login, n.prAuthor) || repositoryMonitorFeedbackBotKey(login) == repositoryMonitorFeedbackBotKey(n.prAuthor))
}

func repositoryMonitorFeedbackIsBot(actor *repositoryMonitorFeedbackActor) bool {
	return strings.EqualFold(actor.TypeName, "Bot") || strings.HasSuffix(strings.ToLower(strings.TrimSpace(actor.Login)), "[bot]")
}

// lookUpStartCandidates spends the bounded permission lookups on the human
// authors whose feedback could start a repair before any other author, so
// evidence-only commenters cannot use them up. Candidates are the authors of
// comments on unresolved, current threads that Orka's findings did not open,
// and, when changes-requested reviews may start repairs, reviewers whose
// current changes-requested review has a body. They are looked up newest
// activity first, then by login; any lookups left go to the other authors
// in the order normalization reaches them.
func (n *repositoryMonitorFeedbackNormalizer) lookUpStartCandidates(threads []repositoryMonitorFeedbackThreadNode, reviews []repositoryMonitorFeedbackReviewNode, orkaReviews map[string]bool) {
	latest := map[string]time.Time{}
	logins := map[string]string{}
	see := func(actor *repositoryMonitorFeedbackActor, body string, at time.Time) {
		if actor == nil || !repositoryMonitorFeedbackLoginPattern.MatchString(strings.TrimSpace(actor.Login)) || repositoryMonitorFeedbackIsBot(actor) {
			return
		}
		login := strings.TrimSpace(actor.Login)
		if n.isPRAuthor(login) {
			return
		}
		if text, _, _ := repositoryMonitorFeedbackBody(body); strings.TrimSpace(text) == "" {
			return
		}
		key := strings.ToLower(login)
		if seen, ok := latest[key]; !ok || at.After(seen) {
			latest[key], logins[key] = at, login
		}
	}
	for _, node := range threads {
		if node.IsResolved || node.IsOutdated || repositoryMonitorFeedbackNodeID(node.ID) == "" ||
			len(node.Root.Nodes) > 0 && repositoryMonitorFeedbackOrkaComment(node.Root.Nodes[0], orkaReviews) {
			continue
		}
		for _, comment := range repositoryMonitorFeedbackThreadComments(node) {
			if comment.IsMinimized || repositoryMonitorFeedbackPendingComment(comment) || repositoryMonitorFeedbackOrkaComment(comment, orkaReviews) || repositoryMonitorFeedbackID(repositoryMonitorFeedbackKindComment, comment.FullDatabaseID) == "" {
				continue
			}
			see(comment.Author, comment.Body, repositoryMonitorFeedbackActivity(comment.CreatedAt, comment.LastEditedAt))
		}
	}
	if n.policy.ChangesRequestedReviews {
		for _, review := range reviews {
			if review.IsMinimized || review.SubmittedAt == nil || orkaReviews[strings.TrimSpace(review.FullDatabaseID)] || repositoryMonitorFeedbackID(repositoryMonitorFeedbackKindReview, review.FullDatabaseID) == "" {
				continue
			}
			see(review.Author, review.Body, repositoryMonitorFeedbackActivity(review.SubmittedAt.UTC(), review.LastEditedAt))
		}
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := latest[b].Compare(latest[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	for _, key := range keys {
		if n.permissions.lookups >= repositoryMonitorFeedbackMaxPermissionLookups {
			return
		}
		n.permissions.permission(logins[key])
	}
}

// repositoryMonitorFeedbackThreadComments returns a thread's root comment
// and its newest comments, without duplicates, oldest first. Comments in
// between are counted, not read.
func repositoryMonitorFeedbackThreadComments(node repositoryMonitorFeedbackThreadNode) []repositoryMonitorFeedbackCommentNode {
	comments := slices.Clone(node.Root.Nodes)
	for _, comment := range node.Recent.Nodes {
		if !slices.ContainsFunc(comments, func(c repositoryMonitorFeedbackCommentNode) bool { return c.FullDatabaseID == comment.FullDatabaseID }) {
			comments = append(comments, comment)
		}
	}
	return comments
}

// repositoryMonitorFeedbackBody removes hidden HTML comments, then sanitizes
// and bounds untrusted reviewer text with the CI evidence sanitizer.
func repositoryMonitorFeedbackBody(raw string) (body string, hidden bool, truncated []string) {
	stripped := repositoryMonitorFeedbackStripHTMLComments(raw)
	hidden = stripped != raw
	body, cut := repositoryMonitorEvidenceText(stripped, repositoryMonitorFeedbackBodyBytes, repositoryMonitorFeedbackBodyLines)
	if cut {
		truncated = []string{repositoryMonitorFeedbackTruncatedBody}
	}
	return body, hidden, truncated
}

// repositoryMonitorFeedbackStripHTMLComments removes every HTML comment, so
// no text a reviewer could not see reaches the repairer. It deliberately
// does not parse Markdown: code spans, fences, escapes and HTML blocks
// decide what GitHub hides in ways a partial parser gets wrong, so comment
// syntax is removed even where GitHub would show it as code, and an
// unclosed "<!--" removes the rest of the text. Losing such visible text is
// the safe failure; the snapshot reports that content was removed.
func repositoryMonitorFeedbackStripHTMLComments(text string) string {
	var out strings.Builder
	for {
		start := strings.Index(text, "<!--")
		if start < 0 {
			out.WriteString(text)
			return out.String()
		}
		out.WriteString(text[:start])
		end := strings.Index(text[start+4:], "-->")
		if end < 0 {
			return out.String()
		}
		text = text[start+4+end+3:]
	}
}

// repositoryMonitorFeedbackID builds a stable entry ID from a GitHub
// database ID, or "" for a missing or malformed one.
func repositoryMonitorFeedbackID(kind, databaseID string) string {
	id, err := strconv.ParseUint(strings.TrimSpace(databaseID), 10, 64)
	if err != nil || id == 0 {
		return ""
	}
	return kind + ":" + strconv.FormatUint(id, 10)
}

// repositoryMonitorFeedbackNodeID keeps a GraphQL node ID, which is opaque
// base64-like text, or "" for anything else.
func repositoryMonitorFeedbackNodeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 || strings.ContainsFunc(id, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '=' && r != '+' && r != '/'
	}) {
		return ""
	}
	return id
}

// repositoryMonitorFeedbackPendingEntry pairs a normalized entry with what
// the controller needs about it.
type repositoryMonitorFeedbackPendingThread struct {
	thread     repositoryMonitorFeedbackThread
	authorized []repositoryMonitorFeedbackAuthorized
}

type repositoryMonitorFeedbackPendingReview struct {
	review     repositoryMonitorFeedbackReview
	authorized *repositoryMonitorFeedbackAuthorized
}

func repositoryMonitorFeedbackPendingThreadStarts(pending repositoryMonitorFeedbackPendingThread) bool {
	return slices.ContainsFunc(pending.authorized, func(entry repositoryMonitorFeedbackAuthorized) bool { return entry.Current })
}

// normalizeRepositoryMonitorReviewFeedback turns raw GitHub feedback into a
// bounded snapshot of base's head. Resolved threads, minimized and empty
// comments, Orka's own comments, and bots outside the allowlist are left
// out. Entries that can start a repair are placed first, so a snapshot cut
// to its budget keeps them.
func normalizeRepositoryMonitorReviewFeedback(snapshot repositoryMonitorReviewFeedbackSnapshot, pr repositoryMonitorPullRequest, raw repositoryMonitorRawReviewFeedback, orkaReviews map[string]bool, policy repositoryMonitorReviewFeedbackPolicy, permissionOf func(string) (string, error)) repositoryMonitorReviewFeedbackSnapshot {
	snapshot.Availability, snapshot.Reason, snapshot.Partial = raw.availability, raw.reason, raw.partial
	if snapshot.Availability == "" {
		snapshot.Availability = repositoryMonitorEvidenceAvailable
	}
	if raw.headSHA != "" && raw.headSHA != pr.HeadSHA {
		// Feedback read for another head is not evidence about this one.
		snapshot.Availability, snapshot.Reason = repositoryMonitorEvidenceStale, repositoryMonitorFeedbackReasonHeadMoved
	}
	if snapshot.Availability == repositoryMonitorEvidenceStale {
		return snapshot
	}
	snapshot.ReviewDecision = repositoryMonitorEvidenceToken(raw.reviewDecision)
	normalizer := &repositoryMonitorFeedbackNormalizer{
		policy:      policy,
		prAuthor:    strings.TrimSpace(raw.prAuthor),
		permissions: &repositoryMonitorFeedbackPermissions{lookup: permissionOf, known: map[string]string{}, failed: map[string]bool{}},
		unverified:  map[string]string{},
	}
	currentReviews := repositoryMonitorCurrentChangesRequestedReviews(raw.reviews)
	normalizer.lookUpStartCandidates(raw.threads, currentReviews, orkaReviews)
	var threads []repositoryMonitorFeedbackPendingThread
	for _, node := range raw.threads {
		pending, ok, hides := normalizer.thread(node, orkaReviews)
		switch {
		case ok:
			threads = append(threads, pending)
		case hides:
			// A thread left out before placement can still hide feedback
			// that could start a repair.
			snapshot.omittedWithAuthority++
		}
	}
	var reviews []repositoryMonitorFeedbackPendingReview
	for _, node := range currentReviews {
		if pending, ok := normalizer.review(node, pr.HeadSHA, orkaReviews); ok {
			reviews = append(reviews, pending)
		}
	}
	// Place entries by what they mean to automation, then by GitHub's order,
	// so a snapshot cut to its budget keeps feedback that can start a repair,
	// then feedback a person must settle, before evidence-only feedback. A
	// review is a reviewer's overall verdict, so it goes before threads of
	// the same rank.
	type placement struct {
		rank   int
		review *repositoryMonitorFeedbackPendingReview
		thread *repositoryMonitorFeedbackPendingThread
	}
	placements := make([]placement, 0, len(reviews)+len(threads))
	for i := range reviews {
		rank := 4
		if entry := reviews[i].authorized; entry != nil {
			rank = 2
			if entry.Current {
				rank = 0
			}
		}
		placements = append(placements, placement{rank: rank, review: &reviews[i]})
	}
	for i := range threads {
		rank := 5
		switch {
		case repositoryMonitorFeedbackPendingThreadStarts(threads[i]):
			rank = 1
		case len(threads[i].authorized) > 0:
			rank = 3
		}
		placements = append(placements, placement{rank: rank, thread: &threads[i]})
	}
	slices.SortStableFunc(placements, func(a, b placement) int { return a.rank - b.rank })
	used := repositoryMonitorFeedbackSnapshotBaseSize(snapshot, len(threads), len(reviews))
	fits := func(entry any) bool {
		size := repositoryMonitorEvidenceEncodedSize(entry) + repositoryMonitorCIEvidenceEntrySeparatorBytes
		if used+size > repositoryMonitorFeedbackTotalEncodedBytes {
			return false
		}
		used += size
		return true
	}
	for _, place := range placements {
		switch {
		case place.review != nil && !fits(place.review.review):
			snapshot.OmittedReviews++
			if place.review.authorized != nil {
				snapshot.omittedWithAuthority++
			}
		case place.review != nil:
			snapshot.Reviews = append(snapshot.Reviews, place.review.review)
			if place.review.authorized != nil {
				snapshot.authorized = append(snapshot.authorized, *place.review.authorized)
			}
		case !fits(place.thread.thread):
			snapshot.OmittedThreads++
			if thread := place.thread; len(thread.authorized) > 0 || thread.thread.OmittedComments > 0 && !thread.thread.OrkaFindingThread {
				snapshot.omittedWithAuthority++
			}
		default:
			snapshot.Threads = append(snapshot.Threads, place.thread.thread)
			snapshot.authorized = append(snapshot.authorized, place.thread.authorized...)
		}
	}
	if (snapshot.OmittedThreads > 0 || snapshot.OmittedReviews > 0) && snapshot.Availability == repositoryMonitorEvidenceAvailable {
		snapshot.Availability, snapshot.Reason = repositoryMonitorEvidenceTruncated, repositoryMonitorEvidenceReasonEvidenceBudget
	}
	snapshot.unverified = normalizer.unverified
	return snapshot
}

// authorityLookupFailed reports whether an included entry's author's
// permission lookup failed, so the entry's authority is unknown for now.
func (s repositoryMonitorReviewFeedbackSnapshot) authorityLookupFailed(id string) bool {
	return s.unverified[id] == repositoryMonitorFeedbackAuthorityLookupFailed
}

// unverifiedFeedback counts the included entries whose author's permission
// lookup failed, and those past the lookup limit.
func (s repositoryMonitorReviewFeedbackSnapshot) unverifiedFeedback() (failed, limited int) {
	for _, reason := range s.unverified {
		if reason == repositoryMonitorFeedbackAuthorityLookupFailed {
			failed++
		} else {
			limited++
		}
	}
	return failed, limited
}

// omittedFeedbackWithAuthority counts the threads and reviews the snapshot
// left out to stay within its budget that hold, or may hide, feedback that
// could start a repair. Feedback that is left out cannot be addressed or
// checked, so a person must look at it.
func (s repositoryMonitorReviewFeedbackSnapshot) omittedFeedbackWithAuthority() int {
	return s.omittedWithAuthority
}

// repositoryMonitorFeedbackSnapshotPrefix is the prefix of the lines of a
// snapshot's encoding in the repair prompt, whose payload encoder puts it one
// level deep. Its threads and reviews are three levels deep, as CI evidence
// entries are, so repositoryMonitorEvidenceEncodedSize measures them.
const repositoryMonitorFeedbackSnapshotPrefix = "  "

// repositoryMonitorFeedbackSnapshotEncodedSize is the number of bytes a
// snapshot takes in the repair prompt.
func repositoryMonitorFeedbackSnapshotEncodedSize(snapshot repositoryMonitorReviewFeedbackSnapshot) int {
	encoded, err := json.MarshalIndent(snapshot, repositoryMonitorFeedbackSnapshotPrefix, repositoryMonitorCIEvidenceEntryIndent)
	if err != nil {
		return repositoryMonitorFeedbackTotalEncodedBytes + 1
	}
	return len(encoded)
}

// repositoryMonitorFeedbackSnapshotBaseSize is the prompt size of snapshot
// before any thread or review is placed, as if the budget runs out: every
// thread and review omitted and the snapshot marked truncated. Each placed
// entry then adds its encoded size and one separator; the first entry of a
// list also replaces null with brackets, two bytes more, reserved here.
func repositoryMonitorFeedbackSnapshotBaseSize(snapshot repositoryMonitorReviewFeedbackSnapshot, threads, reviews int) int {
	worst := snapshot
	worst.Threads, worst.Reviews = nil, nil
	worst.OmittedThreads += threads
	worst.OmittedReviews += reviews
	if worst.Availability == repositoryMonitorEvidenceAvailable {
		worst.Availability, worst.Reason = repositoryMonitorEvidenceTruncated, repositoryMonitorEvidenceReasonEvidenceBudget
	}
	return repositoryMonitorFeedbackSnapshotEncodedSize(worst) + 2*len("[]")
}

// thread normalizes one review thread, or reports false when it has nothing
// to include. For a thread it leaves out, hides reports whether the thread
// may still hold feedback that could start a repair: an unresolved thread
// that Orka's findings did not open with replies that were not read, or one
// that had authorized feedback but could not fit its budget.
func (n *repositoryMonitorFeedbackNormalizer) thread(node repositoryMonitorFeedbackThreadNode, orkaReviews map[string]bool) (pending repositoryMonitorFeedbackPendingThread, ok, hides bool) {
	id := repositoryMonitorFeedbackNodeID(node.ID)
	if node.IsResolved || id == "" {
		return repositoryMonitorFeedbackPendingThread{}, false, false
	}
	thread := repositoryMonitorFeedbackThread{ID: repositoryMonitorFeedbackKindThread + ":" + id, Outdated: node.IsOutdated, Side: repositoryMonitorEvidenceToken(node.DiffSide)}
	thread.Path, _ = repositoryMonitorEvidenceLine(node.Path, repositoryMonitorCIEvidencePathBytes)
	if node.OriginalLine != nil {
		thread.OriginalLine = max(*node.OriginalLine, 0)
	}
	// An outdated thread's comments describe code this head changed, so it
	// is never shown at a current line.
	if node.Line != nil && !node.IsOutdated {
		thread.Line = max(*node.Line, 0)
	}
	comments := repositoryMonitorFeedbackThreadComments(node)
	thread.OmittedComments = max(node.Root.TotalCount-len(comments), 0)
	if len(node.Root.Nodes) > 0 && repositoryMonitorFeedbackOrkaComment(node.Root.Nodes[0], orkaReviews) {
		thread.OrkaFindingThread = true
	}
	var authorized []repositoryMonitorFeedbackAuthorized
	for _, raw := range comments {
		if raw.IsMinimized || repositoryMonitorFeedbackPendingComment(raw) || repositoryMonitorFeedbackOrkaComment(raw, orkaReviews) {
			continue
		}
		commentID := repositoryMonitorFeedbackID(repositoryMonitorFeedbackKindComment, raw.FullDatabaseID)
		author := n.author(raw.Author)
		// A bot comment is evidence only when the bot is allowlisted and
		// the comment sits on a current line of the diff.
		if commentID == "" || !author.evidence || author.kind == repositoryMonitorFeedbackAuthorBot && (thread.Path == "" || thread.Line == 0) {
			continue
		}
		body, hidden, truncated := repositoryMonitorFeedbackBody(raw.Body)
		if strings.TrimSpace(body) == "" {
			continue
		}
		// A reply on an Orka finding thread never starts a repair, whatever
		// its author's permission, so its unknown authority holds nothing.
		if !thread.OrkaFindingThread {
			n.noteUnverified(commentID, author)
		}
		comment := repositoryMonitorFeedbackComment{
			ID: commentID, Author: author.login, AuthorType: author.kind, Permission: author.permission,
			Body: body, URL: repositoryMonitorEvidenceURL(raw.URL), CreatedAt: raw.CreatedAt.UTC(),
			HiddenContentRemoved: hidden, Truncated: truncated,
		}
		if raw.LastEditedAt != nil {
			edited := raw.LastEditedAt.UTC()
			comment.LastEditedAt = &edited
		}
		// Orka's findings drive their own repairs; a reply to one is context
		// for that finding, not a request of its own.
		if author.authority.canStart && !thread.OrkaFindingThread {
			entry := repositoryMonitorFeedbackAuthorized{ID: commentID, Author: author.login, ActivityAt: repositoryMonitorFeedbackActivity(comment.CreatedAt, comment.LastEditedAt), Current: true}
			if thread.Outdated {
				entry.Current, entry.Reason = false, repositoryMonitorFeedbackAuthorityOutdated
			}
			authorized = append(authorized, entry)
		}
		thread.Comments = append(thread.Comments, comment)
	}
	unread := thread.OmittedComments > 0 && !thread.OrkaFindingThread
	if len(thread.Comments) == 0 {
		return repositoryMonitorFeedbackPendingThread{}, false, unread
	}
	authorized, fits := fitRepositoryMonitorFeedbackThread(&thread, authorized)
	if !fits {
		return repositoryMonitorFeedbackPendingThread{}, false, unread || len(authorized) > 0
	}
	return repositoryMonitorFeedbackPendingThread{thread: thread, authorized: authorized}, true, false
}

// fitRepositoryMonitorFeedbackThread keeps thread within its own budget by
// dropping the oldest replies, never the root or the newest comment, then by
// shortening the longest remaining body. Bodies are already sanitized, so a
// cut only removes text. It returns authorized without the dropped replies,
// and false when only metadata is left to cut.
func fitRepositoryMonitorFeedbackThread(thread *repositoryMonitorFeedbackThread, authorized []repositoryMonitorFeedbackAuthorized) ([]repositoryMonitorFeedbackAuthorized, bool) {
	for len(thread.Comments) > 2 && repositoryMonitorEvidenceEncodedSize(thread) > repositoryMonitorFeedbackThreadEncodedBytes {
		dropped := thread.Comments[1]
		thread.Comments = slices.Delete(thread.Comments, 1, 2)
		thread.OmittedComments++
		authorized = slices.DeleteFunc(authorized, func(entry repositoryMonitorFeedbackAuthorized) bool { return entry.ID == dropped.ID })
	}
	for repositoryMonitorEvidenceEncodedSize(thread) > repositoryMonitorFeedbackThreadEncodedBytes {
		longest := 0
		for i := range thread.Comments {
			if len(thread.Comments[i].Body) > len(thread.Comments[longest].Body) {
				longest = i
			}
		}
		comment := &thread.Comments[longest]
		if len(comment.Body) <= repositoryMonitorFeedbackMinCutBody {
			// Only metadata is left to cut; such a thread is malformed.
			return authorized, false
		}
		comment.Body = repositoryMonitorEvidenceCutRunes(comment.Body, len(comment.Body)/2)
		if !slices.Contains(comment.Truncated, repositoryMonitorFeedbackTruncatedBody) {
			comment.Truncated = append(comment.Truncated, repositoryMonitorFeedbackTruncatedBody)
		}
	}
	return authorized, true
}

// review normalizes one current changes-requested review, or reports false
// when it is not evidence.
func (n *repositoryMonitorFeedbackNormalizer) review(node repositoryMonitorFeedbackReviewNode, headSHA string, orkaReviews map[string]bool) (repositoryMonitorFeedbackPendingReview, bool) {
	id := repositoryMonitorFeedbackID(repositoryMonitorFeedbackKindReview, node.FullDatabaseID)
	if id == "" || node.IsMinimized || orkaReviews[strings.TrimSpace(node.FullDatabaseID)] || node.SubmittedAt == nil {
		return repositoryMonitorFeedbackPendingReview{}, false
	}
	author := n.author(node.Author)
	// A review has no path or line, so no bot review is evidence.
	if !author.evidence || author.kind == repositoryMonitorFeedbackAuthorBot {
		return repositoryMonitorFeedbackPendingReview{}, false
	}
	n.noteUnverified(id, author)
	body, hidden, truncated := repositoryMonitorFeedbackBody(node.Body)
	review := repositoryMonitorFeedbackReview{
		ID: id, Author: author.login, AuthorType: author.kind, Permission: author.permission,
		Body: body, URL: repositoryMonitorEvidenceURL(node.URL), SubmittedAt: node.SubmittedAt.UTC(),
		HiddenContentRemoved: hidden, Truncated: truncated,
	}
	if node.Commit != nil {
		review.CommitSHA = repositoryMonitorEvidenceToken(node.Commit.OID)
		review.EarlierHead = review.CommitSHA != "" && !strings.EqualFold(review.CommitSHA, headSHA)
	}
	if node.LastEditedAt != nil {
		edited := node.LastEditedAt.UTC()
		review.LastEditedAt = &edited
	}
	pending := repositoryMonitorFeedbackPendingReview{review: review}
	if !author.authority.canStart {
		return pending, true
	}
	entry := repositoryMonitorFeedbackAuthorized{ID: id, Author: author.login, ActivityAt: repositoryMonitorFeedbackActivity(review.SubmittedAt, review.LastEditedAt), Current: true}
	switch {
	case !n.policy.ChangesRequestedReviews:
		entry.Current, entry.Reason = false, repositoryMonitorFeedbackAuthorityReviewsDisabled
	case strings.TrimSpace(body) == "":
		// The review's inline comments are threads of their own; an empty
		// review body gives a repair nothing more to act on.
		entry.Current, entry.Reason = false, repositoryMonitorFeedbackAuthorityEmptyBody
	}
	pending.authorized = &entry
	return pending, true
}

func repositoryMonitorFeedbackActivity(created time.Time, edited *time.Time) time.Time {
	if edited != nil && edited.After(created) {
		return *edited
	}
	return created
}

// repositoryMonitorFeedbackOrkaComment reports whether comment belongs to a
// review Orka published.
func repositoryMonitorFeedbackOrkaComment(comment repositoryMonitorFeedbackCommentNode, orkaReviews map[string]bool) bool {
	return comment.PullRequestReview != nil && orkaReviews[strings.TrimSpace(comment.PullRequestReview.FullDatabaseID)]
}

// repositoryMonitorCurrentChangesRequestedReviews returns, per reviewer,
// the changes-requested review that is still their latest verdict. A later
// approval, or the dismissal of that review, withdraws it; comment-only
// reviews do not.
func repositoryMonitorCurrentChangesRequestedReviews(reviews []repositoryMonitorFeedbackReviewNode) []repositoryMonitorFeedbackReviewNode {
	latest := map[string]int{}
	var order []string
	for i, review := range reviews {
		switch strings.ToUpper(strings.TrimSpace(review.State)) {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		default:
			continue
		}
		key := ""
		if review.Author != nil {
			key = strings.ToLower(strings.TrimSpace(review.Author.Login))
		}
		if key == "" {
			// Reviews of deleted accounts cannot be told apart, so each
			// stands alone.
			key = "\x00" + strconv.Itoa(i)
		}
		previous, seen := latest[key]
		if !seen {
			order = append(order, key)
		}
		if !seen || !repositoryMonitorFeedbackReviewBefore(review, reviews[previous]) {
			latest[key] = i
		}
	}
	var current []repositoryMonitorFeedbackReviewNode
	for _, key := range order {
		if review := reviews[latest[key]]; strings.EqualFold(strings.TrimSpace(review.State), "CHANGES_REQUESTED") {
			current = append(current, review)
		}
	}
	return current
}

// repositoryMonitorFeedbackReviewBefore reports whether a was submitted
// before b. GitHub lists reviews oldest first, so equal or missing times
// keep list order.
func repositoryMonitorFeedbackReviewBefore(a, b repositoryMonitorFeedbackReviewNode) bool {
	if a.SubmittedAt == nil || b.SubmittedAt == nil {
		return false
	}
	return a.SubmittedAt.Before(*b.SubmittedAt)
}
