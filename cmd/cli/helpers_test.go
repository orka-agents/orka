/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	serviceCheckPath       = "/api/v1/namespaces/default/services/orka-api"
	sharedServiceCheckPath = "/api/v1/namespaces/shared/services/orka-api"
	testAPIPortName        = "api"
	testClusterA           = "cluster-a"
	testCtxName            = "test-ctx"
	testHTTPServer         = "http://test:8080"
	testHTTPServer9090     = "http://test:9090"
	testKubeAPIServer      = "https://k8s.example.com"
	testKubeconfigFlag     = "--kubeconfig"
	testMetadataKey        = "metadata"
	testNameKey            = "name"
	testNamespaceFlag      = "--namespace"
	testNamespaceKey       = "namespace"
	testOrkaServiceName    = "orka-api"
	testServerFlag         = "--server"
	testSharedCluster      = "shared-cluster"
	testSharedContext      = "shared-context"
	testSharedNamespace    = "shared"
	testSharedUser         = "shared-user"
	testTokenFlag          = "--token"
	testTokenValue         = "tok"
	testUsername           = "test-user"
	testWindowsGOOS        = "windows"
)

func TestPortForwardHelperProcess(_ *testing.T) {
	if os.Getenv("ORKA_TEST_PORT_FORWARD_HELPER") != "1" {
		return
	}

	mapping := os.Args[len(os.Args)-1]
	localPort, _, ok := strings.Cut(mapping, ":")
	if !ok {
		os.Exit(2)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", localPort))
	if err != nil {
		os.Exit(2)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, testClusterA)
		}),
	}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		os.Exit(2)
	}
}

func TestMaskToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"empty", "", "***"},
		{"short_1", "a", "***"},
		{"short_4", "abcd", "***"},
		{"five_chars", "abcde", "abcd...***"},
		{"long_token", "sk-1234567890abcdef", "sk-1...***"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maskToken(tt.token)
			if got != tt.want {
				t.Errorf("maskToken(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}

func TestConfigPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got := configPath()
	want := filepath.Join(tmp, ".orka", "config.yaml")
	if got != want {
		t.Errorf("configPath() = %q, want %q", got, want)
	}
}

func TestLoadConfigEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg := loadConfig()
	if cfg.Server != "" || cfg.Token != "" || cfg.Namespace != "" {
		t.Errorf("loadConfig() on missing file should return empty config, got %+v", cfg)
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	want := orkaConfig{
		Server:    "http://example.com",
		Token:     "test-token-123",
		Namespace: "my-ns",
	}

	if err := saveConfig(want); err != nil {
		t.Fatalf("saveConfig() error: %v", err)
	}

	got := loadConfig()
	if got.Server != want.Server {
		t.Errorf("Server = %q, want %q", got.Server, want.Server)
	}
	if got.Token != want.Token {
		t.Errorf("Token = %q, want %q", got.Token, want.Token)
	}
	if got.Namespace != want.Namespace {
		t.Errorf("Namespace = %q, want %q", got.Namespace, want.Namespace)
	}

	// Verify file permissions
	path := configPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file perm = %o, want 0600", perm)
	}
}

func TestSaveConfigCreatesDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg := orkaConfig{Server: "http://test.local"}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig() error: %v", err)
	}

	dir := filepath.Join(tmp, ".orka")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("config dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestSaveAndLoadPortForwardCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cache := &portForwardCache{
		Port:               12345,
		PID:                9999,
		Service:            testOrkaServiceName,
		Namespace:          configTestNamespace,
		ClusterFingerprint: strings.Repeat("a", sha256.Size*2),
		ProcessFingerprint: strings.Repeat("0", sha256.Size*2),
	}

	savePortForwardCacheForTest(t, cache)

	got := loadPortForwardCache()
	if got == nil {
		t.Fatal("loadPortForwardCache() returned nil")
	}
	if got.Port != 12345 {
		t.Errorf("Port = %d, want 12345", got.Port)
	}
	if got.PID != 9999 {
		t.Errorf("PID = %d, want 9999", got.PID)
	}
	if got.Service != testOrkaServiceName {
		t.Errorf("Service = %q, want %q", got.Service, testOrkaServiceName)
	}
	if got.Namespace != configTestNamespace {
		t.Errorf("Namespace = %q, want %q", got.Namespace, configTestNamespace)
	}
	if got.ClusterFingerprint != cache.ClusterFingerprint {
		t.Errorf("ClusterFingerprint = %q, want %q", got.ClusterFingerprint, cache.ClusterFingerprint)
	}
	if got.ProcessFingerprint != cache.ProcessFingerprint {
		t.Errorf("ProcessFingerprint = %q, want %q", got.ProcessFingerprint, cache.ProcessFingerprint)
	}
}

func TestLoadPortForwardCacheExpired(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cache := portForwardCache{
		Port:               12345,
		PID:                9999,
		Service:            testOrkaServiceName,
		Namespace:          defaultNamespace,
		ClusterFingerprint: strings.Repeat("b", sha256.Size*2),
		ProcessFingerprint: strings.Repeat("1", sha256.Size*2),
		Timestamp:          time.Now().Unix() - 3600, // 1 hour ago, exceeds 30 min TTL
	}

	dir := filepath.Join(tmp, configDir)
	os.MkdirAll(dir, 0o700) //nolint:errcheck
	data, _ := json.Marshal(cache)
	path := filepath.Join(dir, "portforward.json")
	os.WriteFile(path, data, 0o600) //nolint:errcheck

	got := loadPortForwardCache()
	if got != nil {
		t.Error("expected nil for expired cache")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired cache was not removed: %v", err)
	}
}

func TestLoadPortForwardCacheMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got := loadPortForwardCache()
	if got != nil {
		t.Error("expected nil for missing cache file")
	}
}

func TestLoadPortForwardCacheInvalidJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, configDir)
	os.MkdirAll(dir, 0o700) //nolint:errcheck
	path := filepath.Join(dir, "portforward.json")
	os.WriteFile(path, []byte("not-json"), 0o600) //nolint:errcheck

	got := loadPortForwardCache()
	if got != nil {
		t.Error("expected nil for invalid JSON")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid cache was not removed: %v", err)
	}
}

func TestLoadPortForwardCacheRejectsLegacyEntryWithoutClusterFingerprint(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cache := map[string]any{
		"port":           12345,
		"pid":            999999,
		"service":        testOrkaServiceName,
		testNamespaceKey: defaultNamespace,
		"timestamp":      time.Now().Unix(),
	}
	dir := filepath.Join(tmp, configDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create cache directory: %v", err)
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatalf("marshal legacy cache: %v", err)
	}
	path := filepath.Join(dir, "portforward.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy cache: %v", err)
	}

	if got := loadPortForwardCache(); got != nil {
		t.Fatalf("loadPortForwardCache() = %+v, want nil for legacy cache", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy cache was not removed: %v", err)
	}
}

func TestLoadPortForwardCacheRejectsMalformedEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*portForwardCache)
	}{
		{
			name: "invalid port",
			mutate: func(cache *portForwardCache) {
				cache.Port = 0
			},
		},
		{
			name: "invalid fingerprint",
			mutate: func(cache *portForwardCache) {
				cache.ClusterFingerprint = "not-a-sha256"
			},
		},
		{
			name: "invalid process fingerprint",
			mutate: func(cache *portForwardCache) {
				cache.ProcessFingerprint = "not-a-sha256"
			},
		},
		{
			name: "missing timestamp",
			mutate: func(cache *portForwardCache) {
				cache.Timestamp = 0
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)

			cache := &portForwardCache{
				Port:               12345,
				PID:                999999,
				Service:            testOrkaServiceName,
				Namespace:          defaultNamespace,
				ClusterFingerprint: strings.Repeat("c", sha256.Size*2),
				ProcessFingerprint: strings.Repeat("3", sha256.Size*2),
				Timestamp:          time.Now().Unix(),
			}
			tt.mutate(cache)

			dir := filepath.Join(tmp, configDir)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("create cache directory: %v", err)
			}
			data, err := json.Marshal(cache)
			if err != nil {
				t.Fatalf("marshal cache: %v", err)
			}
			path := filepath.Join(dir, "portforward.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("write cache: %v", err)
			}

			if got := loadPortForwardCache(); got != nil {
				t.Fatalf("loadPortForwardCache() = %+v, want nil", got)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("malformed cache was not removed: %v", err)
			}
		})
	}
}

func TestClearPortForwardCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Save then clear
	cache := &portForwardCache{
		Port:      12345,
		PID:       1,
		Service:   "svc",
		Namespace: "ns",
	}
	savePortForwardCacheForTest(t, cache)

	clearPortForwardCache()

	path := filepath.Join(tmp, ".orka", "portforward.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected portforward.json to be removed after clear")
	}
}

