/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agenthost"
	"github.com/orka-agents/orka/internal/connectors/credential"
	"github.com/orka-agents/orka/internal/githubauth"
	"github.com/orka-agents/orka/internal/k8scaps"
)

// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosts/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete

const (
	agentHostFinalizer = "core.orka.ai/agenthost"

	agentHostConditionReady              = "Ready"
	agentHostConditionSuspended          = "Suspended"
	agentHostConditionAgentUpdatePending = "HostAgentUpdatePending"

	agentHostReasonReady                   = "HostReady"
	agentHostReasonProvisioning            = "Provisioning"
	agentHostReasonRestarting              = "Restarting"
	agentHostReasonSuspended               = "Suspended"
	agentHostReasonIdle                    = "Idle"
	agentHostReasonRequested               = "Requested"
	agentHostReasonRunning                 = "Running"
	agentHostReasonTemplateNotFound        = "TemplateNotFound"
	agentHostReasonInvalidTemplate         = "InvalidTemplate"
	agentHostReasonGitHubTokenRequired     = "GitHubTokenRequired"
	agentHostReasonGitHubTokenUnavailable  = "GitHubTokenUnavailable"
	agentHostReasonObjectCollision         = "ObjectCollision"
	agentHostReasonObjectRejected          = "ObjectRejected"
	agentHostReasonStateVolumeLost         = "StateVolumeLost"
	agentHostReasonHostAgentUnavailable    = "HostAgentUnavailable"
	agentHostReasonHostNamespaceChanged    = "HostNamespaceChanged"
	agentHostReasonConnectionUnavailable   = "ConnectionUnavailable"
	agentHostReasonGitHubIdentityChanged   = "GitHubIdentityChanged"
	agentHostReasonGitHubMembershipEnded   = "GitHubMembershipEnded"
	agentHostReasonGitHubTokenNeverExpires = "GitHubTokenNeverExpires"
	agentHostReasonWaitingForIdle          = "WaitingForIdle"

	agentHostKindSecret           = "Secret"
	agentHostKindConnectionSecret = "ConnectionSecret"
	agentHostKindPVC              = "PersistentVolumeClaim"
	agentHostKindNetworkPolicy    = "NetworkPolicy"
	agentHostKindPod              = "Pod"

	agentHostResync              = 5 * time.Minute
	agentHostIdlePoll            = time.Minute
	agentHostCleanupRequeue      = 5 * time.Second
	agentHostRestartBackoff      = 10 * time.Second
	agentHostRestartBackoffMax   = 5 * time.Minute
	agentHostTokenBytes          = 32
	agentHostNonceBytes          = 16
	agentHostWakeClockSkew       = time.Minute
	agentHostTokenHorizon        = 15 * time.Minute
	agentHostAgentUpdateDeadline = 24 * time.Hour

	agentHostWaitingInitializing = "PodInitializing"
	agentHostWaitingCreating     = "ContainerCreating"

	agentHostTemplateIndex   = "spec.templateRef.name"
	agentHostSecretIndex     = "spec.githubTokenSecretRef.name"
	agentHostConnectionIndex = "spec.githubTokenFrom.connectionRef.name"
)

type agentHostFailure struct {
	reason  string
	message string
}

func (f *agentHostFailure) Error() string { return f.reason + ": " + f.message }

func agentHostFailed(reason, format string, args ...any) error {
	return &agentHostFailure{reason: reason, message: fmt.Sprintf(format, args...)}
}

var errAgentHostObjectGone = errors.New("agent host object is gone")

type AgentHostReconciler struct {
	client.Client
	APIReader           client.Reader
	Scheme              *runtime.Scheme
	HostNamespace       string
	AgentImage          string
	ClusterCapabilities k8scaps.Capabilities
	Controller          agenthost.ControllerPeer
	IdleStatus          agenthost.StatusReader
	HostCredentials     AgentHostCredentials
	GitHubAccounts      GitHubAccountReader
	Membership          GitHubMembershipChecker
	Now                 func() time.Time
	Random              func([]byte) (int, error)
}

type AgentHostCredentials interface {
	HostCredential(ctx context.Context, namespace, name string, horizon time.Duration) (credential.HostCredential, error)
	CheckHostLink(ctx context.Context, namespace, name string) error
}

type GitHubMembershipChecker func(ctx context.Context, token, login string) error

type GitHubAccountReader interface {
	Account(ctx context.Context, token string) (corev1alpha1.AgentHostGitHubAccount, error)
}

