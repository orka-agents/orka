/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/store"
)

const (
	connectionRefreshInterval = 5 * time.Minute
	connectionRequeueInterval = 100 * time.Millisecond

	// ConnectionCustodyFinalizer holds a Connection until its sealed token
	// material is deleted and the provider token is revoked best-effort.
	ConnectionCustodyFinalizer = "core.orka.ai/connector-custody"

	connectionRevokeTimeout = 10 * time.Second
)

// ConnectorTokenRevoker revokes a token at the provider. It is satisfied by
// *connectors.OAuthClient and by test fakes.
type ConnectorTokenRevoker interface {
	Revoke(ctx context.Context, cfg connectors.OAuthProviderConfig, token string) error
}

// ConnectionReconciler resolves a Connection's provider, projects link state,
// and on deletion removes sealed custody and revokes the upstream token. It
// never writes token material anywhere.
type ConnectionReconciler struct {
	client.Client
	APIReader   client.Reader
	Scheme      *runtime.Scheme
	Credentials store.ConnectorCredentialStore
	Consents    store.ConnectorConsentStore
	Revoker     ConnectorTokenRevoker
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=connections,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=connections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=connections/finalizers,verbs=update

func (r *ConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	connection := &corev1alpha1.Connection{}
	if err := r.Get(ctx, req.NamespacedName, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !connection.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, connection)
	}
	if r.Credentials != nil && !controllerutil.ContainsFinalizer(connection, ConnectionCustodyFinalizer) {
		controllerutil.AddFinalizer(connection, ConnectionCustodyFinalizer)
		if err := r.Update(ctx, connection); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: connectionRequeueInterval}, nil
	}

	r.reapExpiredCompletions(ctx, connection)

	now := metav1.Now()
	providerResolved := metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionProviderResolved,
		Status:             metav1.ConditionTrue,
		Reason:             corev1alpha1.ConnectionReasonProviderResolved,
		Message:            "ConnectorProvider is accepted",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	}

	provider := &corev1alpha1.ConnectorProvider{}
	err := r.Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider)
	switch {
	case apierrors.IsNotFound(err):
		providerResolved.Status = metav1.ConditionFalse
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderMissing
		providerResolved.Message = "ConnectorProvider was not found"
	case err != nil:
		providerResolved.Status = metav1.ConditionUnknown
		providerResolved.Reason = connectors.ReasonResolutionFailed
		providerResolved.Message = "ConnectorProvider could not be read"
		return r.updateStatus(ctx, connection, providerResolved, err)
	case !connectors.ProviderAccepted(provider):
		providerResolved.Status = metav1.ConditionFalse
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderInvalid
		providerResolved.Message = "ConnectorProvider is not accepted for its current generation"
	}
	if providerResolved.Status == metav1.ConditionTrue {
		meta.SetStatusCondition(&connection.Status.Conditions, scopesGrantedCondition(connection, provider, now))
	}
	return r.updateStatus(ctx, connection, providerResolved, nil)
}

// scopesGrantedCondition compares the scopes granted at the last consent with
// the scopes the current mode and provider require. It never touches the
// consent-owned Ready condition, so widening projects Pending while the
// read-only consent stays valid, narrowing restores readiness, and a provider
// that starts requiring more scopes asks for consent again.
func scopesGrantedCondition(connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, now metav1.Time) metav1.Condition {
	condition := metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionScopesGranted,
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		condition.Status = metav1.ConditionFalse
		condition.Reason = corev1alpha1.ConnectionReasonPendingConsent
		condition.Message = "Consent has not completed"
		return condition
	}
	if !connectors.ConsentMatchesProvider(connection, provider) {
		// The held token belongs to another OAuth client (the provider was
		// replaced, or its client or endpoints changed); it must never be
		// refreshed against the new authority.
		condition.Status = metav1.ConditionFalse
		condition.Reason = corev1alpha1.ConnectionReasonConsentRequired
		condition.Message = "The provider's OAuth client changed since consent; consent again"
		return condition
	}
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	if connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, mode)) {
		condition.Status = metav1.ConditionTrue
		condition.Reason = corev1alpha1.ConnectionReasonScopesGranted
		condition.Message = "Granted scopes cover the " + mode + " mode"
		return condition
	}
	condition.Status = metav1.ConditionFalse
	condition.Reason = corev1alpha1.ConnectionReasonConsentRequired
	condition.Message = "Granted scopes do not cover the " + mode + " mode; consent again"
	return condition
}

