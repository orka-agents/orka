package security

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestParseValidationArtifactNormalizesAndBinds(t *testing.T) {
	data := []byte(`{
		"version":1,
		"finding_id":"fnd_1",
		"scan_run_id":"run_1",
		"occurrence_id":"occ_1",
		"status":" VALIDATED ",
		"summary":" confirmed ",
		"validation_steps":[" inspect ",""],
		"evidence":[
			{"kind":"file","path":"internal/api/auth.go","startLine":10,"endLine":12},
			{"kind":"artifact","taskName":"attacker-task","name":"proof.txt"},
			"bounded note"
		],
		"future_extension":{"privateNote":"discard me"},
		"taskName":"attacker-task"
	}`)

	parsed, err := ParseValidationArtifact(data, ValidationArtifactParseOptions{
		ExpectedFindingID:    "fnd_1",
		ExpectedScanRunID:    "run_1",
		ExpectedOccurrenceID: "occ_1",
		TrustedTaskName:      "trusted-task",
		RequireRunBinding:    true,
		ValidateFileEvidence: func(ref store.FindingEvidenceRef) error {
			if ref.Path != "internal/api/auth.go" || ref.StartLine != 10 || ref.EndLine != 12 {
				t.Fatalf("file evidence = %#v", ref)
			}
			return nil
		},
		ResolveArtifact: func(name string) (string, int64, error) {
			if name != "proof.txt" {
				t.Fatalf("artifact name = %q", name)
			}
			return "sha256:" + strings.Repeat("a", 64), 12, nil
		},
	})
	if err != nil {
		t.Fatalf("ParseValidationArtifact() error = %v", err)
	}
	if parsed.Artifact.Status != "validated" || parsed.Artifact.Summary != "confirmed" {
		t.Fatalf("artifact = %#v", parsed.Artifact)
	}
	if got := parsed.Artifact.Evidence[1].TaskName; got != "trusted-task" {
		t.Fatalf("artifact evidence taskName = %q", got)
	}
	if parsed.Artifact.Evidence[1].ContentSHA256 != strings.Repeat("a", 64) || parsed.Artifact.Evidence[1].ContentSize != 12 {
		t.Fatalf("artifact evidence digest metadata = %#v", parsed.Artifact.Evidence[1])
	}
	if strings.Contains(string(parsed.NormalizedJSON), "future_extension") || strings.Contains(string(parsed.NormalizedJSON), "discard me") || strings.Contains(string(parsed.NormalizedJSON), "attacker-task") {
		t.Fatalf("normalized JSON retained untrusted extensions/provenance: %s", parsed.NormalizedJSON)
	}
	if len(parsed.ContentSHA256) != 64 {
		t.Fatalf("digest length = %d", len(parsed.ContentSHA256))
	}
}

