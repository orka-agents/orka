package applicationexec

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type actionReceipt struct {
	Side       string                           `json:"side"`
	Role       string                           `json:"role"`
	Index      int                              `json:"index"`
	Digest     string                           `json:"digest"`
	EffectKey  string                           `json:"effectKey"`
	Target     pv.ApplicationActionTarget       `json:"target"`
	Actor      pv.ApplicationActorIdentity      `json:"actor"`
	Outcome    *pv.ApplicationActionObservation `json:"outcome,omitempty"`
	NotStarted bool                             `json:"notStarted,omitempty"`
}

func actionSlot(side, role string, index int) string {
	return fmt.Sprintf("%s/%s/%d", side, role, index)
}

func (b *Backend) reserveActions(ctx context.Context, r *run, side, role string,
	check pv.Check, actors []pv.ApplicationActorIdentity) ([]pv.ApplicationActionTarget, error) {
	if len(check.Application.Actions) == 0 {
		return nil, nil
	}
	frozen := false
	for _, admitted := range r.state.Request.Checks.Checks {
		frozen = frozen || reflect.DeepEqual(admitted, check)
	}
	if !frozen {
		return nil, contract.ErrConflict
	}
	if err := b.authorizeEffect(ctx, r); err != nil {
		return nil, err
	}
	targets := make([]pv.ApplicationActionTarget, 0, len(check.Application.Actions))
	changed := false
	for index, action := range check.Application.Actions {
		target, reserved, err := b.reserveAction(ctx, r, side, role, index, action, actors)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
		changed = changed || reserved
	}
	if changed {
		if err := b.save(ctx, r); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func (b *Backend) reserveAction(ctx context.Context, r *run, side, role string, index int,
	action pv.ApplicationAction, actors []pv.ApplicationActorIdentity) (pv.ApplicationActionTarget, bool, error) {
	var zero pv.ApplicationActionTarget
	var actor pv.ApplicationActorIdentity
	for _, identity := range actors {
		if identity.ID == action.ActorID {
			actor = identity
		}
	}
	if actor.ID == "" {
		return zero, false, contract.ErrInvalid
	}
	content, _ := json.Marshal(action)
	digest := pv.Digest(content)
	slot := actionSlot(side, role, index)
	if previous, found := r.state.Actions[slot]; found {
		if previous.Digest != digest || previous.Actor != actor || previous.Side != side || previous.Role != role || previous.Index != index || previous.NotStarted {
			return zero, false, contract.ErrConflict
		}
		return previous.Target, false, nil
	}
	object, err := pv.ValidateApplicationResourceWithPolicy(action.Resource, r.state.Request.ApplicationPolicy)
	if err != nil {
		return zero, false, contract.ErrInvalid
	}
	object.SetNamespace(namespaceName(r, side))
	object.SetLabels(mergeLabels(object.GetLabels(), map[string]string{runLabel: string(r.object.UID)}))
	gvk := object.GroupVersionKind()
	target := pv.ApplicationActionTarget{Index: index, APIVersion: object.GetAPIVersion(),
		Resource: resourceName(object.GetAPIVersion(), object.GetKind(), r.state.Request.ApplicationPolicy),
		Name:     object.GetName(), MustBeAbsent: action.Verb == verbCreate, OwnerUID: string(r.object.UID)}
	if target.Resource == "" {
		return zero, false, contract.ErrInvalid
	}
	existing := r.namedEffect(target.APIVersion, gvk.Kind, object.GetNamespace(), target.Name)
	actual := object.DeepCopy()
	err = b.targetReader.Get(ctx, client.ObjectKeyFromObject(object), actual)
	effectKey := ""
	switch action.Verb {
	case verbCreate:
		if existing != nil || !apierrors.IsNotFound(err) {
			if err != nil && !apierrors.IsNotFound(err) {
				return zero, false, safeError(contract.ErrUnavailable, err)
			}
			return zero, false, contract.ErrConflict
		}
		identity, _ := json.Marshal([]string{target.APIVersion, gvk.Kind, target.Name})
		effectKey = resourceKey(side, "actor-object-"+strings.TrimPrefix(pv.Digest(identity), "sha256:")[:24])
		if len(r.state.Effects) >= maxEffects {
			return zero, false, contract.ErrInvalid
		}
		objectHash, err := objectDigest(object)
		if err != nil {
			return zero, false, err
		}
		r.state.Effects = append(r.state.Effects, effect{Key: effectKey, APIVersion: target.APIVersion, Kind: gvk.Kind,
			Namespace: object.GetNamespace(), Name: object.GetName(), Digest: objectHash, Attempted: true, Actor: true})
	case verbUpdate:
		if err != nil {
			return zero, false, safeError(contract.ErrUnavailable, err)
		}
		if existing == nil || existing.UID == "" || existing.Deleted || existing.Rejected ||
			actual.GetUID() != existing.UID || !actual.GetDeletionTimestamp().IsZero() {
			return zero, false, contract.ErrConflict
		}
		target.UID, target.ResourceVersion, effectKey = string(actual.GetUID()), actual.GetResourceVersion(), existing.Key
	default:
		return zero, false, contract.ErrInvalid
	}
	r.state.Actions[slot] = actionReceipt{Side: side, Role: role, Index: index, Digest: digest,
		Target: target, Actor: actor, EffectKey: effectKey}
	return target, true, nil
}

func mergeLabels(original, additions map[string]string) map[string]string {
	copy := make(map[string]string, len(original)+len(additions))
	maps.Copy(copy, original)
	maps.Copy(copy, additions)
	return copy
}

func (b *Backend) recordAction(ctx context.Context, r *run, side, role string,
	input pv.ApplicationWorkerInput, result pv.ApplicationActionObservation) error {
	slot := actionSlot(side, role, result.Index)
	previous, found := r.state.Actions[slot]
	if !found || previous.Target != result.Target || previous.Actor != result.Actor || previous.NotStarted {
		return contract.ErrConflict
	}
	if previous.Outcome != nil {
		if !reflect.DeepEqual(*previous.Outcome, result) {
			return contract.ErrConflict
		}
		return nil
	}
	var check *pv.Check
	for i := range input.Manifest.Checks {
		if input.Manifest.Checks[i].ID == input.CheckID {
			check = &input.Manifest.Checks[i]
		}
	}
	if check == nil || result.Index < 0 || result.Index >= len(check.Application.Actions) ||
		pv.ValidateApplicationActionObservation(check.Application.Actions[result.Index], previous.Target, previous.Actor, result, input.Manifest.ApplicationPolicy) != nil {
		return contract.ErrConflict
	}
	entry := r.effect(previous.EffectKey)
	if entry == nil {
		return contract.ErrConflict
	}
	if previous.Target.MustBeAbsent {
		switch result.StatusCode {
		case 201:
			if result.UID == "" {
				return contract.ErrConflict
			}
			if err := b.recordEffectResult(ctx, r, entry.Key, types.UID(result.UID), ""); err != nil {
				return err
			}
		case 400, 401, 403, 404, 405, 409, 422, 429:
			if result.UID != "" || result.ResourceVersion != "" {
				return contract.ErrConflict
			}
			if err := b.recordEffectResult(ctx, r, entry.Key, "", effectCreateRejected); err != nil {
				return err
			}
		default:
			return ErrUncertain
		}
	} else if result.StatusCode == 200 && result.UID != previous.Target.UID {
		return contract.ErrConflict
	}
	previous.Outcome = &result
	r.state.Actions[slot] = previous
	return b.save(ctx, r)
}

func (b *Backend) validateActionSet(r *run, side, role string, input pv.ApplicationWorkerInput, report pv.ApplicationReport) error {
	actual := report.Evidence.Observation.Application
	if actual == nil || len(actual.Actions) != len(input.ActionTargets) {
		return contract.ErrConflict
	}
	for index, observation := range actual.Actions {
		receipt, ok := r.state.Actions[actionSlot(side, role, index)]
		if !ok || receipt.Outcome == nil || !reflect.DeepEqual(*receipt.Outcome, observation) {
			return contract.ErrConflict
		}
	}
	return nil
}

func (b *Backend) rejectUndispatchedActions(ctx context.Context, r *run, side, role string) error {
	credentialKey := resourceKey(observerSide(r, side), role+"-actors")
	credentialEffect := r.effect(credentialKey)
	if credentialEffect != nil && !credentialEffect.Rejected {
		// Once the token Secret create was attempted, a missing response is
		// not evidence that the observer could not start.
		return ErrUncertain
	}
	changed := false
	for key, action := range r.state.Actions {
		if action.Side != side || action.Role != role || action.Outcome != nil || action.NotStarted {
			continue
		}
		if action.Target.MustBeAbsent {
			entry := r.effect(action.EffectKey)
			if entry == nil || entry.UID != "" {
				return contract.ErrConflict
			}
			entry.Rejected, entry.Failure = true, "not-dispatched"
		}
		action.NotStarted = true
		r.state.Actions[key] = action
		changed = true
	}
	if changed {
		return b.save(ctx, r)
	}
	return nil
}