func (r *AgentHostReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *AgentHostReconciler) randomHex(size int) (string, error) {
	read := rand.Read
	if r.Random != nil {
		read = r.Random
	}
	buf := make([]byte, size)
	if _, err := read(buf); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (r *AgentHostReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *AgentHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &corev1alpha1.AgentHost{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !host.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, host)
	}
	if !controllerutil.ContainsFinalizer(host, agentHostFinalizer) {
		controllerutil.AddFinalizer(host, agentHostFinalizer)
		if err := r.Update(ctx, host); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if host.Status.HostNamespace == "" {
		if strings.TrimSpace(r.HostNamespace) == "" {
			return ctrl.Result{}, fmt.Errorf("agent host namespace is not configured")
		}
		host.Status.HostNamespace = r.HostNamespace
		if err := r.Status().Update(ctx, host); err != nil {
			return ctrl.Result{}, err
		}
	}

	before := host.Status.DeepCopy()
	names := agenthost.ObjectNames(host, host.Status.HostNamespace)
	result, err := r.reconcileHost(ctx, host, names)
	if failure, ok := errors.AsType[*agentHostFailure](err); ok {
		if stopErr := r.stopFailedHost(ctx, host, names, failure); stopErr != nil {
			return ctrl.Result{}, stopErr
		}
		setAgentHostFailed(host, failure)
		err = nil
		result = ctrl.Result{RequeueAfter: agentHostResync}
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	host.Status.ObservedGeneration = host.Generation
	if !equality.Semantic.DeepEqual(before, &host.Status) {
		if err := r.Status().Update(ctx, host); err != nil {
			return ctrl.Result{}, err
		}
	}
	return result, nil
}

func (r *AgentHostReconciler) reconcileHost(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
) (ctrl.Result, error) {
	if host.Status.HostNamespace != strings.TrimSpace(r.HostNamespace) {
		return ctrl.Result{}, agentHostFailed(agentHostReasonHostNamespaceChanged,
			"the host's objects are in namespace %q but the controller now uses %q; delete and recreate the AgentHost",
			host.Status.HostNamespace, r.HostNamespace)
	}
	if host.Status.IdleSuspendedAt != nil && host.Spec.WakeRequestedAt != nil &&
		host.Spec.WakeRequestedAt.After(host.Status.IdleSuspendedAt.Time) &&
		!host.Spec.WakeRequestedAt.After(r.now().Add(agentHostWakeClockSkew)) {
		host.Status.IdleSuspendedAt = nil
		host.Status.IdleSince = nil
	}
	suspended := host.Spec.Suspended || host.Status.IdleSuspendedAt != nil
	if host.Spec.GitHubTokenFrom != nil {
		if err := r.checkHostLink(ctx, host); err != nil {
			if _, failed := errors.AsType[*agentHostFailure](err); failed || !suspended {
				return ctrl.Result{}, err
			}
			logf.FromContext(ctx).Info("suspending an agent host whose link could not be checked", "agentHost", host.Name, "error", err.Error())
		}
	}
	if suspended {
		return r.suspend(ctx, host, names)
	}

	template, err := r.hostTemplate(ctx, host)
	if err != nil {
		return ctrl.Result{}, err
	}
	if strings.TrimSpace(r.AgentImage) == "" {
		return ctrl.Result{}, agentHostFailed(agentHostReasonHostAgentUnavailable,
			"the controller has no host agent image; set --agent-host-agent-image")
	}
	githubToken, err := r.githubToken(ctx, host, names, template)
	if err != nil {
		return ctrl.Result{}, err
	}

	connectionToken, err := r.ensureConnectionToken(ctx, host, names)
	if err != nil {
		return ctrl.Result{}, err
	}
	stopping, err := r.reconcileGitHubTokenCopy(ctx, host, names, githubToken.value, githubToken.source)
	if err != nil {
		return ctrl.Result{}, err
	}
	if stopping {
		return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
	}
	if err := r.ensureObject(ctx, host, agentHostKindPVC, "", names.PVC, &corev1.PersistentVolumeClaim{},
		func(nonce string) client.Object { return agenthost.StateVolumeClaim(host, names, nonce, template) },
	); err != nil {
		if errors.Is(err, errAgentHostObjectGone) {
			return ctrl.Result{}, agentHostFailed(agentHostReasonStateVolumeLost,
				"the host's state volume was deleted; delete and recreate the AgentHost")
		}
		return ctrl.Result{}, err
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := r.ensureRecreatable(ctx, host, agentHostKindNetworkPolicy, "", names.NetworkPolicy, policy,
		func(nonce string) client.Object {
			return agenthost.HostNetworkPolicy(host, names, nonce, template, r.Controller)
		},
	); err != nil {
		return ctrl.Result{}, err
	}
	if desired := agenthost.HostNetworkPolicy(host, names, "", template, r.Controller); !equality.Semantic.DeepEqual(policy.Spec, desired.Spec) {
		policy.Spec = desired.Spec
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, fmt.Errorf("update NetworkPolicy %s: %w", names.NetworkPolicy, err)
		}
	}
	if err := r.reconcileConnectionSecret(ctx, host, names, template, connectionToken); err != nil {
		return ctrl.Result{}, err
	}
	result, err := r.reconcilePod(ctx, host, names, template, githubToken.source)
	if err == nil && host.Spec.GitHubTokenFrom != nil {
		host.Status.ObservedConnectionUID = githubToken.connectionUID
		host.Status.ObservedGrantSequence = githubToken.grantSequence
		if result.RequeueAfter == 0 || result.RequeueAfter > agentHostResync {
			result.RequeueAfter = agentHostResync
		}
	}
	return result, err
}

func (r *AgentHostReconciler) hostTemplate(ctx context.Context, host *corev1alpha1.AgentHost) (*corev1alpha1.AgentHostTemplate, error) {
	template := &corev1alpha1.AgentHostTemplate{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: host.Spec.TemplateRef.Name}, template); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, agentHostFailed(agentHostReasonTemplateNotFound,
				"AgentHostTemplate %q does not exist", host.Spec.TemplateRef.Name)
		}
		return nil, err
	}
	if err := agenthost.ValidateTemplate(template); err != nil {
		return nil, agentHostFailed(agentHostReasonInvalidTemplate, "%v", err)
	}
	if host.Spec.GitHubTokenFrom != nil && template.Spec.Access != corev1alpha1.AgentHostAccessRelay {
		return nil, agentHostFailed(agentHostReasonInvalidTemplate,
			"a host fed by a Connection needs a template with access: Relay")
	}
	return template, nil
}

