/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

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

const DefaultRefreshSkew = 60 * time.Second

type Revoker interface {
	Revoke(ctx context.Context, cfg connectors.OAuthProviderConfig, token string) error
}

type Refresher interface {
	Refresh(ctx context.Context, cfg connectors.OAuthProviderConfig, refreshToken string) (connectors.TokenResponse, error)
}

type Source struct {
	Client      client.Client
	APIReader   client.Reader
	Credentials store.ConnectorCredentialStore
	OAuth       Refresher
	Now         func() time.Time
	RefreshSkew time.Duration

	flights       singleflight.Group
	flightEntered func()
}

var (
	_ outboundaccess.ConnectionCredentialSource = (*Source)(nil)
	_ outboundaccess.ConnectionLivenessChecker  = (*Source)(nil)
)

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

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
	if req.Tool.Class == corev1alpha1.AgentRuntimeBrokeredToolClassWrite &&
		(connection.Spec.Mode == "" || connection.Spec.Mode == corev1alpha1.ConnectionModeReadOnly) {
		return outboundaccess.ConnectionCredential{}, errors.New("connection is readOnly; write tools are not available")
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	credential, err := s.Credentials.GetConnectorCredential(ctx, ref)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return outboundaccess.ConnectionCredential{}, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionNoCredential}
		}
		return outboundaccess.ConnectionCredential{}, err
	}
	if err := frozenGrantHolds(req, credential); err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	if _, err := s.validateProvider(ctx, connection, credential, &req.Tool); err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	horizon := s.refreshHorizon(req.Tool)
	if s.expiresWithin(credential, horizon) {
		credential, err = s.refreshSingleFlight(ctx, connection, ref, credential.GrantSequence, horizon)
		if err != nil {
			return outboundaccess.ConnectionCredential{}, err
		}
		if err := frozenGrantHolds(req, credential); err != nil {
			return outboundaccess.ConnectionCredential{}, err
		}
		if s.expiresWithin(credential, horizon) {
			return outboundaccess.ConnectionCredential{}, errors.New("the provider issued a token that expires before the tool's request timeout; retry or shorten the tool timeout")
		}
		if connection, err = s.loadLiveConnection(ctx, req); err != nil {
			return outboundaccess.ConnectionCredential{}, err
		}
		if _, err := s.validateProvider(ctx, connection, credential, &req.Tool); err != nil {
			return outboundaccess.ConnectionCredential{}, err
		}
	} else if connection, err = s.loadLiveConnection(ctx, req); err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	return outboundaccess.ConnectionCredential{
		AccessToken:   credential.AccessToken,
		TokenType:     credential.TokenType,
		ConnectionUID: string(connection.UID),
		Generation:    connection.Generation,
		Mode:          connection.Spec.Mode,
	}, nil
}

func (s *Source) RequireLiveConnection(ctx context.Context, req outboundaccess.ConnectionCredentialRequest) error {
	if s == nil || s.Client == nil || s.Credentials == nil {
		return errors.New("connection credential source is not configured")
	}
	if strings.TrimSpace(req.Frozen.UID) == "" {
		return errors.New("connection credential requires a frozen Connection binding")
	}
	connection, err := s.loadLiveConnection(ctx, req)
	if err != nil {
		return err
	}
	grants, ok := s.Credentials.(store.ConnectorGrantReader)
	if !ok {
		return errors.New("connector custody cannot report the grant it holds")
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return err
	}
	grant, err := grants.GetConnectorCredentialGrant(ctx, ref)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConnectorCustodyTombstoned) {
		return &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionNoCredential}
	}
	if err != nil {
		return err
	}
	if grant.Sequence != req.Frozen.GrantSequence {
		return &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionRelinked}
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider); err != nil {
		return errors.New("connector provider is unavailable")
	}
	if !connectors.ProviderAccepted(provider) {
		return errors.New("connector provider is not accepted")
	}
	if !connectors.ConsentMatchesProvider(connection, provider) {
		return errors.New("connector provider changed since consent; the person must consent again")
	}
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	required := connectors.ScopesForMode(provider, mode)
	if !connectors.ScopesCover(connection.Status.GrantedScopes, required) || !connectors.ScopesCover(grant.Scopes, required) {
		return errors.New("the provider now requires scopes the person did not grant; the person must consent again")
	}
	if _, err := outboundaccess.DeclaredConnectorTool(provider, req.Tool); err != nil {
		return err
	}
	if _, err := s.loadLiveConnection(ctx, req); err != nil {
		return err
	}
	return nil
}

