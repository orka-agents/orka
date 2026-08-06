package security

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestClassifyGitRef(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		wantKind   GitRefKind
		wantOID    string
		wantOIDSet bool
	}{
		{name: "sha1", ref: strings.Repeat("A", 40), wantKind: GitRefKindFullObjectID, wantOID: strings.Repeat("a", 40), wantOIDSet: true},
		{name: "sha256", ref: strings.Repeat("B", 64), wantKind: GitRefKindFullObjectID, wantOID: strings.Repeat("b", 64), wantOIDSet: true},
		{name: "abbreviated", ref: "abcdef1", wantKind: GitRefKindAbbreviatedObjectID},
		{name: "explicit head", ref: "refs/heads/" + strings.Repeat("c", 40), wantKind: GitRefKindSymbolic},
		{name: "explicit tag", ref: "refs/tags/" + strings.Repeat("d", 40), wantKind: GitRefKindSymbolic},
		{name: "symbolic", ref: "feature/deadbeef", wantKind: GitRefKindSymbolic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyGitRef(tt.ref); got != tt.wantKind {
				t.Fatalf("ClassifyGitRef(%q) = %q, want %q", tt.ref, got, tt.wantKind)
			}
			oid, ok := NormalizeFullGitObjectID(tt.ref)
			if ok != tt.wantOIDSet || oid != tt.wantOID {
				t.Fatalf("NormalizeFullGitObjectID(%q) = (%q, %v), want (%q, %v)", tt.ref, oid, ok, tt.wantOID, tt.wantOIDSet)
			}
		})
	}
}

func TestIntegrityConfigFromEnvDefaultsAndDependencies(t *testing.T) {
	cfg, err := IntegrityConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("IntegrityConfigFromEnv(defaults) error = %v", err)
	}
	if cfg.WorkerOutputBindingMode != WorkerOutputBindingAudit || cfg.BundleSealingMode != BundleSealingOff {
		t.Fatalf("defaults = %#v", cfg)
	}

	values := map[string]string{
		EnvQualityStateWritesEnabled: "true",
		EnvFindingObservationWrites:  "true",
		EnvDeepScanEnabled:           "true",
	}
	cfg, err = IntegrityConfigFromEnv(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("IntegrityConfigFromEnv(configured) error = %v", err)
	}
	if cfg.WorkerOutputBindingMode != WorkerOutputBindingAudit || cfg.StrictCompletionEnabled || !cfg.DeepScanEnabled {
		t.Fatalf("configured = %#v", cfg)
	}
}

func TestIntegrityConfigFromEnvRejectsInvalidOrUnsafeCombinations(t *testing.T) {
	tests := []map[string]string{
		{EnvWorkerOutputBindingMode: "sometimes"},
		{EnvBundleSealingMode: "audit"},
		{EnvPinnedScanTargetsEnabled: "yes please"},
		{EnvStrictCompletionEnabled: "true"},
		{EnvDeepScanEnabled: "true"},
		{EnvWorkerOutputBindingMode: "enforce"},
		{EnvHardenedAnalysisEnabled: "true"},
		{EnvBundleSealingMode: "enforce"},
		{
			EnvWorkerOutputBindingMode:   "enforce",
			EnvPinnedScanTargetsEnabled:  "true",
			EnvQualityStateWritesEnabled: "true",
			EnvFindingObservationWrites:  "true",
			EnvBundleSealingMode:         "shadow",
		},
		{
			EnvQualityStateWritesEnabled: "true",
			EnvStrictCompletionEnabled:   "true",
		},
	}
	for _, values := range tests {
		if _, err := IntegrityConfigFromEnv(func(name string) string { return values[name] }); err == nil {
			t.Fatalf("IntegrityConfigFromEnv(%#v) error = nil", values)
		}
	}
}

