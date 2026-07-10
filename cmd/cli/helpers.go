/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	configDir  = ".orka"
	configFile = "config.yaml"
	cacheFile  = "portforward.json"
	lockFile   = "portforward.lock"

	portForwardCacheTTL       = 30 * time.Minute
	portForwardCacheClockSkew = time.Minute
	portForwardCacheLockWait  = 5 * time.Second

	// defaultNamespace is the Kubernetes default namespace.
	defaultNamespace = "default"

	// orkaServiceLabel is used to discover the Orka service in a cluster.
	orkaServiceLabel = "app.kubernetes.io/name=orka"
)

var errPortForwardOwnershipUnavailable = errors.New("port-forward listener ownership verification unavailable")

// orkaConfig holds the persisted CLI configuration.
type orkaConfig struct {
	Server    string `yaml:"server,omitempty"`
	Token     string `yaml:"token,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`
}

// portForwardCache holds cached port-forward connection info to avoid re-creating on every command.
type portForwardCache struct {
	Port               int    `json:"port"`
	PID                int    `json:"pid"`
	Service            string `json:"service"`
	Namespace          string `json:"namespace"`
	ClusterFingerprint string `json:"clusterFingerprint"`
	ProcessFingerprint string `json:"processFingerprint,omitempty"`
	Timestamp          int64  `json:"timestamp"`
}

func loadPortForwardCache() *portForwardCache {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, configDir, cacheFile))
	if err != nil {
		return nil
	}
	var cache portForwardCache
	if json.Unmarshal(data, &cache) != nil {
		removePortForwardCacheFile()
		return nil
	}
	if cache.ClusterFingerprint == "" && validPortForwardCacheMetadata(&cache) {
		removePortForwardCacheFile()
		return nil
	}
	if !validPortForwardCache(&cache) {
		removePortForwardCacheFile()
		return nil
	}
	if portForwardCacheExpired(&cache) {
		removePortForwardCacheFile()
		return nil
	}
	return &cache
}

func validPortForwardCache(cache *portForwardCache) bool {
	if !validPortForwardCacheMetadata(cache) ||
		!validSHA256Fingerprint(cache.ClusterFingerprint) ||
		!validSHA256Fingerprint(cache.ProcessFingerprint) {
		return false
	}
	return true
}

func validSHA256Fingerprint(fingerprint string) bool {
	if len(fingerprint) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(fingerprint)
	return err == nil
}

func validPortForwardCacheMetadata(cache *portForwardCache) bool {
	return cache != nil &&
		cache.Port > 0 && cache.Port <= 65535 &&
		cache.PID > 0 &&
		cache.Service != "" &&
		cache.Namespace != "" &&
		cache.Timestamp > 0
}

func portForwardCacheExpired(cache *portForwardCache) bool {
	createdAt := time.Unix(cache.Timestamp, 0)
	now := time.Now()
	return createdAt.After(now.Add(portForwardCacheClockSkew)) || now.Sub(createdAt) > portForwardCacheTTL
}

func savePortForwardCache(cache *portForwardCache) error {
	if cache == nil {
		return fmt.Errorf("port-forward cache is nil")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, configDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}
	cache.Timestamp = time.Now().Unix()
	data, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("marshal port-forward cache: %w", err)
	}
	tempFile, err := os.CreateTemp(dir, ".portforward-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary port-forward cache: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath) //nolint:errcheck
	if err := tempFile.Chmod(0o600); err != nil {
		tempFile.Close() //nolint:errcheck
		return fmt.Errorf("set temporary port-forward cache permissions: %w", err)
	}
	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close() //nolint:errcheck
		return fmt.Errorf("write temporary port-forward cache: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temporary port-forward cache: %w", err)
	}
	if err := os.Rename(tempPath, filepath.Join(dir, cacheFile)); err != nil {
		return fmt.Errorf("replace port-forward cache: %w", err)
	}
	return nil
}

func clearPortForwardCache() {
	release, err := acquirePortForwardCacheLock(portForwardCacheLockWait)
	if err != nil {
		return
	}
	defer release()
	removePortForwardCacheFile()
}

