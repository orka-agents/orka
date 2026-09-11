package controller

import (
	"context"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The backend's fault-injection tests exercise its ownership and lifecycle
// barriers independently of transport. Native API conformance is covered by
// the native template store and authenticated gRPC tests.
type substrateTemplateFixtureStore struct{ r *RuntimePoolReconciler }

func substrateFixtureTemplateValidator(reader client.Reader) func(context.Context, *ExecutionWorkspaceRequest) error {
	return func(ctx context.Context, request *ExecutionWorkspaceRequest) error {
		return validateSubstrateRoutableActorTemplateResource(ctx, reader, request)
	}
}

func (s *substrateTemplateFixtureStore) Get(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(substrateActorTemplateGVK)
	reader := s.r.APIReader
	if reader == nil {
		reader = s.r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, object); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	return object, nil
}

func (s *substrateTemplateFixtureStore) Create(ctx context.Context, pool *corev1alpha1.RuntimePool, desired *unstructured.Unstructured) error {
	object := desired.DeepCopy()
	if err := s.r.setRuntimePoolControllerReference(pool, object); err != nil {
		return err
	}
	if err := s.r.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (s *substrateTemplateFixtureStore) Update(ctx context.Context, previous, desired *unstructured.Unstructured) error {
	base := previous.DeepCopy()
	previous.Object["spec"] = desired.Object["spec"]
	previous.SetLabels(desired.GetLabels())
	previous.SetAnnotations(desired.GetAnnotations())
	return s.r.Patch(ctx, previous, client.MergeFrom(base))
}

func (s *substrateTemplateFixtureStore) Delete(ctx context.Context, object *unstructured.Unstructured) error {
	return s.r.Delete(ctx, object, deleteCurrentObjectPreconditions(object)...)
}