func TestParseValidationArtifactRejectsAmbiguousOrInvalidInputs(t *testing.T) {
	tests := map[string]string{
		"empty object":        `{}`,
		"duplicate key":       `{"version":1,"version":1,"finding_id":"f","status":"validated","summary":"ok"}`,
		"case alias":          `{"version":1,"finding_id":"f","Status":"validated","summary":"ok"}`,
		"missing version":     `{"finding_id":"f","status":"validated","summary":"ok"}`,
		"wrong version":       `{"version":2,"finding_id":"f","status":"validated","summary":"ok"}`,
		"missing status":      `{"version":1,"finding_id":"f","summary":"ok"}`,
		"unknown status":      `{"version":1,"finding_id":"f","status":"confirmed","summary":"ok"}`,
		"empty summary":       `{"version":1,"finding_id":"f","status":"validated","summary":" "}`,
		"trailing data":       `{"version":1,"finding_id":"f","status":"validated","summary":"ok"} {}`,
		"missing run binding": `{"version":1,"finding_id":"f","status":"validated","summary":"ok"}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseValidationArtifact([]byte(raw), ValidationArtifactParseOptions{
				ExpectedFindingID: "f",
				ExpectedScanRunID: "run",
				RequireRunBinding: true,
			})
			if err == nil {
				t.Fatal("ParseValidationArtifact() error = nil")
			}
		})
	}
}

func TestParseValidationArtifactRequiresExactOccurrenceBindingWhenExpectedIsEmpty(t *testing.T) {
	raw := []byte(`{"version":1,"finding_id":"f","scan_run_id":"run","occurrence_id":"producer-occurrence","status":"validated","summary":"ok"}`)
	_, err := ParseValidationArtifact(raw, ValidationArtifactParseOptions{
		ExpectedFindingID:    "f",
		ExpectedScanRunID:    "run",
		ExpectedOccurrenceID: "",
		RequireRunBinding:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match trusted occurrence") {
		t.Fatalf("ParseValidationArtifact() error = %v, want exact empty occurrence binding rejection", err)
	}

	withoutOccurrence := []byte(`{"version":1,"finding_id":"f","scan_run_id":"run","status":"validated","summary":"ok"}`)
	if _, err := ParseValidationArtifact(withoutOccurrence, ValidationArtifactParseOptions{
		ExpectedFindingID: "f",
		ExpectedScanRunID: "run",
		RequireRunBinding: true,
	}); err != nil {
		t.Fatalf("ParseValidationArtifact() rejected exact empty occurrence binding: %v", err)
	}
}

func TestParseValidationArtifactRejectsMismatchedBindingsAndEvidence(t *testing.T) {
	base := `{"version":1,"finding_id":"f","scan_run_id":"run","status":"validated","summary":"ok","evidence":%s}`
	tests := []struct {
		name     string
		finding  string
		run      string
		evidence string
		opts     ValidationArtifactParseOptions
	}{
		{name: "finding", finding: "other", run: "run", evidence: `[]`, opts: ValidationArtifactParseOptions{ExpectedFindingID: "f", ExpectedScanRunID: "run", RequireRunBinding: true}},
		{name: "run", finding: "f", run: "other", evidence: `[]`, opts: ValidationArtifactParseOptions{ExpectedFindingID: "f", ExpectedScanRunID: "run", RequireRunBinding: true}},
		{name: "unsafe file", finding: "f", run: "run", evidence: `[{"kind":"file","path":"../private","startLine":1,"endLine":1}]`, opts: ValidationArtifactParseOptions{ExpectedFindingID: "f", ExpectedScanRunID: "run", RequireRunBinding: true}},
		{name: "bad range", finding: "f", run: "run", evidence: `[{"kind":"file","path":"a.go","startLine":5,"endLine":4}]`, opts: ValidationArtifactParseOptions{ExpectedFindingID: "f", ExpectedScanRunID: "run", RequireRunBinding: true}},
		{name: "cross task artifact", finding: "f", run: "run", evidence: `[{"kind":"artifact","taskName":"other","name":"proof.txt"}]`, opts: ValidationArtifactParseOptions{ExpectedFindingID: "f", ExpectedScanRunID: "run", RequireRunBinding: true, TrustedTaskName: "task"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.ReplaceAll(strings.ReplaceAll(base, `"finding_id":"f"`, `"finding_id":"`+tt.finding+`"`), `"scan_run_id":"run"`, `"scan_run_id":"`+tt.run+`"`)
			raw = strings.ReplaceAll(raw, "%s", tt.evidence)
			_, err := ParseValidationArtifact([]byte(raw), tt.opts)
			if err == nil {
				t.Fatal("ParseValidationArtifact() error = nil")
			}
		})
	}
}

func TestParseValidationArtifactRejectsWindowsVolumeQualifiedFileEvidence(t *testing.T) {
	tests := map[string]string{
		"drive absolute backslashes": `C:\repo\file.go`,
		"drive absolute slashes":     `d:/repo/file.go`,
		"drive relative":             `E:repo\file.go`,
		"drive relative slashes":     `z:repo/file.go`,
		"bare drive prefix":          `Q:`,
		"UNC share":                  `\\server\share\file.go`,
		"volume namespace":           `\\?\Volume{01234567-89ab-cdef-0123-456789abcdef}\file.go`,
	}
	for name, evidencePath := range tests {
		t.Run(name, func(t *testing.T) {
			encodedPath, err := json.Marshal(evidencePath)
			if err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"file","path":` + string(encodedPath) + `,"startLine":1,"endLine":1}]}`)
			_, err = ParseValidationArtifact(raw, ValidationArtifactParseOptions{
				ExpectedFindingID: "f",
				ValidateFileEvidence: func(store.FindingEvidenceRef) error {
					t.Fatal("ValidateFileEvidence called for a Windows volume-qualified path")
					return nil
				},
			})
			wantError := "outside repository scope"
			if strings.Contains(evidencePath, `\`) {
				wantError = "must not contain backslashes"
			}
			if err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("ParseValidationArtifact() error = %v, want %q", err, wantError)
			}
		})
	}
}

func TestParseValidationArtifactRejectsRelativeBackslashFileEvidence(t *testing.T) {
	for _, evidencePath := range []string{
		`internal\api/auth.go`,
		`internal/api\auth.go`,
		`safe/..\private.go`,
	} {
		t.Run(evidencePath, func(t *testing.T) {
			encodedPath, err := json.Marshal(evidencePath)
			if err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"file","path":` + string(encodedPath) + `,"startLine":1,"endLine":1}]}`)
			_, err = ParseValidationArtifact(raw, ValidationArtifactParseOptions{
				ExpectedFindingID: "f",
				ValidateFileEvidence: func(store.FindingEvidenceRef) error {
					t.Fatal("ValidateFileEvidence called after a backslash path was rejected")
					return nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "must not contain backslashes") {
				t.Fatalf("ParseValidationArtifact() error = %v", err)
			}
		})
	}
}

