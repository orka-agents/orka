package kube

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// These tests run the controller-epoch interlock (ADR 0058) between two real
// Stores, a holder and a successor, each with its own client, against one
// kube-apiserver and etcd. Fake-client tests cannot show what happens when
// writes are delayed or their outcome is ambiguous, or when one side runs
// the pre-0058 code.
//
// Safety is judged from the server's own record, not from what the clients
// believe: every fenced write creates a ConfigMap labelled with the epoch it
// ran under, and a watch on the epoch Lease records the resourceVersion at
// which the successor's epoch committed. In etcd every write gets the next
// global revision, which the API server reports as resourceVersion, so a
// fenced write under the old epoch committed after the epoch change exactly
// when its creation resourceVersion is larger than the change's. The tests
// rely on that envtest/etcd property only to order events after the fact.

const (
	envtestEpochName     = store.DefaultControllerEpochName
	envtestHolderID      = "holder-a"
	envtestSuccessorID   = "successor-b"
	envtestEpochLabel    = "test.orka.ai/fenced-epoch"
	envtestWriterLabel   = "test.orka.ai/fenced-writer"
	envtestManagerBudget = 3 * time.Minute // defaultEpochAcquisitionTimeout
)

// epochAPIServer is one envtest API server shared by the subtests; each
// subtest gets its own control namespace and epoch.
type epochAPIServer struct {
	config *rest.Config
	scheme *runtime.Scheme
	admin  client.WithWatch
	seq    atomic.Int32
}

func startEpochAPIServer(t *testing.T) *epochAPIServer {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run the controller-epoch interlock against an API server")
	}
	environment := &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases", "core.orka.ai_controllerepochs.yaml")},
		},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	require.NoError(t, err, "start envtest")
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	// controller-runtime prints a stack trace for every client built before
	// a logger is set.
	ctrllog.SetLogger(logr.Discard())
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	server := &epochAPIServer{config: config, scheme: scheme}
	server.admin = server.newClient(t, true)
	return server
}

// newClient is a client with its own connection. Throttled, it has the
// client-side rate limit ctrl.GetConfigOrDie gives the controller (20 QPS,
// burst 30, per API group); unthrottled, none, so a busy holder's requests
// follow each other as fast as the API server answers.
func (e *epochAPIServer) newClient(t *testing.T, unthrottled bool) client.WithWatch {
	t.Helper()
	config := rest.CopyConfig(e.config)
	config.QPS, config.Burst = 20, 30
	if unthrottled {
		config.QPS = -1
	}
	c, err := client.NewWithWatch(config, client.Options{Scheme: e.scheme})
	require.NoError(t, err)
	return c
}

// epochEnv is one subtest's control namespace with epoch 1 held by the
// holder.
type epochEnv struct {
	server    *epochAPIServer
	namespace string
	fence     store.ControllerEpochFence
}

func (e *epochAPIServer) newEnv(t *testing.T) *epochEnv {
	t.Helper()
	namespace := fmt.Sprintf("epoch-%d", e.seq.Add(1))
	require.NoError(t, e.admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	env := &epochEnv{server: e, namespace: namespace}
	creator := env.peer(t, peerOptions{version: peerCurrent, linger: 0})
	epoch, err := creator.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		Name: envtestEpochName, NewEpoch: 1, HolderID: envtestHolderID,
		RequestDigest: testDigest(namespace + "/epoch-1"), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	env.fence = store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
	return env
}

type peerVersion int

const (
	// peerCurrent runs this branch's interlock (ADR 0058).
	peerCurrent peerVersion = iota
	// peerLegacy runs the pre-0058 interlock: see legacyEpochPeer.
	peerLegacy
)

type peerOptions struct {
	version         peerVersion
	linger, maxHold time.Duration
	// faults wraps the peer's client innermost, next to the API server.
	faults *interceptor.Funcs
	// unthrottled drops the controller's client-side rate limit.
	unthrottled bool
}

// legacyEpochPeer makes a store behave as the pre-0058 code on the wire. The
// pre-0058 interlock differs from this branch only in the takeover request
// (published and cleared by a successor, read by a holder before a token
// write), the hold, and the fenced-write bound. With the hold off
// (WithControllerEpochInterlockHold(0, 0)) and every read of a takeover
// request answering NotFound and every write of one dropped before it is
// sent, a legacy holder never sees a request and a legacy successor never
// publishes one; nothing about takeover requests reaches the server. What
// remains is that this code still calls the request paths: a successor's
// epoch change makes one extra GET of the epoch Lease before its first read
// (PublishControllerEpochTakeoverIntent), outside the window between the read
// its compare-and-swap is based on and the compare-and-swap itself, and
// fenced writes end 60s after their token was written, which no write here
// approaches.
func legacyEpochPeer(c client.WithWatch) client.WithWatch {
	request := func(name string) bool { return strings.HasPrefix(name, controllerEpochTakeoverLeasePrefix) }
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, lease := object.(*coordinationv1.Lease); lease && request(key.Name) {
				return apierrors.NewNotFound(coordinationv1.Resource("leases"), key.Name)
			}
			return delegate.Get(ctx, key, object, options...)
		},
		Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, lease := object.(*coordinationv1.Lease); lease && request(object.GetName()) {
				return nil
			}
			return delegate.Create(ctx, object, options...)
		},
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if _, lease := object.(*coordinationv1.Lease); lease && request(object.GetName()) {
				return nil
			}
			return delegate.Update(ctx, object, options...)
		},
		Delete: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			if _, lease := object.(*coordinationv1.Lease); lease && request(object.GetName()) {
				return nil
			}
			return delegate.Delete(ctx, object, options...)
		},
	})
}

