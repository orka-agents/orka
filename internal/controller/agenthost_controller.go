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
)

// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosts/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete

const (
	agentHostFinalizer = "core.orka.ai/agenthost"

	agentHostConditionReady     = "Ready"
	agentHostConditionSuspended = "Suspended"
	// agentHostConditionAgentUpdatePending is True while a running host waits to
	// move to the controller's current host agent. Its lastTransitionTime is
	// when the controller first saw the difference.
	agentHostConditionAgentUpdatePending = "HostAgentUpdatePending"

	agentHostReasonReady                  = "HostReady"
	agentHostReasonProvisioning           = "Provisioning"
	agentHostReasonRestarting             = "Restarting"
	agentHostReasonSuspended              = "Suspended"
	agentHostReasonIdle                   = "Idle"
	agentHostReasonRequested              = "Requested"
	agentHostReasonRunning                = "Running"
	agentHostReasonTemplateNotFound       = "TemplateNotFound"
	agentHostReasonInvalidTemplate        = "InvalidTemplate"
	agentHostReasonGitHubTokenRequired    = "GitHubTokenRequired"
	agentHostReasonGitHubTokenUnavailable = "GitHubTokenUnavailable"
	agentHostReasonObjectCollision        = "ObjectCollision"
	agentHostReasonObjectRejected         = "ObjectRejected"
	agentHostReasonStateVolumeLost        = "StateVolumeLost"
	agentHostReasonHostAgentUnavailable   = "HostAgentUnavailable"
	agentHostReasonHostNamespaceChanged   = "HostNamespaceChanged"
	agentHostReasonWaitingForIdle         = "WaitingForIdle"

	agentHostKindSecret           = "Secret"
	agentHostKindConnectionSecret = "ConnectionSecret"
	agentHostKindPVC              = "PersistentVolumeClaim"
	agentHostKindNetworkPolicy    = "NetworkPolicy"
	agentHostKindPod              = "Pod"

	agentHostResync            = 5 * time.Minute
	agentHostIdlePoll          = time.Minute
	agentHostCleanupRequeue    = 5 * time.Second
	agentHostRestartBackoff    = 10 * time.Second
	agentHostRestartBackoffMax = 5 * time.Minute
	agentHostTokenBytes        = 32
	agentHostNonceBytes        = 16
	// agentHostWakeClockSkew lets a wake request dated slightly ahead count now.
	agentHostWakeClockSkew = time.Minute
	// agentHostAgentUpdateDeadline bounds how long a host that never reports
	// idle keeps an earlier host agent.
	agentHostAgentUpdateDeadline = 24 * time.Hour

	// Kubelet reasons for a container that is starting normally.
	agentHostWaitingInitializing = "PodInitializing"
	agentHostWaitingCreating     = "ContainerCreating"

	agentHostTemplateIndex = "spec.templateRef.name"
	agentHostSecretIndex   = "spec.githubTokenSecretRef.name"
)

// agentHostFailure stops reconciliation with a Ready=False reason the user can act on.
type agentHostFailure struct {
	reason  string
	message string
}

func (f *agentHostFailure) Error() string { return f.reason + ": " + f.message }

func agentHostFailed(reason, format string, args ...any) error {
	return &agentHostFailure{reason: reason, message: fmt.Sprintf(format, args...)}
}

// errAgentHostObjectGone reports that an object the controller created no longer exists.
var errAgentHostObjectGone = errors.New("agent host object is gone")

