package applicationexec

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	maxClusterReadObjects = 64
	maxClusterReadTotal   = 128
)

type clusterReadObject struct {
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
}

func clusterReadResource(policy *pv.ApplicationPolicy, group, resource string) *pv.ApplicationResourceRule {
	if policy == nil {
		return nil
	}
	for i := range policy.Resources {
		rule := &policy.Resources[i]
		version, err := schema.ParseGroupVersion(rule.APIVersion)
		if err == nil && !rule.Namespaced && version.Group == group && rule.Resource == resource {
			return rule
		}
	}
	return nil
}

func subjectClusterRules(plan *pv.ApplicationPlan, policy *pv.ApplicationPolicy) ([]rbacv1.PolicyRule, error) {
	if plan == nil || policy == nil {
		return nil, nil
	}
	var rules []rbacv1.PolicyRule
	for _, permission := range plan.Permissions {
		for _, resource := range permission.Resources {
			admitted := clusterReadResource(policy, permission.APIGroup, resource)
			if admitted == nil {
				continue
			}
			if admitted.CRD == nil || permission.APIGroup == "" || admitted.Status || strings.Contains(resource, "/") {
				return nil, contract.ErrInvalid
			}
			for _, verb := range permission.Verbs {
				if !slices.Contains([]string{verbGet, verbList, verbWatch}, verb) || !slices.Contains(admitted.Verbs, verb) {
					return nil, contract.ErrInvalid
				}
			}
			// Only the subject's startup informers receive these reads. The
			// operator's exact custom API group/resource replaces any generic
			// cluster-reader/admin role or grant to actor/observer identities.
			rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{permission.APIGroup}, Resources: []string{resource},
				Verbs: slices.Clone(permission.Verbs), ResourceNames: slices.Clone(permission.ResourceNames)})
			if len(rules) > 32 {
				return nil, contract.ErrInvalid
			}
		}
	}
	return rules, nil
}

func clusterReadName(r *run, side string) string {
	content, _ := json.Marshal([]string{string(r.object.UID), side})
	return "orka-app-read-" + strings.TrimPrefix(pv.Digest(content), "sha256:")[:32]
}

func clusterReadObjects(r *run, side string, rules []rbacv1.PolicyRule) (*rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding) {
	name := clusterReadName(r, side)
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{runLabel: string(r.object.UID)}}, Rules: rules}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{runLabel: string(r.object.UID)}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: clusterRoleKind, Name: name},
		Subjects: []rbacv1.Subject{{Kind: serviceAccountKind, Namespace: namespaceName(r, side), Name: subjectAccountName}}}
	return role, binding
}

func (b *Backend) ensureSubjectClusterReads(ctx context.Context, r *run, side string) error {
	rules, err := subjectClusterRules(r.state.Request.Checks.Application, r.state.Request.ApplicationPolicy)
	if err != nil || len(rules) == 0 {
		return err
	}
	if side != pv.Original && side != pv.Patched || r.state.Qualification {
		return contract.ErrInvalid
	}
	if err := b.verifyClusterReadSet(ctx, r); err != nil {
		return err
	}
	role, binding := clusterReadObjects(r, side, rules)
	if _, err := b.ensureObject(ctx, r, resourceKey(side, "cluster-read-role"), role); err != nil {
		return err
	}
	_, err = b.ensureObject(ctx, r, resourceKey(side, "cluster-read-binding"), binding)
	return err
}