// finalize deletes sealed custody, drops pending consents, revokes the
// provider token best-effort, and releases the finalizer. Custody deletion
// must succeed before the finalizer is removed; revocation failures are
// logged and do not block disconnect.
func (r *ConnectionReconciler) finalize(ctx context.Context, connection *corev1alpha1.Connection) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(connection, ConnectionCustodyFinalizer) {
		return ctrl.Result{}, nil
	}
	if r.Credentials != nil {
		r.revokeBestEffort(ctx, connection)
		if err := r.Credentials.DeleteConnectorCredential(ctx, string(connection.UID)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Consents != nil {
		// A callback may have parked a freshly issued token that the owner
		// never committed. It is still live upstream, so revoke it too.
		r.revokeParkedCompletions(ctx, connection)
		if err := r.Consents.DeleteConnectorConsentsForConnection(ctx, string(connection.UID)); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(connection, ConnectionCustodyFinalizer)
	if err := r.Update(ctx, connection); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ConnectionReconciler) revokeBestEffort(ctx context.Context, connection *corev1alpha1.Connection) {
	logger := log.FromContext(ctx)
	if r.Revoker == nil {
		return
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return
	}
	credential, err := r.Credentials.GetConnectorCredential(ctx, ref)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			logger.Info("connector credential could not be opened for revocation; deleting custody anyway", "connection", connection.Name)
		}
		return
	}
	if connection.Status.Consent == nil {
		logger.Info("connector credential has no consent record; not revoking against an unverified authority", "connection", connection.Name)
		return
	}
	r.revokeTokens(ctx, connection, credential, connection.Status.Consent.AuthorityDigest)
}

// reapExpiredCompletions revokes and deletes parked completions whose
// redemption window closed. Expired rows are never purged by the store
// itself because only this path can revoke the tokens they hold.
func (r *ConnectionReconciler) reapExpiredCompletions(ctx context.Context, connection *corev1alpha1.Connection) {
	if r.Consents == nil {
		return
	}
	completions, err := r.Consents.ListConnectorCompletionsForConnection(ctx, string(connection.UID))
	if err != nil {
		log.FromContext(ctx).Info("parked completions could not be listed for expiry", "connection", connection.Name)
		return
	}
	now := time.Now()
	committed := r.committedCredential(ctx, connection)
	for _, completion := range completions {
		if completion.ExpiresAt.After(now) {
			continue
		}
		// A completion whose token entered custody (its row outlived a
		// failed delete after commit) is active, not abandoned.
		if !sameConnectorCredential(committed, completion.Credential) {
			r.revokeTokens(ctx, connection, completion.Credential, completion.AuthorityDigest)
		}
		if err := r.Consents.DeleteConnectorCompletion(ctx, completion.Nonce); err != nil {
			log.FromContext(ctx).Info("expired completion could not be deleted", "connection", connection.Name)
		}
	}
}

// revokeParkedCompletions revokes tokens the callback obtained but the owner
// never committed. Failures are logged; the rows are deleted regardless.
func (r *ConnectionReconciler) revokeParkedCompletions(ctx context.Context, connection *corev1alpha1.Connection) {
	if r.Revoker == nil {
		return
	}
	completions, err := r.Consents.ListConnectorCompletionsForConnection(ctx, string(connection.UID))
	if err != nil {
		log.FromContext(ctx).Info("parked completions could not be listed for revocation", "connection", connection.Name)
		return
	}
	committed := r.committedCredential(ctx, connection)
	for _, completion := range completions {
		if sameConnectorCredential(committed, completion.Credential) {
			continue // revoked with the committed credential
		}
		r.revokeTokens(ctx, connection, completion.Credential, completion.AuthorityDigest)
	}
}

// committedCredential returns the committed custody material, or nil.
func (r *ConnectionReconciler) committedCredential(ctx context.Context, connection *corev1alpha1.Connection) *store.ConnectorCredential {
	if r.Credentials == nil {
		return nil
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return nil
	}
	credential, err := r.Credentials.GetConnectorCredential(ctx, ref)
	if err != nil {
		return nil
	}
	return &credential
}

func sameConnectorCredential(committed *store.ConnectorCredential, parked store.ConnectorCredential) bool {
	return committed != nil && committed.AccessToken != "" && committed.AccessToken == parked.AccessToken
}

