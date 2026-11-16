/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
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

// +kubebuilder:rbac:groups=core.orka.ai,resources=connections,verbs=get;list;watch;create;update;patch;delete
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
	// The status as read, before any pass mutates it: the write decision
	// compares against this, so a change made only to ScopesGranted or by
	// a recovered completion is persisted rather than mistaken for no change.
	before := connection.Status.DeepCopy()
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

	// The index labels are what the API lists by; an object created
	// outside the API, or with a label stripped, is repaired here so the
	// person's listing never misses a link it owns.
	if want := connectors.ConnectionLabels(connection); !labelsPresent(connection.Labels, want) {
		base := connection.DeepCopy()
		if connection.Labels == nil {
			connection.Labels = map[string]string{}
		}
		maps.Copy(connection.Labels, want)
		if err := r.Patch(ctx, connection, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	completionDeadline := r.reapExpiredCompletions(ctx, connection)

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
	var err error
	// A name no provider could ever have is a stable invalid reference, not
	// a read failure to retry forever.
	if len(k8svalidation.IsDNS1123Subdomain(connection.Spec.ProviderRef.Name)) > 0 {
		err = apierrors.NewNotFound(corev1alpha1.GroupVersion.WithResource("connectorproviders").GroupResource(), connection.Spec.ProviderRef.Name)
	} else {
		err = r.Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider)
	}
	switch {
	case apierrors.IsNotFound(err):
		providerResolved.Status = metav1.ConditionFalse
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderMissing
		providerResolved.Message = "ConnectorProvider was not found"
	case err != nil:
		providerResolved.Status = metav1.ConditionUnknown
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderReadFailed
		providerResolved.Message = "ConnectorProvider could not be read"
		meta.SetStatusCondition(&connection.Status.Conditions, scopesUnknownCondition(connection, now))
		return r.updateStatus(ctx, connection, before, providerResolved, err)
	case !connectors.ProviderAccepted(provider):
		providerResolved.Status = metav1.ConditionFalse
		providerResolved.Reason = corev1alpha1.ConnectionReasonProviderInvalid
		providerResolved.Message = "ConnectorProvider is not accepted for its current generation"
	}
	var applied []string
	if providerResolved.Status == metav1.ConditionTrue {
		meta.SetStatusCondition(&connection.Status.Conditions, scopesGrantedCondition(connection, provider, now))
		applied = r.applyCommittedCompletions(ctx, connection, provider, now)
	} else {
		meta.SetStatusCondition(&connection.Status.Conditions, scopesUnknownCondition(connection, now))
	}
	r.expireLinkedCredential(ctx, connection, now)
	result, err := r.updateStatus(ctx, connection, before, providerResolved, nil)
	if err == nil {
		// The completion is the durable record that lets a lost status write
		// be repaired; it goes only once the recovered status is persisted.
		r.deleteCompletions(ctx, connection, applied)
		result.RequeueAfter = connectionNextPass(connection, time.Now(), completionDeadline)
	}
	return result, err
}

func (r *ConnectionReconciler) deleteCompletions(ctx context.Context, connection *corev1alpha1.Connection, nonces []string) {
	for _, nonce := range nonces {
		if err := r.Consents.DeleteConnectorCompletion(ctx, nonce); err != nil {
			log.FromContext(ctx).Info("finished completion could not be removed", "connection", connection.Name)
		}
	}
}

// expireLinkedCredential withdraws Ready from a link whose only material is
// an access token that has passed its expiry and cannot be refreshed. The
// credential source records the same verdict when a call happens to hit it;
// a link nothing calls must not advertise itself as usable forever.
func (r *ConnectionReconciler) expireLinkedCredential(ctx context.Context, connection *corev1alpha1.Connection, now metav1.Time) {
	if r.Credentials == nil || connection.Status.ExpiresAt == nil || connection.Status.ExpiresAt.After(now.Time) {
		return
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return
	}
	credential, err := r.Credentials.GetConnectorCredential(ctx, ref)
	if err != nil || credential.RefreshToken != "" || credential.ExpiresAt.IsZero() || credential.ExpiresAt.After(now.Time) {
		return
	}
	connection.Status.State = corev1alpha1.ConnectionStateExpired
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             corev1alpha1.ConnectionReasonExpired,
		Message:            "Access token expired and the provider issued no refresh token",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	})
}