func TestParseValidationArtifactAllowsNonVolumeColonInRepositoryPath(t *testing.T) {
	const evidencePath = "docs/C:notes/file.go"
	raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"file","path":"` + evidencePath + `","startLine":1,"endLine":1}]}`)
	_, err := ParseValidationArtifact(raw, ValidationArtifactParseOptions{
		ExpectedFindingID: "f",
		ValidateFileEvidence: func(ref store.FindingEvidenceRef) error {
			if ref.Path != evidencePath {
				t.Fatalf("path = %q", ref.Path)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ParseValidationArtifact() rejected an in-repository colon: %v", err)
	}
}

func TestParseValidationArtifactRejectsArtifactNameSurroundingWhitespace(t *testing.T) {
	for _, name := range []string{" proof.txt", "proof.txt ", "\tproof.txt", "proof.txt\n", "\u00a0proof.txt"} {
		t.Run(name, func(t *testing.T) {
			encodedName, err := json.Marshal(name)
			if err != nil {
				t.Fatalf("marshal artifact name: %v", err)
			}
			raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"artifact","name":` + string(encodedName) + `}]}`)
			resolverCalled := false
			_, err = ParseValidationArtifact(raw, ValidationArtifactParseOptions{
				ExpectedFindingID: "f",
				TrustedTaskName:   "task",
				ResolveArtifact: func(string) (string, int64, error) {
					resolverCalled = true
					return strings.Repeat("a", 64), 1, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "surrounding whitespace") {
				t.Fatalf("ParseValidationArtifact() error = %v", err)
			}
			if resolverCalled {
				t.Fatal("ResolveArtifact called for a non-exact artifact name")
			}
		})
	}
}

func TestParseValidationArtifactPreservesExactArtifactName(t *testing.T) {
	const name = "proof final.txt"
	raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"artifact","name":"` + name + `"}]}`)
	parsed, err := ParseValidationArtifact(raw, ValidationArtifactParseOptions{
		ExpectedFindingID: "f",
		TrustedTaskName:   "task",
		ResolveArtifact: func(got string) (string, int64, error) {
			if got != name {
				t.Fatalf("artifact name = %q, want exact stored key %q", got, name)
			}
			return strings.Repeat("a", 64), 1, nil
		},
	})
	if err != nil {
		t.Fatalf("ParseValidationArtifact() error = %v", err)
	}
	if got := parsed.Artifact.Evidence[0].Name; got != name {
		t.Fatalf("normalized artifact name = %q, want %q", got, name)
	}
}

func TestParseValidationArtifactArtifactResolver(t *testing.T) {
	raw := []byte(`{"version":1,"finding_id":"f","status":"skipped","summary":"bounded","evidence":[{"kind":"artifact","name":"missing.txt"}]}`)
	_, err := ParseValidationArtifact(raw, ValidationArtifactParseOptions{
		ExpectedFindingID: "f",
		TrustedTaskName:   "task",
		ResolveArtifact: func(string) (string, int64, error) {
			return "", 0, errors.New("artifact not found under validation task")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "artifact not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseValidationArtifactRejectsFileOnlyFieldsOnArtifactEvidence(t *testing.T) {
	tests := map[string]string{
		"unsafe path": `"path":"../../outside"`,
		"start line":  `"startLine":1`,
		"end line":    `"endLine":1`,
		"symbol":      `"symbol":"danger"`,
		"quote":       `"quote":"untrusted"`,
	}
	for name, field := range tests {
		t.Run(name, func(t *testing.T) {
			raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"artifact","name":"proof.txt",` + field + `}]}`)
			_, err := ParseValidationArtifact(raw, ValidationArtifactParseOptions{
				ExpectedFindingID: "f",
				TrustedTaskName:   "task",
				ResolveArtifact: func(string) (string, int64, error) {
					t.Fatal("ResolveArtifact called before file-only artifact fields were rejected")
					return "", 0, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "file-only fields") {
				t.Fatalf("ParseValidationArtifact() error = %v", err)
			}
		})
	}
}

