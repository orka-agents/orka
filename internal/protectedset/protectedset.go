// Package protectedset parses and enforces the operator-owned protected set:
// the repository paths, evaluation case sets and gate thresholds that a
// self-improvement proposer can never change or read. It is declared in one
// ConfigMap in the controller namespace. A missing or invalid declaration
// disables proposer flows; it never means that nothing is protected.
package protectedset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/orka-agents/orka/internal/workspacedelta"
)

const (
	// SchemaVersion is the only accepted protected-set schema.
	SchemaVersion = "orka.protectedSet.v1"
	// ConfigMapKey is the ConfigMap data key that holds the declaration.
	ConfigMapKey = "protectedSet.yaml"

	// SplitTrain marks a case set a proposer's summaries may be built from.
	SplitTrain = "train"
	// SplitHeldOut marks a case set no proposer may ever read.
	SplitHeldOut = "heldOut"

	// maxPathsPerRepository matches the Task deniedPaths list bound, so the
	// effective set for any repository, or any source and publication pair,
	// always fits one Task.
	maxPathsPerRepository = 256
	// maxRepositories bounds the repository list so the pairwise limit check
	// stays cheap; a protected set names a handful of repositories.
	maxRepositories     = 64
	maxDeclarationBytes = 256 << 10
)

// ErrUnavailable reports that no valid protected set is available, so
// proposer flows must not run.
var ErrUnavailable = errors.New("protected set is unavailable")

// Set is a validated orka.protectedSet.v1 declaration.
type Set struct {
	SchemaVersion string `json:"schemaVersion"`
	// Paths are denied to proposers in every repository.
	Paths []string `json:"paths"`
	// Repositories add paths for one repository each.
	Repositories []Repository `json:"repositories,omitempty"`
	// CaseSets name the evaluation case ConfigMaps by split.
	CaseSets []CaseSet `json:"caseSets,omitempty"`
	// Gate holds the improvement thresholds.
	Gate Gate `json:"gate"`
}

// Repository adds protected paths for one repository, named by its
// credential-free identity such as github.com/orka-agents/orka.
type Repository struct {
	Repository string   `json:"repository"`
	Paths      []string `json:"paths"`
}

// CaseSet names one evaluation case ConfigMap and its split.
type CaseSet struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Split     string `json:"split"`
	// BaselineMonitors, read by the EvalRun experiment gate (#993), are the
	// RepositoryMonitors in Namespace whose own reviewer may run a heldOut
	// set outside an experiment.
	BaselineMonitors []string `json:"baselineMonitors,omitempty"`
}

// Gate holds the improvement thresholds frozen into experiments. The last
// three fields are read only by the EvalRun experiment gate (#993), which
// requires them; proposer flows run without them.
type Gate struct {
	MaxInfraErrorPercent     *int32 `json:"maxInfraErrorPercent"`
	MaxCandidatesPerBaseline *int32 `json:"maxCandidatesPerBaseline"`
	// MaxExperimentsPerHeldOutSet is the held-out case set's usage budget.
	MaxExperimentsPerHeldOutSet *int32 `json:"maxExperimentsPerHeldOutSet,omitempty"`
	// MinTargetImprovementPoints is how many percentage points the target
	// pass@1 must rise by.
	MinTargetImprovementPoints *int32 `json:"minTargetImprovementPoints,omitempty"`
	// Significance is the one-sided paired test's level before it is divided
	// by MaxExperimentsPerHeldOutSet.
	Significance *float64 `json:"significance,omitempty"`
	// MaxGuardrailDropPoints is how far another grader's pass@1 may fall.
	MaxGuardrailDropPoints *int32 `json:"maxGuardrailDropPoints,omitempty"`
}

// Parse strictly decodes and validates a declaration. Unknown fields are
// rejected so a misspelled key cannot silently drop protection.
func Parse(data []byte) (*Set, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("protected set declaration is empty")
	}
	if len(data) > maxDeclarationBytes {
		return nil, fmt.Errorf("protected set declaration exceeds %d bytes", maxDeclarationBytes)
	}
	var set Set
	if err := sigsyaml.UnmarshalStrict(data, &set); err != nil {
		return nil, fmt.Errorf("decode protected set: %w", err)
	}
	if err := set.normalize(); err != nil {
		return nil, err
	}
	return &set, nil
}