func (env *epochEnv) peer(t *testing.T, options peerOptions) *Store {
	t.Helper()
	c := env.server.newClient(t, options.unthrottled)
	if options.faults != nil {
		c = interceptor.NewClient(c, *options.faults)
	}
	if options.version == peerLegacy {
		c = legacyEpochPeer(c)
		options.linger, options.maxHold = 0, 0
	}
	s, err := New(c, env.namespace, WithAPIReader(c), WithControllerEpochInterlockHold(options.linger, options.maxHold))
	require.NoError(t, err)
	return s
}

// holder and successor are current stores with the default hold, the
// controller's configuration.
func (env *epochEnv) holder(t *testing.T) *Store {
	t.Helper()
	return env.peer(t, peerOptions{version: peerCurrent, linger: DefaultControllerEpochInterlockLinger, maxHold: DefaultControllerEpochInterlockMaxHold})
}

func (env *epochEnv) successor(t *testing.T) *Store { return env.holder(t) }

// fencedWrite is one fenced control-store write: it creates a ConfigMap
// recording the epoch it ran under.
func fencedWrite(ctx context.Context, s *Store, fence store.ControllerEpochFence) error {
	return s.WithControllerEpochMutation(ctx, fence, func(writeCtx context.Context) error {
		return s.client.Create(writeCtx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: s.controlNamespace, GenerateName: "fenced-",
			Labels: map[string]string{envtestEpochLabel: strconv.FormatInt(fence.Epoch, 10), envtestWriterLabel: fence.HolderID},
		}})
	})
}

// writeLoop counts a holder's fenced writes by outcome.
type writeLoop struct {
	ok, contention, failedClosed atomic.Int32
	mu                           sync.Mutex
	other                        []error
	// draining stops new writes without cancelling the one in flight.
	draining atomic.Bool
	done     chan struct{}
}

// mirrorRefusal reports the refusal of a fenced write that read the old
// epoch's Lease and then the ControllerEpoch mirror of the new one
// (validateControllerEpochObjectForLease). It fails closed like a fence
// mismatch but is not wrapped as store.ErrConflict.
func mirrorRefusal(err error) bool {
	return strings.Contains(err.Error(), "conflicts with authoritative Lease") ||
		strings.Contains(err.Error(), "does not identify authoritative Lease")
}

// writeContinuously makes fenced writes back to back until ctx ends, as a
// busy holder does.
func writeContinuously(ctx context.Context, s *Store, fence store.ControllerEpochFence) *writeLoop {
	return writePaced(ctx, s, fence, 0)
}

// writePaced makes fenced writes, each pace after the previous one ended
// (back to back for zero), so the holder is idle for at least pace between
// writes however slow they are, until ctx ends or the loop drains.
func writePaced(ctx context.Context, s *Store, fence store.ControllerEpochFence, pace time.Duration) *writeLoop {
	loop := &writeLoop{done: make(chan struct{})}
	go func() {
		defer close(loop.done)
		for ctx.Err() == nil && !loop.draining.Load() {
			if pace > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(pace):
				}
			}
			err := fencedWrite(ctx, s, fence)
			switch {
			case err == nil:
				loop.ok.Add(1)
			case errors.Is(err, store.ErrControllerEpochMutationContention):
				loop.contention.Add(1)
			case errors.Is(err, store.ErrConflict), mirrorRefusal(err):
				loop.failedClosed.Add(1)
			case ctx.Err() != nil:
			default:
				loop.mu.Lock()
				loop.other = append(loop.other, err)
				loop.mu.Unlock()
			}
		}
	}()
	return loop
}

func (l *writeLoop) stop(cancel context.CancelFunc) {
	cancel()
	<-l.done
}

// drain stops the loop after the write in flight completes. Cancelling that
// write instead could leave a token write committed on the server with
// its response lost, which no store then knows to remove (see
// testAmbiguousTokenWrite).
func (l *writeLoop) drain() { l.draining.Store(true) }

func (l *writeLoop) otherErrors() []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.other...)
}

// leaseEvent is one committed change of the epoch Lease.
type leaseEvent struct {
	at              time.Time
	resourceVersion int64
	epoch           int64
	token           string
}

// leaseRecorder watches the epoch Lease and records every committed change.
type leaseRecorder struct {
	mu     sync.Mutex
	events []leaseEvent
	stop   context.CancelFunc
	done   chan struct{}
}

func (env *epochEnv) recordLease(t *testing.T) *leaseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	lease := &coordinationv1.Lease{}
	key := client.ObjectKey{Namespace: env.namespace, Name: controllerEpochLeaseName(env.fence.Name)}
	require.NoError(t, env.server.admin.Get(ctx, key, lease))
	watcher, err := env.server.admin.Watch(ctx, &coordinationv1.LeaseList{}, client.InNamespace(env.namespace),
		client.MatchingFields{"metadata.name": key.Name}, &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: lease.ResourceVersion}})
	require.NoError(t, err)
	recorder := &leaseRecorder{stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(recorder.done)
		defer watcher.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case event, open := <-watcher.ResultChan():
				if !open {
					return
				}
				changed, ok := event.Object.(*coordinationv1.Lease)
				if !ok || event.Type != watch.Modified {
					continue
				}
				rv, _ := strconv.ParseInt(changed.ResourceVersion, 10, 64)
				epoch, _ := strconv.ParseInt(changed.Annotations[annotationControllerEpoch], 10, 64)
				recorder.mu.Lock()
				recorder.events = append(recorder.events, leaseEvent{at: time.Now(), resourceVersion: rv, epoch: epoch, token: changed.Annotations[annotationMutationToken]})
				recorder.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-recorder.done })
	return recorder
}

