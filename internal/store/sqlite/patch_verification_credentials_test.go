package sqlite

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	verification "github.com/orka-agents/orka/internal/patchverification"
)

func patchVerificationCredentialArchive(test *testing.T, text string) []byte {
	test.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "config.json", Mode: 0644, Size: int64(len(text))}); err != nil {
		test.Fatal(err)
	}
	if _, err := writer.Write([]byte(text)); err != nil {
		test.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		test.Fatal(err)
	}
	return archive.Bytes()
}

func TestPatchVerificationBenignCredentialSyntax(test *testing.T) {
	cases := []struct {
		name, content string
	}{
		{"schema", `{"password":{"type":"string"}}`},
		{"empty", `{"password":"","client_secret":""}`},
		{"environment lookup", `password := os.Getenv("PASSWORD")`},
		{"environment placeholder", `password="${PASSWORD}"`},
	}
	for _, scenario := range cases {
		for _, surface := range []string{"manifest", "archive", "stdout", "metadata"} {
			test.Run(scenario.name+"/"+surface, func(test *testing.T) {
				storage := setupTestStore(test)
				if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
					test.Fatal(err)
				}
				manifest, binding, provenance, evidence := patchVerificationFixture(test)
				switch surface {
				case "manifest":
					manifest.Checks[0].Stdin = scenario.content
				case "archive":
					delete(provenance, manifest.Sources.Original.ArchiveDigest)
					content := patchVerificationCredentialArchive(test, scenario.content)
					manifest.Sources.Original.ArchiveDigest = verification.Digest(content)
					provenance[manifest.Sources.Original.ArchiveDigest] = content
				}
				var err error
				binding, err = verification.NewRunBinding(manifest, binding.AttemptID, binding.OriginalTaskID, binding.PatchedTaskID)
				if err != nil {
					test.Fatal(err)
				}
				if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
					test.Fatal(err)
				}
				switch surface {
				case "stdout", "metadata":
					if surface == "stdout" {
						replacePatchVerificationOutput(&evidence[0], scenario.content)
					} else {
						evidence[0].Observation.SetupError = scenario.content
					}
					if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, evidence[0]); err != nil {
						test.Fatal(err)
					}
					if surface == "stdout" {
						output, err := storage.GetPatchVerificationBlob(test.Context(), binding.RunID, evidence[0].Observation.StdoutDigest)
						if err != nil || !bytes.Equal(output, []byte(scenario.content)) {
							test.Fatal("screening changed the evidence bytes")
						}
					}
				}
				record, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
				if err != nil || record.State != verification.RunRunning || len(record.Incidents) != 0 {
					test.Fatal("benign credential syntax invalidated the run")
				}
				if surface == "metadata" && (len(record.Evidence) != 1 || record.Evidence[0].Observation.SetupError != scenario.content) {
					test.Fatal("screening changed the evidence metadata")
				}
				if surface == "manifest" && record.Manifest.Checks[0].Stdin != scenario.content {
					test.Fatal("screening changed the manifest")
				}
				if surface == "archive" {
					content, err := storage.GetPatchVerificationBlob(test.Context(), binding.RunID, manifest.Sources.Original.ArchiveDigest)
					if err != nil || !bytes.Equal(content, provenance[manifest.Sources.Original.ArchiveDigest]) {
						test.Fatal("screening changed the archive bytes")
					}
				}
			})
		}
	}
}

func TestPatchVerificationRejectsEmbeddedCredentials(test *testing.T) {
	cases := []struct {
		name, content string
	}{
		{"literal", `password = "synthetic-fixture-only"`},
		{"short literal", `password = "x"`},
		{"environment fallback", `password="${PASSWORD:-synthetic-fixture-only}"`},
		{"authorization", "Authorization: Bearer synthetic-fixture-only"},
		{"private key", "-----BEGIN " + "PRIVATE KEY-----\nsynthetic-fixture-only\n"},
		{"token", "gh" + "p_" + strings.Repeat("a", 20)},
		{"schema default", `{"password":{"type":"string","default":"synthetic-fixture-only"}}`},
		{"schema enum", `{"password":{"type":"string","enum":["synthetic-fixture-only"]}}`},
	}
	for _, scenario := range cases {
		for _, surface := range []string{"manifest", "archive", "stdout", "metadata"} {
			test.Run(scenario.name+"/"+surface, func(test *testing.T) {
				storage := setupTestStore(test)
				if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
					test.Fatal(err)
				}
				manifest, binding, provenance, evidence := patchVerificationFixture(test)
				expectedErr := verification.ErrEvidence
				switch surface {
				case "manifest":
					manifest.Checks[0].Stdin = scenario.content
				case "archive":
					delete(provenance, manifest.Sources.Original.ArchiveDigest)
					content := patchVerificationCredentialArchive(test, scenario.content)
					manifest.Sources.Original.ArchiveDigest = verification.Digest(content)
					provenance[manifest.Sources.Original.ArchiveDigest] = content
					expectedErr = verification.ErrIntegrity
				}
				var err error
				binding, err = verification.NewRunBinding(manifest, binding.AttemptID, binding.OriginalTaskID, binding.PatchedTaskID)
				if err != nil {
					test.Fatal(err)
				}
				err = storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance)
				if surface == "manifest" || surface == "archive" {
					if !errors.Is(err, expectedErr) || err.Error() != expectedErr.Error() {
						test.Fatal("credential-bearing creation did not return its redacted sentinel")
					}
					if _, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID); !errors.Is(err, verification.ErrRunNotFound) {
						test.Fatal("credential-bearing creation persisted a run")
					}
					return
				}
				if err != nil {
					test.Fatal(err)
				}
				if surface == "stdout" {
					replacePatchVerificationOutput(&evidence[0], scenario.content)
				} else {
					evidence[0].Observation.SetupError = scenario.content
				}
				err = storage.RecordPatchVerificationEvidence(test.Context(), binding, evidence[0])
				if !errors.Is(err, verification.ErrEvidence) || err.Error() != verification.ErrEvidence.Error() {
					test.Fatal("credential-bearing evidence did not return its redacted sentinel")
				}
				record, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
				if err != nil || record.State != verification.RunInvalid || len(record.Incidents) == 0 {
					test.Fatal("credential-bearing evidence did not invalidate the run")
				}
				content, err := json.Marshal(record)
				if err != nil {
					test.Fatal(err)
				}
				rejected, err := json.Marshal(scenario.content)
				if err != nil || bytes.Contains(content, rejected[1:len(rejected)-1]) {
					test.Fatal("rejected credential bytes reached persisted metadata")
				}
				if _, err := storage.GetPatchVerificationBlob(test.Context(), binding.RunID, verification.Digest([]byte(scenario.content))); !errors.Is(err, verification.ErrIntegrity) {
					test.Fatal("rejected credential bytes reached persisted blobs")
				}
			})
		}
	}
}
