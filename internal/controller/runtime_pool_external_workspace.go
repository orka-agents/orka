// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const externalRuntimeEndpointAnnotation = "orka.ai/external-runtime-endpoint"
const externalRuntimeEvidenceAnnotation = "orka.ai/external-runtime-instance-evidence"
const externalNativeRuntimeUIDEnvName = "ORKA_ACP_POD_UID"

type externalRuntimeInstanceEvidence struct {
	WorkspaceUID  types.UID                          `json:"workspaceUID"`
	Sequence      int64                              `json:"sequence"`
	Identity      workspacev1alpha1.InstanceIdentity `json:"identity"`
	Pod           workspacev1alpha1.PodReference     `json:"pod"`
	NativeProcess bool                               `json:"nativeProcess,omitempty"`
	RuntimeUID    types.UID                          `json:"runtimeUID,omitempty"`
	Endpoint      string                             `json:"endpoint,omitempty"`
}

func runtimePoolHasExternalWorkspace(pool *corev1alpha1.RuntimePool) bool {
	return pool != nil && pool.Spec.ExecutionWorkspace != nil && pool.Spec.ExecutionWorkspace.WorkspaceRef != nil && pool.Spec.ExecutionWorkspace.Workload != nil
}

func (r *RuntimePoolReconciler) externalPoolWorkspace(ctx context.Context, pool *corev1alpha1.RuntimePool) (*workspacev1alpha1.ExecutionWorkspace, error) {
	if !runtimePoolHasExternalWorkspace(pool) {
		return nil, fmt.Errorf("RuntimePool has no external workload binding")
	}
	ref := pool.Spec.ExecutionWorkspace.WorkspaceRef
	w := &workspacev1alpha1.ExecutionWorkspace{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: ref.Name}, w); err != nil {
		return nil, err
	}
	if ref.UID == "" || w.UID != ref.UID || pool.Labels[acpExecutionWorkspaceLinkLabel] != w.Name || pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(w.UID) || w.Annotations[acpExecutionWorkspacePoolAnnotation] != pool.Name {
		return nil, fmt.Errorf("external RuntimePool workspace identity does not match its immutable link")
	}
	if string(pool.Spec.ExecutionWorkspace.Provider) != w.Labels[workspacev1alpha1.ProviderControllerLabel] {
		return nil, fmt.Errorf("external RuntimePool provider route does not match its frozen workspace")
	}
	return w, nil
}

// Core consumes provider evidence and writes intent. It never writes the
// provider's state, attachment acknowledgement, native objects or journal.
//
//nolint:gocyclo // Keep admission, immutable sequence transitions and bootstrap fences explicit.
func (r *RuntimePoolReconciler) reconcileExternalWorkspaceRuntimePool(ctx context.Context, pool *corev1alpha1.RuntimePool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pool, runtimePoolFinalizer) {
		before := pool.DeepCopy()
		controllerutil.AddFinalizer(pool, runtimePoolFinalizer)
		if err := r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	w, err := r.externalPoolWorkspace(ctx, pool)
	if err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "external workspace binding is unavailable")
	}
	deleting := !pool.DeletionTimestamp.IsZero() || !w.DeletionTimestamp.IsZero() || w.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	stopping := deleting || pool.Spec.DesiredReplicas == 0 || w.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || pool.Annotations["orka.ai/external-runtime-retirement-requested"] == "true"
	if stopping {
		return r.reconcileExternalWorkspaceRetirement(ctx, pool, w, deleting)
	}
	if r.CleanupOnly || r.WorkspaceCleanupOnly {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "workspace dispatch is disabled")
	}
	if r.Epochs != nil {
		if current, ready := r.Epochs.Current(); !ready || current.Epoch <= 0 {
			return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "waiting for the authoritative controller epoch")
		}
	}
	if !workspaceCurrentlyAdmittedByCore(w) {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "waiting for current workspace admission")
	}
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Name: w.Spec.ProviderBinding.Name}, provider); err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "workspace provider is unavailable")
	}
	if provider.UID != w.Spec.ProviderBinding.UID || provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDisabled || !externalProviderUsable(provider) {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "workspace provider is not usable")
	}
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "runtime configuration is not admitted")
	}
	if w.Spec.Workload != nil && (w.Status.Allocation == nil || w.Status.Allocation.State != workspacev1alpha1.AllocationStopped) {
		if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil || w.Spec.Workload.Runtime == nil {
			return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "persisted workload failed its immutable binding")
		}
		frozenPool, frozen, err := runtimePoolPodTemplateValidationTarget(pool, w.Spec.Workload.Runtime.Template)
		if err != nil {
			return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "persisted runtime fence is invalid")
		}
		if frozen.controllerEpoch != cfg.controllerEpoch || frozen.providerProxy.tokenGeneration != cfg.providerProxy.tokenGeneration || frozenPool.Generation != pool.Generation || frozenPool.Spec.Runtime.Profile.Digest != pool.Spec.Runtime.Profile.Digest {
			before := pool.DeepCopy()
			if pool.Annotations == nil {
				pool.Annotations = map[string]string{}
			}
			pool.Annotations["orka.ai/external-runtime-retirement-requested"] = "true"
			if err := r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
			return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "retiring the old runtime fence before credential rotation")
		}
	}
	if err := r.ensureRuntimePoolNamespace(ctx, cfg); err != nil {
		return ctrl.Result{}, err
	}
	auth, providerSecret, err := r.ensureRuntimePoolSecrets(ctx, pool, cfg)
	if err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "private runtime credentials are unavailable")
	}
	if err := r.ensureRuntimePoolAncillaryResources(ctx, pool, cfg); err != nil {
		return ctrl.Result{}, err
	}
	if w.Spec.Workload == nil || (w.Status.Allocation != nil && w.Status.Allocation.State == workspacev1alpha1.AllocationStopped) {
		if w.Spec.Workload != nil {
			a := w.Status.Allocation
			if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil || a.Key != w.Spec.Workload.Key || a.Sequence != w.Spec.Workload.Sequence || a.Identity.RequestRevision != w.Spec.Workload.Revision || !a.Identity.Valid() || a.Startup != nil || w.Status.ObservedGeneration != w.Generation {
				return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "stopped allocation does not prove the current runtime terminated")
			}
			if terminated, err := r.externalObservedInstanceTerminated(ctx, pool, w); err != nil || !terminated {
				return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "waiting for independent observation of the retired Pod's absence")
			}
			rotating, err := r.rotateConsumedWorkspaceRuntimePoolAuthSecret(ctx, pool, cfg, auth)
			if err != nil {
				return ctrl.Result{}, err
			}
			if rotating {
				return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "rotating private credentials before cold boot")
			}
		}
		return r.publishExternalWorkspaceWorkload(ctx, pool, cfg, w, auth, providerSecret)
	}
	if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "persisted workload failed its immutable binding")
	}
	request := w.Spec.Workload
	if request.Runtime == nil || request.Runtime.PoolBinding.UID != pool.UID || request.Runtime.PoolBinding.Name != pool.Name {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "workload refers to a different RuntimePool")
	}
	if w.Spec.Retirement != nil {
		return r.reconcileExternalWorkspaceRetirement(ctx, pool, w, false)
	}
	if slices.Contains(request.Runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) && !externalNativeRuntimeHasOpaqueIdentity(request) {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "native workload requires retirement before opaque runtime identity admission")
	}
	if err := workspacev1alpha1.ValidateStartup(*request, allocationOrEmpty(w)); err != nil || w.Status.ObservedGeneration != w.Generation {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "waiting for current exact-instance startup evidence")
	}
	pod, err := r.attestExternalWorkspaceStartup(ctx, request, w.Status.Allocation.Startup)
	if err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "external workload failed independent materialization verification")
	}
	if w.Status.Allocation.Startup.Process != nil {
		if err := r.ensureExternalProcessIngress(ctx, pool, cfg, w); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.bindExternalRuntimeInstanceEvidence(ctx, pool, w); err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "startup conflicts with the independently observed instance fence")
	}
	if err := r.bindWorkspaceRuntimePoolBootstrapInstance(ctx, pool, auth, pod.UID); err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "runtime instance conflicts with the private credential binding")
	}
	nonce := string(auth.Data[runtimePoolBootstrapNonceKey])
	if w.Status.Allocation.Startup.Process != nil {
		if _, err := r.seedExternalNativeCredentials(ctx, w, auth, providerSecret); err != nil {
			return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "waiting for process-bound credential bootstrap")
		}
	} else if _, err := r.seedWorkspaceSupervisorCredentials(ctx, w.Status.Allocation.Startup.Endpoint, nonce, auth, providerSecret); err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "waiting for exact-instance credential bootstrap")
	}
	status := r.baseRuntimePoolStatus(pool, 1)
	return r.reconcileRuntimePoolServingWithPostProbeFence(ctx, pool, cfg, []corev1.Pod{*pod}, []corev1.Pod{*pod}, auth, status,
		func(ctx context.Context, _ *corev1alpha1.RuntimePoolActiveInstanceStatus) (ctrl.Result, bool, error) {
			current, err := r.externalPoolWorkspace(ctx, pool)
			if err != nil || current.Spec.Workload == nil || current.Spec.Workload.Revision != request.Revision || !reflect.DeepEqual(current.Status.Allocation, w.Status.Allocation) || current.Status.ObservedGeneration != current.Generation || !workspaceCurrentlyAdmittedByCore(current) {
				result, err := r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "provider evidence changed during authenticated runtime verification")
				return result, true, err
			}
			if _, err := r.attestExternalWorkspaceStartup(ctx, request, current.Status.Allocation.Startup); err != nil {
				result, err := r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, "runtime materialization changed during authenticated verification")
				return result, true, err
			}
			return ctrl.Result{}, false, nil
		})
}

