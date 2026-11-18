package applicationexec

import (
	"testing"

	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIssuerDiscoveryDefaultGrantAllowsQualification(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra string
	}{
		{name: "default"},
		{name: "unrelated-endpoint", extra: "/metrics"},
		{name: "wildcard", extra: "/openid/*"},
		{name: "child-path", extra: "/openid/v1/jwks/private"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:service-account-issuer-discovery"},
				Rules: []rbacv1.PolicyRule{{
					Verbs: []string{"get"},
					NonResourceURLs: []string{
						"/.well-known/openid-configuration", "/.well-known/openid-configuration/",
						"/openid/v1/jwks", "/openid/v1/jwks/",
					},
				}}}
			if test.extra != "" {
				role.Rules[0].NonResourceURLs = append(role.Rules[0].NonResourceURLs, test.extra)
			}
			require.NoError(t, f.target.Create(t.Context(), role))
			require.NoError(t, f.target.Create(t.Context(), &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: role.Name},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
				Subjects:   []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "system:serviceaccounts"}},
			}))
			before := len(f.created)
			err := f.backend.CheckReadiness(t.Context())
			if test.extra != "" {
				require.ErrorIs(t, err, ErrIsolation)
				require.Empty(t, f.backend.EvidenceScope())
				require.Len(t, f.created, before, "unrelated additive authority must be rejected before target effects")
			} else {
				require.NoError(t, err)
				require.Equal(t, contract.ApplicationRuntime, f.backend.EvidenceScope())
				require.Greater(t, len(f.created), before, "the admitted default grant must reach actual qualification")
			}
			require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
		})
	}
}
