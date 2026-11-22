package storagetest

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

type writeCase[P any] struct {
	method        string
	setup         func(t *testing.T, provider P)
	args          func(fence storagev1.ControllerEpochFence) []any
	explicitFence bool
	observe       func(t *testing.T, provider P) any
	afterCommit   func(t *testing.T, provider P)
}

type writeOutcome struct {
	returned []any
	args     []any
	err      error
}

var (
	stateWriteCases    []writeCase[storagev1.StateProvider]
	artifactWriteCases []writeCase[storagev1.ArtifactProvider]
)

var deferredStateWrites = map[string]string{}

type writeCaseProvider interface {
	storagev1.ProviderDescriber
	storagev1.ControllerEpochAdvancer
	storagev1.OperationReceiptStore
	storagev1.DatasetBindingStore
}

const writeCallTimeout = 30 * time.Second

var (
	suiteFence    = controlFence(1, "controller-a")
	otherHolder   = controlFence(1, "controller-b")
	nextFence     = controlFence(2, "controller-b")
	suiteInstance = "installation-a"
)

func advanceEpoch(t *testing.T, provider writeCaseProvider, fence storagev1.ControllerEpochFence) storagev1.ControllerEpochMirrorState {
	t.Helper()
	epoch := mirroredEpoch(fence.Epoch, fence.HolderID)
	state, err := provider.AdvanceControllerEpoch(t.Context(), storagev1.AdvanceControllerEpochRequest{
		DatasetID: datasetID(t, provider), Epoch: epoch, InstallationID: suiteInstance,
	})
	require.NoError(t, err)
	return state
}

func datasetID(t *testing.T, provider storagev1.ProviderDescriber) string {
	t.Helper()
	info, err := provider.Describe(t.Context())
	require.NoError(t, err)
	return info.DatasetID
}

func envelopeFor[P any](t *testing.T, dataset string, c writeCase[P], fence storagev1.ControllerEpochFence) storagev1.WriteEnvelope {
	t.Helper()
	return NewWriteEnvelope(t, dataset, fence, c.method, c.args(fence)...)
}

func callWrite[P any](
	ctx context.Context, t *testing.T, provider P, c writeCase[P], envelope storagev1.WriteEnvelope,
	argsFence storagev1.ControllerEpochFence,
) writeOutcome {
	t.Helper()
	return callWriteContext(storagev1.WithWriteEnvelope(ctx, envelope), t, provider, c, argsFence)
}

func callWriteContext[P any](
	ctx context.Context, t *testing.T, provider P, c writeCase[P], argsFence storagev1.ControllerEpochFence,
) writeOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, writeCallTimeout)
	defer cancel()
	method := reflect.ValueOf(provider).MethodByName(c.method)
	require.Truef(t, method.IsValid(), "provider has no method %s", c.method)
	args := c.args(argsFence)
	methodType := method.Type()
	require.Equalf(t, methodType.NumIn(), len(args)+1, "%s takes %d arguments after its context", c.method, methodType.NumIn()-1)
	in := []reflect.Value{reflect.ValueOf(ctx)}
	for i, arg := range args {
		if arg == nil {
			in = append(in, reflect.Zero(methodType.In(i+1)))
			continue
		}
		in = append(in, reflect.ValueOf(arg))
	}
	out := method.Call(in)
	outcome := writeOutcome{args: args}
	for _, value := range out[:len(out)-1] {
		outcome.returned = append(outcome.returned, value.Interface())
	}
	if errValue := out[len(out)-1]; !errValue.IsNil() {
		outcome.err = errValue.Interface().(error)
	}
	return outcome
}

type testingT interface {
	require.TestingT
	Helper()
}

func requireSameOutcome(t testingT, want, got writeOutcome) {
	t.Helper()
	require.NoError(t, got.err)
	wantDigest, err := storagev1.RequestDigest("outcome", append(slices.Clone(want.returned), want.args...)...)
	require.NoError(t, err)
	gotDigest, err := storagev1.RequestDigest("outcome", append(slices.Clone(got.returned), got.args...)...)
	require.NoError(t, err)
	require.Equalf(t, wantDigest, gotDigest, "replay returned %#v, wrote %#v; original returned %#v, wrote %#v",
		got.returned, got.args, want.returned, want.args)
}

func runWriteCatalog[P writeCaseProvider](t *testing.T, newProvider func(t *testing.T) P, cases []writeCase[P]) {
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			t.Run("refusals", func(t *testing.T) { runWriteRefusals(t, newProvider(t), c) })
			t.Run("receipts", func(t *testing.T) { runWriteReceipts(t, newProvider(t), c) })
			t.Run("aborted", func(t *testing.T) { runWriteAborted(t, newProvider(t), c) })
			t.Run("detached", func(t *testing.T) { runWriteDetached(t, newProvider(t), c) })
		})
	}
}

