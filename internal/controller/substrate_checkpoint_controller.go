package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

const substratePublicCheckpointFinalizer = "orka.ai/substrate-checkpoint-reference"

// SubstrateCheckpointReconciler exports only already verified Data artifacts.
// It shares native configuration and journal ownership with RuntimePool.
type SubstrateCheckpointReconciler struct {
	RuntimePools           *RuntimePoolReconciler
	CheckpointAPIInstalled bool
}

// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspacecheckpoints,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspacecheckpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspace.orka.ai,resources=executionworkspacecheckpoints/finalizers,verbs=update

//nolint:gocyclo // Keep ordered durable transitions and their failure boundaries visible together.
func (r *SubstrateCheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pools := r.RuntimePools
	if strings.HasPrefix(req.Name, "catalog/") || strings.HasPrefix(req.Name, "template/") {
		return r.collect(ctx, req)
	}
	if !r.CheckpointAPIInstalled {
		return ctrl.Result{}, nil
	}
	checkpoint := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{}
	if err := pools.nativeSubstrateReader().Get(ctx, req.NamespacedName, checkpoint); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !checkpoint.DeletionTimestamp.IsZero() {
		if checkpoint.Status.Digest != "" {
			if err := pools.releaseSubstrateCheckpointArtifact(ctx, checkpoint.Status.Digest, substratePublicCheckpointOwner(checkpoint)); err != nil {
				return ctrl.Result{}, err
			}
		}
		if controllerutil.RemoveFinalizer(checkpoint, substratePublicCheckpointFinalizer) {
			return ctrl.Result{}, pools.Update(ctx, checkpoint)
		}
		return ctrl.Result{}, nil
	}
	if !pools.SubstrateEnabled {
		return r.phase(ctx, checkpoint, "Pending", "Disabled", "Substrate checkpoint export is disabled")
	}
	if checkpoint.UID == "" {
		return ctrl.Result{}, fmt.Errorf("checkpoint requires an exact Kubernetes UID")
	}
	if controllerutil.AddFinalizer(checkpoint, substratePublicCheckpointFinalizer) {
		return ctrl.Result{RequeueAfter: time.Millisecond}, pools.Update(ctx, checkpoint)
	}
	if checkpoint.Status.Digest == "" {
		ws := &workspacev1alpha1.ExecutionWorkspace{}
		if err := pools.nativeSubstrateReader().Get(ctx, types.NamespacedName{Namespace: checkpoint.Namespace, Name: checkpoint.Spec.WorkspaceRef.Name}, ws); err != nil {
			if apierrors.IsNotFound(err) {
				return r.phase(ctx, checkpoint, "Failed", "SourceMissing", "the pinned source workspace was deleted before checkpoint selection")
			}
			return r.phase(ctx, checkpoint, "Pending", "SourceUnavailable", "source workspace is unavailable")
		}
		if ws.UID != checkpoint.Spec.WorkspaceRef.UID || ws.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceControllerLabelValue {
			return r.phase(ctx, checkpoint, "Failed", "SourceChanged", "source workspace identity or controller does not match")
		}
		pool := &corev1alpha1.RuntimePool{}
		if err := pools.nativeSubstrateReader().Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: ws.Annotations[acpExecutionWorkspacePoolAnnotation]}, pool); err != nil {
			return r.phase(ctx, checkpoint, "Pending", "SourceUnavailable", "source RuntimePool is unavailable")
		}
		if pool.Labels[acpExecutionWorkspaceLinkLabel] != ws.Name || pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(ws.UID) || !substrateRuntimePoolSuspendCapable(pool) {
			return r.phase(ctx, checkpoint, "Failed", "UnsupportedSource", "checkpoint export requires the exact Substrate DataOnly workspace pool")
		}
		_, record, err := pools.readNativeSubstrateState(ctx, pool)
		if err != nil {
			return ctrl.Result{}, err
		}
		if record == nil || record.Checkpoint == nil {
			return r.phase(ctx, checkpoint, "Pending", "AwaitingCheckpoint", "waiting for a completed Data checkpoint; attached work is never interrupted")
		}
		recovery := checkpoint.Spec.RecoverLastCheckpoint && record.Failure != ""
		if !recovery && (record.Phase != substrateNativeSuspended || record.Attempt != nil || ws.Spec.Attachment != nil || ws.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended || !runtimePoolWorkspaceSuspendConsentRecorded(pool)) {
			return r.phase(ctx, checkpoint, "Pending", "AwaitingSuspension", "waiting for idle DataOnly suspension; failed work requires explicit recoverLastCheckpoint")
		}
		_, artifact, err := pools.readSubstrateCheckpointArtifact(ctx, record.Checkpoint.Digest)
		if err != nil {
			return ctrl.Result{}, err
		}
		if artifact == nil || !artifact.Owners[substratePoolCheckpointOwner(pool)] || artifact.Namespace != checkpoint.Namespace ||
			!reflect.DeepEqual(artifact.Checkpoint, *record.Checkpoint) ||
			artifact.ClassBinding != ws.Spec.ClassBinding || artifact.ProviderBinding != ws.Spec.ProviderBinding {
			return r.phase(ctx, checkpoint, "Failed", "ArtifactUnavailable", "the source no longer owns the verified Data artifact")
		}
		// Persist the selected digest before acquiring a reference. Retrying an
		// export can never silently select a newer checkpoint.
		checkpoint.Status.Digest = artifact.Checkpoint.Digest
		checkpoint.Status.ClassBinding = &artifact.ClassBinding
		checkpoint.Status.CreatedAt = artifact.Checkpoint.CreatedAt.DeepCopy()
		return r.phase(ctx, checkpoint, "Pending", "CapturingReference", "recorded the immutable Data checkpoint selected for export")
	}
	cm, artifact, err := pools.readSubstrateCheckpointArtifact(ctx, checkpoint.Status.Digest)
	if err != nil {
		return ctrl.Result{}, err
	}
	if artifact == nil || artifact.Namespace != checkpoint.Namespace ||
		!reflect.DeepEqual(checkpoint.Status.ClassBinding, &artifact.ClassBinding) {
		return r.phase(ctx, checkpoint, "Failed", "ArtifactUnavailable", "the recorded Data artifact or source identity is unavailable")
	}
	from := ""
	if !artifact.Owners[substratePublicCheckpointOwner(checkpoint)] {
		// Imported artifacts retain their original provenance. Acquire from
		// the exporting workspace's exact pool, which may itself be a restore.
		// Once acquired, the public reference survives that workspace's deletion.
		ws := &workspacev1alpha1.ExecutionWorkspace{}
		if err := pools.nativeSubstrateReader().Get(ctx,
			types.NamespacedName{Namespace: checkpoint.Namespace, Name: checkpoint.Spec.WorkspaceRef.Name}, ws); err != nil {
			if apierrors.IsNotFound(err) {
				return r.phase(ctx, checkpoint, "Failed", "SourceMissing", "the pinned source workspace was deleted before checkpoint reference acquisition")
			}
			return r.phase(ctx, checkpoint, "Pending", "SourceUnavailable", "source workspace is unavailable before reference acquisition")
		}
		pool := &corev1alpha1.RuntimePool{}
		if err := pools.nativeSubstrateReader().Get(ctx,
			types.NamespacedName{Namespace: ws.Namespace, Name: ws.Annotations[acpExecutionWorkspacePoolAnnotation]}, pool); err != nil {
			return r.phase(ctx, checkpoint, "Pending", "SourceUnavailable", "source RuntimePool is unavailable before reference acquisition")
		}
		if ws.UID != checkpoint.Spec.WorkspaceRef.UID || ws.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceControllerLabelValue ||
			pool.Labels[acpExecutionWorkspaceLinkLabel] != ws.Name || pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(ws.UID) ||
			!substrateRuntimePoolSuspendCapable(pool) || artifact.ClassBinding != ws.Spec.ClassBinding || artifact.ProviderBinding != ws.Spec.ProviderBinding {
			return r.phase(ctx, checkpoint, "Failed", "SourceChanged", "source workspace or pool binding changed before reference acquisition")
		}
		from = substratePoolCheckpointOwner(pool)
	}
	if err := pools.acquireSubstrateCheckpointArtifact(ctx, cm, artifact, from, substratePublicCheckpointOwner(checkpoint)); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) {
			return ctrl.Result{}, err
		}
		return r.phase(ctx, checkpoint, "Failed", "ReferenceUnavailable", "the selected Data artifact was released before export completed")
	}
	api, err := pools.substrateNativeClient()
	if err != nil {
		return ctrl.Result{}, err
	}
	defer api.Close() //nolint:errcheck
	if err := verifiedNativeSubstrateCheckpoint(ctx, api.Control, artifact.Atespace, &artifact.Checkpoint); err != nil {
		return r.phase(ctx, checkpoint, "Failed", "VerificationFailed", "provider Data artifact is unavailable or its immutable provenance changed")
	}
	if err := verifyNativeSubstrateTemplate(ctx, api.Control, artifact.Atespace, artifact.Checkpoint.Template); err != nil {
		return r.phase(ctx, checkpoint, "Failed", "TemplateUnavailable", "the immutable restore template is unavailable or has changed")
	}
	return r.phase(ctx, checkpoint, substrateNativeReady, "DataRetained", "verified Data checkpoint is retained independently of its source workspace")
}

