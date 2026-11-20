package storagetest

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// The Task whose result and artifact the write order cases replace.
const (
	orderNamespace = "tenant"
	orderTask      = "task"
)

// orderTarget is a whole object a write replaces: a result, or one artifact
// file. The write order cases run the same checks on both.
type orderTarget[P any] struct {
	// limit is the largest object the contract accepts.
	limit int
	// saveMethod and deleteMethod name the contract methods, and saveArgs
	// and deleteArgs return their arguments after the context.
	saveMethod, deleteMethod string
	saveArgs                 func(data []byte) []any
	deleteArgs               func() []any
	save                     func(ctx context.Context, p P, data []byte) error
	delete                   func(ctx context.Context, p P) error
	// get returns the stored object, or ErrNotFound.
	get func(ctx context.Context, p P) ([]byte, error)
	// observe returns everything the Task's reads return, so a check can show
	// that nothing changed.
	observe func(t *testing.T, p P) any
}

func resultOrder() orderTarget[storagev1.StateProvider] {
	target := orderTarget[storagev1.StateProvider]{
		limit:        storagev1.MaxResultBytes,
		saveMethod:   "SaveResult",
		deleteMethod: "DeleteResult",
		saveArgs:     func(data []byte) []any { return []any{orderNamespace, orderTask, data} },
		deleteArgs:   func() []any { return []any{orderNamespace, orderTask} },
		save: func(ctx context.Context, p storagev1.StateProvider, data []byte) error {
			return p.SaveResult(ctx, orderNamespace, orderTask, data)
		},
		delete: func(ctx context.Context, p storagev1.StateProvider) error {
			return p.DeleteResult(ctx, orderNamespace, orderTask)
		},
		get: func(ctx context.Context, p storagev1.StateProvider) ([]byte, error) {
			return p.GetResult(ctx, orderNamespace, orderTask)
		},
	}
	target.observe = func(t *testing.T, p storagev1.StateProvider) any {
		t.Helper()
		data, err := target.get(t.Context(), p)
		return observed(t, data, err)
	}
	return target
}

func artifactOrder() orderTarget[storagev1.ArtifactProvider] {
	owner := storagev1.ArtifactOwner{Namespace: orderNamespace, TaskName: orderTask}
	request := func(data []byte) storagev1.SaveArtifactRequest {
		return storagev1.SaveArtifactRequest{Owner: owner, Filename: "report.txt", ContentType: "text/plain", Data: data}
	}
	target := orderTarget[storagev1.ArtifactProvider]{
		limit:        storagev1.MaxArtifactBytes,
		saveMethod:   "SaveArtifact",
		deleteMethod: "DeleteArtifacts",
		saveArgs:     func(data []byte) []any { return []any{request(data)} },
		deleteArgs:   func() []any { return []any{owner} },
		save: func(ctx context.Context, p storagev1.ArtifactProvider, data []byte) error {
			return p.SaveArtifact(ctx, request(data))
		},
		delete: func(ctx context.Context, p storagev1.ArtifactProvider) error {
			return p.DeleteArtifacts(ctx, owner)
		},
		get: func(ctx context.Context, p storagev1.ArtifactProvider) ([]byte, error) {
			data, _, err := p.GetArtifact(ctx, storagev1.ArtifactKey{Owner: owner, Filename: "report.txt"})
			return data, err
		},
	}
	target.observe = func(t *testing.T, p storagev1.ArtifactProvider) any {
		t.Helper()
		listed, err := p.ListArtifacts(t.Context(), owner)
		require.NoError(t, err)
		for i := range listed {
			listed[i].CreatedAt = listed[i].CreatedAt.UTC()
		}
		data, err := target.get(t.Context(), p)
		return []any{listed, observed(t, data, err)}
	}
	return target
}

// orderWriter makes writes of one target with envelopes whose order stamp the
// case chooses.
type orderWriter[P writeCaseProvider] struct {
	target  orderTarget[P]
	p       P
	dataset string
}

func newOrderWriter[P writeCaseProvider](t *testing.T, p P, target orderTarget[P]) orderWriter[P] {
	t.Helper()
	advanceEpoch(t, p, suiteFence)
	return orderWriter[P]{target: target, p: p, dataset: datasetID(t, p)}
}