type agentHostToken struct {
	value         []byte
	source        string
	connectionUID string
	grantSequence int64
}

func (r *AgentHostReconciler) githubToken(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	template *corev1alpha1.AgentHostTemplate,
) (agentHostToken, error) {
	policy := template.Spec.GitHubToken
	if policy == "" || policy == corev1alpha1.AgentHostCredentialNone {
		return agentHostToken{}, nil
	}
	if host.Spec.GitHubTokenFrom != nil {
		return r.connectionToken(ctx, host, names)
	}
	ref := host.Spec.GitHubTokenSecretRef
	if ref == nil {
		if policy == corev1alpha1.AgentHostCredentialRequired {
			return agentHostToken{}, agentHostFailed(agentHostReasonGitHubTokenRequired,
				"template %q requires spec.githubTokenSecretRef", template.Name)
		}
		return agentHostToken{}, nil
	}
	key := ref.Key
	if key == "" {
		key = agenthost.TokenKey
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return agentHostToken{}, agentHostFailed(agentHostReasonGitHubTokenUnavailable, "Secret %q does not exist", ref.Name)
		}
		return agentHostToken{}, err
	}
	token := bytes.TrimSpace(secret.Data[key])
	if len(token) == 0 {
		return agentHostToken{}, agentHostFailed(agentHostReasonGitHubTokenUnavailable, "Secret %q has no data at key %q", ref.Name, key)
	}
	return agentHostToken{value: token, source: ref.Name + "/" + key}, nil
}

func (r *AgentHostReconciler) checkHostLink(ctx context.Context, host *corev1alpha1.AgentHost) error {
	if r.HostCredentials == nil || r.GitHubAccounts == nil {
		return agentHostFailed(agentHostReasonConnectionUnavailable, "connectors are not enabled on this controller")
	}
	name := host.Spec.GitHubTokenFrom.ConnectionRef.Name
	connection := &corev1alpha1.Connection{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: name}, connection); client.IgnoreNotFound(err) != nil {
		return err
	}
	if connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost] == corev1alpha1.ConnectionAgentHostDeclined {
		return agentHostFailed(agentHostReasonConnectionUnavailable, "the person behind Connection %s declined an agent host", name)
	}
	err := r.HostCredentials.CheckHostLink(ctx, host.Namespace, name)
	if errors.Is(err, credential.ErrHostLinkUnavailable) {
		return agentHostFailed(agentHostReasonConnectionUnavailable, "%v", err)
	}
	return err
}

func (r *AgentHostReconciler) connectionToken(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
) (agentHostToken, error) {
	name := host.Spec.GitHubTokenFrom.ConnectionRef.Name
	held, err := r.HostCredentials.HostCredential(ctx, host.Namespace, name, agentHostTokenHorizon)
	if errors.Is(err, credential.ErrHostLinkUnavailable) {
		return agentHostToken{}, agentHostFailed(agentHostReasonConnectionUnavailable, "%v", err)
	}
	if err != nil {
		return agentHostToken{}, fmt.Errorf("get the host's token from Connection %s: %w", name, err)
	}
	if held.ExpiresAt.IsZero() || !held.Refreshable {
		return agentHostToken{}, agentHostFailed(agentHostReasonGitHubTokenNeverExpires,
			"Connection %s holds a GitHub token with no expiry GitHub confirmed; turn on user-token expiration in the GitHub App, then link again", name)
	}
	token := []byte(held.AccessToken)
	holds, err := r.tokenCopyHolds(ctx, names, token)
	if err != nil {
		return agentHostToken{}, err
	}
	if host.Status.GitHub == nil || !holds {
		account, err := r.GitHubAccounts.Account(ctx, held.AccessToken)
		if err != nil {
			return agentHostToken{}, err
		}
		if pinned := host.Status.GitHub; pinned != nil && pinned.ID != account.ID {
			return agentHostToken{}, agentHostFailed(agentHostReasonGitHubIdentityChanged,
				"Connection %s now links GitHub account %q, but the host belongs to %q; reconnect with %q, or delete the AgentHost",
				name, account.Login, pinned.Login, pinned.Login)
		}
		if r.Membership != nil {
			if err := r.Membership(ctx, held.AccessToken, account.Login); errors.Is(err, githubauth.ErrNotMember) {
				return agentHostToken{}, agentHostFailed(agentHostReasonGitHubMembershipEnded,
					"GitHub account %q is no longer an active member of an org or team allowed to sign in", account.Login)
			} else if err != nil {
				return agentHostToken{}, fmt.Errorf("check the host's GitHub membership: %w", err)
			}
		}
		if host.Status.GitHub == nil {
			host.Status.GitHub = &account
			if err := r.Status().Update(ctx, host); err != nil {
				return agentHostToken{}, err
			}
		}
	}
	return agentHostToken{
		value: token, source: "connection:" + name,
		connectionUID: held.ConnectionUID, grantSequence: held.GrantSequence,
	}, nil
}

func (r *AgentHostReconciler) tokenCopyHolds(ctx context.Context, names agenthost.Names, token []byte) (bool, error) {
	secret := &corev1.Secret{}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: names.Namespace, Name: names.GitHubTokenSecret}, secret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Equal(secret.Data[agenthost.TokenKey], token), nil
}

