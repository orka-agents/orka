package patchverification

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func applicationClusterReadPolicy(t *testing.T) *ApplicationPolicy {
	t.Helper()
	policy := applicationTestPolicy(t)
	policy.Resources = append(policy.Resources, ApplicationResourceRule{
		APIVersion: "testing.example/v1", Kind: "ClusterWidget", Resource: "clusterwidgets", Namespaced: false,
		Verbs: []string{"get", "list", "watch"},
		CRD: &ApplicationCRDIdentity{Name: "clusterwidgets.testing.example", UID: "approved-cluster-crd",
			SchemaDigest: Digest([]byte("complete-cluster-crd-spec"))},
	})
	policy.SubjectPermissions = append(policy.SubjectPermissions, ApplicationPermission{
		APIGroup: "testing.example", Resources: []string{"clusterwidgets"}, Verbs: []string{"get", "list", "watch"},
	})
	require.NoError(t, ValidateApplicationPolicy(*policy))
	return policy
}

func TestApplicationClusterCustomReadIsSubjectOnlyAndNeverAnObjectPrimitive(t *testing.T) {
	policy := applicationClusterReadPolicy(t)
	plan := *applicationTestManifest(t).Application
	plan.Permissions = []ApplicationPermission{{APIGroup: "testing.example", Resources: []string{"clusterwidgets"},
		Verbs: []string{"get", "list", "watch"}}}
	require.NoError(t, ValidateApplicationPlanWithPolicy(plan, policy))
	raw := json.RawMessage(`{"apiVersion":"testing.example/v1","kind":"ClusterWidget","metadata":{"name":"operator-object"},"spec":{}}`)
	_, err := ValidateApplicationResourceWithPolicy(raw, policy)
	require.Error(t, err, "cluster reads cannot be used to create fixture or actor objects")
	plan.Resources = []json.RawMessage{raw}
	require.Error(t, ValidateApplicationPlanWithPolicy(plan, policy))
	check := applicationTestManifest(t).Checks[0]
	check.Application.Kubernetes = &ApplicationKubernetesCheck{
		APIVersion: "testing.example/v1", Resource: "clusterwidgets", Name: "operator-object", Pointer: "/status/ready",
	}
	require.Error(t, ValidateApplicationCheckWithPolicy(check, policy), "cluster objects are not namespaced observation targets")
	policy.Actors[0].Permissions = append(policy.Actors[0].Permissions, ApplicationPermission{
		APIGroup: "testing.example", Resources: []string{"clusterwidgets"}, Verbs: []string{"get"},
	})
	require.Error(t, ValidateApplicationPolicy(*policy), "only the subject may receive cluster startup reads")
}

func TestApplicationClusterReadRejectsBuiltinAndMutationAuthority(t *testing.T) {
	for _, mutate := range []func(*ApplicationResourceRule){
		func(rule *ApplicationResourceRule) { rule.CRD = nil },
		func(rule *ApplicationResourceRule) { rule.Status = true },
		func(rule *ApplicationResourceRule) { rule.Verbs = []string{"get", "update"} },
		func(rule *ApplicationResourceRule) { rule.Verbs = []string{"get", "patch"} },
		func(rule *ApplicationResourceRule) { rule.Verbs = []string{"get", "create"} },
		func(rule *ApplicationResourceRule) { rule.Verbs = []string{"get", "delete"} },
		func(rule *ApplicationResourceRule) { rule.Verbs = []string{"*"} },
		func(rule *ApplicationResourceRule) {
			rule.APIVersion, rule.Kind, rule.Resource = "v1", "Node", "nodes"
			rule.CRD.Name = "nodes"
		},
		func(rule *ApplicationResourceRule) {
			rule.APIVersion, rule.Kind, rule.Resource = "networking.k8s.io/v1", "IngressClass", "ingressclasses"
			rule.CRD.Name = "ingressclasses.networking.k8s.io"
		},
		func(rule *ApplicationResourceRule) {
			rule.APIVersion, rule.Kind, rule.Resource = "rbac.authorization.k8s.io/v1", "ClusterRole", "clusterroles"
			rule.CRD.Name = "clusterroles.rbac.authorization.k8s.io"
		},
	} {
		policy := applicationClusterReadPolicy(t)
		mutate(&policy.Resources[len(policy.Resources)-1])
		require.Error(t, ValidateApplicationPolicy(*policy))
	}
}

