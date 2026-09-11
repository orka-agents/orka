package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testProviderTokenOld  = "0123456789abcdef0123456789abcdef"
	testProviderTokenNext = "11111111111111111111111111111111"
	testProviderTokenNew  = "22222222222222222222222222222222"
)

func TestTokenFileReloaderSupportsLegacySingleTokenFile(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, testProviderTokenOld)

	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, time.Minute)
	if err := reloader.reload(); err != nil {
		t.Fatalf("reload legacy token file: %v", err)
	}
	if !store.isReady() || !tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("legacy single-token file was not accepted")
	}
	if tokenAuthorized(store, testProviderTokenNext) {
		t.Fatal("unconfigured token was accepted")
	}
}

func TestTokenFileReloaderTrimsMountedCurrentAndPreviousTokens(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, "\n"+testProviderTokenNew+"\r\n")
	writeTokenFile(t, previousPath, "\t"+testProviderTokenOld+"\n")
	writeTokenDeadline(t, validUntilPath, now.Add(time.Minute))

	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, time.Minute)
	if err := reloader.reload(); err != nil {
		t.Fatalf("reload newline-terminated mounted tokens: %v", err)
	}
	if !tokenAuthorized(store, testProviderTokenNew) || !tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("trimmed current and previous mounted tokens were not accepted")
	}
	if tokenAuthorized(store, "\n"+testProviderTokenNew) || tokenAuthorized(store, testProviderTokenOld+"\n") {
		t.Fatal("authorization accepted whitespace as part of a bearer token")
	}
}

func TestTokenFileReloaderFailsClosedWhenCurrentFileDisappears(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, testProviderTokenOld)

	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, time.Minute)
	if err := reloader.reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	if err := os.Remove(currentPath); err != nil {
		t.Fatalf("remove current token file: %v", err)
	}
	if err := reloader.reload(); err == nil {
		t.Fatal("reload succeeded after current token file disappeared")
	}
	if store.isReady() || tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("missing current token file did not disable authentication")
	}
}

func TestTokenFileReloaderFailsClosedAndRecovers(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, testProviderTokenOld)

	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, time.Minute)
	if err := reloader.reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	writeTokenFile(t, previousPath, "malformed token material that must never be logged")
	writeTokenDeadline(t, validUntilPath, now.Add(time.Minute))
	if err := reloader.reload(); err == nil {
		t.Fatal("malformed reload succeeded")
	}
	if store.isReady() || tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("last-known token remained active after malformed reload")
	}

	proxy, err := newProviderAuthProxyWithTokenStore(proxyConfig{UpstreamBaseURL: "http://upstream.example"}, store)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	assertProxyStatus(t, proxy, readinessPath, "", http.StatusServiceUnavailable)
	assertProxyStatus(t, proxy, healthPath, "", http.StatusOK)
	assertProxyStatus(t, proxy, "/v1/models", testProviderTokenOld, http.StatusUnauthorized)

	now = now.Add(time.Second)
	writeTokenFile(t, previousPath, testProviderTokenNext)
	writeTokenDeadline(t, validUntilPath, now.Add(time.Minute))
	if err := reloader.reload(); err != nil {
		t.Fatalf("recovery reload: %v", err)
	}
	if !store.isReady() || !tokenAuthorized(store, testProviderTokenOld) || !tokenAuthorized(store, testProviderTokenNext) {
		t.Fatal("valid credentials did not recover authentication")
	}

	if err := os.Remove(validUntilPath); err != nil {
		t.Fatalf("remove validity file: %v", err)
	}
	if err := reloader.reload(); err == nil {
		t.Fatal("previous token without an absolute validity file succeeded")
	}
	if store.isReady() {
		t.Fatal("incomplete previous-token pair did not fail closed")
	}
}