func (s *Source) loadLiveConnection(ctx context.Context, req outboundaccess.ConnectionCredentialRequest) (*corev1alpha1.Connection, error) {
	name := req.Frozen.Name
	if name == "" {
		name = connectors.ConnectionName(req.Provider, req.Issuer, req.Subject)
	}
	connection := &corev1alpha1.Connection{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: name}, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionNotLinked}
		}
		return nil, fmt.Errorf("load connection: %w", err)
	}
	if connection.Spec.Subject.Issuer != req.Issuer || connection.Spec.Subject.Subject != req.Subject ||
		connection.Spec.ProviderRef.Name != req.Provider {
		return nil, errors.New("connection does not belong to the requester")
	}
	if !connection.DeletionTimestamp.IsZero() {
		return nil, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionDeleting}
	}
	if string(connection.UID) != req.Frozen.UID || connection.Generation != req.Frozen.Generation {
		return nil, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionChanged}
	}
	if !connectors.ConnectionLinked(connection) {
		if connection.Status.State == corev1alpha1.ConnectionStateExpired {
			return nil, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionExpired}
		}
		return nil, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionNotReady, State: connection.Status.State}
	}
	if req.Frozen.GrantSequence <= 0 || connection.Status.GrantSequence != req.Frozen.GrantSequence {
		return nil, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionRelinked}
	}
	return connection, nil
}

func (s *Source) needsRefresh(credential store.ConnectorCredential) bool {
	return s.expiresWithin(credential, s.skew())
}

func (s *Source) expiresWithin(credential store.ConnectorCredential, horizon time.Duration) bool {
	return !credential.ExpiresAt.IsZero() && !credential.ExpiresAt.After(s.now().Add(horizon))
}

func (s *Source) refreshHorizon(tool outboundaccess.ToolBinding) time.Duration {
	horizon := s.skew()
	if timeout := outboundaccess.NormalizedConnectorTimeout(tool.Timeout); timeout > horizon {
		horizon = timeout
	}
	return horizon
}