func (w orderWriter[P]) saveEnvelope(t *testing.T, fence storagev1.ControllerEpochFence, sequence uint64, data []byte) storagev1.WriteEnvelope {
	t.Helper()
	envelope := NewWriteEnvelope(t, w.dataset, fence, w.target.saveMethod, w.target.saveArgs(data)...)
	envelope.Sequence = sequence
	return envelope
}

func (w orderWriter[P]) deleteEnvelope(t *testing.T, fence storagev1.ControllerEpochFence, sequence uint64) storagev1.WriteEnvelope {
	t.Helper()
	envelope := NewWriteEnvelope(t, w.dataset, fence, w.target.deleteMethod, w.target.deleteArgs()...)
	envelope.Sequence = sequence
	return envelope
}

func (w orderWriter[P]) saveWith(t *testing.T, envelope storagev1.WriteEnvelope, data []byte) error {
	t.Helper()
	return w.target.save(storagev1.WithWriteEnvelope(t.Context(), envelope), w.p, data)
}

func (w orderWriter[P]) saveAt(t *testing.T, sequence uint64, data []byte) error {
	t.Helper()
	return w.saveWith(t, w.saveEnvelope(t, suiteFence, sequence, data), data)
}

func (w orderWriter[P]) deleteWith(t *testing.T, envelope storagev1.WriteEnvelope) error {
	t.Helper()
	return w.target.delete(storagev1.WithWriteEnvelope(t.Context(), envelope), w.p)
}

func (w orderWriter[P]) requireSees(t *testing.T, want []byte) {
	t.Helper()
	data, err := w.target.get(t.Context(), w.p)
	require.NoError(t, err)
	require.Equal(t, want, data)
}

func (w orderWriter[P]) requireSeesNothing(t *testing.T) {
	t.Helper()
	_, err := w.target.get(t.Context(), w.p)
	require.ErrorIs(t, err, storagev1.ErrNotFound)
}

