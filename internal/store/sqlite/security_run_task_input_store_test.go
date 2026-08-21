package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const testSecurityRunTaskInputUID = "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testSecurityRunTaskInput() *store.SecurityRunTaskInput {
	return &store.SecurityRunTaskInput{
		RunUID:         testSecurityRunTaskInputUID,
		Namespace:      "ns",
		RepositoryScan: "repo",
		ScanRunID:      "scan",
		Stage:          "threat-model",
	}
}

func TestSecurityRunTaskInputEmptySnapshotRoundTrip(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	input := testSecurityRunTaskInput()

	created, err := s.SaveSecurityRunTaskInput(ctx, input)
	if err != nil || !created {
		t.Fatalf("SaveSecurityRunTaskInput() = (%v, %v), want (true, nil)", created, err)
	}
	if input.Content != "" {
		t.Fatalf("normalized empty content = %q, want empty", input.Content)
	}
	if want := securityDigestBytes(nil); input.ContentDigest != want {
		t.Fatalf("contentDigest = %q, want %q", input.ContentDigest, want)
	}
	if input.RecordDigest == "" || input.CreatedAt.IsZero() {
		t.Fatalf("saved input missing immutable metadata: %#v", input)
	}

	got, err := s.GetSecurityRunTaskInput(ctx, input.Namespace, input.RunUID, input.Stage)
	if err != nil {
		t.Fatalf("GetSecurityRunTaskInput() error = %v", err)
	}
	if got.RunUID != input.RunUID || got.Namespace != input.Namespace || got.RepositoryScan != input.RepositoryScan ||
		got.ScanRunID != input.ScanRunID || got.Stage != input.Stage || got.SourceVersion != 0 || got.Content != "" ||
		got.ContentDigest != input.ContentDigest || got.RecordDigest != input.RecordDigest || !got.CreatedAt.Equal(input.CreatedAt) {
		t.Fatalf("round trip = %#v, want %#v", got, input)
	}
}

func TestSecurityRunTaskInputNormalizesAndRedactsContent(t *testing.T) {
	s := setupTestStore(t)
	submitted := strings.Join([]string{
		"  first\r\n",
		"Author", "ization: ", "Bear", "er task-input-value-for-redaction\r\n  ",
	}, "")
	input := testSecurityRunTaskInput()
	input.SourceVersion = 7
	input.Content = submitted
	input.ContentDigest = securityDigestBytes([]byte(submitted))

	created, err := s.SaveSecurityRunTaskInput(context.Background(), input)
	if err != nil || !created {
		t.Fatalf("SaveSecurityRunTaskInput() = (%v, %v), want (true, nil)", created, err)
	}
	const wantContent = "first\nAuthorization: [REDACTED]"
	if input.Content != wantContent {
		t.Fatalf("normalized content = %q, want %q", input.Content, wantContent)
	}
	if strings.Contains(input.Content, "task-input-value-for-redaction") {
		t.Fatalf("normalized content retained credential: %q", input.Content)
	}
	if want := securityDigestBytes([]byte(wantContent)); input.ContentDigest != want {
		t.Fatalf("normalized contentDigest = %q, want %q", input.ContentDigest, want)
	}

	got, err := s.GetSecurityRunTaskInput(context.Background(), input.Namespace, input.RunUID, input.Stage)
	if err != nil {
		t.Fatalf("GetSecurityRunTaskInput() error = %v", err)
	}
	if got.Content != wantContent || got.SourceVersion != input.SourceVersion || got.ContentDigest != input.ContentDigest {
		t.Fatalf("persisted normalized input = %#v, want %#v", got, input)
	}
}

