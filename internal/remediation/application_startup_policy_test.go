package remediation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func clusterReadStartupPolicy() *pv.ApplicationPolicy {
	policy := testApplicationPolicy()
	policy.Resources[0].Namespaced = false
	policy.Resources[0].Status = false
	policy.Resources[0].Verbs = []string{"get", "list", "watch"}
	policy.Actors = nil
	policy.SubjectEnvironment = map[string]string{"APP_NAMESPACE": "{{namespace}}"}
	return policy
}

func encodeStartupPolicy(t *testing.T, value any) []byte {
	t.Helper()
	content, err := json.Marshal(value)
	require.NoError(t, err)
	return content
}

func TestOperatorStartupPolicyAdmitsOnlyExplicitClusterReadsAndNamespaceTemplate(t *testing.T) {
	policy := clusterReadStartupPolicy()
	decoded, err := DecodeApplicationPolicy(encodeStartupPolicy(t, policy))
	require.NoError(t, err)
	require.Equal(t, policy, decoded)
	require.Equal(t, "{{namespace}}", decoded.SubjectEnvironment["APP_NAMESPACE"])
}

func TestOperatorPolicyCannotDefaultMissingScopeToClusterAuthority(t *testing.T) {
	for _, invalid := range []any{nil, "false", 0, "missing"} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			var data map[string]any
			require.NoError(t, json.Unmarshal(encodeStartupPolicy(t, clusterReadStartupPolicy()), &data))
			resource := data["resources"].([]any)[0].(map[string]any)
			if invalid == "missing" {
				delete(resource, "namespaced")
			} else {
				resource["namespaced"] = invalid
			}
			_, err := DecodeApplicationPolicy(encodeStartupPolicy(t, data))
			require.Error(t, err)
		})
	}
}

func TestOperatorClusterReadPolicyRejectsBroaderAuthority(t *testing.T) {
	for _, invalid := range []string{
		"missing CRD", "core secret", "core node", "status", "actor",
		"create", "update", "patch", "delete", "*",
	} {
		t.Run(invalid, func(t *testing.T) {
			policy := clusterReadStartupPolicy()
			switch invalid {
			case "missing CRD":
				policy.Resources[0].CRD = nil
			case "core secret", "core node":
				kind, resource := "Secret", "secrets"
				if invalid == "core node" {
					kind, resource = "Node", "nodes"
				}
				policy.Resources[0] = pv.ApplicationResourceRule{
					APIVersion: "v1", Kind: kind, Resource: resource, Namespaced: false, Verbs: []string{"get"},
				}
				policy.SubjectPermissions = []pv.ApplicationPermission{{Resources: []string{resource}, Verbs: []string{"get"}}}
			case "status":
				policy.Resources[0].Status = true
			case "actor":
				policy.Actors = []pv.ApplicationActorPolicy{{ID: "reader", Permissions: policy.SubjectPermissions}}
			default:
				policy.Resources[0].Verbs = append(policy.Resources[0].Verbs, invalid)
			}
			_, err := DecodeApplicationPolicy(encodeStartupPolicy(t, policy))
			require.Error(t, err)
		})
	}
}

func TestOperatorSubjectEnvironmentRejectsLiteralsReservedNamesAndOversize(t *testing.T) {
	for _, name := range []string{
		"", "NOT-A-VARIABLE", strings.Repeat("X", 65), "PATH", "HOME", "TMPDIR",
		"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "GODEBUG", "HTTP_PROXY", "https_proxy",
		"SSL_CERT_FILE", "KUBECONFIG", "KUBERNETES_SERVICE_HOST",
		"POD_NAMESPACE", "ORKA_APPLICATION_DEADLINE", "APP_PASSWORD",
	} {
		t.Run("name/"+name, func(t *testing.T) {
			policy := clusterReadStartupPolicy()
			policy.SubjectEnvironment = map[string]string{name: "{{namespace}}"}
			_, err := DecodeApplicationPolicy(encodeStartupPolicy(t, policy))
			require.Error(t, err)
		})
	}
	for _, value := range []string{"", "literal-namespace", "{{namespace}}-suffix", " {{namespace}}", "${NAMESPACE}"} {
		t.Run("value/"+value, func(t *testing.T) {
			policy := clusterReadStartupPolicy()
			policy.SubjectEnvironment = map[string]string{"APP_NAMESPACE": value}
			_, err := DecodeApplicationPolicy(encodeStartupPolicy(t, policy))
			require.Error(t, err)
		})
	}
	policy := clusterReadStartupPolicy()
	policy.SubjectEnvironment = make(map[string]string)
	for index := range 8 {
		policy.SubjectEnvironment[fmt.Sprintf("APP_NAMESPACE_%d", index)] = "{{namespace}}"
	}
	_, err := DecodeApplicationPolicy(encodeStartupPolicy(t, policy))
	require.NoError(t, err)
	policy.SubjectEnvironment["APP_NAMESPACE_8"] = "{{namespace}}"
	_, err = DecodeApplicationPolicy(encodeStartupPolicy(t, policy))
	require.Error(t, err)
}

func TestApplicationProposalCannotSetSubjectEnvironment(t *testing.T) {
	for _, field := range []string{"env", "subjectEnvironment", "environment"} {
		t.Run(field, func(t *testing.T) {
			var proposal map[string]any
			require.NoError(t, json.Unmarshal(encodeStartupPolicy(t, applicationProposal(t)), &proposal))
			proposal["application"].(map[string]any)["runtime"].(map[string]any)[field] =
				map[string]string{"APP_NAMESPACE": "{{namespace}}"}
			_, err := DecodeApplicationCheckProposal(string(encodeStartupPolicy(t, proposal)), nil)
			require.Error(t, err)
		})
	}
}