func TestPreviousTokenOverlapExpiresWithoutExtensionAcrossRestart(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, testProviderTokenNew)
	writeTokenFile(t, previousPath, testProviderTokenOld)

	overlap := 2 * time.Minute
	validUntil := now.Add(overlap)
	writeTokenDeadline(t, validUntilPath, validUntil)
	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, overlap)
	if err := reloader.reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	if !tokenAuthorized(store, testProviderTokenOld) || !tokenAuthorized(store, testProviderTokenNew) {
		t.Fatal("current and previous tokens were not both accepted during overlap")
	}

	now = validUntil.Add(time.Nanosecond)
	if tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("previous token remained accepted after absolute deadline")
	}
	if err := reloader.reload(); err != nil {
		t.Fatalf("unchanged reload: %v", err)
	}
	if tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("periodic reload extended an unchanged previous token")
	}
	if !tokenAuthorized(store, testProviderTokenNew) {
		t.Fatal("current token expired with previous token")
	}

	// Recreate every mounted file to model a fresh Pod/projected-volume
	// materialization while preserving the same absolute expiry metadata.
	writeTokenFile(t, currentPath, testProviderTokenNew)
	writeTokenFile(t, previousPath, testProviderTokenOld)
	writeTokenDeadline(t, validUntilPath, validUntil)
	restartedStore, restartedReloader := newTestTokenFileReloader(
		t,
		&now,
		currentPath,
		previousPath,
		validUntilPath,
		overlap,
	)
	if err := restartedReloader.reload(); err != nil {
		t.Fatalf("restart reload: %v", err)
	}
	if tokenAuthorized(restartedStore, testProviderTokenOld) {
		t.Fatal("process restart extended an expired previous token")
	}
}

func TestPreviousTokenDeadlineCannotExceedConfiguredOverlap(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, testProviderTokenNew)
	writeTokenFile(t, previousPath, testProviderTokenOld)
	writeTokenDeadline(t, validUntilPath, now.Add(time.Minute+time.Second))

	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, time.Minute)
	if err := reloader.reload(); err == nil {
		t.Fatal("overlong previous-token deadline succeeded")
	}
	if store.isReady() {
		t.Fatal("overlong previous-token deadline did not fail closed")
	}
}

func TestTokenFileReloaderReadsOneProjectedSecretGeneration(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	overlap := 5 * time.Minute

	firstGeneration := filepath.Join(directory, "..2026_07_25_01")
	if err := os.Mkdir(firstGeneration, 0o700); err != nil {
		t.Fatalf("create first projected generation: %v", err)
	}
	writeTokenFile(t, filepath.Join(firstGeneration, "token"), testProviderTokenOld)
	switchProjectedGeneration(t, directory, filepath.Base(firstGeneration))
	if err := os.Symlink(filepath.Join("..data", "token"), currentPath); err != nil {
		t.Fatalf("create visible current-token symlink: %v", err)
	}

	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, overlap)
	if err := reloader.reload(); err != nil {
		t.Fatalf("initial projected reload: %v", err)
	}

	secondGeneration := filepath.Join(directory, "..2026_07_25_02")
	if err := os.Mkdir(secondGeneration, 0o700); err != nil {
		t.Fatalf("create second projected generation: %v", err)
	}
	writeTokenFile(t, filepath.Join(secondGeneration, "token"), testProviderTokenNew)
	writeTokenFile(t, filepath.Join(secondGeneration, "previous-token"), testProviderTokenOld)
	writeTokenDeadline(t, filepath.Join(secondGeneration, "previous-token-valid-until"), now.Add(overlap))
	switchProjectedGeneration(t, directory, filepath.Base(secondGeneration))

	// Kubernetes publishes ..data before creating visible symlinks for newly
	// added keys. Reload must still read the complete selected generation.
	if _, err := os.Lstat(previousPath); !os.IsNotExist(err) {
		t.Fatalf("previous-token visible path unexpectedly exists: %v", err)
	}
	if err := reloader.reload(); err != nil {
		t.Fatalf("rotated projected reload: %v", err)
	}
	if !tokenAuthorized(store, testProviderTokenNew) || !tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("projected generation was not published as one credential pair")
	}
}

func TestReadStableTokenFilesRetriesProjectedGenerationResolution(t *testing.T) {
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	if err := os.Symlink("..missing-generation", filepath.Join(directory, "..data")); err != nil {
		t.Fatalf("create dangling projected generation: %v", err)
	}
	generation := filepath.Join(directory, "..2026_07_25_retry")
	if err := os.Mkdir(generation, 0o700); err != nil {
		t.Fatalf("create replacement projected generation: %v", err)
	}
	writeTokenFile(t, filepath.Join(generation, "token"), testProviderTokenNew)

	waits := 0
	current, previous, validUntil, err := readStableTokenFilesWithWait(
		currentPath,
		previousPath,
		validUntilPath,
		func(int) {
			waits++
			if waits == 1 {
				switchProjectedGeneration(t, directory, filepath.Base(generation))
			}
		},
	)
	if err != nil {
		t.Fatalf("read replacement projected generation: %v", err)
	}
	defer clear(current.contents)
	defer clear(previous.contents)
	defer clear(validUntil.contents)
	if waits != 1 {
		t.Fatalf("retry waits = %d, want 1", waits)
	}
	if string(current.contents) != testProviderTokenNew || previous.present || validUntil.present {
		t.Fatal("retry did not return the complete replacement generation")
	}
}