func (r *SubstrateCheckpointReconciler) phase(ctx context.Context, checkpoint *workspacev1alpha1.ExecutionWorkspaceCheckpoint, phase, reason, message string) (ctrl.Result, error) {
	checkpoint.Status.Phase = phase
	conditionStatus := metav1.ConditionFalse
	if phase == substrateNativeReady {
		conditionStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&checkpoint.Status.Conditions, metav1.Condition{Type: substrateNativeReady, Status: conditionStatus, Reason: reason, Message: message, ObservedGeneration: checkpoint.Generation, LastTransitionTime: metav1.Now()})
	current := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{}
	if err := r.RuntimePools.nativeSubstrateReader().Get(ctx, client.ObjectKeyFromObject(checkpoint), current); err != nil {
		return ctrl.Result{}, err
	}
	if reflect.DeepEqual(current.Status, checkpoint.Status) {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, r.RuntimePools.Status().Update(ctx, checkpoint)
}

func (r *SubstrateCheckpointReconciler) collect(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pools := r.RuntimePools
	if req.Namespace != controllerNamespaceForRuntimePool(pools.ControllerNamespace) {
		return ctrl.Result{}, nil
	}
	kind, name, _ := strings.Cut(req.Name, "/")
	cm := &corev1.ConfigMap{}
	if err := pools.nativeSubstrateReader().Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: name}, cm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if kind == "template" {
		if cm.Labels[substrateTemplateBindingLabel] != substrateOwnedLabelValue {
			return ctrl.Result{}, nil
		}
		binding := &substrateTemplateBinding{}
		if err := json.Unmarshal([]byte(cm.Data["binding.json"]), binding); err != nil {
			return ctrl.Result{}, err
		}
		if !binding.Retired {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, (&nativeSubstrateTemplateStore{r: pools}).collectHistory(ctx, cm, binding)
		}
		manifest := &unstructured.Unstructured{}
		if err := json.Unmarshal(binding.Manifest, &manifest.Object); err != nil {
			return ctrl.Result{}, err
		}
		manifest.SetUID(types.UID(binding.Current.UID))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, (&nativeSubstrateTemplateStore{r: pools}).Delete(ctx, manifest)
	}
	artifact := &substrateCheckpointArtifact{}
	if cm.Labels[substrateCatalogLabel] != substrateOwnedLabelValue {
		return ctrl.Result{}, nil
	}
	if err := json.Unmarshal([]byte(cm.Data[substrateCatalogKey]), artifact); err != nil {
		return ctrl.Result{}, err
	}
	cm, artifact, err := pools.readSubstrateCheckpointArtifact(ctx, artifact.Checkpoint.Digest)
	if err != nil || artifact == nil {
		return ctrl.Result{}, err
	}
	// Repair references left by deletion or an interrupted owner finalization.
	// A same-name replacement is a different owner and cannot retain the data.
	for owner := range artifact.Owners {
		kind, identity, ok := strings.Cut(owner, ":")
		path, uid, hasUID := strings.Cut(identity, ":")
		namespace, name, hasNamespace := strings.Cut(path, "/")
		if !ok || !hasUID || !hasNamespace || namespace != artifact.Namespace {
			return ctrl.Result{}, fmt.Errorf("checkpoint owner identity is invalid")
		}
		var object client.Object
		switch kind {
		case "pool":
			object = &corev1alpha1.RuntimePool{}
		case "checkpoint":
			object = &workspacev1alpha1.ExecutionWorkspaceCheckpoint{}
		default:
			return ctrl.Result{}, fmt.Errorf("checkpoint owner kind is invalid")
		}
		err := pools.nativeSubstrateReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, object)
		// Discovery confirms removal of the optional checkpoint API. The
		// startup flag can be stale, and uncertain read failures must retain data.
		absent := apierrors.IsNotFound(err) || kind == "checkpoint" && meta.IsNoMatchError(err)
		if err != nil && !absent {
			return ctrl.Result{}, err
		}
		if absent || string(object.GetUID()) != uid {
			delete(artifact.Owners, owner)
			return ctrl.Result{RequeueAfter: time.Millisecond}, pools.saveSubstrateCheckpointArtifact(ctx, cm, artifact)
		}
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, pools.collectSubstrateCheckpointArtifact(ctx, cm, artifact)
}

func (r *SubstrateCheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Private catalogs and template journals exist without the optional public
	// checkpoint API, and must keep collecting after admission is disabled.
	builder := ctrl.NewControllerManagedBy(mgr).Named("substratecheckpoint").
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []reconcile.Request {
			prefix := ""
			if object.GetLabels()[substrateCatalogLabel] == substrateOwnedLabelValue {
				prefix = "catalog/"
			}
			if object.GetLabels()[substrateTemplateBindingLabel] == substrateOwnedLabelValue {
				prefix = "template/"
			}
			if prefix == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: prefix + object.GetName()}}}
		}))
	if r.CheckpointAPIInstalled {
		builder = builder.For(&workspacev1alpha1.ExecutionWorkspaceCheckpoint{})
	}
	return builder.Complete(r)
}