func allocationOrEmpty(w *workspacev1alpha1.ExecutionWorkspace) workspacev1alpha1.AllocationObservation {
	if w.Status.Allocation == nil {
		return workspacev1alpha1.AllocationObservation{}
	}
	return *w.Status.Allocation
}

func (r *RuntimePoolReconciler) externalPoolProgress(ctx context.Context, pool *corev1alpha1.RuntimePool, lifecycle corev1alpha1.RuntimePoolLifecycle, message string) (ctrl.Result, error) {
	status := r.baseRuntimePoolStatus(pool, pool.Status.CurrentReplicas)
	status.ActiveInstance = pool.Status.ActiveInstance
	status.Lifecycle, status.AdmissionState, status.Message = lifecycle, corev1alpha1.RuntimePoolAdmissionClosed, message
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, message)
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

func (r *RuntimePoolReconciler) publishExternalWorkspaceWorkload(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, w *workspacev1alpha1.ExecutionWorkspace, auth, provider *corev1.Secret) (ctrl.Result, error) {
	registration := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Name: w.Spec.ProviderBinding.Name}, registration); err != nil {
		return ctrl.Result{}, err
	}
	if registration.UID != w.Spec.ProviderBinding.UID || registration.Spec.ControllerName != w.Labels[workspacev1alpha1.ProviderControllerLabel] ||
		registration.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDisabled || !externalProviderUsable(registration) {
		return ctrl.Result{}, fmt.Errorf("workspace provider no longer matches the admitted identity and capabilities")
	}
	required, err := externalWorkspaceRuntimeRequiredFeatures(pool, w)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !featureSetContainsAll(registration.Status.SupportedFeatures, required) {
		return ctrl.Result{}, fmt.Errorf("workspace provider does not support every frozen runtime requirement")
	}
	nativeProcess := slices.Contains(required, workspacev1alpha1.WorkspaceFeatureNativeProcess)
	publicKey, err := harnessv2.CredentialBootstrapPublicKey(auth.Data[runtimePoolBootstrapSigningSeedKey])
	if err != nil {
		return ctrl.Result{}, err
	}
	template := r.runtimePoolPodTemplate(pool, cfg, cfg.labels, auth.Name, provider.Name)
	template.Spec.Containers[0].Resources = externalRuntimePoolResourceRequirements(pool.Spec.Runtime.Profile.ResourceClass)
	// Scratch volume size caps are Kubernetes quotas, not a native storage
	// contract. Select unbounded scratch defaults before publishing admission.
	for i := range template.Spec.Volumes {
		if scratch := template.Spec.Volumes[i].EmptyDir; scratch != nil {
			scratch.SizeLimit = nil
		}
	}
	template.Spec.Containers[0].Command = []string{"/usr/local/bin/orka-acp-runtime"}
	template.Spec.RestartPolicy = corev1.RestartPolicyNever
	template.Namespace = cfg.namespace
	bootstrapPort := runtimePoolPort
	if nativeProcess {
		template = runtimePoolNativeProcessTemplate(template)
		bootstrapPort = runtimePoolNativeProcessPort
	}
	if slices.Contains(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend) {
		template = runtimePoolDurableWorkspaceTemplate(template)
		template.Spec.Containers[0].Env = append(template.Spec.Containers[0].Env, corev1.EnvVar{Name: "ORKA_ACP_DURABLE_WORKSPACE_KEY", Value: "shared"})
	}
	template = runtimePoolWorkspaceBootstrapTemplate(template, string(auth.Data[runtimePoolBootstrapNonceKey]), publicKey)
	container := template.Spec.Containers[0]
	request := workspacev1alpha1.WorkloadRequest{
		RestoreFrom: pool.Spec.ExecutionWorkspace.RestoreFrom.DeepCopy(),
		Sequence:    1, Key: workspacev1alpha1.AllocationKey{Namespace: w.Namespace, Name: w.Name, WorkspaceUID: w.UID, ProviderUID: w.Spec.ProviderBinding.UID},
		Image: container.Image, Command: container.Command, Args: container.Args, Resources: container.Resources,
		ParametersRef: pool.Spec.ExecutionWorkspace.ParametersRef, ParametersBinding: pool.Spec.ExecutionWorkspace.ParametersBinding,
		Runtime: &workspacev1alpha1.RuntimeWorkload{BootstrapPort: bootstrapPort, PoolBinding: workspacev1alpha1.ImmutableObjectBinding{Name: pool.Name, UID: pool.UID, Generation: pool.Generation, ProfileHash: pool.Spec.Runtime.Profile.Digest}, ClassBinding: w.Spec.ClassBinding, Protocol: harnessv2.ProtocolVersion, ContainerName: container.Name, Template: template},
	}
	network := &networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}}
	if nativeProcess {
		// Native infrastructure/router ingress is operator-managed. Freeze only
		// runtime egress here; Pod ingress rules do not describe that topology.
		network.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
	}
	// Only Core's generated permissions are admission intent. Public labels
	// on observed policies cannot authorize additional connectivity.
	policies := r.runtimePoolNetworkPolicies(cfg)
	slices.SortFunc(policies, func(a, b networkingv1.NetworkPolicy) int { return strings.Compare(a.Name, b.Name) })
	for _, p := range policies {
		if !nativeProcess {
			network.Ingress = append(network.Ingress, p.Spec.Ingress...)
		}
		network.Egress = append(network.Egress, p.Spec.Egress...)
	}
	request.Runtime.NetworkPolicy = network
	request.Runtime.RequiredFeatures = required
	if w.Spec.Workload != nil {
		request.Sequence = w.Spec.Workload.Sequence + 1
		request.PreviousInstance = &w.Status.Allocation.Identity
		request.RetainedData = w.Status.Allocation.RetainedData
	}
	if nativeProcess {
		for i := range request.Runtime.Template.Spec.Containers[0].Env {
			variable := &request.Runtime.Template.Spec.Containers[0].Env[i]
			if variable.Name == externalNativeRuntimeUIDEnvName {
				*variable = corev1.EnvVar{Name: variable.Name, Value: string(externalNativeRuntimeUID(&request))}
			}
		}
	}
	request.Revision, err = workspacev1alpha1.WorkloadRevision(request)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := workspaceprovider.ValidateWorkloadTransition(w.Spec.Workload, request, w.Status.Allocation, slices.Contains(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)); err != nil {
		return ctrl.Result{}, err
	}
	before := w.DeepCopy()
	w.Spec.Workload, w.Spec.Retirement = &request, nil
	if err := r.Patch(ctx, w, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, err
	}
	return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "public workload request published; waiting for provider materialization")
}