//nolint:gocyclo // Validation keeps every fail-closed rule for the declaration in one place.
func (s *Set) normalize() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("protected set schemaVersion must be %q", SchemaVersion)
	}
	paths, err := normalizePatterns("paths", s.Paths)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("protected set paths must not be empty")
	}
	s.Paths = paths
	if len(s.Repositories) > maxRepositories {
		return fmt.Errorf("protected set repositories exceed %d entries", maxRepositories)
	}
	seenRepositories := map[string]struct{}{}
	for index := range s.Repositories {
		repository := &s.Repositories[index]
		id, err := CanonicalRepositoryID(repository.Repository)
		if err != nil {
			return fmt.Errorf("protected set repositories[%d]: %w", index, err)
		}
		if _, duplicate := seenRepositories[id]; duplicate {
			return fmt.Errorf("protected set repositories[%d]: repository %q is listed twice", index, id)
		}
		seenRepositories[id] = struct{}{}
		repository.Repository = id
		repositoryPaths, err := normalizePatterns(fmt.Sprintf("repositories[%d].paths", index), repository.Paths)
		if err != nil {
			return err
		}
		if len(repositoryPaths) == 0 {
			return fmt.Errorf("protected set repositories[%d].paths must not be empty", index)
		}
		repository.Paths = repositoryPaths
		if len(unionPatterns(s.Paths, repositoryPaths)) > maxPathsPerRepository {
			return fmt.Errorf("protected set for repository %q exceeds %d paths", id, maxPathsPerRepository)
		}
	}
	if len(s.Paths) > maxPathsPerRepository {
		return fmt.Errorf("protected set paths exceed %d entries", maxPathsPerRepository)
	}
	if err := s.checkRepositoryPairLimits(); err != nil {
		return err
	}
	seenCaseSets := map[string]struct{}{}
	for index := range s.CaseSets {
		caseSet := &s.CaseSets[index]
		caseSet.Namespace = strings.TrimSpace(caseSet.Namespace)
		caseSet.Name = strings.TrimSpace(caseSet.Name)
		if errs := validation.IsDNS1123Label(caseSet.Namespace); len(errs) > 0 {
			return fmt.Errorf("protected set caseSets[%d].namespace is invalid: %s", index, strings.Join(errs, "; "))
		}
		if errs := validation.IsDNS1123Subdomain(caseSet.Name); len(errs) > 0 {
			return fmt.Errorf("protected set caseSets[%d].name is invalid: %s", index, strings.Join(errs, "; "))
		}
		if caseSet.Split != SplitTrain && caseSet.Split != SplitHeldOut {
			return fmt.Errorf("protected set caseSets[%d].split must be %q or %q", index, SplitTrain, SplitHeldOut)
		}
		if len(caseSet.BaselineMonitors) > 0 && caseSet.Split != SplitHeldOut {
			return fmt.Errorf("protected set caseSets[%d].baselineMonitors is only for a %q set", index, SplitHeldOut)
		}
		for _, monitor := range caseSet.BaselineMonitors {
			if errs := validation.IsDNS1123Subdomain(monitor); len(errs) > 0 {
				return fmt.Errorf("protected set caseSets[%d].baselineMonitors entry %q is invalid: %s", index, monitor, strings.Join(errs, "; "))
			}
		}
		key := caseSet.Namespace + "/" + caseSet.Name
		if _, duplicate := seenCaseSets[key]; duplicate {
			return fmt.Errorf("protected set caseSets[%d]: %s is listed twice", index, key)
		}
		seenCaseSets[key] = struct{}{}
	}
	if s.Gate.MaxInfraErrorPercent == nil || *s.Gate.MaxInfraErrorPercent < 0 || *s.Gate.MaxInfraErrorPercent > 100 {
		return fmt.Errorf("protected set gate.maxInfraErrorPercent must be set between 0 and 100")
	}
	if s.Gate.MaxCandidatesPerBaseline == nil || *s.Gate.MaxCandidatesPerBaseline < 1 {
		return fmt.Errorf("protected set gate.maxCandidatesPerBaseline must be set and at least 1")
	}
	if v := s.Gate.MaxExperimentsPerHeldOutSet; v != nil && *v < 1 {
		return fmt.Errorf("protected set gate.maxExperimentsPerHeldOutSet must be at least 1")
	}
	if v := s.Gate.MinTargetImprovementPoints; v != nil && (*v < 0 || *v > 100) {
		return fmt.Errorf("protected set gate.minTargetImprovementPoints must be between 0 and 100")
	}
	if v := s.Gate.Significance; v != nil && (*v <= 0 || *v > 0.5) {
		return fmt.Errorf("protected set gate.significance must be greater than 0 and at most 0.5")
	}
	if v := s.Gate.MaxGuardrailDropPoints; v != nil && (*v < 0 || *v > 100) {
		return fmt.Errorf("protected set gate.maxGuardrailDropPoints must be between 0 and 100")
	}
	return nil
}