func acquirePortForwardCacheLock(wait time.Duration) (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, configDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	path := filepath.Join(dir, lockFile)
	deadline := time.Now().Add(wait)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return sync.OnceFunc(func() {
				os.Remove(path) //nolint:errcheck
			}), nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create cache lock: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for port-forward cache lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func cachedPortForwardReady(cache *portForwardCache) bool {
	if !cachedPortForwardProcessVerified(cache) {
		return false
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cache.Port), 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func cachedPortForwardProcessVerified(cache *portForwardCache) bool {
	if cache == nil || cache.PID <= 0 || cache.Port <= 0 || cache.Port > 65535 ||
		!validSHA256Fingerprint(cache.ProcessFingerprint) {
		return false
	}
	processIdentity, err := portForwardProcessIdentity(cache.PID)
	if err != nil ||
		portForwardProcessIdentityFingerprint(processIdentity) != cache.ProcessFingerprint ||
		!processIdentityMatchesPortForward(processIdentity, cache.Port) {
		return false
	}
	owned, err := processOwnsTCPListener(cache.PID, cache.Port)
	return err == nil && owned
}

func portForwardProcessIdentity(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process ID %d", pid)
	}

	psPath := ""
	for _, candidate := range []string{"/bin/ps", "/usr/bin/ps"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			psPath = candidate
			break
		}
	}
	if psPath == "" {
		return "", fmt.Errorf("cannot verify port-forward process identity: ps not found")
	}

	command := exec.Command(
		psPath,
		"-ww",
		"-p", strconv.Itoa(pid),
		"-o", "lstart=",
		"-o", "command=",
	)
	command.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("inspect port-forward process %d: %w", pid, err)
	}
	identity := strings.TrimSpace(string(output))
	if identity == "" {
		return "", fmt.Errorf("inspect port-forward process %d: empty identity", pid)
	}
	return identity, nil
}

func portForwardProcessIdentityFingerprint(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", sum)
}

func processIdentityMatchesPortForward(identity string, port int) bool {
	wantMapping := fmt.Sprintf("%d:8080", port)
	hasPortForward := false
	hasMapping := false
	for field := range strings.FieldsSeq(identity) {
		switch field {
		case "port-forward":
			hasPortForward = true
		case wantMapping:
			hasMapping = true
		}
	}
	return hasPortForward && hasMapping
}

func processOwnsTCPListener(pid, port int) (bool, error) {
	if runtime.GOOS == "linux" {
		owned, linuxErr := linuxProcessOwnsTCPListener(pid, port)
		if linuxErr == nil {
			return owned, nil
		}
		owned, lsofErr := lsofProcessOwnsTCPListener(pid, port)
		if lsofErr == nil {
			return owned, nil
		}
		if errors.Is(lsofErr, errPortForwardOwnershipUnavailable) {
			return false, linuxErr
		}
		return false, errors.Join(linuxErr, lsofErr)
	}
	return lsofProcessOwnsTCPListener(pid, port)
}

func linuxProcessOwnsTCPListener(pid, port int) (bool, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return false, fmt.Errorf("read process file descriptors: %w", err)
	}
	socketInodes := make(map[string]struct{})
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		socketInodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = struct{}{}
	}
	if len(socketInodes) == 0 {
		return false, nil
	}

	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/tcp", pid))
	if err != nil {
		return false, fmt.Errorf("read process TCP listeners: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) <= 9 || fields[3] != "0A" {
			continue
		}
		addressHex, portHex, ok := strings.Cut(fields[1], ":")
		if !ok || !strings.EqualFold(addressHex, "0100007F") {
			continue
		}
		listenerPort, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil || int(listenerPort) != port {
			continue
		}
		if _, ok := socketInodes[fields[9]]; ok {
			return true, nil
		}
	}
	return false, nil
}

