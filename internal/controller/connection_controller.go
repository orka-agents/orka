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

	ConnectionCustodyFinalizer = "core.orka.ai/connector-custody"

	connectionRevokeTimeout = 10 * time.Second
)

type ConnectorTokenRevoker interface {
	Revoke(ctx context.Context, cfg connectors.OAuthProviderConfig, token string) error
}

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
			log.FromContext(ctx).Info("custody could not be read to finish a committed completion; retrying", "connection", connection.Name)
			continue
		}
		if err == nil && held.GrantSequence == completion.Credential.GrantSequence && held.AccessToken == completion.Credential.AccessToken &&
			held.RefreshToken == completion.Credential.RefreshToken && held.AuthorityDigest == completion.Credential.AuthorityDigest {
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

func (r *ConnectionReconciler) finalize(ctx context.Context, connection *corev1alpha1.Connection) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(connection, ConnectionCustodyFinalizer) {
		return ctrl.Result{}, nil
	}
	if r.Credentials != nil {
		if err := r.Credentials.TombstoneConnectorCustody(ctx, string(connection.UID)); err != nil {
			return ctrl.Result{}, err
		}
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

const connectionRevocationRetry = time.Minute

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
	default:
		return fmt.Errorf("%w: current credential: %w", errRevocationUnavailable, err)
	}
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

func (r *ConnectionReconciler) revokeTokens(ctx context.Context, connection *corev1alpha1.Connection, credential store.ConnectorCredential) error {
	if r.Revoker == nil {
		return nil
	}
	logger := log.FromContext(ctx)
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
	clientSecret := string(secret.Data[secretRef.Key])
	if strings.TrimSpace(clientSecret) == "" {
		return unavailable(fmt.Errorf("%w: client secret %q has no %q", errRevocationUnavailable, secretRef.Name, secretRef.Key))
	}
	cfg := connectors.ProviderOAuthConfig(provider, clientSecret)
	for _, token := range []string{credential.RefreshToken, credential.AccessToken} {
		if token == "" {
			continue
		}
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
	if reflect.DeepEqual(before, &connection.Status) {
		return ctrl.Result{}, reconcileErr
	}
	if err := r.Status().Update(ctx, connection); err != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, err)
	}
	return ctrl.Result{}, reconcileErr
}

func connectionNextPass(connection *corev1alpha1.Connection, now, completionDeadline time.Time) time.Duration {
	var next time.Duration
	consider := func(until time.Duration) {
		if next == 0 || until < next {
			next = until
		}
	}
	if connection.Status.ExpiresAt != nil {
		if until := connection.Status.ExpiresAt.Sub(now) + time.Second; until > time.Second {
			consider(until)
		}
	}
	if !completionDeadline.IsZero() {
		consider(max(completionDeadline.Sub(now)+time.Second, time.Second))
	}
	return next
}

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

func labelsPresent(have, want map[string]string) bool {
	for key, value := range want {
		if have[key] != value {
			return false
		}
	}
	return true
}
