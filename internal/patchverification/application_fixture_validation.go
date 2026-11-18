package patchverification

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

func ApplicationReplicaTargetFor(policy *ApplicationPolicy) *ApplicationFixtureProfile {
	if policy != nil {
		for i := range policy.Fixtures {
			if policy.Fixtures[i].ReplicaTarget != nil {
				return &policy.Fixtures[i]
			}
		}
	}
	return nil
}

func validateApplicationFixtureCapabilities(policy ApplicationPolicy) error {
	replicas := 0
	for _, profile := range policy.Fixtures {
		if target := profile.ReplicaTarget; target != nil {
			replicas++
			if replicas > 1 || !applicationName(target.Name) || strings.HasPrefix(target.Name, "orka-") ||
				target.MaxReplicas < 1 || target.MaxReplicas > 4 || target.InitialReplicas < 0 ||
				target.InitialReplicas > target.MaxReplicas || len(profile.ConfigKeys) != 0 || len(profile.SecretFiles) != 0 {
				return errors.New("replica fixtures require one fixed operator target with finite replica bounds and no model config")
			}
		}
		if len(profile.SecretFiles) > 4 {
			return ErrLimit
		}
		ids := map[string]bool{}
		for _, file := range profile.SecretFiles {
			if !applicationName(file.ID) || ids[file.ID] || !applicationName(file.SecretName) ||
				strings.HasPrefix(file.SecretName, "orka-") || len(kvalidation.IsConfigMapKey(file.Key)) != 0 {
				return ErrEvidence
			}
			ids[file.ID] = true
		}
	}
	if len(policy.SubjectTLS) > 2 {
		return ErrLimit
	}
	ids := map[string]bool{}
	for _, bundle := range policy.SubjectTLS {
		if ids[bundle.ID] {
			return ErrEvidence
		}
		if _, err := ApplicationSubjectTLSDNSNames(bundle, "run-namespace"); err != nil {
			return err
		}
		ids[bundle.ID] = true
	}
	return nil
}

func applicationReplicaRule(rule ApplicationResourceRule) bool {
	return rule.APIVersion == applicationAppsVersion && rule.Kind == "Deployment" && rule.Resource == applicationDeployments
}

func applicationHPARule(rule ApplicationResourceRule) bool {
	return rule.APIVersion == "autoscaling/v2" && rule.Kind == "HorizontalPodAutoscaler" && rule.Resource == "horizontalpodautoscalers"
}

func validateApplicationReplicaRule(rule ApplicationResourceRule, policy *ApplicationPolicy) error {
	if rule.Scale && (!applicationReplicaRule(rule) || !rule.Namespaced || rule.CRD != nil || ApplicationReplicaTargetFor(policy) == nil) {
		return errors.New("scale authority requires the fixed namespaced replica primitive")
	}
	if !applicationReplicaRule(rule) && !applicationHPARule(rule) {
		return nil
	}
	if !rule.Namespaced || rule.CRD != nil || ApplicationReplicaTargetFor(policy) == nil {
		return ErrEvidence
	}
	allowed := []string{ApplicationVerbGet, ApplicationVerbList, ApplicationVerbWatch}
	if applicationHPARule(rule) {
		allowed = append(allowed, ApplicationActionCreate, ApplicationActionUpdate)
	}
	for _, verb := range rule.Verbs {
		if !slices.Contains(allowed, verb) {
			return errors.New("replica authority cannot mutate full Deployment templates or grant unrestricted autoscaling")
		}
	}
	return nil
}

func validateApplicationFixtureSelection(plan ApplicationPlan, policy *ApplicationPolicy) error {
	replicas := 0
	for _, fixture := range plan.Fixtures {
		if fixture.Kind != ApplicationFixtureImage && fixture.Kind != ApplicationFixtureReplica {
			continue
		}
		profile, _ := applicationFixtureProfile(plan, policy, fixture.ID)
		if profile == nil {
			return ErrEvidence
		}
		if fixture.Kind == ApplicationFixtureReplica {
			replicas++
			if replicas > 1 || profile.ReplicaTarget == nil || fixture.ID != profile.ReplicaTarget.Name ||
				len(fixture.Config) != 0 || len(profile.ConfigKeys) != 0 || len(profile.SecretFiles) != 0 {
				return ErrEvidence
			}
		} else if profile.ReplicaTarget != nil {
			return ErrEvidence
		}
		for _, selected := range profile.SecretFiles {
			found := false
			for _, secret := range plan.SyntheticSecrets {
				for _, key := range secret.Keys {
					found = found || secret.Name == selected.SecretName && key.Name == selected.Key
				}
			}
			if !found {
				return errors.New("fixture files may reference only this plan's generated synthetic Secret keys")
			}
		}
	}
	for _, permission := range plan.Permissions {
		if permission.APIGroup == applicationAppsGroup || permission.APIGroup == "autoscaling" {
			if replicas != 1 {
				return errors.New("replica and HPA authority requires the selected fixed operator fixture")
			}
		}
	}
	return nil
}

func validateApplicationReplicaCheck(check Check, policy *ApplicationPolicy) error {
	api := check.Application.Kubernetes
	if api == nil || api.APIVersion != applicationAppsVersion || api.Resource != applicationDeployments {
		return nil
	}
	profile := ApplicationReplicaTargetFor(policy)
	if profile == nil || api.Name != profile.ReplicaTarget.Name ||
		!slices.Contains([]string{"/status/replicas", "/status/readyReplicas", "/status/availableReplicas", "/status/updatedReplicas"}, api.Pointer) {
		return errors.New("replica observations must address the fixed target's actual replica status")
	}
	if check.Kind != Normal {
		return nil
	}
	if len(check.Application.Actions) == 0 {
		return errors.New("normal replica reconciliation requires an actor stimulus")
	}
	for count := int32(1); count <= profile.ReplicaTarget.MaxReplicas; count++ {
		if count == profile.ReplicaTarget.InitialReplicas {
			continue
		}
		raw, _ := json.Marshal(count)
		expected, err := ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: raw})
		if err == nil && expected.Stdout == check.Healthy.Stdout {
			return nil
		}
	}
	return errors.New("normal replica evidence must demonstrate a bounded non-initial positive replica count")
}