func TestSecurityRunTaskInputIdenticalReplay(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	submitted := "  initial\r\ninput  "
	first := testSecurityRunTaskInput()
	first.SourceVersion = 3
	first.Content = submitted
	first.ContentDigest = securityDigestBytes([]byte(submitted))
	replay := *first

	created, err := s.SaveSecurityRunTaskInput(ctx, first)
	if err != nil || !created {
		t.Fatalf("first save = (%v, %v), want (true, nil)", created, err)
	}
	created, err = s.SaveSecurityRunTaskInput(ctx, &replay)
	if err != nil || created {
		t.Fatalf("identical replay = (%v, %v), want (false, nil)", created, err)
	}
	if replay.RecordDigest != first.RecordDigest || replay.ContentDigest != first.ContentDigest || replay.Content != first.Content {
		t.Fatalf("canonical replay = %#v, want canonical content/digests from %#v", replay, first)
	}

	got, err := s.GetSecurityRunTaskInput(ctx, first.Namespace, first.RunUID, first.Stage)
	if err != nil {
		t.Fatalf("GetSecurityRunTaskInput() error = %v", err)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) || got.RecordDigest != first.RecordDigest {
		t.Fatalf("identical replay changed persisted row: got %#v, first %#v", got, first)
	}
}

func TestSecurityRunTaskInputRejectsMismatchedReplay(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	input := testSecurityRunTaskInput()
	input.SourceVersion = 2
	input.Content = "initial input"

	created, err := s.SaveSecurityRunTaskInput(ctx, input)
	if err != nil || !created {
		t.Fatalf("first save = (%v, %v), want (true, nil)", created, err)
	}
	conflict := *input
	conflict.SourceVersion++
	conflict.RecordDigest = ""
	conflict.CreatedAt = input.CreatedAt.AddDate(0, 0, 1)
	if _, err := s.SaveSecurityRunTaskInput(ctx, &conflict); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("mismatched replay error = %v, want ErrDuplicateMismatch", err)
	}

	got, err := s.GetSecurityRunTaskInput(ctx, input.Namespace, input.RunUID, input.Stage)
	if err != nil {
		t.Fatalf("GetSecurityRunTaskInput() error = %v", err)
	}
	if got.SourceVersion != input.SourceVersion || got.Content != input.Content || got.RecordDigest != input.RecordDigest {
		t.Fatalf("mismatched replay changed persisted row: got %#v, want %#v", got, input)
	}
}

func TestSecurityRunTaskInputRejectsInvalidAndOversizedInput(t *testing.T) {
	tests := map[string]func(*store.SecurityRunTaskInput){
		"missing namespace": func(input *store.SecurityRunTaskInput) {
			input.Namespace = ""
		},
		"invalid run UID": func(input *store.SecurityRunTaskInput) {
			input.RunUID = "run_short"
		},
		"invalid stage": func(input *store.SecurityRunTaskInput) {
			input.Stage = "threat model"
		},
		"negative source version": func(input *store.SecurityRunTaskInput) {
			input.SourceVersion = -1
		},
		"invalid UTF-8 content": func(input *store.SecurityRunTaskInput) {
			input.Content = string([]byte{'o', 'k', 0xff})
		},
		"mismatched content digest": func(input *store.SecurityRunTaskInput) {
			input.Content = "content"
			input.ContentDigest = securityDigestBytes([]byte("different"))
		},
		"oversized raw content": func(input *store.SecurityRunTaskInput) {
			input.Content = strings.Repeat(" ", maxSecurityPayloadBytes+1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := setupTestStore(t)
			input := testSecurityRunTaskInput()
			mutate(input)
			originalContent := input.Content
			originalDigest := input.ContentDigest
			if _, err := s.SaveSecurityRunTaskInput(context.Background(), input); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("SaveSecurityRunTaskInput() error = %v, want ErrValidation", err)
			}
			if input.Content != originalContent || input.ContentDigest != originalDigest {
				t.Fatalf("rejected input mutated: content bytes=%d digest=%q", len(input.Content), input.ContentDigest)
			}
		})
	}

	s := setupTestStore(t)
	if _, err := s.SaveSecurityRunTaskInput(context.Background(), nil); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SaveSecurityRunTaskInput(nil) error = %v, want ErrValidation", err)
	}
}