// epochChange is the first recorded Lease change to epoch, waiting briefly
// for the watch to deliver it.
func (r *leaseRecorder) epochChange(t *testing.T, epoch int64) leaseEvent {
	t.Helper()
	var found leaseEvent
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, event := range r.events {
			if event.epoch == epoch {
				found = event
				return true
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond, "the watch never delivered the change to epoch %d", epoch)
	return found
}

// tokenWritten is the resourceVersion at which token was written, waiting
// briefly for the watch to deliver it.
func (r *leaseRecorder) tokenWritten(t *testing.T, token string) int64 {
	t.Helper()
	var rv int64
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, event := range r.events {
			if event.token == token {
				rv = event.resourceVersion
				return true
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond, "the watch never delivered token %s", token)
	return rv
}

// tokenWrites counts the token writes (a change to a new non-empty token)
// recorded before resourceVersion before.
func (r *leaseRecorder) tokenWrites(before int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count, last := 0, ""
	for _, event := range r.events {
		if event.resourceVersion >= before {
			break
		}
		if event.token != "" && event.token != last {
			count++
		}
		last = event.token
	}
	return count
}

// fencedWritesAfter lists every committed fenced write under epoch whose
// creation resourceVersion is after changeRV: a write under that epoch that
// committed after the epoch moved on.
func (env *epochEnv) fencedWritesAfter(t *testing.T, epoch, changeRV int64) (after []string, total int) {
	t.Helper()
	list := &corev1.ConfigMapList{}
	require.NoError(t, env.server.admin.List(context.Background(), list, client.InNamespace(env.namespace),
		client.MatchingLabels{envtestEpochLabel: strconv.FormatInt(epoch, 10)}))
	for _, object := range list.Items {
		// A ConfigMap is never updated here, so its resourceVersion is the
		// revision that created it.
		rv, err := strconv.ParseInt(object.ResourceVersion, 10, 64)
		require.NoError(t, err)
		if rv > changeRV {
			after = append(after, fmt.Sprintf("%s@%d", object.Name, rv))
		}
	}
	sort.Strings(after)
	return after, len(list.Items)
}

// acquisition is the outcome of one successor's epoch acquisition.
type acquisition struct {
	epoch    *store.ControllerEpoch
	attempts int
	elapsed  time.Duration
	err      error
}

// acquireAsManager acquires the next epoch exactly as
// ControllerEpochManager.Start does (internal/controller/controller_epoch.go,
// which this package cannot import): publish the takeover intent, read the
// epoch, then compare-and-swap with backoff (10ms per attempt, capped at 1s),
// publishing against the replaced epoch and re-reading after every conflict,
// for at most budget (the manager's is three minutes). A legacy successor
// publishes nothing; its store would drop the request anyway.
func acquireAsManager(ctx context.Context, s *Store, legacy bool, budget time.Duration) acquisition {
	const holder = envtestSuccessorID
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	publish := func(expected int64) {
		if !legacy {
			s.PublishControllerEpochTakeoverIntent(ctx, envtestEpochName, holder, expected)
		}
	}
	result := func(epoch *store.ControllerEpoch, attempts int, err error) acquisition {
		return acquisition{epoch: epoch, attempts: attempts, elapsed: time.Since(started), err: err}
	}
	publish(0)
	predecessor, err := s.GetControllerEpoch(ctx, envtestEpochName)
	if err != nil {
		return result(nil, 0, fmt.Errorf("read controller epoch: %w", err))
	}
	change := func(predecessor *store.ControllerEpoch) store.ControllerEpochCAS {
		return store.ControllerEpochCAS{
			Name: envtestEpochName, ExpectedVersion: predecessor.Version, ExpectedEpoch: predecessor.Epoch, NewEpoch: predecessor.Epoch + 1,
			HolderID: holder, RequestDigest: testDigest(fmt.Sprintf("%s/%d", holder, predecessor.Epoch+1)), UpdatedAt: time.Now().UTC(),
		}
	}
	pending := change(predecessor)
	// controllerEpochMatchesCAS in internal/controller.
	matches := func(epoch *store.ControllerEpoch) bool {
		return epoch != nil && epoch.Name == pending.Name && epoch.Version == pending.ExpectedVersion+1 &&
			epoch.Epoch == pending.NewEpoch && epoch.HolderID == pending.HolderID && epoch.RequestDigest == pending.RequestDigest &&
			epoch.AcquiredAt.Equal(pending.UpdatedAt) && epoch.UpdatedAt.Equal(pending.UpdatedAt)
	}
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return result(nil, attempt-1, fmt.Errorf("controller epoch acquisition did not converge: %w", ctx.Err()))
		}
		acquired, casErr := s.CompareAndSwapControllerEpoch(ctx, pending)
		if casErr == nil {
			if !matches(acquired) {
				return result(nil, attempt, fmt.Errorf("advance controller epoch: store returned %+v, not the submitted change", acquired))
			}
			return result(acquired, attempt, nil)
		}
		if !errors.Is(casErr, store.ErrConflict) {
			return result(nil, attempt, fmt.Errorf("advance controller epoch: %w", casErr))
		}
		publish(pending.ExpectedEpoch)
		observed, readErr := s.GetControllerEpoch(ctx, envtestEpochName)
		if readErr != nil {
			return result(nil, attempt, fmt.Errorf("reconcile controller epoch CAS conflict: %w", readErr))
		}
		switch {
		case matches(observed):
			return result(observed, attempt, nil)
		case observed.Epoch != pending.ExpectedEpoch:
			return result(nil, attempt, fmt.Errorf("epoch moved to %d/%s under the acquisition", observed.Epoch, observed.HolderID))
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(time.Duration(attempt)*10*time.Millisecond, time.Second)):
		}
	}
}

