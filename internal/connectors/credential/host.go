/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package credential

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/store"
)

// ErrHostLinkUnavailable reports a Connection that can no longer feed an
// agent host: it is gone or unlinked, its consent no longer covers the
// provider, or the provider no longer serves agent hosts.
var ErrHostLinkUnavailable = errors.New("the agent host's GitHub link is unavailable")

// HostCredential is the access token Orka hands a person's agent host.
type HostCredential struct {
	AccessToken string
	// ExpiresAt is zero when the provider reported no expiry.
	ExpiresAt time.Time
	// Version changes whenever custody holds new material.
	Version       int64
	GrantSequence int64
	// ConnectionUID binds the grant sequence to this incarnation of the link.
	ConnectionUID string
}

// HostCredential returns the access token for the agent host fed by the named
// Connection, refreshed first when it would expire within horizon. Only the
// access token leaves custody; the refresh token never does. It shares the
// tool path's single-flight refresh, so a host and a tool call never refresh
// the same grant twice, and a refresh made for either replaces the other's
// token.
func (s *Source) HostCredential(ctx context.Context, namespace, name string, horizon time.Duration) (HostCredential, error) {
	connection, ref, credential, err := s.hostLink(ctx, namespace, name)
	if err != nil {
		return HostCredential{}, err
	}
	if s.expiresWithin(credential, horizon) {
		refreshed, err := s.refreshSingleFlight(ctx, connection, ref, credential.GrantSequence, horizon)
		if err != nil {
			// A refresh that ended the link (a revoked or expired grant)
			// shows on the Connection; anything else is worth a retry.
			if _, linkErr := s.hostConnection(ctx, namespace, name); linkErr != nil {
				return HostCredential{}, linkErr
			}
			return HostCredential{}, err
		}
		credential = refreshed
	}
	// Every check runs again before release: a disconnect that began after
	// the first read tombstones custody without the custody read noticing,
	// and the provider may have stopped serving hosts, or a re-consent may
	// have replaced the material, while a refresh was in flight. Only what
	// custody still holds, for a link that still passes, is released.
	again, _, latest, err := s.hostLink(ctx, namespace, name)
	if err != nil {
		return HostCredential{}, err
	}
	if again.UID != connection.UID {
		return HostCredential{}, hostLinkUnavailable("the Connection was replaced")
	}
	if latest.Version != credential.Version || latest.AccessToken != credential.AccessToken {
		return HostCredential{}, errors.New("the Connection's credential changed while it was read; retry")
	}
	return HostCredential{
		AccessToken:   credential.AccessToken,
		ExpiresAt:     credential.ExpiresAt,
		Version:       credential.Version,
		GrantSequence: credential.GrantSequence,
		ConnectionUID: string(connection.UID),
	}, nil
}

// CheckHostLink reports whether the named Connection can still feed an agent
// host, without refreshing its token: a suspended host keeps its token copy
// only while it can.
func (s *Source) CheckHostLink(ctx context.Context, namespace, name string) error {
	_, _, _, err := s.hostLink(ctx, namespace, name)
	return err
}

