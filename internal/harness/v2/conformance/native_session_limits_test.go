package conformance_test

import (
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/conformance"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
	"github.com/stretchr/testify/require"
)

func TestNativeSessionAdditiveCapabilityRegistration(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		registered, advertised int
		want                   bool
	}{
		{name: "legacy registration new runtime", advertised: 8 << 20, want: true},
		{name: "legacy runtime", want: true},
		{name: "explicit same", registered: 8 << 20, advertised: 8 << 20, want: true},
		{name: "explicit drift", registered: 8 << 20, advertised: 16 << 20},
		{name: "invalid advertised", advertised: harnessv2.MaxNativeSessionBytes + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, config := testTargetAndConfig(t)
			target.Limits.MaxNativeSessionBytes = tc.registered
			config.Limits.MaxNativeSessionBytes = tc.advertised
			server, err := conformancetest.NewServer(config)
			require.NoError(t, err)
			defer server.Close()
			target.BaseURL = server.URL()
			result := conformance.Check(t.Context(), target)
			require.Equal(t, tc.want, result.Passed, result.Message)
		})
	}
}
