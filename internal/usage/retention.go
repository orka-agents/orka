package usage

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/orka-agents/orka/internal/store"
)

// Retention expires inactive reporting cohorts independently of Task TTLs.
type Retention struct {
	Store  store.UsageStore
	Period time.Duration
}

func (r *Retention) NeedLeaderElection() bool { return true }

func (r *Retention) Start(ctx context.Context) error {
	if r.Period < 0 {
		return fmt.Errorf("usage retention must not be negative")
	}
	if r.Period == 0 {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := r.Store.PruneUsage(ctx, time.Now().UTC().Add(-r.Period)); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "usage retention failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
