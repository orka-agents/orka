package security

import (
	"fmt"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	EnvWorkerOutputBindingMode   = "ORKA_SECURITY_WORKER_OUTPUT_BINDING_MODE"
	EnvPinnedScanTargetsEnabled  = "ORKA_SECURITY_PINNED_SCAN_TARGETS_ENABLED"
	EnvQualityStateWritesEnabled = "ORKA_SECURITY_QUALITY_STATE_WRITES_ENABLED"
	EnvFindingObservationWrites  = "ORKA_SECURITY_FINDING_OBSERVATION_WRITES_ENABLED"
	EnvBundleSealingMode         = "ORKA_SECURITY_BUNDLE_SEALING_MODE"
	EnvHardenedAnalysisEnabled   = "ORKA_SECURITY_HARDENED_ANALYSIS_ENABLED"
	EnvStrictCompletionEnabled   = "ORKA_SECURITY_STRICT_COMPLETION_ENABLED"
	EnvDeepScanEnabled           = "ORKA_SECURITY_DEEP_SCAN_ENABLED"
)

type GitRefKind string

const (
	GitRefKindSymbolic            GitRefKind = "symbolic"
	GitRefKindFullObjectID        GitRefKind = "full-object-id"
	GitRefKindAbbreviatedObjectID GitRefKind = "abbreviated-object-id"
)

// ClassifyGitRef distinguishes bare commit object IDs from symbolic refs.
// Explicit refs/heads and refs/tags values remain symbolic even when their
// final path component is all hexadecimal.
func ClassifyGitRef(ref string) GitRefKind {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/") {
		return GitRefKindSymbolic
	}
	for _, ch := range ref {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F') {
			continue
		}
		return GitRefKindSymbolic
	}
	if len(ref) == 40 || len(ref) == 64 {
		return GitRefKindFullObjectID
	}
	return GitRefKindAbbreviatedObjectID
}

// NormalizeFullGitObjectID returns the lowercase full object ID for a bare
// 40- or 64-hex ref. Symbolic and abbreviated refs are not normalized.
func NormalizeFullGitObjectID(ref string) (string, bool) {
	if ClassifyGitRef(ref) != GitRefKindFullObjectID {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(ref)), true
}

type WorkerOutputBindingMode string

const (
	WorkerOutputBindingOff     WorkerOutputBindingMode = "off"
	WorkerOutputBindingAudit   WorkerOutputBindingMode = "audit"
	WorkerOutputBindingEnforce WorkerOutputBindingMode = "enforce"
)

type BundleSealingMode string

const (
	BundleSealingOff     BundleSealingMode = "off"
	BundleSealingShadow  BundleSealingMode = "shadow"
	BundleSealingEnforce BundleSealingMode = "enforce"
)

// IntegrityConfig contains independently reversible rollout gates for the
// repository-security integrity pipeline. Zero values preserve compatibility.
type IntegrityConfig struct {
	WorkerOutputBindingMode   WorkerOutputBindingMode
	PinnedScanTargetsEnabled  bool
	QualityStateWritesEnabled bool
	FindingObservationWrites  bool
	BundleSealingMode         BundleSealingMode
	HardenedAnalysisEnabled   bool
	StrictCompletionEnabled   bool
	DeepScanEnabled           bool
}

func DefaultIntegrityConfig() IntegrityConfig {
	return IntegrityConfig{
		WorkerOutputBindingMode: WorkerOutputBindingAudit,
		BundleSealingMode:       BundleSealingOff,
	}
}

