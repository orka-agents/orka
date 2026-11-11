package patchverification

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestActionReportOriginalOnly(test *testing.T) {
	manifest := testManifest()
	manifest.Action = ValidateReport
	manifest.ReportDigest, _ = StableReportDigest(manifest.Problem, manifest.Scope)
	manifest.Sources.Patched = SourceIdentity{}
	manifest.Sources.DiffDigest = ""
	manifest.Checks = manifest.Checks[:1]
	binding, observations := testEvidence(manifest)
	binding.PatchedTaskID = ""
	if err := ValidateManifest(manifest); err != nil {
		test.Fatal(err)
	}
	assessment := Evaluate(manifest, binding, observations[:1])
	if assessment.Conclusion != Reproduced || len(assessment.Checks) != 1 {
		test.Fatalf("original-only report result: %+v", assessment)
	}
	if err := ValidateObservation(manifest, binding, observations[1]); err == nil {
		test.Fatal("report accepted patched evidence")
	}
	manifest.ReportDigest = Digest([]byte("claimed identity"))
	if err := ValidateManifest(manifest); err == nil {
		test.Fatal("accepted an arbitrary report digest")
	}
}

func TestActionReportDigestAndLegacyJSON(test *testing.T) {
	digest, err := StableReportDigest("reported problem", []string{"first", "second"})
	if err != nil || digest != Digest([]byte(`{"problem":"reported problem","scope":["first","second"]}`)) {
		test.Fatalf("report digest contract: %s, %v", digest, err)
	}
	changed, err := StableReportDigest("reported problem", []string{"second", "first"})
	if err != nil || changed == digest {
		test.Fatal("report identity did not bind the ordered scope")
	}
	manifest := testManifest()
	content, err := json.Marshal(manifest)
	if err != nil || strings.Contains(string(content), `"action"`) || strings.Contains(string(content), `"reportDigest"`) {
		test.Fatalf("new fields changed legacy JSON: %s, %v", content, err)
	}
	binding, observations := testEvidence(manifest)
	if result := Evaluate(manifest, binding, observations); result.Conclusion != Verified {
		test.Fatalf("legacy adjudication changed: %+v", result)
	}
}

func TestRequirementsAndLifecycleFrozen(test *testing.T) {
	manifest := testManifest()
	before, _ := ManifestDigest(manifest)
	manifest.Environment.Requirements = []EnvironmentRequirement{
		{Kind: "process", Name: "modeled-manager"},
		{Kind: "cluster", Name: "test-cluster"},
		{Kind: "controller", Name: "real-manager"},
		{Kind: "test-identity", Name: "separate-user"},
	}
	manifest.Checks[0].Lifecycle = []string{"install", "reconcile", "restart", "upgrade"}
	manifest.DeclaredChanges = []DeclaredChange{{Kind: "configuration", Paths: []string{"config/settings.json"},
		Description: "persist the managed setting"}}
	if err := ValidateManifest(manifest); err != nil {
		test.Fatal(err)
	}
	after, _ := ManifestDigest(manifest)
	if before == after || len(MissingRequirements(manifest.Environment)) != 3 {
		test.Fatal("frozen metadata or missing environment requirements were lost")
	}
	manifest.DeclaredChanges[0].Paths = []string{"../outside"}
	if err := ValidateManifest(manifest); err == nil {
		test.Fatal("unsafe declaration path accepted")
	}
}

func TestRequirementsNamedLocalServices(test *testing.T) {
	for _, fixtureID := range []string{"", "other-service", "inventory"} {
		test.Run("fixture="+fixtureID, func(test *testing.T) {
			environment := Environment{Profile: LocalServices, Requirements: []EnvironmentRequirement{
				{Kind: LocalServices, Name: "inventory"},
			}}
			if fixtureID != "" {
				environment.Services = []Service{{ID: fixtureID}}
			}
			missing := MissingRequirements(environment)
			if (len(missing) == 0) != (fixtureID == "inventory") {
				test.Fatalf("required inventory fixture matched %q: %v", fixtureID, missing)
			}
			if len(missing) != 0 && !strings.Contains(missing[0], "inventory") {
				test.Fatal("missing fixture identity was not named")
			}
		})
	}
}
