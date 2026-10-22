package storagefault_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storagefault"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
	"github.com/stretchr/testify/require"
)

const (
	state     = storagev1.StateContract
	artifacts = storagev1.ArtifactsContract
)

func openSQLite(t *testing.T) *sqlite.Store {
	t.Helper()
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

func newClient(t *testing.T) (*storageclient.Client, *storagefault.Injector) {
	t.Helper()
	client, faults, err := storagefault.NewEmbeddedClient(openSQLite(t))
	require.NoError(t, err)
	return client, faults
}

var (
	owner = storagev1.ArtifactOwner{Namespace: "tenant", TaskName: "task"}
	key   = storagev1.ArtifactKey{Owner: owner, Filename: "out.txt"}
)

func saveArtifact(ctx context.Context, provider storagev1.ArtifactStore) error {
	return provider.SaveArtifact(ctx, storagev1.SaveArtifactRequest{Owner: owner, Filename: key.Filename, ContentType: "text/plain", Data: []byte("file")})
}

func requireInjected(t *testing.T, err error, target error) {
	t.Helper()
	require.ErrorIs(t, err, target)
	var injected *storagefault.Error
	require.ErrorAs(t, err, &injected, "the outcome came from a fault, not the provider")
}

// Without faults the wrappers are transparent: the shared provider suite
// passes through the embedded wrapper inside a storage client and through the
// artifact wrapper, and every call reaches the provider.
func TestWrappersPassCallsThrough(t *testing.T) {
	var mu sync.Mutex
	var injectors []*storagefault.Injector
	track := func(faults *storagefault.Injector) {
		mu.Lock()
		defer mu.Unlock()
		injectors = append(injectors, faults)
	}
	t.Run("embedded client", func(t *testing.T) {
		storagetest.RunState(t, func(t *testing.T) storagev1.StateProvider {
			client, faults := newClient(t)
			track(faults)
			return client
		})
	})
	t.Run("artifact provider", func(t *testing.T) {
		storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
			faults := storagefault.New()
			track(faults)
			return faults.WrapArtifacts(openSQLite(t))
		})
	})
	calls := 0
	for _, faults := range injectors {
		counts := faults.Counts(storagefault.Match{})
		require.Equal(t, counts.Calls, counts.Delivered, "every call reached the provider")
		calls += counts.Calls
	}
	require.Positive(t, calls)
}

// Every generated method passes through the injector under the contract its
// wrapper assigns. With both contracts down, no call reaches the provider, so
// each method can be called with zero arguments.
func TestEveryMethodPassesThroughTheInjector(t *testing.T) {
	artifactStore := reflect.TypeFor[storagev1.ArtifactStore]()
	for name, tc := range map[string]struct {
		wrap     func(*storagefault.Injector, *sqlite.Store) any
		iface    reflect.Type
		contract func(method string) string
	}{
		"state": {
			wrap:     func(f *storagefault.Injector, s *sqlite.Store) any { return f.WrapState(s) },
			iface:    reflect.TypeFor[storagev1.StateProvider](),
			contract: func(string) string { return state },
		},
		"artifacts": {
			wrap:     func(f *storagefault.Injector, s *sqlite.Store) any { return f.WrapArtifacts(s) },
			iface:    reflect.TypeFor[storagev1.ArtifactProvider](),
			contract: func(string) string { return artifacts },
		},
		"embedded": {
			wrap:  func(f *storagefault.Injector, s *sqlite.Store) any { return f.WrapEmbedded(s) },
			iface: reflect.TypeFor[storageclient.EmbeddedStore](),
			contract: func(method string) string {
				if _, ok := artifactStore.MethodByName(method); ok {
					return artifacts
				}
				return state
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			faults := storagefault.New()
			faults.Down(state)
			faults.Down(artifacts)
			wrapped := reflect.ValueOf(tc.wrap(faults, openSQLite(t)))
			for method := range tc.iface.Methods() {
				signature := method.Type
				args := []reflect.Value{reflect.ValueOf(t.Context())}
				for i := 1; i < signature.NumIn(); i++ {
					args = append(args, reflect.Zero(signature.In(i)))
				}
				fn := wrapped.MethodByName(method.Name)
				var results []reflect.Value
				if signature.IsVariadic() {
					results = fn.CallSlice(args)
				} else {
					results = fn.Call(args)
				}
				for _, result := range results[:len(results)-1] {
					require.Truef(t, result.IsZero(), "%s returns zero results with an injected error", method.Name)
				}
				err, _ := reflect.TypeAssert[error](results[len(results)-1])
				var injected *storagefault.Error
				require.ErrorAsf(t, err, &injected, "%s passes through the injector", method.Name)
				require.Equal(t, method.Name, injected.Method)
				require.Equalf(t, tc.contract(method.Name), injected.Contract, "contract of %s", method.Name)
				require.Equal(t, storagefault.Counts{Calls: 1}, faults.Counts(storagefault.Match{Methods: []string{method.Name}}))
			}
		})
	}
}

