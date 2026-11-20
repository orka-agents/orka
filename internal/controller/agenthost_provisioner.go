/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/connectors/credential"
)

// +kubebuilder:rbac:groups=core.orka.ai,resources=agenthosts,verbs=create;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

const (
	agentHostReasonNameTaken        = "AgentHostNameTaken"
	agentHostReasonProviderConflict = "AgentHostProviderConflict"
)

// AgentHostProvisioner gives each person who links the agent-host provider
// their own AgentHost, named after their GitHub login (ADR 0036), and records
// their provider. The AgentHost reconciler feeds and stops them, and nothing
// deletes one when its Connection goes.
type AgentHostProvisioner struct {
	client.Client
	HostCredentials AgentHostCredentials
	GitHubAccounts  GitHubAccountReader
	Recorder        events.EventRecorder
}

// Reconcile implements reconcile.Reconciler.
func (r *AgentHostProvisioner) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	connection := &corev1alpha1.Connection{}
	if err := r.Get(ctx, req.NamespacedName, connection); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Only the person's canonical link may provision or identify their host.
	subject := connection.Spec.Subject
	if connection.Name != connectors.ConnectionName(connection.Spec.ProviderRef.Name, subject.Issuer, subject.Subject) {
		return ctrl.Result{}, nil
	}
	// Retained hosts need their full provider name before the Connection is
	// removed, even if the link or provider no longer qualifies for a host.
	hosts := &corev1alpha1.AgentHostList{}
	if err := r.List(ctx, hosts, client.InNamespace(connection.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for i := range hosts.Items {
		if fedBy(&hosts.Items[i], connection) {
			return ctrl.Result{}, r.recordHostProvider(ctx, &hosts.Items[i], connection.Spec.ProviderRef.Name)
		}
	}
	if !connectors.ConnectionLinked(connection) || connection.Status.GrantSequence == 0 ||
		connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost] == corev1alpha1.ConnectionAgentHostDeclined {
		return ctrl.Result{}, nil
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if provider.Spec.AgentHost == nil {
		return ctrl.Result{}, nil
	}
	if conflict, err := r.providerConflict(ctx, provider); err != nil || conflict {
		return ctrl.Result{}, err
	}

	template := provider.Spec.AgentHost.TemplateRef.Name
	held, err := r.HostCredentials.HostCredential(ctx, connection.Namespace, connection.Name, agentHostTokenHorizon)
	if errors.Is(err, credential.ErrHostLinkUnavailable) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	account, err := r.GitHubAccounts.Account(ctx, held.AccessToken)
	if err != nil {
		return ctrl.Result{}, err
	}
	name := agentHostNameFor(account.Login)
	if errs := k8svalidation.IsDNS1123Label(name); len(errs) > 0 {
		r.event(connection, corev1.EventTypeWarning, agentHostReasonNameTaken,
			fmt.Sprintf("GitHub login %q gives no valid AgentHost name; no host was provisioned", account.Login))
		return ctrl.Result{}, nil
	}
	host := &corev1alpha1.AgentHost{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: connection.Namespace,
			Annotations: map[string]string{connectors.AgentHostProviderAnnotation: connection.Spec.ProviderRef.Name},
		},
		Spec: corev1alpha1.AgentHostSpec{
			TemplateRef: corev1alpha1.AgentHostTemplateReference{Name: template},
			GitHubTokenFrom: &corev1alpha1.AgentHostTokenSource{
				ConnectionRef: corev1alpha1.LocalObjectReference{Name: connection.Name},
			},
		},
	}
	err = r.Create(ctx, host)
	if apierrors.IsAlreadyExists(err) {
		existing := &corev1alpha1.AgentHost{}
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(host), existing); getErr != nil {
			return ctrl.Result{}, getErr
		}
		if !fedBy(existing, connection) {
			r.event(connection, corev1.EventTypeWarning, agentHostReasonNameTaken,
				fmt.Sprintf("AgentHost %q already exists and was not created for this link; no host was provisioned", host.Name))
			// Deleting the other host enqueues nothing for this link.
			return ctrl.Result{RequeueAfter: agentHostResync}, nil
		}
		return ctrl.Result{}, r.recordHostProvider(ctx, existing, connection.Spec.ProviderRef.Name)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	r.event(connection, corev1.EventTypeNormal, "AgentHostCreated", fmt.Sprintf("created AgentHost %q", host.Name))
	return ctrl.Result{}, nil
}

