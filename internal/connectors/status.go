/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"errors"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// NormalizeConnectionMode returns the effective Connection mode; an empty
// mode is readOnly.
func NormalizeConnectionMode(mode string) (string, error) {
	switch mode {
	case "":
		return corev1alpha1.ConnectionModeReadOnly, nil
	case corev1alpha1.ConnectionModeReadOnly, corev1alpha1.ConnectionModeReadWrite:
		return mode, nil
	default:
		return "", errors.New("mode must be readOnly or readWrite")
	}
}

// ApplyLinkedStatus records a successful consent on the Connection from the
// material custody holds: the provider OAuth client it was granted against,
// the granted scopes, the expiry, and the three conditions. The granted
// scopes are judged against what the mode requires now: a provider expanded
// after consent started leaves the link Pending until the person consents
// again. Both the API (right after a commit) and the controller (finishing a
// commit whose status write was lost) use it, so they never disagree.
func ApplyLinkedStatus(connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, credential store.ConnectorCredential, now metav1.Time) {
	mode, _ := NormalizeConnectionMode(connection.Spec.Mode)
	connection.Status.State = corev1alpha1.ConnectionStateReady
	connection.Status.Consent = ConsentFor(provider)
	connection.Status.GrantedScopes = append([]string(nil), credential.Scopes...)
	connection.Status.LinkedAt = &now
	connection.Status.GrantSequence = credential.GrantSequence
	connection.Status.LastRefreshTime = nil
	connection.Status.ExpiresAt = nil
	if !credential.ExpiresAt.IsZero() {
		expires := metav1.NewTime(credential.ExpiresAt.UTC())
		connection.Status.ExpiresAt = &expires
	}
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             corev1alpha1.ConnectionReasonLinked,
		Message:            "Account linked",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	})
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionProviderResolved,
		Status:             metav1.ConditionTrue,
		Reason:             corev1alpha1.ConnectionReasonProviderResolved,
		Message:            "ConnectorProvider is accepted",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	})
	granted := metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionScopesGranted,
		Status:             metav1.ConditionTrue,
		Reason:             corev1alpha1.ConnectionReasonScopesGranted,
		Message:            "Granted scopes cover the " + mode + " mode",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	}
	if !ScopesCover(credential.Scopes, ScopesForMode(provider, mode)) {
		granted.Status = metav1.ConditionFalse
		granted.Reason = corev1alpha1.ConnectionReasonConsentRequired
		granted.Message = "Granted scopes do not cover the " + mode + " mode; consent again"
		connection.Status.State = corev1alpha1.ConnectionStatePending
	}
	meta.SetStatusCondition(&connection.Status.Conditions, granted)
}