// AgentHostReconciler runs each AgentHost as Orka-owned objects in the host
// namespace: a connection-token Secret, an optional GitHub-token Secret, a
// state volume, a NetworkPolicy and one Pod, plus a connection Secret in the
// AgentHost's own namespace for Standalone access. It records a receipt for
// every object before creating it and adopts only objects whose receipt matches.
// Only the controller may write in the host namespace.
type AgentHostReconciler struct {
	client.Client
	// APIReader reads without the cache: to confirm that a created object is
	// really gone, and to clean up in a host namespace the cache may not cover.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// HostNamespace receives new hosts' objects.
	HostNamespace string
	// AgentImage is the digest-pinned image holding the Orka host agent.
	AgentImage string
	// Controller selects the controller Pods that may read host status.
	Controller agenthost.ControllerPeer
	// IdleStatus reads a running host's idle status.
	IdleStatus agenthost.StatusReader
	Now        func() time.Time
	Random     func([]byte) (int, error)
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

// Reconcile implements reconcile.Reconciler.
func (r *AgentHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &corev1alpha1.AgentHost{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !host.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, host)
	}
	// The finalizer and the host namespace are persisted before anything is
	// created, so cleanup always knows where to look.
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
	// A wake request counts once its time has come, so it wakes the host at
	// most once: a request dated in the future would otherwise be later than
	// every idle suspension until that date and defeat them all.
	if host.Status.IdleSuspendedAt != nil && host.Spec.WakeRequestedAt != nil &&
		host.Spec.WakeRequestedAt.After(host.Status.IdleSuspendedAt.Time) &&
		!host.Spec.WakeRequestedAt.After(r.now().Add(agentHostWakeClockSkew)) {
		host.Status.IdleSuspendedAt = nil
		host.Status.IdleSince = nil
	}
	// Suspension needs nothing but the Pod's receipt, so it is honored even
	// when the template or token is gone.
	if host.Spec.Suspended || host.Status.IdleSuspendedAt != nil {
		return r.suspend(ctx, host, names)
	}

	template := &corev1alpha1.AgentHostTemplate{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: host.Spec.TemplateRef.Name}, template); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, agentHostFailed(agentHostReasonTemplateNotFound,
				"AgentHostTemplate %q does not exist", host.Spec.TemplateRef.Name)
		}
		return ctrl.Result{}, err
	}
	if err := agenthost.ValidateTemplate(template); err != nil {
		return ctrl.Result{}, agentHostFailed(agentHostReasonInvalidTemplate, "%v", err)
	}
	if strings.TrimSpace(r.AgentImage) == "" {
		return ctrl.Result{}, agentHostFailed(agentHostReasonHostAgentUnavailable,
			"the controller has no host agent image; set --agent-host-agent-image")
	}
	githubToken, tokenSource, err := r.githubToken(ctx, host, template)
	if err != nil {
		return ctrl.Result{}, err
	}

	connectionToken, err := r.ensureConnectionToken(ctx, host, names)
	if err != nil {
		return ctrl.Result{}, err
	}
	if githubToken != nil {
		stopping, err := r.retirePodOfAnotherToken(ctx, host, names, tokenSource)
		if err != nil {
			return ctrl.Result{}, err
		}
		if stopping {
			return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
		}
		if err := r.ensureGitHubToken(ctx, host, names, githubToken); err != nil {
			return ctrl.Result{}, err
		}
	} else if err := r.deleteOwned(ctx, host, agentHostKindSecret, "", names.GitHubTokenSecret); err != nil {
		return ctrl.Result{}, err
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
	// The controller's namespace or Pod labels may have changed since the
	// policy was made; without current rules the controller cannot read the
	// host's status.
	if desired := agenthost.HostNetworkPolicy(host, names, "", template, r.Controller); !equality.Semantic.DeepEqual(policy.Spec, desired.Spec) {
		policy.Spec = desired.Spec
		if err := r.Update(ctx, policy); err != nil {
			return ctrl.Result{}, fmt.Errorf("update NetworkPolicy %s: %w", names.NetworkPolicy, err)
		}
	}
	if err := r.reconcileConnectionSecret(ctx, host, names, template, connectionToken); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcilePod(ctx, host, names, template, tokenSource)
}

// githubToken returns the user's token when the template uses one, or nil,
// and where it comes from as Secret name and key. Admission checks that
// whoever set githubTokenSecretRef may read that Secret.
func (r *AgentHostReconciler) githubToken(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	template *corev1alpha1.AgentHostTemplate,
) ([]byte, string, error) {
	policy := template.Spec.GitHubToken
	if policy == "" || policy == corev1alpha1.AgentHostCredentialNone {
		return nil, "", nil
	}
	ref := host.Spec.GitHubTokenSecretRef
	if ref == nil {
		if policy == corev1alpha1.AgentHostCredentialRequired {
			return nil, "", agentHostFailed(agentHostReasonGitHubTokenRequired,
				"template %q requires spec.githubTokenSecretRef", template.Name)
		}
		return nil, "", nil
	}
	key := ref.Key
	if key == "" {
		key = agenthost.TokenKey
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", agentHostFailed(agentHostReasonGitHubTokenUnavailable, "Secret %q does not exist", ref.Name)
		}
		return nil, "", err
	}
	token := bytes.TrimSpace(secret.Data[key])
	if len(token) == 0 {
		return nil, "", agentHostFailed(agentHostReasonGitHubTokenUnavailable, "Secret %q has no data at key %q", ref.Name, key)
	}
	return token, ref.Name + "/" + key, nil
}

