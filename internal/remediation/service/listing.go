package service

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	kubelabels "k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

func activeStateValue(st *state) string {
	if contract.TerminalPhase(st.Status.Phase) && st.Settled {
		return boolFalse
	}
	return boolTrue
}

func listPendingStates(ctx context.Context, reader client.Reader, namespace string) ([]corev1.ConfigMap, error) {
	// !=false includes pre-label records during upgrades and excludes settled
	// state bodies on the API server, not after transferring their report data.
	selector, err := kubelabels.Parse(stateLabel + "=v1," + activeLabel + "!=" + boolFalse)
	if err != nil {
		return nil, contract.ErrInvalid
	}
	var list corev1.ConfigMapList
	if err := reader.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, contract.ErrUnavailable
	}
	return list.Items, nil
}

func (s *Coordinator) prepareListedRun(ctx context.Context, object *corev1.ConfigMap) (*run, error) {
	var saved state
	if json.Unmarshal([]byte(object.Data["state"]), &saved) != nil {
		return nil, contract.ErrConflict
	}
	r, err := decodeRun(object, saved.ID, s.config.Namespace)
	if err != nil {
		return nil, err
	}
	if object.Labels[activeLabel] != activeStateValue(&r.state) {
		// A CAS update re-derives the label from the latest state; an old
		// settled snapshot must not hide a concurrent cancellation cleanup.
		return s.update(ctx, r.state.ID, func(*run) error { return nil })
	}
	return r, nil
}
