package enforcement

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func registryTLSFiles(t *testing.T, directory string) ProviderConfig {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test registry CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	root, err := x509.CreateCertificate(rand.Reader, ca, ca, public, private)
	require.NoError(t, err)
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "test controller"},
		NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, leaf, ca, clientPublic, private)
	require.NoError(t, err)
	key, err := x509.MarshalPKCS8PrivateKey(clientPrivate)
	require.NoError(t, err)
	config := ProviderConfig{
		Name: "filesystem", Endpoint: "passthrough:///127.0.0.1:1", ServerName: "node-uid.provider.test",
		CAFile: filepath.Join(directory, "ca.pem"), CertificateFile: filepath.Join(directory, "controller.pem"),
		KeyFile: filepath.Join(directory, "controller-key.pem"),
	}
	for path, block := range map[string]*pem.Block{
		config.CAFile:          {Type: "CERTIFICATE", Bytes: root},
		config.CertificateFile: {Type: "CERTIFICATE", Bytes: certificate},
		config.KeyFile:         {Type: "PRIVATE KEY", Bytes: key},
	} {
		require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0600))
	}
	return config
}

func TestRegistryPinsLoadedTrustAndExactNodeIncarnations(t *testing.T) {
	directory := t.TempDir()
	filesystem := registryTLSFiles(t, directory)
	network := filesystem
	network.Name, network.Endpoint = "network", "passthrough:///127.0.0.1:2"
	config := RegistryConfig{Nodes: []NodeProviders{
		{NodeName: "node", NodeUID: "node-uid", Providers: []ProviderConfig{network, filesystem}},
		{NodeName: "node", NodeUID: "old-node-uid", Providers: []ProviderConfig{filesystem}},
	}}
	registry, err := NewRegistry(config)
	require.NoError(t, err)
	t.Cleanup(registry.Close)
	digest, err := registry.ProviderSetDigest("node", "node-uid")
	require.NoError(t, err)
	_, err = registry.ProviderSetDigest("node", "old-node-uid")
	require.NoError(t, err, "old incarnation remains explicitly routable for cleanup")
	_, err = registry.ProviderSetDigest("node", "replacement-node-uid")
	require.Error(t, err)
	_, err = registry.ProviderSetDigest("different-node", "node-uid")
	require.Error(t, err)

	slices.Reverse(config.Nodes[0].Providers)
	reordered, err := NewRegistry(config)
	require.NoError(t, err)
	t.Cleanup(reordered.Close)
	reorderedDigest, err := reordered.ProviderSetDigest("node", "node-uid")
	require.NoError(t, err)
	require.Equal(t, digest, reorderedDigest, "provider order does not change the mandatory set")

	registryTLSFiles(t, directory)
	currentDigest, err := registry.ProviderSetDigest("node", "node-uid")
	require.NoError(t, err)
	require.Equal(t, digest, currentDigest, "identity hashes the actual loaded trust, not a reread projected file")
	rotated, err := NewRegistry(config)
	require.NoError(t, err)
	t.Cleanup(rotated.Close)
	rotatedDigest, err := rotated.ProviderSetDigest("node", "node-uid")
	require.NoError(t, err)
	require.NotEqual(t, digest, rotatedDigest)
	binding := &corev1alpha1.ContainerEnforcementBinding{
		NodeName: "node", NodeUID: "node-uid", ProviderSetDigest: digest, BindingID: "binding",
	}
	_, err = rotated.boundProviders(binding)
	require.Error(t, err, "changed trust cannot silently take over an existing cleanup obligation")
}

func TestRegistryRejectsAmbiguousOperatorConfiguration(t *testing.T) {
	provider := registryTLSFiles(t, t.TempDir())
	for name, config := range map[string]RegistryConfig{
		"empty":                  {},
		"no mandatory providers": {Nodes: []NodeProviders{{NodeName: "node", NodeUID: "uid"}}},
		"duplicate node identity": {Nodes: []NodeProviders{
			{NodeName: "node", NodeUID: "uid", Providers: []ProviderConfig{provider}},
			{NodeName: "other", NodeUID: "uid", Providers: []ProviderConfig{provider}},
		}},
		"duplicate provider": {Nodes: []NodeProviders{{
			NodeName: "node", NodeUID: "uid", Providers: []ProviderConfig{provider, provider},
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			registry, err := NewRegistry(config)
			require.Error(t, err)
			require.Nil(t, registry)
		})
	}
	for name, value := range map[string]string{
		"unknown field": `{"nodes":[],"untrustedRouting":true}`,
		"two objects":   `{"nodes":[]} {"nodes":[]}`,
		"oversized":     string(make([]byte, (1<<20)+1)),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			require.NoError(t, os.WriteFile(path, []byte(value), 0600))
			registry, err := LoadRegistry(path)
			require.Error(t, err)
			require.Nil(t, registry)
		})
	}
}

func TestRegistryRetirementRequiresEveryMandatoryProvider(t *testing.T) {
	for _, name := range []string{"pending", "refused", "mismatched", "retired", "trust-changed"} {
		t.Run(name, func(t *testing.T) {
			binding := &corev1alpha1.ContainerEnforcementBinding{
				BindingID: "binding", NodeName: "node", NodeUID: "node-uid", ProviderSetDigest: "trust",
			}
			retired := &enforcementv1.RetireResponse{Result: &enforcementv1.RetireResponse_Retired{
				Retired: &enforcementv1.Retired{BindingId: binding.BindingID},
			}}
			first := &recordingProvider{retire: retired}
			second := &recordingProvider{retire: retired}
			switch name {
			case "pending":
				second.retire = &enforcementv1.RetireResponse{Result: &enforcementv1.RetireResponse_Pending{
					Pending: &enforcementv1.Pending{Reason: "physical target remains live"},
				}}
			case "refused":
				second.retire = &enforcementv1.RetireResponse{Result: &enforcementv1.RetireResponse_Refused{
					Refused: &enforcementv1.Refused{Reason: "identity refused"},
				}}
			case "mismatched":
				second.retire = &enforcementv1.RetireResponse{Result: &enforcementv1.RetireResponse_Retired{
					Retired: &enforcementv1.Retired{BindingId: "different-binding"},
				}}
			case "trust-changed":
				binding.ProviderSetDigest = "different-trust"
			}
			registry := &Registry{nodes: map[string]*nodeProviders{
				"node-uid": {name: "node", digest: "trust", clients: []*Client{
					{name: "first", rpc: first}, {name: "second", rpc: second},
				}},
			}}
			done, err := registry.Retire(t.Context(), binding)
			require.Equal(t, name == "retired", done)
			require.Equal(t, name == "refused" || name == "mismatched" || name == "trust-changed", err != nil)
		})
	}
}