func (r *AgentHostReconciler) ensureConnectionToken(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
) ([]byte, error) {
	token, err := r.randomHex(agentHostTokenBytes)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	err = r.ensureObject(ctx, host, agentHostKindSecret, "", names.ConnectionTokenSecret, secret,
		func(nonce string) client.Object {
			return agenthost.ConnectionTokenSecret(host, names, nonce, []byte(token))
		})
	if errors.Is(err, errAgentHostObjectGone) {
		if err := r.forgetReceipt(ctx, host, agentHostKindSecret, "", names.ConnectionTokenSecret); err != nil {
			return nil, err
		}
		if err := r.deletePod(ctx, host, names); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("connection token Secret was deleted; replacing it")
	}
	if err != nil {
		return nil, err
	}
	value := bytes.TrimSpace(secret.Data[agenthost.TokenKey])
	if len(value) == 0 {
		return nil, fmt.Errorf("connection token Secret %s has no token", names.ConnectionTokenSecret)
	}
	return value, nil
}

func (r *AgentHostReconciler) reconcileGitHubTokenCopy(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	token []byte,
	tokenSource string,
) (bool, error) {
	if token == nil {
		return false, r.deleteOwned(ctx, host, agentHostKindSecret, "", names.GitHubTokenSecret)
	}
	stopping, err := r.retirePodOfAnotherToken(ctx, host, names, tokenSource)
	if err != nil || stopping {
		return stopping, err
	}
	return false, r.ensureGitHubToken(ctx, host, names, token)
}

func (r *AgentHostReconciler) retirePodOfAnotherToken(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	tokenSource string,
) (bool, error) {
	receipt := findAgentHostReceipt(host, agentHostKindPod, "", names.Pod)
	if receipt == nil {
		return false, nil
	}
	pod := &corev1.Pod{}
	key := types.NamespacedName{Namespace: host.Status.HostNamespace, Name: names.Pod}
	err := r.Get(ctx, key, pod)
	if apierrors.IsNotFound(err) {
		err = r.reader().Get(ctx, key, pod)
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !agentHostOwns(host, receipt, pod) || pod.Annotations[agenthost.AnnotationGitHubToken] == tokenSource {
		return false, nil
	}
	if err := r.deletePod(ctx, host, names); err != nil {
		return false, err
	}
	setAgentHostState(host, corev1alpha1.AgentHostPhaseProvisioning, "", metav1.ConditionFalse,
		agentHostReasonProvisioning, "replacing the host's Pod to change its credentials", "")
	return true, nil
}

func (r *AgentHostReconciler) ensureGitHubToken(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	token []byte,
) error {
	secret := &corev1.Secret{}
	if err := r.ensureRecreatable(ctx, host, agentHostKindSecret, "", names.GitHubTokenSecret, secret,
		func(nonce string) client.Object { return agenthost.GitHubTokenSecret(host, names, nonce, token) },
	); err != nil {
		return err
	}
	if bytes.Equal(secret.Data[agenthost.TokenKey], token) {
		return nil
	}
	secret.Data = map[string][]byte{agenthost.TokenKey: append([]byte(nil), token...)}
	return r.Update(ctx, secret)
}

func (r *AgentHostReconciler) ensureRecreatable(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	kind, namespace, name string,
	live client.Object,
	build func(nonce string) client.Object,
) error {
	err := r.ensureObject(ctx, host, kind, namespace, name, live, build)
	if !errors.Is(err, errAgentHostObjectGone) {
		return err
	}
	if err := r.forgetReceipt(ctx, host, kind, namespace, name); err != nil {
		return err
	}
	return r.ensureObject(ctx, host, kind, namespace, name, live, build)
}

func receiptNamespace(host *corev1alpha1.AgentHost, namespace string) string {
	if namespace == "" {
		return host.Status.HostNamespace
	}
	return namespace
}

func (r *AgentHostReconciler) ensureObject(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	kind, namespace, name string,
	live client.Object,
	build func(nonce string) client.Object,
) error {
	key := types.NamespacedName{Namespace: receiptNamespace(host, namespace), Name: name}
	receipt := findAgentHostReceipt(host, kind, namespace, name)
	err := r.Get(ctx, key, live)
	if err == nil {
		return r.adopt(ctx, host, kind, namespace, name, receipt, live)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if receipt != nil && receipt.UID != "" {
		if err := r.reader().Get(ctx, key, live); err == nil {
			return r.adopt(ctx, host, kind, namespace, name, receipt, live)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		return errAgentHostObjectGone
	}
	if receipt == nil {
		nonce, err := r.randomHex(agentHostNonceBytes)
		if err != nil {
			return err
		}
		host.Status.Objects = append(host.Status.Objects, corev1alpha1.AgentHostObjectReceipt{
			Kind: kind, Namespace: namespace, Name: name, Nonce: nonce,
		})
		if err := r.Status().Update(ctx, host); err != nil {
			return err
		}
		receipt = findAgentHostReceipt(host, kind, namespace, name)
	}
	desired := build(receipt.Nonce)
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("%s %s appeared while creating it; retrying", kind, name)
		}
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			return agentHostFailed(agentHostReasonObjectRejected, "the API server refused the host's %s: %s", kind, agentHostAPIMessage(err))
		}
		return fmt.Errorf("create %s %s: %w", kind, name, err)
	}
	receipt = findAgentHostReceipt(host, kind, namespace, name)
	receipt.UID = string(desired.GetUID())
	if err := r.Status().Update(ctx, host); err != nil {
		return err
	}
	return copyAgentHostObject(live, desired)
}

