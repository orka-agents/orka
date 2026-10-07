// Package codexstate carries the supported SessionKit bundle between Orka and
// local Codex homes. Provider configuration and credentials belong to the caller.
package codexstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/sessionkit"
	"golang.org/x/sys/unix"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// MaxBundleBytes is an Orka transport limit, not SessionKit's storage limit.
const MaxBundleBytes = harnessv2.MaxNativeSessionBytes

// ErrUnsupported identifies an otherwise stopped conversation outside this
// consumer's supported format or transport bounds. I/O and uncertain outcomes
// are deliberately separate.
var ErrUnsupported = errors.New("native session format or transport is unsupported")

const bundleFormat = "sessionkit/codex-paginated-v1"

type wireBundle struct {
	Format   string `json:"format"`
	Manifest []byte `json:"manifest"`
	Rollout  []byte `json:"rollout"`
}

type Summary struct {
	ThreadID   string
	DataDigest string
	Manifest   sessionkit.Manifest
}

func DataDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func limits() sessionkit.Budget {
	return sessionkit.Budget{
		MaxBytes: 8 * MaxBundleBytes, MaxTempBytes: MaxBundleBytes,
		MaxLineBytes: MaxBundleBytes, Timeout: 30 * time.Second,
	}
}

func privateTemp() (string, error) {
	dir, err := os.MkdirTemp("", "orka-codex-bundle-")
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return real, nil
}

// Capture requires a stopped source writer. It retains only manifest and
// rollout bytes, never authentication, configuration, or native databases.
func Capture(ctx context.Context, home, threadID string) ([]byte, error) {
	parent, err := privateTemp()
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(parent) }()
	bundle, err := sessionkit.Capture(ctx, sessionkit.Source{
		Harness: sessionkit.Codex, Root: home, ThreadID: threadID,
	}, sessionkit.CaptureOptions{BundleDir: filepath.Join(parent, "bundle"), Budget: limits()})
	if err != nil {
		var exhausted *sessionkit.BudgetError
		if errors.As(err, &exhausted) && exhausted.Limit != "timeout" {
			return nil, fmt.Errorf("%w: capture budget %s", ErrUnsupported, exhausted.Limit)
		}
		return nil, err
	}
	if err := supported(bundle.Manifest); err != nil {
		return nil, err
	}
	manifest, err := readBounded(filepath.Join(bundle.Dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	rollout, err := readBounded(filepath.Join(bundle.Dir, "components", "rollout.jsonl"))
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(wireBundle{Format: bundleFormat, Manifest: manifest, Rollout: rollout})
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBundleBytes {
		return nil, fmt.Errorf("%w: native bundle exceeds Orka's %d-byte transport limit", ErrUnsupported, MaxBundleBytes)
	}
	return data, nil
}

func supported(manifest sessionkit.Manifest) error {
	if manifest.Profile != sessionkit.CodexPaginated || manifest.SourceCLIVersion != "0.160.0" {
		return fmt.Errorf("%w: native migration requires the Codex 0.160.0 paginated profile", ErrUnsupported)
	}
	for _, warning := range manifest.Inspection.Warnings {
		if warning.Code == "encrypted_content" {
			return fmt.Errorf("%w: encrypted native context has not been validated for Orka migration", ErrUnsupported)
		}
	}
	return nil
}

func decode(data []byte) (wireBundle, error) {
	var bundle wireBundle
	if len(data) == 0 || len(data) > MaxBundleBytes || !utf8.Valid(data) {
		return bundle, errors.New("native bundle exceeds its encoding or size bounds")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return bundle, errors.New("native bundle must be an object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return bundle, errors.New("invalid native bundle encoding")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return bundle, errors.New("duplicate native bundle field")
		}
		seen[key] = true
		switch key {
		case "format":
			err = decoder.Decode(&bundle.Format)
		case "manifest":
			err = decoder.Decode(&bundle.Manifest)
		case "rollout":
			err = decoder.Decode(&bundle.Rollout)
		default:
			return bundle, errors.New("unknown native bundle field")
		}
		if err != nil {
			return bundle, errors.New("invalid native bundle field")
		}
	}
	if _, err := decoder.Token(); err != nil {
		return bundle, errors.New("invalid native bundle encoding")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return bundle, errors.New("native bundle contains trailing data")
	}
	if len(seen) != 3 || bundle.Format != bundleFormat || len(bundle.Manifest) == 0 || len(bundle.Rollout) == 0 {
		return bundle, errors.New("unsupported or incomplete native bundle")
	}
	return bundle, nil
}

func readBounded(name string) ([]byte, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("native bundle component must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBundleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBundleBytes {
		return nil, fmt.Errorf("%w: native bundle component exceeds transport bounds", ErrUnsupported)
	}
	return data, nil
}

func writeNew(name string, data []byte) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	return errors.Join(writeErr, file.Close())
}

func syncDir(name string) error {
	dir, err := os.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func materialize(data []byte, dir string) error {
	wire, err := decode(data)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dir, "components"), 0o700); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, "manifest.json"), wire.Manifest); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, "components", "rollout.jsonl"), wire.Rollout); err != nil {
		return err
	}
	if err := syncDir(filepath.Join(dir, "components")); err != nil {
		return err
	}
	return syncDir(dir)
}