func IntegrityConfigFromEnv(getenv func(string) string) (IntegrityConfig, error) {
	if getenv == nil {
		return DefaultIntegrityConfig(), nil
	}
	cfg := DefaultIntegrityConfig()
	var err error
	if value := strings.TrimSpace(getenv(EnvWorkerOutputBindingMode)); value != "" {
		cfg.WorkerOutputBindingMode, err = ParseWorkerOutputBindingMode(value)
		if err != nil {
			return IntegrityConfig{}, fmt.Errorf("%s: %w", EnvWorkerOutputBindingMode, err)
		}
	}
	if value := strings.TrimSpace(getenv(EnvBundleSealingMode)); value != "" {
		cfg.BundleSealingMode, err = ParseBundleSealingMode(value)
		if err != nil {
			return IntegrityConfig{}, fmt.Errorf("%s: %w", EnvBundleSealingMode, err)
		}
	}
	for name, target := range map[string]*bool{
		EnvPinnedScanTargetsEnabled:  &cfg.PinnedScanTargetsEnabled,
		EnvQualityStateWritesEnabled: &cfg.QualityStateWritesEnabled,
		EnvFindingObservationWrites:  &cfg.FindingObservationWrites,
		EnvHardenedAnalysisEnabled:   &cfg.HardenedAnalysisEnabled,
		EnvStrictCompletionEnabled:   &cfg.StrictCompletionEnabled,
		EnvDeepScanEnabled:           &cfg.DeepScanEnabled,
	} {
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			continue
		}
		parsed, parseErr := strconv.ParseBool(value)
		if parseErr != nil {
			return IntegrityConfig{}, fmt.Errorf("%s must be a boolean: %w", name, parseErr)
		}
		*target = parsed
	}
	if cfg.PinnedScanTargetsEnabled && cfg.WorkerOutputBindingMode != WorkerOutputBindingEnforce {
		return IntegrityConfig{}, fmt.Errorf("pinned scan targets require worker output binding enforce mode")
	}
	if cfg.WorkerOutputBindingMode == WorkerOutputBindingEnforce {
		return IntegrityConfig{}, fmt.Errorf("worker output binding enforce mode remains unavailable until controller-issued unpredictable attempt leases are enabled")
	}
	if cfg.HardenedAnalysisEnabled {
		return IntegrityConfig{}, fmt.Errorf("hardened analysis remains unavailable until run-bound capability snapshots are pinned and revalidated before dispatch")
	}
	if cfg.StrictCompletionEnabled && !cfg.QualityStateWritesEnabled {
		return IntegrityConfig{}, fmt.Errorf("strict completion requires quality state writes")
	}
	if cfg.DeepScanEnabled && !cfg.FindingObservationWrites {
		return IntegrityConfig{}, fmt.Errorf("deep scan requires finding observation writes")
	}
	if cfg.BundleSealingMode != BundleSealingOff && (!cfg.FindingObservationWrites || !cfg.PinnedScanTargetsEnabled || !cfg.QualityStateWritesEnabled) {
		return IntegrityConfig{}, fmt.Errorf("bundle sealing requires pinned targets, quality state writes, and finding observation writes")
	}
	if cfg.BundleSealingMode != BundleSealingOff {
		return IntegrityConfig{}, fmt.Errorf("bundle sealing remains unavailable until persisted frozen input sets and immutable evidence receipts are enabled")
	}
	if cfg.StrictCompletionEnabled {
		return IntegrityConfig{}, fmt.Errorf("strict completion remains unavailable until authorization receipts and bundle freezing are enabled")
	}
	return cfg, nil
}

func ParseWorkerOutputBindingMode(value string) (WorkerOutputBindingMode, error) {
	mode := WorkerOutputBindingMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case WorkerOutputBindingOff, WorkerOutputBindingAudit, WorkerOutputBindingEnforce:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported mode %q (want off, audit, or enforce)", value)
	}
}

func ParseBundleSealingMode(value string) (BundleSealingMode, error) {
	mode := BundleSealingMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case BundleSealingOff, BundleSealingShadow, BundleSealingEnforce:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported mode %q (want off, shadow, or enforce)", value)
	}
}

func EffectiveAnalysisIsolationPolicy(scan *corev1alpha1.RepositoryScan) string {
	if scan == nil || strings.TrimSpace(scan.Spec.AnalysisIsolationPolicy) == "" {
		return IsolationStatusLegacy
	}
	return strings.ToLower(strings.TrimSpace(scan.Spec.AnalysisIsolationPolicy))
}

func EffectiveCompletionPolicy(scan *corev1alpha1.RepositoryScan) string {
	if scan == nil || strings.TrimSpace(scan.Spec.CompletionPolicy) == "" {
		return "discovery"
	}
	return strings.ToLower(strings.TrimSpace(scan.Spec.CompletionPolicy))
}

func EffectiveIncrementalBaselinePolicy(scan *corev1alpha1.RepositoryScan) string {
	if scan == nil || strings.TrimSpace(scan.Spec.IncrementalBaselinePolicy) == "" {
		return "legacy-discovery"
	}
	return strings.ToLower(strings.TrimSpace(scan.Spec.IncrementalBaselinePolicy))
}

