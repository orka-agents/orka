package patchverification

import (
	"fmt"
	"strings"

	verificationv1 "github.com/orka-agents/orka/pkg/storage/v1/verification"
)

type DeclaredChange = verificationv1.DeclaredChange

type EnvironmentRequirement = verificationv1.EnvironmentRequirement

type EarlierValidationReference = verificationv1.EarlierValidationReference

func validateManifestMetadata(manifest Manifest) error {
	if err := validateDeclaredChanges(manifest.Action, manifest.DeclaredChanges); err != nil {
		return err
	}
	if err := validateRequirements(manifest.Environment.Requirements); err != nil {
		return err
	}
	if reference := manifest.EarlierValidation; reference != nil {
		if manifest.Action != VerifyPatch || !identifierPattern.MatchString(reference.RunID) ||
			reference.ReportDigest != manifest.ReportDigest || !sha256Pattern.MatchString(reference.ManifestDigest) ||
			!sha256Pattern.MatchString(reference.SealDigest) {
			return fmt.Errorf("invalid earlier report validation reference")
		}
	}
	return nil
}

func validateDeclaredChanges(action Action, changes []DeclaredChange) error {
	if action == ValidateReport && len(changes) != 0 {
		return fmt.Errorf("report validation cannot declare patch changes")
	}
	if action == VerifyPatch && len(changes) == 0 {
		return fmt.Errorf("explicit patch verification requires declared changes")
	}
	if len(changes) > 100 {
		return fmt.Errorf("too many declared changes")
	}
	for _, change := range changes {
		switch change.Kind {
		case "source", "dependency", "deployment", "configuration", "permission":
		default:
			return fmt.Errorf("unsupported declared change kind")
		}
		if strings.TrimSpace(change.Description) == "" || len(change.Description) > 4096 || len(change.Paths) > 128 {
			return fmt.Errorf("declared changes require bounded descriptions and paths")
		}
		for _, name := range change.Paths {
			if !sourceValidPath(name) {
				return fmt.Errorf("declared change paths must be safe repository-relative paths")
			}
		}
	}
	return nil
}

func validateRequirements(requirements []EnvironmentRequirement) error {
	if len(requirements) > 32 {
		return fmt.Errorf("too many environment requirements")
	}
	seen := make(map[string]bool)
	for _, requirement := range requirements {
		switch requirement.Kind {
		case "process", "local-services", "cluster", "controller", "external-service", "test-identity":
		default:
			return fmt.Errorf("unsupported environment requirement kind")
		}
		key := requirement.Kind + ":" + requirement.Name
		if !identifierPattern.MatchString(requirement.Name) || seen[key] || len(requirement.Description) > 4096 {
			return fmt.Errorf("environment requirements need unique names and bounded descriptions")
		}
		seen[key] = true
	}
	return nil
}

func MissingRequirements(environment Environment) []string {
	var missing []string
	for _, requirement := range environment.Requirements {
		available := requirement.Kind == "process"
		if environment.Profile == ApplicationRuntimeProfile && environment.Dependencies[ApplicationPolicyKey] == ApplicationPolicyVersion {
			available = available ||
				(requirement.Kind == "cluster" && requirement.Name == "disposable-target") ||
				(requirement.Kind == "controller" && requirement.Name == "application") ||
				(requirement.Kind == "test-identity" && requirement.Name == "application")
		}
		if requirement.Kind == LocalServices && environment.Profile == LocalServices {
			for _, service := range environment.Services {
				available = available || service.ID == requirement.Name
			}
		}
		if available {
			continue
		}
		missing = append(missing, requirement.Kind+" "+requirement.Name+" is unavailable in this local setup")
	}
	return missing
}

func validateLifecycle(lifecycle []string) error {
	if len(lifecycle) > 32 {
		return fmt.Errorf("too many lifecycle steps")
	}
	for _, step := range lifecycle {
		switch step {
		case "install", "reconcile", "restart", "upgrade":
		default:
			return fmt.Errorf("unsupported lifecycle step")
		}
	}
	return nil
}