func externalWorkspaceRuntimeRequiredFeatures(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) ([]workspacev1alpha1.ExecutionWorkspaceFeature, error) {
	required := slices.Clone(pool.Spec.ExecutionWorkspace.Workload.RequiredFeatures)
	if w.Spec.Workload != nil {
		if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil || w.Spec.Workload.Runtime == nil {
			return nil, fmt.Errorf("published workload cannot establish frozen materialization intent")
		}
		if len(required) == 0 {
			// Older pools did not freeze this field. Their admitted predecessor
			// proves the materialization kind across restarts and cold resume.
			required = slices.Clone(w.Spec.Workload.Runtime.RequiredFeatures)
		} else if slices.Contains(required, workspacev1alpha1.WorkspaceFeatureNativeProcess) != slices.Contains(w.Spec.Workload.Runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) {
			return nil, fmt.Errorf("pool requirements conflict with the published materialization kind")
		}
	} else if len(required) == 0 {
		return nil, fmt.Errorf("workspace pool has no frozen materialization intent")
	}
	add := func(feature workspacev1alpha1.ExecutionWorkspaceFeature) {
		if !slices.Contains(required, feature) {
			required = append(required, feature)
		}
	}
	add(workspacev1alpha1.WorkspaceFeatureACPRuntime)
	if pool.Spec.ExecutionWorkspace.RestoreFrom != nil {
		add(workspacev1alpha1.WorkspaceFeatureRestore)
		add(workspacev1alpha1.WorkspaceFeatureCheckpoint)
	}
	if slices.Contains(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend) {
		add(workspacev1alpha1.WorkspaceFeatureSuspend)
	}
	return required, nil
}

// externalNativeRuntimeUID is an opaque Core fence for one immutable workload
// sequence. Provider identifiers remain in the separate allocation evidence.
func externalNativeRuntimeUID(request *workspacev1alpha1.WorkloadRequest) types.UID {
	if request == nil || request.Runtime == nil || request.Runtime.PoolBinding.UID == "" || request.Key.WorkspaceUID == "" || request.Sequence < 1 {
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("orka.ai/native-runtime-identity/v1\x00%s\x00%s\x00%d", request.Runtime.PoolBinding.UID, request.Key.WorkspaceUID, request.Sequence)))
	return types.UID("workspace:" + hex.EncodeToString(digest[:]))
}