func (s *Source) refreshSingleFlight(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, grant int64, horizon time.Duration) (store.ConnectorCredential, error) {
	results := s.flights.DoChan(fmt.Sprintf("%s/%d", connection.UID, grant), func() (any, error) {
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		current, err := s.Credentials.GetConnectorCredential(flightCtx, ref)
		if err != nil {
			return nil, err
		}
		if current.GrantSequence != grant || !s.expiresWithin(current, horizon) {
			return current, nil
		}
		return s.refresh(flightCtx, connection, ref, current, horizon)
	})
	if s.flightEntered != nil {
		s.flightEntered()
	}
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

func (s *Source) refresh(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, current store.ConnectorCredential, horizon time.Duration) (store.ConnectorCredential, error) {
	logger := log.FromContext(ctx).WithValues("connection", connection.Name, "provider", connection.Spec.ProviderRef.Name)
	if strings.TrimSpace(current.RefreshToken) == "" {
		if !s.needsRefresh(current) {
			return store.ConnectorCredential{}, errors.New("connection credential cannot be refreshed and expires before the tool's request timeout; shorten the tool timeout or reconnect")
		}
		s.markNotReady(ctx, connection, ref, current.Version, corev1alpha1.ConnectionReasonExpired, corev1alpha1.ConnectionStateExpired,
			"Access token expired and the provider issued no refresh token")
		return store.ConnectorCredential{}, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionExpired}
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
			shredErr := s.Credentials.ShredConnectorCredential(ctx, string(connection.UID), current.Version)
			if errors.Is(shredErr, store.ErrConflict) {
				winner, err := s.Credentials.GetConnectorCredential(ctx, ref)
				if err != nil {
					return store.ConnectorCredential{}, err
				}
				if s.expiresWithin(winner, horizon) {
					return store.ConnectorCredential{}, errors.New("connection credential changed concurrently and is about to expire; retry")
				}
				return winner, nil
			}
			if shredErr != nil {
				logger.Error(shredErr, "connector custody could not be shredded after revocation")
			}
			s.markNotReady(ctx, connection, ref, current.Version, corev1alpha1.ConnectionReasonRevoked, corev1alpha1.ConnectionStateRevoked,
				"The provider rejected the refresh token; the person must reconnect")
			return store.ConnectorCredential{}, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionRevoked}
		}
		logger.Info("connection refresh failed transiently", "reason", oauthReason(err))
		return store.ConnectorCredential{}, errors.New("connection credential refresh failed")
	}
	refreshed := store.ConnectorCredential{
		AccessToken:      token.AccessToken,
		RefreshToken:     token.RefreshToken,
		TokenType:        token.TokenType,
		ExpiresAt:        token.ExpiresAt,
		Scopes:           token.Scopes,
		AuthorityDigest:  current.AuthorityDigest,
		GrantSequence:    current.GrantSequence,
		RevocationDigest: current.RevocationDigest,
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	if refreshed.TokenType == "" {
		refreshed.TokenType = current.TokenType
	}
	if !token.ScopePresent {
		refreshed.Scopes = current.Scopes
	}
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	switch err := s.Credentials.ReplaceConnectorCredential(ctx, ref, refreshed, current.Version); {
	case errors.Is(err, store.ErrConflict):
		return s.refreshLostToConsent(ctx, cfg, provider, ref, refreshed, horizon, logger)
	case errors.Is(err, store.ErrConnectorCustodyTombstoned), errors.Is(err, store.ErrNotFound):
		s.revokeUnstorable(ctx, cfg, provider, refreshed, logger)
		return store.ConnectorCredential{}, &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionNoCredential}
	case err != nil:
		switch retireErr := s.retireRefreshed(ctx, ref, refreshed); {
		case errors.Is(retireErr, store.ErrConnectorCustodyTombstoned):
			s.revokeUnstorable(ctx, cfg, provider, refreshed, logger)
		case retireErr != nil:
			logger.Info("refreshed credential could not be stored or retired; it is left to expire")
		}
		return store.ConnectorCredential{}, fmt.Errorf("store refreshed connection credential: %w", err)
	}
	stored, err := s.Credentials.GetConnectorCredential(ctx, ref)
	if err != nil {
		return store.ConnectorCredential{}, fmt.Errorf("re-read refreshed connection credential: %w", err)
	}
	if stored.AccessToken != refreshed.AccessToken || stored.RefreshToken != refreshed.RefreshToken || stored.GrantSequence != refreshed.GrantSequence {
		return store.ConnectorCredential{}, errors.New("connection credential changed concurrently; retry")
	}
	refreshed = stored
	if !connectors.ScopesCover(refreshed.Scopes, connectors.ScopesForMode(provider, mode)) {
		s.recordNarrowedScopes(ctx, connection, ref, refreshed, mode)
		return store.ConnectorCredential{}, errors.New("refreshed connection credential no longer covers the connection mode; the person must consent again")
	}
	s.recordRefresh(ctx, connection, ref, refreshed)
	if s.expiresWithin(refreshed, horizon) {
		return store.ConnectorCredential{}, errors.New("the provider issued a token that expires within the refresh window or before the tool's request timeout; retry or shorten the tool timeout")
	}
	return refreshed, nil
}

