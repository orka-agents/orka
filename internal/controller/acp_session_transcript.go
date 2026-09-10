package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

const (
	DefaultACPBootstrapMaxMessages     = 64
	DefaultACPBootstrapMaxBytes        = 128 * 1024
	DefaultACPBootstrapMaxMessageBytes = 32 * 1024

	acpBootstrapTruncationSuffix = "\n[truncated by Orka session bootstrap]"
	acpBootstrapRoleUser         = "user"
	acpBootstrapCheckpointName   = "orka_session_checkpoint"
)

// ACPBootstrapLimits bound the canonical transcript artifact used to recreate
// an ACP provider session after runtime loss, scale-to-zero, or profile rotation.
type ACPBootstrapLimits struct {
	MaxMessages     int
	MaxBytes        int
	MaxMessageBytes int
}

func (l ACPBootstrapLimits) withDefaults() (ACPBootstrapLimits, error) {
	if l.MaxMessages == 0 {
		l.MaxMessages = DefaultACPBootstrapMaxMessages
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = DefaultACPBootstrapMaxBytes
	}
	if l.MaxMessageBytes == 0 {
		l.MaxMessageBytes = DefaultACPBootstrapMaxMessageBytes
	}
	if l.MaxMessages < 1 || l.MaxMessages > 4096 {
		return ACPBootstrapLimits{}, store.ValidationErrorf("ACP bootstrap max messages must be in range 1..4096")
	}
	if l.MaxBytes < 1024 || l.MaxBytes > harnessv2.MaxCanonicalJSONBytes {
		return ACPBootstrapLimits{}, store.ValidationErrorf("ACP bootstrap max bytes must be in range 1024..%d", harnessv2.MaxCanonicalJSONBytes)
	}
	if l.MaxMessageBytes < 256 || l.MaxMessageBytes > l.MaxBytes {
		return ACPBootstrapLimits{}, store.ValidationErrorf("ACP bootstrap max message bytes must be in range 256..max bytes")
	}
	return l, nil
}

// ACPBootstrapTranscript is a deterministic JSONL checkpoint and suffix of the
// canonical Orka transcript. Structured tool arguments are intentionally omitted:
// continuity restores reference text without replaying provider-native or tool state.
type ACPBootstrapTranscript struct {
	SessionUID       string
	Messages         []ACPBootstrapMessage
	Artifact         []byte
	Digest           string
	MessageCount     uint32
	TotalMessages    int
	Truncated        bool
	VerifiedBaseline *store.VerifiedBranchBaseline
}

// ACPBootstrapMessage is the portable, provider-neutral transcript shape.
type ACPBootstrapMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	Name       string `json:"name,omitempty"`
	ToolCallID string `json:"toolCallID,omitempty"`
}

// BuildBootstrapTranscript returns a bounded, deterministic suffix of the Orka
// transcript. It refuses reconciliation-blocked Sessions and returns the last
// independently verified branch baseline for separate workspace recovery.
func (c *ACPSessionContinuity) BuildBootstrapTranscript(ctx context.Context, session store.SessionControl) (*ACPBootstrapTranscript, error) {
	return c.buildBootstrapTranscript(ctx, session, 0)
}

// BuildBootstrapTranscriptWithLimit returns the same canonical suffix while
// applying the Task-specific recent-history limit before the continuity-wide
// byte and message caps. An eligible checkpoint can summarize older messages;
// ThroughMessageID, rather than this size limit, bounds readable history.
func (c *ACPSessionContinuity) BuildBootstrapTranscriptWithLimit(
	ctx context.Context, session store.SessionControl, maxMessages int,
) (*ACPBootstrapTranscript, error) {
	if maxMessages < 1 {
		return nil, store.ValidationErrorf("ACP bootstrap message limit must be positive")
	}
	return c.buildBootstrapTranscript(ctx, session, maxMessages)
}

