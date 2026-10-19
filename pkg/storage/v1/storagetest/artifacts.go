package storagetest

import (
	"sort"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func runArtifacts(t *testing.T, newProvider ArtifactFactory) {
	t.Run("bytes and metadata stay together", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		_, _, err := provider.GetArtifact(ctx, "tenant-a", "task", "report.txt")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		listed, err := provider.ListArtifacts(ctx, "tenant-a", "task")
		require.NoError(t, err)
		require.Empty(t, listed)

		require.NoError(t, provider.SaveArtifact(ctx, "tenant-a", "task", "report.txt", "text/plain", []byte("draft")))
		require.NoError(t, provider.SaveArtifact(ctx, "tenant-a", "task", "data.json", "application/json", []byte(`{"a":1}`)))
		require.NoError(t, provider.SaveArtifact(ctx, "tenant-a", "task", "report.txt", "text/markdown", []byte("# final")))

		data, contentType, err := provider.GetArtifact(ctx, "tenant-a", "task", "report.txt")
		require.NoError(t, err)
		require.Equal(t, []byte("# final"), data, "saving the same filename replaces the artifact")
		require.Equal(t, "text/markdown", contentType)

		listed, err = provider.ListArtifacts(ctx, "tenant-a", "task")
		require.NoError(t, err)
		sort.Slice(listed, func(i, j int) bool { return listed[i].Filename < listed[j].Filename })
		require.Len(t, listed, 2)
		require.Equal(t, "data.json", listed[0].Filename)
		require.Equal(t, "application/json", listed[0].ContentType)
		require.EqualValues(t, len(`{"a":1}`), listed[0].Size)
		require.Equal(t, "report.txt", listed[1].Filename)
		require.Equal(t, "text/markdown", listed[1].ContentType)
		require.EqualValues(t, len("# final"), listed[1].Size)
		require.NotEmpty(t, listed[1].CreatedAt)
	})

	t.Run("namespace and Task isolation", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		require.NoError(t, provider.SaveArtifact(ctx, "tenant-a", "task-1", "out.bin", "application/octet-stream", []byte{0, 1, 2}))
		require.NoError(t, provider.SaveArtifact(ctx, "tenant-a", "task-2", "out.bin", "application/octet-stream", []byte{3}))
		require.NoError(t, provider.SaveArtifact(ctx, "tenant-b", "task-1", "out.bin", "application/octet-stream", []byte{4}))

		data, _, err := provider.GetArtifact(ctx, "tenant-a", "task-1", "out.bin")
		require.NoError(t, err)
		require.Equal(t, []byte{0, 1, 2}, data, "binary content is preserved")
		_, _, err = provider.GetArtifact(ctx, "tenant-c", "task-1", "out.bin")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.NoError(t, provider.DeleteArtifacts(ctx, "tenant-a", "task-1"))
		_, _, err = provider.GetArtifact(ctx, "tenant-a", "task-1", "out.bin")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		listed, err := provider.ListArtifacts(ctx, "tenant-a", "task-1")
		require.NoError(t, err)
		require.Empty(t, listed)
		require.NoError(t, provider.DeleteArtifacts(ctx, "tenant-a", "task-1"), "deletion is idempotent")

		data, _, err = provider.GetArtifact(ctx, "tenant-a", "task-2", "out.bin")
		require.NoError(t, err)
		require.Equal(t, []byte{3}, data, "other Tasks keep their artifacts")
		data, _, err = provider.GetArtifact(ctx, "tenant-b", "task-1", "out.bin")
		require.NoError(t, err)
		require.Equal(t, []byte{4}, data, "other namespaces keep their artifacts")
	})
}