func lsofProcessOwnsTCPListener(pid, port int) (bool, error) {
	lsofPath := ""
	for _, candidate := range []string{"/usr/sbin/lsof", "/usr/bin/lsof", "/bin/lsof"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			lsofPath = candidate
			break
		}
	}
	if lsofPath == "" {
		return false, errPortForwardOwnershipUnavailable
	}
	output, err := exec.Command(
		lsofPath,
		"-nP",
		"-a",
		"-p", strconv.Itoa(pid),
		fmt.Sprintf("-iTCP:%d", port),
		"-sTCP:LISTEN",
		"-F", "pn",
	).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("inspect process TCP listeners: %w", err)
	}
	wantPID := "p" + strconv.Itoa(pid)
	wantAddress := fmt.Sprintf("n127.0.0.1:%d", port)
	foundPID := false
	foundAddress := false
	for line := range strings.SplitSeq(string(output), "\n") {
		switch strings.TrimSpace(line) {
		case wantPID:
			foundPID = true
		case wantAddress:
			foundAddress = true
		}
	}
	return foundPID && foundAddress, nil
}

func removePortForwardCacheFile() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	os.Remove(filepath.Join(home, configDir, cacheFile)) //nolint:errcheck
}

// newClientFromCmd creates a client.Client using flag values resolved against config.
func newClientFromCmd(cmd *cobra.Command) *client.Client {
	server, _ := cmd.Flags().GetString("server")
	token, _ := cmd.Flags().GetString("token")
	txnToken, _ := cmd.Flags().GetString("txn-token")
	txnTokenFile, _ := cmd.Flags().GetString("txn-token-file")
	ns, _ := cmd.Flags().GetString("namespace")

	// Load config as fallback
	cfg := loadConfig()
	if server == "" {
		server = cfg.Server
	}
	if token == "" {
		token = cfg.Token
	}
	if ns == "" {
		ns = cfg.Namespace
	}

	// Try kubeconfig for token, namespace, and server (K8s API proxy)
	kubeconfigPath, _ := cmd.Flags().GetString("kubeconfig")
	if token == "" || ns == "" {
		kc := extractKubeContext(kubeconfigPath)
		if token == "" {
			token = kc.token
		}
		if ns == "" {
			ns = kc.namespace
		}
	}

	if ns == "" {
		ns = defaultNamespace
	}

	// Try cached port-forward first, but only for the same resolved cluster.
	clusterID := ""
	var clusterIDErr error
	cacheLockHeld := false
	if server == "" {
		if releaseCacheLock, err := acquirePortForwardCacheLock(portForwardCacheLockWait); err == nil {
			cacheLockHeld = true
			defer releaseCacheLock()
		}
		clusterID, clusterIDErr = clusterFingerprint(kubeconfigPath, ns)
		if cacheLockHeld {
			if cached := loadPortForwardCache(); cached != nil {
				if clusterIDErr != nil || cached.ClusterFingerprint != clusterID {
					removePortForwardCacheFile()
				} else if cachedPortForwardReady(cached) {
					server = fmt.Sprintf("http://localhost:%d", cached.Port)
					fmt.Fprintf(os.Stderr, "Connected to %s in %s (cached port %d)\n",
						cached.Service, cached.Namespace, cached.Port)
				} else {
					removePortForwardCacheFile()
				}
			}
		}
	}

	// Auto-discover server via K8s service discovery + port-forward
	if server == "" {
		kubeconfigFlag := kubeconfigPath
		// Show connecting indicator if stderr is a terminal
		stderrIsTTY := false
		if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			stderrIsTTY = true
		}
		if stderrIsTTY {
			fmt.Fprint(os.Stderr, "⠋ Connecting to cluster…")
		}
		if svcNS, svcName := discoverService(kubeconfigFlag, ns); svcName != "" {
			localPort, pid, processFingerprint, cleanup, err := startPortForward(kubeconfigFlag, svcNS, svcName)
			if err == nil {
				cleanup = sync.OnceFunc(cleanup)
				// Register cleanup for interrupt
				go func() {
					c := make(chan os.Signal, 1)
					signal.Notify(c, os.Interrupt)
					<-c
					cleanup()
				}()
				server = fmt.Sprintf("http://localhost:%d", localPort)
				if cacheLockHeld && clusterIDErr == nil && processFingerprint != "" {
					err := savePortForwardCache(&portForwardCache{
						Port:               localPort,
						PID:                pid,
						Service:            svcName,
						Namespace:          svcNS,
						ClusterFingerprint: clusterID,
						ProcessFingerprint: processFingerprint,
					})
					if err != nil {
						registerCommandCleanup(cleanup)
					}
				} else {
					registerCommandCleanup(cleanup)
				}
				if stderrIsTTY {
					fmt.Fprint(os.Stderr, "\r\033[2K")
				}
				fmt.Fprintf(os.Stderr, "Connected to %s in %s (port %d)\n",
					svcName, svcNS, localPort)
			} else if stderrIsTTY {
				fmt.Fprint(os.Stderr, "\r\033[2K")
			}
		} else if stderrIsTTY {
			fmt.Fprint(os.Stderr, "\r\033[2K")
		}
	}

	if server == "" {
		server = defaultServer
	}

	c := client.NewWithNamespace(server, token, ns)
	if txnToken == "" && txnTokenFile != "" {
		data, err := readFileOrStdin(txnTokenFile)
		if err != nil {
			cobra.CheckErr(fmt.Errorf("reading --txn-token-file: %w", err))
		}
		txnToken = string(data)
	}
	c.TxnToken = strings.TrimSpace(txnToken)
	return c
}