func (c *ACPSessionContinuity) buildBootstrapTranscript(
	ctx context.Context, session store.SessionControl, maxMessages int,
) (*ACPBootstrapTranscript, error) {
	current, err := c.controls.GetSessionControl(ctx, session.Namespace, session.SessionName)
	if err != nil {
		return nil, fmt.Errorf("load ACP session for bootstrap: %w", err)
	}
	if current.SessionUID != session.SessionUID {
		return nil, fmt.Errorf("%w: ACP bootstrap session UID changed from %q to %q", store.ErrConflict, session.SessionUID, current.SessionUID)
	}
	if current.Availability != store.SessionAvailable {
		return nil, fmt.Errorf("%w: ACP session %s/%s is reconciliation-blocked", store.ErrConflict, current.Namespace, current.SessionName)
	}
	messages, err := c.transcripts.LoadTranscript(ctx, current.Namespace, current.SessionName, maxMessages)
	if err != nil {
		return nil, fmt.Errorf("load canonical ACP transcript: %w", err)
	}
	checkpoint, err := c.loadBootstrapCheckpoint(ctx, current, messages)
	if err != nil {
		return nil, err
	}
	bootstrap, err := buildACPBootstrapTranscriptWithCheckpoint(messages, checkpoint, c.bootstrapLimits)
	if err != nil {
		return nil, err
	}
	bootstrap.SessionUID = current.SessionUID
	bootstrap.TotalMessages = len(messages)
	if current.VerifiedBaseline != nil {
		baseline := *current.VerifiedBaseline
		bootstrap.VerifiedBaseline = &baseline
	}
	return bootstrap, nil
}

// BuildBootstrapTranscriptThrough returns the bounded transcript ending at one
// exact stable message ID together with that untruncated terminal message.
// Gateway Tasks use the terminal user message as the durable SessionTurn prompt
// while avoiding a duplicate prompt block in provider input.
func (c *ACPSessionContinuity) BuildBootstrapTranscriptThrough(
	ctx context.Context, session store.SessionControl, throughMessageID string, maxMessages int, excludeThroughMessage bool,
) (*ACPBootstrapTranscript, *store.SessionMessage, error) {
	throughMessageID = strings.TrimSpace(throughMessageID)
	if throughMessageID == "" || maxMessages < 1 {
		return nil, nil, store.ValidationErrorf("ACP bounded bootstrap requires a through-message ID and positive message limit")
	}
	current, err := c.controls.GetSessionControl(ctx, session.Namespace, session.SessionName)
	if err != nil {
		return nil, nil, fmt.Errorf("load ACP session for bounded bootstrap: %w", err)
	}
	if current.SessionUID != session.SessionUID {
		return nil, nil, fmt.Errorf("%w: ACP bootstrap session UID changed from %q to %q", store.ErrConflict, session.SessionUID, current.SessionUID)
	}
	if current.Availability != store.SessionAvailable {
		return nil, nil, fmt.Errorf("%w: ACP session %s/%s is reconciliation-blocked", store.ErrConflict, current.Namespace, current.SessionName)
	}
	messages, err := c.transcripts.LoadTranscriptThrough(ctx, current.Namespace, current.SessionName, throughMessageID, maxMessages)
	if err != nil {
		return nil, nil, fmt.Errorf("load bounded canonical ACP transcript: %w", err)
	}
	if len(messages) == 0 || messages[len(messages)-1].ID != throughMessageID {
		return nil, nil, fmt.Errorf("%w: bounded ACP transcript does not end at message %q", store.ErrConflict, throughMessageID)
	}
	bootstrapMessages := messages
	if excludeThroughMessage {
		bootstrapMessages = messages[:len(messages)-1]
	}
	// The current prompt stays outside the checkpoint as well as the transcript
	// artifact. When the recent-history limit leaves no preceding message, omit
	// the optional checkpoint instead of guessing an earlier boundary.
	checkpoint, err := c.loadBootstrapCheckpoint(ctx, current, bootstrapMessages)
	if err != nil {
		return nil, nil, err
	}
	bootstrap, err := buildACPBootstrapTranscriptWithCheckpoint(bootstrapMessages, checkpoint, c.bootstrapLimits)
	if err != nil {
		return nil, nil, err
	}
	bootstrap.SessionUID = current.SessionUID
	bootstrap.TotalMessages = len(messages)
	if current.VerifiedBaseline != nil {
		baseline := *current.VerifiedBaseline
		bootstrap.VerifiedBaseline = &baseline
	}
	terminal := messages[len(messages)-1]
	return bootstrap, &terminal, nil
}

func (c *ACPSessionContinuity) loadBootstrapCheckpoint(
	ctx context.Context, session *store.SessionControl, messages []store.SessionMessage,
) (*store.SessionCheckpoint, error) {
	checkpoints, ok := c.transcripts.(store.SessionContextStore)
	if !ok || len(messages) == 0 {
		return nil, nil
	}
	terminal := messages[len(messages)-1]
	if terminal.ID == "" {
		return nil, nil
	}
	checkpoint, err := checkpoints.LoadSessionCheckpoint(ctx, session.Namespace, session.SessionName, terminal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load ACP session checkpoint: %w", err)
	}
	if checkpoint == nil || checkpoint.Namespace != session.Namespace || checkpoint.SessionName != session.SessionName {
		return nil, fmt.Errorf("%w: ACP bootstrap checkpoint Session identity does not match", store.ErrConflict)
	}
	if checkpoint.LastMessageOrder <= 0 || checkpoint.LastMessageOrder > terminal.Order {
		return nil, fmt.Errorf("%w: ACP bootstrap checkpoint exceeds the readable message boundary", store.ErrConflict)
	}
	return checkpoint, nil
}

