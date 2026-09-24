package publisher

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// These character limits match WorkspaceConfig's admission bounds and leave
// room in the forge body for publication metadata and reconciliation markers.
const (
	MaxPullRequestTitleLength = 256
	MaxPullRequestBodyLength  = 32768
	// PullRequestMarkerPrefix is reserved for publisher-owned reconciliation metadata.
	PullRequestMarkerPrefix = "<!-- orka.publisher.pr-"
)

// DefaultPullRequestTitle derives a bounded title from trusted Task input.
func DefaultPullRequestTitle(prompt string, generation int64) string {
	line, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "Orka publication generation " + strconv.FormatInt(generation, 10)
	}
	if utf8.RuneCountInString(line) > MaxPullRequestTitleLength {
		line = strings.TrimSpace(string([]rune(line)[:MaxPullRequestTitleLength]))
	}
	return line
}

// ValidatePullRequestText enforces the Task admission bounds at runtime too.
func ValidatePullRequestText(title, body string) error {
	if utf8.RuneCountInString(title) > MaxPullRequestTitleLength {
		return invalid("prTitle", "must not exceed 256 characters")
	}
	if utf8.RuneCountInString(body) > MaxPullRequestBodyLength {
		return invalid("prBody", "must not exceed 32768 characters")
	}
	if strings.Contains(body, PullRequestMarkerPrefix) {
		return invalid("prBody", "must not contain reserved publisher reconciliation markers")
	}
	return nil
}

// Description renders the Task-authored body or the default publication body.
// The forge adapter appends its reconciliation markers after this text.
func (i PullRequestIntent) Description() string {
	body := i.Body
	if body == "" {
		body = "Created by the Orka clean-room workspace publisher."
		if i.TaskName != "" {
			body += "\n\nTask: `" + i.TaskNamespace + "/" + i.TaskName + "`"
		}
	}
	return body + "\n\nPublication generation: " + strconv.FormatInt(i.PublicationGeneration, 10)
}

type pullRequestRepositoryIdentity struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// Key returns the exact immutable tuple identity used for PR reconciliation.
func (i PullRequestIntent) Key() (string, error) {
	if err := validatePullRequestIntent(i); err != nil {
		return "", err
	}
	return digestCanonical(struct {
		Domain                string                        `json:"domain"`
		BaseRepository        pullRequestRepositoryIdentity `json:"baseRepository"`
		BaseRef               string                        `json:"baseRef"`
		HeadRepository        pullRequestRepositoryIdentity `json:"headRepository"`
		HeadRef               string                        `json:"headRef"`
		PublicationGeneration int64                         `json:"publicationGeneration"`
		ExpectedHeadOID       string                        `json:"expectedHeadOid"`
		SessionUID            string                        `json:"sessionUid,omitempty"`
	}{
		Domain:         "orka.publisher.pr-intent.v1",
		BaseRepository: pullRequestRepositoryIdentity{Provider: i.BaseRepository.Provider, ID: i.BaseRepository.ID}, BaseRef: i.BaseRef,
		HeadRepository: pullRequestRepositoryIdentity{Provider: i.HeadRepository.Provider, ID: i.HeadRepository.ID}, HeadRef: i.HeadRef,
		PublicationGeneration: i.PublicationGeneration, ExpectedHeadOID: i.ExpectedHeadOID, SessionUID: i.SessionUID,
	})
}

// SessionKey identifies one Session's PR across verified publications. The
// per-publication Key and receipt still bind the current exact head commit.
func (i PullRequestIntent) SessionKey() (string, error) {
	if err := validatePullRequestIntent(i); err != nil {
		return "", err
	}
	if i.SessionUID == "" {
		return "", nil
	}
	return digestCanonical(struct {
		Domain         string                        `json:"domain"`
		BaseRepository pullRequestRepositoryIdentity `json:"baseRepository"`
		BaseRef        string                        `json:"baseRef"`
		HeadRepository pullRequestRepositoryIdentity `json:"headRepository"`
		HeadRef        string                        `json:"headRef"`
		SessionUID     string                        `json:"sessionUid"`
	}{
		Domain:         "orka.publisher.pr-session.v1",
		BaseRepository: pullRequestRepositoryIdentity{Provider: i.BaseRepository.Provider, ID: i.BaseRepository.ID}, BaseRef: i.BaseRef,
		HeadRepository: pullRequestRepositoryIdentity{Provider: i.HeadRepository.Provider, ID: i.HeadRepository.ID}, HeadRef: i.HeadRef,
		SessionUID: i.SessionUID,
	})
}

func validatePullRequestIntent(intent PullRequestIntent) error {
	if err := validateRepository(intent.BaseRepository); err != nil {
		return err
	}
	if err := validateRepository(intent.HeadRepository); err != nil {
		return err
	}
	if err := validateBranchRef(intent.BaseRef); err != nil {
		return err
	}
	if err := validateBranchRef(intent.HeadRef); err != nil {
		return err
	}
	if intent.PublicationGeneration < 1 {
		return invalid("PR publication generation", "must be at least 1")
	}
	if len(intent.SessionUID) > 512 || strings.TrimSpace(intent.SessionUID) != intent.SessionUID {
		return invalid("PR session UID", "must be a bounded immutable identifier")
	}
	if err := ValidatePullRequestText(intent.Title, intent.Body); err != nil {
		return err
	}
	if len(intent.TaskName) > 253 || len(intent.TaskNamespace) > 63 ||
		strings.ContainsAny(intent.TaskName+intent.TaskNamespace, "`\r\n") {
		return invalid("PR Task identity", "must be bounded and single-line")
	}
	return validateObjectID("PR expected head", intent.ExpectedHeadOID)
}

// ReconcilePullRequest validates the exact tuple before and after delegating to
// the SCM-specific reconciler. The caller must have durably persisted intent
// before invoking this method.
func ReconcilePullRequest(ctx context.Context, intent PullRequestIntent, reconciler PullRequestReconciler) (PullRequestReceipt, error) {
	if reconciler == nil {
		return PullRequestReceipt{}, invalid("PR reconciler", "must not be nil")
	}
	key, err := intent.Key()
	if err != nil {
		return PullRequestReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return PullRequestReceipt{}, err
	}
	receipt, err := reconciler.Reconcile(ctx, intent)
	if err != nil {
		return PullRequestReceipt{}, err
	}
	if receipt.IntentKey != key || receipt.ForgeID == "" || receipt.URL == "" || receipt.HeadOID != intent.ExpectedHeadOID {
		return PullRequestReceipt{}, operationError(ErrIdempotencyConflict, "validate PR receipt", "forge receipt does not match the exact persisted tuple", nil)
	}
	switch receipt.State {
	case PullRequestOpen, PullRequestClosed, PullRequestMerged:
		return receipt, nil
	default:
		return PullRequestReceipt{}, fmt.Errorf("validate PR receipt state %q: %w", receipt.State, ErrInvalidRequest)
	}
}