func TestPortForwardCacheLockSerializesOwners(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	releaseFirst, err := acquirePortForwardCacheLock(time.Second)
	if err != nil {
		t.Fatalf("acquire first cache lock: %v", err)
	}
	t.Cleanup(releaseFirst)

	acquiredSecond := make(chan func(), 1)
	errSecond := make(chan error, 1)
	go func() {
		release, err := acquirePortForwardCacheLock(2 * time.Second)
		if err != nil {
			errSecond <- err
			return
		}
		acquiredSecond <- release
	}()

	select {
	case release := <-acquiredSecond:
		release()
		t.Fatal("second cache owner acquired the lock before the first released it")
	case err := <-errSecond:
		t.Fatalf("second cache owner failed before release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst()
	select {
	case release := <-acquiredSecond:
		release()
	case err := <-errSecond:
		t.Fatalf("second cache owner failed after release: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("second cache owner did not acquire the released lock")
	}
}

func TestPortForwardCacheLockFailsClosedOnExistingOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	lockPath := filepath.Join(home, configDir, lockFile)
	if err := os.MkdirAll(lockPath, 0o700); err != nil {
		t.Fatalf("create existing cache lock: %v", err)
	}

	if release, err := acquirePortForwardCacheLock(100 * time.Millisecond); err == nil {
		release()
		t.Fatal("acquired an existing cache lock")
	}
	if info, err := os.Stat(lockPath); err != nil || !info.IsDir() {
		t.Fatalf("existing cache lock was reclaimed: info=%v err=%v", info, err)
	}
}

func TestStartPortForwardCleanupStopsProcess(t *testing.T) {
	installPortForwardHelper(t)

	port, pid, _, cleanup, err := startPortForward("", defaultNamespace, testOrkaServiceName)
	if err != nil {
		t.Fatalf("startPortForward() error: %v", err)
	}
	t.Cleanup(func() {
		cleanup()
		process, err := os.FindProcess(pid)
		if err == nil {
			_, _ = process.Wait()
		}
	})

	cleanup()
	assertPortForwardStopped(t, port, pid)
}

func TestClearPortForwardCacheDoesNotKillLiveProcess(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	installPortForwardHelper(t)

	port, pid, processFingerprint, cleanup, err := startPortForward("", defaultNamespace, testOrkaServiceName)
	if err != nil {
		t.Fatalf("startPortForward() error: %v", err)
	}
	t.Cleanup(func() {
		cleanup()
		process, err := os.FindProcess(pid)
		if err == nil {
			_, _ = process.Wait()
		}
	})

	savePortForwardCacheForTest(t, &portForwardCache{
		Port:               port,
		PID:                pid,
		Service:            testOrkaServiceName,
		Namespace:          defaultNamespace,
		ClusterFingerprint: strings.Repeat("d", sha256.Size*2),
		ProcessFingerprint: processFingerprint,
	})
	clearPortForwardCache()

	path := filepath.Join(tmp, configDir, cacheFile)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cache was not removed: %v", err)
	}
	baseURL := fmt.Sprintf("http://localhost:%d", port)
	body, err := getPortForwardResponse(baseURL)
	if err != nil {
		t.Fatalf("cache invalidation killed a potentially shared port-forward: %v", err)
	}
	if body != testClusterA {
		t.Fatalf("controlled process response = %q, want %q", body, testClusterA)
	}
}
func assertPortForwardStopped(t *testing.T, port, pid int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	address := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port-forward process %d is still listening on port %d", pid, port)
}

func TestClusterFingerprintChangesWhenCurrentContextChanges(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "context-a"
	config.Contexts["context-a"] = &clientcmdapi.Context{Cluster: testSharedCluster, Namespace: testSharedNamespace}
	config.Contexts["context-b"] = &clientcmdapi.Context{Cluster: testSharedCluster, Namespace: testSharedNamespace}
	config.Clusters[testSharedCluster] = &clientcmdapi.Cluster{
		Server:                   testKubeAPIServer,
		CertificateAuthorityData: []byte("shared-ca"),
	}

	kubeconfigA := writeKubeconfig(t, t.TempDir(), config)
	config.CurrentContext = "context-b"
	kubeconfigB := writeKubeconfig(t, t.TempDir(), config)

	fingerprintA, err := clusterFingerprint(kubeconfigA, testSharedNamespace)
	if err != nil {
		t.Fatalf("clusterFingerprint(context-a) error: %v", err)
	}
	fingerprintB, err := clusterFingerprint(kubeconfigB, testSharedNamespace)
	if err != nil {
		t.Fatalf("clusterFingerprint(context-b) error: %v", err)
	}
	if fingerprintA == fingerprintB {
		t.Fatal("fingerprint did not change when the current context changed")
	}
}

func TestClusterFingerprintRejectsUncacheableAuthentication(t *testing.T) {
	certificateA, _ := generateClientCertificatePair(t)
	_, keyB := generateClientCertificatePair(t)
	expiredCertificate, expiredKey := generateClientCertificatePairWithValidity(
		t,
		time.Now().Add(-2*time.Hour),
		time.Now().Add(-time.Hour),
	)
	futureCertificate, futureKey := generateClientCertificatePairWithValidity(
		t,
		time.Now().Add(time.Hour),
		time.Now().Add(2*time.Hour),
	)
	baseConfig := func() *clientcmdapi.Config {
		config := clientcmdapi.NewConfig()
		config.CurrentContext = testSharedContext
		config.Contexts[testSharedContext] = &clientcmdapi.Context{
			Cluster:  testSharedCluster,
			AuthInfo: testSharedUser,
		}
		config.Clusters[testSharedCluster] = &clientcmdapi.Cluster{Server: testKubeAPIServer}
		config.AuthInfos[testSharedUser] = &clientcmdapi.AuthInfo{}
		return config
	}

	tests := []struct {
		name      string
		configure func(*clientcmdapi.Config)
	}{
		{
			name: "direct bearer",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser] = &clientcmdapi.AuthInfo{Token: "mock-token"}
			},
		},
		{
			name: "bearer file",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser] = &clientcmdapi.AuthInfo{TokenFile: "REDACTED"}
			},
		},
		{
			name: "basic authentication",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser].Username = testUsername
				config.AuthInfos[testSharedUser].Password = "REDACTED"
			},
		},
		{
			name: "exec plugin",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser].Exec = &clientcmdapi.ExecConfig{Command: "identity-helper"}
			},
		},
		{
			name: "auth provider",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "identity-provider"}
			},
		},
		{
			name: "credentialed proxy",
			configure: func(config *clientcmdapi.Config) {
				config.Clusters[testSharedCluster].ProxyURL = "http://test-user:REDACTED@proxy.example.com:8080"
			},
		},
		{
			name: "credentialed API server URL",
			configure: func(config *clientcmdapi.Config) {
				config.Clusters[testSharedCluster].Server = "https://test-user:REDACTED@k8s.example.com"
			},
		},
		{
			name: "incomplete client certificate pair",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser].ClientCertificateData = certificateA
			},
		},
		{
			name: "mismatched client certificate pair",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser].ClientCertificateData = certificateA
				config.AuthInfos[testSharedUser].ClientKeyData = keyB
			},
		},
		{
			name: "expired client certificate",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser].ClientCertificateData = expiredCertificate
				config.AuthInfos[testSharedUser].ClientKeyData = expiredKey
			},
		},
		{
			name: "not-yet-valid client certificate",
			configure: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser].ClientCertificateData = futureCertificate
				config.AuthInfos[testSharedUser].ClientKeyData = futureKey
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := baseConfig()
			tt.configure(config)
			path := writeKubeconfig(t, t.TempDir(), config)
			if fingerprint, err := clusterFingerprint(path, defaultNamespace); err == nil {
				t.Fatalf("clusterFingerprint() = %q, want cache-safety error", fingerprint)
			}
		})
	}
}
func TestClusterFingerprintIncludesContextAndClusterIdentity(t *testing.T) {
	base := clientcmdapi.NewConfig()
	base.CurrentContext = testSharedContext
	base.Contexts[testSharedContext] = &clientcmdapi.Context{
		Cluster:  testSharedCluster,
		AuthInfo: testSharedUser,
	}
	base.Clusters[testSharedCluster] = &clientcmdapi.Cluster{
		Server:                   testKubeAPIServer,
		CertificateAuthorityData: []byte("cluster-ca-a"),
	}
	clientCertificate, clientKey := generateClientCertificatePair(t)
	base.AuthInfos[testSharedUser] = &clientcmdapi.AuthInfo{
		ClientCertificateData: clientCertificate,
		ClientKeyData:         clientKey,
	}

	basePath := writeKubeconfig(t, t.TempDir(), base)
	baseFingerprint, err := clusterFingerprint(basePath, defaultNamespace)
	if err != nil {
		t.Fatalf("clusterFingerprint(base) error: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*clientcmdapi.Config)
	}{
		{
			name: "API server",
			mutate: func(config *clientcmdapi.Config) {
				config.Clusters[testSharedCluster].Server = "https://other-k8s.example.com"
			},
		},
		{
			name: "certificate authority",
			mutate: func(config *clientcmdapi.Config) {
				config.Clusters[testSharedCluster].CertificateAuthorityData = []byte("cluster-ca-b")
			},
		},
		{
			name: "cluster name",
			mutate: func(config *clientcmdapi.Config) {
				cluster := config.Clusters[testSharedCluster]
				delete(config.Clusters, testSharedCluster)
				config.Clusters["renamed-cluster"] = cluster
				config.Contexts[testSharedContext].Cluster = "renamed-cluster"
			},
		},
		{
			name: "auth info reference",
			mutate: func(config *clientcmdapi.Config) {
				config.Contexts[testSharedContext].AuthInfo = "other-user"
			},
		},
		{
			name: "impersonated user",
			mutate: func(config *clientcmdapi.Config) {
				config.AuthInfos[testSharedUser].Impersonate = "other-user"
			},
		},
		{
			name: "proxy route",
			mutate: func(config *clientcmdapi.Config) {
				config.Clusters[testSharedCluster].ProxyURL = "http://proxy.example.com:8080"
			},
		},
		{
			name: "context namespace",
			mutate: func(config *clientcmdapi.Config) {
				config.Contexts[testSharedContext].Namespace = "other-namespace"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base.DeepCopy()
			tt.mutate(changed)
			path := writeKubeconfig(t, t.TempDir(), changed)
			fingerprint, err := clusterFingerprint(path, defaultNamespace)
			if err != nil {
				t.Fatalf("clusterFingerprint() error: %v", err)
			}
			if fingerprint == baseFingerprint {
				t.Fatalf("fingerprint did not change with %s", tt.name)
			}
		})
	}

	otherNamespaceFingerprint, err := clusterFingerprint(basePath, "other-namespace")
	if err != nil {
		t.Fatalf("clusterFingerprint(other effective namespace) error: %v", err)
	}
	if otherNamespaceFingerprint == baseFingerprint {
		t.Fatal("fingerprint did not change with the effective namespace")
	}
}

func TestClusterFingerprintResolvesCertificateAuthorityFilesByContent(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.CurrentContext = testSharedContext
	config.Contexts[testSharedContext] = &clientcmdapi.Context{Cluster: testSharedCluster}
	config.Clusters[testSharedCluster] = &clientcmdapi.Cluster{
		Server:               testKubeAPIServer,
		CertificateAuthority: "ca.crt",
	}

	writeConfig := func(dir string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("shared-ca"), 0o600); err != nil {
			t.Fatalf("write certificate authority: %v", err)
		}
		return writeKubeconfig(t, dir, config)
	}
	kubeconfigA := writeConfig(t.TempDir())
	kubeconfigB := writeConfig(t.TempDir())

	fingerprintA, err := clusterFingerprint(kubeconfigA, defaultNamespace)
	if err != nil {
		t.Fatalf("clusterFingerprint(first path) error: %v", err)
	}
	fingerprintB, err := clusterFingerprint(kubeconfigB, defaultNamespace)
	if err != nil {
		t.Fatalf("clusterFingerprint(second path) error: %v", err)
	}
	if fingerprintA != fingerprintB {
		t.Fatal("fingerprint changed when only the resolved CA file path changed")
	}
}

func TestCheckServiceExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case serviceCheckPath:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := srv.Client()

	if !checkServiceExists(client, srv.URL, defaultNamespace, testOrkaServiceName) {
		t.Error("expected true for existing service")
	}
	if checkServiceExists(client, srv.URL, defaultNamespace, "missing-svc") {
		t.Error("expected false for missing service")
	}
}

func TestFindServiceByLabel(t *testing.T) {
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		wantName string
	}{
		{
			name: "service_with_api_port",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{
					"items": []map[string]any{
						{
							testMetadataKey: map[string]any{testNameKey: testOrkaServiceName},
							"spec": map[string]any{
								"ports": []map[string]any{
									{testNameKey: testAPIPortName},
								},
							},
						},
					},
				}
				json.NewEncoder(w).Encode(resp) //nolint:errcheck
			},
			wantName: testOrkaServiceName,
		},
		{
			name: "service_without_api_port",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{
					"items": []map[string]any{
						{
							testMetadataKey: map[string]any{testNameKey: "my-svc"},
							"spec": map[string]any{
								"ports": []map[string]any{
									{testNameKey: "http"},
								},
							},
						},
					},
				}
				json.NewEncoder(w).Encode(resp) //nolint:errcheck
			},
			wantName: "my-svc",
		},
		{
			name: "no_items",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{"items": []map[string]any{}}
				json.NewEncoder(w).Encode(resp) //nolint:errcheck
			},
			wantName: "",
		},
		{
			name: "server_error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			got := findServiceByLabel(srv.Client(), srv.URL, defaultNamespace)
			if got != tt.wantName {
				t.Errorf("findServiceByLabel() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestNewClientFromCmd_WithExplicitFlags(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	root := newRootCmd()
	root.SetArgs([]string{testServerFlag, testHTTPServer9090, testTokenFlag, "my-token", testNamespaceFlag, "my-ns"})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		if c.BaseURL != testHTTPServer9090 {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL, testHTTPServer9090)
		}
		if c.Token != "my-token" {
			t.Errorf("Token = %q, want %q", c.Token, "my-token")
		}
		if c.Namespace != "my-ns" {
			t.Errorf("Namespace = %q, want %q", c.Namespace, "my-ns")
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_FallbackToConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg := orkaConfig{Server: "http://cfg-server:8080", Token: "cfg-token", Namespace: "cfg-ns"}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig error: %v", err)
	}

	root := newRootCmd()
	root.SetArgs([]string{})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		if c.BaseURL != "http://cfg-server:8080" {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL, "http://cfg-server:8080")
		}
		if c.Token != "cfg-token" {
			t.Errorf("Token = %q, want %q", c.Token, "cfg-token")
		}
		if c.Namespace != "cfg-ns" {
			t.Errorf("Namespace = %q, want %q", c.Namespace, "cfg-ns")
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_DefaultNamespace(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	root := newRootCmd()
	root.SetArgs([]string{testServerFlag, testHTTPServer9090, testTokenFlag, testTokenValue})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		if c.Namespace != defaultNamespace {
			t.Errorf("Namespace = %q, want %q", c.Namespace, defaultNamespace)
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_FallbackDefaultServer(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	root := newRootCmd()
	root.SetArgs([]string{testTokenFlag, testTokenValue, testNamespaceFlag, "ns"})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		// Without kubeconfig or K8s cluster, server should fall back to default
		if c.BaseURL != defaultServer {
			t.Logf("BaseURL = %q (may not be default if kubeconfig exists)", c.BaseURL)
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestBuildRESTConfig_BadPath(t *testing.T) {
	_, err := buildRESTConfig("/nonexistent/kubeconfig")
	if err == nil {
		t.Error("expected error for nonexistent kubeconfig")
	}
}

func TestNewClientFromCmd_WithKubeconfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Create a kubeconfig
	config := clientcmdapi.NewConfig()
	config.CurrentContext = testCtxName
	config.Contexts[testCtxName] = &clientcmdapi.Context{
		Cluster:   "test-cluster",
		AuthInfo:  testUsername,
		Namespace: "kube-ns",
	}
	config.Clusters["test-cluster"] = &clientcmdapi.Cluster{
		Server: testKubeAPIServer,
	}
	config.AuthInfos[testUsername] = &clientcmdapi.AuthInfo{
		Token: "kube-token",
	}
	kubePath := filepath.Join(tmp, "kubeconfig")
	if err := clientcmd.WriteToFile(*config, kubePath); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	root := newRootCmd()
	root.SetArgs([]string{testKubeconfigFlag, kubePath, testServerFlag, testHTTPServer})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		if c.Token != "kube-token" {
			t.Errorf("Token = %q, want %q", c.Token, "kube-token")
		}
		if c.Namespace != "kube-ns" {
			t.Errorf("Namespace = %q, want %q", c.Namespace, "kube-ns")
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_CachedPortForward(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	installPortForwardHelper(t)
	kubeconfigPath := writePortForwardKubeconfig(
		t,
		testSharedContext,
		testSharedCluster,
		testKubeAPIServer,
		"ns",
	)
	fingerprint, err := clusterFingerprint(kubeconfigPath, "ns")
	if err != nil {
		t.Fatalf("clusterFingerprint() error: %v", err)
	}

	port, pid, processFingerprint, cleanup, err := startPortForward(
		"",
		defaultNamespace,
		testOrkaServiceName,
	)
	if err != nil {
		t.Fatalf("startPortForward() error: %v", err)
	}
	t.Cleanup(func() {
		cleanup()
		process, err := os.FindProcess(pid)
		if err == nil {
			_, _ = process.Wait()
		}
	})

	// Save a port-forward cache pointing to our test server
	savePortForwardCacheForTest(t, &portForwardCache{
		Port:               port,
		PID:                pid,
		Service:            testOrkaServiceName,
		Namespace:          configTestNamespace,
		ClusterFingerprint: fingerprint,
		ProcessFingerprint: processFingerprint,
	})

	root := newRootCmd()
	root.SetArgs([]string{testKubeconfigFlag, kubeconfigPath, testTokenFlag, testTokenValue, testNamespaceFlag, "ns"})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c := newClientFromCmd(cmd)
		expected := fmt.Sprintf("http://localhost:%d", port)
		if c.BaseURL != expected {
			t.Errorf("BaseURL = %q, want %q", c.BaseURL, expected)
		}
		return nil
	}

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewClientFromCmd_RejectsCacheAfterCurrentContextSwitch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	installPortForwardHelper(t)

	cachedPort, cachedPID, processFingerprint, cleanup, err := startPortForward(
		"",
		defaultNamespace,
		testOrkaServiceName,
	)
	if err != nil {
		t.Fatalf("startPortForward() error: %v", err)
	}
	t.Cleanup(func() {
		cleanup()
		process, err := os.FindProcess(cachedPID)
		if err == nil {
			_, _ = process.Wait()
		}
	})

	clusterAPI := httptest.NewServer(http.NotFoundHandler())
	defer clusterAPI.Close()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "context-a"
	config.Contexts["context-a"] = &clientcmdapi.Context{
		Cluster:   testSharedCluster,
		Namespace: testSharedNamespace,
	}
	config.Contexts["context-b"] = &clientcmdapi.Context{
		Cluster:   testSharedCluster,
		Namespace: testSharedNamespace,
	}
	config.Clusters[testSharedCluster] = &clientcmdapi.Cluster{Server: clusterAPI.URL}
	kubeconfigPath := writeKubeconfig(t, tmp, config)
	fingerprint, err := clusterFingerprint(kubeconfigPath, testSharedNamespace)
	if err != nil {
		t.Fatalf("clusterFingerprint(context-a) error: %v", err)
	}
	savePortForwardCacheForTest(t, &portForwardCache{
		Port:               cachedPort,
		PID:                cachedPID,
		Service:            testOrkaServiceName,
		Namespace:          testSharedNamespace,
		ClusterFingerprint: fingerprint,
		ProcessFingerprint: processFingerprint,
	})

	config.CurrentContext = "context-b"
	if err := clientcmd.WriteToFile(*config, kubeconfigPath); err != nil {
		t.Fatalf("switch current context: %v", err)
	}

	baseURL := clientBaseURLForKubeconfig(t, kubeconfigPath)
	cachedBaseURL := fmt.Sprintf("http://localhost:%d", cachedPort)
	if baseURL == cachedBaseURL {
		t.Fatalf("context-b reused context-a cached port-forward %s", baseURL)
	}
	path := filepath.Join(tmp, configDir, cacheFile)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("context-a cache was not removed after context switch: %v", err)
	}
	body, err := getPortForwardResponse(cachedBaseURL)
	if err != nil {
		t.Fatalf("context switch killed a potentially shared port-forward: %v", err)
	}
	if body != testClusterA {
		t.Fatalf("controlled process response = %q, want %q", body, testClusterA)
	}
}

func TestNewClientFromCmd_RejectsCachedPortForwardFromDifferentCluster(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	installPortForwardHelper(t)

	clusterAAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == sharedServiceCheckPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer clusterAAPI.Close()

	clusterBAPI := httptest.NewServer(http.NotFoundHandler())
	defer clusterBAPI.Close()

	kubeconfigA := writePortForwardKubeconfig(t, "context-a", testClusterA, clusterAAPI.URL, testSharedNamespace)
	kubeconfigB := writePortForwardKubeconfig(t, "context-b", "cluster-b", clusterBAPI.URL, testSharedNamespace)

	clusterABaseURL := clientBaseURLForKubeconfig(t, kubeconfigA)
	cached := loadPortForwardCache()
	if cached == nil {
		t.Fatal("cluster-A port-forward was not cached")
	}
	t.Cleanup(func() {
		process, err := os.FindProcess(cached.PID)
		if err == nil {
			_ = process.Kill()
			_, _ = process.Wait()
		}
	})

	body, err := getPortForwardResponse(clusterABaseURL)
	if err != nil {
		t.Fatalf("contact cluster-A port-forward: %v", err)
	}
	if body != testClusterA {
		t.Fatalf("cluster-A port-forward response = %q, want %q", body, testClusterA)
	}

	clusterBBaseURL := clientBaseURLForKubeconfig(t, kubeconfigB)
	body, err = getPortForwardResponse(clusterBBaseURL)
	if err == nil && body == testClusterA {
		t.Fatalf("cluster-B command contacted cached cluster-A port-forward at %s", clusterBBaseURL)
	}
	if clusterBBaseURL == clusterABaseURL {
		t.Fatalf("cluster-B command reused cluster-A port-forward %s", clusterBBaseURL)
	}

	cachePath := filepath.Join(tmp, configDir, "portforward.json")
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("stale cluster-A cache was not removed: %v", err)
	}

	body, err = getPortForwardResponse(clusterABaseURL)
	if err != nil {
		t.Fatalf("cluster switch killed a potentially shared port-forward: %v", err)
	}
	if body != testClusterA {
		t.Fatalf("cluster-A port-forward response after invalidation = %q, want %q", body, testClusterA)
	}
}

func TestNewClientFromCmdCleansNonCacheablePortForwardOnCompletion(t *testing.T) {
	tests := []struct {
		name      string
		returnErr bool
	}{
		{name: "success"},
		{name: "command error", returnErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			installPortForwardHelper(t)

			clusterAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == sharedServiceCheckPath {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer clusterAPI.Close()

			config := clientcmdapi.NewConfig()
			config.CurrentContext = testSharedContext
			config.Contexts[testSharedContext] = &clientcmdapi.Context{
				Cluster:   testSharedCluster,
				AuthInfo:  testSharedUser,
				Namespace: testSharedNamespace,
			}
			config.Clusters[testSharedCluster] = &clientcmdapi.Cluster{Server: clusterAPI.URL}
			config.AuthInfos[testSharedUser] = &clientcmdapi.AuthInfo{Token: "cache-disabled"}
			kubeconfigPath := writeKubeconfig(t, t.TempDir(), config)

			var baseURL string
			postRunCalled := false
			root := newRootCmd()
			root.SetArgs([]string{
				testKubeconfigFlag, kubeconfigPath,
				testTokenFlag, testTokenValue,
				testNamespaceFlag, testSharedNamespace,
			})
			root.PostRun = func(_ *cobra.Command, _ []string) {
				postRunCalled = true
			}
			root.RunE = func(cmd *cobra.Command, _ []string) error {
				baseURL = newClientFromCmd(cmd).BaseURL
				body, err := getPortForwardResponse(baseURL)
				if err != nil {
					return fmt.Errorf("contact non-cacheable port-forward: %w", err)
				}
				if body != testClusterA {
					return fmt.Errorf("non-cacheable port-forward response = %q, want %q", body, testClusterA)
				}
				if tt.returnErr {
					return fmt.Errorf("intentional command error")
				}
				return nil
			}
			err := root.Execute()
			if tt.returnErr && err == nil {
				t.Fatal("Execute() error = nil, want command error")
			}
			if !tt.returnErr && err != nil {
				t.Fatalf("Execute() error: %v", err)
			}
			if postRunCalled != !tt.returnErr {
				t.Fatalf("PostRun called = %v, want %v", postRunCalled, !tt.returnErr)
			}

			if _, err := os.Stat(filepath.Join(tmp, configDir, cacheFile)); !os.IsNotExist(err) {
				t.Fatalf("non-cacheable port-forward unexpectedly persisted a cache entry: %v", err)
			}
			var port int
			fmt.Sscanf(baseURL, "http://localhost:%d", &port) //nolint:errcheck
			assertPortForwardStopped(t, port, 0)
		})
	}
}

func TestNewClientFromCmdCleansPortForwardWhenCacheWriteFails(t *testing.T) {
	tests := []struct {
		name      string
		returnErr bool
	}{
		{name: "success"},
		{name: "command error", returnErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installPortForwardHelper(t)

			clusterAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == sharedServiceCheckPath {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer clusterAPI.Close()

			config := clientcmdapi.NewConfig()
			config.CurrentContext = testSharedContext
			config.Contexts[testSharedContext] = &clientcmdapi.Context{
				Cluster:   testSharedCluster,
				Namespace: testSharedNamespace,
			}
			config.Clusters[testSharedCluster] = &clientcmdapi.Cluster{Server: clusterAPI.URL}
			kubeconfigPath := writeKubeconfig(t, t.TempDir(), config)

			homeDir := t.TempDir()
			t.Setenv("HOME", homeDir)
			cacheDestination := filepath.Join(homeDir, configDir, cacheFile)
			if err := os.MkdirAll(cacheDestination, 0o700); err != nil {
				t.Fatalf("create invalid cache destination: %v", err)
			}

			var baseURL string
			root := newRootCmd()
			root.SetArgs([]string{
				testKubeconfigFlag, kubeconfigPath,
				testTokenFlag, testTokenValue,
				testNamespaceFlag, testSharedNamespace,
			})
			root.RunE = func(cmd *cobra.Command, _ []string) error {
				baseURL = newClientFromCmd(cmd).BaseURL
				body, err := getPortForwardResponse(baseURL)
				if err != nil {
					return fmt.Errorf("contact uncached port-forward: %w", err)
				}
				if body != testClusterA {
					return fmt.Errorf("uncached port-forward response = %q, want %q", body, testClusterA)
				}
				if tt.returnErr {
					return fmt.Errorf("intentional command error")
				}
				return nil
			}
			err := root.Execute()
			if tt.returnErr && err == nil {
				t.Fatal("Execute() error = nil, want command error")
			}
			if !tt.returnErr && err != nil {
				t.Fatalf("Execute() error: %v", err)
			}

			var port int
			fmt.Sscanf(baseURL, "http://localhost:%d", &port) //nolint:errcheck
			assertPortForwardStopped(t, port, 0)
		})
	}
}

func generateClientCertificatePair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	return generateClientCertificatePairWithValidity(
		t,
		time.Now().Add(-time.Minute),
		time.Now().Add(time.Hour),
	)
}

func generateClientCertificatePairWithValidity(
	t *testing.T,
	notBefore, notAfter time.Time,
) ([]byte, []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certificateDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		template,
		&privateKey.PublicKey,
		privateKey,
	)
	if err != nil {
		t.Fatalf("create client certificate: %v", err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
	return certificatePEM, privateKeyPEM
}

func savePortForwardCacheForTest(t *testing.T, cache *portForwardCache) {
	t.Helper()
	if err := savePortForwardCache(cache); err != nil {
		t.Fatalf("savePortForwardCache() error: %v", err)
	}
}

func installPortForwardHelper(t *testing.T) {
	t.Helper()
	if runtime.GOOS == testWindowsGOOS {
		t.Skip("port-forward process verification is unavailable on Windows")
	}

	binDir := t.TempDir()
	script := filepath.Join(binDir, "kubectl")
	contents := "#!/bin/sh\nexec \"$ORKA_TEST_BINARY\" -test.run=^TestPortForwardHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}

	t.Setenv("ORKA_TEST_BINARY", os.Args[0])
	t.Setenv("ORKA_TEST_PORT_FORWARD_HELPER", "1")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writePortForwardKubeconfig(
	t *testing.T,
	contextName, clusterName, server, namespace string,
) string {
	t.Helper()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = contextName
	config.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:   clusterName,
		Namespace: namespace,
	}
	config.Clusters[clusterName] = &clientcmdapi.Cluster{Server: server}

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func clientBaseURLForKubeconfig(t *testing.T, kubeconfigPath string) string {
	t.Helper()

	var baseURL string
	root := newRootCmd()
	root.SetArgs([]string{
		testKubeconfigFlag, kubeconfigPath,
		testTokenFlag, "test-token",
		testNamespaceFlag, testSharedNamespace,
	})
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		baseURL = newClientFromCmd(cmd).BaseURL
		return nil
	}
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	return baseURL
}

func getPortForwardResponse(baseURL string) (string, error) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	response, err := client.Get(baseURL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func TestDiscoverOrkaService_WellKnownName(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == serviceCheckPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	restCfg := &rest.Config{
		Host: srv.URL,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	got := discoverOrkaService(restCfg, defaultNamespace)
	if got != testOrkaServiceName {
		t.Errorf("discoverOrkaService() = %q, want %q", got, testOrkaServiceName)
	}
}

func TestDiscoverOrkaService_ByLabel(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No well-known services found
		if r.URL.Path == serviceCheckPath ||
			r.URL.Path == "/api/v1/namespaces/default/services/orka" ||
			r.URL.Path == "/api/v1/namespaces/default/services/orka-controller-manager" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Label query returns a service
		if r.URL.Path == "/api/v1/namespaces/default/services" {
			resp := map[string]any{
				"items": []map[string]any{
					{
						testMetadataKey: map[string]any{testNameKey: "custom-orka"},
						"spec":          map[string]any{"ports": []map[string]any{{testNameKey: testAPIPortName}}},
					},
				},
			}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	restCfg := &rest.Config{
		Host: srv.URL,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	got := discoverOrkaService(restCfg, defaultNamespace)
	if got != "custom-orka" {
		t.Errorf("discoverOrkaService() = %q, want %q", got, "custom-orka")
	}
}

func TestDiscoverOrkaService_NotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	restCfg := &rest.Config{
		Host: srv.URL,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	got := discoverOrkaService(restCfg, defaultNamespace)
	if got != "" {
		t.Errorf("discoverOrkaService() = %q, want empty", got)
	}
}

func TestManifestMapYAMLAndJSON(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "skill.yaml")
	yamlData := []byte("metadata:\n  name: test-skill\nspec:\n  description: hello\n")
	if err := os.WriteFile(yamlPath, yamlData, 0o600); err != nil {
		t.Fatal(err)
	}
	m, body, err := manifestMap(yamlPath)
	if err != nil {
		t.Fatalf("manifestMap yaml error: %v", err)
	}
	if metadataName(m) != "test-skill" {
		t.Fatalf("metadataName = %q, want test-skill", metadataName(m))
	}
	if !json.Valid(body) {
		t.Fatalf("manifest body is not JSON: %s", string(body))
	}

	jsonPath := filepath.Join(dir, "provider.json")
	if err := os.WriteFile(jsonPath, []byte(`{"name":"p","spec":{"type":"openai"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _, err = manifestMap(jsonPath)
	if err != nil {
		t.Fatalf("manifestMap json error: %v", err)
	}
	if metadataName(m) != "p" {
		t.Fatalf("metadataName = %q, want p", metadataName(m))
	}
}

func TestRootCmdIncludesCoverageCommands(t *testing.T) {
	cmd := newRootCmd()
	want := []string{
		"provider", "tool", "session", "secret", "security", "monitor",
		"memory", "auth", "models", "workspace", "substrate",
	}
	seen := map[string]bool{}
	for _, sub := range cmd.Commands() {
		seen[sub.Name()] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Fatalf("root command missing %s", name)
		}
	}
}

func TestArticleFor(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "agent", want: "an"},
		{name: "provider", want: "a"},
		{name: "", want: "a"},
	}
	for _, tt := range tests {
		if got := articleFor(tt.name); got != tt.want {
			t.Errorf("articleFor(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
