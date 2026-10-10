package controller

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Recover in one startup after process death leaves a durable mutation interlock.
// The failed release preserves the production TTL without sleeping in real time
// or contacting a cluster. Startup must not steal or renew the abandoned lock.
//
//nolint:gocyclo // Keep crash, lease-expiry, and fencing assertions in one ordered scenario.
func TestControllerEpochManagerCrashRecoveryAfterMutationExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		authority, kubeClient := newKubernetesControllerEpochAuthority(t)
		previous := advanceKubernetesControllerEpoch(t, t.Context(), authority, 1)
		previousFence := store.ControllerEpochFence{
			Name: previous.Name, Epoch: previous.Epoch, HolderID: previous.HolderID,
		}
		const mutationTokenAnnotation = "core.orka.ai/epoch-mutation-token"
		const mutationExpiryAnnotation = "core.orka.ai/epoch-mutation-expires-at"
		withWatch, ok := kubeClient.(client.WithWatch)
		if !ok {
			t.Fatal("fake client does not implement client.WithWatch")
		}
		releaseLost := false
		crashedClient := interceptor.NewClient(withWatch, interceptor.Funcs{
			Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
				if lease, ok := object.(*coordinationv1.Lease); ok && lease.Annotations[mutationTokenAnnotation] == "" {
					releaseLost = true
					return errors.New("simulated process death before mutation release")
				}
				return delegate.Update(ctx, object, options...)
			},
		})
		crashedStore, err := kubestore.New(crashedClient, controllerEpochManagerTestNamespace, kubestore.WithAPIReader(kubeClient))
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if err := crashedStore.WithControllerEpochMutation(t.Context(), previousFence, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if !releaseLost {
			t.Fatal("test did not abandon the mutation lock")
		}
		_, abandoned := kubernetesControllerEpochObjects(t, t.Context(), kubeClient)
		expiresAt, err := time.Parse(time.RFC3339Nano, abandoned.Annotations[mutationExpiryAnnotation])
		if err != nil {
			t.Fatal("abandoned mutation expiry is missing or malformed")
		}
		if expiresAt.Sub(started) != 2*time.Minute {
			t.Fatalf("mutation TTL = %s, want 2m", expiresAt.Sub(started))
		}

		recorded := &crashRecoveryEpochStore{ControllerEpochStore: authority}
		mirror := &recordingControllerEpochMirror{}
		manager := NewControllerEpochManager(recorded, "controller-restarted-before-expiry").WithMirror(mirror)
		cleanup := &crashRecoveryCleanupProbe{}
		recovery := NewSessionCleanupRecoveryManager(cleanup, manager)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done, cleanupDone := make(chan error, 1), make(chan error, 1)
		go func() { done <- manager.Start(ctx) }()
		go func() { cleanupDone <- recovery.Start(ctx) }()
		synctest.Wait()

		time.Sleep(time.Until(expiresAt) - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("startup exited instead of waiting for mutation expiry: %v", err)
		default:
		}
		if cleanup.calls.Load() != 0 {
			t.Fatal("startup cleanup used a predecessor fence before epoch readiness")
		}
		if _, ready := manager.Current(); ready || len(mirror.synced) != 0 {
			t.Fatal("startup published or mirrored an epoch before mutation expiry")
		}
		_, beforeExpiry := kubernetesControllerEpochObjects(t, t.Context(), kubeClient)
		if beforeExpiry.ResourceVersion != abandoned.ResourceVersion || !reflect.DeepEqual(beforeExpiry.Annotations, abandoned.Annotations) {
			t.Fatal("waiting startup changed the abandoned mutation lock or renewed its expiry")
		}
		current, err := authority.GetControllerEpoch(t.Context(), previous.Name)
		if err != nil || !controllerEpochsEqual(current, previous) {
			t.Fatalf("waiting startup changed the authoritative epoch: %v", err)
		}

		// The next capped retry must recover without restarting the manager.
		time.Sleep(time.Second + time.Nanosecond)
		synctest.Wait()
		acquired, ready := manager.Current()
		if !ready || acquired.Epoch != 2 || acquired.Version != 2 || acquired.HolderID != manager.HolderID {
			t.Fatal("startup did not acquire exactly the next epoch after mutation expiry")
		}
		if len(recorded.changes) <= 16 {
			t.Fatalf("CAS attempts = %d, want recovery beyond the former 16-attempt limit", len(recorded.changes))
		}
		for _, change := range recorded.changes {
			if !reflect.DeepEqual(change, recorded.changes[0]) {
				t.Fatal("retry changed the submitted CAS intent")
			}
		}
		if len(mirror.synced) != 1 || !controllerEpochsEqual(&mirror.synced[0], &acquired) {
			t.Fatal("recovered epoch was not mirrored before readiness")
		}
		if calls := cleanup.calls.Load(); calls != 1 {
			t.Fatalf("cleanup calls after epoch readiness = %d, want 1", calls)
		}
		fence, err := manager.CurrentFence(ctx)
		if err != nil || fence.Epoch != acquired.Epoch || fence.HolderID != acquired.HolderID {
			t.Fatalf("recovered manager did not publish the acquired fence: %v", err)
		}
		_, recovered := kubernetesControllerEpochObjects(t, t.Context(), kubeClient)
		if recovered.Annotations[mutationTokenAnnotation] != "" || recovered.Annotations[mutationExpiryAnnotation] != "" {
			t.Fatal("successful takeover retained the expired mutation lock")
		}
		called := false
		err = authority.WithControllerEpochMutation(t.Context(), previousFence, func(context.Context) error { called = true; return nil })
		if !errors.Is(err, store.ErrConflict) || called {
			t.Fatal("recovered epoch allowed a predecessor mutation")
		}

		// The acquisition deadline must not limit the acquired manager's lifetime.
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("acquired manager stopped without parent cancellation: %v", err)
		default:
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-cleanupDone; err != nil {
			t.Fatal(err)
		}
	})
}

func TestControllerEpochManagerAcquisitionIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name        string
		blockAt     string
		parentLimit bool
		cancelEarly bool
	}{
		{name: "persistent conflict"},
		{name: "blocked initial read", blockAt: "initial read"},
		{name: "blocked CAS", blockAt: "CAS"},
		{name: "blocked conflict reconciliation", blockAt: "reconciliation"},
		{name: "parent deadline", parentLimit: true},
		{name: "parent cancellation", cancelEarly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				authority := &crashRecoveryUnavailableEpochStore{
					ControllerEpochStore: newRetainedIntentEpochStore(epochConflictLeavesPredecessor),
					blockAt:              tc.blockAt, casError: store.ErrConflict,
				}
				recorded := &crashRecoveryEpochStore{ControllerEpochStore: authority}
				manager := NewControllerEpochManager(recorded, "controller-waiting-for-epoch")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				wantElapsed, wantError := 3*time.Minute, context.DeadlineExceeded
				if tc.parentLimit {
					var cancelDeadline context.CancelFunc
					ctx, cancelDeadline = context.WithTimeout(ctx, 75*time.Millisecond)
					defer cancelDeadline()
					wantElapsed = 75 * time.Millisecond
				}
				if tc.cancelEarly {
					timer := time.AfterFunc(75*time.Millisecond, cancel)
					defer timer.Stop()
					wantElapsed, wantError = 75*time.Millisecond, context.Canceled
				}
				started := time.Now()
				if err := manager.Start(ctx); !errors.Is(err, wantError) {
					t.Fatalf("startup error = %v, want %v", err, wantError)
				}
				if elapsed := time.Since(started); elapsed != wantElapsed {
					t.Fatalf("acquisition duration = %s, want %s", elapsed, wantElapsed)
				}
				if _, ready := manager.Current(); ready {
					t.Fatal("unsuccessful acquisition published an epoch")
				}
				for i := 1; i < len(recorded.attemptedAt); i++ {
					delay := recorded.attemptedAt[i].Sub(recorded.attemptedAt[i-1])
					if delay <= 0 || delay > time.Second {
						t.Fatalf("retry delay = %s, want positive delay capped at 1s", delay)
					}
				}
				if tc.name == "persistent conflict" {
					n := len(recorded.attemptedAt)
					if n < 2 || recorded.attemptedAt[n-1].Sub(recorded.attemptedAt[n-2]) != time.Second {
						t.Fatal("persistent conflicts did not reach the 1s backoff cap")
					}
				}
			})
		})
	}
}

func TestControllerEpochManagerAcquisitionRejectsNonConflictsImmediately(t *testing.T) {
	for _, failure := range []error{store.ErrValidation, errors.New("definitive admission failure")} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				authority := &crashRecoveryUnavailableEpochStore{
					ControllerEpochStore: newRetainedIntentEpochStore(epochConflictLeavesPredecessor),
					casError:             failure,
				}
				recorded := &crashRecoveryEpochStore{ControllerEpochStore: authority}
				manager := NewControllerEpochManager(recorded, "controller-with-definitive-failure")
				started := time.Now()
				if err := manager.Start(t.Context()); !errors.Is(err, failure) {
					t.Fatalf("startup error = %v, want original non-conflict", err)
				}
				if time.Since(started) != 0 || len(recorded.changes) != 1 || authority.reads != 1 {
					t.Fatal("startup retried or reconciled a non-conflict failure")
				}
				if _, ready := manager.Current(); ready {
					t.Fatal("definitive acquisition failure published an epoch")
				}
			})
		})
	}
}

type crashRecoveryEpochStore struct {
	store.ControllerEpochStore
	changes     []store.ControllerEpochCAS
	attemptedAt []time.Time
}

func (s *crashRecoveryEpochStore) CompareAndSwapControllerEpoch(ctx context.Context, change store.ControllerEpochCAS) (*store.ControllerEpoch, error) {
	s.changes = append(s.changes, change)
	s.attemptedAt = append(s.attemptedAt, time.Now())
	return s.ControllerEpochStore.CompareAndSwapControllerEpoch(ctx, change)
}

type crashRecoveryUnavailableEpochStore struct {
	store.ControllerEpochStore
	blockAt  string
	casError error
	reads    int
}

func (s *crashRecoveryUnavailableEpochStore) GetControllerEpoch(ctx context.Context, name string) (*store.ControllerEpoch, error) {
	s.reads++
	if s.blockAt == "initial read" || (s.blockAt == "reconciliation" && s.reads > 1) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.ControllerEpochStore.GetControllerEpoch(ctx, name)
}

func (s *crashRecoveryUnavailableEpochStore) CompareAndSwapControllerEpoch(ctx context.Context, _ store.ControllerEpochCAS) (*store.ControllerEpoch, error) {
	if s.blockAt == "CAS" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, s.casError
}

type crashRecoveryCleanupProbe struct {
	calls atomic.Int64
}

func (p *crashRecoveryCleanupProbe) ResumeSessionCleanups(context.Context, store.ControllerEpochFence) error {
	p.calls.Add(1)
	return nil
}