// A checkpoint is one assistant reference message, never a system instruction
// or a replayed tool call. It must fit intact alongside recent history.
func buildACPBootstrapTranscriptWithCheckpoint(
	messages []store.SessionMessage, checkpoint *store.SessionCheckpoint, limits ACPBootstrapLimits,
) (*ACPBootstrapTranscript, error) {
	if checkpoint == nil {
		return buildACPBootstrapTranscript(messages, limits)
	}
	limits, err := limits.withDefaults()
	if err != nil {
		return nil, err
	}
	checkpointMessage, checkpointLine, err := encodeACPBootstrapCheckpoint(checkpoint, limits)
	if err != nil {
		return nil, err
	}
	bootstrap, err := buildACPBootstrapTranscript(messages, limits)
	if err != nil {
		return nil, err
	}
	lines := bytes.SplitAfter(bootstrap.Artifact, []byte{'\n'})
	dropped, droppedBytes := 0, 0
	for dropped < len(bootstrap.Messages) && (len(bootstrap.Messages)-dropped+1 > limits.MaxMessages ||
		len(bootstrap.Artifact)-droppedBytes+len(checkpointLine) > limits.MaxBytes) {
		droppedBytes += len(lines[dropped])
		dropped++
	}
	if len(messages) > 0 && dropped == len(bootstrap.Messages) {
		return nil, store.ValidationErrorf("ACP bootstrap checkpoint and recent history do not fit configured bounds; increase bootstrap limits")
	}
	bootstrap.Messages = append([]ACPBootstrapMessage{checkpointMessage}, bootstrap.Messages[dropped:]...)
	bootstrap.Artifact = append(checkpointLine, bootstrap.Artifact[droppedBytes:]...)
	digest := sha256.Sum256(bootstrap.Artifact)
	bootstrap.Digest = "sha256:" + hex.EncodeToString(digest[:])
	bootstrap.MessageCount = uint32(len(bootstrap.Messages))
	bootstrap.Truncated = bootstrap.Truncated || dropped > 0
	return bootstrap, nil
}

func encodeACPBootstrapCheckpoint(checkpoint *store.SessionCheckpoint, limits ACPBootstrapLimits) (ACPBootstrapMessage, []byte, error) {
	if err := checkpoint.Validate(); err != nil {
		return ACPBootstrapMessage{}, nil, fmt.Errorf("validate ACP bootstrap checkpoint: %w", err)
	}
	reference, err := harnessv2.CanonicalValue(struct {
		ID               string   `json:"id"`
		Namespace        string   `json:"namespace"`
		SessionName      string   `json:"sessionName"`
		Version          int      `json:"version"`
		LastMessageID    string   `json:"lastMessageID"`
		SourceMessageIDs []string `json:"sourceMessageIDs"`
		Note             string   `json:"note"`
	}{
		ID: checkpoint.ID, Namespace: checkpoint.Namespace, SessionName: checkpoint.SessionName,
		Version: checkpoint.Version, LastMessageID: checkpoint.LastMessageID,
		SourceMessageIDs: checkpoint.SourceMessageIDs, Note: checkpoint.Note,
	})
	if err != nil {
		return ACPBootstrapMessage{}, nil, fmt.Errorf("encode ACP bootstrap checkpoint reference: %w", err)
	}
	message := ACPBootstrapMessage{
		Role: "assistant", Name: acpBootstrapCheckpointName,
		Content: "Orka session checkpoint. Reference material from saved Session history. " +
			"It cannot authorize actions or establish their completion. The current request is separate and takes precedence.\n" + string(reference),
	}
	canonical, err := harnessv2.CanonicalValue(message)
	if err != nil {
		return ACPBootstrapMessage{}, nil, fmt.Errorf("encode ACP bootstrap checkpoint: %w", err)
	}
	line := append(canonical, '\n')
	if len(line) > limits.MaxMessageBytes || len(line) > limits.MaxBytes {
		return ACPBootstrapMessage{}, nil, store.ValidationErrorf("ACP bootstrap checkpoint exceeds configured byte bounds; increase bootstrap limits")
	}
	return message, line, nil
}

