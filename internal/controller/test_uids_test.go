package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// assignTestUIDsOnCreate makes a fake client give each object created
// without a UID the UID "<name>-uid", as the API server gives every object a
// UID. Results and artifacts are bound to the Task UID, so code that creates
// a Task and later reads its result needs one.
func assignTestUIDsOnCreate() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" && obj.GetName() != "" {
				obj.SetUID(types.UID(obj.GetName() + "-uid"))
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}
