package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func newExternalUsageAuthorizationFixture(t *testing.T) *externalAuthorizationFixture {
	t.Helper()
	f := newExternalAuthorizationFixture(t)
	f.server.handlers.watchNamespace = ""
	f.server.handlers.enforceNamespaceIsolation = false
	seedUsageAPIWork(t, f.store, "default", 9000)
	for _, namespace := range []string{"payments", "platform"} {
		require.NoError(t, f.kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: namespace, UID: types.UID(namespace + "-namespace-uid"),
		}}))
		seedUsageAPIWork(t, f.store, namespace, 100)
	}
	return f
}

func externalUsagePermissions(namespaces ...string) []authorizationv1.ResourceAttributes {
	permissions := make([]authorizationv1.ResourceAttributes, 0, 3*len(namespaces))
	for _, namespace := range namespaces {
		for _, resource := range []string{"tasks", "repositorymonitors", "sessions"} {
			permissions = append(permissions, authorizationv1.ResourceAttributes{
				Namespace: namespace, Group: corev1alpha1.GroupVersion.Group, Resource: resource, Verb: "list",
			})
		}
	}
	return permissions
}

func externalUsagePaths(namespace string) []string {
	return []string{
		"/api/v1/usage",
		"/api/v1/usage/work/" + store.UsageWorkID(namespace, "monitor", "org/repo", "issue", 1),
		"/api/v1/usage/other/unassociated",
	}
}

func TestExternalAPIUsageAuthorizesOnlySelectedTeams(t *testing.T) {
	for _, tc := range []struct {
		name       string
		query      string
		namespaces []string
	}{
		{"single team", "teams=payments", []string{"payments"}},
		{"multiple teams without default grant", "teams=payments,platform", []string{"payments", "platform"}},
		{"teams override namespace query", "namespace=unselected&teams=payments,platform", []string{"payments", "platform"}},
		{"teams are trimmed sorted and deduplicated", "teams=+platform+,+payments+,payments", []string{"payments", "platform"}},
		{"namespace fallback", "namespace=payments", []string{"payments"}},
		{"default fallback", "", []string{"default"}},
		{"empty teams fallback", "teams=+", []string{"default"}},
	} {
		for _, path := range externalUsagePaths(tc.namespaces[0]) {
			t.Run(tc.name+path, func(t *testing.T) {
				f := newExternalUsageAuthorizationFixture(t)
				permissions := externalUsagePermissions(tc.namespaces...)
				f.allowOnly(t, permissions...)
				status, body := f.request(t, http.MethodGet, path+"?from=2000-01-01&"+tc.query, "")
				require.Equal(t, http.StatusOK, status, body)
				var response struct {
					Selection store.UsageFilter `json:"selection"`
				}
				require.NoError(t, json.Unmarshal([]byte(body), &response))
				require.Equal(t, tc.namespaces, response.Selection.Namespaces)
				reviewed := make([]authorizationv1.ResourceAttributes, 0, len(f.reviews))
				for _, review := range f.reviews {
					reviewed = append(reviewed, *review.ResourceAttributes)
				}
				require.ElementsMatch(t, permissions, reviewed, "each selected-team permission must be checked exactly once")
			})
		}
	}
}

func TestExternalAPIUsageRequiresEverySelectedTeamPermission(t *testing.T) {
	permissions := externalUsagePermissions("payments", "platform")
	for _, path := range externalUsagePaths("payments") {
		for i, denied := range permissions {
			t.Run(path+"/"+denied.Namespace+"/"+denied.Resource, func(t *testing.T) {
				f := newExternalUsageAuthorizationFixture(t)
				f.allowOnly(t, slices.Delete(slices.Clone(permissions), i, i+1)...)
				f.kubeCalls = 0
				before := f.changes(t)
				status, body := f.request(t, http.MethodGet, path+"?teams=payments,platform&from=2000-01-01", "")
				require.Equal(t, http.StatusForbidden, status, body)
				require.NotContains(t, body, `"selection"`)
				require.NotEmpty(t, f.reviews)
				require.Equal(t, denied, *f.reviews[len(f.reviews)-1].ResourceAttributes)
				require.Zero(t, f.kubeCalls, "denial must happen before loading report data")
				require.Equal(t, before, f.changes(t))
			})
		}
	}
}

func TestExternalAPIUsageSelectedTeamsPreserveNamespaceIsolation(t *testing.T) {
	for _, tc := range []struct {
		name           string
		watchNamespace string
		isolation      bool
	}{
		{"watch namespace", "default", false},
		{"identity namespace", "", true},
	} {
		for _, path := range externalUsagePaths("default") {
			t.Run(tc.name+path, func(t *testing.T) {
				f := newExternalUsageAuthorizationFixture(t)
				f.server.handlers.watchNamespace = tc.watchNamespace
				f.server.handlers.enforceNamespaceIsolation = tc.isolation
				f.allowOnly(t, externalUsagePermissions("default", "payments")...)
				f.kubeCalls = 0
				status, body := f.request(t, http.MethodGet, path+"?teams=default,payments", "")
				require.Equal(t, http.StatusForbidden, status, body)
				require.Empty(t, f.reviews)
				require.Zero(t, f.kubeCalls)
			})
		}
	}
}