func TestTokenRotationProxyFirst(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, testProviderTokenOld)

	overlap := 5 * time.Minute
	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, overlap)
	if err := reloader.reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	now = now.Add(time.Minute)
	validUntil := now.Add(overlap)
	writeTokenFile(t, currentPath, testProviderTokenNew)
	writeTokenFile(t, previousPath, testProviderTokenOld)
	writeTokenDeadline(t, validUntilPath, validUntil)
	if err := reloader.reload(); err != nil {
		t.Fatalf("proxy-first reload: %v", err)
	}
	if !tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("old controller token was rejected after proxy-first rotation")
	}
	if !tokenAuthorized(store, testProviderTokenNew) {
		t.Fatal("new controller token was rejected after proxy-first rotation")
	}

	now = validUntil.Add(time.Nanosecond)
	if tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("old token remained accepted after proxy-first overlap")
	}
}

func TestTokenRotationControllerFirstWithPreloadedOverlapToken(t *testing.T) {
	now := time.Now().UTC()
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, testProviderTokenOld)

	overlap := 5 * time.Minute
	store, reloader := newTestTokenFileReloader(t, &now, currentPath, previousPath, validUntilPath, overlap)
	if err := reloader.reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	if tokenAuthorized(store, testProviderTokenNext) {
		t.Fatal("unstaged next token was accepted")
	}

	// Pre-stage the next controller token in the overlap slot, then switch the
	// controller before changing which token is designated current by the proxy.
	now = now.Add(time.Minute)
	writeTokenFile(t, previousPath, testProviderTokenNext)
	writeTokenDeadline(t, validUntilPath, now.Add(overlap))
	if err := reloader.reload(); err != nil {
		t.Fatalf("pre-stage next token: %v", err)
	}
	if !tokenAuthorized(store, testProviderTokenOld) || !tokenAuthorized(store, testProviderTokenNext) {
		t.Fatal("pre-staged controller-first tokens were not both accepted")
	}

	now = now.Add(time.Minute)
	validUntil := now.Add(overlap)
	writeTokenFile(t, currentPath, testProviderTokenNext)
	writeTokenFile(t, previousPath, testProviderTokenOld)
	writeTokenDeadline(t, validUntilPath, validUntil)
	if !tokenAuthorized(store, testProviderTokenNext) {
		t.Fatal("controller-first request failed before the proxy observed the role swap")
	}
	if err := reloader.reload(); err != nil {
		t.Fatalf("normalize controller-first token roles: %v", err)
	}
	if !tokenAuthorized(store, testProviderTokenNext) || !tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("normalized controller-first tokens were not both accepted")
	}

	now = validUntil.Add(time.Nanosecond)
	if tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("old token remained accepted after controller-first overlap")
	}
}

func TestBearerTokenStorePublishesCredentialPairsAtomically(t *testing.T) {
	now := time.Now().UTC()
	store := newBearerTokenStore(func() time.Time { return now })
	validUntil := now.Add(time.Hour)
	store.activate([]byte(testProviderTokenOld), []byte(testProviderTokenNext), validUntil)

	oldDigest := sha256.Sum256([]byte(testProviderTokenOld))
	nextDigest := sha256.Sum256([]byte(testProviderTokenNext))
	newDigest := sha256.Sum256([]byte(testProviderTokenNew))
	var invalid atomic.Bool
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 10_000 {
				active := store.snapshot.Load()
				oldPair := active.currentDigest == oldDigest && active.previousDigest == nextDigest
				newPair := active.currentDigest == newDigest && active.previousDigest == oldDigest
				if !active.ready || !active.hasPrevious || (!oldPair && !newPair) {
					invalid.Store(true)
					return
				}
			}
		})
	}
	for range 10_000 {
		store.activate([]byte(testProviderTokenNew), []byte(testProviderTokenOld), validUntil)
		store.activate([]byte(testProviderTokenOld), []byte(testProviderTokenNext), validUntil)
	}
	readers.Wait()
	if invalid.Load() {
		t.Fatal("reader observed a partially published credential pair")
	}
}

