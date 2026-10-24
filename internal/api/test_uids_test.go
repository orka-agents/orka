package api

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// withTestUIDs gives each object without a UID the UID "<name>-uid", as the
// API server gives every object a UID. Results and artifacts are bound to the
// Task UID, so a handler test that stores a Task's result needs one.
func withTestUIDs(objs ...runtime.Object) []runtime.Object {
	for _, obj := range objs {
		accessor, err := meta.Accessor(obj)
		if err == nil && accessor.GetUID() == "" && accessor.GetName() != "" {
			accessor.SetUID(types.UID(accessor.GetName() + "-uid"))
		}
	}
	return objs
}
