package store

import "context"

// ControllerEpochMutationStore acquires a bounded epoch Lease interlock before
// invoking a short Kubernetes mutation callback. This is not a cross-resource
// transaction: an ambiguous request can still complete after callback return or
// Lease expiry. Callers must retain target UID/resourceVersion preconditions and
// immutable ownership checks; neither timeout nor expiry proves completion.
// The callback must not invoke another control-store mutation or wait for a
// runtime/network operation to settle.
type ControllerEpochMutationStore interface {
	WithControllerEpochMutation(context.Context, ControllerEpochFence, func(context.Context) error) error
}
