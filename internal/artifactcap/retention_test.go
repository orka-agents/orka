package artifactcap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollectorRetiresOnlyAfterGraceAndPreservesLateLiveReferences(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("r", MinSecretBytes))
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	service, err := NewService(ServiceConfig{Root: root, Secret: secret, MaxObjectBytes: 1 << 20, Now: nowFn})
	if err != nil {
		t.Fatal(err)
	}
	collector, err := NewCollector(CollectorConfig{Root: root, Now: nowFn})
	if err != nil {
		t.Fatal(err)
	}
	taskA := Identity{Namespace: "default", TaskID: "task-a"}
	taskB := Identity{Namespace: "default", TaskID: "task-b"}
	unique := []byte("owned only by task a")
	shared := []byte("shared content")
	uploadArtifactForIdentity(t, service, secret, now, taskA, unique, "task-a-unique")
	uploadArtifactForIdentity(t, service, secret, now, taskA, shared, "task-a-shared")

	staleDownload := operationRequestForIdentity(OperationDownload, taskA, unique, "task-a-stale-download")
	staleAuthorization := issueTestAuthorization(t, secret, staleDownload, now)
	lateLiveDownload := operationRequestForIdentity(OperationDownload, taskB, shared, "task-b-download")
	retiredAt := now
	if err := collector.Retire(context.Background(), taskA); err != nil {
		t.Fatal(err)
	}

	// Retirement blocks stale capabilities immediately but keeps bytes and replay
	// records through the capability grace window.
	assertArtifactPresence(t, root, DigestBytes(unique), true)
	assertArtifactPresence(t, root, DigestBytes(shared), true)
	assertOperationPresence(t, root, "task-a-unique", true)
	assertOperationPresence(t, root, "task-a-shared", true)
	if _, err := service.OpenDownload(context.Background(), staleAuthorization.Capability, present(staleDownload, staleAuthorization)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("retired identity download error = %v, want ErrUnauthorized", err)
	}

	// A different live identity can receive a capability late in the retirement
	// grace window. Its persisted reservation keeps the shared digest live even
	// though the transfer has not started yet.
	now = retiredAt.Add(4 * time.Minute)
	lateLiveAuthorization, err := Issue(secret, lateLiveDownload, now, MaxCapabilityTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := collector.Reserve(context.Background(), lateLiveDownload, now.Add(MaxCapabilityTTL+MaxClockSkew)); err != nil {
		t.Fatal(err)
	}
	collector.lockTimeout = time.Second
	now = retiredAt.Add(MinimumRetirementGrace + time.Second)
	if err := collector.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertArtifactPresence(t, root, DigestBytes(unique), false)
	assertArtifactPresence(t, root, DigestBytes(shared), true)
	assertOperationPresence(t, root, "task-a-unique", false)
	assertOperationPresence(t, root, "task-a-shared", true)
	assertOperationPresence(t, root, "task-b-download", false)

	// Starting the reserved transfer creates the live replay reference that takes
	// over once the capability reservation expires.
	download, err := service.OpenDownload(context.Background(), lateLiveAuthorization.Capability, present(lateLiveDownload, lateLiveAuthorization))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(download)
	if err != nil {
		t.Fatal(err)
	}
	if err := download.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, shared) {
		t.Fatalf("late live download = %q, want %q", got, shared)
	}

	now = retiredAt.Add(4*time.Minute + MaxCapabilityTTL + MaxClockSkew + time.Second)
	if err := collector.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertArtifactPresence(t, root, DigestBytes(shared), true)
	assertOperationPresence(t, root, "task-a-shared", false)
	assertOperationPresence(t, root, "task-b-download", true)

	taskBRetiredAt := now
	if err := collector.Retire(context.Background(), taskB); err != nil {
		t.Fatal(err)
	}
	now = taskBRetiredAt.Add(MinimumRetirementGrace + time.Second)
	if err := collector.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertArtifactPresence(t, root, DigestBytes(shared), false)
	assertOperationPresence(t, root, "task-b-download", false)
	retirements, err := os.ReadDir(filepath.Join(root, "retirements"))
	if err != nil {
		t.Fatal(err)
	}
	if len(retirements) != 0 {
		t.Fatalf("expired retirement records = %v, want none", retirements)
	}
}

