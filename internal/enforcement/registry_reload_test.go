package enforcement

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/connectivity"
)

func publishRegistry(t *testing.T, path string, config RegistryConfig) {
	t.Helper()
	body, err := json.Marshal(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path+".next", body, 0600))
	require.NoError(t, os.Rename(path+".next", path))
}

func TestRegistryAddsIncarnationsWithoutReplacingCleanupClients(t *testing.T) {
	provider := registryTLSFiles(t, t.TempDir())
	path := filepath.Join(t.TempDir(), "registry.json")
	config := RegistryConfig{Nodes: []NodeProviders{
		{NodeName: "node", NodeUID: "old", Providers: []ProviderConfig{provider}},
	}}
	publishRegistry(t, path, config)
	registry, err := LoadRegistry(path)
	require.NoError(t, err)
	t.Cleanup(registry.Close)
	original, err := registry.ProviderSetDigest("node", "old")
	require.NoError(t, err)
	client := registry.nodes["old"].clients[0]

	config.Nodes = append(config.Nodes, NodeProviders{
		NodeName: "node", NodeUID: "new", Providers: []ProviderConfig{provider},
	})
	publishRegistry(t, path, config)
	_, err = registry.ProviderSetDigest("node", "new")
	require.NoError(t, err)
	current, err := registry.ProviderSetDigest("node", "old")
	require.NoError(t, err)
	require.Equal(t, original, current)
	bound, err := registry.boundProviders(&corev1alpha1.ContainerEnforcementBinding{
		NodeName: "node", NodeUID: "old", ProviderSetDigest: original,
	})
	require.NoError(t, err)
	require.Same(t, client, bound[0])
	require.NotEqual(t, connectivity.Shutdown, client.conn.GetState())

	registry.Close()
	_, err = registry.ProviderSetDigest("node", "old")
	require.Error(t, err)
}

func TestRegistryUpdateCannotReplaceExistingAuthority(t *testing.T) {
	for _, mutation := range []string{"remove", "rename", "endpoint", "credential-path", "provider", "trust", "malformed"} {
		t.Run(mutation, func(t *testing.T) {
			directory := t.TempDir()
			provider := registryTLSFiles(t, directory)
			path := filepath.Join(directory, "registry.json")
			config := RegistryConfig{Nodes: []NodeProviders{
				{NodeName: "node", NodeUID: "old", Providers: []ProviderConfig{provider}},
			}}
			publishRegistry(t, path, config)
			registry, err := LoadRegistry(path)
			require.NoError(t, err)
			t.Cleanup(registry.Close)
			original := registry.nodes["old"]
			next := RegistryConfig{Nodes: []NodeProviders{
				{NodeName: "node", NodeUID: "old", Providers: []ProviderConfig{provider}},
				{NodeName: "node", NodeUID: "new", Providers: []ProviderConfig{provider}},
			}}
			switch mutation {
			case "remove":
				next.Nodes = next.Nodes[1:]
			case "rename":
				next.Nodes[0].NodeName = "different-node"
			case "endpoint":
				next.Nodes[0].Providers[0].Endpoint = "passthrough:///127.0.0.1:2"
			case "credential-path":
				certificate, err := os.ReadFile(provider.CertificateFile)
				require.NoError(t, err)
				moved := provider.CertificateFile + ".moved"
				require.NoError(t, os.WriteFile(moved, certificate, 0600))
				next.Nodes[0].Providers[0].CertificateFile = moved
			case "provider":
				next.Nodes[0].Providers[0].Name = "replacement-provider"
			case "trust":
				registryTLSFiles(t, directory)
			}
			publishRegistry(t, path, next)
			if mutation == "malformed" {
				require.NoError(t, os.WriteFile(path, []byte("{"), 0600))
			}
			_, err = registry.ProviderSetDigest("node", "new")
			require.Error(t, err)
			require.Same(t, original, registry.nodes["old"])
			require.NotContains(t, registry.nodes, "new", "rejected updates must not partially publish")
			require.NotEqual(t, connectivity.Shutdown, original.clients[0].conn.GetState())
		})
	}
}

func TestRegistryRefreshAndLookupAreRaceSafe(t *testing.T) {
	provider := registryTLSFiles(t, t.TempDir())
	path := filepath.Join(t.TempDir(), "registry.json")
	config := RegistryConfig{Nodes: []NodeProviders{
		{NodeName: "node", NodeUID: "old", Providers: []ProviderConfig{provider}},
	}}
	publishRegistry(t, path, config)
	registry, err := LoadRegistry(path)
	require.NoError(t, err)
	t.Cleanup(registry.Close)
	original, err := registry.ProviderSetDigest("node", "old")
	require.NoError(t, err)
	errors := make(chan error, 4)
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range 100 {
				got, err := registry.ProviderSetDigest("node", "old")
				if err != nil || got != original {
					errors <- fmt.Errorf("existing authority changed during append: %s %v", got, err)
					return
				}
			}
		})
	}
	config.Nodes = append(config.Nodes, NodeProviders{
		NodeName: "node", NodeUID: "new", Providers: []ProviderConfig{provider},
	})
	publishRegistry(t, path, config)
	readers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	_, err = registry.ProviderSetDigest("node", "new")
	require.NoError(t, err)
}
