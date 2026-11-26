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

// ADR 0057: a Task attaching a pool that already serves, or a settlement
// revoking it, changes only the workspace's attachment fields and, after
// core re-admits it, its core-admission record. Each change bumps the
// generation, and the serving path used to close admission until the
// provider observed it, then re-attest, re-bootstrap, and re-probe the same
// runtime. The pool now records the runtime-relevant spec and the exact
// instance it admitted, and keeps serving across a generation lag in which
// neither changed.

// externalRuntimeServedAnnotation records, on the RuntimePool, the admitted
// state: the runtime-relevant workspace spec digest, the allocation sequence
// and instance identity, and the active Pod UID and boot ID.
const externalRuntimeServedAnnotation = "orka.ai/external-runtime-served"

// externalWorkspaceRuntimeSpecDigest digests the workspace spec without the
// fields an attachment or its re-admission writes. Any other spec change
// (request, retirement, desired state, lifecycle, bindings) changes it.
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

// externalRuntimeServedRecord renders the admitted state of a workspace and
// the pool's active instance, or "" when either is incomplete.
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

// externalAttachmentOnlyLag reports that the workspace's generation moved
// past what the provider observed or core admitted only through attachment
// or re-admission fields, while the pool still serves the exact instance it
// admitted. Then the pool keeps serving instead of closing.
//
// Everything a re-probe would guard against is checked or unchanged:
//   - the runtime-relevant spec digest equals the admitted one, so no new
//     request, retirement, desired state, or lifecycle exists;
//   - the provider's last observation names the same allocation, instance,
//     and request revision, Ready with startup evidence, and the pool's active
//     Pod UID and boot ID are the admitted ones;
//   - core admission is lagging, not withdrawn or unverified: the immutable
//     bindings still match and the workspace carries a positive prior
//     Admitted=True (ReasonReady) condition for its last admitted generation;
//   - the workspace is not deleting, Failed, quarantined, or tainted, and the
//     pool has no retirement or eviction pending.
//
// Prompts cannot race the new attachment: a Task is queued only after the
// workspace handshake requires current core admission, the provider's
// observation of the current generation, and its acknowledgement of the
// attachment epoch.
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
	if admitted == nil || admitted.Status != metav1.ConditionTrue ||
		admitted.Reason != string(workspacev1alpha1.ReasonReady) ||
		admitted.ObservedGeneration != w.Spec.CoreAdmission.AdmittedGeneration {
		return false
	}
	a := w.Status.Allocation
	if a == nil || a.State != workspacev1alpha1.AllocationReady || a.Startup == nil {
		return false
	}
	recorded := pool.Annotations[externalRuntimeServedAnnotation]
	return recorded != "" && recorded == externalRuntimeServedRecord(w, pool.Status.ActiveInstance)
}

// recordExternalRuntimeServed records the admitted state once the pool
// serves and accepts the exact attested instance at the current generation.
// It writes only when the record changes; the patch refreshes pool in place,
// so the rest of the pass continues on the new resourceVersion.
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
