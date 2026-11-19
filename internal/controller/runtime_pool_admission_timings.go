/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// Workspace RuntimePool admission timing: from the first reconcile that sees
// the provider's startup evidence for the current workspace generation,
// through the sealed credential bootstrap, to the pool serving and accepting
// on its authenticated probe. Times are taken in this process; the provider's
// DataPlaneReady condition time has whole-second precision and is not used.

const (
	acpRuntimeAdmissionTimingsLogMessage = "ACP workspace runtime admission timings"
	acpRuntimeAdmissionMaxRecords        = 1024
	acpRuntimeAdmissionRetention         = 30 * time.Minute
)

type acpRuntimeAdmissionKey struct {
	pool     types.UID
	sequence int64
}

type acpRuntimeAdmissionRecord struct {
	observed time.Time
	seeded   time.Time
}

type acpRuntimeAdmissionTimings struct {
	now     func() time.Time
	mu      sync.Mutex
	records map[acpRuntimeAdmissionKey]*acpRuntimeAdmissionRecord
}

var acpRuntimeAdmission = &acpRuntimeAdmissionTimings{
	now: time.Now, records: map[acpRuntimeAdmissionKey]*acpRuntimeAdmissionRecord{},
}

// startupObserved records the first reconcile that sees current startup
// evidence for the pool's workload sequence while the pool is not yet
// admitting, as on a cold start, a resume, or re-admission after a warm
// re-attach closed it. A pool already serving and accepting starts nothing.
func (t *acpRuntimeAdmissionTimings) startupObserved(pool *corev1alpha1.RuntimePool, sequence int64) {
	if pool == nil || pool.UID == "" || (pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing &&
		pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting) {
		return
	}
	now := t.now()
	key := acpRuntimeAdmissionKey{pool: pool.UID, sequence: sequence}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.records[key] != nil {
		return
	}
	if len(t.records) >= acpRuntimeAdmissionMaxRecords {
		for existing, record := range t.records {
			if now.Sub(record.observed) >= acpRuntimeAdmissionRetention {
				delete(t.records, existing)
			}
		}
		if len(t.records) >= acpRuntimeAdmissionMaxRecords {
			return
		}
	}
	t.records[key] = &acpRuntimeAdmissionRecord{observed: now}
}

// credentialsSeeded records the first successful credential bootstrap.
func (t *acpRuntimeAdmissionTimings) credentialsSeeded(pool *corev1alpha1.RuntimePool, sequence int64) {
	if pool == nil {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if record := t.records[acpRuntimeAdmissionKey{pool: pool.UID, sequence: sequence}]; record != nil && record.seeded.IsZero() {
		record.seeded = now
	}
}

// served logs the sequence's admission once the pool serves and accepts.
func (t *acpRuntimeAdmissionTimings) served(ctx context.Context, pool *corev1alpha1.RuntimePool, sequence int64) {
	if pool == nil || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing ||
		pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		return
	}
	now := t.now()
	key := acpRuntimeAdmissionKey{pool: pool.UID, sequence: sequence}
	t.mu.Lock()
	record := t.records[key]
	delete(t.records, key)
	t.mu.Unlock()
	if record == nil {
		return
	}
	values := []any{"namespace", pool.Namespace, "runtimePool", pool.Name, "sequence", sequence,
		"totalMs", acpPreDispatchMillis(now.Sub(record.observed))}
	if !record.seeded.IsZero() {
		values = append(values,
			"attestAndBootstrapMs", acpPreDispatchMillis(record.seeded.Sub(record.observed)),
			"probeToServingMs", acpPreDispatchMillis(now.Sub(record.seeded)))
	}
	logf.FromContext(ctx).Info(acpRuntimeAdmissionTimingsLogMessage, values...)
}