func TestApplicationResourceScopeMustBeExplicitBoolean(t *testing.T) {
	policy := applicationClusterReadPolicy(t)
	rule := policy.Resources[len(policy.Resources)-1]
	content, err := json.Marshal(rule)
	require.NoError(t, err)
	var parsed ApplicationResourceRule
	require.NoError(t, json.Unmarshal(content, &parsed))
	require.Equal(t, rule, parsed)
	for _, replacement := range []string{"", `"namespaced":null,`, `"namespaced":"false",`, `"namespaced":0,`} {
		raw := strings.Replace(string(content), `"namespaced":false,`, replacement, 1)
		require.Error(t, json.Unmarshal([]byte(raw), &parsed))
	}
	raw := strings.Replace(string(content), `"namespaced":false`, `"namespaced":false,"unknown":true`, 1)
	require.Error(t, json.Unmarshal([]byte(raw), &parsed))
	rule.Namespaced = true
	content, err = json.Marshal(rule)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(content, &parsed))
	again, err := json.Marshal(parsed)
	require.NoError(t, err)
	require.Equal(t, content, again, "valid pre-existing true-bearing canonical policy JSON must not change")
}

func TestApplicationSubjectEnvironmentIsOperatorNamespaceOnly(t *testing.T) {
	policy := applicationTestPolicy(t)
	policy.SubjectEnvironment = map[string]string{"WATCH_NAMESPACE": "{{namespace}}", "OTHER_NAMESPACE": "{{namespace}}"}
	require.NoError(t, ValidateApplicationPolicy(*policy))
	variables, err := ApplicationSubjectEnvironmentVariables(policy, "run-owned")
	require.NoError(t, err)
	require.Equal(t, []string{"OTHER_NAMESPACE=run-owned", "WATCH_NAMESPACE=run-owned"}, variables)
	first, err := ApplicationPolicyDigest(*policy)
	require.NoError(t, err)
	delete(policy.SubjectEnvironment, "OTHER_NAMESPACE")
	second, err := ApplicationPolicyDigest(*policy)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "operator environment must participate in the frozen policy identity")
	for _, name := range []string{
		"POD_NAMESPACE", "ORKA_APPLICATION_DEADLINE", "KUBERNETES_SERVICE_HOST", "KUBECONFIG",
		"PATH", "HOME", "TMPDIR", "ENV", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "GODEBUG",
		"CGO_ENABLED", "PYTHONPATH", "RUBYOPT", "PERL5OPT", "JAVA_TOOL_OPTIONS", "NODE_OPTIONS",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE",
		"SERVICE_TOKEN", "PASSWORD", "CLIENT_SECRET", "ACCESS_KEY_ID", "bad-name", strings.Repeat("X", 65),
	} {
		policy.SubjectEnvironment = map[string]string{name: "{{namespace}}"}
		require.Error(t, ValidateApplicationPolicy(*policy))
		_, err := ApplicationSubjectEnvironmentVariables(policy, "run-owned")
		require.Error(t, err)
	}
	for _, value := range []string{"literal", "", "${namespace}", "{{namespace}},other", "../outside"} {
		policy.SubjectEnvironment = map[string]string{"WATCH_NAMESPACE": value}
		require.Error(t, ValidateApplicationPolicy(*policy))
	}

	policy.SubjectEnvironment = map[string]string{}
	for index := range 8 {
		policy.SubjectEnvironment[fmt.Sprintf("WATCH_%d_NAMESPACE", index)] = "{{namespace}}"
	}
	require.NoError(t, ValidateApplicationPolicy(*policy))
	policy.SubjectEnvironment["NINTH_NAMESPACE"] = "{{namespace}}"
	require.Error(t, ValidateApplicationPolicy(*policy))
}
