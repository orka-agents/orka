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

// The Task the identity cases address. Two UIDs share its name, as a Task
// and a later Task created with the same name do.
const (
	identityNamespace = "tenant"
	identityTask      = "task"
	identityUID       = "task-uid-1"
	identityLaterUID  = "task-uid-2"
)

// taskResultKey returns the result key of the suite Task named "task" in
// namespace.
func taskResultKey(namespace string) storagev1.ResultKey {
	return storagev1.ResultKey{Namespace: namespace, TaskName: "task", TaskUID: "task-uid"}
}

// identityTarget is a whole object bound to a Task UID: a result, or one
// artifact file. The identity cases run the same checks on both.
type identityTarget[P any] struct {
	// limit is the largest object the contract accepts.
	limit int
	// saveMethod and deleteMethod name the contract methods, and saveArgs
	// and deleteArgs return their arguments after the context.
	saveMethod, deleteMethod string
	saveArgs                 func(uid string, data []byte) []any
	deleteArgs               func(uid string) []any
	save                     func(ctx context.Context, p P, uid string, data []byte) error
	delete                   func(ctx context.Context, p P, uid string) error
	// get returns the object the UID sees, or ErrNotFound.
	get func(ctx context.Context, p P, uid string) ([]byte, error)
	// observe returns everything either UID sees, so a check can show that
	// nothing changed.
	observe func(t *testing.T, p P) any
}

func resultIdentity() identityTarget[storagev1.StateProvider] {
	key := func(uid string) storagev1.ResultKey {
		return storagev1.ResultKey{Namespace: identityNamespace, TaskName: identityTask, TaskUID: uid}
	}
	target := identityTarget[storagev1.StateProvider]{
		limit:        storagev1.MaxResultBytes,
		saveMethod:   "SaveResult",
		deleteMethod: "DeleteResult",
		saveArgs:     func(uid string, data []byte) []any { return []any{key(uid), data} },
		deleteArgs:   func(uid string) []any { return []any{key(uid)} },
		save: func(ctx context.Context, p storagev1.StateProvider, uid string, data []byte) error {
			return p.SaveResult(ctx, key(uid), data)
		},
		delete: func(ctx context.Context, p storagev1.StateProvider, uid string) error {
			return p.DeleteResult(ctx, key(uid))
		},
		get: func(ctx context.Context, p storagev1.StateProvider, uid string) ([]byte, error) {
			return p.GetResult(ctx, key(uid))
		},
	}
	target.observe = func(t *testing.T, p storagev1.StateProvider) any {
		t.Helper()
		seen := make([]any, 0, 2)
		for _, uid := range []string{identityUID, identityLaterUID} {
			data, err := p.GetResult(t.Context(), key(uid))
			seen = append(seen, observed(t, data, err))
		}
		return seen
	}
	return target
}

func artifactIdentity() identityTarget[storagev1.ArtifactProvider] {
	owner := func(uid string) storagev1.ArtifactOwner {
		return storagev1.ArtifactOwner{Namespace: identityNamespace, TaskName: identityTask, TaskUID: uid}
	}
	request := func(uid string, data []byte) storagev1.SaveArtifactRequest {
		return storagev1.SaveArtifactRequest{Owner: owner(uid), Filename: "report.txt", ContentType: "text/plain", Data: data}
	}
	target := identityTarget[storagev1.ArtifactProvider]{
		limit:        storagev1.MaxArtifactBytes,
		saveMethod:   "SaveArtifact",
		deleteMethod: "DeleteArtifacts",
		saveArgs:     func(uid string, data []byte) []any { return []any{request(uid, data)} },
		deleteArgs:   func(uid string) []any { return []any{owner(uid)} },
		save: func(ctx context.Context, p storagev1.ArtifactProvider, uid string, data []byte) error {
			return p.SaveArtifact(ctx, request(uid, data))
		},
		delete: func(ctx context.Context, p storagev1.ArtifactProvider, uid string) error {
			return p.DeleteArtifacts(ctx, owner(uid))
		},
		get: func(ctx context.Context, p storagev1.ArtifactProvider, uid string) ([]byte, error) {
			data, _, err := p.GetArtifact(ctx, storagev1.ArtifactKey{Owner: owner(uid), Filename: "report.txt"})
			return data, err
		},
	}
	target.observe = func(t *testing.T, p storagev1.ArtifactProvider) any {
		t.Helper()
		seen := make([]any, 0, 4)
		for _, uid := range []string{identityUID, identityLaterUID} {
			listed, err := p.ListArtifacts(t.Context(), owner(uid))
			require.NoError(t, err)
			for i := range listed {
				listed[i].CreatedAt = listed[i].CreatedAt.UTC()
			}
			data, err := target.get(t.Context(), p, uid)
			seen = append(seen, listed, observed(t, data, err))
		}
		return seen
	}
	return target
}