// requireFailClosed checks the server's record after the successor's epoch
// committed: no fenced write under the replaced epoch committed after it,
// the holder can no longer write under it, and the new epoch carries no
// token.
func requireFailClosed(t *testing.T, env *epochEnv, recorder *leaseRecorder, holder *Store) leaseEvent {
	t.Helper()
	change := recorder.epochChange(t, env.fence.Epoch+1)
	// Probe first, so the server-side check below covers the probe too.
	err := fencedWrite(t.Context(), holder, env.fence)
	require.ErrorIs(t, err, store.ErrConflict, "the holder wrote under the replaced epoch")
	require.NotErrorIs(t, err, store.ErrControllerEpochMutationContention, "the holder's write under the replaced epoch was not refused by the fence")
	after, total := env.fencedWritesAfter(t, env.fence.Epoch, change.resourceVersion)
	require.Empty(t, after, "fenced writes under epoch %d committed after epoch %d did (resourceVersion %d)", env.fence.Epoch, env.fence.Epoch+1, change.resourceVersion)
	require.Positive(t, total, "the holder made no fenced writes")
	lease := &coordinationv1.Lease{}
	require.NoError(t, env.server.admin.Get(t.Context(), client.ObjectKey{Namespace: env.namespace, Name: controllerEpochLeaseName(env.fence.Name)}, lease))
	require.Empty(t, lease.Annotations[annotationMutationToken], "a token was left on the new epoch")
	return change
}

func TestControllerEpochInterlockAgainstAPIServer(t *testing.T) {
	server := startEpochAPIServer(t)
	t.Run("TakeoverUnderContinuousWrites", func(t *testing.T) { testTakeoverUnderContinuousWrites(t, server) })
	t.Run("AmbiguousTokenRemovals", func(t *testing.T) { testAmbiguousTokenRemovals(t, server) })
	t.Run("AmbiguousTokenWrite", func(t *testing.T) { testAmbiguousTokenWrite(t, server) })
	t.Run("AmbiguousEpochChange", func(t *testing.T) { testAmbiguousEpochChange(t, server) })
	t.Run("ShutdownReleasesTheHeldToken", func(t *testing.T) { testShutdownReleasesTheHeldToken(t, server) })
	t.Run("MixedVersions", func(t *testing.T) { testMixedVersions(t, server) })
}

// waitForWrites waits, bounded, until a holder has made n fenced writes.
func waitForWrites(t *testing.T, loop *writeLoop, n int32) {
	t.Helper()
	require.Eventually(t, func() bool { return loop.ok.Load() >= n }, 20*time.Second, time.Millisecond, "the holder made %d of %d fenced writes", loop.ok.Load(), n)
}

// takeoverBound is ADR 0058's bound on a successor's epoch change after its
// takeover request commits, for a live holder making short writes: the
// holder's current token is reused for at most the max hold, removed within
// the linger after its last write, and the successor's next retry follows
// within the manager's 1s retry cap.
const takeoverBound = DefaultControllerEpochInterlockMaxHold + DefaultControllerEpochInterlockLinger + time.Second

// takeoverMargin absorbs scheduling and API latency on a loaded CI host.
const takeoverMargin = 5 * time.Second

// (a) A successor takes over from a holder writing continuously with the
// hold on, within the ADR's bound, and no write of the old epoch commits
// after the change.
func testTakeoverUnderContinuousWrites(t *testing.T, server *epochAPIServer) {
	const trials = 3
	var worst time.Duration
	for trial := range trials {
		env := server.newEnv(t)
		holder := env.holder(t)
		successor := env.successor(t)
		recorder := env.recordLease(t)
		ctx, cancel := context.WithCancel(t.Context())
		loop := writeContinuously(ctx, holder, env.fence)
		waitForWrites(t, loop, 20)
		// Start at a different point of the holder's 5s hold each trial.
		time.Sleep(time.Duration(trial) * DefaultControllerEpochInterlockMaxHold / trials)
		before := loop.ok.Load()

		got := acquireAsManager(t.Context(), successor, false, envtestManagerBudget)
		require.NoError(t, got.err)
		require.Less(t, got.elapsed, takeoverBound+takeoverMargin, "the successor's epoch change exceeded the ADR bound")
		require.Eventually(t, func() bool { return loop.failedClosed.Load() > 0 }, 10*time.Second, time.Millisecond, "the holder's writes did not fail closed")
		loop.stop(cancel)
		require.Empty(t, loop.otherErrors())
		change := requireFailClosed(t, env, recorder, holder)
		worst = max(worst, got.elapsed)
		t.Logf("trial %d: successor committed epoch %d %s after it started (%d CAS attempts); the holder made %d writes meanwhile (%d in all, %d refused for the takeover, %d failed closed), %d tokens before the change at resourceVersion %d",
			trial, got.epoch.Epoch, got.elapsed.Round(time.Millisecond), got.attempts, loop.ok.Load()-before, loop.ok.Load(), loop.contention.Load(), loop.failedClosed.Load(),
			recorder.tokenWrites(change.resourceVersion), change.resourceVersion)
		require.NoError(t, holder.ReleaseHeldControllerEpochInterlock(t.Context()))
	}
	t.Logf("worst takeover under continuous writes: %s (ADR bound %s)", worst.Round(time.Millisecond), takeoverBound)
}