// applyCommittedCompletions finishes a completion whose custody write
// succeeded but whose status write never did (the client lost its retry
// window). The committed marker is authoritative: while custody still holds
// exactly that material, the link is recorded from it, and the nonce is
// returned so the caller removes the row only after the status is
// persisted; a completion custody or the provider no longer matches is
// dropped at once.
func (r *ConnectionReconciler) applyCommittedCompletions(ctx context.Context, connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, now metav1.Time) []string {
	if r.Consents == nil || r.Credentials == nil {
		return nil
	}
	completions, err := r.Consents.ListConnectorCompletionsForConnection(ctx, string(connection.UID))
	if err != nil {
		return nil
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return nil
	}
	var applied []string
	issuer := connectors.ProviderIssuerDigest(provider)
	authority := connectors.ProviderAuthorityDigest(provider)
	for _, completion := range completions {
		if !completion.Committed {
			continue
		}
		held, err := r.Credentials.GetConnectorCredential(ctx, ref)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			// The completion is the only durable record that can repair
			// the status; a custody read that failed proves no mismatch,
			// so it is kept for the next pass.
			log.FromContext(ctx).Info("custody could not be read to finish a committed completion; retrying", "connection", connection.Name)
			continue
		}
		if err == nil && held.GrantSequence == completion.Credential.GrantSequence && held.AccessToken == completion.Credential.AccessToken &&
			held.RefreshToken == completion.Credential.RefreshToken && held.AuthorityDigest == completion.Credential.AuthorityDigest {
			// The same fence the API applies to a retried completion: the
			// material must have been issued by the provider's current OAuth
			// client and consented under its current authority (client plus
			// tool destinations). Otherwise the link stays Pending until the
			// person consents again; the row is dropped either way. A mode
			// changed since the completion does not discard it: custody
			// holds this material, and recording it against the current
			// mode projects Pending on its own when the scopes fall short.
			if completion.Credential.AuthorityDigest == issuer && completion.ConsentAuthorityDigest == authority {
				connectors.ApplyLinkedStatus(connection, provider, held, now)
				applied = append(applied, completion.Nonce)
				continue
			}
		}
		if err := r.Consents.DeleteConnectorCompletion(ctx, completion.Nonce); err != nil {
			log.FromContext(ctx).Info("committed completion could not be removed", "connection", connection.Name)
		}
	}
	return applied
}