func agentHostAPIMessage(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Message != "" {
		return status.Status().Message
	}
	return err.Error()
}

func copyAgentHostObject(dst, src client.Object) error {
	target, source := reflect.ValueOf(dst), reflect.ValueOf(src)
	if target.Type() != source.Type() || target.Kind() != reflect.Pointer {
		return fmt.Errorf("copy %T into %T", src, dst)
	}
	target.Elem().Set(source.Elem())
	return nil
}

func (r *AgentHostReconciler) adopt(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	kind, namespace, name string,
	receipt *corev1alpha1.AgentHostObjectReceipt,
	live client.Object,
) error {
	if receipt != nil && agentHostOwns(host, receipt, live) {
		if receipt.UID == "" {
			receipt.UID = string(live.GetUID())
			return r.Status().Update(ctx, host)
		}
		return nil
	}
	return agentHostFailed(agentHostReasonObjectCollision,
		"%s %s/%s exists but was not created for this AgentHost", kind, receiptNamespace(host, namespace), name)
}

func (r *AgentHostReconciler) deleteOwned(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	kind, namespace, name string,
) error {
	receipt := findAgentHostReceipt(host, kind, namespace, name)
	if receipt == nil {
		return nil
	}
	object := newAgentHostObject(kind)
	key := types.NamespacedName{Namespace: receiptNamespace(host, namespace), Name: name}
	if err := r.reader().Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return r.forgetReceipt(ctx, host, kind, namespace, name)
		}
		return err
	}
	if !agentHostOwns(host, receipt, object) {
		return agentHostFailed(agentHostReasonObjectCollision,
			"%s %s/%s exists but was not created for this AgentHost", kind, key.Namespace, name)
	}
	if !object.GetDeletionTimestamp().IsZero() {
		return nil
	}
	uid := object.GetUID()
	return client.IgnoreNotFound(r.Delete(ctx, object, client.Preconditions{UID: &uid}))
}

func (r *AgentHostReconciler) forgetReceipt(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	kind, namespace, name string,
) error {
	kept := host.Status.Objects[:0]
	removed := false
	for _, receipt := range host.Status.Objects {
		if receipt.Kind == kind && receipt.Namespace == namespace && receipt.Name == name {
			removed = true
			continue
		}
		kept = append(kept, receipt)
	}
	if !removed {
		return nil
	}
	host.Status.Objects = kept
	return r.Status().Update(ctx, host)
}

func findAgentHostReceipt(
	host *corev1alpha1.AgentHost,
	kind, namespace, name string,
) *corev1alpha1.AgentHostObjectReceipt {
	for i := range host.Status.Objects {
		receipt := &host.Status.Objects[i]
		if receipt.Kind == kind && receipt.Namespace == namespace && receipt.Name == name {
			return receipt
		}
	}
	return nil
}

func (r *AgentHostReconciler) reconcileConnectionSecret(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	template *corev1alpha1.AgentHostTemplate,
	token []byte,
) error {
	if template.Spec.Access != corev1alpha1.AgentHostAccessStandalone {
		host.Status.ConnectionSecretRef = nil
		return r.deleteOwned(ctx, host, agentHostKindConnectionSecret, host.Namespace, names.Connection)
	}
	secret := &corev1.Secret{}
	if err := r.ensureRecreatable(ctx, host, agentHostKindConnectionSecret, host.Namespace, names.Connection, secret,
		func(nonce string) client.Object {
			desired := agenthost.ConnectionSecret(host, names, nonce, template, token)
			_ = controllerutil.SetControllerReference(host, desired, r.Scheme)
			return desired
		},
	); err != nil {
		return err
	}
	host.Status.ConnectionSecretRef = &corev1alpha1.SecretReference{Name: names.Connection}
	desired := agenthost.ConnectionSecret(host, names, "", template, token)
	if equality.Semantic.DeepEqual(secret.Data, desired.Data) {
		return nil
	}
	secret.Data = desired.Data
	return r.Update(ctx, secret)
}

func (r *AgentHostReconciler) suspend(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
) (ctrl.Result, error) {
	reason := agentHostReasonRequested
	if !host.Spec.Suspended {
		reason = agentHostReasonIdle
	}
	meta.RemoveStatusCondition(&host.Status.Conditions, agentHostConditionAgentUpdatePending)
	if err := r.deletePod(ctx, host, names); err != nil {
		return ctrl.Result{}, err
	}
	if findAgentHostReceipt(host, agentHostKindPod, "", names.Pod) != nil {
		setAgentHostState(host, corev1alpha1.AgentHostPhaseSuspended, "", metav1.ConditionFalse,
			agentHostReasonSuspended, "the host's Pod is stopping", reason)
		return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
	}
	if host.Spec.GitHubTokenFrom != nil {
		if err := r.deleteOwned(ctx, host, agentHostKindSecret, "", names.GitHubTokenSecret); err != nil {
			return ctrl.Result{}, err
		}
	}
	message := "the host is suspended; its state volume is kept"
	if reason == agentHostReasonIdle {
		message = "the host was idle and is suspended; set spec.wakeRequestedAt to start it"
	}
	setAgentHostState(host, corev1alpha1.AgentHostPhaseSuspended, "", metav1.ConditionFalse,
		agentHostReasonSuspended, message, reason)
	return ctrl.Result{RequeueAfter: agentHostResync}, nil
}