// errAmbiguous is what a client sees when its request timed out: it cannot
// tell whether the server applied it.
func errAmbiguous(action string) error {
	return fmt.Errorf("simulated client timeout on %s: %w", action, context.DeadlineExceeded)
}

func isEpochLease(object client.Object) (*coordinationv1.Lease, bool) {
	lease, ok := object.(*coordinationv1.Lease)
	return lease, ok && strings.HasPrefix(lease.Name, controllerEpochLeasePrefix)
}

// (b) Token removals whose outcome is ambiguous: half are applied by the
// server and their response lost, half time out before they are sent. The
// holder writes continuously with a short max hold, so it removes tokens
// often. Fencing stays closed and the successor still acquires.
func testAmbiguousTokenRemovals(t *testing.T, server *epochAPIServer) {
	env := server.newEnv(t)
	var removals, appliedLost, notSent atomic.Int32
	faults := &interceptor.Funcs{
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			lease, ok := isEpochLease(object)
			if !ok || lease.Annotations[annotationMutationToken] != "" || lease.Annotations[annotationControllerEpoch] != strconv.FormatInt(env.fence.Epoch, 10) {
				return delegate.Update(ctx, object, options...)
			}
			if removals.Add(1)%2 == 1 {
				// The server applies it; the response never arrives, so the
				// caller's object keeps the version it was sent against.
				if err := delegate.Update(ctx, object.DeepCopyObject().(client.Object), options...); err != nil {
					return err
				}
				appliedLost.Add(1)
				return errAmbiguous("token removal")
			}
			notSent.Add(1)
			return errAmbiguous("token removal")
		},
	}
	holder := env.peer(t, peerOptions{version: peerCurrent, linger: DefaultControllerEpochInterlockLinger, maxHold: 200 * time.Millisecond, faults: faults})
	successor := env.successor(t)
	recorder := env.recordLease(t)
	ctx, cancel := context.WithCancel(t.Context())
	loop := writeContinuously(ctx, holder, env.fence)
	waitForWrites(t, loop, 40)
	require.Eventually(t, func() bool { return appliedLost.Load() >= 3 && notSent.Load() >= 3 }, 20*time.Second, time.Millisecond, "too few ambiguous removals")

	got := acquireAsManager(t.Context(), successor, false, envtestManagerBudget)
	require.NoError(t, got.err)
	require.Less(t, got.elapsed, takeoverBound+takeoverMargin)
	require.Eventually(t, func() bool { return loop.failedClosed.Load() > 0 }, 10*time.Second, time.Millisecond)
	loop.stop(cancel)
	change := requireFailClosed(t, env, recorder, holder)
	// A removal reported as failed surfaces only from a write that removes a
	// token before writing its own; the write fails without running.
	for _, err := range loop.otherErrors() {
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	t.Logf("successor committed epoch %d %s after it started (%d CAS attempts) at resourceVersion %d; %d removals (%d applied with the response lost, %d not sent); holder: %d writes, %d failed on a removal error, %d refused for the takeover, %d failed closed",
		got.epoch.Epoch, got.elapsed.Round(time.Millisecond), got.attempts, change.resourceVersion, removals.Load(), appliedLost.Load(), notSent.Load(),
		loop.ok.Load(), len(loop.otherErrors()), loop.contention.Load(), loop.failedClosed.Load())
}

