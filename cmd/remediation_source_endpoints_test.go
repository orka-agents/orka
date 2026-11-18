package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplicationSourceEndpointFlagsRejectAmbientOrBroadRoutes(t *testing.T) {
	for _, invalid := range []string{
		"", "github.com", "github.com=", "github.com=github.com",
		"https://github.com=192.0.2.31", "*.github.com=192.0.2.31",
		"github.com.example.test=192.0.2.31", "codeload.github.com=192.0.2.31",
		"github.com=192.0.2.31:443", "github.com=127.0.0.1",
		"github.com=0.0.0.0", "github.com=224.0.0.1",
		"github.com=::1", "github.com=fe80::1%eth0",
	} {
		t.Run(invalid, func(t *testing.T) {
			var endpoints remediationSourceEndpoints
			require.Error(t, endpoints.Set(invalid))
			require.Empty(t, endpoints)
		})
	}
}

func TestApplicationSourceEndpointsAreExplicitBoundedAndOptIn(t *testing.T) {
	options := remediationApplicationOptions{Platform: "linux/amd64"}
	require.False(t, options.configured())
	require.Empty(t, options.SourceEndpoints.String())
	require.NoError(t, options.SourceEndpoints.Set("github.com=192.0.2.31"))
	require.NoError(t, options.SourceEndpoints.Set("api.github.com=2001:db8::31"))
	require.True(t, options.configured())
	require.Equal(t, "github.com=192.0.2.31,api.github.com=2001:db8::31", options.SourceEndpoints.String())
	for len(options.SourceEndpoints) < 8 {
		require.NoError(t, options.SourceEndpoints.Set("github.com=192.0.2.31"))
	}
	require.Error(t, options.SourceEndpoints.Set("github.com=192.0.2.32"))
	require.Len(t, options.SourceEndpoints, 8)
}
