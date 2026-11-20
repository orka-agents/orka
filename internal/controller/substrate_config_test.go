package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSubstrateRouterURLRejectsComponentsUnsafeForToolStatus(t *testing.T) {
	t.Parallel()

	valid := SubstrateConfig{
		APIInsecureSkipVerify: true,
		APICertFile:           "/run/substrate-client/credential-bundle.pem",
		APIKeyFile:            "/run/substrate-client/credential-bundle.pem",
	}
	for _, routerURL := range []string{
		"https://atenet-router.ate-system.svc:443",
		"https://router.example.test:8443/prefix/",
	} {
		cfg := valid
		cfg.RouterURL = routerURL
		require.NoError(t, cfg.ValidateMCPTools(), routerURL)
	}

	// Substrate v1 serves Actor traffic only over mTLS.
	plaintext := valid
	plaintext.RouterURL = "http://atenet-router.ate-system.svc"
	require.ErrorContains(t, plaintext.ValidateMCPTools(), "must be an https URL")

	for name, routerURL := range map[string]string{
		"password":       "https://operator:hunter2@router.example.test",
		"username":       "https://operator@router.example.test",
		"query":          "https://router.example.test?token=hunter2",
		"empty query":    "https://router.example.test?",
		"fragment":       "https://router.example.test#hunter2",
		"empty fragment": "https://router.example.test#",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			cfg.RouterURL = routerURL
			err := cfg.ValidateMCPTools()
			require.ErrorContains(t, err, "substrate router URL must not contain credentials, a query, or a fragment")
			require.NotContains(t, err.Error(), "hunter2")
		})
	}
}

func TestSubstrateRouterRequiresPodCertificateIdentity(t *testing.T) {
	t.Parallel()

	// A bearer token authenticates control calls, but the router admits only
	// clients presenting an allowlisted pod-certificate SPIFFE identity.
	cfg := SubstrateConfig{APIInsecureSkipVerify: true, APIBearerTokenFile: "/var/run/substrate/token"}
	require.ErrorContains(t, cfg.ValidateMCPTools(), "pod-certificate identity")
}

func TestSubstrateConfigRejectsRemovedV010Settings(t *testing.T) {
	t.Parallel()

	for _, name := range removedSubstrateEnv {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := SubstrateConfigFromEnv(func(key string) string {
				if key == name {
					return "configured"
				}
				return ""
			})
			require.ErrorContains(t, err, name)
		})
	}
	cfg, err := SubstrateConfigFromEnv(func(string) string { return "" })
	require.NoError(t, err)
	require.Equal(t, "https://atenet-router.ate-system.svc:443", cfg.RouterURL)
}
