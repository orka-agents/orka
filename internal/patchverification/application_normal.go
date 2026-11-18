package patchverification

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Replica controls already prove a non-initial count through their independently
// verified descendants. Other API controls need an exact initial API revision.
func ApplicationUsesAPINormalTransition(check Check, policy *ApplicationPolicy) bool {
	if check.Kind != Normal || check.Application == nil || check.Application.Kubernetes == nil {
		return false
	}
	api := check.Application.Kubernetes
	profile := ApplicationReplicaTargetFor(policy)
	return profile == nil || api.APIVersion != applicationAppsVersion || api.Resource != applicationDeployments ||
		api.Name != profile.ReplicaTarget.Name
}

func validateApplicationNormalValue(check Check, policy *ApplicationPolicy) error {
	api := check.Application.Kubernetes
	if api == nil {
		return nil
	}
	if !ApplicationUsesAPINormalTransition(check, policy) {
		if len(api.NormalValue) != 0 {
			return errors.New("only non-replica API normal controls declare normalValue")
		}
		return nil
	}
	if len(api.NormalValue) == 0 || len(api.NormalValue) > 16<<10 || ContainsCredentialMaterial(api.NormalValue) {
		return errors.New("API normal controls require a bounded non-vacuous normalValue")
	}
	decoder := json.NewDecoder(bytes.NewReader(api.NormalValue))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || !applicationMeaningfulValue(value, 0) {
		return errors.New("API normal controls cannot use absent, null or empty functionality")
	}
	expected, err := ApplicationAPIExpectation(ApplicationAPIValue{Exists: true, Found: true, Value: api.NormalValue})
	if err != nil || expected.ExitCode != check.Healthy.ExitCode || expected.Stdout != check.Healthy.Stdout ||
		len(check.Healthy.Services) != 0 {
		return errors.New("normalValue must bind the exact existing, found healthy API observation")
	}
	return nil
}

func applicationMeaningfulValue(value any, depth int) bool {
	if depth > 16 {
		return false
	}
	switch typed := value.(type) {
	case bool, json.Number:
		return true
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		for _, child := range typed {
			if applicationMeaningfulValue(child, depth+1) {
				return true
			}
		}
	case map[string]any:
		for _, child := range typed {
			if applicationMeaningfulValue(child, depth+1) {
				return true
			}
		}
	}
	return false
}

func validateApplicationNormalTarget(manifest Manifest, check Check) error {
	if !ApplicationUsesAPINormalTransition(check, manifest.ApplicationPolicy) {
		return nil
	}
	api := check.Application.Kubernetes
	declared := false
	for _, resources := range [][]json.RawMessage{manifest.Application.Resources, check.Application.Resources} {
		for _, resource := range resources {
			object, err := ValidateApplicationResourceWithPolicy(resource, manifest.ApplicationPolicy)
			if err != nil {
				return err
			}
			rule, err := ApplicationResourceRuleFor(manifest.ApplicationPolicy, object.GetAPIVersion(), object.GetKind())
			declared = declared || err == nil && api.APIVersion == object.GetAPIVersion() &&
				api.Resource == rule.Resource && api.Name == object.GetName()
		}
	}
	for _, action := range check.Application.Actions {
		declared = declared || ApplicationActionObservesAPI(check, action, manifest.ApplicationPolicy)
	}
	if !declared || !applicationSubjectWritesNormalStatus(*manifest.Application, *api) {
		return errors.New("API normal controls need an owned declared target and admitted subject status-write authority")
	}
	return nil
}

func applicationSubjectWritesNormalStatus(plan ApplicationPlan, api ApplicationKubernetesCheck) bool {
	version, err := schema.ParseGroupVersion(api.APIVersion)
	if err != nil {
		return false
	}
	for _, permission := range plan.Permissions {
		if permission.APIGroup == version.Group && slices.Contains(permission.Resources, api.Resource+"/status") &&
			(slices.Contains(permission.Verbs, ApplicationActionUpdate) || slices.Contains(permission.Verbs, ApplicationVerbPatch)) &&
			(len(permission.ResourceNames) == 0 || slices.Contains(permission.ResourceNames, api.Name)) {
			return true
		}
	}
	return false
}