func TestStoresGoDownAndComeBackSeparately(t *testing.T) {
	client, faults := newClient(t)
	ctx := t.Context()
	require.NoError(t, client.SaveResult(ctx, "tenant", "task", []byte("kept")))
	require.NoError(t, saveArtifact(ctx, client))

	faults.ResetCounts()
	outage := faults.Down(state)
	_, err := client.GetResult(ctx, "tenant", "task")
	requireInjected(t, err, storagev1.ErrUnavailable)
	requireInjected(t, client.HealthCheck(ctx), storagev1.ErrUnavailable)
	_, err = client.Describe(ctx)
	requireInjected(t, err, storagev1.ErrUnavailable)
	_, err = client.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: "task-uid", Digest: "sha256:digest"})
	requireInjected(t, err, storagev1.ErrUnavailable)
	data, _, err := client.GetArtifact(ctx, key)
	require.NoError(t, err, "artifacts keep working while state storage is down")
	require.Equal(t, []byte("file"), data)
	require.Equal(t, storagefault.Counts{Calls: 4}, faults.Counts(storagefault.Match{Contract: state}))

	outage.Remove()
	data, err = client.GetResult(ctx, "tenant", "task")
	require.NoError(t, err, "state storage is back")
	require.Equal(t, []byte("kept"), data)

	outage = faults.Down(artifacts)
	_, _, err = client.GetArtifact(ctx, key)
	requireInjected(t, err, storagev1.ErrUnavailable)
	requireInjected(t, saveArtifact(ctx, client), storagev1.ErrUnavailable)
	require.NoError(t, client.SaveResult(ctx, "tenant", "task-2", []byte("new")), "state keeps working while artifact storage is down")
	outage.Remove()
	_, _, err = client.GetArtifact(ctx, key)
	require.NoError(t, err)
	require.Equal(t, 2, faults.Counts(storagefault.Match{Contract: artifacts, Methods: []string{"GetArtifact"}}).Delivered)
}

func TestFaultsMatchMethodsAndKinds(t *testing.T) {
	client, faults := newClient(t)
	ctx := t.Context()
	require.NoError(t, client.SaveResult(ctx, "tenant", "task", []byte("kept")))

	writes := faults.Inject(storagefault.Fault{
		Match:  storagefault.Match{Contract: state, Kinds: []storagev1.MethodKind{storagev1.MethodWrite}},
		Action: storagefault.Fail(storagev1.ErrNotReady),
	})
	requireInjected(t, client.SaveResult(ctx, "tenant", "task", []byte("changed")), storagev1.ErrNotReady)
	data, err := client.GetResult(ctx, "tenant", "task")
	require.NoError(t, err, "reads continue while writes fail")
	require.Equal(t, []byte("kept"), data)

	// The fault added last applies.
	method := faults.Inject(storagefault.Fault{
		Match:  storagefault.Match{Methods: []string{"SaveResult"}},
		Action: storagefault.Fail(storagev1.ErrUnauthenticated),
	})
	requireInjected(t, client.SaveResult(ctx, "tenant", "task", []byte("changed")), storagev1.ErrUnauthenticated)
	requireInjected(t, client.DeleteResult(ctx, "tenant", "task"), storagev1.ErrNotReady)
	method.Remove()
	writes.Remove()
	require.Equal(t, 2, writes.Hits())
	require.Equal(t, 1, method.Hits())

	once := faults.Inject(storagefault.Fault{Action: storagefault.Fail(storagev1.ErrUnavailable), Times: 1})
	requireInjected(t, client.SaveResult(ctx, "tenant", "task", []byte("changed")), storagev1.ErrUnavailable)
	require.NoError(t, client.SaveResult(ctx, "tenant", "task", []byte("changed")), "a fault with Times removes itself")
	require.Equal(t, 1, once.Hits())

	faults.ResetCounts()
	require.Equal(t, storagefault.Counts{}, faults.Counts(storagefault.Match{}))
}

