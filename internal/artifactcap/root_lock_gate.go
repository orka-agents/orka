package artifactcap

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

const artifactRootReaderWeight int64 = 1 << 20

var artifactRootProcessGates sync.Map

type artifactRootProcessGate struct {
	semaphore *semaphore.Weighted
}

func acquireArtifactProcessGate(ctx context.Context, root string, exclusive bool) (func() error, error) {
	value, _ := artifactRootProcessGates.LoadOrStore(root, &artifactRootProcessGate{
		semaphore: semaphore.NewWeighted(artifactRootReaderWeight),
	})
	gate := value.(*artifactRootProcessGate)
	weight := int64(1)
	if exclusive {
		weight = artifactRootReaderWeight
	}
	if err := gate.semaphore.Acquire(ctx, weight); err != nil {
		return nil, err
	}
	return func() error {
		gate.semaphore.Release(weight)
		return nil
	}, nil
}