func registerCommandCleanup(cleanup func()) {
	cobra.OnFinalize(cleanup)
}

// clusterFingerprint returns a stable, credential-free identity for the resolved kubeconfig cluster.
func clusterFingerprint(kubeconfigPath, effectiveNamespace string) (string, error) {
	loadingRules := &clientcmd.ClientConfigLoadingRules{}
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	} else {
		loadingRules = clientcmd.NewDefaultClientConfigLoadingRules()
	}

	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil)
	rawConfig, err := config.RawConfig()
	if err != nil {
		return "", fmt.Errorf("load kubeconfig: %w", err)
	}
	contextName := rawConfig.CurrentContext
	if contextName == "" {
		return "", fmt.Errorf("kubeconfig has no current context")
	}
	currentContext, ok := rawConfig.Contexts[contextName]
	if !ok {
		return "", fmt.Errorf("kubeconfig context %q not found", contextName)
	}
	clusterName := currentContext.Cluster
	cluster, ok := rawConfig.Clusters[clusterName]
	if !ok {
		return "", fmt.Errorf("kubeconfig cluster %q not found", clusterName)
	}
	if cluster.Server == "" {
		return "", fmt.Errorf("kubeconfig cluster %q has no API server", clusterName)
	}
	serverURL, err := url.Parse(cluster.Server)
	if err != nil {
		return "", fmt.Errorf("parse Kubernetes API server URL: %w", err)
	}
	if serverURL.User != nil {
		return "", fmt.Errorf("kubernetes API server URL userinfo is not cache-safe")
	}

	certificateAuthorityData := cluster.CertificateAuthorityData
	if len(certificateAuthorityData) == 0 && cluster.CertificateAuthority != "" {
		certificateAuthorityData, err = os.ReadFile(cluster.CertificateAuthority)
		if err != nil {
			return "", fmt.Errorf("read certificate authority for cluster %q: %w", clusterName, err)
		}
	}
	certificateAuthorityHash := ""
	if len(certificateAuthorityData) > 0 {
		sum := sha256.Sum256(certificateAuthorityData)
		certificateAuthorityHash = fmt.Sprintf("%x", sum)
	}
	authIdentity, err := sanitizedAuthInfoIdentity(rawConfig.AuthInfos, currentContext.AuthInfo)
	if err != nil {
		return "", err
	}
	proxyIdentity, err := effectiveProxyIdentity(cluster)
	if err != nil {
		return "", err
	}

	identity := struct {
		Context                  string                 `json:"context"`
		Cluster                  string                 `json:"cluster"`
		AuthInfo                 string                 `json:"authInfo,omitempty"`
		ContextNamespace         string                 `json:"contextNamespace,omitempty"`
		EffectiveNamespace       string                 `json:"effectiveNamespace"`
		AuthIdentity             sanitizedAuthIdentity  `json:"authIdentity"`
		Server                   string                 `json:"server"`
		CertificateAuthorityHash string                 `json:"certificateAuthorityHash,omitempty"`
		TLSServerName            string                 `json:"tlsServerName,omitempty"`
		InsecureSkipTLSVerify    bool                   `json:"insecureSkipTLSVerify,omitempty"`
		Proxy                    sanitizedProxyIdentity `json:"proxy"`
	}{
		Context:                  contextName,
		Cluster:                  clusterName,
		AuthInfo:                 currentContext.AuthInfo,
		ContextNamespace:         currentContext.Namespace,
		EffectiveNamespace:       effectiveNamespace,
		AuthIdentity:             authIdentity,
		Server:                   cluster.Server,
		CertificateAuthorityHash: certificateAuthorityHash,
		TLSServerName:            cluster.TLSServerName,
		InsecureSkipTLSVerify:    cluster.InsecureSkipTLSVerify,
		Proxy:                    proxyIdentity,
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal cluster identity: %w", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}

type sanitizedAuthIdentity struct {
	Exists                bool                `json:"exists"`
	ClientCertificateHash string              `json:"clientCertificateHash,omitempty"`
	Username              string              `json:"username,omitempty"`
	Impersonate           string              `json:"impersonate,omitempty"`
	ImpersonateUID        string              `json:"impersonateUID,omitempty"`
	ImpersonateGroups     []string            `json:"impersonateGroups,omitempty"`
	ImpersonateUserExtra  map[string][]string `json:"impersonateUserExtra,omitempty"`
}

func sanitizedAuthInfoIdentity(
	authInfos map[string]*clientcmdapi.AuthInfo,
	authInfoName string,
) (sanitizedAuthIdentity, error) {
	authInfo, exists := authInfos[authInfoName]
	identity := sanitizedAuthIdentity{Exists: exists}
	if !exists || authInfo == nil {
		return identity, nil
	}
	if authInfo.Token != "" || authInfo.TokenFile != "" {
		return sanitizedAuthIdentity{}, fmt.Errorf(
			"auth info %q uses opaque bearer authentication that is not cache-safe",
			authInfoName,
		)
	}
	if authInfo.Password != "" {
		return sanitizedAuthIdentity{}, fmt.Errorf(
			"auth info %q uses basic authentication that is not cache-safe",
			authInfoName,
		)
	}
	if authInfo.Exec != nil {
		return sanitizedAuthIdentity{}, fmt.Errorf(
			"auth info %q uses exec authentication that is not cache-safe",
			authInfoName,
		)
	}
	if authInfo.AuthProvider != nil {
		return sanitizedAuthIdentity{}, fmt.Errorf(
			"auth info %q uses an auth provider that is not cache-safe",
			authInfoName,
		)
	}

	certificateData := authInfo.ClientCertificateData
	if len(certificateData) == 0 && authInfo.ClientCertificate != "" {
		data, err := os.ReadFile(authInfo.ClientCertificate)
		if err != nil {
			return sanitizedAuthIdentity{}, fmt.Errorf(
				"read client certificate for auth info %q: %w",
				authInfoName,
				err,
			)
		}
		certificateData = data
	}
	keyData := authInfo.ClientKeyData
	if len(keyData) == 0 && authInfo.ClientKey != "" {
		data, err := os.ReadFile(authInfo.ClientKey)
		if err != nil {
			return sanitizedAuthIdentity{}, fmt.Errorf(
				"read client key for auth info %q: %w",
				authInfoName,
				err,
			)
		}
		keyData = data
	}
	hasCertificate := len(certificateData) > 0
	hasKey := len(keyData) > 0
	if hasCertificate != hasKey {
		return sanitizedAuthIdentity{}, fmt.Errorf(
			"auth info %q has an incomplete client certificate/key pair",
			authInfoName,
		)
	}
	if hasCertificate {
		pair, err := tls.X509KeyPair(certificateData, keyData)
		if err != nil {
			return sanitizedAuthIdentity{}, fmt.Errorf(
				"validate client certificate/key pair for auth info %q: %w",
				authInfoName,
				err,
			)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return sanitizedAuthIdentity{}, fmt.Errorf(
				"parse client certificate for auth info %q: %w",
				authInfoName,
				err,
			)
		}
		now := time.Now()
		if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return sanitizedAuthIdentity{}, fmt.Errorf(
				"client certificate for auth info %q is not currently valid",
				authInfoName,
			)
		}
		sum := sha256.Sum256(certificateData)
		identity.ClientCertificateHash = fmt.Sprintf("%x", sum)
	}

	identity.Username = authInfo.Username
	identity.Impersonate = authInfo.Impersonate
	identity.ImpersonateUID = authInfo.ImpersonateUID
	identity.ImpersonateGroups = append([]string(nil), authInfo.ImpersonateGroups...)
	sort.Strings(identity.ImpersonateGroups)
	if len(authInfo.ImpersonateUserExtra) > 0 {
		identity.ImpersonateUserExtra = make(map[string][]string, len(authInfo.ImpersonateUserExtra))
		for key, values := range authInfo.ImpersonateUserExtra {
			identity.ImpersonateUserExtra[key] = append([]string(nil), values...)
			sort.Strings(identity.ImpersonateUserExtra[key])
		}
	}
	return identity, nil
}

