package remediation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func testApplicationPolicy() *pv.ApplicationPolicy {
	return &pv.ApplicationPolicy{
		Version: 1,
		Resources: []pv.ApplicationResourceRule{{
			APIVersion: "applications.example.test/v1", Kind: "Widget", Resource: "widgets", Namespaced: true,
			Verbs: []string{"get", "list", "watch", "create", "update", "patch"}, Status: true,
			CRD: &pv.ApplicationCRDIdentity{Name: "widgets.applications.example.test",
				UID: "approved-crd-uid", SchemaDigest: pv.Digest([]byte("synthetic complete CRD spec"))},
		}},
		SubjectPermissions: []pv.ApplicationPermission{{
			APIGroup: "applications.example.test", Resources: []string{"widgets"}, Verbs: []string{"get", "list", "watch"},
		}},
		Actors: []pv.ApplicationActorPolicy{{
			ID: "reporter", Permissions: []pv.ApplicationPermission{{
				APIGroup: "applications.example.test", Resources: []string{"widgets"}, Verbs: []string{"create", "update"},
			}},
		}},
	}
}

func TestApplicationPolicyIsTrustedInputNotModelAuthority(t *testing.T) {
	policy := testApplicationPolicy()
	policy.SubjectPermissions = append(policy.SubjectPermissions, pv.ApplicationPermission{
		APIGroup: "applications.example.test", Resources: []string{"widgets/status"},
		Verbs: []string{"update", "patch"}, ResourceNames: []string{"synthetic"},
	})
	encoded, err := json.Marshal(policy)
	require.NoError(t, err)
	decoded, err := DecodeApplicationPolicy(encoded)
	require.NoError(t, err)
	require.Equal(t, policy, decoded)
	proposal := applicationProposal(t)
	proposal.Application.Resources = []json.RawMessage{json.RawMessage(
		`{"apiVersion":"applications.example.test/v1","kind":"Widget","metadata":{"name":"synthetic"},"spec":{"mode":"ordinary"}}`,
	)}
	proposal.Application.Permissions = policy.SubjectPermissions
	for index := range proposal.Checks {
		proposal.Checks[index].Application.Kubernetes = &pv.ApplicationKubernetesCheck{
			APIVersion: "applications.example.test/v1", Resource: "widgets", Name: "synthetic", Pointer: "/status/conditions",
		}
		if proposal.Checks[index].Kind == pv.Normal {
			proposal.Checks[index].Application.Kubernetes.NormalValue =
				json.RawMessage(`[{"hostname":"ready.invalid"}]`)
		}
	}
	raw, err := json.Marshal(proposal)
	require.NoError(t, err)
	_, err = DecodeCheckProposal(string(raw))
	require.Error(t, err, "custom resource syntax cannot grant its own authority")
	frozen, err := DecodeApplicationCheckProposal(string(raw), decoded)
	require.NoError(t, err)
	_, err = FreezeApplicationProposalFiles(frozen, decoded)
	require.NoError(t, err)
	for _, field := range []string{"policy", "applicationPolicy", "operatorPolicy"} {
		var data map[string]any
		require.NoError(t, json.Unmarshal(raw, &data))
		data[field] = policy
		injected, err := json.Marshal(data)
		require.NoError(t, err)
		_, err = DecodeApplicationCheckProposal(string(injected), decoded)
		require.Error(t, err, "proposal must not contain a policy field")
	}
	require.NotContains(t, string(raw), "schemaDigest", "operator authority is not copied into the proposal")
}

func TestOperatorApplicationPolicyIsStrictAndDoesNotEchoInput(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `[]`,
		`{"version":1,"version":1,"resources":[]}`,
		`{"version":1,"resources":[],"privateDetail":"synthetic-private-do-not-repeat"}`,
		`{"version":1,"resources":[{"apiVersion":"v1","kind":"Pod","resource":"pods","namespaced":false,"verbs":["*"]}]}`,
	} {
		_, err := DecodeApplicationPolicy([]byte(raw))
		require.Error(t, err)
		require.NotContains(t, err.Error(), "synthetic-private-do-not-repeat")
	}
	_, err := DecodeApplicationPolicy([]byte(strings.Repeat("x", maxProposalBytes+1)))
	require.Error(t, err)
}