func TestIntegrityConfigFromEnvRejectsUnimplementedAuthorityGates(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{
			name:   "output enforcement requires unpredictable lease",
			values: map[string]string{EnvWorkerOutputBindingMode: "enforce"},
			want:   "unpredictable attempt leases",
		},
		{
			name: "pinned targets cannot bypass lease dependency",
			values: map[string]string{
				EnvWorkerOutputBindingMode:  "enforce",
				EnvPinnedScanTargetsEnabled: "true",
			},
			want: "unpredictable attempt leases",
		},
		{
			name:   "hardened analysis requires capability snapshot",
			values: map[string]string{EnvHardenedAnalysisEnabled: "true"},
			want:   "capability snapshots",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := IntegrityConfigFromEnv(func(name string) string { return tt.values[name] })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("IntegrityConfigFromEnv() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestIntegrityConfigValidateRepositoryScanSpec(t *testing.T) {
	cfg := IntegrityConfig{
		PinnedScanTargetsEnabled:  true,
		QualityStateWritesEnabled: true,
		FindingObservationWrites:  true,
		HardenedAnalysisEnabled:   true,
		StrictCompletionEnabled:   true,
		DeepScanEnabled:           true,
	}
	if err := cfg.ValidateRepositoryScanSpec(corev1alpha1.RepositoryScanSpec{
		ValidationMode:            "full",
		AnalysisIsolationPolicy:   "require-hardened",
		CompletionPolicy:          "validated",
		IncrementalBaselinePolicy: "assurance-qualified",
	}); err == nil {
		t.Fatal("ValidateRepositoryScanSpec(validated completion) error = nil")
	}

	if err := cfg.ValidateRepositoryScanSpec(corev1alpha1.RepositoryScanSpec{
		ValidationMode: "full",
		DeepScan:       &corev1alpha1.RepositoryDeepScanSpec{Enabled: true},
	}); err == nil {
		t.Fatal("deep scan without atomic authorization/budget implementation error = nil")
	}

	if err := (IntegrityConfig{}).ValidateRepositoryScanSpec(corev1alpha1.RepositoryScanSpec{
		ValidationMode:   "light",
		CompletionPolicy: "validated",
	}); err == nil {
		t.Fatal("validated completion without gates error = nil")
	}
	if err := cfg.ValidateRepositoryScanSpec(corev1alpha1.RepositoryScanSpec{
		IncrementalBaselinePolicy: "complete-coverage",
	}); err == nil {
		t.Fatal("complete-coverage baseline without identity/backlog support error = nil")
	}
}

func TestIntegrityConfigValidateRepositoryScanSpecRejectsAbbreviatedPinnedRef(t *testing.T) {
	cfg := IntegrityConfig{PinnedScanTargetsEnabled: true}

	for _, ref := range []string{
		"abcdef1",
		strings.Repeat("a", 39),
		strings.Repeat("b", 41),
		strings.Repeat("c", 63),
	} {
		t.Run("reject_"+ref[:min(len(ref), 12)], func(t *testing.T) {
			err := cfg.ValidateRepositoryScanSpec(corev1alpha1.RepositoryScanSpec{Ref: ref})
			if err == nil || !strings.Contains(err.Error(), "abbreviated") {
				t.Fatalf("ValidateRepositoryScanSpec(ref=%q) error = %v, want abbreviated ref rejection", ref, err)
			}
		})
	}

	for _, ref := range []string{
		strings.Repeat("d", 40),
		strings.Repeat("e", 64),
		"refs/heads/abcdef1",
		"refs/tags/abcdef1",
		"feature/abcdef1",
	} {
		t.Run("allow_"+strings.ReplaceAll(ref[:min(len(ref), 20)], "/", "_"), func(t *testing.T) {
			if err := cfg.ValidateRepositoryScanSpec(corev1alpha1.RepositoryScanSpec{Ref: ref}); err != nil {
				t.Fatalf("ValidateRepositoryScanSpec(ref=%q) error = %v, want nil", ref, err)
			}
		})
	}

	if err := (IntegrityConfig{}).ValidateRepositoryScanSpec(corev1alpha1.RepositoryScanSpec{Ref: "abcdef1"}); err != nil {
		t.Fatalf("legacy ref validation error = %v, want compatibility when pinned targets are disabled", err)
	}
}

func TestAnalysisIsolationAnnotations(t *testing.T) {
	agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}}}
	annotations, status, err := AnalysisIsolationAnnotations("require-hardened", agent)
	if err != nil || status != IsolationStatusHardened || annotations["orka.ai/agent-read-only"] != "true" {
		t.Fatalf("annotations/status/error = %#v/%q/%v", annotations, status, err)
	}
	unsupported := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode}}}
	if _, status, err := AnalysisIsolationAnnotations("prefer-hardened", unsupported); err != nil || status != IsolationStatusFallback {
		t.Fatalf("prefer fallback status/error = %q/%v", status, err)
	}
	if _, _, err := AnalysisIsolationAnnotations("require-hardened", unsupported); err == nil {
		t.Fatal("require hardened unsupported error = nil")
	}
}

