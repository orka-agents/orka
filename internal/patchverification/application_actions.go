package patchverification

import (
	"context"
	"errors"
	"slices"
)

// Implementations use actor-specific HTTP authentication. The interface is a
// trusted worker adapter, never a model-defined hook or executable callback.
type ApplicationActorClient interface {
	Apply(context.Context, string, ApplicationAction, ApplicationActionTarget) (ApplicationActionObservation, error)
}

func ValidateApplicationActionTarget(action ApplicationAction, target ApplicationActionTarget, index int, policy *ApplicationPolicy) error {
	object, err := ValidateApplicationResourceWithPolicy(action.Resource, policy)
	if err != nil {
		return err
	}
	rule, err := ApplicationResourceRuleFor(policy, object.GetAPIVersion(), object.GetKind())
	if err != nil || target.Index != index || target.APIVersion != object.GetAPIVersion() ||
		target.Resource != rule.Resource || target.Name != object.GetName() ||
		!identifierPattern.MatchString(target.OwnerUID) || len(target.OwnerUID) > 63 {
		return ErrBinding
	}
	if action.Verb == ApplicationActionCreate {
		if !target.MustBeAbsent || target.UID != "" || target.ResourceVersion != "" {
			return ErrBinding
		}
	} else if action.Verb != ApplicationActionUpdate || target.MustBeAbsent || !identifierPattern.MatchString(target.UID) ||
		target.ResourceVersion == "" || len(target.ResourceVersion) > 128 {
		return ErrBinding
	}
	return nil
}

func applicationActorValid(actor ApplicationActorIdentity) bool {
	return applicationName(actor.ID) && applicationName(actor.Namespace) &&
		identifierPattern.MatchString(actor.NamespaceUID) && applicationName(actor.ServiceAccountName) &&
		identifierPattern.MatchString(actor.ServiceAccountUID)
}

func ValidateApplicationActionObservation(action ApplicationAction, target ApplicationActionTarget, actor ApplicationActorIdentity, observation ApplicationActionObservation, policy *ApplicationPolicy) error {
	if ValidateApplicationActionTarget(action, target, target.Index, policy) != nil ||
		!applicationActorValid(actor) || actor.ID != action.ActorID || observation.Actor != actor ||
		observation.Index != target.Index || observation.Target != target || observation.APIVersion != target.APIVersion ||
		observation.Resource != target.Resource || observation.Name != target.Name {
		return ErrBinding
	}
	switch observation.StatusCode {
	case 400, 401, 403, 404, 409, 422, 429:
		if observation.UID != "" || observation.ResourceVersion != "" ||
			observation.StatusCode != 403 && observation.AuthorizationDenied {
			return ErrEvidence
		}
	case 200, 201:
		if observation.AuthorizationDenied || !identifierPattern.MatchString(observation.UID) || observation.ResourceVersion == "" ||
			len(observation.ResourceVersion) > 128 ||
			action.Verb == ApplicationActionCreate && observation.StatusCode != 201 ||
			action.Verb == ApplicationActionUpdate && (observation.StatusCode != 200 || observation.UID != target.UID) {
			return ErrEvidence
		}
	default:
		return errors.New("application action did not return a complete creation/update or authenticated denial")
	}
	return validateApplicationActionInitialAPI(target, observation)
}

func validateApplicationActionInitialAPI(target ApplicationActionTarget, observation ApplicationActionObservation) error {
	if initial := observation.InitialAPI; initial != nil {
		if observation.StatusCode != 200 && observation.StatusCode != 201 ||
			initial.APIVersion != target.APIVersion || initial.Resource != target.Resource || initial.Name != target.Name ||
			initial.UID != observation.UID || initial.ResourceVersion != observation.ResourceVersion ||
			!applicationJSONPointer(initial.Pointer) || !initial.Result.Exists || !applicationAPIResultValid(initial.Result) {
			return ErrBinding
		}
	}
	return nil
}

