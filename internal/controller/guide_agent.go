/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	// GuideAgentName is the fixed name of the controller-owned guide Agent.
	GuideAgentName = "orka-guide"
	// GuideDocsMountPath is where the docs snapshot toolbox is mounted.
	GuideDocsMountPath = "/opt/orka-docs"
	// DefaultGuideMaxTurns bounds one guide answer.
	DefaultGuideMaxTurns int32 = 30

	guideComponentLabelKey   = "app.kubernetes.io/component"
	guideComponentLabelValue = "guide"
	guideNameLabelKey        = "app.kubernetes.io/name"
	guideNameLabelValue      = "orka"
)

// guideSystemPrompt only bootstraps the guide. Its instructions and sources
// both come from the docs snapshot, so they always match one Orka version.
const guideSystemPrompt = `You are the guide for this Orka installation. You answer questions about Orka.
Before you answer, read /opt/orka-docs/guide/SKILL.md and follow it, then read
/opt/orka-docs/guide/references/in-cluster.md, which replaces the parts of the skill
that need git, gh or a shell. Answer only from the files under /opt/orka-docs.
You are read-only: you change nothing, and you give people the commands to run.`

// guideAllowedTools is the guide's whole tool policy: it reads, never writes
// or runs commands.
var guideAllowedTools = []string{"Glob", "Grep", "Read"}

// GuideRuntimeTypes lists the built-in runtimes the guide supports: those that
// enforce its Read, Glob and Grep tool set natively. Codex is left out because
// its read-only sessions still run read-only shell commands, and OpenCode
// because its read-only sessions disable Grep.
var GuideRuntimeTypes = []corev1alpha1.AgentRuntimeType{
	corev1alpha1.AgentRuntimeClaude,
	corev1alpha1.AgentRuntimeCopilot,
}

// GuideAgentConfig is the controller's guide configuration, set from flags.
type GuideAgentConfig struct {
	// Enabled creates and maintains the guide Agent. When false, the
	// controller deletes a guide Agent it created earlier.
	Enabled bool
	// Namespace holds the guide Agent: the controller's watch namespace.
	Namespace string
	// Runtime is one of GuideRuntimeTypes.
	Runtime corev1alpha1.AgentRuntimeType
	// Model is the model ID the runtime requests from the provider proxy.
	Model string
	// MaxTurns is the Agent's runtime.defaultMaxTurns.
	MaxTurns int32
	// DocsImage is the digest-pinned docs snapshot image.
	DocsImage string
	// ContractVersion is the installation's harness contract.
	ContractVersion corev1alpha1.AgentRuntimeContractVersion
	// OwnerDeployment names the controller Deployment in Namespace. When set,
	// the guide Agent carries an ownerReference to it, so deleting the
	// installation also deletes the Agent, and the Agent is created only
	// once that Deployment is found.
	OwnerDeployment string
}

// Validate rejects an enabled configuration that could never produce a
// usable guide Agent. images are the configured runtime images, before
// resolution: a registry or mounting outage is not a configuration error, and
// the Agent and its Tasks report it instead.
func (c GuideAgentConfig) Validate(policy ACPToolboxPolicy, images ACPRuntimeImages) error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Namespace) == "" {
		return fmt.Errorf("the guide needs a namespace")
	}
	if !slices.Contains(GuideRuntimeTypes, c.Runtime) {
		return fmt.Errorf("--guide-runtime %q is not supported; use claude or copilot", c.Runtime)
	}
	if image, err := configuredACPRuntimeImage(string(c.Runtime), images); err != nil || image == "" {
		return fmt.Errorf("--guide-runtime %s needs its runtime image (--acp-%s-runtime-image)", c.Runtime, c.Runtime)
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("--guide-model is required when the guide is enabled")
	}
	if c.MaxTurns < 1 || c.MaxTurns > 1000 {
		return fmt.Errorf("--guide-max-turns must be between 1 and 1000, got %d", c.MaxTurns)
	}
	if strings.TrimSpace(c.DocsImage) == "" {
		return fmt.Errorf("--guide-docs-image is required when the guide is enabled")
	}
	if c.ContractVersion == "" {
		return fmt.Errorf("the guide needs the installation's harness contract version")
	}
	policy.UnavailableReason = ""
	if err := policy.ValidateAgentToolboxes(c.DesiredAgent()); err != nil {
		return fmt.Errorf("--guide-docs-image: %w", err)
	}
	return nil
}

