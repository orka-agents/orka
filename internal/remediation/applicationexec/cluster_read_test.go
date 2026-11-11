package applicationexec

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type clusterMetadataReader struct {
	client.Reader
	items        []metav1.PartialObjectMetadata
	continuation string
	listed       int
}

func (r *clusterMetadataReader) List(ctx context.Context, object client.ObjectList, options ...client.ListOption) error {
	list, metadataOnly := object.(*metav1.PartialObjectMetadataList)
	if !metadataOnly {
		return r.Reader.List(ctx, object, options...)
	}
	r.listed++
	list.Items = nil
	for _, item := range r.items {
		list.Items = append(list.Items, *item.DeepCopy())
	}
	list.Continue = r.continuation
	return nil
}

func installClusterReadFixture(t *testing.T, f *fixture) *clusterMetadataReader {
	t.Helper()
	installPolicyFixture(t, f)
	spec := map[string]any{
		"group": "globals.orka.ai", "scope": "Cluster", "conversion": map[string]any{"strategy": "None"},
		"names": map[string]any{"kind": "Catalog", "plural": "catalogs", "singular": "catalog", "listKind": "CatalogList"},
		"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true,
			"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object",
				"properties": map[string]any{"spec": map[string]any{"type": "object",
					"properties": map[string]any{"enabled": map[string]any{"type": "boolean"}}}}}}}},
	}
	content, err := json.Marshal(spec)
	require.NoError(t, err)
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "catalogs.globals.orka.ai", "uid": "cluster-profile-crd"},
		"spec":     spec, "status": map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}},
	}}
	require.NoError(t, f.target.Create(t.Context(), crd))
	f.backend.config.ResourcePolicy.Resources = append(f.backend.config.ResourcePolicy.Resources, pv.ApplicationResourceRule{
		APIVersion: "globals.orka.ai/v1", Kind: "Catalog", Resource: "catalogs", Namespaced: false,
		Verbs: []string{"get", "list", "watch"},
		CRD:   &pv.ApplicationCRDIdentity{Name: crd.GetName(), UID: string(crd.GetUID()), SchemaDigest: pv.Digest(content)},
	})
	f.backend.config.ResourcePolicy.SubjectPermissions = append(f.backend.config.ResourcePolicy.SubjectPermissions,
		pv.ApplicationPermission{APIGroup: "globals.orka.ai", Resources: []string{"catalogs"}, Verbs: []string{"get", "list", "watch"}})
	require.NoError(t, pv.ValidateApplicationPolicy(*f.backend.config.ResourcePolicy))
	f.request.ApplicationPolicy = f.backend.ApplicationPolicy()
	f.request.Checks.Application.Permissions = f.backend.ApplicationPolicy().SubjectPermissions
	reader := &clusterMetadataReader{Reader: f.target, items: []metav1.PartialObjectMetadata{
		{ObjectMeta: metav1.ObjectMeta{Name: "operator-catalog", UID: "catalog-uid", ResourceVersion: "41"}},
	}}
	f.backend.targetReader = reader
	return reader
}

func TestClusterReadsUseSeparateExactSubjectGrantAndRevokeBeforeNamespace(t *testing.T) {
	f := newFixture(t)
	reader := installClusterReadFixture(t, f)
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
	require.Greater(t, reader.listed, 0)
	require.Len(t, r.state.ClusterReads["globals.orka.ai/v1/catalogs"], 1)
	namespaceRole := &rbacv1.Role{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: namespaceName(r, pv.Original), Name: "orka-subject"}, namespaceRole))
	for _, rule := range namespaceRole.Rules {
		require.NotContains(t, rule.APIGroups, "globals.orka.ai", "a namespace Role must not masquerade as cluster authority")
	}
	roleObject, err := f.backend.boundObject(t.Context(), r, resourceKey(pv.Original, "cluster-read-role"))
	require.NoError(t, err)
	role := roleObject.(*rbacv1.ClusterRole)
	require.Nil(t, role.AggregationRule)
	require.Equal(t, []rbacv1.PolicyRule{{APIGroups: []string{"globals.orka.ai"}, Resources: []string{"catalogs"}, Verbs: []string{"get", "list", "watch"}}}, role.Rules)
	bindingObject, err := f.backend.boundObject(t.Context(), r, resourceKey(pv.Original, "cluster-read-binding"))
	require.NoError(t, err)
	binding := bindingObject.(*rbacv1.ClusterRoleBinding)
	require.Equal(t, []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespaceName(r, pv.Original), Name: "orka-subject"}}, binding.Subjects)
	require.Empty(t, binding.OwnerReferences)
	require.NoError(t, f.backend.auditAuthorization(t.Context(), r))
	namespaceUID := r.effect(resourceKey(pv.Original, "namespace")).UID
	subjectUID := r.effect(resourceKey(pv.Original, "sa-subject")).UID
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
	bindingIndex, roleIndex := slices.Index(f.deleted, binding.UID), slices.Index(f.deleted, role.UID)
	require.GreaterOrEqual(t, bindingIndex, 0)
	require.Greater(t, roleIndex, bindingIndex)
	require.Greater(t, slices.Index(f.deleted, subjectUID), roleIndex)
	require.Greater(t, slices.Index(f.deleted, namespaceUID), roleIndex)
	require.NotContains(t, f.deleted, types.UID("catalog-uid"), "operator-prepared cluster data is never deleted")
}

