package v1

import (
	"context"
)

// ControllerEpochMirror receives the exact epoch acquired from the
// Kubernetes-authoritative ControllerEpochStore. Mirrors are subordinate
// fencing records for SQLite payload transactions; they never select or
// advance controller ownership themselves.
type ControllerEpochMirror interface {
	SyncControllerEpochMirror(ctx context.Context, epoch ControllerEpoch) error
}