func ApplicationActionObservesAPI(check Check, action ApplicationAction, policy *ApplicationPolicy) bool {
	if !ApplicationUsesAPINormalTransition(check, policy) ||
		(action.ExpectedStatus != 200 && action.ExpectedStatus != 201) {
		return false
	}
	object, err := ValidateApplicationResourceWithPolicy(action.Resource, policy)
	if err != nil {
		return false
	}
	rule, err := ApplicationResourceRuleFor(policy, object.GetAPIVersion(), object.GetKind())
	api := check.Application.Kubernetes
	return err == nil && object.GetAPIVersion() == api.APIVersion && rule.Resource == api.Resource && object.GetName() == api.Name
}

func ValidateApplicationAPIBaseline(check Check, baseline ApplicationAPIIdentity) error {
	if check.Application == nil {
		return ErrBinding
	}
	api := check.Application.Kubernetes
	if api == nil || baseline.APIVersion != api.APIVersion || baseline.Resource != api.Resource ||
		baseline.Name != api.Name || baseline.Pointer != api.Pointer ||
		!baseline.Result.Exists || !identifierPattern.MatchString(baseline.UID) ||
		!identifierPattern.MatchString(baseline.ResourceVersion) || !applicationAPIResultValid(baseline.Result) {
		return ErrBinding
	}
	return nil
}

func validateApplicationWorkerAPIBaseline(input ApplicationWorkerInput, check Check) error {
	if !ApplicationUsesAPINormalTransition(check, input.Manifest.ApplicationPolicy) {
		if input.APIBaseline != nil {
			return ErrBinding
		}
		return nil
	}
	if input.APIBaseline != nil {
		return ValidateApplicationAPIBaseline(check, *input.APIBaseline)
	}
	for _, action := range check.Application.Actions {
		if ApplicationActionObservesAPI(check, action, input.Manifest.ApplicationPolicy) {
			return nil
		}
	}
	return errors.New("API normal observation requires its frozen owned target's actual initial API revision")
}

func applicationCheckAPIBaseline(input ApplicationWorkerInput, check Check,
	actions []ApplicationActionObservation) (*ApplicationAPIIdentity, error) {
	if !ApplicationUsesAPINormalTransition(check, input.Manifest.ApplicationPolicy) {
		return nil, nil
	}
	baseline := input.APIBaseline
	for index, action := range check.Application.Actions {
		if !ApplicationActionObservesAPI(check, action, input.Manifest.ApplicationPolicy) {
			continue
		}
		if index >= len(actions) || actions[index].InitialAPI == nil ||
			actions[index].InitialAPI.UID != actions[index].UID ||
			actions[index].InitialAPI.ResourceVersion != actions[index].ResourceVersion {
			return nil, ErrBinding
		}
		baseline = actions[index].InitialAPI
	}
	if baseline == nil || ValidateApplicationAPIBaseline(check, *baseline) != nil {
		return nil, ErrBinding
	}
	copy := *baseline
	return &copy, nil
}

func validateApplicationNormalAPIObservation(manifest Manifest, check Check, app *ApplicationObservation) error {
	if !ApplicationUsesAPINormalTransition(check, manifest.ApplicationPolicy) {
		if app.APIBaseline != nil {
			return ErrBinding
		}
		return nil
	}
	if app.APIBaseline == nil || app.Kubernetes == nil || ValidateApplicationAPIBaseline(check, *app.APIBaseline) != nil ||
		app.APIBaseline.UID != app.Kubernetes.UID || app.Kubernetes.Pointer != check.Application.Kubernetes.Pointer {
		return ErrBinding
	}
	var actionBaseline *ApplicationAPIIdentity
	for index, action := range check.Application.Actions {
		if ApplicationActionObservesAPI(check, action, manifest.ApplicationPolicy) {
			if index >= len(app.Actions) || app.Actions[index].InitialAPI == nil {
				return ErrBinding
			}
			actionBaseline = app.Actions[index].InitialAPI
		}
	}
	if actionBaseline != nil && !reflect.DeepEqual(app.APIBaseline, actionBaseline) {
		return ErrBinding
	}
	content, _ := json.Marshal(app.Kubernetes.Result)
	if string(content) == check.Healthy.Stdout &&
		(app.APIBaseline.ResourceVersion == app.Kubernetes.ResourceVersion ||
			app.APIBaseline.Result == app.Kubernetes.Result) {
		return errors.New("healthy API normal evidence must change the exact owned target's initial API status")
	}
	return nil
}
