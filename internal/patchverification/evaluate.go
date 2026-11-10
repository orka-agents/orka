package patchverification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"
	"unicode/utf8"
)

type Conclusion = verificationv1.Conclusion

type Action = verificationv1.Action

const (
	ValidateReport   Action     = verificationv1.ValidateReport
	VerifyPatch      Action     = verificationv1.VerifyPatch
	Reproduced       Conclusion = verificationv1.Reproduced
	NotReproduced    Conclusion = verificationv1.NotReproduced
	UnableToValidate Conclusion = verificationv1.UnableToValidate
	Verified         Conclusion = verificationv1.Verified
	NotFixed         Conclusion = verificationv1.NotFixed
	PartiallyFixed   Conclusion = verificationv1.PartiallyFixed
	Regression       Conclusion = verificationv1.Regression
	UnableToVerify   Conclusion = verificationv1.UnableToVerify
	Original                    = verificationv1.Original
	Patched                     = verificationv1.Patched
	Reproduction                = verificationv1.Reproduction
	Normal                      = verificationv1.Normal
	Offline                     = verificationv1.Offline
	LocalServices               = verificationv1.LocalServices
	SchemaVersion               = verificationv1.SchemaVersion
)

type Expectation = verificationv1.Expectation

type Check = verificationv1.Check

type FrozenFile = verificationv1.FrozenFile

type SourceIdentity = verificationv1.SourceIdentity

type Sources = verificationv1.Sources

type Service = verificationv1.Service

type Environment = verificationv1.Environment

type Manifest = verificationv1.Manifest

type Binding = verificationv1.Binding

type CapturedOutput = verificationv1.CapturedOutput

type Observation = verificationv1.Observation

type CheckResult = verificationv1.CheckResult

type ExecutionEvidence = verificationv1.ExecutionEvidence

type Assessment = verificationv1.Assessment

var identifierPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var sha256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var gitIdentityPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

func Digest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func ManifestDigest(manifest Manifest) (string, error) {
	if !validManifestUTF8(reflect.ValueOf(manifest)) {
		return "", fmt.Errorf("manifest strings must be valid UTF-8")
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode frozen manifest: %w", err)
	}
	return Digest(content), nil
}

func validManifestUTF8(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		return value.IsNil() || validManifestUTF8(value.Elem())
	case reflect.String:
		return utf8.ValidString(value.String())
	case reflect.Struct:
		for _, field := range value.Fields() {
			if !validManifestUTF8(field) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return true
		}
		for index := 0; index < value.Len(); index++ {
			if !validManifestUTF8(value.Index(index)) {
				return false
			}
		}
	case reflect.Map:
		entries := value.MapRange()
		for entries.Next() {
			if !validManifestUTF8(entries.Key()) || !validManifestUTF8(entries.Value()) {
				return false
			}
		}
	}
	return true
}

func ValidateManifest(manifest Manifest) error {
	if !validManifestUTF8(reflect.ValueOf(manifest)) {
		return fmt.Errorf("manifest strings must be valid UTF-8")
	}
	if manifest.Version != SchemaVersion || strings.TrimSpace(manifest.Problem) == "" || len(manifest.Scope) == 0 {
		return fmt.Errorf("version, problem description, and tested scope are required")
	}
	if len(manifest.Checks) == 0 || len(manifest.Checks) > 100 {
		return fmt.Errorf("between 1 and 100 frozen checks are required")
	}
	if manifest.Action != "" && manifest.Action != ValidateReport && manifest.Action != VerifyPatch {
		return fmt.Errorf("unsupported validation action")
	}
	if manifest.Action != "" || manifest.ReportDigest != "" {
		digest, err := StableReportDigest(manifest.Problem, manifest.Scope)
		if err != nil || manifest.ReportDigest != digest {
			return fmt.Errorf("report digest does not match the frozen problem and scope")
		}
	}
	if err := validateActionSources(manifest); err != nil {
		return err
	}
	if err := validateManifestMetadata(manifest); err != nil {
		return err
	}
	if err := validateApplicationManifest(manifest); err != nil {
		return err
	}
	if manifest.Application != nil {
		return nil
	}
	if err := validateHTTPManifest(manifest); err != nil {
		return err
	}
	protectedHTTP := manifest.Checks[0].HTTP != nil
	services, err := validateManifestEnvironment(manifest.Environment, protectedHTTP)
	if err != nil {
		return err
	}
	if err := validateManifestChecks(manifest.Checks, services, ActionOrDefault(manifest.Action)); err != nil {
		return err
	}
	for _, file := range manifest.Files {
		if file.Digest != Digest(file.Content) {
			return fmt.Errorf("frozen file digest mismatch")
		}
	}
	return nil
}

