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

	"github.com/go-logr/logr"

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
// Revoker revokes a token at the provider. *connectors.OAuthClient
// implements it; a Source whose OAuth client does not can only let tokens
// it could not store expire.
type Revoker interface {
	Revoke(ctx context.Context, cfg connectors.OAuthProviderConfig, token string) error
}

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
	// The provider is judged on every resolution, not only when refreshing:
	// status can trail a provider change by one reconcile, and the held
	// material must never be released against a client or destination set
	// the person did not consent to.
	if _, err := s.validateProvider(ctx, connection, credential, &req.Tool); err != nil {
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
		if _, err := s.validateProvider(ctx, connection, credential, &req.Tool); err != nil {
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
	if req.Frozen.GrantSequence <= 0 || connection.Status.GrantSequence != req.Frozen.GrantSequence {
		return nil, errors.New("connection was re-linked since the task was dispatched; re-dispatch to use it")
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
	results := s.flights.DoChan(string(connection.UID), func() (any, error) {
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
	// The shared flight keeps running for the callers that still need it,
	// but each caller returns as soon as its own context is done.
	var flight singleflight.Result
	select {
	case flight = <-results:
	case <-ctx.Done():
		return store.ConnectorCredential{}, fmt.Errorf("connection credential refresh abandoned: %w", ctx.Err())
	}
	if flight.Err != nil {
		return store.ConnectorCredential{}, flight.Err
	}
	credential, ok := flight.Val.(store.ConnectorCredential)
	if !ok {
		return store.ConnectorCredential{}, errors.New("connection refresh returned an unexpected result")
	}
	return credential, nil
}

func (s *Source) refresh(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, current store.ConnectorCredential) (store.ConnectorCredential, error) {
	logger := log.FromContext(ctx).WithValues("connection", connection.Name, "provider", connection.Spec.ProviderRef.Name)
	if strings.TrimSpace(current.RefreshToken) == "" {
		s.markNotReady(ctx, connection, ref, current.Version, corev1alpha1.ConnectionReasonExpired, corev1alpha1.ConnectionStateExpired,
			"Access token expired and the provider issued no refresh token")
		return store.ConnectorCredential{}, errors.New("connection credential expired and cannot be refreshed; the person must reconnect")
	}
	if s.OAuth == nil {
		return store.ConnectorCredential{}, errors.New("connection credential refresh is not configured")
	}
	provider, err := s.validateProvider(ctx, connection, current, nil)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	cfg, err := s.providerConfig(ctx, provider)
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
				// A consent won the race; its material is released only if it
				// is not itself about to expire, as any resolution would judge.
				winner, err := s.Credentials.GetConnectorCredential(ctx, ref)
				if err != nil {
					return store.ConnectorCredential{}, err
				}
				if s.needsRefresh(winner) {
					return store.ConnectorCredential{}, errors.New("connection credential changed concurrently and is about to expire; retry")
				}
				return winner, nil
			}
			if shredErr != nil {
				logger.Error(shredErr, "connector custody could not be shredded after revocation")
			}
			s.markNotReady(ctx, connection, ref, current.Version, corev1alpha1.ConnectionReasonRevoked, corev1alpha1.ConnectionStateRevoked,
				"The provider rejected the refresh token; the person must reconnect")
			return store.ConnectorCredential{}, errors.New("connection was revoked by the provider; the person must reconnect")
		}
		logger.Info("connection refresh failed transiently", "reason", oauthReason(err))
		return store.ConnectorCredential{}, errors.New("connection credential refresh failed")
	}
	refreshed := store.ConnectorCredential{
		AccessToken:     token.AccessToken,
		RefreshToken:    token.RefreshToken,
		TokenType:       token.TokenType,
		ExpiresAt:       token.ExpiresAt,
		Scopes:          token.Scopes,
		AuthorityDigest: current.AuthorityDigest,
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	if refreshed.TokenType == "" {
		refreshed.TokenType = current.TokenType
	}
	// Only an omitted scope field inherits the previous grant; an explicitly
	// empty one is a grant of nothing and fails the mode check below.
	if !token.ScopePresent {
		refreshed.Scopes = current.Scopes
	}
	// A provider may narrow the scopes on refresh. The narrowed grant is
	// recorded so the controller re-judges ScopesGranted, and nothing is
	// released for a mode the new token no longer covers.
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	// The rotated material is sealed first, whatever the verdict below: the
	// provider may have invalidated the previous refresh token, and only
	// what custody holds can later be revoked at disconnect or re-consent.
	// Fenced on the version read at flight start: a consent that completed
	// meanwhile wins, and its material is returned instead.
	switch err := s.Credentials.ReplaceConnectorCredential(ctx, ref, refreshed, current.Version); {
	case errors.Is(err, store.ErrConflict):
		// A consent won the race. Its material is released only if it is
		// not itself about to expire; otherwise this call fails and the
		// next one refreshes the winner as any resolution would. The pair
		// this refresh obtained cannot be stored and derives from this
		// Connection's own committed grant, so it is revoked rather than
		// left live outside custody.
		winner, err := s.Credentials.GetConnectorCredential(ctx, ref)
		if err != nil {
			return store.ConnectorCredential{}, err
		}
		if winner.AccessToken != refreshed.AccessToken || winner.RefreshToken != refreshed.RefreshToken {
			s.revokeUnstorable(ctx, cfg, refreshed, logger)
		}
		if s.needsRefresh(winner) {
			return store.ConnectorCredential{}, errors.New("connection credential changed concurrently and is about to expire; retry")
		}
		return winner, nil
	case errors.Is(err, store.ErrConnectorCustodyTombstoned), errors.Is(err, store.ErrNotFound):
		// Disconnect fenced custody while the provider was rotating the
		// material. The rotated pair derives from this Connection's own
		// committed grant, so its ownership is proven and it is revoked
		// rather than left live outside the disconnect's revocation set.
		s.revokeUnstorable(ctx, cfg, refreshed, logger)
		return store.ConnectorCredential{}, errors.New("connection was disconnected during refresh")
	case err != nil:
		return store.ConnectorCredential{}, fmt.Errorf("store refreshed connection credential: %w", err)
	}
	// The version the replacement received comes from the store's sequence.
	if stored, err := s.Credentials.GetConnectorCredential(ctx, ref); err == nil {
		refreshed.Version = stored.Version
	}
	if !connectors.ScopesCover(refreshed.Scopes, connectors.ScopesForMode(provider, mode)) {
		s.recordNarrowedScopes(ctx, connection, ref, refreshed, mode)
		return store.ConnectorCredential{}, errors.New("refreshed connection credential no longer covers the connection mode; the person must consent again")
	}
	s.recordRefresh(ctx, connection, ref, refreshed)
	// A token the provider issued already inside the refresh skew would
	// expire mid-call; it is not released, and the next call refreshes again.
	if s.needsRefresh(refreshed) {
		return store.ConnectorCredential{}, errors.New("the provider issued a token that expires within the refresh window; retry")
	}
	return refreshed, nil
}

// revokeUnstorable revokes, best effort, a refreshed credential that custody
// refused because the Connection was disconnected meanwhile.
func (s *Source) revokeUnstorable(ctx context.Context, cfg connectors.OAuthProviderConfig, credential store.ConnectorCredential, logger logr.Logger) {
	revoker, ok := s.OAuth.(Revoker)
	if !ok {
		logger.Info("refreshed credential could not be stored after disconnect and the OAuth client cannot revoke it")
		return
	}
	for _, token := range []string{credential.RefreshToken, credential.AccessToken} {
		if token == "" {
			continue
		}
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err := revoker.Revoke(revokeCtx, cfg, token)
		cancel()
		if err != nil {
			logger.Info("refreshed credential could not be revoked after disconnect")
		}
	}
}

// validateProvider loads the Connection's provider uncached and checks that
// the held credential may be used against it: the provider is accepted, the
// credential was issued by this OAuth client (issuer digest), the person's
// consent covers the provider's current authority (client plus tool
// destinations), and the granted scopes cover the mode.
func (s *Source) validateProvider(ctx context.Context, connection *corev1alpha1.Connection, credential store.ConnectorCredential, tool *outboundaccess.ToolBinding) (*corev1alpha1.ConnectorProvider, error) {
	provider := &corev1alpha1.ConnectorProvider{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider); err != nil {
		return nil, errors.New("connector provider is unavailable")
	}
	if !connectors.ProviderAccepted(provider) {
		return nil, errors.New("connector provider is not accepted")
	}
	// The token was issued by the OAuth client sealed with it (the issuer
	// digest: client identity and endpoints, not tool destinations); a
	// replaced provider or rotated client must never receive or use it.
	if credential.AuthorityDigest == "" || credential.AuthorityDigest != connectors.ProviderIssuerDigest(provider) {
		return nil, errors.New("connector provider OAuth client changed since the token was issued; the person must reconnect")
	}
	if !connectors.ConsentMatchesProvider(connection, provider) {
		return nil, errors.New("connector provider changed since consent; the person must consent again")
	}
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	if !connectors.ScopesCover(credential.Scopes, connectors.ScopesForMode(provider, mode)) {
		return nil, errors.New("connection credential does not cover the connection mode; the person must consent again")
	}
	// The executing Tool is judged against this same provider state, so a
	// credential returned after a re-consent on a retargeted provider can
	// never be paired with a destination that provider no longer declares.
	if tool != nil {
		if _, err := outboundaccess.DeclaredConnectorTool(provider, *tool); err != nil {
			return nil, err
		}
	}
	return provider, nil
}

func (s *Source) providerConfig(ctx context.Context, provider *corev1alpha1.ConnectorProvider) (connectors.OAuthProviderConfig, error) {
	reader := s.reader()
	secretRef := provider.Spec.OAuth.ClientSecretRef
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: secretRef.Name}, secret); err != nil {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is unavailable")
	}
	// The secret is opaque bytes; only emptiness is judged, never trimmed.
	value := string(secret.Data[secretRef.Key])
	if strings.TrimSpace(value) == "" {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is empty")
	}
	return connectors.ProviderOAuthConfig(provider, value), nil
}