// hostLink loads a host's Connection and the credential it holds, and checks
// that the link may feed an agent host. A host keeps the template it was
// created from: pointing the provider at another template affects only hosts
// created afterwards.
func (s *Source) hostLink(ctx context.Context, namespace, name string) (
	*corev1alpha1.Connection, store.ConnectorCredentialRef, store.ConnectorCredential, error) {
	var ref store.ConnectorCredentialRef
	if s == nil || s.Client == nil || s.Credentials == nil {
		return nil, ref, store.ConnectorCredential{}, errors.New("connection credential source is not configured")
	}
	connection, err := s.hostConnection(ctx, namespace, name)
	if err != nil {
		return nil, ref, store.ConnectorCredential{}, err
	}
	provider := &corev1alpha1.ConnectorProvider{}
	key := types.NamespacedName{Namespace: namespace, Name: connection.Spec.ProviderRef.Name}
	if err := s.reader().Get(ctx, key, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ref, store.ConnectorCredential{}, hostLinkUnavailable("its connector provider is gone")
		}
		return nil, ref, store.ConnectorCredential{}, fmt.Errorf("load connector provider: %w", err)
	}
	if provider.Spec.AgentHost == nil {
		return nil, ref, store.ConnectorCredential{}, hostLinkUnavailable("its connector provider no longer serves agent hosts")
	}
	// A settled refusal, including deletion, needs no custody read. A storage
	// outage must not keep a host running after its provider refused it.
	judged, accepted := providerVerdict(provider)
	if judged && !accepted {
		return nil, ref, store.ConnectorCredential{}, hostLinkUnavailable("its connector provider is not accepted")
	}
	ref, err = connectors.CredentialRef(connection)
	if err != nil {
		return nil, ref, store.ConnectorCredential{}, err
	}
	credential, err := s.Credentials.GetConnectorCredential(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ref, store.ConnectorCredential{}, hostLinkUnavailable("it holds no credential; the person must reconnect")
	}
	if err != nil {
		return nil, ref, store.ConnectorCredential{}, err
	}
	// An unobserved or inconclusive status is retryable, but only after
	// custody: missing credentials already prove that the link has ended.
	if !judged {
		return nil, ref, store.ConnectorCredential{}, fmt.Errorf("connector provider %q has not judged its latest change", provider.Name)
	}
	// The provider just read is accepted, so a mismatch with it is a
	// verdict too; a failed read above is retried.
	if err := checkProvider(connection, provider, credential, nil); err != nil {
		return nil, ref, store.ConnectorCredential{}, hostLinkUnavailable(err.Error())
	}
	// Everything the Connection's status summarizes has now been checked
	// against the provider and custody directly, so a status that has not
	// caught up with an edit, or that records a failed read, stops no host.
	if !connectors.ConnectionLinked(connection) {
		return nil, ref, store.ConnectorCredential{}, fmt.Errorf("the Connection %q has not caught up with its latest change", name)
	}
	return connection, ref, credential, nil
}

// hostConnection reads a host's Connection fresh and requires that its person
// still holds a link: it exists, is not being deleted, and is Ready.
func (s *Source) hostConnection(ctx context.Context, namespace, name string) (*corev1alpha1.Connection, error) {
	connection := &corev1alpha1.Connection{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, hostLinkUnavailable("the Connection is gone")
		}
		return nil, fmt.Errorf("load connection: %w", err)
	}
	// A Connection being deleted is a disconnect, even while its finalizer
	// holds it.
	if !connection.DeletionTimestamp.IsZero() {
		return nil, hostLinkUnavailable("the Connection is being deleted")
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return nil, hostLinkUnavailable("the Connection is not linked")
	}
	return connection, nil
}

// providerVerdict reports whether the provider controller has judged the
// provider's current generation, and if so whether it accepted it. A current
// False condition is a refusal; a missing, older or Unknown one awaits
// judgment. A provider being deleted is refused.
func providerVerdict(provider *corev1alpha1.ConnectorProvider) (judged, accepted bool) {
	if !provider.DeletionTimestamp.IsZero() {
		return true, false
	}
	judged = true
	for _, conditionType := range []string{
		corev1alpha1.ConnectorProviderConditionAccepted,
		corev1alpha1.ConnectorProviderConditionResolvedRefs,
	} {
		condition := meta.FindStatusCondition(provider.Status.Conditions, conditionType)
		if condition == nil || condition.ObservedGeneration != provider.Generation {
			judged = false
			continue
		}
		switch condition.Status {
		case metav1.ConditionFalse:
			return true, false
		case metav1.ConditionTrue:
		default:
			judged = false
		}
	}
	return judged, judged
}

func hostLinkUnavailable(reason string) error {
	return fmt.Errorf("%w: %s", ErrHostLinkUnavailable, reason)
}
