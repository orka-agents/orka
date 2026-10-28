package enforcement

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectedRegistryWaitsForRegistryAndIdentityWithoutFailingStartup(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "registry.json")
	registry, err := NewProjectedRegistry(path)
	require.NoError(t, err)
	t.Cleanup(registry.Close)
	_, err = LoadRegistry(path)
	require.ErrorIs(t, err, os.ErrNotExist, "manual configuration must remain strict")
	_, err = registry.ProviderSetDigest("node", "node-uid")
	require.ErrorIs(t, err, os.ErrNotExist, "absent optional projection cannot authorize a workload")

	provider := registryTLSFiles(t, directory)
	config := RegistryConfig{Nodes: []NodeProviders{
		{NodeName: "node", NodeUID: "node-uid", Providers: []ProviderConfig{provider}},
	}}
	publishRegistry(t, path, config)
	require.NoError(t, os.Rename(provider.KeyFile, provider.KeyFile+".delayed"))
	_, err = registry.ProviderSetDigest("node", "node-uid")
	require.Error(t, err, "registry arriving before its credential projection cannot authorize")
	require.NoError(t, os.Rename(provider.KeyFile+".delayed", provider.KeyFile))
	digest, err := registry.ProviderSetDigest("node", "node-uid")
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	_, err = registry.ProviderSetDigest("node", "another-incarnation")
	require.Error(t, err)
}

func TestProjectedRegistryDoesNotAcceptMalformedInitialProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0600))
	registry, err := NewProjectedRegistry(path)
	require.NoError(t, err)
	t.Cleanup(registry.Close)
	_, err = registry.ProviderSetDigest("node", "node-uid")
	require.Error(t, err)
	_, err = LoadRegistry(path)
	require.Error(t, err)
	for _, path := range []string{"", "relative/registry.json"} {
		_, err := NewProjectedRegistry(path)
		require.Error(t, err)
	}
	registry.Close()
	_, err = registry.ProviderSetDigest("node", "node-uid")
	require.Error(t, err)
}