// checkRepositoryPairLimits rejects a declaration whose global paths plus the
// paths of any two repositories exceed one Task's deniedPaths bound. A Task
// whose source and publication repositories differ must cover both, so every
// such pair has to fit one Task.
func (s *Set) checkRepositoryPairLimits() error {
	extras := make([]int, len(s.Repositories))
	for index, repository := range s.Repositories {
		extras[index] = len(unionPatterns(s.Paths, repository.Paths)) - len(s.Paths)
	}
	global := make(map[string]struct{}, len(s.Paths))
	for _, pattern := range s.Paths {
		global[pattern] = struct{}{}
	}
	// extraSets holds each repository's patterns beyond the global ones, so a
	// pair is checked by counting shared extras with map lookups rather than
	// building and sorting a full union for every pair.
	extraSets := make([]map[string]struct{}, len(s.Repositories))
	for index, repository := range s.Repositories {
		extraSets[index] = make(map[string]struct{}, extras[index])
		for _, pattern := range repository.Paths {
			if _, ok := global[pattern]; !ok {
				extraSets[index][pattern] = struct{}{}
			}
		}
	}
	for i := range s.Repositories {
		for j := i + 1; j < len(s.Repositories); j++ {
			if len(s.Paths)+extras[i]+extras[j] <= maxPathsPerRepository {
				continue
			}
			shared := 0
			for pattern := range extraSets[i] {
				if _, ok := extraSets[j][pattern]; ok {
					shared++
				}
			}
			if len(s.Paths)+extras[i]+extras[j]-shared > maxPathsPerRepository {
				return fmt.Errorf("protected set for repositories %q and %q together exceeds %d paths",
					s.Repositories[i].Repository, s.Repositories[j].Repository, maxPathsPerRepository)
			}
		}
	}
	return nil
}

func normalizePatterns(field string, patterns []string) ([]string, error) {
	result := make([]string, 0, len(patterns))
	for index, pattern := range patterns {
		if !workspacedelta.ValidDeniedPathPattern(pattern) {
			return nil, fmt.Errorf("protected set %s[%d] is not a valid path pattern", field, index)
		}
		result = append(result, normalizePattern(pattern))
	}
	return unionPatterns(result), nil
}

func normalizePattern(pattern string) string {
	return strings.TrimPrefix(strings.TrimSpace(pattern), "./")
}

func unionPatterns(groups ...[]string) []string {
	var result []string
	seen := map[string]struct{}{}
	for _, group := range groups {
		for _, pattern := range group {
			if _, ok := seen[pattern]; ok {
				continue
			}
			seen[pattern] = struct{}{}
			result = append(result, pattern)
		}
	}
	slices.Sort(result)
	return result
}

