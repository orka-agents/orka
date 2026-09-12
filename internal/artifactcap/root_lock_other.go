//go:build !darwin && !linux

package artifactcap

import "context"

func acquireArtifactRootLock(ctx context.Context, root string, exclusive bool) (func() error, error) {
	return acquireArtifactProcessGate(ctx, root, exclusive)
}
