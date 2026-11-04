package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSubstrateRouterURLRejectsComponentsUnsafeForToolStatus(t *testing.T) {
	t.Parallel()

	valid := SubstrateConfig{APIInsecureSkipVerify: true, APIBearerTokenFile: "/var/run/substrate/token"}
	for _, routerURL := range []string{
		"http://atenet-router.ate-system.svc",
		"https://router.example.test:8443/prefix/",
	} {
		cfg := valid
		cfg.RouterURL = routerURL
		require.NoError(t, cfg.ValidateMCPTools(), routerURL)
	}

	for name, routerURL := range map[string]string{
		"password":       "http://operator:hunter2@router.example.test",
		"username":       "http://operator@router.example.test",
		"query":          "http://router.example.test?token=hunter2",
		"empty query":    "http://router.example.test?",
		"fragment":       "http://router.example.test#hunter2",
		"empty fragment": "http://router.example.test#",
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