// ensureConnectionToken creates the host's connection token once and returns it.
// If the Secret was deleted, a new token is minted and the Pod replaced so the
// host and its clients agree on it again.
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

// retirePodOfAnotherToken stops a Pod started with a GitHub token from another
// Secret or key, and reports whether one is still stopping. The copy is
// mounted into the running Pod and follows its Secret, so a token from a new
// source is written only once no Pod of the old one remains: otherwise the
// old Pod's processes could read it before they stop.
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
		// The cache may lag behind a recent create, including one whose UID
		// was never recorded; only the API server can say the Pod is gone.
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
	// Keep the copy current; the mounted file follows, and the host can pick
	// up a rotated token without restarting.
	secret.Data = map[string][]byte{agenthost.TokenKey: append([]byte(nil), token...)}
	return r.Update(ctx, secret)
}

// ensureRecreatable is ensureObject for objects that are safe to recreate.
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

// receiptNamespace resolves a receipt's namespace; empty means the host namespace.
func receiptNamespace(host *corev1alpha1.AgentHost, namespace string) string {
	if namespace == "" {
		return host.Status.HostNamespace
	}
	return namespace
}

// ensureObject reads the named object into live, creating it from build when
// absent. A receipt with a fresh nonce is persisted before creation and the
// created object's UID right after. Once a UID is recorded, only that exact
// object is accepted. An object found while the receipt has no UID yet, which
// happens only if the controller stopped between creating it and recording
// it, is adopted only if it carries this host's UID label and the receipt's
// nonce. It returns errAgentHostObjectGone when an object it created has
// disappeared, and an ObjectCollision failure for any object it did not create.
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
		// The cache may lag behind a recent create; ask the API server.
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
		// The API server's own rules (RuntimeClasses, resource rules such as
		// huge pages, Pod Security, quotas, admission policies) are reported
		// with its reason rather than mirrored here; the next resync retries.
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

// agentHostAPIMessage is the API server's message for a refused request.
func agentHostAPIMessage(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Message != "" {
		return status.Status().Message
	}
	return err.Error()
}

// copyAgentHostObject copies a created object into the caller's value of the same type.
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

// deleteOwned deletes an object only if this host's receipt matches it, and
// forgets the receipt once it is gone.
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

// reconcileConnectionSecret keeps the Standalone connection Secret in the
// AgentHost's namespace under a receipt, and removes it for Relay hosts.
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
			// Garbage collection is a fallback; the finalizer deletes it first.
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

// suspend stops the host's Pod and keeps its volume and Secrets.
func (r *AgentHostReconciler) suspend(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
) (ctrl.Result, error) {
	reason := agentHostReasonRequested
	if !host.Spec.Suspended {
		reason = agentHostReasonIdle
	}
	// The next Pod starts on the current host agent.
	meta.RemoveStatusCondition(&host.Status.Conditions, agentHostConditionAgentUpdatePending)
	if err := r.deletePod(ctx, host, names); err != nil {
		return ctrl.Result{}, err
	}
	if findAgentHostReceipt(host, agentHostKindPod, "", names.Pod) != nil {
		setAgentHostState(host, corev1alpha1.AgentHostPhaseSuspended, "", metav1.ConditionFalse,
			agentHostReasonSuspended, "the host's Pod is stopping", reason)
		return ctrl.Result{RequeueAfter: agentHostCleanupRequeue}, nil
	}
	message := "the host is suspended; its state volume is kept"
	if reason == agentHostReasonIdle {
		message = "the host was idle and is suspended; set spec.wakeRequestedAt to start it"
	}
	setAgentHostState(host, corev1alpha1.AgentHostPhaseSuspended, "", metav1.ConditionFalse,
		agentHostReasonSuspended, message, reason)
	return ctrl.Result{RequeueAfter: agentHostResync}, nil
}