type sanitizedProxyIdentity struct {
	Source   string `json:"source"`
	Scheme   string `json:"scheme,omitempty"`
	Host     string `json:"host,omitempty"`
	Path     string `json:"path,omitempty"`
	Username string `json:"username,omitempty"`
}

func effectiveProxyIdentity(cluster *clientcmdapi.Cluster) (sanitizedProxyIdentity, error) {
	proxySource := "kubeconfig"
	proxyURL := cluster.ProxyURL
	if proxyURL == "" {
		serverURL, err := url.Parse(cluster.Server)
		if err != nil {
			return sanitizedProxyIdentity{}, fmt.Errorf("parse Kubernetes API server URL: %w", err)
		}
		resolvedProxy, err := http.ProxyFromEnvironment(&http.Request{URL: serverURL})
		if err != nil {
			return sanitizedProxyIdentity{}, fmt.Errorf("resolve Kubernetes API proxy: %w", err)
		}
		if resolvedProxy == nil {
			return sanitizedProxyIdentity{Source: "direct"}, nil
		}
		proxySource = "environment"
		proxyURL = resolvedProxy.String()
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return sanitizedProxyIdentity{}, fmt.Errorf("parse Kubernetes API proxy URL: %w", err)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return sanitizedProxyIdentity{}, fmt.Errorf("kubernetes API proxy URL query and fragment are not cache-safe")
	}
	identity := sanitizedProxyIdentity{
		Source: proxySource,
		Scheme: strings.ToLower(parsed.Scheme),
		Host:   strings.ToLower(parsed.Host),
		Path:   parsed.EscapedPath(),
	}
	if parsed.User != nil {
		identity.Username = parsed.User.Username()
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return sanitizedProxyIdentity{}, fmt.Errorf("credentialed Kubernetes API proxies are not cache-safe")
		}
	}
	return identity, nil
}