func (r *AgentHostReconciler) stopFailedHost(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	failure *agentHostFailure,
) error {
	if failure.reason == agentHostReasonHostNamespaceChanged {
		return nil
	}
	ignoreCollision := func(err error) error {
		if other, ok := errors.AsType[*agentHostFailure](err); ok {
			logf.FromContext(ctx).Info("left an agent host object in place", "agentHost", host.Name, "reason", other.message)
			return nil
		}
		return err
	}
	if err := ignoreCollision(r.deletePod(ctx, host, names)); err != nil {
		return err
	}
	host.Status.PodName = ""
	if host.Spec.GitHubTokenFrom != nil {
		return ignoreCollision(r.deleteOwned(ctx, host, agentHostKindSecret, "", names.GitHubTokenSecret))
	}
	switch failure.reason {
	case agentHostReasonGitHubTokenRequired, agentHostReasonGitHubTokenUnavailable:
		return ignoreCollision(r.deleteOwned(ctx, host, agentHostKindSecret, "", names.GitHubTokenSecret))
	}
	return nil
}

func (r *AgentHostReconciler) reconcilePod(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	template *corev1alpha1.AgentHostTemplate,
	tokenSource string,
) (ctrl.Result, error) {
	if findAgentHostReceipt(host, agentHostKindPod, "", names.Pod) == nil {
		if wait := r.restartWait(host); wait > 0 {
			setAgentHostState(host, corev1alpha1.AgentHostPhaseProvisioning, "", metav1.ConditionFalse,
				agentHostReasonRestarting, fmt.Sprintf("the host failed; restarting in %s", wait.Round(time.Second)), "")
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	pod := &corev1.Pod{}
	err := r.ensureObject(ctx, host, agentHostKindPod, "", names.Pod, pod, func(nonce string) client.Object {
		return agenthost.HostPod(host, names, nonce, template, agenthost.PodOptions{
			AgentImage: r.AgentImage, GitHubTokenSource: tokenSource, ClusterCapabilities: r.ClusterCapabilities,
		})
	})
	if errors.Is(err, errAgentHostObjectGone) {
		if err := r.forgetReceipt(ctx, host, agentHostKindPod, "", names.Pod); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !pod.DeletionTimestamp.IsZero() {
		setAgentHostState(host, corev1alpha1.AgentHostPhaseProvisioning, "", metav1.ConditionFalse,
			agentHostReasonProvisioning, "the previous Pod is stopping", "")
		return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
	}
	if agentHostPodTerminated(pod) {
		now := metav1.NewTime(r.now().UTC().Truncate(time.Second))
		host.Status.Restarts++
		host.Status.LastRestartTime = &now
		if err := r.deletePod(ctx, host, names); err != nil {
			return ctrl.Result{}, err
		}
		setAgentHostState(host, corev1alpha1.AgentHostPhaseProvisioning, "", metav1.ConditionFalse,
			agentHostReasonRestarting, "the host's Pod stopped; replacing it", "")
		return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
	}
	if message := staleAgentHostPod(pod, template, tokenSource); message != "" {
		if err := r.deletePod(ctx, host, names); err != nil {
			return ctrl.Result{}, err
		}
		setAgentHostState(host, corev1alpha1.AgentHostPhaseProvisioning, "", metav1.ConditionFalse,
			agentHostReasonProvisioning, message, "")
		return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
	}
	replaced, updateWait, err := r.reconcileAgentUpdate(ctx, host, names, template, pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	if replaced {
		return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
	}
	if !agentHostPodReady(pod) {
		setAgentHostState(host, corev1alpha1.AgentHostPhaseProvisioning, pod.Name, metav1.ConditionFalse,
			agentHostReasonProvisioning, agentHostPodWaitingMessage(pod), "")
		return ctrl.Result{RequeueAfter: sooner(agentHostResync, updateWait)}, nil
	}
	host.Status.Restarts = 0
	host.Status.LastRestartTime = nil
	setAgentHostState(host, corev1alpha1.AgentHostPhaseReady, pod.Name, metav1.ConditionTrue,
		agentHostReasonReady, "the host answers the Agent Host Protocol", "")
	result, err := r.reconcileIdle(ctx, host, names, template, pod)
	if err == nil && host.Status.IdleSuspendedAt == nil {
		result.RequeueAfter = sooner(result.RequeueAfter, updateWait)
	}
	return result, err
}

func sooner(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

func (r *AgentHostReconciler) reconcileAgentUpdate(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	template *corev1alpha1.AgentHostTemplate,
	pod *corev1.Pod,
) (bool, time.Duration, error) {
	if agenthost.PodAgentImage(pod) == r.AgentImage {
		meta.RemoveStatusCondition(&host.Status.Conditions, agentHostConditionAgentUpdatePending)
		return false, 0, nil
	}
	now := r.now()
	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type: agentHostConditionAgentUpdatePending, Status: metav1.ConditionTrue, Reason: agentHostReasonWaitingForIdle,
		Message: fmt.Sprintf("the host's Pod is replaced to update its host agent once the host reports idle, or within %d hours",
			int(agentHostAgentUpdateDeadline.Hours())),
		ObservedGeneration: host.Generation,
		LastTransitionTime: metav1.NewTime(now.UTC().Truncate(time.Second)),
	})
	since := meta.FindStatusCondition(host.Status.Conditions, agentHostConditionAgentUpdatePending).LastTransitionTime.Time
	remaining := agentHostAgentUpdateDeadline - now.Sub(since)
	replace := remaining <= 0
	if !replace && agentHostPodReady(pod) && template.Spec.IdleStatusFile != "" && r.IdleStatus != nil {
		status, err := r.IdleStatus.ReadIdleStatus(ctx, pod.Status.PodIP)
		if err != nil {
			logf.FromContext(ctx).Info("could not read agent host idle status", "agentHost", host.Name, "error", err.Error())
		} else {
			replace = status.Idle && !status.Stale
		}
	}
	if !replace {
		return false, min(remaining, agentHostIdlePoll), nil
	}
	if err := r.deletePod(ctx, host, names); err != nil {
		return false, 0, err
	}
	setAgentHostState(host, corev1alpha1.AgentHostPhaseProvisioning, "", metav1.ConditionFalse,
		agentHostReasonProvisioning, "replacing the host's Pod to update its host agent", "")
	return true, 0, nil
}

func (r *AgentHostReconciler) restartWait(host *corev1alpha1.AgentHost) time.Duration {
	if host.Status.Restarts <= 0 || host.Status.LastRestartTime == nil {
		return 0
	}
	backoff := agentHostRestartBackoff
	for i := int32(1); i < host.Status.Restarts && backoff < agentHostRestartBackoffMax; i++ {
		backoff *= 2
	}
	backoff = min(backoff, agentHostRestartBackoffMax)
	return host.Status.LastRestartTime.Add(backoff).Sub(r.now())
}

func (r *AgentHostReconciler) reconcileIdle(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	template *corev1alpha1.AgentHostTemplate,
	pod *corev1.Pod,
) (ctrl.Result, error) {
	if template.Spec.IdleSuspend == nil || r.IdleStatus == nil {
		host.Status.IdleSince = nil
		return ctrl.Result{RequeueAfter: agentHostResync}, nil
	}
	status, err := r.IdleStatus.ReadIdleStatus(ctx, pod.Status.PodIP)
	if err != nil {
		logf.FromContext(ctx).Info("could not read agent host idle status", "agentHost", host.Name, "error", err.Error())
		return ctrl.Result{RequeueAfter: agentHostIdlePoll}, nil
	}
	if !status.Idle || status.Stale || status.IdleSince == nil {
		host.Status.IdleSince = nil
		return ctrl.Result{RequeueAfter: agentHostIdlePoll}, nil
	}
	idleSince := metav1.NewTime(status.IdleSince.UTC().Truncate(time.Second))
	host.Status.IdleSince = &idleSince
	remaining := template.Spec.IdleSuspend.After.Duration - r.now().Sub(idleSince.Time)
	if remaining > 0 {
		return ctrl.Result{RequeueAfter: min(remaining, agentHostIdlePoll)}, nil
	}
	now := metav1.NewTime(r.now().UTC().Truncate(time.Second))
	host.Status.IdleSuspendedAt = &now
	return r.suspend(ctx, host, names)
}

func (r *AgentHostReconciler) deletePod(ctx context.Context, host *corev1alpha1.AgentHost, names agenthost.Names) error {
	return r.deleteOwned(ctx, host, agentHostKindPod, "", names.Pod)
}

func agentHostOwns(host *corev1alpha1.AgentHost, receipt *corev1alpha1.AgentHostObjectReceipt, object client.Object) bool {
	if receipt.UID != "" {
		return receipt.UID == string(object.GetUID())
	}
	if receipt.Namespace != "" {
		return false
	}
	return object.GetAnnotations()[agenthost.AnnotationNonce] == receipt.Nonce &&
		object.GetLabels()[agenthost.LabelHostUID] == string(host.UID)
}

func (r *AgentHostReconciler) finalize(ctx context.Context, host *corev1alpha1.AgentHost) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(host, agentHostFinalizer) {
		return ctrl.Result{}, nil
	}
	if host.Status.Phase != corev1alpha1.AgentHostPhaseDeleting {
		host.Status.Phase = corev1alpha1.AgentHostPhaseDeleting
		if err := r.Status().Update(ctx, host); err != nil {
			return ctrl.Result{}, err
		}
	}
	order := []string{
		agentHostKindPod, agentHostKindNetworkPolicy, agentHostKindPVC, agentHostKindSecret, agentHostKindConnectionSecret,
	}
	for _, kind := range order {
		pending := false
		for i := range host.Status.Objects {
			receipt := host.Status.Objects[i]
			if receipt.Kind != kind {
				continue
			}
			object := newAgentHostObject(kind)
			key := types.NamespacedName{Namespace: receiptNamespace(host, receipt.Namespace), Name: receipt.Name}
			if err := r.reader().Get(ctx, key, object); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return ctrl.Result{}, err
			}
			if !agentHostOwns(host, &receipt, object) {
				logf.FromContext(ctx).Info("leaving an agent host object this host did not create",
					"kind", kind, "namespace", key.Namespace, "name", receipt.Name)
				continue
			}
			pending = true
			if object.GetDeletionTimestamp().IsZero() {
				uid := object.GetUID()
				if err := r.Delete(ctx, object, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
					return ctrl.Result{}, err
				}
			}
		}
		if pending {
			return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
		}
	}
	controllerutil.RemoveFinalizer(host, agentHostFinalizer)
	return ctrl.Result{}, r.Update(ctx, host)
}

func newAgentHostObject(kind string) client.Object {
	switch kind {
	case agentHostKindPod:
		return &corev1.Pod{}
	case agentHostKindNetworkPolicy:
		return &networkingv1.NetworkPolicy{}
	case agentHostKindPVC:
		return &corev1.PersistentVolumeClaim{}
	default:
		return &corev1.Secret{}
	}
}

func staleAgentHostPod(pod *corev1.Pod, template *corev1alpha1.AgentHostTemplate, tokenSource string) string {
	switch {
	case pod.Annotations[agenthost.AnnotationGitHubToken] != tokenSource:
		return "replacing the host's Pod to change its credentials"
	case pod.Annotations[agenthost.AnnotationTemplateUID] != string(template.UID):
		return "replacing the host's Pod to run its recreated template"
	}
	return ""
}

func agentHostPodTerminated(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated != nil {
			return true
		}
	}
	return false
}

func agentHostPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func agentHostPodWaitingMessage(pod *corev1.Pod) string {
	statuses := append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		if status.State.Waiting == nil {
			continue
		}
		switch reason := status.State.Waiting.Reason; reason {
		case "", agentHostWaitingInitializing, agentHostWaitingCreating:
		default:
			return fmt.Sprintf("container %s is waiting: %s", status.Name, reason)
		}
	}
	return "the host is starting"
}

func setAgentHostState(
	host *corev1alpha1.AgentHost,
	phase, podName string,
	ready metav1.ConditionStatus,
	reason, message, suspendReason string,
) {
	host.Status.Phase = phase
	host.Status.PodName = podName
	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type: agentHostConditionReady, Status: ready, Reason: reason, Message: message,
		ObservedGeneration: host.Generation,
	})
	suspended := metav1.Condition{
		Type: agentHostConditionSuspended, Status: metav1.ConditionFalse, Reason: agentHostReasonRunning,
		Message: "the host is not suspended", ObservedGeneration: host.Generation,
	}
	if suspendReason != "" {
		suspended.Status = metav1.ConditionTrue
		suspended.Reason = suspendReason
		suspended.Message = message
	}
	meta.SetStatusCondition(&host.Status.Conditions, suspended)
}

func setAgentHostFailed(host *corev1alpha1.AgentHost, failure *agentHostFailure) {
	host.Status.Phase = corev1alpha1.AgentHostPhaseFailed
	host.Status.PodName = ""
	meta.RemoveStatusCondition(&host.Status.Conditions, agentHostConditionAgentUpdatePending)
	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type: agentHostConditionReady, Status: metav1.ConditionFalse, Reason: failure.reason, Message: failure.message,
		ObservedGeneration: host.Generation,
	})
}