func externalNativeRuntimeIdentity(request *workspacev1alpha1.WorkloadRequest, legacyInstanceID string) (types.UID, bool, error) {
	opaque := externalNativeRuntimeUID(request)
	if opaque == "" {
		return "", false, workspaceprovider.ErrStaleIdentity
	}
	var identity *corev1.EnvVar
	for _, container := range request.Runtime.Template.Spec.Containers {
		if container.Name != request.Runtime.ContainerName {
			continue
		}
		for i := range container.Env {
			if container.Env[i].Name == externalNativeRuntimeUIDEnvName {
				if identity != nil {
					return "", false, workspaceprovider.ErrStaleIdentity
				}
				identity = &container.Env[i]
			}
		}
	}
	if identity == nil {
		return "", false, workspaceprovider.ErrStaleIdentity
	}
	if identity.ValueFrom == nil && identity.Value == string(opaque) {
		return opaque, true, nil
	}
	// Pre-fix requests are immutable. Their admitted downward identity remains
	// usable for exact retirement, while startup and Task dispatch deny it.
	if identity.Value == "" && identity.ValueFrom != nil && identity.ValueFrom.FieldRef != nil && legacyInstanceID != "" {
		field := identity.ValueFrom.FieldRef
		if (field.APIVersion == "" || field.APIVersion == "v1") && field.FieldPath == "metadata.uid" && reflect.DeepEqual(identity.ValueFrom, &corev1.EnvVarSource{FieldRef: field}) {
			return types.UID(legacyInstanceID), false, nil
		}
	}
	return "", false, workspaceprovider.ErrStaleIdentity
}

func externalNativeRuntimeHasOpaqueIdentity(request *workspacev1alpha1.WorkloadRequest) bool {
	_, opaque, err := externalNativeRuntimeIdentity(request, "")
	return err == nil && opaque
}

//nolint:gocyclo // Check every accessible Pod and storage identity before credential delivery.
func (r *RuntimePoolReconciler) attestExternalWorkspaceStartup(ctx context.Context, request *workspacev1alpha1.WorkloadRequest, evidence *workspacev1alpha1.StartupEvidence) (*corev1.Pod, error) {
	if request == nil || request.Runtime == nil || evidence == nil {
		return nil, fmt.Errorf("runtime startup evidence is incomplete")
	}
	nativeProcess := slices.Contains(request.Runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess)
	if nativeProcess != (evidence.Process != nil) || (nativeProcess && evidence.Pod != nil) {
		return nil, fmt.Errorf("startup evidence does not match the frozen runtime materialization kind")
	}
	reader := uncachedReader(r.APIReader, r.Client)
	if evidence.Process != nil {
		process := evidence.Process
		worker := &corev1.Pod{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: process.Worker.Namespace, Name: process.Worker.Name}, worker); err != nil {
			return nil, err
		}
		if worker.UID != process.Worker.UID || !worker.DeletionTimestamp.IsZero() {
			return nil, workspaceprovider.ErrStaleIdentity
		}
		runtimeUID, _, err := externalNativeRuntimeIdentity(request, evidence.Identity.InstanceID)
		if err != nil {
			return nil, err
		}
		endpoint, err := url.Parse(evidence.Endpoint)
		if err != nil {
			return nil, err
		}
		pod := &corev1.Pod{ObjectMeta: *request.Runtime.Template.ObjectMeta.DeepCopy(), Spec: *request.Runtime.Template.Spec.DeepCopy()}
		pod.Namespace, pod.Name, pod.UID = request.Runtime.Template.Namespace, process.Name, runtimeUID
		pod.Status.PodIP = endpoint.Hostname()
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[externalRuntimeEndpointAnnotation] = evidence.Endpoint
		return pod, nil
	}
	if evidence.Pod == nil {
		return nil, fmt.Errorf("startup has no verifiable process identity")
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: evidence.Pod.Namespace, Name: evidence.Pod.Name}, pod); err != nil {
		return nil, err
	}
	if pod.UID != evidence.Pod.UID || !pod.DeletionTimestamp.IsZero() || pod.Namespace != request.Runtime.Template.Namespace {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	for k, v := range request.Runtime.Template.Labels {
		if pod.Labels[k] != v {
			return nil, fmt.Errorf("runtime Pod labels differ from admission")
		}
	}
	for k, v := range request.Runtime.Template.Annotations {
		if pod.Annotations[k] != v {
			return nil, fmt.Errorf("runtime Pod annotations differ from admission")
		}
	}
	claimName := ""
	for _, volume := range evidence.PersistentVolumes {
		// A Pod resolves claimName in its own namespace, regardless of the
		// namespace supplied by provider evidence for a same-name claim.
		if volume.Claim.Namespace != pod.Namespace {
			return nil, fmt.Errorf("durable storage claim is outside the observed runtime Pod namespace")
		}
		claim := &corev1.PersistentVolumeClaim{}
		pv := &corev1.PersistentVolume{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: volume.Claim.Namespace, Name: volume.Claim.Name}, claim); err != nil {
			return nil, err
		}
		if err := reader.Get(ctx, types.NamespacedName{Name: volume.Volume.Name}, pv); err != nil {
			return nil, err
		}
		if claim.UID != volume.Claim.UID || pv.UID != volume.Volume.UID || claim.Spec.VolumeName != pv.Name || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != claim.UID || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete || !claim.DeletionTimestamp.IsZero() || claim.Spec.DataSource != nil || claim.Spec.DataSourceRef != nil {
			return nil, fmt.Errorf("durable storage identity does not match admission")
		}
		found := false
		for _, realized := range pod.Spec.Volumes {
			if realized.Name == volume.VolumeName && realized.PersistentVolumeClaim != nil && realized.PersistentVolumeClaim.ClaimName == claim.Name && !realized.PersistentVolumeClaim.ReadOnly {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("runtime does not mount the verified durable storage")
		}
		if volume.VolumeName == runtimePoolDurableWorkspaceVolume {
			claimName = claim.Name
		}
	}
	if !runtimePoolWorkspacePodSpecsMatch(request.Runtime.Template.Spec, pod.Spec, claimName) {
		return nil, fmt.Errorf("runtime Pod spec differs from the frozen admitted workload")
	}
	endpoint := (&url.URL{Scheme: urlSchemeHTTP, Host: net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(request.Runtime.BootstrapPort)))}).String()
	if evidence.Endpoint != endpoint || pod.Status.PodIP == "" {
		return nil, fmt.Errorf("endpoint does not identify the exact observed Pod")
	}
	if err := r.verifyExternalRuntimeNetworkPolicies(ctx, request.Runtime, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

// Kubernetes combines permissions from every matching policy. Provider
// bookkeeping labels must not select rules outside the admitted network.
func (r *RuntimePoolReconciler) verifyExternalRuntimeNetworkPolicies(ctx context.Context, runtime *workspacev1alpha1.RuntimeWorkload, pod *corev1.Pod) error {
	if runtime.NetworkPolicy == nil {
		return fmt.Errorf("runtime has no admitted network policy")
	}
	policies := &networkingv1.NetworkPolicyList{}
	if err := uncachedReader(r.APIReader, r.Client).List(ctx, policies, client.InNamespace(pod.Namespace)); err != nil {
		return err
	}
	ingressIsolated, egressIsolated := false, false
	for _, policy := range policies.Items {
		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			return err
		}
		if !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		ingressIsolated = ingressIsolated || slices.Contains(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) || len(policy.Spec.PolicyTypes) == 0
		egressIsolated = egressIsolated || slices.Contains(policy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) || (len(policy.Spec.PolicyTypes) == 0 && len(policy.Spec.Egress) > 0)
		for _, rule := range policy.Spec.Ingress {
			if !slices.ContainsFunc(runtime.NetworkPolicy.Ingress, func(admitted networkingv1.NetworkPolicyIngressRule) bool { return reflect.DeepEqual(rule, admitted) }) {
				return fmt.Errorf("runtime labels select unadmitted ingress permissions")
			}
		}
		for _, rule := range policy.Spec.Egress {
			if !slices.ContainsFunc(runtime.NetworkPolicy.Egress, func(admitted networkingv1.NetworkPolicyEgressRule) bool { return reflect.DeepEqual(rule, admitted) }) {
				return fmt.Errorf("runtime labels select unadmitted egress permissions")
			}
		}
	}
	if !ingressIsolated || !egressIsolated {
		return fmt.Errorf("runtime is not isolated in both network directions")
	}
	return nil
}