// ownedClusterReadGrant recognizes only exact persisted role/binding UIDs and
// the unchanged operator-admitted rules. Matching a name or label alone never
// exempts a cluster binding from the additive-authority audit.
func (b *Backend) ownedClusterReadGrant(r *run, role *rbacv1.ClusterRole, binding *rbacv1.ClusterRoleBinding) (bool, error) {
	for _, side := range pv.ActionSides(r.state.Request.Action) {
		roleEntry := r.effect(resourceKey(side, "cluster-read-role"))
		bindingEntry := r.effect(resourceKey(side, "cluster-read-binding"))
		if roleEntry == nil && bindingEntry == nil {
			continue
		}
		name := clusterReadName(r, side)
		if binding.Name != name && (bindingEntry == nil || bindingEntry.UID != binding.UID) {
			continue
		}
		if r.state.Qualification || roleEntry == nil || bindingEntry == nil ||
			roleEntry.UID == "" || bindingEntry.UID == "" || roleEntry.Deleted || bindingEntry.Deleted ||
			role.UID != roleEntry.UID || binding.UID != bindingEntry.UID || role.Name != name || binding.Name != name ||
			!role.DeletionTimestamp.IsZero() || !binding.DeletionTimestamp.IsZero() || role.AggregationRule != nil {
			return false, contract.ErrConflict
		}
		rules, err := subjectClusterRules(r.state.Request.Checks.Application, r.state.Request.ApplicationPolicy)
		if err != nil || len(rules) == 0 {
			return false, contract.ErrConflict
		}
		expectedRole, expectedBinding := clusterReadObjects(r, side, rules)
		if !staticObjectMatches(expectedRole, role) || !staticObjectMatches(expectedBinding, binding) {
			return false, ErrIsolation
		}
		return true, nil
	}
	return false, nil
}

// The cluster objects themselves are never created or mutated here. A bounded
// metadata-only snapshot prevents ambient shared-object changes from explaining
// an apparent original/candidate improvement. List resourceVersion is omitted:
// it can advance because unrelated namespace work changed the global revision.
func (b *Backend) verifyClusterReadSet(ctx context.Context, r *run) error {
	plan, policy := r.state.Request.Checks.Application, r.state.Request.ApplicationPolicy
	if r.state.Qualification || plan == nil || policy == nil {
		return nil
	}
	reads := make(map[string][]clusterReadObject)
	total := 0
	for _, definition := range policy.Resources {
		if definition.Namespaced {
			continue
		}
		version, err := schema.ParseGroupVersion(definition.APIVersion)
		if err != nil || definition.CRD == nil || definition.Status {
			return contract.ErrInvalid
		}
		used := false
		for _, permission := range plan.Permissions {
			used = used || permission.APIGroup == version.Group && slices.Contains(permission.Resources, definition.Resource)
		}
		if !used {
			continue
		}
		objects := &metav1.PartialObjectMetadataList{}
		objects.SetGroupVersionKind(version.WithKind(definition.Kind + "List"))
		if err := b.targetReader.List(ctx, objects, client.Limit(maxClusterReadObjects+1)); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
		if objects.Continue != "" || len(objects.Items) > maxClusterReadObjects {
			return ErrIsolation
		}
		key := definition.APIVersion + "/" + definition.Resource
		entries, err := clusterReadEntries(objects.Items)
		if err != nil {
			return err
		}
		total += len(entries)
		if total > maxClusterReadTotal {
			return ErrIsolation
		}
		reads[key] = entries
	}
	if len(reads) == 0 {
		if len(r.state.ClusterReads) != 0 {
			return contract.ErrConflict
		}
		return nil
	}
	if r.state.ClusterReads != nil {
		if !reflect.DeepEqual(r.state.ClusterReads, reads) {
			return contract.ErrConflict
		}
		return nil
	}
	r.state.ClusterReads = reads
	return b.save(ctx, r)
}

func clusterReadEntries(objects []metav1.PartialObjectMetadata) ([]clusterReadObject, error) {
	entries := make([]clusterReadObject, 0, len(objects))
	names, uids := make(map[string]bool), make(map[types.UID]bool)
	for _, object := range objects {
		if !validUID(string(object.UID)) || object.Name == "" || len(object.Name) > 253 ||
			names[object.Name] || uids[object.UID] || object.Namespace != "" || !object.DeletionTimestamp.IsZero() ||
			object.ResourceVersion == "" || len(object.ResourceVersion) > 128 {
			return nil, contract.ErrConflict
		}
		names[object.Name], uids[object.UID] = true, true
		entries = append(entries, clusterReadObject{Name: object.Name, UID: object.UID, ResourceVersion: object.ResourceVersion})
	}
	slices.SortFunc(entries, func(a, b clusterReadObject) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

func clusterReadDigest(r *run) string {
	content, _ := json.Marshal(r.state.ClusterReads)
	return pv.Digest(content)
}