// (b) A token write whose outcome is ambiguous: the server applies it and
// the response is lost, so the holder never learns its token. No fenced
// write runs under that token, the holder's own writes and the successor's
// epoch change are refused until it expires, and then the successor
// acquires. Instead of waiting out the two-minute expiry, the successor's
// clock is moved past it (the store's wallClock hook); nothing else writes a
// token meanwhile, because the stranded token stops the holder too.
func testAmbiguousTokenWrite(t *testing.T, server *epochAPIServer) {
	env := server.newEnv(t)
	var armed, fired atomic.Bool
	var stranded atomic.Value
	faults := &interceptor.Funcs{
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			lease, ok := isEpochLease(object)
			if !ok || lease.Annotations[annotationMutationToken] == "" || !armed.Load() || fired.Load() {
				return delegate.Update(ctx, object, options...)
			}
			// Only an update the server applied is made ambiguous; a
			// conflict stays an ordinary conflict.
			if err := delegate.Update(ctx, object.DeepCopyObject().(client.Object), options...); err != nil {
				return err
			}
			fired.Store(true)
			stranded.Store(lease.Annotations[annotationMutationToken])
			return errAmbiguous("token write")
		},
	}
	holder := env.peer(t, peerOptions{version: peerCurrent, linger: DefaultControllerEpochInterlockLinger, maxHold: 200 * time.Millisecond, faults: faults})
	successor := env.successor(t)
	recorder := env.recordLease(t)
	ctx, cancel := context.WithCancel(t.Context())
	loop := writeContinuously(ctx, holder, env.fence)
	waitForWrites(t, loop, 20)
	armed.Store(true)
	require.Eventually(t, func() bool { return fired.Load() && len(loop.otherErrors()) > 0 }, 20*time.Second, time.Millisecond)
	okAtFault := loop.ok.Load()
	// The holder's writes are refused, never run, while its stranded token
	// stands; so is the successor's change.
	require.Eventually(t, func() bool { return loop.contention.Load() >= 2 }, 20*time.Second, time.Millisecond)
	require.Equal(t, okAtFault, loop.ok.Load(), "fenced writes ran under a token the holder never learned")
	_, err := successor.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		Name: envtestEpochName, ExpectedVersion: env.fence.Epoch, ExpectedEpoch: env.fence.Epoch, NewEpoch: env.fence.Epoch + 1,
		HolderID: envtestSuccessorID, RequestDigest: testDigest("refused"), UpdatedAt: time.Now().UTC(),
	})
	require.ErrorIs(t, err, store.ErrConflict, "the epoch advanced past a live stranded token")
	lease := &coordinationv1.Lease{}
	require.NoError(t, server.admin.Get(t.Context(), client.ObjectKey{Namespace: env.namespace, Name: controllerEpochLeaseName(env.fence.Name)}, lease))
	require.Equal(t, stranded.Load(), lease.Annotations[annotationMutationToken])
	expiresAt, err := parseTime("expiry", lease.Annotations[annotationMutationExpiresAt])
	require.NoError(t, err)
	// The server's record: no fenced write committed once the stranded token
	// stood, whatever the holder's calls returned.
	strandedAt := recorder.tokenWritten(t, stranded.Load().(string))
	after, _ := env.fencedWritesAfter(t, env.fence.Epoch, strandedAt)
	require.Empty(t, after, "fenced writes committed under a token the holder never learned")

	successor.wallClock = func() time.Time { return time.Now().Add(controllerEpochMutationTokenTTL + time.Second) }
	got := acquireAsManager(t.Context(), successor, false, 30*time.Second)
	require.NoError(t, got.err)
	require.Eventually(t, func() bool { return loop.failedClosed.Load() > 0 }, 10*time.Second, time.Millisecond)
	loop.stop(cancel)
	change := requireFailClosed(t, env, recorder, holder)
	t.Logf("a token write applied with its response lost stranded the epoch until %s (%s after the fault); the holder ran %d writes after it and was refused %d times; the successor's real-clock change was refused, and past the expiry it committed epoch %d at resourceVersion %d in %s",
		expiresAt.Format(time.RFC3339), time.Until(expiresAt).Round(time.Second), loop.ok.Load()-okAtFault, loop.contention.Load(), got.epoch.Epoch, change.resourceVersion, got.elapsed.Round(time.Millisecond))
}

// (b) The successor's epoch change is applied by the server and its
// response lost. The old holder fails closed at once, and the successor's
// retry of the same change is idempotent.
func testAmbiguousEpochChange(t *testing.T, server *epochAPIServer) {
	env := server.newEnv(t)
	var fired atomic.Bool
	faults := &interceptor.Funcs{
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			lease, ok := isEpochLease(object)
			if !ok || lease.Annotations[annotationControllerEpoch] != strconv.FormatInt(env.fence.Epoch+1, 10) || fired.Load() {
				return delegate.Update(ctx, object, options...)
			}
			if err := delegate.Update(ctx, object.DeepCopyObject().(client.Object), options...); err != nil {
				return err
			}
			fired.Store(true)
			return errAmbiguous("epoch change")
		},
	}
	holder := env.holder(t)
	successor := env.peer(t, peerOptions{version: peerCurrent, linger: DefaultControllerEpochInterlockLinger, maxHold: DefaultControllerEpochInterlockMaxHold, faults: faults})
	recorder := env.recordLease(t)
	ctx, cancel := context.WithCancel(t.Context())
	loop := writeContinuously(ctx, holder, env.fence)
	waitForWrites(t, loop, 20)
	change := store.ControllerEpochCAS{
		Name: envtestEpochName, ExpectedVersion: env.fence.Epoch, ExpectedEpoch: env.fence.Epoch, NewEpoch: env.fence.Epoch + 1,
		HolderID: envtestSuccessorID, RequestDigest: testDigest("ambiguous"), UpdatedAt: time.Now().UTC(),
	}
	started := time.Now()
	var err error
	for !fired.Load() {
		require.Less(t, time.Since(started), takeoverBound+takeoverMargin)
		_, err = successor.CompareAndSwapControllerEpoch(t.Context(), change)
		if !fired.Load() {
			require.ErrorIs(t, err, store.ErrConflict)
			time.Sleep(100 * time.Millisecond)
		}
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Eventually(t, func() bool { return loop.failedClosed.Load() > 0 }, 10*time.Second, time.Millisecond)
	loop.stop(cancel)
	committed := requireFailClosed(t, env, recorder, holder)
	retried, err := successor.CompareAndSwapControllerEpoch(t.Context(), change)
	require.NoError(t, err, "a retry of the committed change is not idempotent")
	require.Equal(t, env.fence.Epoch+1, retried.Epoch)
	t.Logf("epoch change applied with its response lost %s after the successor started, at resourceVersion %d; the holder failed closed (%d writes before); the identical retry returned epoch %d",
		time.Since(started).Round(time.Millisecond), committed.resourceVersion, loop.ok.Load(), retried.Epoch)
}