// Native ingress binds an exact infrastructure worker before any credential
// delivery. Its endpoint namespaces come from immutable Core network intent.
func (r *RuntimePoolReconciler) ensureExternalProcessIngress(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, w *workspacev1alpha1.ExecutionWorkspace) error {
	process := w.Status.Allocation.Startup.Process
	worker, err := r.externalUniqueIngressWorker(ctx, process, w.Status.Allocation.Identity)
	if err != nil {
		return err
	}
	targets, err := r.freezeExternalIngressTargets(ctx, pool, w.Spec.Workload.Runtime)
	if err != nil {
		return err
	}
	binding, err := externalRuntimeEvidence(pool)
	if err != nil {
		return err
	}
	if binding != nil && binding.Sequence != w.Spec.Workload.Sequence {
		if !binding.NativeProcess || binding.WorkspaceUID != w.UID || binding.Sequence+1 != w.Spec.Workload.Sequence || w.Spec.Workload.PreviousInstance == nil || *w.Spec.Workload.PreviousInstance != binding.Identity {
			return workspaceprovider.ErrStaleIdentity
		}
		pending, err := r.deleteExternalIngressPolicies(ctx, pool, cfg, targets, binding)
		if err != nil {
			return err
		}
		if pending {
			return fmt.Errorf("waiting for exact predecessor ingress policies to disappear")
		}
	}
	// Save the independent worker/instance fence before policy creation. A
	// lost Create response can then be recovered or safely retired even if
	// the provider later clears startup evidence.
	if err := r.bindExternalRuntimeInstanceEvidence(ctx, pool, w); err != nil {
		return err
	}
	for _, target := range targets {
		if err := r.ensureExternalIngressPolicy(ctx, pool, cfg, target, worker); err != nil {
			return err
		}
	}
	_, err = r.externalUniqueIngressWorker(ctx, process, w.Status.Allocation.Identity)
	return err
}