func ensureBundle(data []byte, journalDir string) (string, error) {
	dir := filepath.Join(journalDir, "bundle")
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() {
			return "", errors.New("saved native bundle must be a directory")
		}
		wire, err := decode(data)
		if err != nil {
			return "", err
		}
		for name, expected := range map[string][]byte{
			"manifest.json": wire.Manifest, "components/rollout.jsonl": wire.Rollout,
		} {
			actual, err := readBounded(filepath.Join(dir, name))
			if err != nil || !bytes.Equal(actual, expected) {
				return "", errors.New("saved native bundle differs from the requested operation")
			}
		}
		return dir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	stage, err := os.MkdirTemp(journalDir, ".bundle-stage-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	ready := filepath.Join(stage, "bundle")
	if err := materialize(data, ready); err != nil {
		return "", err
	}
	if err := os.Rename(ready, dir); err != nil {
		return "", err
	}
	if err := syncDir(journalDir); err != nil {
		return "", err
	}
	return dir, nil
}

// Inspect verifies the complete bundle without activating a native client.
func Inspect(ctx context.Context, data []byte) (Summary, error) {
	parent, err := privateTemp()
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = os.RemoveAll(parent) }()
	dir := filepath.Join(parent, "bundle")
	if err := materialize(data, dir); err != nil {
		return Summary{}, err
	}
	bundle, err := sessionkit.OpenBundle(ctx, dir, limits())
	if err != nil {
		return Summary{}, err
	}
	if err := supported(bundle.Manifest); err != nil {
		return Summary{}, err
	}
	return Summary{ThreadID: bundle.Manifest.ThreadID, DataDigest: DataDigest(data), Manifest: bundle.Manifest}, nil
}

type storedPlan struct {
	DataDigest string          `json:"dataDigest"`
	Plan       sessionkit.Plan `json:"plan"`
}

func atomicJSON(dir, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".handoff-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer func() { _ = os.Remove(temp) }()
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(temp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

// within reports whether name is root or lies under it. Existing paths compare
// by identity so aliases such as symlinked parents cannot hide an overlap; a
// root that does not exist yet falls back to the cleaned lexical ancestry.
func within(root, name string) bool {
	root, name = filepath.Clean(root), filepath.Clean(name)
	rootInfo, err := os.Stat(root)
	if err != nil {
		rel, relErr := filepath.Rel(root, name)
		return relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for ; ; name = filepath.Dir(name) {
		info, err := os.Stat(name)
		if err == nil && os.SameFile(rootInfo, info) {
			return true
		}
		if filepath.Dir(name) == name {
			return false
		}
	}
}

// Install records one immutable plan before publication and reconciles that
// same plan on retries. journalDir must be private and outside the Codex home.
func Install(ctx context.Context, data []byte, home, cwd, journalDir string) (sessionkit.Receipt, error) {
	var receipt sessionkit.Receipt
	for _, name := range []string{home, cwd, journalDir} {
		if !filepath.IsAbs(name) || filepath.Clean(name) != name {
			return receipt, errors.New("native migration directories must be absolute and clean")
		}
	}
	if within(home, journalDir) || within(journalDir, home) {
		return receipt, errors.New("native journal and Codex home must not overlap")
	}
	summary, err := Inspect(ctx, data)
	if err != nil {
		return receipt, err
	}
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		return receipt, err
	}
	lock, err := os.OpenFile(filepath.Join(journalDir, "handoff.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = lock.Close() }()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return receipt, err
		}
		select {
		case <-ctx.Done():
			return receipt, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	planFile := filepath.Join(journalDir, "plan.json")
	var saved storedPlan
	planData, err := readBounded(planFile)
	switch {
	case err == nil:
		decoder := json.NewDecoder(bytes.NewReader(planData))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&saved); err != nil {
			return receipt, errors.New("invalid saved native install plan")
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return receipt, errors.New("invalid saved native install plan tail")
		}
		if saved.DataDigest != summary.DataDigest || saved.Plan.ThreadID != summary.ThreadID ||
			saved.Plan.ResumeHints.CodexHome != home || saved.Plan.ResumeHints.CWDOverride != cwd {
			return receipt, errors.New("native install operation is already bound to another bundle or destination")
		}
	case errors.Is(err, os.ErrNotExist):
		for _, name := range []string{"sessions", "archived_sessions", "state_5.sqlite", "thread_history_1.sqlite", "session_index.jsonl", "history.jsonl"} {
			if _, err := os.Lstat(filepath.Join(home, name)); !errors.Is(err, os.ErrNotExist) {
				return receipt, errors.New("native migration requires a fresh isolated Codex home")
			}
		}
		dir, err := ensureBundle(data, journalDir)
		if err != nil {
			return receipt, err
		}
		bundle, err := sessionkit.OpenBundle(ctx, dir, limits())
		if err != nil {
			return receipt, err
		}
		saved.Plan, err = sessionkit.PlanInstall(ctx, bundle, sessionkit.Destination{
			Harness: sessionkit.Codex, CLIVersion: "0.160.0", Root: home,
			WorkingDir: cwd, JournalDir: journalDir,
		})
		if err != nil {
			return receipt, err
		}
		saved.DataDigest = summary.DataDigest
		if err := atomicJSON(journalDir, "plan.json", saved); err != nil {
			return receipt, err
		}
	default:
		return receipt, err
	}
	receipt, err = sessionkit.Install(ctx, saved.Plan)
	if saveErr := atomicJSON(journalDir, "receipt.json", receipt); saveErr != nil {
		receipt.Outcome = sessionkit.Unknown
		return receipt, &sessionkit.UnknownOutcomeError{OperationID: saved.Plan.OperationID, Err: errors.Join(err, saveErr)}
	}
	return receipt, err
}