// (c) A holder that stops, mid-burst, releases its held token on the way
// out (as the epoch manager and cmd/main do), and a successor starting right
// after acquires on its first attempt instead of waiting out the token's
// expiry. Without the release, as after a crash, the successor is refused
// until the token expires.
func testShutdownReleasesTheHeldToken(t *testing.T, server *epochAPIServer) {
	for _, release := range []bool{true, false} {
		env := server.newEnv(t)
		// A linger longer than the test, so no idle timer removes the token:
		// only the explicit release can.
		holder := env.peer(t, peerOptions{version: peerCurrent, linger: time.Hour, maxHold: maxControllerEpochInterlockHold})
		successor := env.successor(t)
		recorder := env.recordLease(t)
		ctx, cancel := context.WithCancel(t.Context())
		loop := writeContinuously(ctx, holder, env.fence)
		waitForWrites(t, loop, 20)
		loop.drain()
		<-loop.done
		cancel()
		// Timed from when the holder's last write ended.
		stopped := time.Now()
		// The last write parked its token on the Lease.
		holder.interlockMu.Lock()
		require.NotNil(t, holder.heldInterlock, "the last write parked no token")
		heldToken := holder.heldInterlock.snapshot.MutationToken
		if !release {
			// The process dies: its idle timer never runs.
			holder.heldInterlock.timer.Stop()
		}
		holder.interlockMu.Unlock()
		held := &coordinationv1.Lease{}
		require.NoError(t, server.admin.Get(t.Context(), client.ObjectKey{Namespace: env.namespace, Name: controllerEpochLeaseName(env.fence.Name)}, held))
		require.Equal(t, heldToken, held.Annotations[annotationMutationToken], "the parked token is not on the Lease")
		if release {
			require.NoError(t, holder.ReleaseHeldControllerEpochInterlock(context.Background()))
			require.NoError(t, server.admin.Get(t.Context(), client.ObjectKey{Namespace: env.namespace, Name: controllerEpochLeaseName(env.fence.Name)}, held))
			require.Empty(t, held.Annotations[annotationMutationToken], "the release did not remove the held token")
		}
		released := time.Since(stopped)
		if release {
			got := acquireAsManager(t.Context(), successor, false, 30*time.Second)
			require.NoError(t, got.err)
			require.Equal(t, 1, got.attempts, "the successor waited for a released token")
			// Far inside the token's two-minute life, which a crash costs.
			require.Less(t, released+got.elapsed, controllerEpochMutationTokenTTL/4)
			change := requireFailClosed(t, env, recorder, holder)
			t.Logf("release on stop: holder released its token %s after its last write; the successor committed epoch %d on its first attempt %s later at resourceVersion %d (token TTL %s)",
				released.Round(time.Millisecond), got.epoch.Epoch, got.elapsed.Round(time.Millisecond), change.resourceVersion, controllerEpochMutationTokenTTL)
			continue
		}
		lease := &coordinationv1.Lease{}
		require.NoError(t, server.admin.Get(t.Context(), client.ObjectKey{Namespace: env.namespace, Name: controllerEpochLeaseName(env.fence.Name)}, lease))
		require.NotEmpty(t, lease.Annotations[annotationMutationToken], "a crashed holder left no token")
		expiresAt, err := parseTime("expiry", lease.Annotations[annotationMutationExpiresAt])
		require.NoError(t, err)
		got := acquireAsManager(t.Context(), successor, false, 3*time.Second)
		require.Error(t, got.err, "the successor advanced past a crashed holder's live token")
		require.NoError(t, server.admin.Get(t.Context(), client.ObjectKey{Namespace: env.namespace, Name: controllerEpochLeaseName(env.fence.Name)}, lease))
		require.Equal(t, strconv.FormatInt(env.fence.Epoch, 10), lease.Annotations[annotationControllerEpoch], "the epoch advanced past a crashed holder's live token")
		t.Logf("control, no release (crash): the successor was refused for %s over %d attempts; the token stands until %s, %s after the holder's last write",
			got.elapsed.Round(time.Millisecond), got.attempts, expiresAt.Format(time.RFC3339), expiresAt.Sub(stopped).Round(time.Second))
	}
}

// mixedScenario is one holder/successor pairing under one write load.
type mixedScenario struct {
	holder, successor peerVersion
	// unthrottled drops the controller's client-side rate limit on both.
	unthrottled bool
	// pace spaces the holder's writes; zero writes back to back.
	pace time.Duration
	// handoff stops the holder and releases its held token before the
	// successor starts, as on a leadership loss or shutdown.
	handoff bool
}

func (m mixedScenario) String() string {
	name := map[peerVersion]string{peerCurrent: "new", peerLegacy: "old"}
	load := "back to back"
	if m.pace > 0 {
		load = "every " + m.pace.String()
	}
	throttle := "controller rate limit"
	if m.unthrottled {
		throttle = "unthrottled"
	}
	if m.handoff {
		load += ", holder stops first"
	}
	return fmt.Sprintf("%s holder, %s successor, writes %s, %s", name[m.holder], name[m.successor], load, throttle)
}

