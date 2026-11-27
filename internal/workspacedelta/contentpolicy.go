package workspacedelta

import (
	"context"
	"runtime"
	"sync"

	"golang.org/x/sync/semaphore"
)

const contentPolicyInFlightBytes int64 = 64 << 20

type contentPolicyJob struct {
	path    string
	content []byte
	weight  int64
}

type contentPolicyVerdict struct {
	flagged      bool
	fingerprints []string
}

type contentPolicyPool struct {
	options  normalizedOptions
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     chan contentPolicyJob
	inFlight *semaphore.Weighted
	workers  sync.WaitGroup

	mu       sync.Mutex
	verdicts map[string]contentPolicyVerdict
}

func newContentPolicyPool(ctx context.Context, options normalizedOptions) *contentPolicyPool {
	workers := runtime.GOMAXPROCS(0)
	ctx, cancel := context.WithCancel(ctx)
	pool := &contentPolicyPool{
		options:  options,
		ctx:      ctx,
		cancel:   cancel,
		jobs:     make(chan contentPolicyJob, workers),
		inFlight: semaphore.NewWeighted(contentPolicyInFlightBytes),
		verdicts: map[string]contentPolicyVerdict{},
	}
	pool.workers.Add(workers)
	for range workers {
		go pool.work()
	}
	return pool
}

func (p *contentPolicyPool) work() {
	defer p.workers.Done()
	for job := range p.jobs {
		if p.ctx.Err() == nil {
			verdict := evaluateContentPolicy(p.options, job.content)
			p.mu.Lock()
			p.verdicts[job.path] = verdict
			p.mu.Unlock()
		}
		p.release(job.weight)
	}
}

func (p *contentPolicyPool) reserve(size int64) (int64, error) {
	weight := contentPolicyWeight(size)
	if err := p.inFlight.Acquire(p.ctx, weight); err != nil {
		return 0, err
	}
	return weight, nil
}

func (p *contentPolicyPool) release(weight int64) {
	p.inFlight.Release(weight)
}

func (p *contentPolicyPool) submit(path string, content []byte, weight int64) error {
	select {
	case p.jobs <- contentPolicyJob{path: path, content: content, weight: weight}:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

func contentPolicyWeight(size int64) int64 {
	return min(max(size, 1), contentPolicyInFlightBytes)
}

func (p *contentPolicyPool) close() map[string]contentPolicyVerdict {
	close(p.jobs)
	p.workers.Wait()
	p.cancel()
	return p.verdicts
}

func evaluateContentPolicy(options normalizedOptions, content []byte) contentPolicyVerdict {
	var verdict contentPolicyVerdict
	if options.contentFlagger != nil {
		verdict.flagged = options.contentFlagger(content)
	}
	if options.contentFingerprinter != nil && (options.contentFlagger == nil || verdict.flagged) {
		verdict.fingerprints = options.contentFingerprinter(content)
	}
	return verdict
}