func TestPeriodicTokenReloadDoesNotLogTokenMaterial(t *testing.T) {
	directory := t.TempDir()
	currentPath := filepath.Join(directory, "token")
	previousPath := filepath.Join(directory, "previous-token")
	validUntilPath := filepath.Join(directory, "previous-token-valid-until")
	writeTokenFile(t, currentPath, testProviderTokenOld)

	store := newBearerTokenStore(time.Now)
	reloader, err := newTokenFileReloader(tokenFileReloaderConfig{
		CurrentTokenFile:            currentPath,
		PreviousTokenFile:           previousPath,
		PreviousTokenValidUntilFile: validUntilPath,
		ReloadInterval:              5 * time.Millisecond,
		PreviousTokenOverlap:        time.Minute,
	}, store)
	if err != nil {
		t.Fatalf("new reloader: %v", err)
	}
	if err := reloader.reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var messagesMu sync.Mutex
	var messages []string
	go reloader.run(ctx, func(message string) {
		messagesMu.Lock()
		defer messagesMu.Unlock()
		messages = append(messages, message)
	})

	malformed := "malformed token material that must never be logged"
	writeTokenFile(t, currentPath, malformed)
	eventually(t, time.Second, func() bool { return !store.isReady() })
	if tokenAuthorized(store, testProviderTokenOld) {
		t.Fatal("old token remained accepted after periodic malformed reload")
	}

	writeTokenFile(t, currentPath, testProviderTokenNew)
	eventually(t, time.Second, func() bool {
		return store.isReady() && tokenAuthorized(store, testProviderTokenNew)
	})

	eventually(t, time.Second, func() bool {
		messagesMu.Lock()
		defer messagesMu.Unlock()
		joined := strings.Join(messages, "\n")
		return strings.Contains(joined, "reload failed") && strings.Contains(joined, "reload recovered")
	})
	cancel()
	messagesMu.Lock()
	joined := strings.Join(messages, "\n")
	messagesMu.Unlock()
	for _, secret := range []string{malformed, testProviderTokenOld, testProviderTokenNew} {
		if strings.Contains(joined, secret) {
			t.Fatalf("token material appeared in reload logs: %q", joined)
		}
	}
}

func newTestTokenFileReloader(
	t *testing.T,
	now *time.Time,
	currentPath string,
	previousPath string,
	validUntilPath string,
	overlap time.Duration,
) (*bearerTokenStore, *tokenFileReloader) {
	t.Helper()
	store := newBearerTokenStore(func() time.Time { return *now })
	reloader, err := newTokenFileReloader(tokenFileReloaderConfig{
		CurrentTokenFile:            currentPath,
		PreviousTokenFile:           previousPath,
		PreviousTokenValidUntilFile: validUntilPath,
		ReloadInterval:              time.Second,
		PreviousTokenOverlap:        overlap,
	}, store)
	if err != nil {
		t.Fatalf("new token file reloader: %v", err)
	}
	return store, reloader
}

func writeTokenFile(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
}

func writeTokenDeadline(t *testing.T, path string, deadline time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(deadline.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatalf("write token deadline: %v", err)
	}
}

func switchProjectedGeneration(t *testing.T, directory, generation string) {
	t.Helper()
	temporaryLink := filepath.Join(directory, "..data_tmp")
	if err := os.Symlink(generation, temporaryLink); err != nil {
		t.Fatalf("create projected generation link: %v", err)
	}
	if err := os.Rename(temporaryLink, filepath.Join(directory, "..data")); err != nil {
		t.Fatalf("publish projected generation: %v", err)
	}
}

func tokenAuthorized(store *bearerTokenStore, token string) bool {
	return store.authorized([]string{"Bearer " + token})
}

func assertProxyStatus(t *testing.T, proxy *providerAuthProxy, path, token string, expected int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://proxy"+path, nil)
	if token != "" {
		request.Header.Set(authorizationHeader, "Bearer "+token)
	}
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != expected {
		t.Fatalf("%s status = %d, want %d", path, response.Code, expected)
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