// mixedTakeover runs one holder writing continuously against one successor
// acquiring as the epoch manager does, for at most budget. Whatever the
// outcome, no fenced write of the old epoch may commit after a change.
func mixedTakeover(t *testing.T, server *epochAPIServer, scenario mixedScenario, budget time.Duration) (acquisition, *writeLoop, int) {
	t.Helper()
	env := server.newEnv(t)
	hold := peerOptions{linger: DefaultControllerEpochInterlockLinger, maxHold: DefaultControllerEpochInterlockMaxHold, unthrottled: scenario.unthrottled}
	holderOptions, successorOptions := hold, hold
	holderOptions.version, successorOptions.version = scenario.holder, scenario.successor
	holder := env.peer(t, holderOptions)
	successor := env.peer(t, successorOptions)
	recorder := env.recordLease(t)
	ctx, cancel := context.WithCancel(t.Context())
	loop := writePaced(ctx, holder, env.fence, scenario.pace)
	waitForWrites(t, loop, 20)
	if scenario.handoff {
		loop.drain()
		<-loop.done
		require.NoError(t, holder.ReleaseHeldControllerEpochInterlock(t.Context()))
	}
	got := acquireAsManager(t.Context(), successor, scenario.successor == peerLegacy, budget)
	var tokens int
	lease := &coordinationv1.Lease{}
	require.NoError(t, server.admin.Get(t.Context(), client.ObjectKey{Namespace: env.namespace, Name: controllerEpochLeaseName(env.fence.Name)}, lease))
	// An acquisition that ran out of budget after its compare-and-swap
	// committed still changed the epoch: judge by the Lease, not the error.
	if committed := lease.Annotations[annotationControllerEpoch] != strconv.FormatInt(env.fence.Epoch, 10); committed {
		if got.err != nil {
			got.err = fmt.Errorf("epoch committed, but the acquisition returned: %w", got.err)
		}
		if !scenario.handoff {
			require.Eventually(t, func() bool { return loop.failedClosed.Load() > 0 }, 10*time.Second, time.Millisecond)
		}
		loop.stop(cancel)
		change := requireFailClosed(t, env, recorder, holder)
		tokens = recorder.tokenWrites(change.resourceVersion)
	} else {
		loop.stop(cancel)
		tokens = recorder.tokenWrites(1 << 62)
	}
	require.Empty(t, loop.otherErrors())
	require.NoError(t, holder.ReleaseHeldControllerEpochInterlock(t.Context()))
	return got, loop, tokens
}

func describeTakeover(got acquisition, loop *writeLoop, tokens int) string {
	var outcome string
	if got.err == nil {
		outcome = fmt.Sprintf("committed epoch %d after %s", got.epoch.Epoch, got.elapsed.Round(time.Millisecond))
	} else {
		outcome = fmt.Sprintf("NOT committed after %s (%v)", got.elapsed.Round(time.Millisecond), got.err)
	}
	return fmt.Sprintf("%s over %d CAS attempts; holder made %d writes under %d tokens, %d refused for a takeover, %d failed closed",
		outcome, got.attempts, loop.ok.Load(), tokens, loop.contention.Load(), loop.failedClosed.Load())
}

// (d) Mixed versions during a rolling upgrade (old holder, new successor)
// or a rollback (new holder, old successor), against the same pairings
// without a mixed version. The old side is simulated by legacyEpochPeer.
//
// Bounded rows, where the holder honours takeover requests or stops first,
// must take over within the ADR bound. The other rows record their outcome
// without asserting it, because nothing bounds it: the successor must read
// and compare-and-swap inside a gap between the holder's tokens. An old
// holder leaves a gap after every write, and in practice only back-to-back
// writes without a client rate limit starve a successor there (as before
// ADR 0058). A new holder whose writes come faster than its linger leaves
// one only at each max-hold rollover, a few reads wide, and an old
// successor, which publishes no takeover request, mostly misses it. The
// pace and the rate limit set how wide those gaps are.
func testMixedVersions(t *testing.T, server *epochAPIServer) {
	if testing.Short() {
		t.Skip("mixed-version takeover measurement runs for over a minute")
	}
	budget := 10 * time.Second
	if raw := os.Getenv("ORKA_EPOCH_MIXED_BUDGET"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		require.NoError(t, err)
		budget = parsed
	}
	const paced = 200 * time.Millisecond
	for _, scenario := range []struct {
		mixedScenario
		bounded bool
	}{
		{mixedScenario{holder: peerLegacy, successor: peerLegacy}, false},
		{mixedScenario{holder: peerLegacy, successor: peerCurrent}, false},
		{mixedScenario{holder: peerCurrent, successor: peerCurrent}, true},
		{mixedScenario{holder: peerCurrent, successor: peerLegacy}, false},
		{mixedScenario{holder: peerLegacy, successor: peerLegacy, unthrottled: true, pace: paced}, false},
		{mixedScenario{holder: peerLegacy, successor: peerCurrent, unthrottled: true, pace: paced}, false},
		{mixedScenario{holder: peerCurrent, successor: peerCurrent, unthrottled: true, pace: paced}, true},
		{mixedScenario{holder: peerCurrent, successor: peerLegacy, unthrottled: true, pace: paced}, false},
		{mixedScenario{holder: peerCurrent, successor: peerCurrent, unthrottled: true}, true},
		{mixedScenario{holder: peerCurrent, successor: peerLegacy, unthrottled: true}, false},
		{mixedScenario{holder: peerLegacy, successor: peerLegacy, unthrottled: true}, false},
		{mixedScenario{holder: peerLegacy, successor: peerCurrent, unthrottled: true}, false},
		{mixedScenario{holder: peerCurrent, successor: peerLegacy, unthrottled: true, handoff: true}, true},
	} {
		// Bounded rows get the manager's whole window, so only the asserted
		// bound can fail them; unbounded rows stop at the budget.
		rowBudget := budget
		if scenario.bounded {
			rowBudget = envtestManagerBudget
		}
		got, loop, tokens := mixedTakeover(t, server, scenario.mixedScenario, rowBudget)
		t.Logf("%s: %s", scenario.mixedScenario, describeTakeover(got, loop, tokens))
		if scenario.bounded {
			require.NoError(t, got.err, "%s", scenario.mixedScenario)
			require.Less(t, got.elapsed, takeoverBound+takeoverMargin, "%s", scenario.mixedScenario)
		}
	}
}
