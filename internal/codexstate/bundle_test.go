package codexstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/orka-agents/sessionkit"
)

const fixtureThread = "01a10020-1222-76e3-977d-d5165792ae72"
const fixtureName = "rollout-2026-10-02T21-57-44-" + fixtureThread + ".jsonl"

func fixtureHome(t *testing.T) (string, []byte) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", fixtureName))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions", "2026", "10", "02")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fixtureName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return home, data
}

func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCaptureInstallAndReconcile(t *testing.T) {
	home, original := fixtureHome(t)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("credential-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := Capture(t.Context(), home, fixtureThread)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire.Manifest, []byte("credential-marker")) || bytes.Contains(wire.Rollout, []byte("credential-marker")) {
		t.Fatal("captured authentication")
	}
	summary, err := Inspect(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ThreadID != fixtureThread || summary.DataDigest != DataDigest(data) {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	destination, cwd, journal := realDir(t), realDir(t), realDir(t)
	receipt, err := Install(t.Context(), data, destination, cwd, journal)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != sessionkit.Installed {
		t.Fatalf("outcome: %s", receipt.Outcome)
	}
	installed, err := os.ReadFile(filepath.Join(destination, receipt.TargetPath))
	if err != nil || !bytes.Equal(installed, original) {
		t.Fatalf("rollout changed: %v", err)
	}
	retry, err := Install(t.Context(), data, destination, cwd, journal)
	if err != nil {
		t.Fatal(err)
	}
	if retry.OperationID != receipt.OperationID || retry.Outcome != sessionkit.Installed {
		t.Fatalf("retry changed operation: %+v", retry)
	}
	if _, err := Install(t.Context(), data, destination, realDir(t), journal); err == nil {
		t.Fatal("retry changed destination")
	}
	source, err := os.ReadFile(filepath.Join(home, "sessions", "2026", "10", "02", fixtureName))
	if err != nil || !bytes.Equal(source, original) {
		t.Fatalf("source changed: %v", err)
	}
}

func TestCaptureRejectsEncryptedContext(t *testing.T) {
	home, original := fixtureHome(t)
	line := []byte("{\"ordinal\":999,\"timestamp\":\"2026-10-03T04:58:00Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\",\"summary\":[]}}\n")
	if err := os.WriteFile(filepath.Join(home, "sessions", "2026", "10", "02", fixtureName), append(original, line...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(t.Context(), home, fixtureThread); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("encrypted context must be explicitly unsupported: %v", err)
	}
}

func TestInspectRejectsInvalidTransportAndTampering(t *testing.T) {
	home, _ := fixtureHome(t)
	data, err := Capture(t.Context(), home, fixtureThread)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	wire.Rollout = append(wire.Rollout, []byte("{}\n")...)
	tampered, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string][]byte{
		"tampered": tampered, "trailing": append(bytes.Clone(data), []byte("{}")...),
		"duplicate": []byte(`{"format":"x","format":"x","manifest":"eA==","rollout":"eA=="}`),
		"case":      []byte(`{"Format":"x","manifest":"eA==","rollout":"eA=="}`),
		"oversize":  bytes.Repeat([]byte("x"), MaxBundleBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Inspect(t.Context(), invalid); err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
}

func TestInstallResumesBundlePersistedBeforePlan(t *testing.T) {
	home, _ := fixtureHome(t)
	data, err := Capture(t.Context(), home, fixtureThread)
	if err != nil {
		t.Fatal(err)
	}
	journal := realDir(t)
	if _, err := ensureBundle(data, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(t.Context(), data, realDir(t), realDir(t), journal); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRejectsOccupiedHomeAndCancelledLock(t *testing.T) {
	home, _ := fixtureHome(t)
	data, err := Capture(t.Context(), home, fixtureThread)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(t.Context(), data, home, realDir(t), realDir(t)); err == nil {
		t.Fatal("occupied source home accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Install(ctx, data, realDir(t), realDir(t), realDir(t)); err == nil {
		t.Fatal("cancelled install accepted")
	}
}

func TestNativeFormatRejectionClassificationStaysNarrow(t *testing.T) {
	for _, tc := range []struct {
		codes []string
		want  bool
	}{
		{[]string{"encrypted_content"}, true}, {[]string{"unsupported_profile"}, false},
		{[]string{"encrypted_content", "invalid_ordinal"}, false}, {nil, false},
	} {
		var rejected sessionkit.RejectionError
		for _, code := range tc.codes {
			rejected.Rejections = append(rejected.Rejections, sessionkit.Rejection{Code: code})
		}
		if got := errors.Is(classifyNativeFormatError(&rejected), ErrUnsupported); got != tc.want {
			t.Fatalf("classification %v = %v, want %v", tc.codes, got, tc.want)
		}
	}
	ioFailure := &os.PathError{Op: "read", Path: "private", Err: os.ErrPermission}
	if got := classifyNativeFormatError(ioFailure); got != ioFailure {
		t.Fatal("I/O failures must remain distinct")
	}
}