func validateActionSources(manifest Manifest) error {
	if err := validateScopedSourcePaths(manifest.Sources.Paths); err != nil {
		return err
	}
	if manifest.Action != ValidateReport {
		return validateManifestSources(manifest.Sources)
	}
	sources := manifest.Sources
	if sources.Repository == "" || !gitIdentityPattern.MatchString(sources.Original.Commit) ||
		!gitIdentityPattern.MatchString(sources.Original.Tree) || !sha256Pattern.MatchString(sources.Original.ArchiveDigest) {
		return fmt.Errorf("report validation requires an exact original source identity")
	}
	if sources.Patched != (SourceIdentity{}) || sources.PatchDigest != "" || sources.DiffDigest != "" {
		return fmt.Errorf("report validation cannot contain a patch")
	}
	return nil
}

func validateScopedSourcePaths(paths []string) error {
	if len(paths) > 32 {
		return fmt.Errorf("at most 32 scoped source paths are supported")
	}
	previous := ""
	for i, name := range paths {
		if !sourceValidPath(name) || name <= previous {
			return fmt.Errorf("scoped source paths must be sorted, unique, safe relative paths")
		}
		for j := range i {
			if sourcePathsOverlap(paths[j], name) {
				return fmt.Errorf("scoped source paths must not overlap")
			}
		}
		previous = name
	}
	return nil
}

func validateManifestSources(sources Sources) error {
	if sources.Repository == "" || !gitIdentityPattern.MatchString(sources.Original.Commit) || !sha256Pattern.MatchString(sources.DiffDigest) {
		return fmt.Errorf("exact original commit, repository, and source diff digest are required")
	}
	for _, source := range []SourceIdentity{sources.Original, sources.Patched} {
		if !gitIdentityPattern.MatchString(source.Tree) || !sha256Pattern.MatchString(source.ArchiveDigest) {
			return fmt.Errorf("exact source trees and archive digests are required")
		}
	}
	if (sources.Patched.Commit == "") == (sources.PatchDigest == "") {
		return fmt.Errorf("exactly one patched commit or patch digest is required")
	}
	if sources.Patched.Commit != "" && !gitIdentityPattern.MatchString(sources.Patched.Commit) {
		return fmt.Errorf("patched commit must be an exact git identity")
	}
	if sources.PatchDigest != "" && !sha256Pattern.MatchString(sources.PatchDigest) {
		return fmt.Errorf("patch digest must be SHA-256")
	}
	return nil
}

func validateManifestEnvironment(environment Environment, protectedHTTP bool) (map[string]bool, error) {
	imageParts := strings.Split(environment.Image, "@")
	if len(imageParts) != 2 || imageParts[0] == "" || !sha256Pattern.MatchString(imageParts[1]) || !validResolvedImageID(environment.ImageID) || environment.Platform == "" {
		return nil, fmt.Errorf("digest-pinned tool image, resolved image identity, and platform are required")
	}
	if environment.Profile != Offline && environment.Profile != LocalServices {
		return nil, fmt.Errorf("unsupported test network profile")
	}
	if environment.Profile == Offline && len(environment.Services) != 0 {
		return nil, fmt.Errorf("offline profile cannot provide network services")
	}
	services := make(map[string]bool)
	ports := make(map[int]bool)
	for _, service := range environment.Services {
		if !identifierPattern.MatchString(service.ID) || services[service.ID] || service.Port < 1024 || service.Port > 65535 || ports[service.Port] || len(service.Command) == 0 || service.Command[0] == "" || service.ReadyOutput == "" || len(service.ReadyOutput) > 1024 {
			return nil, fmt.Errorf("invalid service identity, command, readiness output, or unprivileged port")
		}
		services[service.ID], ports[service.Port] = true, true
	}
	if environment.Profile == LocalServices && len(services) == 0 && !protectedHTTP {
		return nil, fmt.Errorf("local-services profile requires a frozen service")
	}
	return services, nil
}

func validResolvedImageID(imageID string) bool {
	if sha256Pattern.MatchString(imageID) {
		return true
	}
	_, digest, found := strings.Cut(imageID, "@")
	return found && sha256Pattern.MatchString(digest) && !strings.ContainsAny(imageID, "\r\n\x00")
}