// discoverService finds the Orka service in the cluster.
// Returns the namespace and service name, or empty strings if not found.
func discoverService(kubeconfigPath, defaultNS string) (string, string) {
	restConfig, err := buildRESTConfig(kubeconfigPath)
	if err != nil {
		return "", ""
	}

	// Try the user's namespace first, then well-known orka namespaces
	namespacesToTry := []string{defaultNS}
	for _, ns := range []string{"orka-system", "orka", defaultNamespace} {
		if ns != defaultNS {
			namespacesToTry = append(namespacesToTry, ns)
		}
	}

	for _, ns := range namespacesToTry {
		if name := discoverOrkaService(restConfig, ns); name != "" {
			return ns, name
		}
	}
	return "", ""
}

// startPortForward starts a kubectl port-forward to the Orka service.
// Returns the local port, process PID and fingerprint, cleanup function, and any error.
func startPortForward(kubeconfigPath, namespace, service string) (int, int, string, func(), error) {
	// Find a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, 0, "", nil, fmt.Errorf("find free port: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close() //nolint:errcheck

	args := []string{
		"port-forward",
		"--address", "127.0.0.1",
		"-n", namespace,
		"svc/" + service,
		fmt.Sprintf("%d:8080", localPort),
	}
	if kubeconfigPath != "" {
		args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	}

	cmd := exec.Command("kubectl", args...)
	cmd.Stderr = nil
	cmd.Stdout = nil

	if err := cmd.Start(); err != nil {
		return 0, 0, "", nil, fmt.Errorf("start port-forward: %w", err)
	}

	cleanup := func() {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
		}
	}

	// Wait for port-forward to be ready
	ready := false
	for range 30 {
		time.Sleep(100 * time.Millisecond)
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 200*time.Millisecond)
		if err == nil {
			conn.Close() //nolint:errcheck
			ready = true
			break
		}
	}

	if !ready {
		cleanup()
		return 0, 0, "", nil, fmt.Errorf("port-forward not ready after 3s")
	}

	processIdentity, err := portForwardProcessIdentity(cmd.Process.Pid)
	if err != nil {
		cleanup()
		return 0, 0, "", nil, fmt.Errorf("verify port-forward process identity: %w", err)
	}
	if !processIdentityMatchesPortForward(processIdentity, localPort) {
		cleanup()
		return 0, 0, "", nil, fmt.Errorf("port-forward process command does not match port %d", localPort)
	}
	processFingerprint := portForwardProcessIdentityFingerprint(processIdentity)
	owned, ownershipErr := processOwnsTCPListener(cmd.Process.Pid, localPort)
	if ownershipErr != nil {
		cleanup()
		return 0, 0, "", nil, fmt.Errorf("verify port-forward listener ownership: %w", ownershipErr)
	}
	if !owned {
		cleanup()
		return 0, 0, "", nil, fmt.Errorf("port-forward process does not own 127.0.0.1:%d", localPort)
	}
	return localPort, cmd.Process.Pid, processFingerprint, cleanup, nil
}