func (s *Source) retireRefreshed(ctx context.Context, ref store.ConnectorCredentialRef, refreshed store.ConnectorCredential) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshRetireTimeout)
	defer cancel()
	var err error
	for attempt := range refreshRetireAttempts {
		if attempt > 0 {
			select {
			case <-time.After(refreshRetireBackoff << (attempt - 1)):
			case <-ctx.Done():
				return fmt.Errorf("retire refreshed connection credential: %w", ctx.Err())
			}
		}
		if err = s.Credentials.RetireConnectorCredential(ctx, ref, refreshed); err == nil || errors.Is(err, store.ErrConnectorCustodyTombstoned) {
			return err
		}
	}
	return err
}

const refreshRetireAttempts = 3

var (
	refreshRetireTimeout = 10 * time.Second
	refreshRetireBackoff = 100 * time.Millisecond
)

func (s *Source) revokeUnstorable(ctx context.Context, cfg connectors.OAuthProviderConfig, provider *corev1alpha1.ConnectorProvider, credential store.ConnectorCredential, logger logr.Logger) {
	revoker, ok := s.OAuth.(Revoker)
	if !ok {
		logger.Info("refreshed credential could not be stored after disconnect and the OAuth client cannot revoke it")
		return
	}
	if credential.RevocationDigest == "" || credential.RevocationDigest != connectors.ProviderRevocationDigest(provider) {
		logger.Info("refreshed credential could not be stored and the provider's revocation authority changed since consent; the tokens are left to expire")
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

func (s *Source) validateProvider(ctx context.Context, connection *corev1alpha1.Connection, credential store.ConnectorCredential, tool *outboundaccess.ToolBinding) (*corev1alpha1.ConnectorProvider, error) {
	provider := &corev1alpha1.ConnectorProvider{}
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider); err != nil {
		return nil, errors.New("connector provider is unavailable")
	}
	if err := checkProvider(connection, provider, credential, tool); err != nil {
		return nil, err
	}
	return provider, nil
}

func checkProvider(connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider,
	credential store.ConnectorCredential, tool *outboundaccess.ToolBinding) error {
	if !connectors.ProviderAccepted(provider) {
		return errors.New("connector provider is not accepted")
	}
	if credential.AuthorityDigest == "" || credential.AuthorityDigest != connectors.ProviderIssuerDigest(provider) {
		return &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionClientChanged}
	}
	if !connectors.ConsentMatchesProvider(connection, provider) {
		return errors.New("connector provider changed since consent; the person must consent again")
	}
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	if !connectors.ScopesCover(credential.Scopes, connectors.ScopesForMode(provider, mode)) {
		return errors.New("connection credential does not cover the connection mode; the person must consent again")
	}
	if tool != nil {
		if _, err := outboundaccess.DeclaredConnectorTool(provider, *tool); err != nil {
			return err
		}
	}
	return nil
}

func (s *Source) providerConfig(ctx context.Context, provider *corev1alpha1.ConnectorProvider) (connectors.OAuthProviderConfig, error) {
	reader := s.reader()
	secretRef := provider.Spec.OAuth.ClientSecretRef
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: secretRef.Name}, secret); err != nil {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is unavailable")
	}
	value := string(secret.Data[secretRef.Key])
	if strings.TrimSpace(value) == "" {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is empty")
	}
	return connectors.ProviderOAuthConfig(provider, value), nil
}

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