// runWriteOrder checks that a whole object is replaced in issue order rather
// than commit order, and that its size limit holds on every attempt.
func runWriteOrder[P writeCaseProvider](t *testing.T, newProvider func(t *testing.T) P, target orderTarget[P]) {
	t.Run("a replayed save leaves one version", func(t *testing.T) {
		w := newOrderWriter(t, newProvider(t), target)
		envelope := w.saveEnvelope(t, suiteFence, 10, []byte("first"))
		require.NoError(t, w.saveWith(t, envelope, []byte("first")))
		after := target.observe(t, w.p)
		require.NoError(t, w.saveWith(t, envelope, []byte("first")), "the response was lost; the caller retries")
		require.Equal(t, after, target.observe(t, w.p), "the replay stored nothing new")
		w.requireSees(t, []byte("first"))
	})

	t.Run("a replayed delete succeeds again", func(t *testing.T) {
		w := newOrderWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 1, []byte("first")))
		envelope := w.deleteEnvelope(t, suiteFence, 2)
		require.NoError(t, w.deleteWith(t, envelope))
		require.NoError(t, w.deleteWith(t, envelope), "the response was lost; the caller retries")
		require.NoError(t, w.deleteWith(t, w.deleteEnvelope(t, suiteFence, 3)), "deleting again succeeds")
		w.requireSeesNothing(t)
	})

	t.Run("a write issued earlier never replaces one issued later", func(t *testing.T) {
		w := newOrderWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 20, []byte("issued second")))
		before := target.observe(t, w.p)
		require.NoError(t, w.saveAt(t, 10, []byte("issued first")), "an older write succeeds")
		require.Equal(t, before, target.observe(t, w.p), "and changes nothing")
		w.requireSees(t, []byte("issued second"))

		require.NoError(t, w.saveAt(t, 21, []byte("issued third")))
		w.requireSees(t, []byte("issued third"))
	})

	t.Run("a new write with the stored stamp never replaces it", func(t *testing.T) {
		w := newOrderWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 10, []byte("stored")))
		before := target.observe(t, w.p)
		require.NoError(t, w.saveAt(t, 10, []byte("same stamp")),
			"another operation with the same epoch and sequence succeeds")
		require.Equal(t, before, target.observe(t, w.p), "and changes nothing")
		w.requireSees(t, []byte("stored"))
	})

	t.Run("a later controller's write is newer than any sequence of an earlier one", func(t *testing.T) {
		w := newOrderWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 1000, []byte("epoch 1")))
		advanceEpoch(t, w.p, nextFence)
		next := w.saveEnvelope(t, nextFence, 1, []byte("epoch 2"))
		require.NoError(t, w.saveWith(t, next, []byte("epoch 2")))
		w.requireSees(t, []byte("epoch 2"))
	})

	t.Run("concurrent writers leave the write issued last", func(t *testing.T) {
		w := newOrderWriter(t, newProvider(t), target)
		const writers = 8
		envelopes := make([]storagev1.WriteEnvelope, writers)
		for i := range writers {
			envelopes[i] = w.saveEnvelope(t, suiteFence, uint64(i+1), fmt.Appendf(nil, "writer %d", i+1))
		}
		start := make(chan struct{})
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				<-start
				errs[i] = w.saveWith(t, envelopes[i], fmt.Appendf(nil, "writer %d", i+1))
			})
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			require.NoError(t, err, "writer %d", i+1)
		}
		w.requireSees(t, fmt.Appendf(nil, "writer %d", writers))
	})

	t.Run("a delete removes the stored stamp", func(t *testing.T) {
		w := newOrderWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 10, []byte("before the delete")))
		require.NoError(t, w.deleteWith(t, w.deleteEnvelope(t, suiteFence, 11)))
		w.requireSeesNothing(t)
		require.NoError(t, w.saveAt(t, 5, []byte("after the delete")))
		w.requireSees(t, []byte("after the delete"))
	})

	t.Run("enforces the size limit, including on a retry", func(t *testing.T) {
		w := newOrderWriter(t, newProvider(t), target)
		largest := bytes.Repeat([]byte{'a'}, target.limit)
		envelope := w.saveEnvelope(t, suiteFence, 1, largest)
		require.NoError(t, w.saveWith(t, envelope, largest))
		require.NoError(t, w.saveWith(t, envelope, largest))
		before := target.observe(t, w.p)

		tooLarge := bytes.Repeat([]byte{'b'}, target.limit+1)
		envelope = w.saveEnvelope(t, suiteFence, 2, tooLarge)
		require.ErrorIs(t, w.saveWith(t, envelope, tooLarge), storagev1.ErrTooLarge)
		require.ErrorIs(t, w.saveWith(t, envelope, tooLarge), storagev1.ErrTooLarge,
			"a refusal leaves no receipt, so a retry is refused again")
		require.Equal(t, before, target.observe(t, w.p))
		data, err := target.get(t.Context(), w.p)
		require.NoError(t, err)
		require.Len(t, data, target.limit)
	})
}

// runResultWriteOrder runs the write order cases on results, and checks that
// prompt result receipts hold the result limit.
func runResultWriteOrder(t *testing.T, newProvider StateFactory) {
	runWriteOrder(t, newProvider, resultOrder())

	t.Run("prompt result receipts fit the result limit", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		receipt := func(attempt string, data []byte) storagev1.PromptResultReceipt {
			return storagev1.PromptResultReceipt{
				AttemptID: attempt, Namespace: orderNamespace, TaskName: orderTask,
				OperationID: "operation-" + attempt, OperationDigest: storagev1.CanonicalBytesDigest([]byte(attempt)),
				Data: data,
			}
		}
		require.NoError(t, provider.SavePromptResultReceipt(ctx, receipt("attempt-1", bytes.Repeat([]byte{'a'}, storagev1.MaxResultBytes))))
		require.ErrorIs(t, provider.SavePromptResultReceipt(ctx, receipt("attempt-2", bytes.Repeat([]byte{'a'}, storagev1.MaxResultBytes+1))),
			storagev1.ErrTooLarge)
		_, err := provider.GetPromptResultReceipt(ctx, "attempt-2")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})
}

// runArtifactWriteOrder runs the write order cases on one artifact file.
func runArtifactWriteOrder(t *testing.T, newProvider ArtifactFactory) {
	runWriteOrder(t, newProvider, artifactOrder())
}
