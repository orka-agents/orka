//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"slices"
	"strings"
	"testing"
)

func TestGatewayE2EFixtureArgsPinsNativeWorker(t *testing.T) {
	originalRef := gatewayNativeWorkerRef
	gatewayNativeWorkerRef = "localhost:5000/orka/gateway-e2e-worker@sha256:" + strings.Repeat("a", 64)
	t.Cleanup(func() { gatewayNativeWorkerRef = originalRef })

	preserved := []string{
		"--general-worker-image=localhost:5000/orka/general-worker@sha256:" + strings.Repeat("b", 64),
		"--gateway-enabled=true",
	}
	for _, tc := range []struct {
		name     string
		override []string
	}{
		{name: "no overrides"},
		{
			name: "joined overrides",
			override: []string{
				"--ai-worker-image=" + aiWorkerImage,
				"--gateway-terminal-retention=1h",
			},
		},
		{
			name: "split overrides",
			override: []string{
				"--ai-worker-image", aiWorkerImage,
				"--gateway-terminal-retention", "1h",
			},
		},
		{
			name: "repeated overrides",
			override: []string{
				"--ai-worker-image", aiWorkerImage,
				"--ai-worker-image=" + gatewayNativeWorkerImage,
				"--gateway-terminal-retention=1h",
				"--gateway-terminal-retention", "2h",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := append(slices.Clone(preserved), tc.override...)
			before := slices.Clone(original)
			want := append(slices.Clone(preserved),
				"--ai-worker-image="+gatewayNativeWorkerRef,
				"--gateway-terminal-retention="+gatewayE2ETerminalRetention.String(),
			)

			if got := gatewayE2EFixtureArgs(original); !slices.Equal(got, want) {
				t.Fatalf("Gateway fixture args = %v, want %v", got, want)
			}
			if !slices.Equal(original, before) {
				t.Fatalf("original manager args changed from %v to %v", before, original)
			}
		})
	}
}