func TestCollectorKeepsOneReclamationAnchorPerReservedDigest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("q", MinSecretBytes))
	now := time.Date(2026, 7, 29, 12, 15, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	service, err := NewService(ServiceConfig{Root: root, Secret: secret, MaxObjectBytes: 1 << 20, Now: nowFn})
	if err != nil {
		t.Fatal(err)
	}
	collector, err := NewCollector(CollectorConfig{Root: root, Now: nowFn})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("multiply referenced content")
	ownerA := Identity{Namespace: "default", TaskID: "owner-a"}
	ownerB := Identity{Namespace: "default", TaskID: "owner-b"}
	uploadArtifactForIdentity(t, service, secret, now, ownerA, data, "owner-a-upload")
	uploadArtifactForIdentity(t, service, secret, now, ownerB, data, "owner-b-upload")
	retiredAt := now
	if err := collector.Retire(context.Background(), ownerA, ownerB); err != nil {
		t.Fatal(err)
	}
	now = retiredAt.Add(4 * time.Minute)
	reader := Identity{Namespace: "default", TaskID: "future-reader"}
	request := operationRequestForIdentity(OperationDownload, reader, data, "future-reader-download")
	if err := collector.Reserve(context.Background(), request, now.Add(MaxCapabilityTTL+MaxClockSkew)); err != nil {
		t.Fatal(err)
	}
	now = retiredAt.Add(MinimumRetirementGrace + time.Second)
	if err := collector.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	operations, err := os.ReadDir(filepath.Join(root, "operations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("retired replay anchors = %d, want one per reserved digest", len(operations))
	}
	retirements, err := os.ReadDir(filepath.Join(root, "retirements"))
	if err != nil {
		t.Fatal(err)
	}
	if len(retirements) != 1 {
		t.Fatalf("blocked retirement tombstones = %d, want one per reserved digest", len(retirements))
	}
}

func TestCollectorRejectsDownloadReservationAfterReclamation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("m", MinSecretBytes))
	now := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)
	service, err := NewService(ServiceConfig{Root: root, Secret: secret, MaxObjectBytes: 1 << 20, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	collector, err := NewCollector(CollectorConfig{Root: root, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	owner := Identity{Namespace: "default", TaskID: "deleted-owner"}
	data := []byte("reclaimed artifact")
	uploadArtifactForIdentity(t, service, secret, now, owner, data, "deleted-owner-upload")
	if err := collector.Retire(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	now = now.Add(MinimumRetirementGrace + time.Second)
	if err := collector.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := operationRequestForIdentity(OperationDownload, Identity{Namespace: "default", TaskID: "late-reader"}, data, "late-reader-download")
	if err := collector.Reserve(context.Background(), request, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reserve after object reclamation error = %v, want ErrNotFound", err)
	}
}

func TestCollectorRetirementLockWaitIsBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("w", MinSecretBytes))
	now := time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC)
	service, err := NewService(ServiceConfig{Root: root, Secret: secret, MaxObjectBytes: 1 << 20, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	collector, err := NewCollector(CollectorConfig{Root: root, LockTimeout: 25 * time.Millisecond, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{Namespace: "default", PublicationID: "publication-in-flight"}
	data := []byte("in-flight artifact")
	request := operationRequestForIdentity(OperationUpload, identity, data, "in-flight-upload")
	authorization := issueTestAuthorization(t, secret, request, now)
	reader := &gatedReader{data: data, entered: make(chan struct{}), release: make(chan struct{})}
	uploadDone := make(chan error, 1)
	go func() {
		_, err := service.Upload(context.Background(), authorization.Capability, present(request, authorization), reader)
		uploadDone <- err
	}()
	<-reader.entered

	retireDone := make(chan error, 1)
	go func() { retireDone <- collector.Retire(context.Background(), identity) }()
	select {
	case err := <-retireDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Retire while upload held storage lock = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Retire did not honor bounded lock timeout")
	}
	close(reader.release)
	if err := <-uploadDone; err != nil {
		t.Fatal(err)
	}
	if err := collector.Retire(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	assertArtifactPresence(t, root, request.ObjectDigest, true)
	assertOperationPresence(t, root, request.OperationID, true)

	collector.lockTimeout = time.Second
	now = now.Add(MinimumRetirementGrace + time.Second)
	if err := collector.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertArtifactPresence(t, root, request.ObjectDigest, false)
	assertOperationPresence(t, root, request.OperationID, false)
}

func TestCollectorWriterIntentBlocksNewTransfers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := []byte(strings.Repeat("f", MinSecretBytes))
	now := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	service, err := NewService(ServiceConfig{Root: root, Secret: secret, MaxObjectBytes: 1 << 20, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	collector, err := NewCollector(CollectorConfig{Root: root, LockTimeout: time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	retiredIdentity := Identity{Namespace: "default", TaskID: "retired-writer"}
	firstData := []byte("first transfer")
	firstRequest := operationRequestForIdentity(OperationUpload, retiredIdentity, firstData, "first-transfer")
	firstAuthorization := issueTestAuthorization(t, secret, firstRequest, now)
	reader := &gatedReader{data: firstData, entered: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Upload(context.Background(), firstAuthorization.Capability, present(firstRequest, firstAuthorization), reader)
		firstDone <- err
	}()
	<-reader.entered

	retireDone := make(chan error, 1)
	go func() { retireDone <- collector.Retire(context.Background(), retiredIdentity) }()
	waitForArtifactWriterIntent(t, root)

	liveIdentity := Identity{Namespace: "default", TaskID: "live-reader"}
	secondData := []byte("second transfer")
	secondRequest := operationRequestForIdentity(OperationUpload, liveIdentity, secondData, "second-transfer")
	secondAuthorization := issueTestAuthorization(t, secret, secondRequest, now)
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.Upload(context.Background(), secondAuthorization.Capability, present(secondRequest, secondAuthorization), bytes.NewReader(secondData))
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("new transfer bypassed queued retention writer: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(reader.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-retireDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func waitForArtifactWriterIntent(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value, ok := artifactRootProcessGates.Load(root); ok {
			gate := value.(*artifactRootProcessGate)
			if !gate.semaphore.TryAcquire(1) {
				return
			}
			gate.semaphore.Release(1)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("retention writer did not queue behind active transfer")
}

func TestArtifactWriterIntentBlocksReadersAcrossProcessGate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_, releaseIntent, err := acquireArtifactWriterIntent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := acquireArtifactRootLock(ctx, root, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shared lock with writer intent error = %v, want deadline exceeded", err)
	}
	if err := releaseIntent(); err != nil {
		t.Fatal(err)
	}
	release, err := acquireArtifactRootLock(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactWriterIntentQueueIsFIFO(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	firstPath, releaseFirst, err := acquireArtifactWriterIntent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	secondPath, releaseSecond, err := acquireArtifactWriterIntent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond() //nolint:errcheck
	if err := waitForArtifactWriterTurn(context.Background(), root, firstPath); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := waitForArtifactWriterTurn(ctx, root, secondPath); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second writer turn error = %v, want deadline exceeded while first owns turn", err)
	}
	if err := releaseFirst(); err != nil {
		t.Fatal(err)
	}
	if err := waitForArtifactWriterTurn(context.Background(), root, secondPath); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactManagedFilesRepairFSGroupPermissions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	_, releaseIntent, err := acquireArtifactWriterIntent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseIntent(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, artifactWriterSequenceName),
		filepath.Join(root, artifactWriterSequenceLockName),
	} {
		if err := os.Chmod(path, 0o660); err != nil {
			t.Fatal(err)
		}
	}

	intentPath, releaseIntent, err := acquireArtifactWriterIntent(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseIntent != nil {
			_ = releaseIntent()
		}
	})
	for _, path := range []string{
		filepath.Join(root, artifactWriterSequenceName),
		filepath.Join(root, artifactWriterSequenceLockName),
	} {
		assertArtifactManagedFileMode(t, path)
	}
	if err := os.Chmod(intentPath, 0o660); err != nil {
		t.Fatal(err)
	}
	queue, err := artifactWriterQueue(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].path != intentPath {
		t.Fatalf("writer queue = %#v, want active intent %q", queue, intentPath)
	}
	assertArtifactManagedFileMode(t, intentPath)
	if err := releaseIntent(); err != nil {
		t.Fatal(err)
	}
	releaseIntent = nil

	lockPath := filepath.Join(root, artifactRootLockName)
	if err := os.WriteFile(lockPath, nil, 0o660); err != nil {
		t.Fatal(err)
	}
	releaseLock, err := acquireArtifactRootLock(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseLock(); err != nil {
		t.Fatal(err)
	}
	assertArtifactManagedFileMode(t, lockPath)
}

func TestCollectorRepairsFSGroupManagedFilesAcrossLifecycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	collector, err := NewCollector(CollectorConfig{Root: root, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{Namespace: "orka-system", TaskID: "task-fsgroup-restart"}
	reserve := func(operationID string) {
		t.Helper()
		request := operationRequestForIdentity(OperationUpload, identity, []byte(operationID), operationID)
		if err := collector.Reserve(context.Background(), request, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	managedPaths := []string{
		filepath.Join(root, artifactRootLockName),
		filepath.Join(root, artifactWriterSequenceName),
		filepath.Join(root, artifactWriterSequenceLockName),
	}
	chmodManaged := func() {
		t.Helper()
		for _, path := range managedPaths {
			if err := os.Chmod(path, 0o660); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertRepaired := func() {
		t.Helper()
		for _, path := range managedPaths {
			assertArtifactManagedFileMode(t, path)
		}
	}

	reserve("reserve-before-restart")
	chmodManaged()
	reserve("reserve-after-restart")
	assertRepaired()
	chmodManaged()
	if err := collector.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertRepaired()
	chmodManaged()
	if err := collector.Retire(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	assertRepaired()
}

func TestArtifactManagedFilesRejectWorldPermissions(t *testing.T) {
	t.Parallel()
	createWithMode := func(t *testing.T, path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("sequence lock", func(t *testing.T) {
		root := t.TempDir()
		createWithMode(t, filepath.Join(root, artifactWriterSequenceLockName), nil, 0o606)
		if _, _, err := acquireArtifactWriterIntent(context.Background(), root); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("writer intent error = %v, want ErrUnsafePath", err)
		}
	})
	t.Run("sequence", func(t *testing.T) {
		root := t.TempDir()
		createWithMode(t, filepath.Join(root, artifactWriterSequenceLockName), nil, 0o600)
		createWithMode(t, filepath.Join(root, artifactWriterSequenceName), []byte("1\n"), 0o606)
		if _, _, err := acquireArtifactWriterIntent(context.Background(), root); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("writer intent error = %v, want ErrUnsafePath", err)
		}
	})
	t.Run("root lock", func(t *testing.T) {
		root := t.TempDir()
		createWithMode(t, filepath.Join(root, artifactRootLockName), nil, 0o606)
		if _, err := acquireArtifactRootLock(context.Background(), root, false); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("root lock error = %v, want ErrUnsafePath", err)
		}
	})
	t.Run("intent", func(t *testing.T) {
		root := t.TempDir()
		name := artifactWriterIntentPrefix + "00000000000000000001-test" + artifactWriterIntentSuffix
		createWithMode(t, filepath.Join(root, name), []byte("1\n"), 0o606)
		if _, err := artifactWriterQueue(root); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("writer queue error = %v, want ErrUnsafePath", err)
		}
	})
}

func TestArtifactManagedFilesRejectSymlinkAndNonRegularEntries(t *testing.T) {
	t.Parallel()
	t.Run("sequence lock symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, artifactWriterSequenceLockName)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := acquireArtifactWriterIntent(context.Background(), root); err == nil {
			t.Fatal("writer intent accepted a symlink sequence lock")
		}
	})
	t.Run("sequence symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, []byte("1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, artifactWriterSequenceLockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, artifactWriterSequenceName)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := acquireArtifactWriterIntent(context.Background(), root); err == nil {
			t.Fatal("writer intent accepted a symlink sequence")
		}
	})
	t.Run("root lock symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, artifactRootLockName)); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireArtifactRootLock(context.Background(), root, false); err == nil {
			t.Fatal("root lock accepted a symlink lock file")
		}
	})
	t.Run("sequence lock directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, artifactWriterSequenceLockName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := acquireArtifactWriterIntent(context.Background(), root); err == nil {
			t.Fatal("writer intent accepted a non-regular sequence lock")
		}
	})
	t.Run("sequence directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, artifactWriterSequenceLockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, artifactWriterSequenceName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, _, err := acquireArtifactWriterIntent(context.Background(), root); err == nil {
			t.Fatal("writer intent accepted a non-regular sequence")
		}
	})
	t.Run("root lock directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, artifactRootLockName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireArtifactRootLock(context.Background(), root, false); err == nil {
			t.Fatal("root lock accepted a non-regular lock file")
		}
	})
	t.Run("intent symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, []byte("1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		name := artifactWriterIntentPrefix + "00000000000000000001-test" + artifactWriterIntentSuffix
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := artifactWriterQueue(root); err == nil {
			t.Fatal("writer queue accepted a symlink intent entry")
		}
	})
	t.Run("intent directory", func(t *testing.T) {
		root := t.TempDir()
		name := artifactWriterIntentPrefix + "00000000000000000001-test" + artifactWriterIntentSuffix
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := artifactWriterQueue(root); err == nil {
			t.Fatal("writer queue accepted a non-regular intent entry")
		}
	})
}