// discoverOrkaService finds the Orka API service in the given namespace.
// Tries well-known service names first, then falls back to label selector.
func discoverOrkaService(restConfig *rest.Config, namespace string) string {
	transportConfig, err := restConfig.TransportConfig()
	if err != nil {
		return ""
	}
	rt, err := transport.New(transportConfig)
	if err != nil {
		return ""
	}

	httpClient := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	// Strategy 1: check well-known service names
	for _, candidate := range []string{"orka-api", "orka", "orka-controller-manager"} {
		if checkServiceExists(httpClient, restConfig.Host, namespace, candidate) {
			return candidate
		}
	}

	// Strategy 2: find by label (look for a service with an "api" port)
	if name := findServiceByLabel(httpClient, restConfig.Host, namespace); name != "" {
		return name
	}

	return ""
}

func findServiceByLabel(httpClient *http.Client, host, namespace string) string {
	listURL := fmt.Sprintf("%s/api/v1/namespaces/%s/services?labelSelector=%s",
		host, namespace, orkaServiceLabel)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return ""
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Ports []struct {
					Name string `json:"name"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	// Prefer a service with an "api" port
	for _, svc := range result.Items {
		for _, port := range svc.Spec.Ports {
			if port.Name == "api" {
				return svc.Metadata.Name
			}
		}
	}

	if len(result.Items) > 0 {
		return result.Items[0].Metadata.Name
	}

	return ""
}

func checkServiceExists(httpClient *http.Client, host, namespace, name string) bool {
	svcURL := fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s", host, namespace, name)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svcURL, nil)
	if err != nil {
		return false
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck

	return resp.StatusCode == http.StatusOK
}

// buildRESTConfig builds a Kubernetes REST config from kubeconfig.
func buildRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	loadingRules := &clientcmd.ClientConfigLoadingRules{}
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	} else {
		loadingRules = clientcmd.NewDefaultClientConfigLoadingRules()
	}

	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil)
	return config.ClientConfig()
}

// configPath returns the full path to the config file.
func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, configDir, configFile)
}

// loadConfig reads the config file. Returns empty config on error.
func loadConfig() orkaConfig {
	var cfg orkaConfig
	path := configPath()
	if path == "" {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = yaml.Unmarshal(data, &cfg)
	return cfg
}

// saveConfig writes the config file with 0600 permissions.
func saveConfig(cfg orkaConfig) error {
	path := configPath()
	if path == "" {
		return fmt.Errorf("cannot determine home directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// maskToken shows only the first 4 chars + *** for security.
func maskToken(token string) string {
	if len(token) <= 4 {
		return "***"
	}
	return token[:4] + "...***"
}