func runMissingEnvelopes[P writeCaseProvider](
	t *testing.T, newProvider func(t *testing.T) P, wrap func(P) P, cases []writeCase[P],
) {
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			raw := newProvider(t)
			wrapped := wrap(raw)
			_, baseline := prepareWriteCase(t, wrapped, c)
			outcome := callWriteContext(t.Context(), t, raw, c, suiteFence)
			require.ErrorIs(t, outcome.err, storagev1.ErrValidation)
			require.Equal(t, baseline, c.observe(t, wrapped), "a write without an envelope changed state")
		})
	}
}

func prepareWriteCase[P writeCaseProvider](t *testing.T, provider P, c writeCase[P]) (string, any) {
	t.Helper()
	advanceEpoch(t, provider, suiteFence)
	if c.setup != nil {
		c.setup(t, provider)
	}
	return datasetID(t, provider), c.observe(t, provider)
}

func runWriteRefusals[P writeCaseProvider](t *testing.T, provider P, c writeCase[P]) {
	dataset, baseline := prepareWriteCase(t, provider, c)
	ctx := t.Context()
	refusals := []struct {
		name      string
		envelope  func() storagev1.WriteEnvelope
		argsFence storagev1.ControllerEpochFence
		want      error
	}{
		{"wrong dataset", func() storagev1.WriteEnvelope {
			e := envelopeFor(t, dataset, c, suiteFence)
			e.DatasetID = "00000000-0000-4000-8000-000000000000"
			return e
		}, suiteFence, storagev1.ErrDatasetMismatch},
		{"other holder", func() storagev1.WriteEnvelope { return envelopeFor(t, dataset, c, otherHolder) }, otherHolder, storagev1.ErrFenced},
		{"future epoch", func() storagev1.WriteEnvelope { return envelopeFor(t, dataset, c, nextFence) }, nextFence, storagev1.ErrFenced},
		{"expired", func() storagev1.WriteEnvelope {
			e := envelopeFor(t, dataset, c, suiteFence)
			e.IssuedAt = time.Now().UTC().Add(-storagev1.MaxOperationAge - time.Minute)
			return e
		}, suiteFence, storagev1.ErrOperationExpired},
		{"clock skew", func() storagev1.WriteEnvelope {
			e := envelopeFor(t, dataset, c, suiteFence)
			e.IssuedAt = time.Now().UTC().Add(storagev1.MaxClockSkew + time.Minute)
			return e
		}, suiteFence, storagev1.ErrClockSkew},
	}
	if c.explicitFence {
		refusals = append(refusals, struct {
			name      string
			envelope  func() storagev1.WriteEnvelope
			argsFence storagev1.ControllerEpochFence
			want      error
		}{"explicit fence differs", func() storagev1.WriteEnvelope { return envelopeFor(t, dataset, c, suiteFence) }, otherHolder, storagev1.ErrValidation})
	}
	for _, refusal := range refusals {
		outcome := callWrite(ctx, t, provider, c, refusal.envelope(), refusal.argsFence)
		require.ErrorIsf(t, outcome.err, refusal.want, "%s: %v", refusal.name, outcome.err)
		require.Equalf(t, baseline, c.observe(t, provider), "%s changed state", refusal.name)
	}
	stale := envelopeFor(t, dataset, c, suiteFence)
	advanceEpoch(t, provider, nextFence)
	outcome := callWrite(ctx, t, provider, c, stale, suiteFence)
	requireFenced(t, outcome.err)
	require.Equal(t, baseline, c.observe(t, provider), "a write under a replaced controller's fence changed state")
}

func runWriteReceipts[P writeCaseProvider](t *testing.T, provider P, c writeCase[P]) {
	dataset, _ := prepareWriteCase(t, provider, c)
	ctx := t.Context()
	envelope := envelopeFor(t, dataset, c, suiteFence)
	first := callWrite(ctx, t, provider, c, envelope, suiteFence)
	require.NoError(t, first.err)
	if c.afterCommit != nil {
		c.afterCommit(t, provider)
	}
	after := c.observe(t, provider)

	requireSameOutcome(t, first, callWrite(ctx, t, provider, c, envelope, suiteFence))
	require.Equal(t, after, c.observe(t, provider), "a replay changed state")

	other := envelope
	other.RequestDigest = controlDigest("a different request")
	outcome := callWrite(ctx, t, provider, c, other, suiteFence)
	require.ErrorIs(t, outcome.err, storagev1.ErrDuplicateMismatch)
	require.Equal(t, after, c.observe(t, provider))

	resolved, err := provider.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: envelope.OperationID, Method: c.method,
		RequestDigest: envelope.RequestDigest, IssuedAt: envelope.IssuedAt,
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationCommitted, resolved.State)
	require.Equal(t, envelope.Fence, resolved.Fence)
	require.Equal(t, envelope.RequestDigest, resolved.RequestDigest)
	require.Equal(t, c.method, resolved.Method)
	require.True(t, resolved.IssuedAt.Equal(envelope.IssuedAt))

	advanceEpoch(t, provider, nextFence)
	requireSameOutcome(t, first, callWrite(ctx, t, provider, c, envelope, suiteFence))
	require.Equal(t, after, c.observe(t, provider), "a replay under a replaced controller's fence changed state")
}

