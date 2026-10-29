package storagetest

import (
	"bytes"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func artifactOwner(namespace, taskName string) storagev1.ArtifactOwner {
	return storagev1.ArtifactOwner{Namespace: namespace, TaskName: taskName}
}

func artifactKey(namespace, taskName, filename string) storagev1.ArtifactKey {
	return storagev1.ArtifactKey{Owner: artifactOwner(namespace, taskName), Filename: filename}
}

func saveArtifact(t *testing.T, provider storagev1.ArtifactProvider, owner storagev1.ArtifactOwner, filename, contentType string, data []byte) {
	t.Helper()
	require.NoError(t, provider.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{
		Owner: owner, Filename: filename, ContentType: contentType, Data: data,
	}))
}

func runArtifacts(t *testing.T, newProvider ArtifactFactory) {
	t.Run("bytes and metadata stay together", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		owner := artifactOwner("tenant-a", "task")
		_, _, err := provider.GetArtifact(ctx, artifactKey("tenant-a", "task", "report.txt"))
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		listed, err := provider.ListArtifacts(ctx, owner)
		require.NoError(t, err)
		require.Empty(t, listed)

		saveArtifact(t, provider, owner, "report.txt", "text/plain", []byte("draft"))
		saveArtifact(t, provider, owner, "data.json", "application/json", []byte(`{"a":1}`))
		saveArtifact(t, provider, owner, "report.txt", "text/markdown", []byte("# final"))

		data, contentType, err := provider.GetArtifact(ctx, artifactKey("tenant-a", "task", "report.txt"))
		require.NoError(t, err)
		require.Equal(t, []byte("# final"), data, "saving the same filename replaces the artifact")
		require.Equal(t, "text/markdown", contentType)

		listed, err = provider.ListArtifacts(ctx, owner)
		require.NoError(t, err)
		require.Len(t, listed, 2)
		require.Equal(t, "data.json", listed[0].Filename)
		require.Equal(t, "application/json", listed[0].ContentType)
		require.EqualValues(t, len(`{"a":1}`), listed[0].Size)
		require.Equal(t, "report.txt", listed[1].Filename)
		require.Equal(t, "text/markdown", listed[1].ContentType)
		require.EqualValues(t, len("# final"), listed[1].Size)
		for _, artifact := range listed {
			require.False(t, artifact.CreatedAt.IsZero(), "the provider assigns a creation time")
			require.Equal(t, time.UTC, artifact.CreatedAt.Location(), "creation times are UTC")
		}
	})

	t.Run("lists filenames in byte order", func(t *testing.T) {
		provider := newProvider(t)
		owner := artifactOwner("tenant-a", "task")
		for _, filename := range []string{"b.txt", "a.txt", "_notes", "B.txt", "Z.log", "a.txt.1"} {
			saveArtifact(t, provider, owner, filename, "text/plain", []byte(filename))
		}
		listed, err := provider.ListArtifacts(t.Context(), owner)
		require.NoError(t, err)
		filenames := make([]string, 0, len(listed))
		for _, artifact := range listed {
			filenames = append(filenames, artifact.Filename)
		}
		require.Equal(t, []string{"B.txt", "Z.log", "_notes", "a.txt", "a.txt.1", "b.txt"}, filenames)
	})

	t.Run("enforces the artifact size limit", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		owner := artifactOwner("tenant-a", "task")
		largest := bytes.Repeat([]byte{'a'}, storagev1.MaxArtifactBytes)
		saveArtifact(t, provider, owner, "largest.bin", "application/octet-stream", largest)
		saveArtifact(t, provider, owner, "kept.bin", "application/octet-stream", []byte("kept"))

		err := provider.SaveArtifact(ctx, storagev1.SaveArtifactRequest{
			Owner: owner, Filename: "kept.bin", ContentType: "application/octet-stream",
			Data: bytes.Repeat([]byte{'b'}, storagev1.MaxArtifactBytes+1),
		})
		require.ErrorIs(t, err, storagev1.ErrTooLarge)

		data, _, err := provider.GetArtifact(ctx, artifactKey("tenant-a", "task", "largest.bin"))
		require.NoError(t, err)
		require.Len(t, data, storagev1.MaxArtifactBytes)
		data, _, err = provider.GetArtifact(ctx, artifactKey("tenant-a", "task", "kept.bin"))
		require.NoError(t, err)
		require.Equal(t, []byte("kept"), data, "a rejected write leaves the existing artifact")
	})

	t.Run("accepts the owning Task UID", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		owner := storagev1.ArtifactOwner{Namespace: "tenant-a", TaskName: "task", TaskUID: "task-uid-1"}
		saveArtifact(t, provider, owner, "out.txt", "text/plain", []byte("v1"))
		data, _, err := provider.GetArtifact(ctx, storagev1.ArtifactKey{Owner: owner, Filename: "out.txt"})
		require.NoError(t, err)
		require.Equal(t, []byte("v1"), data)
		listed, err := provider.ListArtifacts(ctx, owner)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.NoError(t, provider.DeleteArtifacts(ctx, owner))
		_, _, err = provider.GetArtifact(ctx, storagev1.ArtifactKey{Owner: owner, Filename: "out.txt"})
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("namespace and Task isolation", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		saveArtifact(t, provider, artifactOwner("tenant-a", "task-1"), "out.bin", "application/octet-stream", []byte{0, 1, 2})
		saveArtifact(t, provider, artifactOwner("tenant-a", "task-2"), "out.bin", "application/octet-stream", []byte{3})
		saveArtifact(t, provider, artifactOwner("tenant-b", "task-1"), "out.bin", "application/octet-stream", []byte{4})

		data, _, err := provider.GetArtifact(ctx, artifactKey("tenant-a", "task-1", "out.bin"))
		require.NoError(t, err)
		require.Equal(t, []byte{0, 1, 2}, data, "binary content is preserved")
		_, _, err = provider.GetArtifact(ctx, artifactKey("tenant-c", "task-1", "out.bin"))
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.NoError(t, provider.DeleteArtifacts(ctx, artifactOwner("tenant-a", "task-1")))
		_, _, err = provider.GetArtifact(ctx, artifactKey("tenant-a", "task-1", "out.bin"))
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		listed, err := provider.ListArtifacts(ctx, artifactOwner("tenant-a", "task-1"))
		require.NoError(t, err)
		require.Empty(t, listed)
		require.NoError(t, provider.DeleteArtifacts(ctx, artifactOwner("tenant-a", "task-1")), "deletion is idempotent")

		data, _, err = provider.GetArtifact(ctx, artifactKey("tenant-a", "task-2", "out.bin"))
		require.NoError(t, err)
		require.Equal(t, []byte{3}, data, "other Tasks keep their artifacts")
		data, _, err = provider.GetArtifact(ctx, artifactKey("tenant-b", "task-1", "out.bin"))
		require.NoError(t, err)
		require.Equal(t, []byte{4}, data, "other namespaces keep their artifacts")
	})

	t.Run("describes the provider", func(t *testing.T) {
		provider := newProvider(t)
		info := requireValidProviderInfo(t, provider, storagev1.ArtifactsContract)
		require.GreaterOrEqual(t, info.Limits.MaxArtifactBytes, int64(storagev1.MaxArtifactBytes),
			"an artifacts.v1 provider accepts artifacts up to the contract limit")
	})
}

