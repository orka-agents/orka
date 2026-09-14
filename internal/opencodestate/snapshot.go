// Package opencodestate copies the private conversation data used by the pinned
// OpenCode ACP implementation. It does not copy databases, credentials, config,
// event journals, workspace files, or SQL supplied by the agent.
package opencodestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const (
	// ProviderVersion is the only native database version this format supports.
	ProviderVersion = "1.18.9"
	// ProviderID and Mode are the model namespace and mode projected by Orka.
	ProviderID = "orka"
	Mode       = "build"

	// MaxCompressedBytes and MaxDecompressedBytes bound the private wire format.
	MaxCompressedBytes   = 512 << 10
	MaxDecompressedBytes = 16 << 20
	// MaxMessages, MaxParts, and MaxRowBytes bound native conversation reads.
	MaxMessages = 1024
	MaxParts    = 8192
	MaxRowBytes = 1 << 20

	formatVersion = 1
)

var (
	// ErrIncompatible means reconstruction from Orka history is required.
	ErrIncompatible = errors.New("OpenCode native history is incompatible")
	// ErrInvalidSnapshot means the private snapshot failed validation.
	ErrInvalidSnapshot = errors.New("OpenCode native snapshot is invalid")
	// ErrSizeLimit means a conversation exceeds a capture or restore bound.
	ErrSizeLimit = errors.New("OpenCode native snapshot exceeds its size limit")
)

type snapshot struct {
	Format   int          `json:"format"`
	Version  string       `json:"version"`
	CWD      string       `json:"cwd"`
	Model    string       `json:"model"`
	Project  projectRow   `json:"project"`
	Session  sessionRow   `json:"session"`
	Messages []messageRow `json:"messages"`
	Parts    []partRow    `json:"parts"`
}

type projectRow struct {
	ID          string  `json:"id"`
	Worktree    string  `json:"worktree"`
	VCS         *string `json:"vcs,omitempty"`
	TimeCreated int64   `json:"timeCreated"`
	TimeUpdated int64   `json:"timeUpdated"`
}

type sessionRow struct {
	ID          string          `json:"id"`
	ProjectID   string          `json:"projectID"`
	Slug        string          `json:"slug"`
	Directory   string          `json:"directory"`
	Path        *string         `json:"path,omitempty"`
	Title       string          `json:"title"`
	Version     string          `json:"version"`
	Agent       string          `json:"agent"`
	Model       json.RawMessage `json:"model"`
	TimeCreated int64           `json:"timeCreated"`
	TimeUpdated int64           `json:"timeUpdated"`
}

