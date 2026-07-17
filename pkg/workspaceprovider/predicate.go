package workspaceprovider

import (
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ControllerNamePredicate filters provider events to the immutable controllerName
// owned by one adapter. Out-of-tree controllers should combine this with generation
// predicates appropriate for their reconciliation model.
func ControllerNamePredicate(controllerName string) predicate.Predicate {
	matches := func(obj any) bool {
		provider, ok := obj.(*workspacev1alpha1.ExecutionWorkspaceProvider)
		return ok && provider.Spec.ControllerName == controllerName
	}

	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return matches(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return matches(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool { return matches(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool {
			return matches(e.Object)
		},
	}
}

// ProviderOwnedBy reports whether a provider belongs to controllerName.
func ProviderOwnedBy(provider *workspacev1alpha1.ExecutionWorkspaceProvider, controllerName string) bool {
	return provider != nil && provider.Spec.ControllerName == controllerName
}
