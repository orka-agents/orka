package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func runImportedWriteOrder[P writeCaseProvider](t *testing.T, newProvider func(t *testing.T) P, target orderTarget[P]) {
	t.Run("an imported object keeps no write stamp", func(t *testing.T) {
		source := newProvider(t)
		sourceWriter := newOrderWriter(t, source, target)
		later := suiteFence
		later.Epoch += 5
		advanceEpoch(t, source, later)
		require.NoError(t, sourceWriter.saveWith(t, sourceWriter.saveEnvelope(t, later, 1000, []byte("from the source")), []byte("from the source")))

		imported := newProvider(t)
		copyDataset(t, exporter(source), exporter(imported))
		writer := newOrderWriter(t, imported, target)
		writer.requireSees(t, []byte("from the source"))

		stale := writer
		stale.dataset = sourceWriter.dataset
		before := target.observe(t, imported)
		err := stale.saveWith(t, stale.saveEnvelope(t, later, 2000, []byte("from a stale writer")), []byte("from a stale writer"))
		require.ErrorIs(t, err, storagev1.ErrDatasetMismatch, "a writer bound to the source cannot write to the imported dataset")
		require.Equal(t, before, target.observe(t, imported))

		require.NoError(t, writer.saveAt(t, 1, []byte("after the import")),
			"the first write under the target's own fence replaces the imported object")
		writer.requireSees(t, []byte("after the import"))

		takeover := suiteFence
		takeover.Epoch++
		advanceEpoch(t, imported, takeover)
		err = writer.saveWith(t, writer.saveEnvelope(t, suiteFence, 2, []byte("superseded")), []byte("superseded"))
		require.ErrorIs(t, err, storagev1.ErrFenced, "a writer whose fence a later takeover superseded cannot replace it")
		writer.requireSees(t, []byte("after the import"))
	})
}