func TestCreateScanRunWithTaskInputReservesRepositoryAcrossRequestKeys(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	firstRequestKey := "req_" + strings.Repeat("a", 64)
	firstRun := &store.ScanRun{
		ID: "scan_first", RunUID: testSecurityRunTaskInputUID, Namespace: "ns", RepositoryScan: "repo",
		RepositoryScanUID: "repo-uid", RepositoryScanGeneration: 1, TaskName: "task-first",
		Mode: "manual", Phase: "pending", RequestIdempotencyKey: firstRequestKey, IdempotencyKey: firstRequestKey,
		StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}
	firstInput := testSecurityRunTaskInput()
	firstInput.ScanRunID = firstRun.ID
	if err := s.CreateScanRunWithTaskInput(ctx, firstRun, firstInput); err != nil {
		t.Fatalf("CreateScanRunWithTaskInput(first) error = %v", err)
	}

	secondUID := "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	secondRequestKey := "req_" + strings.Repeat("b", 64)
	secondRun := *firstRun
	secondRun.ID = "scan_second"
	secondRun.RunUID = secondUID
	secondRun.TaskName = "task-second"
	secondRun.RequestIdempotencyKey = secondRequestKey
	secondRun.IdempotencyKey = secondRequestKey
	secondRun.StartedAt = firstRun.StartedAt.Add(time.Second)
	secondInput := *firstInput
	secondInput.RunUID = secondUID
	secondInput.ScanRunID = secondRun.ID
	secondInput.RecordDigest = ""
	secondInput.CreatedAt = time.Time{}
	if err := s.CreateScanRunWithTaskInput(ctx, &secondRun, &secondInput); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateScanRunWithTaskInput(conflict) error = %v, want ErrConflict", err)
	}
	if _, err := s.GetSecurityRunTaskInput(ctx, secondInput.Namespace, secondUID, secondInput.Stage); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSecurityRunTaskInput(conflicting run) error = %v, want ErrNotFound", err)
	}

	completed, err := s.GetScanRun(ctx, firstRun.Namespace, firstRun.ID)
	if err != nil {
		t.Fatalf("GetScanRun(first) error = %v", err)
	}
	now := time.Now().UTC()
	completed.Phase = storedScanRunPhaseSucceeded
	completed.CompletedAt = &now
	if err := s.UpdateScanRun(ctx, completed); err != nil {
		t.Fatalf("UpdateScanRun(first terminal) error = %v", err)
	}
	if err := s.CreateScanRunWithTaskInput(ctx, &secondRun, &secondInput); err != nil {
		t.Fatalf("CreateScanRunWithTaskInput(after terminal) error = %v", err)
	}

	runs, _, err := s.ListScanRuns(ctx, firstRun.Namespace, firstRun.RepositoryScan, 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns() error = %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %#v, want both sequential repository runs", runs)
	}
}