func assertArtifactManagedFileMode(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %04o, want 0600", path, got)
	}
}

func uploadArtifactForIdentity(t *testing.T, service *Service, secret []byte, now time.Time, identity Identity, data []byte, operationID string) {
	t.Helper()
	request := operationRequestForIdentity(OperationUpload, identity, data, operationID)
	authorization := issueTestAuthorization(t, secret, request, now)
	if _, err := service.Upload(context.Background(), authorization.Capability, present(request, authorization), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func operationRequestForIdentity(operation Operation, identity Identity, data []byte, operationID string) OperationRequest {
	return OperationRequest{
		Operation: operation, ObjectDigest: DigestBytes(data), Identity: identity,
		ContentLength: int64(len(data)), MediaType: "application/octet-stream", OperationID: operationID,
	}
}

func assertArtifactPresence(t *testing.T, root, digest string, want bool) {
	t.Helper()
	hexDigest, err := DigestHex(digest)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "objects", hexDigest), filepath.Join(root, "metadata", hexDigest+".json")} {
		_, err := os.Stat(path)
		if want && err != nil {
			t.Fatalf("expected artifact path %s: %v", path, err)
		}
		if !want && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retired artifact path %s still exists or returned unexpected error: %v", path, err)
		}
	}
}

func assertOperationPresence(t *testing.T, root, operationID string, want bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, "operations", operationRecordName(operationID)))
	if want && err != nil {
		t.Fatalf("expected operation %q: %v", operationID, err)
	}
	if !want && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired operation %q still exists or returned unexpected error: %v", operationID, err)
	}
}

type gatedReader struct {
	data    []byte
	entered chan struct{}
	release chan struct{}
	done    bool
}

func (r *gatedReader) Read(buffer []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	close(r.entered)
	<-r.release
	return copy(buffer, r.data), nil
}
