/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package credential resolves a person's linked-account access token for a
// tool call, refreshing it lazily with single-flight per Connection and
// writing rotated material back to custody. It runs only in the controller.
package credential

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
)

// DefaultRefreshSkew refreshes tokens that expire within this window so a
// call does not start with a token that dies mid-request.
const DefaultRefreshSkew = 60 * time.Second

// Refresher performs the OAuth refresh grant. *connectors.OAuthClient
// satisfies it.
type Refresher interface {
	Refresh(ctx context.Context, cfg connectors.OAuthProviderConfig, refreshToken string) (connectors.TokenResponse, error)
}

// Source implements outboundaccess.ConnectionCredentialSource.
type Source struct {
	// Client reads Connections and writes their status.
	Client client.Client
	// APIReader reads provider client secrets uncached.
	APIReader   client.Reader
	Credentials store.ConnectorCredentialStore
	OAuth       Refresher
	Now         func() time.Time
	RefreshSkew time.Duration

	flights singleflight.Group
}

var _ outboundaccess.ConnectionCredentialSource = (*Source)(nil)

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// reader is the uncached API reader when configured, so identity and
// readiness checks never trust a lagging cache. Status writes use Client.
func (s *Source) reader() client.Reader {
	if s.APIReader != nil {
		return s.APIReader
	}
	return s.Client
}

func (s *Source) skew() time.Duration {
	if s.RefreshSkew > 0 {
		return s.RefreshSkew
	}
	return DefaultRefreshSkew
}

// ResolveConnectionCredential implements outboundaccess.ConnectionCredentialSource.
func (s *Source) ResolveConnectionCredential(ctx context.Context, req outboundaccess.ConnectionCredentialRequest) (outboundaccess.ConnectionCredential, error) {
	if s == nil || s.Client == nil || s.Credentials == nil {
		return outboundaccess.ConnectionCredential{}, errors.New("connection credential source is not configured")
	}
	if strings.TrimSpace(req.Issuer) == "" || strings.TrimSpace(req.Subject) == "" {
		return outboundaccess.ConnectionCredential{}, errors.New("connection credential requires a verified requester")
	}
	if strings.TrimSpace(req.Frozen.UID) == "" {
		return outboundaccess.ConnectionCredential{}, errors.New("connection credential requires a frozen Connection binding")
	}
	connection, err := s.loadLiveConnection(ctx, req)
	if err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	credential, err := s.Credentials.GetConnectorCredential(ctx, ref)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return outboundaccess.ConnectionCredential{}, errors.New("connection holds no credential; the person must reconnect")
		}
		return outboundaccess.ConnectionCredential{}, err
	}
	if s.needsRefresh(credential) {
		credential, err = s.refreshSingleFlight(ctx, connection, ref)
		if err != nil {
			return outboundaccess.ConnectionCredential{}, err
		}
		// The refresh may have lost to a re-consent, or the Connection may
		// have changed mode or generation while the exchange was in flight.
		// Re-read it and reapply every check before pairing the material
		// with a mode and generation.
		if connection, err = s.loadLiveConnection(ctx, req); err != nil {
			return outboundaccess.ConnectionCredential{}, err
		}
	}
	return outboundaccess.ConnectionCredential{
		AccessToken:   credential.AccessToken,
		TokenType:     credential.TokenType,
		ConnectionUID: string(connection.UID),
		Generation:    connection.Generation,
		Mode:          connection.Spec.Mode,
	}, nil
}

// loadLiveConnection re-reads the requester's Connection and checks that it
// is exactly the one frozen at dispatch and still Ready for its current
// generation.
func (s *Source) loadLiveConnection(ctx context.Context, req outboundaccess.ConnectionCredentialRequest) (*corev1alpha1.Connection, error) {
	name := connectors.ConnectionName(req.Provider, req.Issuer, req.Subject)
	connection := &corev1alpha1.Connection{}
	// Authorization reads bypass the informer cache: a generation change
	// after dispatch must be seen even before the watch catches up, or the
	// frozen-binding check could pass against stale readiness.
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: name}, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.New("the requester has no connection to this provider")
		}
		return nil, fmt.Errorf("load connection: %w", err)
	}
	if connection.Spec.Subject.Issuer != req.Issuer || connection.Spec.Subject.Subject != req.Subject ||
		connection.Spec.ProviderRef.Name != req.Provider {
		return nil, errors.New("connection does not belong to the requester")
	}
	if !connection.DeletionTimestamp.IsZero() {
		return nil, errors.New("connection is being deleted")
	}
	if string(connection.UID) != req.Frozen.UID || connection.Generation != req.Frozen.Generation {
		return nil, errors.New("connection changed since the task was dispatched; re-dispatch to use it")
	}
	if !connectors.ConnectionLinked(connection) {
		return nil, errors.New("connection is not ready")
	}
	return connection, nil
}

func (s *Source) needsRefresh(credential store.ConnectorCredential) bool {
	return !credential.ExpiresAt.IsZero() && !credential.ExpiresAt.After(s.now().Add(s.skew()))
}

// refreshSingleFlight refreshes once per Connection at a time. Concurrent
// callers share the result, and a waiter that arrives after another flight
// finished re-reads custody instead of refreshing again.
func (s *Source) refreshSingleFlight(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef) (store.ConnectorCredential, error) {
	result, err, _ := s.flights.Do(string(connection.UID), func() (any, error) {
		// Detach from the caller so a canceled waiter cannot abort a refresh
		// other callers depend on; bound it independently.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		current, err := s.Credentials.GetConnectorCredential(flightCtx, ref)
		if err != nil {
			return nil, err
		}
		if !s.needsRefresh(current) {
			return current, nil
		}
		return s.refresh(flightCtx, connection, ref, current)
	})
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	credential, ok := result.(store.ConnectorCredential)
	if !ok {
		return store.ConnectorCredential{}, errors.New("connection refresh returned an unexpected result")
	}
	return credential, nil
}