func TestParseValidationArtifactPreservesSignificantPathWhitespace(t *testing.T) {
	raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"file","path":"foo.go ","startLine":1,"endLine":1}]}`)
	_, err := ParseValidationArtifact(raw, ValidationArtifactParseOptions{
		ExpectedFindingID: "f",
		ValidateFileEvidence: func(ref store.FindingEvidenceRef) error {
			if ref.Path != "foo.go " {
				t.Fatalf("path = %q", ref.Path)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseValidationArtifactRejectsExactParentPath(t *testing.T) {
	raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"file","path":"..","startLine":1,"endLine":1}]}`)
	if _, err := ParseValidationArtifact(raw, ValidationArtifactParseOptions{ExpectedFindingID: "f"}); err == nil {
		t.Fatal("parent path error = nil")
	}
}

func TestParseValidationArtifactRejectsOversizedNormalizedJSON(t *testing.T) {
	buildArtifact := func(nameBytes int) []byte {
		evidence := make(ValidationArtifactEvidenceRefs, MaxValidationEvidenceItems)
		for i := range evidence {
			evidence[i] = store.FindingEvidenceRef{
				Kind: "artifact",
				Name: strings.Repeat("a", nameBytes),
			}
		}
		data, err := json.Marshal(ValidationArtifact{
			Version:   ValidationArtifactSchemaVersion,
			FindingID: "f",
			Status:    validationStatusValidated,
			Summary:   "ok",
			Evidence:  evidence,
		})
		if err != nil {
			t.Fatalf("marshal validation artifact: %v", err)
		}
		return data
	}

	low, high := 1, MaxValidationEvidenceTextBytes
	for low < high {
		mid := low + (high-low+1)/2
		if len(buildArtifact(mid)) <= MaxValidationArtifactBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	data := buildArtifact(low)
	if len(data) > MaxValidationArtifactBytes {
		t.Fatalf("input artifact size = %d, max = %d", len(data), MaxValidationArtifactBytes)
	}

	_, err := ParseValidationArtifact(data, ValidationArtifactParseOptions{
		ExpectedFindingID: "f",
		TrustedTaskName:   "trusted-task",
		ResolveArtifact: func(string) (string, int64, error) {
			return strings.Repeat("a", 64), 1, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "normalized validation artifact exceeds") {
		t.Fatalf("ParseValidationArtifact() error = %v", err)
	}
}

func TestParseValidationArtifactRejectsDriveQualifiedPathAfterCleaning(t *testing.T) {
	for _, evidencePath := range []string{
		"safe/../C:/repo/file.go",
		"safe/../d:repo/file.go",
	} {
		t.Run(evidencePath, func(t *testing.T) {
			encodedPath, err := json.Marshal(evidencePath)
			if err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"version":1,"finding_id":"f","status":"validated","summary":"ok","evidence":[{"kind":"file","path":` + string(encodedPath) + `,"startLine":1,"endLine":1}]}`)
			_, err = ParseValidationArtifact(raw, ValidationArtifactParseOptions{
				ExpectedFindingID: "f",
				ValidateFileEvidence: func(store.FindingEvidenceRef) error {
					t.Fatal("ValidateFileEvidence called for drive-qualified path exposed by cleaning")
					return nil
				},
			})
			if err == nil {
				t.Fatal("ParseValidationArtifact() accepted a drive-qualified evidence path exposed by cleaning")
			}
		})
	}
}