func TestLostResponseAfterCommit(t *testing.T) {
	client, faults := newClient(t)
	ctx := t.Context()
	lost := faults.Inject(storagefault.Fault{Action: storagefault.LoseResponse(), Times: 2})

	requireInjected(t, client.SaveResult(ctx, "tenant", "task", []byte("committed")), storagev1.ErrOutcomeUnknown)
	_, err := client.GetResult(ctx, "tenant", "task")
	requireInjected(t, err, storagev1.ErrUnavailable)
	require.Equal(t, 2, lost.Hits())

	data, err := client.GetResult(ctx, "tenant", "task")
	require.NoError(t, err)
	require.Equal(t, []byte("committed"), data, "the write committed although its response was lost")
	require.Equal(t, storagefault.Counts{Calls: 3, Delivered: 3}, faults.Counts(storagefault.Match{}))
}

func TestHangHoldsCallsUntilReleased(t *testing.T) {
	client, faults := newClient(t)
	ctx := t.Context()
	hang := storagefault.NewHang()
	faults.Inject(storagefault.Fault{Match: storagefault.Match{Methods: []string{"GetArtifact"}}, Action: hang})
	require.NoError(t, saveArtifact(ctx, client))

	cancelled, cancel := context.WithCancel(ctx)
	results := make(chan error, 2)
	go func() {
		_, _, err := client.GetArtifact(cancelled, key)
		results <- err
	}()
	go func() {
		_, _, err := client.GetArtifact(ctx, key)
		results <- err
	}()
	waitCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	require.NoError(t, hang.WaitHeld(waitCtx, 2))
	require.NoError(t, client.HealthCheck(ctx), "other calls proceed while calls are held")
	require.Equal(t, 0, faults.Counts(storagefault.Match{Methods: []string{"GetArtifact"}}).Delivered)

	cancel()
	err := <-results
	requireInjected(t, err, storagev1.ErrUnavailable)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, hang.WaitHeld(waitCtx, 1))
	require.Equal(t, 1, hang.Held())

	hang.Release()
	require.NoError(t, <-results, "a released call reaches the provider")
	_, _, err = client.GetArtifact(ctx, key)
	require.NoError(t, err, "calls after Release pass")
	require.Equal(t, storagefault.Counts{Calls: 3, Delivered: 2}, faults.Counts(storagefault.Match{Methods: []string{"GetArtifact"}}))
	require.Equal(t, 0, hang.Held())
}

func TestDelay(t *testing.T) {
	client, faults := newClient(t)
	ctx := t.Context()
	short := faults.Inject(storagefault.Fault{
		Match:  storagefault.Match{Methods: []string{"HealthCheck"}},
		Action: storagefault.Delay(50 * time.Millisecond),
		Times:  1,
	})
	started := time.Now()
	require.NoError(t, client.HealthCheck(ctx))
	require.GreaterOrEqual(t, time.Since(started), 50*time.Millisecond)
	require.Equal(t, 1, short.Hits())

	// A delay far longer than the deadline, so the deadline always ends it.
	faults.Inject(storagefault.Fault{Match: storagefault.Match{Methods: []string{"HealthCheck"}}, Action: storagefault.Delay(time.Hour)})
	deadline, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	err := client.HealthCheck(deadline)
	requireInjected(t, err, storagev1.ErrUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, storagefault.Counts{Calls: 2, Delivered: 1}, faults.Counts(storagefault.Match{}))
}

func TestMatchByCallClass(t *testing.T) {
	client, faults := newClient(t)
	ctx := t.Context()
	bulk := faults.Inject(storagefault.Fault{
		Match:  storagefault.Match{Classes: []storagev1.CallClass{storagev1.CallBulk}},
		Action: storagefault.Fail(storagev1.ErrUnavailable),
	})
	requireInjected(t, client.SaveResult(ctx, "tenant", "task", []byte("x")), storagev1.ErrUnavailable)
	requireInjected(t, saveArtifact(ctx, client), storagev1.ErrUnavailable)
	require.NoError(t, client.DeleteResult(ctx, "tenant", "task"), "interactive calls are not matched")
	require.NoError(t, client.HealthCheck(ctx), "control calls are not matched")
	_, err := client.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: "task-uid", Digest: "sha256:digest"})
	require.NotErrorIs(t, err, storagev1.ErrUnavailable, "methods outside the catalog match no class")
	require.Equal(t, 2, bulk.Hits())
}