// DesiredAgent returns the guide Agent the controller maintains, without an
// owner reference.
func (c GuideAgentConfig) DesiredAgent() *corev1alpha1.Agent {
	contractVersion := c.ContractVersion
	maxTurns := c.MaxTurns
	allowBash := false
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GuideAgentName,
			Namespace: c.Namespace,
			Labels:    guideAgentLabels(),
		},
		Spec: corev1alpha1.AgentSpec{
			Model:        &corev1alpha1.ModelConfig{Name: strings.TrimSpace(c.Model)},
			SystemPrompt: &corev1alpha1.PromptSource{Inline: guideSystemPrompt},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:                c.Runtime,
				ContractVersion:     &contractVersion,
				DefaultMaxTurns:     &maxTurns,
				DefaultAllowedTools: slices.Clone(guideAllowedTools),
				DefaultAllowBash:    &allowBash,
				Toolboxes: []corev1alpha1.AgentToolbox{{
					Image:     strings.TrimSpace(c.DocsImage),
					MountPath: GuideDocsMountPath,
				}},
			},
		},
	}
}

func guideAgentLabels() map[string]string {
	return map[string]string{
		guideNameLabelKey:      guideNameLabelValue,
		guideComponentLabelKey: guideComponentLabelValue,
		managedByLabelKey:      managedByLabelValue,
		orkaManagedByLabelKey:  managedByLabelValue,
	}
}

// isManagedGuideAgent reports whether the controller created agent: it carries
// the guide's managed-by labels, or an owner reference to the controller
// Deployment with ownerUID, so removing the labels doesn't release it. An Agent
// that only shares the name is the operator's and is never changed or deleted.
func isManagedGuideAgent(agent *corev1alpha1.Agent, ownerUID types.UID) bool {
	if agent.Labels[guideComponentLabelKey] == guideComponentLabelValue &&
		agent.Labels[managedByLabelKey] == managedByLabelValue {
		return true
	}
	if ownerUID == "" {
		return false
	}
	for _, ref := range agent.OwnerReferences {
		if ref.UID == ownerUID && ref.Kind == "Deployment" && ref.APIVersion == appsv1.SchemeGroupVersion.String() {
			return true
		}
	}
	return false
}

// GuideAgentReconciler creates the guide Agent when the guide is enabled,
// reverts changes to it, and deletes it when the guide is disabled.
type GuideAgentReconciler struct {
	client.Client
	// APIReader reads the owner Deployment, which the manager cache does not
	// hold.
	APIReader client.Reader
	Config    GuideAgentConfig
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get

// Reconcile converges the one guide Agent with the configuration.
func (r *GuideAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.NamespacedName != r.key() {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx).WithValues("agent", req.NamespacedName)

	current := &corev1alpha1.Agent{}
	err := r.Get(ctx, req.NamespacedName, current)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	found := err == nil

	if !r.Config.Enabled {
		if !found || !current.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, nil
		}
		managed := isManagedGuideAgent(current, "")
		if !managed {
			ownerUID, err := r.ownerUID(ctx)
			if err != nil {
				return ctrl.Result{}, err
			}
			managed = isManagedGuideAgent(current, ownerUID)
		}
		if !managed {
			return ctrl.Result{}, nil
		}
		uid, resourceVersion := current.UID, current.ResourceVersion
		if err := r.Delete(ctx, current, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		logger.Info("Deleted the guide Agent because the guide is disabled")
		return ctrl.Result{}, nil
	}

	if found && !current.DeletionTimestamp.IsZero() {
		// The delete event recreates it.
		return ctrl.Result{}, nil
	}

	owner, err := r.ownerReference(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	var ownerUID types.UID
	if owner != nil {
		ownerUID = owner.UID
	}
	if found && !isManagedGuideAgent(current, ownerUID) {
		logger.Info("An Agent named " + GuideAgentName + " exists that the controller didn't create; leaving it unchanged. Delete or rename it to let the controller create the guide.")
		return ctrl.Result{}, nil
	}

	desired := r.Config.DesiredAgent()
	if !found {
		if owner != nil {
			desired.OwnerReferences = []metav1.OwnerReference{*owner}
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("create the guide Agent: %w", err)
		}
		logger.Info("Created the guide Agent", "runtime", desired.Spec.Runtime.Type, "model", desired.Spec.Model.Name)
		return ctrl.Result{}, nil
	}

	updated := current.DeepCopy()
	changed := false
	if !equality.Semantic.DeepEqual(updated.Spec, desired.Spec) {
		updated.Spec = desired.Spec
		changed = true
	}
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	for key, value := range desired.Labels {
		if updated.Labels[key] != value {
			updated.Labels[key] = value
			changed = true
		}
	}
	if owner != nil {
		refs, ownerChanged := withGuideOwner(updated.OwnerReferences, *owner)
		updated.OwnerReferences = refs
		changed = changed || ownerChanged
	}
	if !changed {
		return ctrl.Result{}, nil
	}
	if err := r.Update(ctx, updated); err != nil {
		return ctrl.Result{}, fmt.Errorf("update the guide Agent: %w", err)
	}
	logger.Info("Restored the guide Agent to the controller's configuration")
	return ctrl.Result{}, nil
}

// ownerReference returns the reference to the controller Deployment, or nil
// when none is configured. A configured Deployment that is missing or being
// deleted is an error, so a guide Agent is never created without its owner
// while the installation is being removed.
func (r *GuideAgentReconciler) ownerReference(ctx context.Context) (*metav1.OwnerReference, error) {
	name := strings.TrimSpace(r.Config.OwnerDeployment)
	if name == "" {
		return nil, nil
	}
	deployment := &appsv1.Deployment{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: name}, deployment); err != nil {
		return nil, fmt.Errorf("read the guide owner Deployment %s/%s: %w", r.Config.Namespace, name, err)
	}
	if !deployment.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("the guide owner Deployment %s/%s is being deleted", r.Config.Namespace, name)
	}
	return &metav1.OwnerReference{
		APIVersion: appsv1.SchemeGroupVersion.String(),
		Kind:       "Deployment",
		Name:       deployment.Name,
		UID:        deployment.UID,
	}, nil
}