// identityWriter makes writes of one target with envelopes whose order stamp
// the case chooses.
type identityWriter[P writeCaseProvider] struct {
	target  identityTarget[P]
	p       P
	dataset string
}

func newIdentityWriter[P writeCaseProvider](t *testing.T, p P, target identityTarget[P]) identityWriter[P] {
	t.Helper()
	advanceEpoch(t, p, suiteFence)
	return identityWriter[P]{target: target, p: p, dataset: datasetID(t, p)}
}

func (w identityWriter[P]) saveEnvelope(t *testing.T, fence storagev1.ControllerEpochFence, sequence uint64, uid string, data []byte) storagev1.WriteEnvelope {
	t.Helper()
	envelope := NewWriteEnvelope(t, w.dataset, fence, w.target.saveMethod, w.target.saveArgs(uid, data)...)
	envelope.Sequence = sequence
	return envelope
}

func (w identityWriter[P]) deleteEnvelope(t *testing.T, fence storagev1.ControllerEpochFence, sequence uint64, uid string) storagev1.WriteEnvelope {
	t.Helper()
	envelope := NewWriteEnvelope(t, w.dataset, fence, w.target.deleteMethod, w.target.deleteArgs(uid)...)
	envelope.Sequence = sequence
	return envelope
}

func (w identityWriter[P]) saveWith(t *testing.T, envelope storagev1.WriteEnvelope, uid string, data []byte) error {
	t.Helper()
	return w.target.save(storagev1.WithWriteEnvelope(t.Context(), envelope), w.p, uid, data)
}

func (w identityWriter[P]) saveAt(t *testing.T, sequence uint64, uid string, data []byte) error {
	t.Helper()
	return w.saveWith(t, w.saveEnvelope(t, suiteFence, sequence, uid, data), uid, data)
}

func (w identityWriter[P]) deleteWith(t *testing.T, envelope storagev1.WriteEnvelope, uid string) error {
	t.Helper()
	return w.target.delete(storagev1.WithWriteEnvelope(t.Context(), envelope), w.p, uid)
}

func (w identityWriter[P]) requireSees(t *testing.T, uid string, want []byte) {
	t.Helper()
	data, err := w.target.get(t.Context(), w.p, uid)
	require.NoError(t, err)
	require.Equal(t, want, data)
}

func (w identityWriter[P]) requireSeesNothing(t *testing.T, uid string) {
	t.Helper()
	_, err := w.target.get(t.Context(), w.p, uid)
	require.ErrorIs(t, err, storagev1.ErrNotFound)
}

