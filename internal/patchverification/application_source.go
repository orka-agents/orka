package patchverification

import (
	"bytes"
	"context"
	"crypto/sha1" // Git's SHA-1 object format is an identity format, not a new security primitive.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
)

// Application preparation retains the existing provenance/Manifest protocol;
// unlike the bounded model source packet it always imports the complete tree.
type ApplicationPreparationInput = PreparationInput

type ApplicationSourceMetadata = verificationv1.ApplicationSourceMetadata

type ApplicationArchiveMetadata = verificationv1.ApplicationArchiveMetadata

type ApplicationPreparationReport struct {
	Version     int               `json:"version"`
	TaskUID     string            `json:"taskUID"`
	PodUID      string            `json:"podUID"`
	InputDigest string            `json:"inputDigest"`
	Manifest    Manifest          `json:"manifest"`
	Provenance  map[string][]byte `json:"provenance"`
}

func DecodeApplicationPreparationInput(reader io.Reader) (ApplicationPreparationInput, error) {
	input, err := DecodePreparationInput(reader)
	if err == nil && input.Manifest.Application == nil {
		err = ErrEvidence
	}
	return input, err
}

func DecodeApplicationPreparationReport(reader io.Reader) (ApplicationPreparationReport, error) {
	var report ApplicationPreparationReport
	err := decodePreparationJSON(reader, &report, MaxApplicationPreparationReportBytes)
	if err == nil && report.Manifest.Application == nil {
		err = ErrEvidence
	}
	return report, err
}

func ValidateApplicationPreparationReport(input ApplicationPreparationInput, report ApplicationPreparationReport) error {
	if input.Manifest.Application == nil || report.Manifest.Application == nil {
		return ErrEvidence
	}
	digest, err := PreparationInputDigest(input)
	if err != nil || ValidatePreparationInput(input) != nil || report.Version != ApplicationProtocolVersion ||
		report.TaskUID != input.TaskUID || report.InputDigest != digest || ValidateManifest(report.Manifest) != nil ||
		ValidateApplicationSourceMetadata(report.Manifest) != nil {
		return ErrEvidence
	}
	expected := input.Manifest
	expected.Sources, expected.ApplicationSourceMetadata = report.Manifest.Sources, report.Manifest.ApplicationSourceMetadata
	if !reflect.DeepEqual(expected, report.Manifest) ||
		report.Manifest.Sources.Repository != input.Manifest.Sources.Repository ||
		report.Manifest.Sources.Original.Commit != input.Manifest.Sources.Original.Commit ||
		report.Manifest.Sources.Original.Tree != input.Manifest.Sources.Original.Tree ||
		!slices.Equal(report.Manifest.Sources.Paths, input.Manifest.Sources.Paths) {
		return ErrBinding
	}
	if input.Manifest.Action == VerifyPatch &&
		(report.Manifest.Sources.Patched.Commit != "" || report.Manifest.Sources.PatchDigest != Digest(input.Patch)) {
		return ErrBinding
	}
	required, err := RequiredProvenanceDigests(report.Manifest)
	if err != nil || len(required) != len(report.Provenance) {
		return ErrEvidence
	}
	metadata, err := json.Marshal(report.Manifest.ApplicationSourceMetadata)
	if err != nil || !bytes.Equal(metadata, report.Provenance[Digest(metadata)]) {
		return ErrBinding
	}
	for _, id := range required {
		content, ok := report.Provenance[id]
		if !ok || len(content) > MaxPreparationInputBytes || Digest(content) != id || ContainsCredentialMaterial(content) {
			return ErrIntegrity
		}
	}
	content, err := json.Marshal(report)
	if err != nil || len(content)+1 > MaxApplicationPreparationReportBytes || ContainsCredentialMaterial(content) {
		return ErrLimit
	}
	return nil
}