// stopFailedHost stops the Pod of a host that cannot run correctly, and
// deletes the copied GitHub token when its source is gone. The volume stays.
func (r *AgentHostReconciler) stopFailedHost(
	ctx context.Context,
	host *corev1alpha1.AgentHost,
	names agenthost.Names,
	failure *agentHostFailure,
) error {
	if failure.reason == agentHostReasonHostNamespaceChanged {
		// The objects are outside the namespace this controller manages; only
		// deletion of the AgentHost cleans them up.
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
	if failure.reason == agentHostReasonGitHubTokenRequired || failure.reason == agentHostReasonGitHubTokenUnavailable {
		return ignoreCollision(r.deleteOwned(ctx, host, agentHostKindSecret, "", names.GitHubTokenSecret))
	}
	return nil
}

// reconcilePod runs the host's Pod, replaces it with backoff when it fails, and
// reports readiness.
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
			AgentImage: r.AgentImage, GitHubTokenSource: tokenSource,
		})
	})
	if errors.Is(err, errAgentHostObjectGone) {
		// A deleted or replaced Pod is expected; start a new one.
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
		// Retire the whole Pod; its replacement waits for the backoff.
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

// sooner is the earlier of two requeue delays, where zero means none.
func sooner(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// reconcileAgentUpdate moves a live host to the controller's current host
// agent, such as after an upgrade, and reports whether it replaced the Pod or
// when to look again. The agent is copied into the Pod when it starts, so
// moving means a new Pod, which loses a running turn: it waits until the host
// reports idle, but no longer than agentHostAgentUpdateDeadline, so a host
// that never reports idle, or never becomes ready, still moves.
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
		// Kept while the condition stays True, so the deadline counts from
		// when the update was first seen.
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

// restartWait is how long a failed host still has to wait before its next Pod.
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

// reconcileIdle suspends a host that has been idle for the template's
// idleSuspend duration.
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

// deletePod deletes the host's Pod if this host created it, and forgets the
// receipt once it is gone.
func (r *AgentHostReconciler) deletePod(ctx context.Context, host *corev1alpha1.AgentHost, names agenthost.Names) error {
	return r.deleteOwned(ctx, host, agentHostKindPod, "", names.Pod)
}

func agentHostOwns(host *corev1alpha1.AgentHost, receipt *corev1alpha1.AgentHostObjectReceipt, object client.Object) bool {
	if receipt.UID != "" {
		return receipt.UID == string(object.GetUID())
	}
	// Nonce recovery relies on the controller being the only writer. That holds
	// in the host namespace only; anyone who can read the receipt and create
	// objects in the AgentHost's namespace could copy the nonce there.
	if receipt.Namespace != "" {
		return false
	}
	return object.GetAnnotations()[agenthost.AnnotationNonce] == receipt.Nonce &&
		object.GetLabels()[agenthost.LabelHostUID] == string(host.UID)
}

// finalize deletes every object this host created, the Pod first so the
// workload never runs without its NetworkPolicy or credentials, and removes
// the finalizer once they are all gone. It reads without the cache, so it
// works in a host namespace the controller no longer caches.
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
			// Delete kinds in order: confinement and credentials outlive the Pod,
			// and the volume cannot go while the Pod uses it.
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

// staleAgentHostPod says why a running Pod must be replaced, or returns "".
func staleAgentHostPod(pod *corev1.Pod, template *corev1alpha1.AgentHostTemplate, tokenSource string) string {
	switch {
	case pod.Annotations[agenthost.AnnotationGitHubToken] != tokenSource:
		return "replacing the host's Pod to change its credentials"
	case pod.Annotations[agenthost.AnnotationTemplateUID] != string(template.UID):
		// Recreating a template under its name is how an operator changes
		// it; its hosts move to the new one on their existing volumes.
		return "replacing the host's Pod to run its recreated template"
	}
	return ""
}

func agentHostPodTerminated(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true
	}
	// With restartPolicy Never, nothing restarts a regular container that
	// exits: the host leaves the Pod running only until its sidecar stops, and
	// a template helper leaves it running but never Ready. Do not wait for
	// either. The Orka sidecar is an init container and restarts on its own.
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

// SetupWithManager registers the controller, its indexes and its watches.
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
	componentLabel := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()[agenthost.LabelComponent] == agenthost.ComponentValue
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentHost{}).
		Named("agenthost").
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(agentHostFromAnnotation),
			builder.WithPredicates(componentLabel)).
		// A host's NetworkPolicy is its confinement, so a deleted or edited one
		// is repaired at once rather than at the next resync.
		Watches(&networkingv1.NetworkPolicy{}, handler.EnqueueRequestsFromMapFunc(agentHostFromAnnotation),
			builder.WithPredicates(componentLabel)).
		Watches(&corev1alpha1.AgentHostTemplate{}, handler.EnqueueRequestsFromMapFunc(
			r.hostsUsing(agentHostTemplateIndex))).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.hostsUsing(agentHostSecretIndex))).
		Complete(r)
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