type messageRow struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"sessionID"`
	TimeCreated int64           `json:"timeCreated"`
	TimeUpdated int64           `json:"timeUpdated"`
	Data        json.RawMessage `json:"data"`
}

type partRow struct {
	ID          string          `json:"id"`
	MessageID   string          `json:"messageID"`
	SessionID   string          `json:"sessionID"`
	TimeCreated int64           `json:"timeCreated"`
	TimeUpdated int64           `json:"timeUpdated"`
	Data        json.RawMessage `json:"data"`
}

// Capture reads one complete conversation through a single SQLite transaction,
// including committed WAL data. The caller must first prove that the provider
// process and its descendants are frozen and that no prompt can start. The
// database must be a regular file at a canonical absolute path. Only inline
// history is supported; external tool output, snapshots, native child sessions,
// compaction, and unfinished messages require reconstruction instead.
func Capture(ctx context.Context, dbPath, sessionID, cwd, model string) ([]byte, error) {
	if err := validateIdentity(sessionID, cwd, model); err != nil {
		return nil, err
	}
	db, conn, err := openDatabase(ctx, dbPath, true)
	if err != nil {
		return nil, err
	}
	defer closeDatabase(db, conn)
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, databaseError(ctx, "begin consistent read")
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateSchema(ctx, tx); err != nil {
		return nil, err
	}
	if err := validateCaptureTables(ctx, tx, sessionID); err != nil {
		return nil, err
	}
	saved := snapshot{Format: formatVersion, Version: ProviderVersion, CWD: cwd, Model: model}
	if err := readConversation(ctx, tx, &saved, sessionID); err != nil {
		return nil, err
	}
	if err := saved.validate(sessionID, cwd, model); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, databaseError(ctx, "finish consistent read")
	}
	return encodeSnapshot(&saved)
}

// Restore imports a validated conversation into an empty database initialized
// by the exact pinned OpenCode binary. No provider process may have the target
// database open. The caller must regenerate provider and MCP configuration and
// apply ProviderID/model and Mode through ACP before authorizing a new prompt.
// The target path follows the same regular-file rules as Capture.
func Restore(ctx context.Context, freshDBPath string, data []byte, sessionID, cwd, model string) error {
	if err := validateIdentity(sessionID, cwd, model); err != nil {
		return err
	}
	saved, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	if err := saved.validate(sessionID, cwd, model); err != nil {
		return err
	}
	db, conn, err := openDatabase(ctx, freshDBPath, false)
	if err != nil {
		return err
	}
	defer closeDatabase(db, conn)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(ctx, "begin restore")
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateSchema(ctx, tx); err != nil {
		return err
	}
	if err := validateEmptyDatabase(ctx, tx); err != nil {
		return err
	}
	if err := writeConversation(ctx, tx, saved); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return databaseError(ctx, "commit restore")
	}
	return nil
}

func validateIdentity(sessionID, cwd, model string) error {
	if !validID(sessionID, "ses_") {
		return incompatible("native session ID is invalid")
	}
	if !canonicalAbsolutePath(cwd) {
		return incompatible("working directory must be a stable absolute path")
	}
	provider, name, ok := strings.Cut(model, "/")
	if !ok || provider == "" || name == "" || len(model) > 512 || strings.TrimSpace(model) != model ||
		strings.ContainsAny(model, "\x00\r\n{}") {
		return incompatible("model must match the governed provider/model profile")
	}
	return nil
}

func canonicalAbsolutePath(path string) bool {
	return len(path) <= 4096 && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func validID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func incompatible(reason string) error { return fmt.Errorf("%w: %s", ErrIncompatible, reason) }

// Database errors can contain agent-controlled SQL or values. Callers may log
// these errors, so expose only a fixed stage and context cancellation.
func databaseError(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return incompatible("cannot " + stage)
}

func encodeSnapshot(saved *snapshot) ([]byte, error) {
	plain, err := json.Marshal(saved)
	if err != nil {
		return nil, ErrInvalidSnapshot
	}
	if len(plain) > MaxDecompressedBytes {
		return nil, ErrSizeLimit
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&boundedWriter{target: &compressed, remaining: MaxCompressedBytes})
	if _, err := writer.Write(plain); err != nil {
		_ = writer.Close()
		return nil, ErrSizeLimit
	}
	if err := writer.Close(); err != nil {
		return nil, ErrSizeLimit
	}
	return compressed.Bytes(), nil
}

type boundedWriter struct {
	target    *bytes.Buffer
	remaining int
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if len(data) > w.remaining {
		return 0, ErrSizeLimit
	}
	n, err := w.target.Write(data)
	w.remaining -= n
	return n, err
}

func decodeSnapshot(data []byte) (*snapshot, error) {
	if len(data) > MaxCompressedBytes {
		return nil, ErrSizeLimit
	}
	input := bytes.NewReader(data)
	reader, err := gzip.NewReader(input)
	if err != nil {
		return nil, ErrInvalidSnapshot
	}
	reader.Multistream(false)
	plain, readErr := io.ReadAll(io.LimitReader(reader, MaxDecompressedBytes+1))
	closeErr := reader.Close()
	if len(plain) > MaxDecompressedBytes {
		return nil, ErrSizeLimit
	}
	if readErr != nil || closeErr != nil || input.Len() != 0 {
		return nil, ErrInvalidSnapshot
	}
	if err := validateJSON(plain); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	var saved snapshot
	if err := decoder.Decode(&saved); err != nil {
		return nil, ErrInvalidSnapshot
	}
	return &saved, nil
}