func validateManifestChecks(checks []Check, services map[string]bool, action Action) error {
	seen := make(map[string]bool)
	reproductions, normal := 0, 0
	for _, check := range checks {
		if err := validateLifecycle(check.Lifecycle); err != nil {
			return err
		}
		command := CheckExecutable(check)
		if !identifierPattern.MatchString(check.ID) || seen[check.ID] || len(command) == 0 || command[0] == "" || check.TimeoutSeconds < 1 || check.TimeoutSeconds > 300 {
			return fmt.Errorf("invalid or duplicate check identity, command, or timeout")
		}
		seen[check.ID] = true
		switch check.Kind {
		case Reproduction:
			reproductions++
		case Normal:
			normal++
		default:
			return fmt.Errorf("unsupported check kind")
		}
		if reflect.DeepEqual(check.Healthy, check.Failure) || check.Healthy.ExitCode < 0 || check.Healthy.ExitCode > 124 || check.Failure.ExitCode < 0 || check.Failure.ExitCode > 124 || len(check.Healthy.Stdout) > 8192 || len(check.Failure.Stdout) > 8192 {
			return fmt.Errorf("checks require distinct bounded healthy and failure observations without reserved exit codes")
		}
		for _, expectation := range []Expectation{check.Healthy, check.Failure} {
			if len(expectation.Services) != len(services) {
				return fmt.Errorf("every expectation must cover all frozen service observations")
			}
			for serviceID, output := range expectation.Services {
				if !services[serviceID] || len(output) > 8192 {
					return fmt.Errorf("unknown service or oversized expected output")
				}
			}
		}
	}
	if reproductions == 0 || (action == VerifyPatch && normal == 0) {
		return fmt.Errorf("both reproduction and normal-use checks are required")
	}
	return nil
}

func ValidateObservation(manifest Manifest, binding Binding, observation Observation) error {
	return validateObservation(manifest, binding, observation, false)
}

// ValidateApplicationWorkerObservation validates a transport report, not final
// evidence. Only fixed replica descendant identity may await API enrichment by
// the authoritative backend; ValidateObservation remains mandatory for storage.
func ValidateApplicationWorkerObservation(manifest Manifest, binding Binding, observation Observation) error {
	if manifest.Application == nil {
		return ErrEvidence
	}
	return validateObservation(manifest, binding, observation, true)
}

func validateObservation(manifest Manifest, binding Binding, observation Observation, pendingReplica bool) error {
	digest, err := ManifestDigest(manifest)
	if err != nil {
		return err
	}
	if err := ValidateBinding(manifest.Action, binding); err != nil || digest != binding.ManifestDigest {
		return fmt.Errorf("invalid run or attempt binding")
	}
	expectedTask, expectedTree := binding.OriginalTaskID, manifest.Sources.Original.Tree
	if observation.Side == Patched {
		if manifest.Action == ValidateReport {
			return fmt.Errorf("report validation cannot accept patched evidence")
		}
		expectedTask, expectedTree = binding.PatchedTaskID, manifest.Sources.Patched.Tree
	} else if observation.Side != Original {
		return fmt.Errorf("unknown source side")
	}
	expectedImage, expectedOrigin := manifest.Environment.ImageID, "runner"
	if manifest.Application != nil {
		expectedImage, expectedOrigin = manifest.Environment.Dependencies[ApplicationObserverImageIDKey], ApplicationObserverOrigin
	}
	if observation.RunID != binding.RunID || observation.AttemptID != binding.AttemptID || observation.TaskID != expectedTask || observation.ManifestDigest != binding.ManifestDigest || observation.SourceTree != expectedTree || observation.ImageID != expectedImage || observation.Origin != expectedOrigin {
		return fmt.Errorf("observation does not match the frozen run, task, attempt, source, image, and runner identities")
	}
	for _, check := range manifest.Checks {
		if check.ID == observation.CheckID {
			if manifest.Application != nil {
				return validateApplicationObservation(manifest, binding, check, observation, pendingReplica)
			}
			if observation.Application != nil {
				return fmt.Errorf("source-harness evidence cannot claim application-runtime provenance")
			}
			if check.HTTP == nil && observation.HTTPCompleted {
				return fmt.Errorf("a command-only check cannot claim protected HTTP completion")
			}
			return nil
		}
	}
	return fmt.Errorf("observation names an unknown check")
}

