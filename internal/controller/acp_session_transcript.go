package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// ACPBootstrapTranscript is a deterministic JSONL suffix of the canonical Orka
// transcript. Structured tool arguments are intentionally omitted: continuity
// restores conversation text without replaying provider-native or tool state.
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
// applying the Task-specific transcript limit before the continuity-wide byte
// and message caps. The explicit limit prevents a caller's narrower context
// boundary from being widened during provider-session recreation.
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
	bootstrap, err := buildACPBootstrapTranscript(messages, c.bootstrapLimits)
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
	bootstrap, err := buildACPBootstrapTranscript(bootstrapMessages, c.bootstrapLimits)
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
	for index := len(encoded) - 1; index >= 0; index-- {
		entry := encoded[index]
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