// ExtractApplicationSource checks both the archive digest and its reconstructed
// Git tree, so a valid archive hash cannot bless a filtered or substituted tree.
func ExtractApplicationSource(ctx context.Context, archive []byte, source SourceIdentity, directory string, expected ...ApplicationArchiveMetadata) error {
	if !gitIdentityPattern.MatchString(source.Tree) || len(archive) > MaxApplicationSourceBytes ||
		Digest(archive) != source.ArchiveDigest {
		return ErrBinding
	}
	if err := sourceExtractArchiveBounds(ctx, archive, directory, MaxApplicationSourceBytes,
		MaxApplicationSourceContentBytes, MaxApplicationSourceEntries); err != nil {
		return err
	}
	tree, err := applicationDirectoryTree(ctx, directory, len(source.Tree))
	if err != nil || tree != source.Tree {
		return ErrIntegrity
	}
	if len(expected) > 1 {
		return ErrEvidence
	}
	if len(expected) == 1 {
		metadata := ApplicationArchiveMetadata{ArchiveDigest: Digest(archive), ArchiveBytes: int64(len(archive))}
		err := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || ctx.Err() != nil {
				return ErrIntegrity
			}
			if name == directory {
				return nil
			}
			metadata.Entries++
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return ErrIntegrity
				}
				metadata.ContentBytes += info.Size()
			}
			return nil
		})
		if err != nil || metadata != expected[0] {
			return ErrBinding
		}
	}
	return nil
}

func ValidateApplicationSourceMetadata(manifest Manifest) error {
	metadata := manifest.ApplicationSourceMetadata
	if manifest.Application == nil {
		if metadata != nil {
			return ErrEvidence
		}
		return nil
	}
	if metadata == nil || metadata.Version != ApplicationProtocolVersion ||
		metadata.Original.ArchiveDigest != manifest.Sources.Original.ArchiveDigest ||
		!validApplicationArchiveMetadata(metadata.Original) {
		return ErrBinding
	}
	if manifest.Action == ValidateReport {
		if metadata.Patched != nil {
			return ErrBinding
		}
	} else if metadata.Patched == nil || !validApplicationArchiveMetadata(*metadata.Patched) ||
		metadata.Patched.ArchiveDigest != manifest.Sources.Patched.ArchiveDigest {
		return ErrBinding
	}
	return nil
}

func validApplicationArchiveMetadata(metadata ApplicationArchiveMetadata) bool {
	return sha256Pattern.MatchString(metadata.ArchiveDigest) && metadata.ArchiveBytes >= 1024 &&
		metadata.ArchiveBytes <= MaxApplicationSourceBytes && metadata.ContentBytes >= 0 &&
		metadata.ContentBytes <= MaxApplicationSourceContentBytes && metadata.ContentBytes <= metadata.ArchiveBytes &&
		metadata.Entries > 0 && metadata.Entries <= MaxApplicationSourceEntries
}

// RequiredProvenanceDigests preserves legacy archive retention, while application
// runs retain only canonical source metadata and the bounded patch/diff. Actual
// complete archives remain on the exact run-owned target PVC and are measured
// independently again by the compiler worker.
func RequiredProvenanceDigests(manifest Manifest) ([]string, error) {
	required := map[string]bool{}
	if manifest.Application == nil {
		required[manifest.Sources.Original.ArchiveDigest] = true
		if manifest.Action != ValidateReport {
			required[manifest.Sources.Patched.ArchiveDigest] = true
		}
	} else {
		if err := ValidateApplicationSourceMetadata(manifest); err != nil {
			return nil, err
		}
		content, err := json.Marshal(manifest.ApplicationSourceMetadata)
		if err != nil {
			return nil, err
		}
		required[Digest(content)] = true
	}
	if manifest.Action != ValidateReport {
		required[manifest.Sources.DiffDigest] = true
		if manifest.Sources.PatchDigest != "" {
			required[manifest.Sources.PatchDigest] = true
		}
	}
	for _, file := range manifest.Files {
		required[file.Digest] = true
	}
	result := make([]string, 0, len(required))
	for digest := range required {
		if !sha256Pattern.MatchString(digest) {
			return nil, ErrEvidence
		}
		result = append(result, digest)
	}
	slices.Sort(result)
	return result, nil
}

func applicationObjectDigest(kind string, content []byte, identityLength int) string {
	hasher := sha1.New()
	if identityLength == 64 {
		hasher = sha256.New()
	}
	_, _ = fmt.Fprintf(hasher, "%s %d\x00", kind, len(content))
	_, _ = hasher.Write(content)
	return hex.EncodeToString(hasher.Sum(nil))
}

