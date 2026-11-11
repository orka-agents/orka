package applicationexec

import (
	"context"
	"encoding/json"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (b *Backend) profileKind(gvk schema.GroupVersionKind) bool {
	if b.config.ResourcePolicy == nil {
		return false
	}
	for _, rule := range b.config.ResourcePolicy.Resources {
		if rule.APIVersion == gvk.GroupVersion().String() && rule.Kind == gvk.Kind && rule.Namespaced {
			return true
		}
	}
	return false
}

func (b *Backend) verifyProfile(ctx context.Context) error {
	if b.config.ResourcePolicy == nil {
		return nil
	}
	for _, rule := range b.config.ResourcePolicy.Resources {
		if rule.CRD == nil {
			continue
		}
		if err := b.verifyCRD(ctx, rule); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) verifyCRD(ctx context.Context, rule pv.ApplicationResourceRule) error {
	version, err := schema.ParseGroupVersion(rule.APIVersion)
	if err != nil || version.Group == "" ||
		rule.CRD.Name != rule.Resource+"."+version.Group {
		return contract.ErrInvalid
	}
	expectedScope := "Namespaced"
	if !rule.Namespaced {
		expectedScope = "Cluster"
		if rule.Status {
			return contract.ErrInvalid
		}
		for _, verb := range rule.Verbs {
			if verb != verbGet && verb != verbList && verb != verbWatch {
				return contract.ErrInvalid
			}
		}
	}
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	if err := b.targetReader.Get(ctx, client.ObjectKey{Name: rule.CRD.Name}, crd); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	spec, found, err := unstructured.NestedMap(crd.Object, "spec")
	if err != nil || !found || string(crd.GetUID()) != rule.CRD.UID || !crd.GetDeletionTimestamp().IsZero() {
		return contract.ErrConflict
	}
	content, err := json.Marshal(spec)
	if err != nil || pv.Digest(content) != rule.CRD.SchemaDigest {
		return contract.ErrConflict
	}
	group, _, _ := unstructured.NestedString(spec, "group")
	scope, _, _ := unstructured.NestedString(spec, "scope")
	kind, _, _ := unstructured.NestedString(spec, "names", "kind")
	plural, _, _ := unstructured.NestedString(spec, "names", "plural")
	conversion, _, _ := unstructured.NestedString(spec, "conversion", "strategy")
	if group != version.Group || scope != expectedScope || kind != rule.Kind || plural != rule.Resource ||
		(conversion != "" && conversion != "None") {
		return ErrIsolation
	}
	if err := validateServedCRDVersion(spec, version.Version, rule.Status); err != nil {
		return err
	}
	return establishedCRD(crd)
}

func validateServedCRDVersion(spec map[string]any, version string, needsStatus bool) error {
	versions, _, err := unstructured.NestedSlice(spec, "versions")
	if err != nil {
		return contract.ErrConflict
	}
	served := false
	for _, raw := range versions {
		entry, ok := raw.(map[string]any)
		if !ok || entry["name"] != version || entry["served"] != true {
			continue
		}
		_, structural, err := unstructured.NestedMap(entry, "schema", "openAPIV3Schema")
		_, status, statusErr := unstructured.NestedMap(entry, "subresources", "status")
		served = err == nil && structural && (!needsStatus || statusErr == nil && status)
	}
	if !served {
		return ErrIsolation
	}
	return nil
}

func establishedCRD(crd *unstructured.Unstructured) error {
	conditions, _, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
	if err != nil {
		return contract.ErrConflict
	}
	for _, raw := range conditions {
		if condition, ok := raw.(map[string]any); ok && condition["type"] == "Established" && condition["status"] == "True" {
			return nil
		}
	}
	return contract.ErrUnavailable
}
