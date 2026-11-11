package applicationexec

import (
	"reflect"
	"strconv"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type apiBaselineReceipt struct {
	EffectKey string                    `json:"effectKey"`
	Identity  pv.ApplicationAPIIdentity `json:"identity"`
}

func (b *Backend) apiBaselinesFromCreate(r *run, effectKey string, created client.Object) (map[string]apiBaselineReceipt, error) {
	baselines := make(map[string]apiBaselineReceipt)
	for _, side := range pv.ActionSides(r.state.Request.Action) {
		if created.GetNamespace() != namespaceName(r, side) {
			continue
		}
		for _, check := range r.state.Request.Checks.Checks {
			if !pv.ApplicationUsesAPINormalTransition(check, r.state.Request.ApplicationPolicy) {
				continue
			}
			target := check.Application.Kubernetes
			gvk, err := b.kind(created)
			if err != nil {
				return nil, err
			}
			if created.GetName() != target.Name || gvk.GroupVersion().String() != target.APIVersion ||
				resourceName(target.APIVersion, gvk.Kind, r.state.Request.ApplicationPolicy) != target.Resource {
				continue
			}
			value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(created)
			if err != nil {
				return nil, contract.ErrConflict
			}
			response := &unstructured.Unstructured{Object: value}
			if actual := response.GroupVersionKind(); !actual.Empty() && actual != gvk {
				return nil, contract.ErrConflict
			}
			// Typed client responses may omit TypeMeta. Restore only the GVK
			// established by the concrete registered type, never UID/RV/status.
			response.SetGroupVersionKind(gvk)
			identity, err := pv.ApplicationAPIIdentityFromObject(response,
				created.GetNamespace(), *target, r.state.Request.ApplicationPolicy)
			if err != nil || identity == nil || pv.ValidateApplicationAPIBaseline(check, *identity) != nil {
				return nil, contract.ErrConflict
			}
			baselines[resourceKey(side, check.ID)] = apiBaselineReceipt{EffectKey: effectKey, Identity: *identity}
		}
	}
	return baselines, nil
}

func (b *Backend) inputAPIBaseline(r *run, side, checkID string) (*pv.ApplicationAPIIdentity, error) {
	receipt, found := r.state.APIBaselines[resourceKey(side, checkID)]
	if !found {
		return nil, nil
	}
	entry := r.effect(receipt.EffectKey)
	if entry == nil || entry.Deleted || entry.Rejected || entry.UID == "" || string(entry.UID) != receipt.Identity.UID {
		return nil, contract.ErrConflict
	}
	identity := receipt.Identity
	return &identity, nil
}

func validateNormalAPIBaseline(r *run, input pv.ApplicationWorkerInput, observation pv.Observation) error {
	for checkIndex, check := range r.state.Request.Checks.Checks {
		if check.ID != input.CheckID || !pv.ApplicationUsesAPINormalTransition(check, r.state.Request.ApplicationPolicy) {
			continue
		}
		expected := input.APIBaseline
		effectKey := ""
		if baseline, found := r.state.APIBaselines[resourceKey(input.Side, check.ID)]; found {
			if expected == nil || !reflect.DeepEqual(*expected, baseline.Identity) {
				return contract.ErrConflict
			}
			effectKey = baseline.EffectKey
		} else if expected != nil {
			return contract.ErrConflict
		}
		for actionIndex, action := range check.Application.Actions {
			if !pv.ApplicationActionObservesAPI(check, action, r.state.Request.ApplicationPolicy) {
				continue
			}
			receipt := r.state.Actions[actionSlot(input.Side, "observe-"+strconv.Itoa(checkIndex), actionIndex)]
			if receipt.Outcome == nil || receipt.Outcome.StatusCode != 200 && receipt.Outcome.StatusCode != 201 {
				continue
			}
			expected, effectKey = receipt.Outcome.InitialAPI, receipt.EffectKey
		}
		if expected == nil || observation.Application == nil || observation.Application.APIBaseline == nil ||
			!reflect.DeepEqual(*expected, *observation.Application.APIBaseline) {
			return contract.ErrConflict
		}
		entry := r.effect(effectKey)
		if entry == nil || entry.UID == "" || entry.Rejected || entry.Deleted || string(entry.UID) != expected.UID {
			return contract.ErrConflict
		}
	}
	return nil
}