// runIdentity checks that a whole object is bound to its Task UID, replaced
// in issue order rather than commit order, and kept from a deleted Task.
func runIdentity[P writeCaseProvider](t *testing.T, newProvider func(t *testing.T) P, target identityTarget[P]) {
	t.Run("a replayed save leaves one version", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		envelope := w.saveEnvelope(t, suiteFence, 10, identityUID, []byte("first"))
		require.NoError(t, w.saveWith(t, envelope, identityUID, []byte("first")))
		after := target.observe(t, w.p)
		require.NoError(t, w.saveWith(t, envelope, identityUID, []byte("first")), "the response was lost; the caller retries")
		require.Equal(t, after, target.observe(t, w.p), "the replay stored nothing new")
		w.requireSees(t, identityUID, []byte("first"))
	})

	t.Run("a replayed delete succeeds again", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 1, identityUID, []byte("first")))
		envelope := w.deleteEnvelope(t, suiteFence, 2, identityUID)
		require.NoError(t, w.deleteWith(t, envelope, identityUID))
		require.NoError(t, w.deleteWith(t, envelope, identityUID), "the response was lost; the caller retries")
		require.NoError(t, w.deleteWith(t, w.deleteEnvelope(t, suiteFence, 3, identityUID), identityUID), "deleting again succeeds")
		w.requireSeesNothing(t, identityUID)
	})

	t.Run("a write issued earlier never replaces one issued later", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 20, identityUID, []byte("issued second")))
		require.NoError(t, w.saveAt(t, 10, identityUID, []byte("issued first")), "an older write succeeds")
		w.requireSees(t, identityUID, []byte("issued second"))

		require.NoError(t, w.saveAt(t, 21, identityUID, []byte("issued third")))
		w.requireSees(t, identityUID, []byte("issued third"))
	})

	t.Run("a later controller's write is newer than any sequence of an earlier one", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 1000, identityUID, []byte("epoch 1")))
		advanceEpoch(t, w.p, nextFence)
		next := w.saveEnvelope(t, nextFence, 1, identityUID, []byte("epoch 2"))
		require.NoError(t, w.saveWith(t, next, identityUID, []byte("epoch 2")))
		w.requireSees(t, identityUID, []byte("epoch 2"))
	})

	t.Run("concurrent writers leave the write issued last", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		const writers = 8
		envelopes := make([]storagev1.WriteEnvelope, writers)
		for i := range writers {
			envelopes[i] = w.saveEnvelope(t, suiteFence, uint64(i+1), identityUID, fmt.Appendf(nil, "writer %d", i+1))
		}
		start := make(chan struct{})
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				<-start
				errs[i] = w.saveWith(t, envelopes[i], identityUID, fmt.Appendf(nil, "writer %d", i+1))
			})
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			require.NoError(t, err, "writer %d", i+1)
		}
		w.requireSees(t, identityUID, fmt.Appendf(nil, "writer %d", writers))
	})

	t.Run("a deleted Task keeps nothing", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		committed := w.saveEnvelope(t, suiteFence, 1, identityUID, []byte("saved"))
		require.NoError(t, w.saveWith(t, committed, identityUID, []byte("saved")))
		delayed := w.saveEnvelope(t, suiteFence, 2, identityUID, []byte("delayed"))
		require.NoError(t, w.deleteWith(t, w.deleteEnvelope(t, suiteFence, 3, identityUID), identityUID))
		w.requireSeesNothing(t, identityUID)
		deleted := target.observe(t, w.p)

		err := w.saveWith(t, delayed, identityUID, []byte("delayed"))
		requireFenced(t, err)
		require.Equal(t, deleted, target.observe(t, w.p), "a save issued before the delete changes nothing when it arrives after it")
		requireFenced(t, w.saveAt(t, 4, identityUID, []byte("issued after")))
		require.NoError(t, w.saveWith(t, committed, identityUID, []byte("saved")),
			"a replay of a save committed before the delete returns its outcome")
		require.Equal(t, deleted, target.observe(t, w.p), "and changes nothing")
	})

	t.Run("a reused Task name sees only its own UID", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 2, identityUID, []byte("earlier Task")))
		w.requireSeesNothing(t, identityLaterUID)
		require.NoError(t, w.saveAt(t, 1, identityLaterUID, []byte("later Task")), "UIDs are ordered separately")
		w.requireSees(t, identityUID, []byte("earlier Task"))
		w.requireSees(t, identityLaterUID, []byte("later Task"))
	})

	t.Run("a delayed delete of an earlier Task leaves a later Task's object", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		require.NoError(t, w.saveAt(t, 1, identityUID, []byte("earlier Task")))
		delayed := w.deleteEnvelope(t, suiteFence, 2, identityUID)
		require.NoError(t, w.saveAt(t, 3, identityLaterUID, []byte("later Task")))
		require.NoError(t, w.deleteWith(t, delayed, identityUID))
		w.requireSeesNothing(t, identityUID)
		w.requireSees(t, identityLaterUID, []byte("later Task"))
	})

	t.Run("enforces the size limit, including on a retry", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		largest := bytes.Repeat([]byte{'a'}, target.limit)
		envelope := w.saveEnvelope(t, suiteFence, 1, identityUID, largest)
		require.NoError(t, w.saveWith(t, envelope, identityUID, largest))
		require.NoError(t, w.saveWith(t, envelope, identityUID, largest))
		before := target.observe(t, w.p)

		tooLarge := bytes.Repeat([]byte{'b'}, target.limit+1)
		envelope = w.saveEnvelope(t, suiteFence, 2, identityUID, tooLarge)
		require.ErrorIs(t, w.saveWith(t, envelope, identityUID, tooLarge), storagev1.ErrTooLarge)
		require.ErrorIs(t, w.saveWith(t, envelope, identityUID, tooLarge), storagev1.ErrTooLarge,
			"a refusal leaves no receipt, so a retry is refused again")
		require.Equal(t, before, target.observe(t, w.p))
		data, err := target.get(t.Context(), w.p, identityUID)
		require.NoError(t, err)
		require.Len(t, data, target.limit)
	})

	t.Run("requires the Task UID", func(t *testing.T) {
		w := newIdentityWriter(t, newProvider(t), target)
		require.ErrorIs(t, target.save(t.Context(), w.p, "", []byte("data")), storagev1.ErrValidation)
		require.ErrorIs(t, target.delete(t.Context(), w.p, ""), storagev1.ErrValidation)
		_, err := target.get(t.Context(), w.p, "")
		require.ErrorIs(t, err, storagev1.ErrValidation)
		w.requireSeesNothing(t, identityUID)
	})
}

