//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/codexstate"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
)

func migrationCommand(t *testing.T, server string, args ...string) error {
	t.Helper()
	cmd := newRootCmd()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(append([]string{"--server", server, "--namespace", "test", "--token", "test-only", "session", "migrate"}, args...))
	return cmd.Execute()
}

func TestMigrateImportRetriesFrozenRequest(t *testing.T) {
	const thread = "01a10020-1222-76e3-977d-d5165792ae72"
	const name = "rollout-2026-10-02T21-57-44-" + thread + ".jsonl"
	home, journal := t.TempDir(), t.TempDir()
	if err := os.Chmod(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions", "2026", "10", "02")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "codexstate", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	var first []byte
	var operation string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/new/native" || r.URL.Query().Get("namespace") != "test" {
			t.Errorf("request: %s %s", r.Method, r.URL)
		}
		var request struct {
			OperationID string `json:"operationID"`
			Data        []byte `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		calls++
		if calls == 1 {
			first = bytes.Clone(request.Data)
			operation = request.OperationID
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !bytes.Equal(first, request.Data) || operation != request.OperationID {
			t.Error("uncertain retry changed native request")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"operationID": operation, "namespace": "test", "sessionName": "new", "providerSessionID": thread, "dataDigest": codexstate.DataDigest(first)})
	}))
	defer server.Close()
	args := []string{"import", "new", "--codex-home", home, "--thread", thread, "--journal-dir", journal, "--source-stopped"}
	if err := migrationCommand(t, server.URL, args...); err == nil {
		t.Fatal("lost response accepted")
	}
	// The source is no longer readable; a safe retry must use its saved bundle.
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	if err := migrationCommand(t, server.URL, args...); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls: %d", calls)
	}
	info, err := os.Stat(filepath.Join(journal, "import-receipt.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private receipt: %v", err)
	}
	changed := append([]string(nil), args...)
	changed[1] = "other"
	if err := migrationCommand(t, server.URL, changed...); err == nil {
		t.Fatal("journal rebound to another Session")
	}
	if calls != 2 {
		t.Fatal("changed target reached API")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("changed explicit endpoint reached API")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer other.Close()
	if err := migrationCommand(t, other.URL, args...); err == nil {
		t.Fatal("journal rebound to another explicit endpoint")
	}
}

// A subprocess supplies the real kubectl transport boundary while the command
// still performs normal service discovery and journal reconciliation.
func TestMigratePortForwardHelper(t *testing.T) {
	if os.Getenv("ORKA_MIGRATION_TEST_FORWARD") == "" {
		return
	}
	target, err := url.Parse(os.Getenv("ORKA_MIGRATION_TEST_FORWARD"))
	require.NoError(t, err)
	port, _, found := strings.Cut(os.Args[len(os.Args)-1], ":")
	require.True(t, found)
	listener, err := net.Listen("tcp4", "127.0.0.1:"+port)
	require.NoError(t, err)
	server := http.Server{Handler: httputil.NewSingleHostReverseProxy(target), ReadHeaderTimeout: 5 * time.Second}
	require.NoError(t, server.Serve(listener))
}

func TestMigrateImportRetriesAcrossAutomaticPortChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const thread = "01a10020-1222-76e3-977d-d5165792ae72"
	const name = "rollout-2026-10-02T21-57-44-" + thread + ".jsonl"
	home, journal := t.TempDir(), t.TempDir()
	require.NoError(t, os.Chmod(journal, 0o700))
	rollout := filepath.Join(home, "sessions", "2026", "10", "02", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(rollout), 0o700))
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "codexstate", "testdata", name))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(rollout, fixture, 0o600))
	var first []byte
	var operation, firstHost, retryHost string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/new/native" {
			t.Errorf("request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var request struct {
			OperationID string `json:"operationID"`
			Data        []byte `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		calls++
		if calls == 1 {
			first, operation, firstHost = bytes.Clone(request.Data), request.OperationID, r.Host
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		retryHost = r.Host
		if !bytes.Equal(first, request.Data) || operation != request.OperationID {
			t.Error("uncertain retry changed native request")
			http.Error(w, "changed request", http.StatusConflict)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"operationID": operation, "namespace": "test", "sessionName": "new", "providerSessionID": thread, "dataDigest": codexstate.DataDigest(first)}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	t.Setenv("ORKA_MIGRATION_TEST_FORWARD", server.URL)
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("ORKA_MIGRATION_TEST_BINARY", executable)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kubectl"), []byte("#!/bin/sh\nexec \"$ORKA_MIGRATION_TEST_BINARY\" -test.run=^TestMigratePortForwardHelper$ -- \"$@\"\n"), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	serviceUID := "stable-service-uid"
	kubeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/test/services/orka-api" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"namespace": "test", "name": "orka-api", "uid": serviceUID}}); err != nil {
			t.Error(err)
		}
	})
	kube := httptest.NewServer(kubeHandler)
	defer kube.Close()
	kubeconfig := filepath.Join(t.TempDir(), "config")
	writeKubeconfig := func(cluster string) {
		t.Helper()
		require.NoError(t, os.WriteFile(kubeconfig, []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: test\ncontexts:\n- name: test\n  context:\n    cluster: test\nclusters:\n- name: test\n  cluster:\n    server: %s\n", cluster)), 0o600))
	}
	writeKubeconfig(kube.URL)
	// A cached tunnel is deliberately unrelated to the selected cluster.
	cached := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("migration reused an unbound cached tunnel")
	}))
	defer cached.Close()
	var cachedPort int
	_, err = fmt.Sscanf(cached.URL, "http://127.0.0.1:%d", &cachedPort)
	require.NoError(t, err)
	savePortForwardCache(&portForwardCache{Port: cachedPort, Service: "other-orka", Namespace: "other"})
	args := []string{"import", "new", "--codex-home", home, "--thread", thread, "--journal-dir", journal, "--source-stopped"}
	run := func() error {
		cmd := newRootCmd()
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(append([]string{"--kubeconfig", kubeconfig, "--namespace", "test", "--token", "test-only", "session", "migrate"}, args...))
		return cmd.Execute()
	}
	require.Error(t, run())
	require.Equal(t, 1, calls)
	require.NoError(t, os.Remove(rollout))
	_, firstPort, err := net.SplitHostPort(firstHost)
	require.NoError(t, err)
	var occupied net.Listener
	require.Eventually(t, func() bool {
		occupied, err = net.Listen("tcp4", "127.0.0.1:"+firstPort)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	defer func() { _ = occupied.Close() }()
	serviceUID = "replacement-service-uid"
	require.ErrorContains(t, run(), "another operation or target")
	require.Equal(t, 1, calls)
	serviceUID = "stable-service-uid"
	otherCluster := httptest.NewServer(kubeHandler)
	defer otherCluster.Close()
	writeKubeconfig(otherCluster.URL)
	require.ErrorContains(t, run(), "another operation or target")
	require.Equal(t, 1, calls)
	writeKubeconfig(kube.URL)
	require.NoError(t, run())
	require.Equal(t, 2, calls)
	require.NotEqual(t, firstHost, retryHost)
}

func TestMigrateExportInstallsAndReconcilesSamePlan(t *testing.T) {
	snapshot := storetest.NativeSessionSnapshot(t, "native-export-context")
	serverCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalls++
		if r.Method != http.MethodGet || r.URL.Query().Get("namespace") != "test" {
			t.Errorf("request: %s %s", r.Method, r.URL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": snapshot.Data, "dataDigest": snapshot.DataDigest, "providerSessionID": snapshot.ProviderSessionID})
	}))
	defer server.Close()
	home, cwd, journal := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'codex-cli 0.160.0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"export", "source", "--codex-home", home, "--cwd", cwd, "--journal-dir", journal, "--codex-bin", binary}
	if err := migrationCommand(t, server.URL, args...); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		OperationID string `json:"operationID"`
		TargetPath  string `json:"targetPath"`
	}
	data, err := os.ReadFile(filepath.Join(journal, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	rollout, err := os.ReadFile(filepath.Join(home, receipt.TargetPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rollout, []byte("native-export-context")) {
		t.Fatal("context absent from native target")
	}
	if err := migrationCommand(t, server.URL, args...); err != nil {
		t.Fatal(err)
	}
	if serverCalls != 1 {
		t.Fatalf("retry fetched a newer checkpoint: %d", serverCalls)
	}
	data, err = os.ReadFile(filepath.Join(journal, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(receipt.OperationID)) {
		t.Fatal("retry changed install plan")
	}
}

func TestMigrateRequiresStoppedSourceAndPrivateJournal(t *testing.T) {
	if err := migrationCommand(t, "http://unused", "import", "new", "--codex-home", t.TempDir(), "--thread", "id", "--journal-dir", t.TempDir()); err == nil {
		t.Fatal("active source was accepted")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := privateMigrationJournal(dir, t.TempDir()); err == nil {
		t.Fatal("public journal accepted")
	}
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := privateMigrationJournal(filepath.Join(home, "journal"), canonicalHome); err == nil {
		t.Fatal("journal overlaps home")
	}
}

func TestMigrateExportRejectsPublicHomeBeforeAPI(t *testing.T) {
	serverCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'codex-cli 0.160.0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{home, link} {
		if err := migrationCommand(t, server.URL, "export", "source", "--codex-home", destination, "--cwd", t.TempDir(), "--journal-dir", t.TempDir(), "--codex-bin", binary); err == nil {
			t.Fatal("public destination accepted")
		}
	}
	if serverCalls != 0 {
		t.Fatal("public destination reached the API")
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected destination changed: %v", err)
	}
}

func TestMigrateRejectsWrongDestinationVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprint(w, "private-conversation-marker")
	}))
	defer server.Close()
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--server", server.URL, "--namespace", "test", "--token", "test-only", "session", "migrate", "export", "source", "--codex-home", t.TempDir(), "--cwd", t.TempDir(), "--journal-dir", t.TempDir(), "--codex-bin", "missing"})
	// Version mismatch is caught before contacting the API.
	if err := cmd.Execute(); err == nil {
		t.Fatal("unsupported destination accepted")
	}
}

func TestMigrationTargetFailsClosedWithoutExplicitServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mode := range []string{"missing", "malformed", "no-context"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if mode == "malformed" {
				require.NoError(t, os.WriteFile(path, []byte("not: [valid"), 0o600))
			}
			if mode == "no-context" {
				require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600))
			}
			home, journal := t.TempDir(), t.TempDir()
			require.NoError(t, os.Chmod(journal, 0o700))
			cmd := newRootCmd()
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs([]string{"--kubeconfig", path, "session", "migrate", "import", "private", "--codex-home", home, "--thread", "thread", "--journal-dir", journal, "--source-stopped"})
			require.ErrorContains(t, cmd.Execute(), "invalid or unavailable kubeconfig")
		})
	}
	cmd := newRootCmd()
	require.NoError(t, cmd.PersistentFlags().Set("server", "http://explicit.example"))
	require.NoError(t, cmd.PersistentFlags().Set("kubeconfig", "/missing/config"))
	cmd.Flags().AddFlagSet(cmd.PersistentFlags())
	c, target, cleanup, err := newMigrationClient(cmd)
	require.NoError(t, err)
	defer cleanup()
	require.Equal(t, "http://explicit.example", target)
	require.Equal(t, target, c.BaseURL)
}