func TestIncrementalBaselineCommitFailsClosedForUnboundWatermarks(t *testing.T) {
	const (
		completeCoveragePolicy = "complete-coverage"
		assurancePolicy        = "assurance-qualified"
		legacyPolicy           = "legacy-discovery"
		legacyCommit           = "legacy"
	)
	scan := &corev1alpha1.RepositoryScan{
		Spec: corev1alpha1.RepositoryScanSpec{IncrementalBaselinePolicy: completeCoveragePolicy},
		Status: corev1alpha1.RepositoryScanStatus{
			LastProcessedCommit:          legacyCommit,
			LastCompleteCoverageCommit:   "complete",
			LastAssuranceQualifiedCommit: "assured",
		},
	}
	if got := IncrementalBaselineCommit(scan); got != "" {
		t.Fatalf("complete-coverage baseline = %q, want full-scan fallback", got)
	}
	scan.Spec.IncrementalBaselinePolicy = assurancePolicy
	if got := IncrementalBaselineCommit(scan); got != "" {
		t.Fatalf("assurance-qualified baseline = %q, want full-scan fallback", got)
	}
	scan.Spec.IncrementalBaselinePolicy = legacyPolicy
	if got := IncrementalBaselineCommit(scan); got != legacyCommit {
		t.Fatalf("legacy baseline = %q, want %s", got, legacyCommit)
	}
}

func TestValidateRunRepositoryScanIdentityRequiresExactBoundIncarnation(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
		UID:        types.UID("scan-uid"),
		Generation: 7,
	}}
	matching := &store.ScanRun{
		RepositoryScanUID:        "scan-uid",
		RepositoryScanGeneration: 7,
	}
	if err := ValidateRunRepositoryScanIdentity(matching, scan); err != nil {
		t.Fatalf("ValidateRunRepositoryScanIdentity(matching) error = %v, want nil", err)
	}

	tests := []struct {
		name    string
		run     *store.ScanRun
		scan    *corev1alpha1.RepositoryScan
		wantErr string
	}{
		{name: "nil run", scan: scan, wantErr: "required"},
		{name: "nil scan", run: matching, wantErr: "required"},
		{name: "legacy unbound run", run: &store.ScanRun{}, scan: scan, wantErr: "UID is required"},
		{
			name: "blank UID",
			run: &store.ScanRun{
				RepositoryScanUID:        "  ",
				RepositoryScanGeneration: scan.Generation,
			},
			scan: scan, wantErr: "UID is required",
		},
		{
			name: "zero generation",
			run: &store.ScanRun{
				RepositoryScanUID: "scan-uid",
			},
			scan: scan, wantErr: "generation must be positive",
		},
		{
			name: "negative generation",
			run: &store.ScanRun{
				RepositoryScanUID:        "scan-uid",
				RepositoryScanGeneration: -1,
			},
			scan: scan, wantErr: "generation must be positive",
		},
		{
			name: "UID mismatch",
			run: &store.ScanRun{
				RepositoryScanUID:        "other-uid",
				RepositoryScanGeneration: scan.Generation,
			},
			scan: scan, wantErr: "UID changed",
		},
		{
			name: "generation mismatch",
			run: &store.ScanRun{
				RepositoryScanUID:        "scan-uid",
				RepositoryScanGeneration: scan.Generation - 1,
			},
			scan: scan, wantErr: "generation changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRunRepositoryScanIdentity(tt.run, tt.scan)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateRunRepositoryScanIdentity() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