// recordHostProvider also upgrades hosts created before the annotation existed.
func (r *AgentHostProvisioner) recordHostProvider(ctx context.Context, host *corev1alpha1.AgentHost, provider string) error {
	if host.Annotations[connectors.AgentHostProviderAnnotation] == provider {
		return nil
	}
	base := host.DeepCopy()
	if host.Annotations == nil {
		host.Annotations = map[string]string{}
	}
	host.Annotations[connectors.AgentHostProviderAnnotation] = provider
	// Do not patch a replacement host or overwrite concurrent metadata changes.
	return r.Patch(ctx, host, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// providerConflict reports whether another provider in the namespace also
// serves agent hosts; then none provisions, and each says so.
func (r *AgentHostProvisioner) providerConflict(ctx context.Context, provider *corev1alpha1.ConnectorProvider) (bool, error) {
	providers := &corev1alpha1.ConnectorProviderList{}
	if err := r.List(ctx, providers, client.InNamespace(provider.Namespace)); err != nil {
		return false, err
	}
	var serving []*corev1alpha1.ConnectorProvider
	for i := range providers.Items {
		if providers.Items[i].Spec.AgentHost != nil && providers.Items[i].DeletionTimestamp.IsZero() {
			serving = append(serving, &providers.Items[i])
		}
	}
	if len(serving) < 2 {
		return false, nil
	}
	for _, other := range serving {
		r.event(other, corev1.EventTypeWarning, agentHostReasonProviderConflict,
			fmt.Sprintf("%d ConnectorProviders in this namespace set spec.agentHost; none provisions agent hosts until one does", len(serving)))
	}
	return true, nil
}

func (r *AgentHostProvisioner) event(object client.Object, kind, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(object, nil, kind, reason, "Provision", "%s", message)
	}
}

// agentHostNameFor names a person's host after their GitHub login. Managed
// user logins carry an underscore, which a name cannot; it becomes a double
// hyphen, which no GitHub login contains, so no two logins share a name.
func agentHostNameFor(login string) string {
	return "gh-" + strings.ReplaceAll(strings.ToLower(login), "_", "--")
}

// fedBy reports whether host takes its token from connection.
func fedBy(host *corev1alpha1.AgentHost, connection *corev1alpha1.Connection) bool {
	from := host.Spec.GitHubTokenFrom
	return from != nil && from.ConnectionRef.Name == connection.Name
}

// SetupWithManager registers the provisioner. It runs only beside the
// AgentHost reconciler, with connectors enabled.
func (r *AgentHostProvisioner) SetupWithManager(mgr ctrl.Manager) error {
	if r.HostCredentials == nil || r.GitHubAccounts == nil {
		return errors.New("agent host provisioning needs connection credentials and a GitHub account reader")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Connection{}).
		Named("agenthost-provisioner").
		// A provider change can start or end a namespace's provider conflict,
		// so it revisits every link there; a deleted host is provisioned
		// again for its link.
		Watches(&corev1alpha1.ConnectorProvider{}, handler.EnqueueRequestsFromMapFunc(r.connectionsIn)).
		Watches(&corev1alpha1.AgentHost{}, handler.EnqueueRequestsFromMapFunc(hostConnection)).
		Complete(r)
}

func (r *AgentHostProvisioner) connectionsIn(ctx context.Context, object client.Object) []reconcile.Request {
	connections := &corev1alpha1.ConnectionList{}
	if err := r.List(ctx, connections, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(connections.Items))
	for i := range connections.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&connections.Items[i])})
	}
	return requests
}

func hostConnection(_ context.Context, object client.Object) []reconcile.Request {
	from := object.(*corev1alpha1.AgentHost).Spec.GitHubTokenFrom
	if from == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: from.ConnectionRef.Name}}}
}
