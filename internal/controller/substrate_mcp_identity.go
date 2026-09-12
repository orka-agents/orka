package controller

import (
	"context"
	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/workspace"
)

func (r *ToolReconciler) substrateMCPFinalizerMigrationContext(
	ctx context.Context, tool *corev1alpha1.Tool,
) (*ExecutionWorkspaceRequest, *corev1alpha1.SubstrateActorPool, error) {
	if tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil {
		return nil, nil, nil
	}
	request := r.substrateMCPTemplateRequest(tool)
	if request.TemplateName == "" {
		return nil, nil, nil
	}
	poolName, poolNamespace := substrateActorPoolReference(tool.Spec.MCP.SubstrateActor.PoolRef, tool.Namespace)
	if poolName == "" {
		// The dedicated Actor name hashes this complete binding. An edited
		// template cannot qualify an Actor belonging to the previous spec.
		return request, nil, nil
	}
	pool := &corev1alpha1.SubstrateActorPool{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: poolNamespace, Name: poolName}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil // recorded status may still supply provenance
		}
		return nil, nil, err
	}
	atespace := strings.TrimSpace(pool.Spec.TemplateRef.Namespace)
	if atespace == "" {
		atespace = pool.Namespace
	}
	if atespace != request.TemplateNamespace || strings.TrimSpace(pool.Spec.TemplateRef.Name) != request.TemplateName {
		return nil, nil, nil
	}
	// Pool Atespace is immutable. Cleanup can use this binding even while the
	// pool is deleting or its native template is gone; admission is unnecessary.
	return request, pool, nil
}

// migrateSubstrateMCPIdentities qualifies legacy IDs using recorded template
// provenance or the validated current binding. It never deletes/recreates an
// Actor or transfers a lease. Status, leases and annotations converge through
// separate optimistic writes, so interruption cannot turn the same Actor into
// its own replacement cleanup target.
func (r *ToolReconciler) migrateSubstrateMCPIdentities(
	ctx context.Context, tool *corev1alpha1.Tool, request *ExecutionWorkspaceRequest, pool *corev1alpha1.SubstrateActorPool,
) (bool, error) {
	qualify := substrateMCPLegacyIdentityQualifier(tool, request, pool)
	if actor := tool.Status.Actor; actor != nil && actor.Provider == corev1alpha1.WorkspaceProviderSubstrate {
		if qualified := qualify(actor.ActorID); qualified != actor.ActorID {
			before := tool.DeepCopy()
			actor.ActorID = qualified
			actor.RouteHost = qualified + "." + strings.Trim(r.SubstrateConfig.WithDefaults().ActorDNSSuffix, ".")
			tool.Status.Available = false
			tool.Status.Endpoint = ""
			tool.Status.Error = "native Actor identity migrated; endpoint readiness must be revalidated"
			meta.SetStatusCondition(&tool.Status.Conditions, metav1.Condition{
				Type: "Available", Status: metav1.ConditionFalse, Reason: "ActorIdentityMigrated",
				Message: tool.Status.Error, ObservedGeneration: tool.Generation,
			})
			return true, r.Status().Patch(ctx, tool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}
	}
	leaseNamespaces := map[string]bool{tool.Namespace: true}
	for _, ref := range substrateMCPPoolActorLeaseRefs(tool) {
		leaseNamespaces[ref.poolNamespace] = true
	}
	if pool != nil {
		leaseNamespaces[pool.Namespace] = true
	}
	leaseChanged := false
	for namespace := range leaseNamespaces {
		leases := &coordinationv1.LeaseList{}
		if err := r.List(ctx, leases, client.InNamespace(namespace), client.MatchingLabels{
			substratePoolActorLeaseHolderUIDLabel: labels.SelectorValue(string(tool.UID)),
		}); err != nil {
			return false, err
		}
		for i := range leases.Items {
			lease := &leases.Items[i]
			purpose := lease.Labels[labels.LabelPurpose]
			if (purpose != substratePoolActorLeasePurpose && purpose != substrateMCPToolActorLeasePurpose) ||
				!substratePoolActorLeaseHeldByTool(lease, tool) {
				continue
			}
			actorID := substratePoolActorLeaseActorID(lease)
			if qualified := qualify(actorID); qualified != actorID {
				before := lease.DeepCopy()
				lease.Labels[substratePoolActorLeaseActorIDLabel] = labels.SelectorValue(qualified)
				lease.Annotations["orka.ai/substrate-actor-ref"] = qualified
				if err := r.Patch(ctx, lease, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					return false, err
				}
				leaseChanged = true
			}
		}
	}
	if leaseChanged {
		return true, nil
	}
	before := tool.DeepCopy()
	changed := false
	for _, key := range []string{substrateMCPToolActorIDAnno, substrateMCPToolBootedIDAnno, substrateMCPToolCleanupActorIDAnno} {
		if actorID := tool.Annotations[key]; actorID != "" {
			if qualified := qualify(actorID); qualified != actorID {
				tool.Annotations[key] = qualified
				changed = true
			}
		}
	}
	if changed {
		return true, r.Patch(ctx, tool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	return false, nil
}

func substrateMCPLegacyIdentityQualifier(
	tool *corev1alpha1.Tool, request *ExecutionWorkspaceRequest, pool *corev1alpha1.SubstrateActorPool,
) func(string) string {
	known := map[string]string{}
	remember := func(actorID, atespace string) {
		name, _, _ := strings.Cut(strings.TrimSpace(actorID), ".")
		if name != "" && strings.TrimSpace(atespace) != "" {
			known[name] = workspace.SubstrateActorKey(atespace, actorID)
		}
	}
	if actor := tool.Status.Actor; actor != nil && actor.Provider == corev1alpha1.WorkspaceProviderSubstrate && actor.TemplateRef != nil {
		atespace := strings.TrimSpace(actor.TemplateRef.Namespace)
		if atespace == "" {
			atespace = tool.Namespace
		}
		remember(actor.ActorID, atespace)
	}
	poolPrefix := ""
	if request != nil {
		if pool == nil {
			remember(deterministicSubstrateToolActorID(tool.Namespace, tool.Name, request.TemplateNamespace, request.TemplateName), request.TemplateNamespace)
		} else {
			// Pool Atespaces are immutable, and the caller has validated this
			// pool against the requested template before reaching migration.
			poolPrefix = deterministicSubstratePoolActorPrefix(pool.Namespace, pool.Name)
		}
	}
	qualify := func(actorID string) string {
		actorID = strings.TrimSpace(actorID)
		if actorID == "" || strings.Contains(actorID, ".") {
			return actorID
		}
		if qualified := known[actorID]; qualified != "" {
			return qualified
		}
		if poolPrefix != "" {
			if _, ok := substratePoolActorOrdinalFromID(actorID, poolPrefix); ok {
				return workspace.SubstrateActorKey(request.TemplateNamespace, actorID)
			}
		}
		// Do not guess an Atespace for historical cleanup without provenance.
		// The native client will continue to reject an unqualified operation.
		return actorID
	}
	return qualify
}
