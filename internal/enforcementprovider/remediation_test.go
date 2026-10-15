package enforcementprovider

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func activatedAttempt(t *testing.T, store Store) Authorization {
	t.Helper()
	provider, trust, authorization := newSynthetic()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_ = attempt
	return authorization
}

func TestProofExpiryCheckedAtFirstConsumptionAndRetryRemainsIdempotent(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	now := time.Now().UTC()
	provider.p.IssuedAt = now.Add(-time.Second)
	provider.p.ExpiresAt = now.Add(time.Second)
	provider.p.AuthorizationDigest = authorizationDigest(authorization)
	SignProof(&provider.p, provider.priv)
	clock := now
	orchestrator := Orchestrator{Store: NewMemoryStore(), Provider: provider, Trust: trust, Now: func() time.Time { return clock }}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Second)
	if _, err = orchestrator.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID); err == nil {
		t.Fatal("consumed expired proof")
	}
	clock = now
	consumed, err := orchestrator.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Second)
	if retried, retryErr := orchestrator.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID); retryErr != nil || retried.ReleaseID != consumed.ReleaseID {
		t.Fatalf("idempotent retry failed: %#v %v", retried, retryErr)
	}
}

func TestStoreSnapshotsAndFailedCASDoNotAlias(t *testing.T) {
	for name, store := range map[string]Store{"memory": NewMemoryStore()} {
		t.Run(name, func(t *testing.T) {
			authorization := activatedAttempt(t, store)
			snapshot, err := store.Get(authorization.Context.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.PolicyBytes[0] ^= 1
			snapshot.Descriptor.Capabilities[0] = "mutated"
			original, _ := store.Get(authorization.Context.AttemptID)
			if original.Context.PolicyBytes[0] == snapshot.Context.PolicyBytes[0] || original.Descriptor.Capabilities[0] == "mutated" {
				t.Fatal("Get returned mutable aliases")
			}
			_, err = store.CompareAndSwap(authorization.Context.AttemptID, original.Version, func(candidate *Attempt) error { candidate.Context.PolicyBytes[0] ^= 1; return errors.New("abort") })
			if err == nil {
				t.Fatal("failed CAS unexpectedly committed")
			}
			after, _ := store.Get(authorization.Context.AttemptID)
			if after.Context.PolicyBytes[0] != original.Context.PolicyBytes[0] {
				t.Fatal("failed CAS leaked mutation")
			}
		})
	}
}

func TestFileStoreExclusiveOwnershipCloseAndAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewFileStore(path); err == nil {
		t.Fatal("second writer opened same state")
	}
	authorization := activatedAttempt(t, store)
	snapshot, err := store.Get(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.PolicyBytes[0] ^= 1
	again, _ := store.Get(authorization.Context.AttemptID)
	if snapshot.Context.PolicyBytes[0] == again.Context.PolicyBytes[0] {
		t.Fatal("file snapshot aliases cache")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(authorization.Context.AttemptID); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Get after close = %v", err)
	}
	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreRejectsMalformedAndUnknownState(t *testing.T) {
	for name, makeData := range map[string]func(string) string{
		"trailing": func(path string) string {
			return fmt.Sprintf(`{"version":4,"storePath":%q,"attempts":{}} {}`, path)
		},
		"unknown field": func(path string) string {
			return fmt.Sprintf(`{"version":4,"storePath":%q,"attempts":{},"other":true}`, path)
		},
		"duplicate field": func(path string) string {
			return fmt.Sprintf(`{"version":4,"version":4,"storePath":%q,"attempts":{}}`, path)
		},
		"unknown phase": func(path string) string {
			return fmt.Sprintf(`{"version":4,"storePath":%q,"attempts":{"a":{"Context":{"AttemptID":"a"},"Phase":"Future","Version":1}}}`, path)
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(makeData(path)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewFileStore(path); err == nil {
				t.Fatal("accepted malformed state")
			}
		})
	}
}

func TestRecoveryFailsClosedForQuarantinedAndUnknownPhases(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	for _, phase := range []Phase{PhaseQuarantined, Phase("Future")} {
		store := NewMemoryStore()
		attempt, err := store.Create(authorization.Context)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.CompareAndSwap(authorization.Context.AttemptID, attempt.Version, func(a *Attempt) error {
			a.Authorization = cloneAuthorization(authorization)
			a.Descriptor = cloneDescriptor(provider.d)
			a.Phase = phase
			return nil
		})
		if phase == Phase("Future") && err == nil {
			t.Fatal("store accepted unknown phase")
		}
		if phase == PhaseQuarantined {
			orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
			result := orchestrator.Recover([]Attempt{{Context: authorization.Context}})
			if result[authorization.Context.AttemptID] == nil || !strings.Contains(result[authorization.Context.AttemptID].Error(), "quarantined") {
				t.Fatal(result)
			}
		}
	}
}

func TestProofCannotReplayAcrossAttempts(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	store := NewMemoryStore()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.Activate(authorization.Context.AttemptID); err != nil {
		t.Fatal(err)
	}
	other := authorization
	other.Context = cloneContext(authorization.Context)
	other.Context.AttemptID = "other-attempt"
	other.Context.RequestID = "other-request"
	other.Context.IdempotencyKey = "other-idempotency"
	other.ID = "other-authorization"
	if _, err := orchestrator.Authorize(other); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.Activate(other.Context.AttemptID); err == nil {
		t.Fatal("replayed proof across attempt")
	}
}

func TestCleanupAcceptsOnlyTypedNotFound(t *testing.T) {
	for name, cleanupErr := range map[string]error{"typed not found": &ProviderError{Code: "not_found", Message: "gone"}, "other": &ProviderError{Code: "unavailable", Message: "down"}} {
		t.Run(name, func(t *testing.T) {
			provider, trust, authorization := newSynthetic()
			provider.cleanupErr = cleanupErr
			orchestrator := Orchestrator{Store: NewMemoryStore(), Provider: provider, Trust: trust, Now: time.Now}
			if _, err := orchestrator.Authorize(authorization); err != nil {
				t.Fatal(err)
			}
			attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err = orchestrator.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err = orchestrator.Release(authorization.Context.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err = orchestrator.Cleanup(authorization.Context.AttemptID)
			if name == "typed not found" && (err != nil || attempt.Phase != PhaseCleaned) {
				t.Fatalf("typed not-found did not clean: %#v %v", attempt, err)
			}
			if name == "other" && err == nil {
				t.Fatal("non-not-found cleanup failure was ignored")
			}
		})
	}
}

type blockingCleanupProvider struct {
	*synthetic
	entered chan struct{}
	resume  chan struct{}
}

func (p *blockingCleanupProvider) Cleanup(request CleanupRequest) error {
	close(p.entered)
	<-p.resume
	return p.synthetic.Cleanup(request)
}

func TestControllerCloseWaitsForInflightOperation(t *testing.T) {
	base, trust, authorization := newSynthetic()
	provider := &blockingCleanupProvider{synthetic: base, entered: make(chan struct{}), resume: make(chan struct{})}
	controller, err := NewController(filepath.Join(t.TempDir(), "state.json"), provider, trust, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.Start(); err != nil {
		t.Fatal(err)
	}
	attempt, err := controller.Hold(authorization)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = controller.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = controller.Release(authorization.Context.AttemptID); err != nil {
		t.Fatal(err)
	}
	cleanupDone := make(chan error, 1)
	go func() {
		_, cleanupErr := controller.Cleanup(authorization.Context.AttemptID)
		cleanupDone <- cleanupErr
	}()
	<-provider.entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- controller.Close() }()
	select {
	case closeErr := <-closeDone:
		t.Fatalf("Close returned during in-flight operation: %v", closeErr)
	case <-time.After(20 * time.Millisecond):
	}
	close(provider.resume)
	if cleanupErr := <-cleanupDone; cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if closeErr := <-closeDone; closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestStrictTrustJSONRejectsDuplicateAndTrailingData(t *testing.T) {
	for _, data := range []string{
		`{"providerId":"a","providerId":"b"}`,
		`{"providerId":"a"} {}`,
	} {
		if _, err := ParseTrustProfileJSON([]byte(data)); err == nil {
			t.Fatal("accepted ambiguous trust JSON")
		}
	}
}

func TestFileStoreCanonicalizesSymlinkedParentBeforeLocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FileStore locking is unsupported on Windows")
	}
	realDirectory := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasDirectory := filepath.Join(filepath.Dir(realDirectory), "alias")
	if err := os.Symlink(realDirectory, aliasDirectory); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(filepath.Join(realDirectory, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = NewFileStore(filepath.Join(aliasDirectory, "state.json")); err == nil {
		t.Fatal("second writer opened initially absent store through a symlinked parent")
	}
}

func TestFileStoreRejectsPreexistingHardlinkAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FileStore locking is unsupported on Windows")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, authorization := newSynthetic()
	if _, err = store.Create(authorization.Context); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(path), "alias.json")
	if err = os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err = NewFileStore(path); err == nil || !strings.Contains(err.Error(), "hard-link") {
		t.Fatalf("original hard-linked path opened: %v", err)
	}
	if _, err = NewFileStore(alias); err == nil || !strings.Contains(err.Error(), "hard-link") {
		t.Fatalf("alias hard-linked path opened: %v", err)
	}
}

func TestFileStoreHardlinkCreatedWhileOpenCannotDiverge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FileStore locking is unsupported on Windows")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	_, _, authorization := newSynthetic()
	attempt, err := store.Create(authorization.Context)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(path), "alias.json")
	if err = os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CompareAndSwap(attempt.Context.AttemptID, attempt.Version, func(*Attempt) error {
		return nil
	}); err == nil || !strings.Contains(err.Error(), "hard-link") {
		t.Fatalf("write with a hard-link alias was not rejected: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := os.ReadFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, original) || !bytes.Equal(original, linked) {
		t.Fatal("rejected write caused hard-link aliases to diverge")
	}
	if _, err = NewFileStore(alias); err == nil {
		t.Fatal("second same-process writer opened a hard-link alias")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = NewFileStore(alias); err == nil || !strings.Contains(err.Error(), "different path") {
		t.Fatalf("stale alias opened after the canonical path was removed: %v", err)
	}
}

func TestFileStoreHardlinkAliasRejectedAcrossProcesses(t *testing.T) {
	if os.Getenv("ORKA_FILESTORE_HARDLINK_HELPER") == "1" {
		store, err := NewFileStore(os.Getenv("ORKA_FILESTORE_HARDLINK_PATH"))
		if err == nil {
			_ = store.Close()
			os.Exit(42)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("FileStore locking is unsupported on Windows")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	_, _, authorization := newSynthetic()
	if _, err = store.Create(authorization.Context); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(path), "alias.json")
	if err = os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestFileStoreHardlinkAliasRejectedAcrossProcesses$")
	command.Env = append(os.Environ(), "ORKA_FILESTORE_HARDLINK_HELPER=1", "ORKA_FILESTORE_HARDLINK_PATH="+alias)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helper did not reject hard-link alias: %v\n%s", err, output)
	}
}

func TestFileStoreFailsClosedWhenDirectorySyncLeavesDurabilityUncertain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FileStore locking is unsupported on Windows")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, authorization := newSynthetic()
	syncFailure := errors.New("injected directory sync failure")
	store.syncDirectory = func(string) error { return syncFailure }
	created, err := store.Create(authorization.Context)
	if !errors.Is(err, syncFailure) || !strings.Contains(err.Error(), "durability is uncertain") {
		t.Fatalf("Create error = %v, want uncertain durability wrapping injected failure", err)
	}
	cached := store.attempts[authorization.Context.AttemptID]
	if cached.Version != created.Version {
		t.Fatalf("committed Create version rolled back in cache: cached=%d returned=%d", cached.Version, created.Version)
	}
	if _, err = store.Create(authorization.Context); !errors.Is(err, syncFailure) {
		t.Fatalf("retry on uncertain store succeeded: %v", err)
	}
	if _, err = store.Get(authorization.Context.AttemptID); !errors.Is(err, syncFailure) {
		t.Fatalf("Get on uncertain store succeeded: %v", err)
	}
	if attempts := store.List(); attempts != nil {
		t.Fatalf("List exposed uncertain state: %#v", attempts)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("committed Create missing from disk: %v", err)
	}
	reopened.syncDirectory = func(string) error { return syncFailure }
	updated, err := reopened.CompareAndSwap(cached.Context.AttemptID, cached.Version, func(*Attempt) error { return nil })
	if !errors.Is(err, syncFailure) || !strings.Contains(err.Error(), "durability is uncertain") {
		t.Fatalf("CompareAndSwap error = %v, want uncertain durability wrapping injected failure", err)
	}
	cached = reopened.attempts[cached.Context.AttemptID]
	if cached.Version != updated.Version || cached.Version != created.Version+1 {
		t.Fatalf("committed CAS rolled back in cache: created=%d updated=%d cached=%d", created.Version, updated.Version, cached.Version)
	}
	if _, err = reopened.CompareAndSwap(cached.Context.AttemptID, cached.Version, func(*Attempt) error { return nil }); !errors.Is(err, syncFailure) {
		t.Fatalf("CAS retry on uncertain store succeeded: %v", err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}

	finalStore, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("committed CAS missing from disk: %v", err)
	}
	defer func() {
		if closeErr := finalStore.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	persisted, err := finalStore.Get(cached.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Version != cached.Version {
		t.Fatalf("disk/cache divergence after post-rename error: disk=%d cache=%d", persisted.Version, cached.Version)
	}
}