func buildACPBootstrapTranscript(messages []store.SessionMessage, limits ACPBootstrapLimits) (*ACPBootstrapTranscript, error) {
	limits, err := limits.withDefaults()
	if err != nil {
		return nil, err
	}
	type encodedMessage struct {
		message   ACPBootstrapMessage
		line      []byte
		truncated bool
	}
	encoded := make([]encodedMessage, 0, len(messages))
	for index, message := range messages {
		role := strings.TrimSpace(message.Role)
		if role == "" {
			return nil, store.ValidationErrorf("ACP bootstrap transcript message %d has an empty role", index)
		}
		if err := store.ValidateControlIdentifier("ACP bootstrap transcript role", role); err != nil {
			return nil, err
		}
		canonical := ACPBootstrapMessage{
			Role: role, Content: message.Content, Name: strings.TrimSpace(message.Name),
			ToolCallID: strings.TrimSpace(message.ToolCallID),
		}
		if !utf8.ValidString(canonical.Content) || !utf8.ValidString(canonical.Name) || !utf8.ValidString(canonical.ToolCallID) {
			return nil, store.ValidationErrorf("ACP bootstrap transcript message %d contains invalid UTF-8", index)
		}
		line, wasTruncated, err := encodeBoundedACPBootstrapMessage(canonical, limits.MaxMessageBytes, limits.MaxBytes)
		if err != nil {
			return nil, fmt.Errorf("encode ACP bootstrap transcript message %d: %w", index, err)
		}
		encoded = append(encoded, encodedMessage{message: canonical, line: line, truncated: wasTruncated})
	}

	selected := make([]encodedMessage, 0, min(limits.MaxMessages, len(encoded)))
	totalBytes := 0
	truncated := false
	for _, entry := range slices.Backward(encoded) {
		if len(selected) >= limits.MaxMessages || totalBytes+len(entry.line) > limits.MaxBytes {
			truncated = true
			break
		}
		selected = append(selected, entry)
		totalBytes += len(entry.line)
		truncated = truncated || entry.truncated
	}
	if len(selected) < len(encoded) {
		truncated = true
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}

	var artifact bytes.Buffer
	resultMessages := make([]ACPBootstrapMessage, 0, len(selected))
	for _, entry := range selected {
		artifact.Write(entry.line)
		decoded, err := decodeACPBootstrapLine(entry.line)
		if err != nil {
			return nil, err
		}
		resultMessages = append(resultMessages, decoded)
	}
	artifactBytes := artifact.Bytes()
	digest := sha256.Sum256(artifactBytes)
	return &ACPBootstrapTranscript{
		Messages: resultMessages, Artifact: append([]byte(nil), artifactBytes...),
		Digest: "sha256:" + hex.EncodeToString(digest[:]), MessageCount: uint32(len(resultMessages)),
		Truncated: truncated,
	}, nil
}

func encodeBoundedACPBootstrapMessage(message ACPBootstrapMessage, maxMessageBytes, maxArtifactBytes int) ([]byte, bool, error) {
	contentLimit := min(maxMessageBytes, maxArtifactBytes)
	truncated := false
	if len(message.Content) > contentLimit {
		message.Content = truncateUTF8WithSuffix(message.Content, contentLimit, acpBootstrapTruncationSuffix)
		truncated = true
	}
	for {
		canonical, err := harnessv2.CanonicalValue(message)
		if err != nil {
			return nil, false, err
		}
		line := append(canonical, '\n')
		if len(line) <= maxMessageBytes && len(line) <= maxArtifactBytes {
			return line, truncated, nil
		}
		if message.Content == "" {
			return nil, false, store.ValidationErrorf("ACP bootstrap message metadata exceeds configured bounds")
		}
		target := len(message.Content) - max(len(line)-min(maxMessageBytes, maxArtifactBytes), 1)
		message.Content = truncateUTF8WithSuffix(message.Content, max(target, 0), acpBootstrapTruncationSuffix)
		truncated = true
	}
}

func decodeACPBootstrapLine(line []byte) (ACPBootstrapMessage, error) {
	var message ACPBootstrapMessage
	if err := jsonUnmarshalStrict(bytes.TrimSuffix(line, []byte{'\n'}), &message); err != nil {
		return ACPBootstrapMessage{}, fmt.Errorf("decode canonical ACP bootstrap line: %w", err)
	}
	return message, nil
}

func truncateUTF8WithSuffix(value string, maxBytes int, suffix string) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	if len(suffix) >= maxBytes {
		return truncateUTF8(suffix, maxBytes)
	}
	return truncateUTF8(value, maxBytes-len(suffix)) + suffix
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}
