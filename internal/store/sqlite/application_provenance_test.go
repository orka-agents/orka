package sqlite

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	verification "github.com/orka-agents/orka/internal/patchverification"
)

func applicationProvenanceFixture(t *testing.T, action verification.Action) (verification.Manifest, verification.Binding, map[string][]byte) {
	t.Helper()
	manifest, _, legacy, _ := patchVerificationFixture(t)
	manifest.Action = action
	manifest.Sources.Repository = "https://github.com/example/public-source"
	manifest.Environment.Profile = verification.ApplicationRuntimeProfile
	manifest.Environment.Dependencies = map[string]string{
		verification.ApplicationPolicyKey:          verification.ApplicationPolicyVersion,
		verification.ApplicationObserverImageKey:   "example/observer@" + verification.Digest([]byte("observer-image")),
		verification.ApplicationObserverImageIDKey: verification.Digest([]byte("observer-config")),
	}
	manifest.Application = &verification.ApplicationPlan{
		Version: verification.ApplicationProtocolVersion,
		Build: verification.ApplicationBuild{
			Profile: verification.ApplicationBuildGoPackage, Package: "./cmd/server", GoVersion: "go1.27.2",
		},
		Runtime: verification.ApplicationRuntime{
			Image: "example/runtime@" + verification.Digest([]byte("runtime-image")), ImageID: verification.Digest([]byte("runtime-config")),
		},
		Resources: []json.RawMessage{
			json.RawMessage(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"case-unsafe"},` +
				`"spec":{"defaultBackend":{"service":{"name":"backend","port":{"number":8080}}}}}`),
			json.RawMessage(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"case-normal"},` +
				`"spec":{"defaultBackend":{"service":{"name":"backend","port":{"number":8080}}}}}`),
		},
		Permissions: []verification.ApplicationPermission{{
			APIGroup: "networking.k8s.io", Resources: []string{"ingresses/status"},
			Verbs: []string{"update", "patch"}, ResourceNames: []string{"case-unsafe", "case-normal"},
		}},
	}
	manifest.ApplicationSourceMetadata = &verification.ApplicationSourceMetadata{
		Version: verification.ApplicationProtocolVersion,
		Original: verification.ApplicationArchiveMetadata{
			ArchiveDigest: manifest.Sources.Original.ArchiveDigest, ArchiveBytes: 280 << 20, ContentBytes: 256 << 20, Entries: 24000,
		},
	}
	provenance := make(map[string][]byte)
	patchedTask := ""
	if action == verification.ValidateReport {
		manifest.Sources.Patched, manifest.Sources.DiffDigest = verification.SourceIdentity{}, ""
	} else {
		patchedTask = "application-patched-task"
		manifest.ApplicationSourceMetadata.Patched = &verification.ApplicationArchiveMetadata{
			ArchiveDigest: manifest.Sources.Patched.ArchiveDigest, ArchiveBytes: 280 << 20, ContentBytes: 256 << 20, Entries: 24000,
		}
		manifest.DeclaredChanges = []verification.DeclaredChange{{Kind: "source", Description: "repair application behavior"}}
		provenance[manifest.Sources.DiffDigest] = legacy[manifest.Sources.DiffDigest]
	}
	var err error
	manifest.ReportDigest, err = verification.StableReportDigest(manifest.Problem, manifest.Scope)
	if err != nil {
		t.Fatal(err)
	}
	healthyValue := json.RawMessage(`[{"ip":"192.0.2.1"}]`)
	normalValue := json.RawMessage(`[{"ip":"192.0.2.2"}]`)
	healthy, err := verification.ApplicationAPIExpectation(verification.ApplicationAPIValue{Exists: true, Found: true, Value: healthyValue})
	if err != nil {
		t.Fatal(err)
	}
	failure, err := verification.ApplicationAPIExpectation(verification.ApplicationAPIValue{Exists: true, Found: true, Value: normalValue})
	if err != nil {
		t.Fatal(err)
	}
	manifest.Checks = []verification.Check{
		{ID: "unsafe", Kind: verification.Reproduction, TimeoutSeconds: 10, Healthy: healthy, Failure: failure,
			Application: &verification.ApplicationCheck{Version: verification.ApplicationProtocolVersion,
				Kubernetes: &verification.ApplicationKubernetesCheck{
					APIVersion: "networking.k8s.io/v1", Resource: "ingresses", Name: "case-unsafe", Pointer: "/status/loadBalancer/ingress",
				}}},
		{ID: "normal", Kind: verification.Normal, TimeoutSeconds: 10, Healthy: failure, Failure: healthy,
			Application: &verification.ApplicationCheck{Version: verification.ApplicationProtocolVersion,
				Kubernetes: &verification.ApplicationKubernetesCheck{
					APIVersion: "networking.k8s.io/v1", Resource: "ingresses", Name: "case-normal",
					Pointer: "/status/loadBalancer/ingress", NormalValue: normalValue,
				}}},
	}
	metadata, err := json.Marshal(manifest.ApplicationSourceMetadata)
	if err != nil {
		t.Fatal(err)
	}
	provenance[verification.Digest(metadata)] = metadata
	binding, err := verification.NewRunBinding(manifest, "application-attempt", "application-original-task", patchedTask)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, binding, provenance
}

func TestApplicationProvenancePersistsMetadataWithoutArchivePayloads(t *testing.T) {
	for _, action := range []verification.Action{verification.ValidateReport, verification.VerifyPatch} {
		t.Run(string(action), func(t *testing.T) {
			storage := setupTestStore(t)
			if err := storage.InitializePatchVerificationStore(t.Context()); err != nil {
				t.Fatal(err)
			}
			manifest, binding, provenance := applicationProvenanceFixture(t, action)
			if err := storage.CreatePatchVerificationRun(t.Context(), manifest, binding, provenance); err != nil {
				t.Fatal(err)
			}
			record, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if len(record.Provenance) != len(provenance) {
				t.Fatalf("unexpected retained provenance count: %d", len(record.Provenance))
			}
			for _, reference := range record.Provenance {
				if reference.Digest == manifest.Sources.Original.ArchiveDigest || reference.Digest == manifest.Sources.Patched.ArchiveDigest {
					t.Fatal("application source archive was retained in the controller store")
				}
				content, err := storage.GetPatchVerificationBlob(t.Context(), binding.RunID, reference.Digest)
				if err != nil || !bytes.Equal(content, provenance[reference.Digest]) {
					t.Fatalf("metadata provenance mismatch: %v", err)
				}
			}
		})
	}
}

func TestApplicationProvenanceRejectsMissingMetadataAndArchiveSubstitution(t *testing.T) {
	manifest, _, provenance := applicationProvenanceFixture(t, verification.ValidateReport)
	if _, _, err := patchVerificationProvenance(manifest, nil); !errors.Is(err, verification.ErrIntegrity) {
		t.Fatalf("missing application metadata accepted: %v", err)
	}
	archive := []byte("original")
	provenance[verification.Digest(archive)] = archive
	if _, _, err := patchVerificationProvenance(manifest, provenance); !errors.Is(err, verification.ErrEvidence) {
		t.Fatalf("unexpected complete source archive accepted: %v", err)
	}
	delete(provenance, verification.Digest(archive))
	manifest.ApplicationSourceMetadata.Original.ContentBytes++
	if _, _, err := patchVerificationProvenance(manifest, provenance); !errors.Is(err, verification.ErrIntegrity) {
		t.Fatalf("inconsistent source metadata accepted: %v", err)
	}
}
