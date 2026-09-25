package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"yaml", "namespaces:\n  team-a: http://orka-api.team-a.svc:8080\n", true},
		{"json", `{"namespaces":{"team-a":"http://orka-api.team-a.svc:8080"}}`, true},
		{"unknown field", "namespace: team-a", false},
		{"duplicate namespace", "namespaces:\n  team-a: http://first\n  team-a: http://second\n", false},
		{"invalid syntax", "namespaces: [", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "routes.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0600))
			routes, err := loadRoutes(path)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, map[string]string{"team-a": "http://orka-api.team-a.svc:8080"}, routes)
		})
	}
	_, err := loadRoutes("")
	require.Error(t, err)
}