func (s *Source) refresh(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, current store.ConnectorCredential) (store.ConnectorCredential, error) {
	logger := log.FromContext(ctx).WithValues("connection", connection.Name, "provider", connection.Spec.ProviderRef.Name)
	if strings.TrimSpace(current.RefreshToken) == "" {
		s.markNotReady(ctx, connection, corev1alpha1.ConnectionReasonExpired, corev1alpha1.ConnectionStateExpired,
			"Access token expired and the provider issued no refresh token")
		return store.ConnectorCredential{}, errors.New("connection credential expired and cannot be refreshed; the person must reconnect")
	}
	if s.OAuth == nil {
		return store.ConnectorCredential{}, errors.New("connection credential refresh is not configured")
	}
	cfg, err := s.providerConfig(ctx, connection)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	token, err := s.OAuth.Refresh(ctx, cfg, current.RefreshToken)
	if err != nil {
		var oauthErr *connectors.OAuthError
		if errors.As(err, &oauthErr) && oauthErr.IsInvalidGrant() {
			// The provider no longer honors this refresh token. Shred the
			// stale material (without a disconnect tombstone, so the person
			// can consent again) unless a newer consent already replaced it,
			// in which case the newer material is simply returned.
			shredErr := s.Credentials.ShredConnectorCredential(ctx, string(connection.UID), current.Version)
			if errors.Is(shredErr, store.ErrConflict) {
				return s.Credentials.GetConnectorCredential(ctx, ref)
			}
			if shredErr != nil {
				logger.Error(shredErr, "connector custody could not be shredded after revocation")
			}
			s.markNotReady(ctx, connection, corev1alpha1.ConnectionReasonRevoked, corev1alpha1.ConnectionStateRevoked,
				"The provider rejected the refresh token; the person must reconnect")
			return store.ConnectorCredential{}, errors.New("connection was revoked by the provider; the person must reconnect")
		}
		logger.Info("connection refresh failed transiently", "reason", oauthReason(err))
		return store.ConnectorCredential{}, errors.New("connection credential refresh failed")
	}
	refreshed := store.ConnectorCredential{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		TokenType:    token.TokenType,
		ExpiresAt:    token.ExpiresAt,
		Scopes:       token.Scopes,
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	if refreshed.TokenType == "" {
		refreshed.TokenType = current.TokenType
	}
	if len(refreshed.Scopes) == 0 {
		refreshed.Scopes = current.Scopes
	}
	// Fenced on the version read at flight start: a consent that completed
	// meanwhile wins, and its material is returned instead.
	switch err := s.Credentials.ReplaceConnectorCredential(ctx, ref, refreshed, current.Version); {
	case errors.Is(err, store.ErrConflict):
		return s.Credentials.GetConnectorCredential(ctx, ref)
	case errors.Is(err, store.ErrNotFound):
		return store.ConnectorCredential{}, errors.New("connection was disconnected during refresh")
	case err != nil:
		return store.ConnectorCredential{}, fmt.Errorf("store refreshed connection credential: %w", err)
	}
	s.recordRefresh(ctx, connection, refreshed)
	refreshed.Version = current.Version + 1
	return refreshed, nil
}

func (s *Source) providerConfig(ctx context.Context, connection *corev1alpha1.Connection) (connectors.OAuthProviderConfig, error) {
	provider := &corev1alpha1.ConnectorProvider{}
	reader := s.reader()
	if err := reader.Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider); err != nil {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider is unavailable for refresh")
	}
	if !connectors.ProviderAccepted(provider) {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider is not accepted")
	}
	secretRef := provider.Spec.OAuth.ClientSecretRef
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: secretRef.Name}, secret); err != nil {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is unavailable")
	}
	value := strings.TrimSpace(string(secret.Data[secretRef.Key]))
	if value == "" {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is empty")
	}
	return connectors.ProviderOAuthConfig(provider, value), nil
}

// recordRefresh updates non-secret status after a successful refresh. A
// failed status write is logged; the refreshed material is already safe in
// custody and the call may proceed.
func (s *Source) recordRefresh(ctx context.Context, connection *corev1alpha1.Connection, credential store.ConnectorCredential) {
	now := metav1.NewTime(s.now().UTC())
	patch := client.MergeFrom(connection.DeepCopy())
	connection.Status.LastRefreshTime = &now
	connection.Status.ExpiresAt = nil
	if !credential.ExpiresAt.IsZero() {
		expires := metav1.NewTime(credential.ExpiresAt.UTC())
		connection.Status.ExpiresAt = &expires
	}
	if err := s.Client.Status().Patch(ctx, connection, patch); err != nil {
		log.FromContext(ctx).Info("connection refresh status could not be recorded", "connection", connection.Name)
	}
}

// markNotReady records a terminal link failure. The Ready reason is the
// durable record the controller projects state from.
func (s *Source) markNotReady(ctx context.Context, connection *corev1alpha1.Connection, reason, state, message string) {
	now := metav1.NewTime(s.now().UTC())
	patch := client.MergeFrom(connection.DeepCopy())
	connection.Status.State = state
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	})
	if err := s.Client.Status().Patch(ctx, connection, patch); err != nil {
		log.FromContext(ctx).Error(err, "connection status could not record link failure", "connection", connection.Name, "reason", reason)
	}
}

func oauthReason(err error) string {
	if oauthErr, ok := errors.AsType[*connectors.OAuthError](err); ok {
		return fmt.Sprintf("status=%d code=%s", oauthErr.StatusCode, oauthErr.Code)
	}
	return "transport"
}
