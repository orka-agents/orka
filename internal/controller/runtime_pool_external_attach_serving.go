// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

const externalRuntimeServedAnnotation = "orka.ai/external-runtime-served"

func externalWorkspaceRuntimeSpecDigest(w *workspacev1alpha1.ExecutionWorkspace) string {
	spec := w.Spec.DeepCopy()
	spec.Attachment, spec.AttachmentEpoch, spec.CoreAdmission = nil, 0, nil
	encoded, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func externalRuntimeServedRecord(w *workspacev1alpha1.ExecutionWorkspace, active *corev1alpha1.RuntimePoolActiveInstanceStatus) string {
	a := w.Status.Allocation
	digest := externalWorkspaceRuntimeSpecDigest(w)
	if a == nil || active == nil || digest == "" || active.PodUID == "" || active.BootID == "" {
		return ""
	}
	return strings.Join([]string{digest, fmt.Sprint(a.Sequence), a.Identity.AllocationID, a.Identity.InstanceID,
		a.Identity.RequestRevision, active.PodUID, active.BootID}, " ")
}

func externalRuntimePoolAdmitting(pool *corev1alpha1.RuntimePool) bool {
	return pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing &&
		pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting && pool.Status.ActiveInstance != nil
}

func externalAttachmentOnlyLag(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) bool {
	if !externalRuntimePoolAdmitting(pool) || (w.Status.ObservedGeneration == w.Generation && workspaceCurrentlyAdmittedByCore(w)) {
		return false
	}
	if !w.DeletionTimestamp.IsZero() || w.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		w.Spec.Retirement != nil || w.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed ||
		acpWorkspaceQuarantined(w) || operatoraccess.Tainted(w.Annotations) ||
		pool.Annotations[externalRuntimeRetirementRequestedAnnotation] == booleanTrueValue ||
		strings.TrimSpace(pool.Annotations[externalRuntimeEvictionAnnotation]) != "" {
		return false
	}
	if !workspaceHasCoreAdmission(w) || w.Spec.CoreAdmission.AdmittedGeneration <= 0 {
		return false
	}
	admitted := workspaceprovider.FindCondition(w.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted))
	admittedGenValid := admitted != nil && admitted.Status == metav1.ConditionTrue &&
		admitted.Reason == string(workspacev1alpha1.ReasonReady) &&
		(admitted.ObservedGeneration == w.Spec.CoreAdmission.AdmittedGeneration ||
			(w.Spec.CoreAdmission.AdmittedGeneration < w.Generation && admitted.ObservedGeneration == w.Generation+1))
	if !admittedGenValid {
		return false
	}
	a := w.Status.Allocation
	if a == nil || a.State != workspacev1alpha1.AllocationReady || a.Startup == nil {
		return false
	}
	recorded := pool.Annotations[externalRuntimeServedAnnotation]
	return recorded != "" && recorded == externalRuntimeServedRecord(w, pool.Status.ActiveInstance)
}

func (r *RuntimePoolReconciler) recordExternalRuntimeServed(ctx context.Context, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace, pod *corev1.Pod) error {
	if !externalRuntimePoolAdmitting(pool) || pod == nil || pool.Status.ActiveInstance.PodUID != string(pod.UID) ||
		w.Status.ObservedGeneration != w.Generation || !workspaceCurrentlyAdmittedByCore(w) {
		return nil
	}
	record := externalRuntimeServedRecord(w, pool.Status.ActiveInstance)
	if record == "" || pool.Annotations[externalRuntimeServedAnnotation] == record {
		return nil
	}
	before := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[externalRuntimeServedAnnotation] = record
	return r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