// CanonicalRepositoryID normalizes a repository identity to host/path form:
// it drops an https:// scheme and a .git suffix, lowercases the host, and
// lowercases the whole identity on github.com, whose repository names are
// case-insensitive. It matches the identity Orka derives from a Task's
// workspace repository URL.
func CanonicalRepositoryID(value string) (string, error) {
	id := strings.TrimSpace(value)
	id = strings.TrimPrefix(id, "https://")
	id = strings.TrimSuffix(strings.TrimSuffix(id, "/"), ".git")
	host, repositoryPath, ok := strings.Cut(id, "/")
	if !ok || host == "" || repositoryPath == "" || strings.ContainsAny(id, " \t\\?#@:") ||
		strings.Contains(id, "//") || strings.Contains("/"+repositoryPath+"/", "/../") || strings.Contains("/"+repositoryPath+"/", "/./") {
		// The rejected value is omitted: it may carry credentials, and this
		// error reaches controller logs and Task status.
		return "", errors.New("repository must be a host/owner/name identity")
	}
	host = strings.ToLower(host)
	if host == "github.com" {
		repositoryPath = strings.ToLower(repositoryPath)
		// A GitHub repository is exactly owner/name; an owner alone would
		// match no workspace and protect nothing.
		if owner, name, ok := strings.Cut(repositoryPath, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return "", errors.New("a github.com repository must be github.com/owner/name")
		}
	}
	return host + "/" + repositoryPath, nil
}

// Digest is the SHA-256 of the canonical JSON form of the validated set.
func (s *Set) Digest() string {
	encoded, err := json.Marshal(s)
	if err != nil {
		// A validated Set holds only strings and integers.
		panic(fmt.Sprintf("encode protected set: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DeniedPathsFor returns the sorted protected path patterns that apply to a
// Task touching the given repositories: the global paths plus each listed
// repository's paths.
func (s *Set) DeniedPathsFor(repositoryIDs ...string) []string {
	groups := [][]string{s.Paths}
	for _, raw := range repositoryIDs {
		id, err := CanonicalRepositoryID(raw)
		if err != nil {
			continue
		}
		for _, repository := range s.Repositories {
			if repository.Repository == id {
				groups = append(groups, repository.Paths)
			}
		}
	}
	return unionPatterns(groups...)
}

// MissingDeniedPaths returns the protected patterns for the repositories that
// denied does not list exactly. Coverage is by exact pattern, never by glob
// subsumption, so a Task covers the protected set only by naming each entry.
func (s *Set) MissingDeniedPaths(denied []string, repositoryIDs ...string) []string {
	have := make(map[string]struct{}, len(denied))
	for _, pattern := range denied {
		have[normalizePattern(pattern)] = struct{}{}
	}
	var missing []string
	for _, pattern := range s.DeniedPathsFor(repositoryIDs...) {
		if _, ok := have[pattern]; !ok {
			missing = append(missing, pattern)
		}
	}
	return missing
}

// Source reads the protected set from its ConfigMap on every call, so a
// change takes effect on the next check without a controller restart.
type Source struct {
	Reader    client.Reader
	Namespace string
	Name      string
}

// Configured reports whether a ConfigMap has been named.
func (s Source) Configured() bool {
	return s.Reader != nil && strings.TrimSpace(s.Namespace) != "" && strings.TrimSpace(s.Name) != ""
}

// Load returns the validated protected set. A missing, unconfigured or invalid
// declaration wraps ErrUnavailable: proposer flows stay disabled until a valid
// set exists. Other read errors are returned unwrapped so callers can retry.
func (s Source) Load(ctx context.Context) (*Set, error) {
	if !s.Configured() {
		return nil, fmt.Errorf("%w: no protected-set ConfigMap is configured", ErrUnavailable)
	}
	var configMap corev1.ConfigMap
	key := types.NamespacedName{Namespace: strings.TrimSpace(s.Namespace), Name: strings.TrimSpace(s.Name)}
	if err := s.Reader.Get(ctx, key, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: ConfigMap %s does not exist", ErrUnavailable, key)
		}
		return nil, fmt.Errorf("read protected-set ConfigMap %s: %w", key, err)
	}
	data, ok := configMap.Data[ConfigMapKey]
	if !ok {
		return nil, fmt.Errorf("%w: ConfigMap %s has no %s key", ErrUnavailable, key, ConfigMapKey)
	}
	set, err := Parse([]byte(data))
	if err != nil {
		return nil, fmt.Errorf("%w: ConfigMap %s: %w", ErrUnavailable, key, err)
	}
	return set, nil
}