// revokeTokens revokes the refresh then access token of one credential at
// the provider, best effort. The tokens are sent only to the OAuth authority
// that issued them: when the provider was replaced or its client changed since
// authorityDigest was recorded, nothing is sent.
func (r *ConnectionReconciler) revokeTokens(ctx context.Context, connection *corev1alpha1.Connection, credential store.ConnectorCredential, authorityDigest string) {
	if r.Revoker == nil {
		return
	}
	logger := log.FromContext(ctx)
	provider := &corev1alpha1.ConnectorProvider{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider); err != nil {
		return
	}
	if authorityDigest == "" || authorityDigest != connectors.ProviderAuthorityDigest(provider) {
		logger.Info("provider OAuth client changed since the token was issued; not revoking against a different authority",
			"connection", connection.Name, "provider", provider.Name)
		return
	}
	if strings.TrimSpace(provider.Spec.OAuth.RevocationURL) == "" {
		return
	}
	secret := &corev1.Secret{}
	secretRef := provider.Spec.OAuth.ClientSecretRef
	if err := r.referenceReader().Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: secretRef.Name}, secret); err != nil {
		return
	}
	// The secret is opaque bytes; only emptiness is judged, never trimmed.
	clientSecret := string(secret.Data[secretRef.Key])
	if strings.TrimSpace(clientSecret) == "" {
		return
	}
	revokeCtx, cancel := context.WithTimeout(ctx, connectionRevokeTimeout)
	defer cancel()
	cfg := connectors.ProviderOAuthConfig(provider, clientSecret)
	for _, token := range []string{credential.RefreshToken, credential.AccessToken} {
		if token == "" {
			continue
		}
		if err := r.Revoker.Revoke(revokeCtx, cfg, token); err != nil {
			logger.Info("provider token revocation failed; continuing with disconnect", "connection", connection.Name, "provider", provider.Name)
		}
	}
}

func (r *ConnectionReconciler) referenceReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ConnectionReconciler) updateStatus(
	ctx context.Context,
	connection *corev1alpha1.Connection,
	providerResolved metav1.Condition,
	reconcileErr error,
) (ctrl.Result, error) {
	before := connection.Status.DeepCopy()
	connection.Status.ObservedGeneration = connection.Generation
	meta.SetStatusCondition(&connection.Status.Conditions, providerResolved)
	connection.Status.State = projectConnectionState(connection, providerResolved)
	if reflect.DeepEqual(before, &connection.Status) {
		return ctrl.Result{RequeueAfter: connectionRefreshInterval}, reconcileErr
	}
	if err := r.Status().Update(ctx, connection); err != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, err)
	}
	return ctrl.Result{RequeueAfter: connectionRefreshInterval}, reconcileErr
}

// projectConnectionState derives the coarse state from the conditions. An
// unresolved provider is an Error; otherwise the Ready condition owned by the
// consent and refresh paths decides, qualified by the controller-owned
// ScopesGranted condition. Its reason, not the previously stored
// state, carries Expired and Revoked, so a provider outage that briefly
// projects Error cannot erase them.
func projectConnectionState(connection *corev1alpha1.Connection, providerResolved metav1.Condition) string {
	if providerResolved.Status != metav1.ConditionTrue {
		return corev1alpha1.ConnectionStateError
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil {
		return corev1alpha1.ConnectionStatePending
	}
	if ready.Status == metav1.ConditionTrue {
		granted := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted)
		if granted != nil && granted.Status != metav1.ConditionTrue {
			// Consent is valid but does not cover the current mode.
			return corev1alpha1.ConnectionStatePending
		}
		return corev1alpha1.ConnectionStateReady
	}
	switch ready.Reason {
	case corev1alpha1.ConnectionReasonExpired:
		return corev1alpha1.ConnectionStateExpired
	case corev1alpha1.ConnectionReasonRevoked:
		return corev1alpha1.ConnectionStateRevoked
	}
	return corev1alpha1.ConnectionStatePending
}

func (r *ConnectionReconciler) requestsForProvider(ctx context.Context, object client.Object) []reconcile.Request {
	connections := &corev1alpha1.ConnectionList{}
	if err := r.List(ctx, connections, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range connections.Items {
		connection := &connections.Items[i]
		if connection.Spec.ProviderRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: connection.Namespace, Name: connection.Name}})
		}
	}
	return requests
}

// SetupWithManager registers Connection and provider watches.
func (r *ConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Connection{}).
		Watches(&corev1alpha1.ConnectorProvider{}, handler.EnqueueRequestsFromMapFunc(r.requestsForProvider)).
		Named("connection").
		Complete(r)
}