func (r *AgentHostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if err := indexer.IndexField(context.Background(), &corev1alpha1.AgentHost{}, agentHostTemplateIndex,
		func(object client.Object) []string {
			return []string{object.(*corev1alpha1.AgentHost).Spec.TemplateRef.Name}
		}); err != nil {
		return err
	}
	if err := indexer.IndexField(context.Background(), &corev1alpha1.AgentHost{}, agentHostSecretIndex,
		func(object client.Object) []string {
			if ref := object.(*corev1alpha1.AgentHost).Spec.GitHubTokenSecretRef; ref != nil {
				return []string{ref.Name}
			}
			return nil
		}); err != nil {
		return err
	}
	if err := indexer.IndexField(context.Background(), &corev1alpha1.AgentHost{}, agentHostConnectionIndex,
		func(object client.Object) []string {
			if from := object.(*corev1alpha1.AgentHost).Spec.GitHubTokenFrom; from != nil {
				return []string{from.ConnectionRef.Name}
			}
			return nil
		}); err != nil {
		return err
	}
	componentLabel := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()[agenthost.LabelComponent] == agenthost.ComponentValue
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentHost{}).
		Named("agenthost").
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(agentHostFromAnnotation),
			builder.WithPredicates(componentLabel)).
		Watches(&networkingv1.NetworkPolicy{}, handler.EnqueueRequestsFromMapFunc(agentHostFromAnnotation),
			builder.WithPredicates(componentLabel)).
		Watches(&corev1alpha1.AgentHostTemplate{}, handler.EnqueueRequestsFromMapFunc(
			r.hostsUsing(agentHostTemplateIndex))).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.hostsUsing(agentHostSecretIndex))).
		Watches(&corev1alpha1.Connection{}, handler.EnqueueRequestsFromMapFunc(r.hostsUsing(agentHostConnectionIndex))).
		Watches(&corev1alpha1.ConnectorProvider{}, handler.EnqueueRequestsFromMapFunc(r.connectionFedHosts)).
		Complete(r)
}

func (r *AgentHostReconciler) connectionFedHosts(ctx context.Context, object client.Object) []reconcile.Request {
	hosts := &corev1alpha1.AgentHostList{}
	if err := r.List(ctx, hosts, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range hosts.Items {
		if hosts.Items[i].Spec.GitHubTokenFrom != nil {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&hosts.Items[i])})
		}
	}
	return requests
}

func agentHostFromAnnotation(_ context.Context, object client.Object) []reconcile.Request {
	namespace, name, ok := strings.Cut(object.GetAnnotations()[agenthost.AnnotationHost], "/")
	if !ok || namespace == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

func (r *AgentHostReconciler) hostsUsing(index string) handler.MapFunc {
	return func(ctx context.Context, object client.Object) []reconcile.Request {
		hosts := &corev1alpha1.AgentHostList{}
		if err := r.List(ctx, hosts, client.InNamespace(object.GetNamespace()),
			client.MatchingFields{index: object.GetName()}); err != nil {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(hosts.Items))
		for i := range hosts.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&hosts.Items[i])})
		}
		return requests
	}
}