func TestCreateScanRunWithTaskInputRequiresRequestAndRepositoryBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store.ScanRun)
	}{
		{name: "missing request key", mutate: func(run *store.ScanRun) {
			run.RequestIdempotencyKey = ""
			run.IdempotencyKey = ""
		}},
		{name: "missing repository UID", mutate: func(run *store.ScanRun) { run.RepositoryScanUID = "" }},
		{name: "missing repository generation", mutate: func(run *store.ScanRun) { run.RepositoryScanGeneration = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			run := &store.ScanRun{
				ID: "scan-bound", RunUID: testSecurityRunTaskInputUID, Namespace: "ns", RepositoryScan: "repo",
				RepositoryScanUID: "repo-uid", RepositoryScanGeneration: 1, TaskName: "task-bound",
				Mode: "manual", Phase: "pending", RequestIdempotencyKey: "req_bound", IdempotencyKey: "req_bound",
				StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
			}
			tt.mutate(run)
			input := testSecurityRunTaskInput()
			input.ScanRunID = run.ID
			if err := s.CreateScanRunWithTaskInput(context.Background(), run, input); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("CreateScanRunWithTaskInput() error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestCreateScanRunWithTaskInputRequiresPendingInitialState(t *testing.T) {
	tests := []struct {
		name         string
		phase        string
		bundleStatus store.BundleStatus
	}{
		{name: "running phase", phase: "running", bundleStatus: store.BundleStatusNotStarted},
		{name: "succeeded phase", phase: "succeeded", bundleStatus: store.BundleStatusNotStarted},
		{name: "draft bundle", phase: "pending", bundleStatus: store.BundleStatusDraft},
		{name: "sealing bundle", phase: "pending", bundleStatus: store.BundleStatusSealing},
		{name: "sealed bundle", phase: "pending", bundleStatus: store.BundleStatusSealed},
		{name: "retryable failed bundle", phase: "pending", bundleStatus: store.BundleStatusRetryableFailed},
		{name: "failed bundle", phase: "pending", bundleStatus: store.BundleStatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			quality := store.LegacyScanQuality()
			quality.BundleStatus = tt.bundleStatus
			run := &store.ScanRun{
				ID: "scan", RunUID: testSecurityRunTaskInputUID, Namespace: "ns", RepositoryScan: "repo",
				RepositoryScanUID: "repo-uid", RepositoryScanGeneration: 1, TaskName: "task-bound",
				Mode: "manual", Phase: tt.phase, RequestIdempotencyKey: "req_bound", IdempotencyKey: "req_bound",
				StartedAt: time.Now().UTC(), Quality: quality,
			}
			input := testSecurityRunTaskInput()
			if err := s.CreateScanRunWithTaskInput(context.Background(), run, input); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("CreateScanRunWithTaskInput() error = %v, want ErrValidation", err)
			}
			if _, err := s.GetScanRun(context.Background(), run.Namespace, run.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetScanRun(after rejected reservation) error = %v, want ErrNotFound", err)
			}
			if _, err := s.GetSecurityRunTaskInput(context.Background(), input.Namespace, input.RunUID, input.Stage); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetSecurityRunTaskInput(after rejected reservation) error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestActiveRepositoryIndexRetainsSealingRunUntilTerminalBundleState(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	firstRequestKey := "req_sealing_first"
	sealingRun := &store.ScanRun{
		ID: "scan-sealing", RunUID: testSecurityRunTaskInputUID, Namespace: "ns", RepositoryScan: "repo",
		RepositoryScanUID: "repo-uid", RepositoryScanGeneration: 1, TaskName: "task-sealing",
		Mode: "manual", Phase: "succeeded", RequestIdempotencyKey: firstRequestKey, IdempotencyKey: firstRequestKey,
		StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}
	sealingRun.Quality.BundleStatus = store.BundleStatusSealing
	if err := s.CreateScanRun(ctx, sealingRun); err != nil {
		t.Fatalf("CreateScanRun(sealing) error = %v", err)
	}

	secondUID := "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	secondRun := &store.ScanRun{
		ID: "scan-second", RunUID: secondUID, Namespace: "ns", RepositoryScan: "repo",
		RepositoryScanUID: "repo-uid", RepositoryScanGeneration: 1, TaskName: "task-second",
		Mode: "manual", Phase: "pending", RequestIdempotencyKey: "req_sealing_second", IdempotencyKey: "req_sealing_second",
		StartedAt: sealingRun.StartedAt.Add(time.Second), Quality: store.LegacyScanQuality(),
	}
	secondInput := testSecurityRunTaskInput()
	secondInput.RunUID = secondUID
	secondInput.ScanRunID = secondRun.ID
	if err := s.CreateScanRunWithTaskInput(ctx, secondRun, secondInput); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateScanRunWithTaskInput(during sealing) error = %v, want ErrConflict", err)
	}

	sealed, err := s.GetScanRun(ctx, sealingRun.Namespace, sealingRun.ID)
	if err != nil {
		t.Fatalf("GetScanRun(sealing) error = %v", err)
	}
	sealed.Quality.BundleStatus = store.BundleStatusSealed
	if err := s.UpdateScanRun(ctx, sealed); err != nil {
		t.Fatalf("UpdateScanRun(sealed) error = %v", err)
	}
	if err := s.CreateScanRunWithTaskInput(ctx, secondRun, secondInput); err != nil {
		t.Fatalf("CreateScanRunWithTaskInput(after sealing) error = %v", err)
	}

	var indexSQL string
	if err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_security_scan_runs_active_repository'`).Scan(&indexSQL); err != nil {
		t.Fatalf("load active-repository index: %v", err)
	}
	if !strings.Contains(indexSQL, "bundle_status = 'sealing'") {
		t.Fatalf("active-repository index = %q, want sealing predicate", indexSQL)
	}
}