func (r *RuntimePoolReconciler) seedExternalNativeCredentials(ctx context.Context, w *workspacev1alpha1.ExecutionWorkspace, auth, provider *corev1.Secret) (bool, error) {
	evidence := w.Status.Allocation.Startup
	p := evidence.Process
	request := harnessv2.CredentialBootstrapRequest{ControllerToken: string(auth.Data[runtimePoolControllerTokenKey]), CapabilitySecret: string(auth.Data[runtimePoolCapabilitySecretKey]), ProviderToken: string(provider.Data[runtimePoolProviderTokenKey])}
	if err := request.Validate(); err != nil {
		return false, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	transport := r.HTTPClient
	if transport == nil {
		transport = &http.Client{Timeout: runtimePoolProbeTimeout, Transport: harnessv2.NewProxylessTransport()}
	}
	return seedSealedWorkspaceCredentials(ctx, transport, strings.TrimRight(evidence.Endpoint, "/")+harnessv2.CredentialBootstrapPath, string(auth.Data[runtimePoolBootstrapNonceKey]), auth.Data[runtimePoolBootstrapSigningSeedKey], payload,
		harnessv2.SubstrateActorIdentity{Atespace: p.Namespace, Name: p.Name, UID: p.UID}, func(challenge harnessv2.SealedBootstrapChallenge) error {
			encoded, err := json.Marshal(challenge)
			if err != nil {
				return err
			}
			if store.CanonicalBytesDigest(encoded) != p.ChallengeSHA256 {
				return workspaceprovider.ErrStaleIdentity
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := uncachedReader(r.APIReader, r.Client).Get(ctx, client.ObjectKeyFromObject(w), current); err != nil {
				return err
			}
			if current.UID != w.UID || current.Spec.Workload == nil || current.Spec.Workload.Revision != w.Spec.Workload.Revision || !reflect.DeepEqual(current.Status.Allocation, w.Status.Allocation) {
				return workspaceprovider.ErrStaleIdentity
			}
			_, err = r.attestExternalWorkspaceStartup(ctx, current.Spec.Workload, current.Status.Allocation.Startup)
			return err
		})
}

//nolint:gocyclo // Retirement authority follows independent identity, drain, termination and disposition gates.
func (r *RuntimePoolReconciler) reconcileExternalWorkspaceRetirement(ctx context.Context, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace, deleting bool) (ctrl.Result, error) {
	if pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "closing admission before exact-instance retirement")
	}
	a := w.Status.Allocation
	if deleting && w.Spec.Workload != nil && a == nil && externalNeverBootstrappedDeletionProven(pool, w) {
		return r.finalizeExternalWorkspacePool(ctx, pool)
	}
	if w.Spec.Workload == nil {
		if deleting && w.Status.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted && w.Status.ObservedGeneration == w.Generation {
			return r.finalizeExternalWorkspacePool(ctx, pool)
		}
		if !deleting {
			return r.externalPoolStopped(ctx, pool)
		}
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "waiting for provider proof that no allocation exists")
	}
	if a == nil || a.Sequence != w.Spec.Workload.Sequence || a.Key != w.Spec.Workload.Key || a.Identity.RequestRevision != w.Spec.Workload.Revision || !a.Identity.Valid() {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "waiting for current exact-instance retirement identity")
	}
	if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "retirement workload failed its immutable binding")
	}
	if compatible, err := r.externalRetirementEvidenceCompatible(ctx, pool, w); err != nil || !compatible {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "provider retirement observation changed the independently bound instance")
	}
	if deleting && a.State == workspacev1alpha1.AllocationDeleted && a.Disposition != nil && w.Status.ObservedGeneration == w.Generation {
		if err := workspaceprovider.ValidateDeletedDisposition(a.Disposition, w.Spec.Lifecycle.DeletionPolicy); err == nil {
			return r.finalizeExternalWorkspacePool(ctx, pool)
		}
	}
	if !deleting && a.State == workspacev1alpha1.AllocationStopped && a.Startup == nil && w.Status.ObservedGeneration == w.Generation {
		if terminated, err := r.externalObservedInstanceTerminated(ctx, pool, w); err != nil || !terminated {
			return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "waiting for independent observation of the retired Pod's absence")
		}
		return r.externalPoolStopped(ctx, pool)
	}
	if w.Spec.Retirement == nil {
		if !runtimePoolControllerWorkIsQuiescent(pool.Status.Capacity) {
			return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "waiting for controller reservations and settlement before retirement")
		}
		if pool.Status.ActiveInstance != nil {
			terminated, err := r.externalActivePodTerminationProven(ctx, pool, w)
			if err != nil {
				return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "independent runtime termination observation is unavailable")
			}
			if !terminated {
				if a.Startup == nil {
					return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "provider is unavailable; exact runtime drain remains unverified")
				}
				pod, err := r.attestExternalWorkspaceStartup(ctx, w.Spec.Workload, a.Startup)
				if err != nil {
					return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "exact runtime materialization is unavailable for drain")
				}
				validationPool, cfg, err := runtimePoolPodTemplateValidationTarget(pool, w.Spec.Workload.Runtime.Template)
				if err != nil {
					return ctrl.Result{}, err
				}
				auth, err := r.runtimePoolPodTemplateAuthSecret(ctx, pool, pod.Namespace, w.Spec.Workload.Runtime.Template.Spec)
				if err != nil {
					return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "exact runtime credentials are unavailable for drain")
				}
				probe, err := r.supervisorClientForPool(pool).Probe(ctx, a.Startup.Endpoint, string(auth.Data[runtimePoolControllerTokenKey]), auth.Data[runtimePoolCapabilitySecretKey])
				if err != nil {
					return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "authenticated runtime drain is unavailable")
				}
				active, err := validateRuntimePoolProbe(validationPool, cfg, pod, probe, r.now())
				if err != nil || !runtimePoolRolloutActiveInstanceMatches(pool.Status.ActiveInstance, active) {
					return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "runtime drain fence changed")
				}
				if !probe.Status.Drain.Requested {
					if err := r.supervisorClientForPool(pool).RequestDrain(ctx, a.Startup.Endpoint, string(auth.Data[runtimePoolControllerTokenKey]), auth.Data[runtimePoolCapabilitySecretKey], probe.Status, "workspace_retirement"); err != nil {
						return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "authenticated runtime drain request is unavailable")
					}
					return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "waiting for authenticated drain settlement")
				}
				if !runtimePoolProbeIsQuiescent(pool.Status.Capacity, probe.Status) {
					return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, "waiting for runtime drain settlement")
				}
				if err := r.recordDrainedRuntimePoolTaskCleanup(ctx, validationPool, active, probe.Status); err != nil {
					return ctrl.Result{}, err
				}

			}
		}
	}
	action := workspacev1alpha1.WorkloadRetirementStop
	if deleting {
		action = workspacev1alpha1.WorkloadRetirementDelete
	} else if w.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended || slices.Contains(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend) {
		action = workspacev1alpha1.WorkloadRetirementSuspend
	}
	retirement := &workspacev1alpha1.WorkloadRetirement{Sequence: w.Spec.Workload.Sequence, Identity: a.Identity, Action: action}
	if !reflect.DeepEqual(w.Spec.Retirement, retirement) {
		before := w.DeepCopy()
		w.Spec.Retirement = retirement
		if deleting {
			w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
			w.Spec.Attachment = nil
		}
		if err := r.Patch(ctx, w, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "waiting for identity-bound provider termination and disposition")
}