// recordNarrowedScopes stores the scopes a refresh actually returned and
// withdraws ScopesGranted for the current mode. The controller recomputes the
// condition from the same field on its next pass. Like markNotReady, a
// conflict with an unrelated writer is retried while custody still holds
// the narrowed material, or status would keep advertising the wider grant.
func (s *Source) recordNarrowedScopes(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, narrowed store.ConnectorCredential, mode string) {
	logger := log.FromContext(ctx).WithValues("connection", connection.Name)
	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		now := metav1.NewTime(s.now().UTC())
		patch := client.MergeFromWithOptions(connection.DeepCopy(), client.MergeFromWithOptimisticLock{})
		connection.Status.GrantedScopes = append([]string(nil), narrowed.Scopes...)
		connection.Status.State = corev1alpha1.ConnectionStatePending
		meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
			Type:               corev1alpha1.ConnectionConditionScopesGranted,
			Status:             metav1.ConditionFalse,
			Reason:             corev1alpha1.ConnectionReasonConsentRequired,
			Message:            "The refreshed token no longer covers the " + mode + " mode; consent again",
			ObservedGeneration: connection.Generation,
			LastTransitionTime: now,
		})
		err := s.Client.Status().Patch(ctx, connection, patch)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			logger.Info("connection narrowed scopes could not be recorded", "reason", err.Error())
			return
		}
		if attempt >= maxAttempts || !s.verdictStillCurrent(ctx, connection, ref, narrowed.Version) {
			logger.Info("connection changed concurrently; leaving narrowed scopes to the newer writer")
			return
		}
	}
}