// ownerUID returns the UID of the configured controller Deployment, or "" when
// none is configured or it no longer exists.
func (r *GuideAgentReconciler) ownerUID(ctx context.Context) (types.UID, error) {
	name := strings.TrimSpace(r.Config.OwnerDeployment)
	if name == "" {
		return "", nil
	}
	deployment := &appsv1.Deployment{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: name}, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("read the guide owner Deployment %s/%s: %w", r.Config.Namespace, name, err)
	}
	return deployment.UID, nil
}

// withGuideOwner replaces any reference to a Deployment of the same name with
// owner, keeping other owners.
func withGuideOwner(refs []metav1.OwnerReference, owner metav1.OwnerReference) ([]metav1.OwnerReference, bool) {
	result := make([]metav1.OwnerReference, 0, len(refs)+1)
	present := false
	for _, ref := range refs {
		if ref.APIVersion == owner.APIVersion && ref.Kind == owner.Kind && ref.Name == owner.Name {
			if !present && equality.Semantic.DeepEqual(ref, owner) {
				present = true
				result = append(result, ref)
			}
			continue
		}
		result = append(result, ref)
	}
	if !present {
		result = append(result, owner)
	}
	return result, !present || len(result) != len(refs)
}

func (r *GuideAgentReconciler) key() types.NamespacedName {
	return types.NamespacedName{Namespace: r.Config.Namespace, Name: GuideAgentName}
}

// SetupWithManager watches only the guide Agent and enqueues it once at start,
// so the guide is created, or cleaned up after being disabled, without any
// Agent event.
func (r *GuideAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	key := r.key()
	isGuide := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetNamespace() == key.Namespace && object.GetName() == key.Name
	})
	// Status writes from the Agent controller are not drift.
	ownedFieldsChanged := predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		if e.ObjectOld == nil || e.ObjectNew == nil {
			return true
		}
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			!maps.Equal(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()) ||
			!equality.Semantic.DeepEqual(e.ObjectOld.GetOwnerReferences(), e.ObjectNew.GetOwnerReferences()) ||
			!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp())
	}}
	start := source.Func(func(_ context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		queue.Add(reconcile.Request{NamespacedName: key})
		return nil
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Agent{}, builder.WithPredicates(isGuide, ownedFieldsChanged)).
		WatchesRawSource(start).
		Named("guide-agent").
		Complete(r)
}
