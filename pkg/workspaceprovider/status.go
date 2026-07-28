package workspaceprovider

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PatchStatus applies only status changes from before to after. Callers must pass
// two distinct snapshots of the same object; after may be mutated by the API server.
func PatchStatus(ctx context.Context, c client.Client, before, after client.Object) error {
	return c.Status().Patch(ctx, after, client.MergeFrom(before))
}