func TestClusterReadGrantRejectsReplacementAndDrift(t *testing.T) {
	for _, test := range []string{"write-verb", "different-subject", "unmatched-foreign-subject", "replacement-role"} {
		t.Run(test, func(t *testing.T) {
			f := newFixture(t)
			installClusterReadFixture(t, f)
			r := f.newRun()
			require.NoError(t, f.backend.ensureClaim(t.Context(), r))
			require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
			role := &rbacv1.ClusterRole{}
			binding := &rbacv1.ClusterRoleBinding{}
			key := client.ObjectKey{Name: clusterReadName(r, pv.Original)}
			require.NoError(t, f.target.Get(t.Context(), key, role))
			require.NoError(t, f.target.Get(t.Context(), key, binding))
			switch test {
			case "write-verb":
				role.Rules[0].Verbs = append(role.Rules[0].Verbs, "patch")
				require.NoError(t, f.target.Update(t.Context(), role))
			case "different-subject":
				binding.Subjects[0].Name = "orka-observer"
				require.NoError(t, f.target.Update(t.Context(), binding))
			case "unmatched-foreign-subject":
				binding.Subjects = []rbacv1.Subject{{Kind: "Group", Name: "unrelated-group"}}
				require.NoError(t, f.target.Update(t.Context(), binding))
			case "replacement-role":
				uid := role.UID
				require.NoError(t, f.target.Delete(t.Context(), role, client.Preconditions{UID: &uid}))
				role.UID, role.ResourceVersion = "replacement-role", ""
				role.CreationTimestamp = metav1.Time{}
				require.NoError(t, f.target.Create(t.Context(), role))
			}
			require.Error(t, f.backend.auditAuthorization(t.Context(), r))
		})
	}
}

func TestClusterReadSnapshotDetectsAmbientChangesAndBounds(t *testing.T) {
	for _, test := range []string{"replacement", "revision", "new-object", "deleting", "pagination", "too-many", "wrong-namespace"} {
		t.Run(test, func(t *testing.T) {
			f := newFixture(t)
			reader := installClusterReadFixture(t, f)
			r := f.newRun()
			require.NoError(t, f.backend.verifyClusterReadSet(t.Context(), r))
			original := clusterReadDigest(r)
			switch test {
			case "replacement":
				reader.items[0].UID = "replacement"
			case "revision":
				reader.items[0].ResourceVersion = "42"
			case "new-object":
				reader.items = append(reader.items, metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "second", UID: "second", ResourceVersion: "1"}})
			case "deleting":
				now := metav1.Now()
				reader.items[0].DeletionTimestamp = &now
			case "pagination":
				reader.continuation = "more"
			case "too-many":
				for len(reader.items) <= maxClusterReadObjects {
					reader.items = append(reader.items, reader.items[0])
				}
			case "wrong-namespace":
				reader.items[0].Namespace = "foreign"
			}
			require.Error(t, f.backend.verifyClusterReadSet(t.Context(), r))
			require.Equal(t, original, clusterReadDigest(r), "a changed read set cannot replace the admitted baseline")
		})
	}
}

func TestClusterRulesCannotBecomeActorOrMutationAuthority(t *testing.T) {
	f := newFixture(t)
	installClusterReadFixture(t, f)
	f.backend.qualified.Store(true)
	for _, verb := range []string{"create", "update", "patch", "delete", "*"} {
		t.Run(verb, func(t *testing.T) {
			content, err := json.Marshal(f.request)
			require.NoError(t, err)
			var request contract.ExecutionRequest
			require.NoError(t, json.Unmarshal(content, &request))
			request.Checks.Application.Permissions = append(request.Checks.Application.Permissions,
				pv.ApplicationPermission{APIGroup: "globals.orka.ai", Resources: []string{"catalogs"}, Verbs: []string{verb}})
			_, err = f.backend.RequestDigest(request)
			require.ErrorIs(t, err, contract.ErrInvalid)
		})
	}
	request := f.request
	content, err := json.Marshal(f.request.ApplicationPolicy)
	require.NoError(t, err)
	var policy pv.ApplicationPolicy
	require.NoError(t, json.Unmarshal(content, &policy))
	policy.Actors[0].Permissions = append(policy.Actors[0].Permissions,
		pv.ApplicationPermission{APIGroup: "globals.orka.ai", Resources: []string{"catalogs"}, Verbs: []string{"get"}})
	require.Error(t, pv.ValidateApplicationPolicy(policy))
	request.ApplicationPolicy = &policy
	_, err = f.backend.RequestDigest(request)
	require.ErrorIs(t, err, contract.ErrInvalid)
}
