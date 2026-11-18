package applicationexec

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type runPrincipals struct {
	namespaces []string
	accounts   []string
}

func principalsFor(r *run) runPrincipals {
	principals := runPrincipals{
		namespaces: []string{namespaceName(r, "qualification")},
		accounts:   []string{"orka-worker", subjectAccountName, observerAccountName, markerAccountName, replicaAccountName},
	}
	if !r.state.Qualification {
		principals.namespaces = nil
		for _, side := range pv.ActionSides(r.state.Request.Action) {
			principals.namespaces = append(principals.namespaces, namespaceName(r, side))
			if r.state.Request.ApplicationPolicy != nil {
				principals.namespaces = append(principals.namespaces, namespaceName(r, protectedSide(side)))
			}
		}
	}
	if r.state.Request.ApplicationPolicy != nil {
		for _, actor := range r.state.Request.ApplicationPolicy.Actors {
			principals.accounts = append(principals.accounts, actorName(actor.ID))
		}
	}
	return principals
}

func (principals runPrincipals) matches(subjects []rbacv1.Subject, bindingNamespace string) bool {
	for _, subject := range subjects {
		for _, namespace := range principals.namespaces {
			if subject.Kind == "Group" && slices.Contains([]string{
				"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + namespace,
			}, subject.Name) {
				return true
			}
			for _, account := range principals.accounts {
				switch subject.Kind {
				case serviceAccountKind:
					ns := subject.Namespace
					if ns == "" {
						ns = bindingNamespace
					}
					if subject.Name == account && ns == namespace {
						return true
					}
				case "User":
					if subject.Name == "system:serviceaccount:"+namespace+":"+account {
						return true
					}
				}
			}
		}
	}
	return false
}

// Namespace Roles are not an RBAC upper bound. Audit additive grants before
// effects; only exact retained custom-read grants bypass the public-rule filter.
func (b *Backend) auditAuthorization(ctx context.Context, r *run) error {
	principals := principalsFor(r)
	authority, err := b.auditClusterBindings(ctx, r, principals)
	if err != nil {
		return err
	}
	if err := b.auditNamespaceBindings(ctx, r, principals); err != nil {
		return err
	}
	content, _ := json.Marshal(authority)
	digest := pv.Digest(content)
	if r.state.AuthorizationDigest != "" && r.state.AuthorizationDigest != digest {
		return contract.ErrConflict
	}
	if r.state.AuthorizationDigest == "" {
		r.state.AuthorizationDigest = digest
		return b.save(ctx, r)
	}
	return nil
}

func recordedClusterBinding(r *run, binding *rbacv1.ClusterRoleBinding) bool {
	for _, side := range pv.ActionSides(r.state.Request.Action) {
		entry := r.effect(resourceKey(side, "cluster-read-binding"))
		if entry != nil && (entry.Name == binding.Name || entry.UID != "" && entry.UID == binding.UID) {
			return true
		}
	}
	return false
}

func (b *Backend) auditClusterBindings(ctx context.Context, r *run, principals runPrincipals) (map[string]string, error) {
	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := b.targetReader.List(ctx, bindings, client.Limit(256)); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if bindings.Continue != "" {
		return nil, ErrIsolation
	}
	authority := make(map[string]string)
	for _, binding := range bindings.Items {
		recorded := recordedClusterBinding(r, &binding)
		if !recorded && !principals.matches(binding.Subjects, "") {
			continue
		}
		if binding.UID == "" || !binding.DeletionTimestamp.IsZero() ||
			binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != clusterRoleKind {
			return nil, ErrIsolation
		}
		role := &rbacv1.ClusterRole{}
		if err := b.targetReader.Get(ctx, client.ObjectKey{Name: binding.RoleRef.Name}, role); err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
		if role.UID == "" || role.AggregationRule != nil || !role.DeletionTimestamp.IsZero() || len(role.Rules) > 32 {
			return nil, ErrIsolation
		}
		owned, err := b.ownedClusterReadGrant(r, role, &binding)
		if err != nil {
			return nil, err
		}
		if owned {
			continue
		}
		if recorded {
			return nil, contract.ErrConflict
		}
		for _, rule := range role.Rules {
			if !publicAuthenticationRule(rule) {
				return nil, ErrIsolation
			}
		}
		content, _ := json.Marshal(struct {
			Binding rbacv1.ClusterRoleBinding
			Role    rbacv1.ClusterRole
		}{binding, *role})
		authority[binding.Name] = pv.Digest(content)
	}
	return authority, nil
}

func (b *Backend) auditNamespaceBindings(ctx context.Context, r *run, principals runPrincipals) error {
	bindings := &rbacv1.RoleBindingList{}
	if err := b.targetReader.List(ctx, bindings, client.Limit(256)); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if bindings.Continue != "" {
		return ErrIsolation
	}
	for _, binding := range bindings.Items {
		if !principals.matches(binding.Subjects, binding.Namespace) {
			continue
		}
		owned, roleOwned := false, false
		for _, entry := range r.state.Effects {
			owned = owned || entry.Kind == "RoleBinding" && entry.Namespace == binding.Namespace &&
				entry.Name == binding.Name && entry.UID == binding.UID && entry.UID != "" && !entry.Deleted
			roleOwned = roleOwned || entry.Kind == roleKind && entry.Namespace == binding.Namespace &&
				entry.Name == binding.RoleRef.Name && entry.UID != "" && !entry.Deleted
		}
		if !owned || !roleOwned || binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != roleKind ||
			binding.RoleRef.Name != binding.Name || !slices.Contains(principals.namespaces, binding.Namespace) {
			return ErrIsolation
		}
	}
	return nil
}

func publicAuthenticationRule(rule rbacv1.PolicyRule) bool {
	if len(rule.ResourceNames) != 0 || len(rule.Verbs) != 1 {
		return false
	}
	if len(rule.NonResourceURLs) != 0 {
		if rule.Verbs[0] != verbGet || len(rule.Resources) != 0 || len(rule.APIGroups) != 0 || len(rule.NonResourceURLs) > 32 {
			return false
		}
		for _, path := range rule.NonResourceURLs {
			if !slices.Contains([]string{"/api", "/api/*", "/apis", "/apis/*", "/healthz", "/livez", "/readyz",
				"/version", "/version/", "/openapi", "/openapi/*",
				"/.well-known/openid-configuration", "/.well-known/openid-configuration/",
				"/openid/v1/jwks", "/openid/v1/jwks/"}, path) {
				return false
			}
		}
		return true
	}
	if rule.Verbs[0] != verbCreate || len(rule.APIGroups) != 1 || len(rule.Resources) == 0 || len(rule.Resources) > 2 {
		return false
	}
	for _, resource := range rule.Resources {
		authorizationReview := rule.APIGroups[0] == "authorization.k8s.io" &&
			slices.Contains([]string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"}, resource)
		authenticationReview := rule.APIGroups[0] == "authentication.k8s.io" && resource == "selfsubjectreviews"
		if strings.Contains(resource, "/") || !authorizationReview && !authenticationReview {
			return false
		}
	}
	return true
}