func Evaluate(manifest Manifest, binding Binding, observations []Observation) Assessment {
	if manifest.Action == ValidateReport {
		return evaluateReport(manifest, binding, observations)
	}
	if manifest.Action == VerifyPatch {
		return evaluateVerification(manifest, binding, observations)
	}
	assessment := Assessment{Conclusion: UnableToVerify, Reason: "required evidence is missing"}
	if err := ValidateManifest(manifest); err != nil {
		assessment.Reason = err.Error()
		return assessment
	}
	byCheck := make(map[string]Observation)
	for _, observation := range observations {
		if err := ValidateObservation(manifest, binding, observation); err != nil {
			assessment.Reason = err.Error()
			return assessment
		}
		key := observation.Side + ":" + observation.CheckID
		if previous, found := byCheck[key]; found && !reflect.DeepEqual(previous, observation) {
			assessment.Reason = "conflicting observations for the same check"
			return assessment
		}
		byCheck[key] = observation
	}
	fixed, unfixed, regressions, incomplete := 0, 0, 0, false
	for _, check := range manifest.Checks {
		original, hasOriginal := byCheck[Original+":"+check.ID]
		patched, hasPatched := byCheck[Patched+":"+check.ID]
		result := CheckResult{CheckID: check.ID, Outcome: "unable"}
		switch {
		case !hasOriginal || !hasPatched:
			result.Reason = "required check is missing from one or both versions"
		case !usableCheck(check, original) || !usableCheck(check, patched):
			result.Reason = "check was not executed completely, or setup, timeout, skip, or output capture failed"
		case check.Kind == Reproduction && !matches(original, check.Failure):
			result.Reason = "original did not reproduce the stated problem"
		case check.Kind == Normal && !matches(original, check.Healthy):
			result.Reason = "required normal behavior was not established on the original"
		case matches(patched, check.Healthy):
			result.Outcome = OutcomePassed
			if check.Kind == Reproduction {
				fixed++
			}
		case matches(patched, check.Failure):
			if check.Kind == Reproduction {
				result.Outcome = OutcomeNotFixed
				unfixed++
			} else {
				result.Outcome = OutcomeRegression
				regressions++
			}
		default:
			result.Reason = "observed output and exit code match neither frozen expectation"
		}
		incomplete = incomplete || result.Outcome == "unable"
		assessment.Checks = append(assessment.Checks, result)
	}
	switch {
	case incomplete:
		assessment.Reason = "one or more required comparisons could not be verified"
	case regressions > 0:
		assessment.Conclusion, assessment.Reason = Regression, "required normal behavior fails on the patch"
	case fixed > 0 && unfixed > 0:
		assessment.Conclusion, assessment.Reason = PartiallyFixed, "some demonstrated cases remain unfixed"
	case unfixed > 0:
		assessment.Conclusion, assessment.Reason = NotFixed, "all demonstrated cases remain unfixed"
	default:
		assessment.Conclusion, assessment.Reason = Verified, "all stated problems reproduced on the original; patched checks and required normal behavior passed"
	}
	return assessment
}

func usable(observation Observation) bool {
	return observation.Executed && observation.ContainerID != "" && !observation.StartedAt.IsZero() && !observation.FinishedAt.Before(observation.StartedAt) && observation.ExitCode != nil && *observation.ExitCode >= 0 && *observation.ExitCode < 125 && sha256Pattern.MatchString(observation.StdoutDigest) && observation.StdoutBytes >= 0 && sha256Pattern.MatchString(observation.StderrDigest) && observation.StderrBytes >= 0 && !observation.TimedOut && !observation.Skipped && !observation.OutputTruncated && observation.SetupError == ""
}

func usableCheck(check Check, observation Observation) bool {
	return usable(observation) && (check.HTTP == nil || observation.HTTPCompleted) &&
		(check.Application == nil || observation.Application != nil && observation.Application.Completed)
}

func matches(observation Observation, expectation Expectation) bool {
	if *observation.ExitCode != expectation.ExitCode || observation.StdoutBytes != len(expectation.Stdout) || observation.StdoutDigest != Digest([]byte(expectation.Stdout)) || len(observation.ServiceOutputs) != len(expectation.Services) {
		return false
	}
	for serviceID, expected := range expectation.Services {
		observed, found := observation.ServiceOutputs[serviceID]
		if !found || observed.Truncated || observed.Bytes != len(expected) || observed.Digest != Digest([]byte(expected)) {
			return false
		}
	}
	return true
}
