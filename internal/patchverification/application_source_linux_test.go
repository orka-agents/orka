//go:build linux

package patchverification

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplicationSourceBudgetsDoNotRelaxLegacyBounds(t *testing.T) {
	require.Equal(t, 32<<20, sourceMaxArchiveBytes)
	require.Equal(t, 8192, sourceMaxTreeEntries)
	require.Equal(t, 512<<10, MaxPublicSourceArchiveBytes)
	require.GreaterOrEqual(t, MaxApplicationSourceContentBytes, 256<<20)
	require.Greater(t, MaxApplicationSourceBytes, MaxApplicationSourceContentBytes)
	require.GreaterOrEqual(t, MaxApplicationSourceEntries, 20000)
	metadata := ApplicationArchiveMetadata{ArchiveDigest: Digest([]byte("full source")), ArchiveBytes: MaxApplicationSourceBytes,
		ContentBytes: MaxApplicationSourceContentBytes, Entries: MaxApplicationSourceEntries}
	require.True(t, validApplicationArchiveMetadata(metadata))
	for _, invalid := range []ApplicationArchiveMetadata{
		{ArchiveDigest: metadata.ArchiveDigest, ArchiveBytes: MaxApplicationSourceBytes + 1, ContentBytes: 1, Entries: 1},
		{ArchiveDigest: metadata.ArchiveDigest, ArchiveBytes: MaxApplicationSourceBytes, ContentBytes: MaxApplicationSourceContentBytes + 1, Entries: 1},
		{ArchiveDigest: metadata.ArchiveDigest, ArchiveBytes: MaxApplicationSourceBytes, ContentBytes: 1, Entries: MaxApplicationSourceEntries + 1},
	} {
		require.False(t, validApplicationArchiveMetadata(invalid))
	}
	header := &tar.Header{Name: "source.go", Mode: 0644, Typeflag: tar.TypeReg, Size: 64 << 20}
	require.Error(t, sourceValidateArchiveHeader(header))
	require.NoError(t, sourceValidateArchiveHeaderLimit(header, MaxApplicationSourceContentBytes))
	header.Size = MaxApplicationSourceContentBytes + 1
	require.Error(t, sourceValidateArchiveHeaderLimit(header, MaxApplicationSourceContentBytes))
	writer := applicationArchiveWriter{writer: &bytes.Buffer{}, count: MaxApplicationSourceBytes}
	_, err := writer.Write([]byte{1})
	require.ErrorIs(t, err, ErrLimit)
}

func TestApplicationCompleteTreeCountAndPathThresholds(t *testing.T) {
	var listing bytes.Buffer
	for index := range MaxApplicationSourceEntries {
		fmt.Fprintf(&listing, "100644 blob %s\tfile-%05d.go\x00", strings.Repeat("a", 40), index)
	}
	entries, err := applicationTreeEntries(listing.Bytes())
	require.NoError(t, err)
	require.Len(t, entries, MaxApplicationSourceEntries)
	fmt.Fprintf(&listing, "100644 blob %s\tover-limit.go\x00", strings.Repeat("b", 40))
	_, err = applicationTreeEntries(listing.Bytes())
	require.Error(t, err)
	for _, name := range []string{"../outside", "/absolute", ".git/config", "folder/../../outside"} {
		raw := []byte(fmt.Sprintf("100644 blob %s\t%s\x00", strings.Repeat("a", 40), name))
		_, err := applicationTreeEntries(raw)
		require.Error(t, err)
	}
	_, err = applicationTreeEntries([]byte(fmt.Sprintf("120000 blob %s\tlink\x00", strings.Repeat("a", 40))))
	require.Error(t, err)
}

func TestApplicationBatchInventoryBindsCompleteGitTree(t *testing.T) {
	id := applicationObjectDigest("blob", []byte("public immutable source"), 40)
	rawID := make([]byte, 20)
	for i := range rawID {
		_, err := fmt.Sscanf(id[i*2:i*2+2], "%02x", &rawID[i])
		require.NoError(t, err)
	}
	tree := applicationObjectDigest("tree", append([]byte("100644 source.go\x00"), rawID...), 40)
	entries := []sourceTreeEntry{{name: "source.go", kind: sourceGitBlob, identity: id, mode: 0644}}
	require.NoError(t, applicationVerifyEntries(entries, tree))
	entries[0].name = "filtered.go"
	require.Error(t, applicationVerifyEntries(entries, tree))
	entries[0].name = "missing-parent/source.go"
	require.Error(t, applicationVerifyEntries(entries, tree))
}

func TestApplicationMetadataOnlyProvenanceRetainsNoArchivePayload(t *testing.T) {
	manifest := applicationTestManifest(t)
	manifest.Action, manifest.DeclaredChanges = ValidateReport, nil
	manifest.Sources.Patched, manifest.Sources.DiffDigest = SourceIdentity{}, ""
	manifest.ApplicationSourceMetadata = nil
	manifest.Sources.Original.ArchiveDigest = ""
	manifest.Environment.Dependencies[PreparationPolicyKey] = PreparationPolicy
	manifest.Environment.Dependencies[PreparationConfigurationKey] = Digest([]byte("operator-configuration"))
	input := ApplicationPreparationInput{Version: PreparationProtocolVersion, ID: "prepare", Namespace: "control",
		ParentTaskName: "parent", ParentTaskUID: "parent-uid", TaskUID: "prepare-uid", Manifest: manifest}
	require.NoError(t, ValidatePreparationInput(input))
	manifest.Sources.Original.ArchiveDigest = Digest([]byte("complete immutable PVC archive"))
	manifest.ApplicationSourceMetadata = &ApplicationSourceMetadata{Version: ApplicationProtocolVersion,
		Original: ApplicationArchiveMetadata{ArchiveDigest: manifest.Sources.Original.ArchiveDigest,
			ArchiveBytes: 280 << 20, ContentBytes: 256 << 20, Entries: 24000}}
	metadata, err := json.Marshal(manifest.ApplicationSourceMetadata)
	require.NoError(t, err)
	digest, err := PreparationInputDigest(input)
	require.NoError(t, err)
	report := ApplicationPreparationReport{Version: ApplicationProtocolVersion, TaskUID: input.TaskUID,
		InputDigest: digest, Manifest: manifest, Provenance: map[string][]byte{Digest(metadata): metadata}}
	require.NoError(t, ValidateApplicationPreparationReport(input, report))
	required, err := RequiredProvenanceDigests(manifest)
	require.NoError(t, err)
	require.Equal(t, []string{Digest(metadata)}, required)
	require.NotContains(t, report.Provenance, manifest.Sources.Original.ArchiveDigest)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.Less(t, len(encoded), 32<<10)
	report.Manifest.ApplicationSourceMetadata.Original.ContentBytes++
	require.Error(t, ValidateApplicationPreparationReport(input, report))
}

func TestApplicationTextPatchBudgetDoesNotMultiplyFullTreeByChangedFiles(t *testing.T) {
	patch := []byte("diff --git a/one.go b/one.go\n--- a/one.go\n+++ b/one.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/two.go b/two.go\n--- a/two.go\n+++ b/two.go\n@@ -1 +1 @@\n-old\n+new\n")
	require.NoError(t, applicationPatchBounds(patch))
	require.Error(t, applicationPatchBounds(append(patch, []byte("GIT binary patch\nliteral 1000000000\n")...)))
	require.Error(t, applicationPatchBounds(bytes.Repeat([]byte("x"), MaxPreparationPatchBytes+1)))
}
