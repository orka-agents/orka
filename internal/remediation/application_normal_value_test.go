package remediation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func TestApplicationNormalValueMustBeExplicitMeaningfulAndMatchHealthy(t *testing.T) {
	for _, invalid := range []string{
		"", "null", `""`, `"   "`, "[]", "{}", `{"items":[]}`, `[{}]`, `"mismatched"`,
	} {
		t.Run(invalid, func(t *testing.T) {
			proposal := applicationProposal(t)
			for index := range proposal.Checks {
				if proposal.Checks[index].Kind == pv.Normal {
					proposal.Checks[index].Application.Kubernetes.NormalValue = json.RawMessage(invalid)
				}
			}
			_, err := decodeApplicationProposal(t, proposal)
			require.Error(t, err)
		})
	}
}

func TestApplicationNormalValueDoesNotRewriteFalseOrZero(t *testing.T) {
	for _, value := range []string{"false", "0"} {
		t.Run(value, func(t *testing.T) {
			proposal := applicationProposal(t)
			healthy, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{
				Exists: true, Found: true, Value: json.RawMessage(value),
			})
			require.NoError(t, err)
			for index := range proposal.Checks {
				if proposal.Checks[index].Kind == pv.Normal {
					proposal.Checks[index].Application.Kubernetes.NormalValue = json.RawMessage(value)
					proposal.Checks[index].Healthy = healthy
				}
			}
			decoded, err := decodeApplicationProposal(t, proposal)
			require.NoError(t, err)
			for _, check := range decoded.Checks {
				if check.Kind == pv.Normal {
					require.Equal(t, json.RawMessage(value), check.Application.Kubernetes.NormalValue)
					require.Equal(t, healthy, check.Healthy)
				}
			}
		})
	}
}