// ValidateRepositoryScanSpec rejects policy requests whose rollout gate or
// required companion configuration is unavailable. It never silently downgrades.
func (c IntegrityConfig) ValidateRepositoryScanSpec(spec corev1alpha1.RepositoryScanSpec) error {
	if c.PinnedScanTargetsEnabled && ClassifyGitRef(spec.Ref) == GitRefKindAbbreviatedObjectID {
		return fmt.Errorf(
			"ref %q looks like an abbreviated commit object ID; pinned scan targets require a full 40- or 64-hex object ID or an explicit refs/heads/... or refs/tags/... ref",
			spec.Ref,
		)
	}

	isolation := strings.ToLower(strings.TrimSpace(spec.AnalysisIsolationPolicy))
	switch isolation {
	case "", IsolationStatusLegacy:
	case "prefer-hardened", "require-hardened":
		if !c.HardenedAnalysisEnabled {
			return fmt.Errorf("analysisIsolationPolicy %q requires hardened analysis to be enabled", isolation)
		}
	default:
		return fmt.Errorf("unsupported analysisIsolationPolicy %q", spec.AnalysisIsolationPolicy)
	}

	completion := strings.ToLower(strings.TrimSpace(spec.CompletionPolicy))
	switch completion {
	case "", "discovery":
	case "validated":
		if !c.StrictCompletionEnabled {
			return fmt.Errorf("completionPolicy validated requires strict completion to be enabled")
		}
		if strings.ToLower(strings.TrimSpace(spec.ValidationMode)) != "full" {
			return fmt.Errorf("completionPolicy validated requires validationMode full")
		}
		return fmt.Errorf("completionPolicy validated remains unavailable until authorization receipts and bundle freezing are enabled")
	default:
		return fmt.Errorf("unsupported completionPolicy %q", spec.CompletionPolicy)
	}

	baseline := strings.ToLower(strings.TrimSpace(spec.IncrementalBaselinePolicy))
	switch baseline {
	case "", "legacy-discovery":
	case "complete-coverage":
		return fmt.Errorf("incrementalBaselinePolicy complete-coverage remains unavailable until coverage-baseline identities and retry backlog receipts are enabled")
	case "assurance-qualified":
		return fmt.Errorf("incrementalBaselinePolicy assurance-qualified remains unavailable until strict completion authorization receipts are enabled")
	default:
		return fmt.Errorf("unsupported incrementalBaselinePolicy %q", spec.IncrementalBaselinePolicy)
	}

	if spec.DeepScan != nil && spec.DeepScan.Enabled {
		if !c.DeepScanEnabled {
			return fmt.Errorf("deep scan is not enabled by the controller")
		}
		if !c.PinnedScanTargetsEnabled || !c.QualityStateWritesEnabled || !c.FindingObservationWrites {
			return fmt.Errorf("deep scan requires pinned targets, quality state writes, and finding observation writes")
		}
		return fmt.Errorf("deep scan dispatch remains unavailable until authorization receipts and atomic budgets are enabled")
	}
	return nil
}

const (
	IsolationStatusLegacy   = "legacy"
	IsolationStatusHardened = "hardened"
	IsolationStatusFallback = "fallback"
)

// AnalysisIsolationAnnotations resolves the actual built-in runtime capability
// before dispatch. It never treats an external runtime's self-report as a
// verified hardened profile.
func AnalysisIsolationAnnotations(policy string, agent *corev1alpha1.Agent) (map[string]string, string, error) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		policy = "legacy"
	}
	if policy == "legacy" {
		return map[string]string{"orka.ai/security-isolation-status": IsolationStatusLegacy}, IsolationStatusLegacy, nil
	}

	supported := agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.RuntimeRef == nil &&
		(agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeCodex || agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeClaude)
	if !supported {
		if policy == "prefer-hardened" {
			return map[string]string{"orka.ai/security-isolation-status": IsolationStatusFallback}, IsolationStatusFallback, nil
		}
		return nil, "failed", fmt.Errorf("analysis agent does not support verified hardened execution")
	}
	return map[string]string{
		"orka.ai/agent-read-only":           "true",
		"orka.ai/agent-runtime-auth-only":   "true",
		"orka.ai/security-isolation-status": IsolationStatusHardened,
	}, IsolationStatusHardened, nil
}

func IncrementalBaselineCommit(scan *corev1alpha1.RepositoryScan) string {
	if scan == nil {
		return ""
	}
	switch EffectiveIncrementalBaselinePolicy(scan) {
	case "complete-coverage":
		// A commit alone is not a safe baseline. Until the durable watermark is
		// bound to scope/schema/policy identity and retry backlog receipts, force
		// a full scan instead of silently reusing an incompatible baseline.
		return ""
	case "assurance-qualified":
		return ""
	default:
		return strings.TrimSpace(scan.Status.LastProcessedCommit)
	}
}

func ValidateRunRepositoryScanIdentity(run *store.ScanRun, scan *corev1alpha1.RepositoryScan) error {
	if run == nil || scan == nil {
		return fmt.Errorf("scan run and RepositoryScan are required")
	}
	if strings.TrimSpace(run.RepositoryScanUID) == "" {
		return fmt.Errorf("scan run RepositoryScan UID is required")
	}
	if run.RepositoryScanGeneration <= 0 {
		return fmt.Errorf("scan run RepositoryScan generation must be positive")
	}
	if run.RepositoryScanUID != string(scan.UID) {
		return fmt.Errorf("repository scan UID changed since run creation")
	}
	if run.RepositoryScanGeneration != scan.Generation {
		return fmt.Errorf("repository scan generation changed since run creation")
	}
	return nil
}