// requireValidProviderInfo checks the contract fields of a provider
// description and that the dataset identity is stable across calls.
func requireValidProviderInfo(t *testing.T, provider storagev1.ProviderDescriber, contract string) storagev1.ProviderInfo {
	t.Helper()
	info, err := provider.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, storagev1.ContractVersion, info.ContractVersion)
	require.Contains(t, info.Contracts, contract)
	require.NotEmpty(t, info.DatasetID)
	require.True(t, storagev1.IsValidDatasetOrigin(info.DatasetOrigin), "dataset origin %q", info.DatasetOrigin)
	require.NotEmpty(t, info.SchemaVersion)
	require.True(t, storagev1.IsValidDurabilityClass(info.Durability), "durability class %q", info.Durability)
	require.GreaterOrEqual(t, info.Limits.MaxValueBytes, int64(0))
	require.Equal(t, info.DatasetOrigin == storagev1.DatasetOriginMigrated, info.Source != nil,
		"a dataset names its source exactly when it was imported")
	if info.Source != nil {
		require.NoError(t, info.Source.Validate())
	}
	for _, marker := range info.MigratedTo {
		require.Contains(t, info.Contracts, marker.Contract, "a migration marker names a served contract part")
		require.NotEmpty(t, marker.TargetDatasetID)
	}
	again, err := provider.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, info.DatasetID, again.DatasetID, "the dataset ID is stable")
	return info
}