// The embedded client probes its store through the state binding. A binding
// check for artifacts wraps the same store with WrapArtifacts, which shares
// the injector, so the artifact store's outage and Describe changes reach it
// while the state binding stays healthy.
func TestArtifactsBindingProbesOfTheEmbeddedStore(t *testing.T) {
	faults := storagefault.New()
	backend := openSQLite(t)
	client, err := storageclient.NewEmbedded(faults.WrapEmbedded(backend))
	require.NoError(t, err)
	artifactBinding := faults.WrapArtifacts(backend)
	ctx := t.Context()

	outage := faults.Down(artifacts)
	requireInjected(t, artifactBinding.HealthCheck(ctx), storagev1.ErrUnavailable)
	_, err = artifactBinding.Describe(ctx)
	requireInjected(t, err, storagev1.ErrUnavailable)
	require.NoError(t, client.HealthCheck(ctx), "the state binding of the same store stays healthy")
	outage.Remove()

	faults.SetDatasetID(artifacts, "another-dataset")
	info, err := artifactBinding.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, "another-dataset", info.DatasetID)
	stateInfo, err := client.Describe(ctx)
	require.NoError(t, err)
	require.NotEqual(t, "another-dataset", stateInfo.DatasetID)
}

// notWrappedYet lists catalog methods of optional provider interfaces that the
// wrappers do not serve until those interfaces join StateProvider or
// ArtifactProvider.
var notWrappedYet = []string{"AdvanceControllerEpoch", "GetControllerEpochMirror", "ResolveOperation", "PruneOperationReceipts"}

// Every catalog method reaches the provider through the wrappers, except the
// listed optional ones, and those stay listed only while the embedded store
// does not serve them either, so the wrappers never hide a method from a
// caller that type-asserts for it.
func TestWrappersServeEveryCatalogMethod(t *testing.T) {
	backend := openSQLite(t)
	wrapped := reflect.TypeOf(storagefault.New().WrapEmbedded(backend))
	inner := reflect.TypeOf(backend)
	for name := range storagev1.Methods() {
		if slices.Contains(notWrappedYet, name) {
			_, wraps := wrapped.MethodByName(name)
			require.Falsef(t, wraps, "%s is wrapped now; remove it from notWrappedYet", name)
			_, serves := inner.MethodByName(name)
			require.Falsef(t, serves, "SQLite serves %s, which the wrappers hide; wrap its interface", name)
			continue
		}
		_, ok := wrapped.MethodByName(name)
		require.Truef(t, ok, "the wrappers do not serve catalog method %s", name)
	}
}

func TestDescribeChanges(t *testing.T) {
	faults := storagefault.New()
	backend := openSQLite(t)
	stateProvider, artifactProvider := faults.WrapState(backend), faults.WrapArtifacts(backend)
	ctx := t.Context()
	original, err := stateProvider.Describe(ctx)
	require.NoError(t, err)

	incarnation := faults.SetIncarnation(state, "after-failover")
	dataset := faults.SetDatasetID(artifacts, "another-dataset")
	changed, err := stateProvider.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, "after-failover", changed.Incarnation)
	require.Equal(t, original.DatasetID, changed.DatasetID)
	other, err := artifactProvider.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, "another-dataset", other.DatasetID)
	require.Equal(t, original.Incarnation, other.Incarnation)

	both := faults.ModifyDescribe("", func(info *storagev1.ProviderInfo) {
		info.State = storagev1.DatasetIncompatible
		info.Contracts[0] = "changed"
	})
	changed, err = stateProvider.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, storagev1.DatasetIncompatible, changed.State)
	require.Equal(t, "after-failover", changed.Incarnation, "changes apply together, in order")
	again, err := backend.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, original.Contracts, again.Contracts, "changes never reach the provider's own report")

	rejected := faults.Inject(storagefault.Fault{
		Match:  storagefault.Match{Methods: []string{"Describe", "HealthCheck"}},
		Action: storagefault.Fail(storagev1.ErrUnauthenticated),
	})
	_, err = stateProvider.Describe(ctx)
	requireInjected(t, err, storagev1.ErrUnauthenticated)
	requireInjected(t, artifactProvider.HealthCheck(ctx), storagev1.ErrUnauthenticated)
	rejected.Remove()

	for _, rule := range []*storagefault.Rule{incarnation, dataset, both} {
		rule.Remove()
	}
	restored, err := stateProvider.Describe(ctx)
	require.NoError(t, err)
	require.Equal(t, original.Incarnation, restored.Incarnation)
	require.Equal(t, original.Contracts, restored.Contracts)
	require.Equal(t, 2, incarnation.Hits(), "a change applies only to a Describe the provider answered")
}

func TestErrorUnwrapsToTheInjectedOutcome(t *testing.T) {
	err := error(&storagefault.Error{Contract: state, Method: "GetResult", Err: storagev1.ErrUnavailable})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
	require.Equal(t, "storagefault: state.v1 GetResult: storage unavailable", err.Error())
}