// runResultIdentity runs the identity cases on results, and the rules that
// bind prompt result receipts to the same Task UID.
func runResultIdentity(t *testing.T, newProvider StateFactory) {
	runIdentity(t, newProvider, resultIdentity())

	receipt := func(attempt, uid string) storagev1.PromptResultReceipt {
		return storagev1.PromptResultReceipt{
			AttemptID: attempt, Namespace: identityNamespace, TaskName: identityTask, TaskUID: uid,
			OperationID: "operation-" + attempt, OperationDigest: storagev1.CanonicalBytesDigest([]byte(attempt)),
			Data: []byte("result of " + attempt),
		}
	}
	key := func(uid string) storagev1.ResultKey {
		return storagev1.ResultKey{Namespace: identityNamespace, TaskName: identityTask, TaskUID: uid}
	}

	t.Run("prompt result receipts belong to their Task UID", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		require.NoError(t, provider.SavePromptResultReceipt(ctx, receipt("attempt-1", identityUID)))
		require.NoError(t, provider.SavePromptResultReceipt(ctx, receipt("attempt-2", identityLaterUID)))
		require.NoError(t, provider.DeleteResult(ctx, key(identityUID)))
		_, err := provider.GetPromptResultReceipt(ctx, "attempt-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		got, err := provider.GetPromptResultReceipt(ctx, "attempt-2")
		require.NoError(t, err, "deleting one Task's result keeps a later Task's receipts")
		require.Equal(t, receipt("attempt-2", identityLaterUID), *got)

		requireFenced(t, provider.SavePromptResultReceipt(ctx, receipt("attempt-3", identityUID)))
		_, err = provider.GetPromptResultReceipt(ctx, "attempt-3")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a deleted Task's receipt is refused")
	})

	t.Run("prompt result receipts need a Task UID and fit the result limit", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		require.ErrorIs(t, provider.SavePromptResultReceipt(ctx, receipt("attempt-1", "")), storagev1.ErrValidation)
		largest := receipt("attempt-2", identityUID)
		largest.Data = bytes.Repeat([]byte{'a'}, storagev1.MaxResultBytes)
		require.NoError(t, provider.SavePromptResultReceipt(ctx, largest))
		tooLarge := receipt("attempt-3", identityUID)
		tooLarge.Data = bytes.Repeat([]byte{'a'}, storagev1.MaxResultBytes+1)
		require.ErrorIs(t, provider.SavePromptResultReceipt(ctx, tooLarge), storagev1.ErrTooLarge)
		_, err := provider.GetPromptResultReceipt(ctx, "attempt-3")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})
}

// runArtifactIdentity runs the identity cases on one artifact file, and the
// rules for the files of a Task UID as a whole.
func runArtifactIdentity(t *testing.T, newProvider ArtifactFactory) {
	runIdentity(t, newProvider, artifactIdentity())

	owner := func(uid string) storagev1.ArtifactOwner {
		return storagev1.ArtifactOwner{Namespace: identityNamespace, TaskName: identityTask, TaskUID: uid}
	}
	t.Run("a deleted Task lists nothing and a reused name lists only its own files", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		for _, uid := range []string{identityUID, identityLaterUID} {
			for _, filename := range []string{"shared.txt", uid + ".txt"} {
				saveArtifact(t, provider, owner(uid), filename, "text/plain", []byte(uid+"/"+filename))
			}
		}
		listed, err := provider.ListArtifacts(ctx, owner(identityUID))
		require.NoError(t, err)
		require.Equal(t, []string{"shared.txt", identityUID + ".txt"}, artifactNames(listed))
		data, _, err := provider.GetArtifact(ctx, storagev1.ArtifactKey{Owner: owner(identityLaterUID), Filename: "shared.txt"})
		require.NoError(t, err)
		require.Equal(t, identityLaterUID+"/shared.txt", string(data))

		require.NoError(t, provider.DeleteArtifacts(ctx, owner(identityUID)))
		listed, err = provider.ListArtifacts(ctx, owner(identityUID))
		require.NoError(t, err)
		require.Empty(t, listed)
		listed, err = provider.ListArtifacts(ctx, owner(identityLaterUID))
		require.NoError(t, err)
		require.Equal(t, []string{"shared.txt", identityLaterUID + ".txt"}, artifactNames(listed))
	})

	t.Run("listing requires the Task UID", func(t *testing.T) {
		provider := newProvider(t)
		_, err := provider.ListArtifacts(t.Context(), owner(""))
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = provider.ListArtifacts(t.Context(), storagev1.ArtifactOwner{TaskName: identityTask, TaskUID: identityUID})
		require.ErrorIs(t, err, storagev1.ErrValidation, "and the namespace")
	})
}

func artifactNames(listed []storagev1.ArtifactMetadata) []string {
	names := make([]string, 0, len(listed))
	for _, artifact := range listed {
		names = append(names, artifact.Filename)
	}
	return names
}