func runWriteAborted[P writeCaseProvider](t *testing.T, provider P, c writeCase[P]) {
	dataset, baseline := prepareWriteCase(t, provider, c)
	ctx := t.Context()
	envelope := envelopeFor(t, dataset, c, suiteFence)
	request := storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: envelope.OperationID, Method: c.method,
		RequestDigest: envelope.RequestDigest, IssuedAt: envelope.IssuedAt,
	}
	resolved, err := provider.ResolveOperation(ctx, request)
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationAborted, resolved.State)
	require.Zero(t, resolved.Fence)
	outcome := callWrite(ctx, t, provider, c, envelope, suiteFence)
	require.ErrorIs(t, outcome.err, storagev1.ErrOperationAborted)
	require.Equal(t, baseline, c.observe(t, provider))
	again, err := provider.ResolveOperation(ctx, request)
	require.NoError(t, err)
	require.Equal(t, resolved, again)
}

func runWriteDetached[P writeCaseProvider](t *testing.T, provider P, c writeCase[P]) {
	dataset, baseline := prepareWriteCase(t, provider, c)
	ctx := t.Context()
	detach := func(generation int64) {
		t.Helper()
		_, err := provider.DetachDatasetBinding(ctx, storagev1.DetachDatasetRequest{
			DatasetID: dataset, InstallationID: suiteInstance, RecordUID: "suite-record", SwitchGeneration: generation,
		})
		require.NoError(t, err)
	}
	bind := func(generation int64) {
		t.Helper()
		_, err := provider.BindDataset(ctx, storagev1.BindDatasetRequest{
			DatasetID: dataset, InstallationID: suiteInstance, RecordUID: "suite-record", SwitchGeneration: generation,
			Mode: storagev1.BindRebind,
		})
		require.NoError(t, err)
	}
	detach(1)
	envelope := envelopeFor(t, dataset, c, suiteFence)
	refused := callWrite(ctx, t, provider, c, envelope, suiteFence)
	require.ErrorIs(t, refused.err, storagev1.ErrDatasetDetached)
	require.ErrorIs(t, refused.err, storagev1.ErrNotReady)

	bind(2)
	require.Equal(t, baseline, c.observe(t, provider), "a write to a detached dataset changed state")
	committed := callWrite(ctx, t, provider, c, envelope, suiteFence)
	require.NoError(t, committed.err, "the refused operation commits once the dataset is bound again")
	if c.afterCommit != nil {
		c.afterCommit(t, provider)
	}
	after := c.observe(t, provider)

	detach(3)
	requireSameOutcome(t, committed, callWrite(ctx, t, provider, c, envelope, suiteFence))
	bind(4)
	require.Equal(t, after, c.observe(t, provider), "a replay on a detached dataset changed state")
}

func runConcurrentReplays[P writeCaseProvider](t *testing.T, provider P, c writeCase[P], callers int) []writeOutcome {
	t.Helper()
	dataset := datasetID(t, provider)
	envelope := envelopeFor(t, dataset, c, suiteFence)
	outcomes := make([]writeOutcome, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() { outcomes[i] = callWrite(t.Context(), t, provider, c, envelope, suiteFence) })
	}
	wg.Wait()
	for _, outcome := range outcomes {
		requireSameOutcome(t, outcomes[0], outcome)
	}
	return outcomes
}

func uncoveredWrites[P any](contract string, cases []writeCase[P]) ([]string, error) {
	covered := map[string]bool{}
	for _, c := range cases {
		info, ok := storagev1.LookupMethod(c.method)
		switch {
		case !ok:
			return nil, fmt.Errorf("write case %q names no contract method", c.method)
		case info.Kind != storagev1.MethodWrite || info.Contract != contract:
			return nil, fmt.Errorf("write case %q is not a %s write", c.method, contract)
		case covered[c.method]:
			return nil, fmt.Errorf("write case %q is listed twice", c.method)
		}
		covered[c.method] = true
	}
	var uncovered []string
	for name, info := range storagev1.Methods() {
		if info.Kind == storagev1.MethodWrite && info.Contract == contract && !covered[name] {
			uncovered = append(uncovered, name)
		}
	}
	slices.Sort(uncovered)
	return uncovered, nil
}