func validateApplicationWorkerActions(input ApplicationWorkerInput, check Check) error {
	if check.Application == nil || len(input.ActionTargets) != len(check.Application.Actions) || len(input.Actors) > 8 {
		return ErrBinding
	}
	actors, identities := map[string]bool{}, map[string]bool{}
	for _, actor := range input.Actors {
		if !applicationActorValid(actor) || actors[actor.ID] || identities[actor.ServiceAccountUID] ||
			actor.Namespace == input.Target.Namespace || actor.NamespaceUID == input.Target.NamespaceUID ||
			actor.ServiceAccountUID == input.Worker.ServiceAccountUID ||
			input.Subject != nil && actor.ServiceAccountUID == input.Subject.ServiceAccountUID {
			return ErrBinding
		}
		actors[actor.ID], identities[actor.ServiceAccountUID] = true, true
	}
	required := map[string]bool{}
	for index, action := range check.Application.Actions {
		if !actors[action.ActorID] || ValidateApplicationActionTarget(action, input.ActionTargets[index], index, input.Manifest.ApplicationPolicy) != nil {
			return ErrBinding
		}
		required[action.ActorID] = true
	}
	if len(required) != len(actors) {
		return ErrBinding
	}
	return nil
}

func executeApplicationActions(ctx context.Context, input ApplicationWorkerInput, check Check, clients map[string]ApplicationActorClient, onAction func(ApplicationActionObservation) error) ([]ApplicationActionObservation, error) {
	if err := validateApplicationWorkerActions(input, check); err != nil {
		return nil, err
	}
	result := make([]ApplicationActionObservation, 0, len(check.Application.Actions))
	for index, action := range check.Application.Actions {
		if ctx.Err() != nil || clients[action.ActorID] == nil || onAction == nil {
			return result, ErrEvidence
		}
		actual, err := clients[action.ActorID].Apply(ctx, input.Target.Namespace, action, input.ActionTargets[index])
		if err != nil {
			return result, err
		}
		actorIndex := slices.IndexFunc(input.Actors, func(actor ApplicationActorIdentity) bool { return actor.ID == action.ActorID })
		if actorIndex < 0 || ValidateApplicationActionObservation(action, input.ActionTargets[index],
			input.Actors[actorIndex], actual, input.Manifest.ApplicationPolicy) != nil {
			return result, ErrBinding
		}
		if err := onAction(actual); err != nil {
			return result, err
		}
		result = append(result, actual)
		if actual.StatusCode != action.ExpectedStatus || action.ExpectedStatus == 403 && !actual.AuthorizationDenied {
			return result, errors.New("application actor authorization outcome differs from the frozen expected status")
		}
	}
	return result, nil
}

func validateApplicationActionEvidence(manifest Manifest, check Check, proof *ApplicationObservation) error {
	if len(proof.Actions) != len(check.Application.Actions) {
		return ErrEvidence
	}
	for index, action := range check.Application.Actions {
		actual := proof.Actions[index]
		if actual.Index != index || actual.StatusCode != action.ExpectedStatus ||
			action.ExpectedStatus == 403 && !actual.AuthorizationDenied ||
			actual.Actor.Namespace == proof.Target.Namespace ||
			actual.Actor.ServiceAccountUID == proof.Subject.ServiceAccountUID ||
			ValidateApplicationActionObservation(action, actual.Target, actual.Actor, actual, manifest.ApplicationPolicy) != nil {
			return ErrEvidence
		}
		approvedActor := false
		if manifest.ApplicationPolicy != nil {
			for _, actor := range manifest.ApplicationPolicy.Actors {
				approvedActor = approvedActor || actor.ID == actual.Actor.ID
			}
		}
		if !approvedActor {
			return ErrBinding
		}
	}
	return nil
}

func applicationIdentityTarget(target ApplicationTarget, identity ApplicationContainerIdentity) ApplicationTarget {
	if identity.Namespace != "" {
		target.Namespace, target.NamespaceUID = identity.Namespace, identity.NamespaceUID
	}
	return target
}
