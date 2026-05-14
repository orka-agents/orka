/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: assignFakeUIDOnCreate,
		}).
		Build()
}

func assignFakeUIDOnCreate(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	if err := c.Create(ctx, obj, opts...); err != nil {
		return err
	}
	if obj.GetUID() != "" {
		return nil
	}
	obj.SetUID(apitypes.UID(fmt.Sprintf("test-uid-%s-%s", obj.GetNamespace(), obj.GetName())))
	return c.Update(ctx, obj)
}

func newFakeClientWithAgents(agents []*corev1alpha1.Agent) client.Client {
	objs := make([]client.Object, len(agents))
	for i, a := range agents {
		objs[i] = a
	}
	return newFakeClient(objs...)
}

func newFakeClientWithTools(tools []*corev1alpha1.Tool) client.Client {
	objs := make([]client.Object, len(tools))
	for i, t := range tools {
		objs[i] = t
	}
	return newFakeClient(objs...)
}