// scopesUnknownCondition replaces a ScopesGranted verdict while the provider
// is not resolved, so the conditions never pair ProviderResolved=False with
// a stale ScopesGranted=True.
func scopesUnknownCondition(connection *corev1alpha1.Connection, now metav1.Time) metav1.Condition {
	return metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionScopesGranted,
		Status:             metav1.ConditionUnknown,
		Reason:             corev1alpha1.ConnectionReasonProviderUnavailable,
		Message:            "Granted scopes cannot be judged while the provider is unavailable",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	}
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
		// Fence commits first: a completion that loaded the Connection just
		// before deletion must not land new material while the provider
		// calls below are in flight, or it would be neither revoked nor kept.
		if err := r.Credentials.TombstoneConnectorCustody(ctx, string(connection.UID)); err != nil {
			return ctrl.Result{}, err
		}
		// Custody is the only copy of the tokens. Without the material to
		// authenticate a revocation (the client Secret or its key is gone)
		// it is kept, and the finalizer with it, until an operator restores
		// the Secret; a provider that refuses a revocation is best effort.
		if err := r.revokeBestEffort(ctx, connection); err != nil {
			log.FromContext(ctx).Info("connector tokens cannot be revoked yet; custody is retained until they can be",
				"connection", connection.Name, "reason", err.Error())
			return ctrl.Result{RequeueAfter: connectionRevocationRetry}, nil
		}
		if err := r.Credentials.DeleteConnectorCredential(ctx, string(connection.UID)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Consents != nil {
		// Parked completions nobody committed are dropped with the consents;
		// their tokens are left to expire, never revoked (see revokeTokens).
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

// connectionRevocationRetry is how long a disconnect waits for revocation
// material (the provider's client Secret) to be restored before trying again.
const connectionRevocationRetry = time.Minute

// errRevocationUnavailable reports tokens that could not be offered for
// revocation because the material to authenticate the call is missing.
var errRevocationUnavailable = errors.New("revocation material is unavailable")

func (r *ConnectionReconciler) revokeBestEffort(ctx context.Context, connection *corev1alpha1.Connection) error {
	if r.Revoker == nil {
		return nil
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return nil
	}
	credential, err := r.Credentials.GetConnectorCredential(ctx, ref)
	switch {
	case err == nil:
		if err := r.revokeTokens(ctx, connection, credential); err != nil {
			return err
		}
	case errors.Is(err, store.ErrNotFound):
		// The current grant may already be gone (the provider revoked it and
		// custody was shredded); the grants earlier commits replaced can
		// still be live and are revoked below regardless.
	default:
		// A row that cannot be opened is not deleted on that account: the
		// disconnect retries rather than discarding the only copy.
		return fmt.Errorf("%w: current credential: %w", errRevocationUnavailable, err)
	}
	// Credentials that later commits replaced were committed by the same
	// owner and are still live upstream; disconnect revokes them too.
	retired, err := r.Credentials.ListRetiredConnectorCredentials(ctx, ref)
	if err != nil {
		return fmt.Errorf("%w: retired credentials: %w", errRevocationUnavailable, err)
	}
	for _, previous := range retired {
		if err := r.revokeTokens(ctx, connection, previous); err != nil {
			return err
		}
	}
	return nil
}

// reapExpiredCompletions deletes parked completions whose redemption window
// closed. Their tokens are left to expire, never revoked: Orka cannot prove
// whose grant a token nobody committed belongs to.
// It returns the earliest expiry among the parked completions it kept, so
// the next reconcile can be scheduled for it; a completion whose deletion
// failed is due again after connectionRevocationRetry.
func (r *ConnectionReconciler) reapExpiredCompletions(ctx context.Context, connection *corev1alpha1.Connection) time.Time {
	var earliest time.Time
	if r.Consents == nil {
		return earliest
	}
	completions, err := r.Consents.ListConnectorCompletionsForConnection(ctx, string(connection.UID))
	if err != nil {
		log.FromContext(ctx).Info("parked completions could not be listed for expiry", "connection", connection.Name)
		return earliest
	}
	now := time.Now()
	for _, completion := range completions {
		// A committed completion is finished by applyCommittedCompletions,
		// never reaped: its marker is what lets the link be recorded from the
		// material custody already holds.
		if completion.Committed {
			continue
		}
		if completion.ExpiresAt.After(now) {
			if earliest.IsZero() || completion.ExpiresAt.Before(earliest) {
				earliest = completion.ExpiresAt
			}
			continue
		}
		if err := r.Consents.DeleteConnectorCompletion(ctx, completion.Nonce); err != nil {
			log.FromContext(ctx).Info("expired completion could not be deleted", "connection", connection.Name)
			if retry := now.Add(connectionRevocationRetry); earliest.IsZero() || retry.Before(earliest) {
				earliest = retry
			}
		}
	}
	return earliest
}

// revokeTokens revokes the refresh then access token of the committed
// credential at the provider, best effort. The tokens are sent only to the
// OAuth authority sealed with them: when the provider was replaced or its
// client changed since they were issued, nothing is sent. Only committed
// material is ever revoked; a token from a consent nobody completed may be
// another person's live credential (a forwarded consent link, or a provider
// re-issuing a long-lived token), so it is deleted and left to expire.
// revokeTokens offers a credential's tokens to the provider's revocation
// endpoint. A provider whose revocation authority (client and revocation
// endpoint) no longer matches the one sealed with the material, or that has
// no revocation endpoint, cannot revoke it and the tokens are left to
// expire; missing material to authenticate the call is reported so the
// caller keeps custody instead of deleting the only copy.
func (r *ConnectionReconciler) revokeTokens(ctx context.Context, connection *corev1alpha1.Connection, credential store.ConnectorCredential) error {
	if r.Revoker == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	// Read uncached: tokens are disclosed to the endpoint this spec names,
	// so an operator's change to the client or revocation URL must be seen
	// even when the informer has not caught up.
	provider := &corev1alpha1.ConnectorProvider{}
	if err := r.referenceReader().Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%w: %w", errRevocationUnavailable, err)
	}
	if credential.RevocationDigest == "" || credential.RevocationDigest != connectors.ProviderRevocationDigest(provider) {
		logger.Info("provider OAuth client or revocation endpoint changed since the token was issued; not revoking against a different authority",
			"connection", connection.Name, "provider", provider.Name)
		return nil
	}
	if strings.TrimSpace(provider.Spec.OAuth.RevocationURL) == "" {
		return nil
	}
	secret := &corev1.Secret{}
	secretRef := provider.Spec.OAuth.ClientSecretRef
	// A missing Secret is recoverable while the namespace stays: an operator
	// restores it and the disconnect finishes, even while the provider alone
	// is being deleted. Namespace teardown is not: it deletes the Secret
	// first and nothing can be recreated in a terminating namespace, so the
	// tokens are left to expire rather than holding the namespace forever.
	unavailable := func(err error) error {
		if provider.DeletionTimestamp.IsZero() {
			return err
		}
		namespace := &corev1.Namespace{}
		if nsErr := r.referenceReader().Get(ctx, types.NamespacedName{Name: provider.Namespace}, namespace); nsErr != nil || namespace.DeletionTimestamp.IsZero() {
			return err
		}
		logger.Info("namespace is terminating and the provider's client secret is gone; tokens are left to expire unrevoked",
			"connection", connection.Name, "provider", provider.Name)
		return nil
	}
	if err := r.referenceReader().Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: secretRef.Name}, secret); err != nil {
		return unavailable(fmt.Errorf("%w: client secret %q: %w", errRevocationUnavailable, secretRef.Name, err))
	}
	// The secret is opaque bytes; only emptiness is judged, never trimmed.
	clientSecret := string(secret.Data[secretRef.Key])
	if strings.TrimSpace(clientSecret) == "" {
		return unavailable(fmt.Errorf("%w: client secret %q has no %q", errRevocationUnavailable, secretRef.Name, secretRef.Key))
	}
	cfg := connectors.ProviderOAuthConfig(provider, clientSecret)
	for _, token := range []string{credential.RefreshToken, credential.AccessToken} {
		if token == "" {
			continue
		}
		// Each token gets its own bounded attempt so a stalled first call
		// cannot consume the deadline of the second.
		revokeCtx, cancel := context.WithTimeout(ctx, connectionRevokeTimeout)
		err := r.Revoker.Revoke(revokeCtx, cfg, token)
		cancel()
		if err != nil {
			logger.Info("provider token revocation failed; continuing with disconnect", "connection", connection.Name, "provider", provider.Name)
		}
	}
	return nil
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
	before *corev1alpha1.ConnectionStatus,
	providerResolved metav1.Condition,
	reconcileErr error,
) (ctrl.Result, error) {
	connection.Status.ObservedGeneration = connection.Generation
	meta.SetStatusCondition(&connection.Status.Conditions, providerResolved)
	connection.Status.State = projectConnectionState(connection, providerResolved)
	// Connection and provider changes are watched; nothing here depends on
	// the passage of time, so there is no periodic requeue to multiply by
	// the number of linked accounts.
	if reflect.DeepEqual(before, &connection.Status) {
		return ctrl.Result{}, reconcileErr
	}
	if err := r.Status().Update(ctx, connection); err != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, err)
	}
	return ctrl.Result{}, reconcileErr
}

// connectionNextPass schedules the next reconcile at the next moment the
// link's state changes with time alone: just past the credential's known
// expiry, so a link whose only material expires stops advertising itself
// promptly, or the earliest parked completion's expiry, so it is reaped.
// Otherwise the watches (and the manager's resync) drive reconciles: a
// fixed requeue per linked account does not scale.
func connectionNextPass(connection *corev1alpha1.Connection, now, completionDeadline time.Time) time.Duration {
	var next time.Duration
	consider := func(until time.Duration) {
		if next == 0 || until < next {
			next = until
		}
	}
	// An expiry already in the past was handled by this pass.
	if connection.Status.ExpiresAt != nil {
		if until := connection.Status.ExpiresAt.Sub(now) + time.Second; until > time.Second {
			consider(until)
		}
	}
	// A kept completion is still pending work even if its deadline passed
	// while this pass ran: it is reaped on a prompt next pass.
	if !completionDeadline.IsZero() {
		consider(max(completionDeadline.Sub(now)+time.Second, time.Second))
	}
	return next
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

// labelsPresent reports whether every wanted label is set to its value.
func labelsPresent(have, want map[string]string) bool {
	for key, value := range want {
		if have[key] != value {
			return false
		}
	}
	return true
}