// recordRefresh updates non-secret status after a successful refresh. A
// failed status write is logged; the refreshed material is already safe in
// custody and the call may proceed.
func (s *Source) recordRefresh(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) {
	logger := log.FromContext(ctx).WithValues("connection", connection.Name)
	const maxAttempts = 4
	// Fenced on the resourceVersion read at flight start: a consent that
	// completed meanwhile has already rewritten status, and this stale
	// refresh must not overwrite its scopes or expiry. Any other concurrent
	// writer is retried while custody still holds this refreshed material,
	// or scopes and expiry would stay stale indefinitely.
	for attempt := 1; ; attempt++ {
		now := metav1.NewTime(s.now().UTC())
		patch := client.MergeFromWithOptions(connection.DeepCopy(), client.MergeFromWithOptimisticLock{})
		connection.Status.GrantedScopes = append([]string(nil), credential.Scopes...)
		connection.Status.LastRefreshTime = &now
		connection.Status.ExpiresAt = nil
		if !credential.ExpiresAt.IsZero() {
			expires := metav1.NewTime(credential.ExpiresAt.UTC())
			connection.Status.ExpiresAt = &expires
		}
		err := s.Client.Status().Patch(ctx, connection, patch)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			logger.Info("connection refresh status could not be recorded")
			return
		}
		if attempt >= maxAttempts || !s.verdictStillCurrent(ctx, connection, ref, credential.Version) {
			logger.Info("connection changed concurrently; leaving refresh status to the newer writer")
			return
		}
	}
}