func applicationDirectoryTree(ctx context.Context, directory string, identityLength int) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", ErrIntegrity
	}
	sort.Slice(entries, func(i, j int) bool {
		first, second := entries[i].Name(), entries[j].Name()
		if entries[i].IsDir() {
			first += "/"
		}
		if entries[j].IsDir() {
			second += "/"
		}
		return first < second
	})
	tree := make([]byte, 0)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		info, err := entry.Info()
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return "", ErrIntegrity
		}
		mode, identity := "40000", ""
		if entry.IsDir() {
			identity, err = applicationDirectoryTree(ctx, filepath.Join(directory, entry.Name()), identityLength)
		} else if info.Mode().IsRegular() {
			mode = "100644"
			if info.Mode().Perm()&0111 != 0 {
				mode = "100755"
			}
			var content []byte
			content, err = os.ReadFile(filepath.Join(directory, entry.Name()))
			if err == nil {
				identity = applicationObjectDigest("blob", content, identityLength)
			}
		} else {
			return "", ErrIntegrity
		}
		if err != nil {
			return "", ErrIntegrity
		}
		rawIdentity, err := hex.DecodeString(identity)
		if err != nil {
			return "", ErrIntegrity
		}
		tree = append(tree, []byte(mode+" "+entry.Name()+"\x00")...)
		tree = append(tree, rawIdentity...)
	}
	return applicationObjectDigest("tree", tree, identityLength), nil
}

func NewApplicationArtifact(input ApplicationWorkerInput, digest string, size int64, build ApplicationContainerIdentity) (ApplicationArtifact, error) {
	if err := ValidateApplicationWorkerInput(input); err != nil {
		return ApplicationArtifact{}, err
	}
	source, taskID, err := applicationSource(input.Manifest, input.Binding, input.Side)
	if err != nil || !sha256Pattern.MatchString(digest) || size <= 0 || size > MaxApplicationBinaryBytes ||
		build.PodUID == "" || build.ImageID != input.Manifest.Environment.ImageID {
		return ApplicationArtifact{}, ErrBinding
	}
	planDigest, err := ApplicationPlanDigest(*input.Manifest.Application)
	if err != nil {
		return ApplicationArtifact{}, err
	}
	return ApplicationArtifact{
		Version: ApplicationProtocolVersion, RunID: input.Binding.RunID, AttemptID: input.Binding.AttemptID,
		TaskID: taskID, ManifestDigest: input.Binding.ManifestDigest, PlanDigest: planDigest,
		Side: input.Side, Source: source, BinaryDigest: digest, BinaryBytes: size,
		GoVersion: input.Manifest.Application.Build.GoVersion, Platform: input.Manifest.Environment.Platform, Build: build,
	}, nil
}

func DecodeApplicationArtifact(reader io.Reader) (ApplicationArtifact, error) {
	var artifact ApplicationArtifact
	err := decodePreparationJSON(reader, &artifact, MaxApplicationReportBytes)
	return artifact, err
}

func DecodeApplicationBuildReport(reader io.Reader) (ApplicationBuildReport, error) {
	var report ApplicationBuildReport
	err := decodePreparationJSON(reader, &report, MaxApplicationReportBytes)
	return report, err
}

func DecodeApplicationReport(reader io.Reader) (ApplicationReport, error) {
	var report ApplicationReport
	err := decodePreparationJSON(reader, &report, MaxApplicationReportBytes)
	return report, err
}

func DecodeApplicationBoundaryInput(reader io.Reader) (ApplicationBoundaryInput, error) {
	var input ApplicationBoundaryInput
	err := decodePreparationJSON(reader, &input, MaxApplicationReportBytes)
	if err == nil {
		_, err = ApplicationBoundaryInputDigest(input)
	}
	return input, err
}

func DecodeApplicationBoundaryReport(reader io.Reader) (ApplicationBoundaryReport, error) {
	var report ApplicationBoundaryReport
	err := decodePreparationJSON(reader, &report, MaxApplicationReportBytes)
	return report, err
}

func MarshalApplicationReport(report any) ([]byte, error) {
	content, err := json.Marshal(report)
	if err != nil || len(content)+1 > MaxApplicationReportBytes || ContainsCredentialMaterial(content) {
		return nil, ErrLimit
	}
	return append(content, '\n'), nil
}