func (r *RuntimePoolReconciler) externalPoolStopped(ctx context.Context, pool *corev1alpha1.RuntimePool) (ctrl.Result, error) {
	if pool.Annotations["orka.ai/external-runtime-retirement-requested"] != "" {
		before := pool.DeepCopy()
		delete(pool.Annotations, "orka.ai/external-runtime-retirement-requested")
		if err := r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	status := r.baseRuntimePoolStatus(pool, 0)
	status.ActiveInstance = nil
	status.Lifecycle, status.AdmissionState, status.Message = corev1alpha1.RuntimePoolLifecycleStopped, corev1alpha1.RuntimePoolAdmissionClosed, runtimePoolMessageStopped
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
	return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
}

func (r *RuntimePoolReconciler) finalizeExternalWorkspacePool(ctx context.Context, pool *corev1alpha1.RuntimePool) (ctrl.Result, error) {
	cfg, err := r.runtimePoolConfigForDeletion(pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	remaining, err := r.deleteExternalRuntimePoolCoreResources(ctx, pool, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	// Runtime credentials are core-owned and are deleted only after provider
	// termination is proved. Attachment credentials are revoked by workspace
	// finalization, independently of the provider's disposition.
	if pool.DeletionTimestamp.IsZero() {
		return r.externalPoolStopped(ctx, pool)
	}
	before := pool.DeepCopy()
	controllerutil.RemoveFinalizer(pool, runtimePoolFinalizer)
	if err := r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *RuntimePoolReconciler) deleteExternalRuntimePoolCoreResources(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) (bool, error) {
	// Core observes termination; it never deletes provider compute to make a
	// provider's deletion assertion become true.
	w, err := r.externalPoolWorkspace(ctx, pool)
	if err != nil {
		return false, err
	}
	if terminated, err := r.externalObservedInstanceTerminated(ctx, pool, w); err != nil || !terminated {
		return true, err
	}
	if w.Spec.Workload != nil {
		if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil || w.Spec.Workload.Runtime == nil || w.Spec.Workload.Runtime.PoolBinding.UID != pool.UID {
			return false, workspaceprovider.ErrStaleIdentity
		}
		cfg.namespace = w.Spec.Workload.Runtime.Template.Namespace
	}
	remaining, err := r.deleteExternalPrivateCredentials(ctx, pool, cfg)
	if err != nil {
		return false, err
	}
	discoveryRemaining, err := r.deleteExternalCoreDiscoveryResources(ctx, pool, cfg)
	if err != nil {
		return false, err
	}
	policiesRemaining, err := r.deleteExternalCoreNetworkPolicies(ctx, pool, cfg, w)
	return remaining || discoveryRemaining || policiesRemaining, err
}

func (r *RuntimePoolReconciler) externalObservedInstanceTerminated(ctx context.Context, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) (bool, error) {
	if w.Spec.Workload == nil || w.Status.Allocation == nil {
		return true, nil
	}
	binding, err := externalRuntimeEvidence(pool)
	if err != nil {
		return false, err
	}
	namespace, uid := w.Spec.Workload.Runtime.Template.Namespace, types.UID(w.Status.Allocation.Identity.InstanceID)
	if binding != nil {
		compatible, err := r.externalRetirementEvidenceCompatible(ctx, pool, w)
		if err != nil || !compatible {
			return false, workspaceprovider.ErrStaleIdentity
		}
		if binding.NativeProcess || binding.Sequence != w.Status.Allocation.Sequence {
			// The worker is infrastructure, not the native process. For an
			// unbootstrapped replacement the saved evidence is its terminated
			// predecessor. Neither may be mistaken for the current runtime Pod.
			// Only the exact core-authorized current termination claim can
			// release credentials in these cases.
			a, retirement := w.Status.Allocation, w.Spec.Retirement
			if retirement == nil || retirement.Sequence != a.Sequence || retirement.Identity != a.Identity ||
				w.Status.ObservedGeneration != w.Generation || a.Startup != nil ||
				(a.State != workspacev1alpha1.AllocationStopped && a.State != workspacev1alpha1.AllocationDeleted) {
				return false, nil
			}
			if binding.NativeProcess {
				return true, nil
			}
		} else {
			namespace, uid = binding.Pod.Namespace, binding.Pod.UID
		}
	}
	return r.externalPodUIDAbsent(ctx, namespace, uid)
}

// A replacement may be cancelled before startup attestation. Its predecessor's
// binding remains until credential delivery; it is not a conflicting current
// binding when the immutable request names that exact retired predecessor.
func (r *RuntimePoolReconciler) externalRetirementEvidenceCompatible(ctx context.Context, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) (bool, error) {
	binding, err := externalRuntimeEvidence(pool)
	if err != nil {
		return false, err
	}
	if binding == nil {
		return true, nil
	}
	a := w.Status.Allocation
	if a == nil || binding.WorkspaceUID != w.UID {
		return false, nil
	}
	if binding.Sequence == a.Sequence {
		return binding.Identity == a.Identity, nil
	}
	request := w.Spec.Workload
	if request == nil || binding.Sequence+1 != a.Sequence || request.PreviousInstance == nil || *request.PreviousInstance != binding.Identity {
		return false, nil
	}
	if binding.NativeProcess {
		// Core published this predecessor reference only after observing its
		// authorized termination. A worker may survive that process lifetime.
		return true, nil
	}
	return r.externalPodUIDAbsent(ctx, binding.Pod.Namespace, binding.Pod.UID)
}

func (r *RuntimePoolReconciler) externalPodUIDAbsent(ctx context.Context, namespace string, uid types.UID) (bool, error) {
	pods := &corev1.PodList{}
	if err := uncachedReader(r.APIReader, r.Client).List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		if pod.UID == uid {
			return false, nil
		}
	}
	return true, nil
}

func externalRuntimeEvidence(pool *corev1alpha1.RuntimePool) (*externalRuntimeInstanceEvidence, error) {
	value := pool.Annotations[externalRuntimeEvidenceAnnotation]
	if value == "" {
		return nil, nil
	}
	var binding externalRuntimeInstanceEvidence
	if err := json.Unmarshal([]byte(value), &binding); err != nil {
		return nil, err
	}
	if binding.WorkspaceUID == "" || binding.Sequence < 1 || !binding.Identity.Valid() || binding.Pod.Namespace == "" || binding.Pod.Name == "" || binding.Pod.UID == "" {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	if binding.NativeProcess && binding.RuntimeUID == "" {
		// Old evidence can retain its exact already-admitted fence for cleanup.
		binding.RuntimeUID = types.UID(binding.Identity.InstanceID)
	}
	return &binding, nil
}

func (r *RuntimePoolReconciler) bindExternalRuntimeInstanceEvidence(ctx context.Context, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) error {
	a := w.Status.Allocation
	binding := externalRuntimeInstanceEvidence{WorkspaceUID: w.UID, Sequence: a.Sequence, Identity: a.Identity, Endpoint: a.Startup.Endpoint}
	if a.Startup.Process != nil {
		var err error
		binding.RuntimeUID, _, err = externalNativeRuntimeIdentity(w.Spec.Workload, a.Identity.InstanceID)
		if err != nil {
			return err
		}
		binding.Pod = a.Startup.Process.Worker
		binding.NativeProcess = true
	} else {
		binding.Pod = *a.Startup.Pod
	}
	previous, err := externalRuntimeEvidence(pool)
	if err != nil {
		return err
	}
	if previous != nil {
		if *previous == binding {
			return nil
		}
		if previous.WorkspaceUID != w.UID || previous.Sequence+1 != binding.Sequence || w.Spec.Workload.PreviousInstance == nil || *w.Spec.Workload.PreviousInstance != previous.Identity {
			return workspaceprovider.ErrStaleIdentity
		}
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	before := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[externalRuntimeEvidenceAnnotation] = string(encoded)
	return r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func externalProviderUsable(provider *workspacev1alpha1.ExecutionWorkspaceProvider) bool {
	if provider.Status.ObservedGeneration != provider.Generation || provider.Status.Adapter == nil || provider.Status.Adapter.Version == "" {
		return false
	}
	for _, kind := range []workspacev1alpha1.ExecutionWorkspaceConditionType{workspacev1alpha1.ConditionProviderHeartbeat, workspacev1alpha1.ConditionProviderCompatible} {
		condition := workspaceprovider.FindCondition(provider.Status.Conditions, string(kind))
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != provider.Generation {
			return false
		}
	}
	return slices.Contains(provider.Status.SupportedContracts, workspacev1alpha1.LifecycleContractV1) && slices.Contains(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureACPRuntime)
}

// runtimePoolWorkspaceStartupPod returns only the exact materialization whose
// identity and complete endpoint Core observed before releasing credentials.
func runtimePoolWorkspaceStartupPod(ctx context.Context, reader client.Reader, pool *corev1alpha1.RuntimePool) (*corev1.Pod, error) {
	return runtimePoolWorkspaceBoundStartupPod(ctx, reader, pool, true)
}

// Cleanup retains an already-bound legacy native fence without admitting it
// to a new Task or replacing its immutable workload identity.
func runtimePoolWorkspaceCleanupPod(ctx context.Context, reader client.Reader, pool *corev1alpha1.RuntimePool) (*corev1.Pod, error) {
	return runtimePoolWorkspaceBoundStartupPod(ctx, reader, pool, false)
}

//nolint:gocyclo // Keep endpoint, current admission and independently observed identity fences together.
func runtimePoolWorkspaceBoundStartupPod(ctx context.Context, reader client.Reader, pool *corev1alpha1.RuntimePool, requireOpaqueIdentity bool) (*corev1.Pod, error) {
	r := &RuntimePoolReconciler{Client: nil, APIReader: reader}
	w, err := r.externalPoolWorkspace(ctx, pool)
	if err != nil {
		return nil, err
	}
	evidence, err := externalRuntimeEvidence(pool)
	if err != nil {
		return nil, err
	}
	active := pool.Status.ActiveInstance
	a := w.Status.Allocation
	if evidence == nil || evidence.Endpoint == "" || active == nil || w.Spec.Workload == nil || a == nil || a.Startup == nil ||
		evidence.WorkspaceUID != w.UID || evidence.Sequence != a.Sequence || evidence.Identity != a.Identity || evidence.Endpoint != a.Startup.Endpoint ||
		a.Identity.RequestRevision != w.Spec.Workload.Revision || w.Status.ObservedGeneration != w.Generation {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	if err := workspacev1alpha1.ValidateStartup(*w.Spec.Workload, *a); err != nil {
		return nil, err
	}
	if !workspaceCurrentlyAdmittedByCore(w) || w.Spec.Workload.Runtime == nil || w.Spec.Workload.Runtime.PoolBinding.UID != pool.UID {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	if requireOpaqueIdentity && evidence.NativeProcess && !externalNativeRuntimeHasOpaqueIdentity(w.Spec.Workload) {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	pinned := a.Startup.Pod
	native := a.Startup.Process != nil
	if native {
		pinned = &a.Startup.Process.Worker
	}
	if pinned == nil || *pinned != evidence.Pod || native != evidence.NativeProcess {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	pod, err := r.attestExternalWorkspaceStartup(ctx, w.Spec.Workload, a.Startup)
	if err != nil {
		return nil, err
	}
	if active.PodUID != string(pod.UID) || active.PodName != pod.Name || active.PodNamespace != pod.Namespace ||
		active.PodAddress != pod.Status.PodIP || active.RuntimeInstanceID != runtimePoolRuntimeInstanceID(pod.UID, harnessv2.SupervisorBootID(active.BootID)) || active.ProfileDigest != pool.Spec.Runtime.Profile.Digest {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	return pod, nil
}

func runtimePoolWorkspaceStartupEndpoint(ctx context.Context, reader client.Reader, pool *corev1alpha1.RuntimePool) (string, error) {
	pod, err := runtimePoolWorkspaceStartupPod(ctx, reader, pool)
	if err != nil {
		return "", err
	}
	endpoint := runtimePoolInstanceEndpoint(pool, pod)
	if endpoint == "" {
		return "", workspaceprovider.ErrStaleIdentity
	}
	return endpoint, nil
}

func runtimePoolWorkspaceCleanupEndpoint(ctx context.Context, reader client.Reader, pool *corev1alpha1.RuntimePool) (string, error) {
	pod, err := runtimePoolWorkspaceCleanupPod(ctx, reader, pool)
	if err != nil {
		return "", err
	}
	endpoint := runtimePoolInstanceEndpoint(pool, pod)
	if endpoint == "" {
		return "", workspaceprovider.ErrStaleIdentity
	}
	return endpoint, nil
}

// Loss of an independently observed runtime Pod makes authenticated drain
// impossible. Its exact UID's absence can release retirement intent only after
// controller work is quiescent. Provider assertions and infrastructure worker
// Pods cannot establish this exception for a native process.
func (r *RuntimePoolReconciler) externalActivePodTerminationProven(ctx context.Context, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) (bool, error) {
	evidence, err := externalRuntimeEvidence(pool)
	if err != nil {
		return false, err
	}
	active, allocation := pool.Status.ActiveInstance, w.Status.Allocation
	if evidence == nil || evidence.NativeProcess || active == nil || allocation == nil ||
		evidence.WorkspaceUID != w.UID || evidence.Sequence != allocation.Sequence || evidence.Identity != allocation.Identity ||
		string(evidence.Pod.UID) != active.PodUID || evidence.Pod.Name != active.PodName || evidence.Pod.Namespace != active.PodNamespace {
		return false, nil
	}
	return r.externalPodUIDAbsent(ctx, evidence.Pod.Namespace, evidence.Pod.UID)
}

// A provider may reject a public request before allocating any instance. Its
// terminal no-compute proof releases only core-owned resources, and only if
// core has never attested an instance or begun credential delivery.
func externalNeverBootstrappedDeletionProven(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) bool {
	if pool.Status.ActiveInstance != nil || !runtimePoolControllerWorkIsQuiescent(pool.Status.Capacity) ||
		pool.Annotations[externalRuntimeEvidenceAnnotation] != "" || pool.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] != "" ||
		w.Status.Allocation != nil || w.Status.State != workspacev1alpha1.ExecutionWorkspaceStateDeleted ||
		w.Status.ObservedGeneration != w.Generation || w.Status.ExternalID != "" || w.Status.Disposition == nil ||
		w.Status.Disposition.Compute != workspacev1alpha1.DispositionDeleted || w.Spec.Workload.Sequence != 1 || w.Spec.Workload.PreviousInstance != nil {
		return false
	}
	return workspaceprovider.ValidateWorkspaceWorkload(w) == nil &&
		workspaceprovider.ValidateDeletedDisposition(w.Status.Disposition, w.Spec.Lifecycle.DeletionPolicy) == nil
}