// markNotReady records a terminal link failure. The Ready reason is the
// durable record the controller projects state from. The patch is fenced on
// the resourceVersion read at flight start: a consent that completed after
// the custody shred has already rewritten status, and this stale verdict
// must not overwrite it. A conflict caused by any other writer (a periodic
// reconciler pass, say) is retried against the re-read Connection as long as
// custody still holds nothing newer than the material this verdict judged;
// otherwise a Ready link with no usable credential could persist.
func (s *Source) markNotReady(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, judgedVersion int64, reason, state, message string) {
	logger := log.FromContext(ctx).WithValues("connection", connection.Name, "reason", reason)
	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		now := metav1.NewTime(s.now().UTC())
		patch := client.MergeFromWithOptions(connection.DeepCopy(), client.MergeFromWithOptimisticLock{})
		connection.Status.State = state
		meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
			Type:               corev1alpha1.ConnectionConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: connection.Generation,
			LastTransitionTime: now,
		})
		err := s.Client.Status().Patch(ctx, connection, patch)
		if err == nil {
			return
		}
		if !apierrors.IsConflict(err) {
			logger.Error(err, "connection status could not record link failure")
			return
		}
		if attempt >= maxAttempts || !s.verdictStillCurrent(ctx, connection, ref, judgedVersion) {
			logger.Info("connection changed concurrently; leaving status to the newer writer")
			return
		}
	}
}

// verdictStillCurrent re-reads the Connection for another markNotReady
// attempt and reports whether the judged material is still what custody
// holds: nothing (shredded and not re-committed) or the same version.
// Versions never repeat for a Connection, even across a shred and
// re-consent, so any other version means a consent or refresh won and its
// status stands.
func (s *Source) verdictStillCurrent(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, judgedVersion int64) bool {
	fresh := &corev1alpha1.Connection{}
	if err := s.reader().Get(ctx, client.ObjectKeyFromObject(connection), fresh); err != nil {
		return false
	}
	if fresh.UID != connection.UID || fresh.Generation != connection.Generation || !fresh.DeletionTimestamp.IsZero() {
		return false
	}
	held, err := s.Credentials.GetConnectorCredential(ctx, ref)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return false
	case held.Version != judgedVersion:
		return false
	}
	*connection = *fresh
	return true
}

func oauthReason(err error) string {
	if oauthErr, ok := errors.AsType[*connectors.OAuthError](err); ok {
		return fmt.Sprintf("status=%d code=%s", oauthErr.StatusCode, oauthErr.Code)
	}
	return "transport"
}