func (s *Source) recordRefresh(ctx context.Context, connection *corev1alpha1.Connection, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) {
	logger := log.FromContext(ctx).WithValues("connection", connection.Name)
	const maxAttempts = 4
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

func frozenGrantHolds(req outboundaccess.ConnectionCredentialRequest, credential store.ConnectorCredential) error {
	if req.Frozen.GrantSequence <= 0 || credential.GrantSequence != req.Frozen.GrantSequence {
		return &outboundaccess.ConnectionRequiredError{Kind: outboundaccess.ConnectionRelinked}
	}
	return nil
}

func (s *Source) refreshLostToConsent(ctx context.Context, cfg connectors.OAuthProviderConfig, provider *corev1alpha1.ConnectorProvider, ref store.ConnectorCredentialRef, refreshed store.ConnectorCredential, horizon time.Duration, logger logr.Logger) (store.ConnectorCredential, error) {
	winner, err := s.Credentials.GetConnectorCredential(ctx, ref)
	if err != nil {
		return store.ConnectorCredential{}, err
	}
	if winner.AccessToken != refreshed.AccessToken || winner.RefreshToken != refreshed.RefreshToken {
		switch err := s.retireRefreshed(ctx, ref, refreshed); {
		case errors.Is(err, store.ErrConnectorCustodyTombstoned):
			s.revokeUnstorable(ctx, cfg, provider, refreshed, logger)
		case err != nil:
			logger.Info("refreshed credential that lost to a consent could not be retired; it is left to expire")
		}
	}
	if s.expiresWithin(winner, horizon) {
		return store.ConnectorCredential{}, errors.New("connection credential changed concurrently and is about to expire; retry")
	}
	return winner, nil
}

func (s *Source) ResolveGitHubPersonCredential(ctx context.Context, namespace, provider, issuer, subject string, horizon time.Duration) (outboundaccess.ConnectionCredential, error) {
	if s == nil || s.Client == nil || s.Credentials == nil {
		return outboundaccess.ConnectionCredential{}, errors.New("connection credential source is not configured")
	}
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(subject) == "" {
		return outboundaccess.ConnectionCredential{}, errors.New("connection credential requires a verified person")
	}
	load := func() (*corev1alpha1.Connection, error) {
		connection := &corev1alpha1.Connection{}
		name := connectors.ConnectionName(provider, issuer, subject)
		if err := s.reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, connection); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, ErrNoPersonConnection
			}
			return nil, fmt.Errorf("load connection: %w", err)
		}
		if connection.Spec.Subject.Issuer != issuer || connection.Spec.Subject.Subject != subject || connection.Spec.ProviderRef.Name != provider {
			return nil, errors.New("connection does not belong to the person")
		}
		if !connection.DeletionTimestamp.IsZero() || !connectors.ConnectionLinked(connection) {
			return nil, errors.New("connection is not ready")
		}
		return connection, nil
	}
	connection, err := load()
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
	validate := func(connection *corev1alpha1.Connection, credential store.ConnectorCredential) error {
		if string(connection.UID) != ref.ConnectionUID {
			return errors.New("connection was replaced; try again")
		}
		if credential.GrantSequence <= 0 || credential.GrantSequence != connection.Status.GrantSequence {
			return errors.New("connection was re-linked; try again")
		}
		accepted, err := s.validateProvider(ctx, connection, credential, nil)
		if err != nil {
			return err
		}
		if !connectors.ProviderIssuesGitHubCredentials(accepted) {
			return errors.New("connector provider does not issue github.com credentials")
		}
		return nil
	}
	if err := validate(connection, credential); err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	if s.expiresWithin(credential, horizon) {
		if credential, err = s.refreshSingleFlight(ctx, connection, ref, credential.GrantSequence, horizon); err != nil {
			return outboundaccess.ConnectionCredential{}, err
		}
		if s.expiresWithin(credential, horizon) {
			return outboundaccess.ConnectionCredential{}, errors.New("the provider issued a token that expires too soon")
		}
	}
	if connection, err = load(); err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	if err := validate(connection, credential); err != nil {
		return outboundaccess.ConnectionCredential{}, err
	}
	return outboundaccess.ConnectionCredential{
		AccessToken: credential.AccessToken, TokenType: credential.TokenType,
		ConnectionUID: string(connection.UID), Generation: connection.Generation, Mode: connection.Spec.Mode,
	}, nil
}

var ErrNoPersonConnection = errors.New("the person has no connection to this provider")
