/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"

	"github.com/sozercan/mercan/internal/cli/client"
)

const (
	configDir  = ".mercan"
	configFile = "config.yaml"

	// mercanServiceLabel is used to discover the Mercan service in a cluster.
	mercanServiceLabel = "app.kubernetes.io/name=mercan"
)

// mercanConfig holds the persisted CLI configuration.
type mercanConfig struct {
	Server    string `yaml:"server,omitempty"`
	Token     string `yaml:"token,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`
}

// newClientFromCmd creates a client.Client using flag values resolved against config.
func newClientFromCmd(cmd *cobra.Command) *client.Client {
	server, _ := cmd.Flags().GetString("server")
	token, _ := cmd.Flags().GetString("token")
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
		ns = "default"
	}

	// Auto-discover server via K8s service discovery + port-forward
	if server == "" {
		kubeconfigFlag := kubeconfigPath
		if svcNS, svcName := discoverService(kubeconfigFlag, ns); svcName != "" {
			localPort, cleanup, err := startPortForward(kubeconfigFlag, svcNS, svcName)
			if err == nil {
				// Register cleanup for process exit
				go func() {
					c := make(chan os.Signal, 1)
					signal.Notify(c, os.Interrupt)
					<-c
					cleanup()
				}()
				server = fmt.Sprintf("http://localhost:%d", localPort)
				fmt.Fprintf(os.Stderr, "Auto-connected to %s/%s in namespace %s (port-forward :%d)\n",
					svcName, "api", svcNS, localPort)
			}
		}
	}

	if server == "" {
		server = defaultServer
	}

	return client.NewWithNamespace(server, token, ns)
}

// discoverService finds the Mercan service in the cluster.
// Returns the namespace and service name, or empty strings if not found.
func discoverService(kubeconfigPath, defaultNS string) (string, string) {
	restConfig, err := buildRESTConfig(kubeconfigPath)
	if err != nil {
		return "", ""
	}

	// Try the user's namespace first, then well-known mercan namespaces
	namespacesToTry := []string{defaultNS}
	for _, ns := range []string{"mercan-system", "mercan", "default"} {
		if ns != defaultNS {
			namespacesToTry = append(namespacesToTry, ns)
		}
	}

	for _, ns := range namespacesToTry {
		if name := discoverMercanService(restConfig, ns); name != "" {
			return ns, name
		}
	}
	return "", ""
}

// startPortForward starts a kubectl port-forward to the Mercan service.
// Returns the local port, a cleanup function, and any error.
func startPortForward(kubeconfigPath, namespace, service string) (int, func(), error) {
	// Find a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, fmt.Errorf("find free port: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close() //nolint:errcheck

	args := []string{"port-forward", "-n", namespace, "svc/" + service, fmt.Sprintf("%d:8080", localPort)}
	if kubeconfigPath != "" {
		args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	}

	cmd := exec.Command("kubectl", args...)
	cmd.Stderr = nil
	cmd.Stdout = nil

	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("start port-forward: %w", err)
	}

	cleanup := func() {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
		}
	}

	// Wait for port-forward to be ready
	ready := false
	for i := 0; i < 30; i++ {
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
		return 0, nil, fmt.Errorf("port-forward not ready after 3s")
	}

	return localPort, cleanup, nil
}

// discoverMercanService finds the Mercan API service in the given namespace.
// Tries well-known service names first, then falls back to label selector.
func discoverMercanService(restConfig *rest.Config, namespace string) string {
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
	for _, candidate := range []string{"mercan-api", "mercan", "mercan-controller-manager"} {
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
		host, namespace, mercanServiceLabel)

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
func loadConfig() mercanConfig {
	var cfg mercanConfig
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
func saveConfig(cfg mercanConfig) error {
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
