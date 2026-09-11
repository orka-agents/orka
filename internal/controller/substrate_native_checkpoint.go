package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (r *RuntimePoolReconciler) drainNativeSubstrateRuntime(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, cm *corev1.ConfigMap, record *substrateNativeState, actor *ateapipb.Actor, checkpoint bool) (bool, ctrl.Result, error) {
	wait := func(message string) (bool, ctrl.Result, error) {
		poolStatus := r.baseRuntimePoolStatus(pool, 1)
		poolStatus.ActiveInstance = pool.Status.ActiveInstance
		poolStatus.Lifecycle, poolStatus.AdmissionState, poolStatus.Message = corev1alpha1.RuntimePoolLifecycleDraining, corev1alpha1.RuntimePoolAdmissionDraining, message
		r.setRuntimePoolCondition(pool, &poolStatus, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, message)
		result, err := r.finishRuntimePoolStatus(ctx, pool, poolStatus, time.Second)
		return false, result, err
	}
	// Queued Tasks need the replacement Actor and cannot drain on the closed
	// old instance. Reservations and finalization still block its retirement.
	recycling := record.RecycleRequested && pool.Spec.DesiredReplicas > 0 && pool.DeletionTimestamp.IsZero()
	controllerQuiescent := runtimePoolControllerWorkIsQuiescent(pool.Status.Capacity)
	if recycling {
		controllerQuiescent = runtimePoolRolloutControllerWorkIsQuiescent(pool.Status.Capacity)
	}
	if !controllerQuiescent {
		return wait(runtimePoolMessageDrainSettling)
	}
	if record.Attempt != nil && record.Attempt.BootID != "" && record.Attempt.DrainStartedAt.IsZero() {
		record.Attempt.DrainStartedAt = metav1.NewTime(r.now())
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return false, ctrl.Result{}, err
		}
		return wait("recorded the native runtime drain deadline")
	}
	if record.Attempt == nil || record.Attempt.BootID == "" || actor == nil || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		if checkpoint {
			// Demand can disappear during boot, including after controller
			// recovery retires the previous prompt. Persist failure so cleanup
			// can stop this uncheckpointable attempt and retain verified data.
			result, err := r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "a data checkpoint requires an admitted, running native Actor with authenticated quiescence; the last verified checkpoint remains available for explicit recovery")
			return false, result, err
		}
		return true, ctrl.Result{}, nil
	}
	template, err := r.substrateTemplates().Get(ctx, record.Atespace, runtimePoolSubstrateTemplateName(cfg.baseName))
	if err != nil {
		return false, ctrl.Result{}, err
	}
	validationPool, validationConfig, auth, err := r.substrateRuntimePoolDeployedValidationTarget(ctx, pool, cfg, template)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	route := substrateActorRouteHost(workspace.SubstrateActorKey(record.Atespace, record.Attempt.Name), r.SubstrateConfig.ActorDNSSuffix)
	synthetic := substrateSyntheticInstancePod(validationPool, validationConfig, nativeSubstrateRuntimeActorView(actor), record.Attempt.Name, route)
	endpoint := runtimePoolInstanceEndpoint(validationPool, synthetic)
	probe, err := r.supervisorClientForPool(pool).Probe(ctx, endpoint, string(auth.Data[runtimePoolControllerTokenKey]), auth.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		if r.now().Sub(record.Attempt.DrainStartedAt.Time) > max(3*r.SubstrateConfig.WithDefaults().ClaimTimeout, 5*time.Minute) {
			if !checkpoint {
				return true, ctrl.Result{}, nil
			}
			result, err := r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "authenticated drain could not be proved within its recovery window; the last checkpoint remains available for explicit recovery")
			return false, result, err
		}
		return wait("waiting for the exact native supervisor's authenticated drain status")
	}
	active, err := validateRuntimePoolProbe(validationPool, validationConfig, synthetic, probe, r.now())
	if err != nil || active.BootID != record.Attempt.BootID {
		result, finishErr := r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("native supervisor identity changed before drain; checkpoint admission is closed"))
		return false, result, finishErr
	}
	if !probe.Status.Drain.Requested {
		if err := r.supervisorClientForPool(pool).RequestDrain(ctx, endpoint, string(auth.Data[runtimePoolControllerTokenKey]), auth.Data[runtimePoolCapabilitySecretKey], probe.Status, "runtime_pool_workspace_suspend"); err != nil {
			return false, ctrl.Result{}, err
		}
		return wait(runtimePoolMessageDrainRequested)
	}
	probeQuiescent := runtimePoolProbeIsQuiescent(pool.Status.Capacity, probe.Status)
	if recycling {
		probeQuiescent = runtimePoolRolloutProbeIsQuiescent(pool.Status.Capacity, probe.Status)
	}
	if !probeQuiescent {
		return wait(runtimePoolMessageDrainSettling)
	}
	if err := r.recordDrainedRuntimePoolTaskCleanup(ctx, validationPool, active, probe.Status); err != nil {
		return false, ctrl.Result{}, err
	}
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent {
		poolStatus := r.baseRuntimePoolStatus(pool, 1)
		poolStatus.ActiveInstance = active
		applyRuntimePoolProbeCapacity(&poolStatus, cfg, probe)
		poolStatus.Lifecycle, poolStatus.AdmissionState, poolStatus.Message = corev1alpha1.RuntimePoolLifecycleQuiescent, corev1alpha1.RuntimePoolAdmissionDraining, runtimePoolMessageDrainQuiescent
		result, err := r.finishRuntimePoolStatus(ctx, pool, poolStatus, time.Second)
		return false, result, err
	}
	return true, ctrl.Result{}, nil
}

func (r *RuntimePoolReconciler) beginNativeSubstrateCheckpoint(ctx context.Context, pool *corev1alpha1.RuntimePool, cm *corev1.ConfigMap, record *substrateNativeState, actor *ateapipb.Actor) (ctrl.Result, error) {
	if !substrateRuntimePoolSuspendCapable(pool) || record.Attempt == nil || record.Attempt.Worker == nil || record.Attempt.BootID == "" || actor == nil || actor.GetStatus().GetCurrentActorTemplateUid() != record.Attempt.Template.UID {
		return ctrl.Result{}, fmt.Errorf("native Data checkpoint requires the admitted Actor, template, and worker identities")
	}
	suffix, err := r.randomHex(16)
	if err != nil {
		return ctrl.Result{}, err
	}
	record.Pending = &substrateNativeCheckpointOperation{Name: "orka-data-" + suffix, SourceName: record.Attempt.Name, SourceUID: record.Attempt.UID, TemplateUID: record.Attempt.Template.UID, StartedAt: metav1.NewTime(r.now()), PriorSnapshotDigest: nativeSubstrateSnapshotDigest(actor.GetStatus().GetExternalSnapshot())}
	record.Phase = substrateNativeCheckpointing
	record.RecycleRequested = false
	if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateNativeDataProtection, nativeSubstrateRecordID(record)); err != nil {
		return ctrl.Result{}, err
	}
	return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "recorded a Data checkpoint intent after authenticated runtime quiescence")
}

//nolint:gocyclo // Keep ordered durable transitions and their failure boundaries visible together.
func (r *RuntimePoolReconciler) checkpointNativeSubstrateRuntime(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, api ateapipb.ControlClient, cm *corev1.ConfigMap, record *substrateNativeState, actor *ateapipb.Actor) (ctrl.Result, error) {
	a, operation := record.Attempt, record.Pending
	if a == nil || operation == nil || actor == nil || a.Worker == nil || a.UID == "" || actor.GetStatus().GetCurrentActorTemplateUid() != a.Template.UID {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native checkpoint lost its source Actor, worker, or template identity; any existing Tag is preserved")
	}
	if nativeSubstrateOperationExpired(operation, r.now(), r.SubstrateConfig.WithDefaults().ClaimTimeout) {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native checkpoint did not settle within its recovery window; the source and any partial Tag remain recorded for cleanup")
	}
	if err := verifyNativeSubstrateTemplate(ctx, api, record.Atespace, a.Template); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if !a.WorkerDrained {
		if err := drainNativeSubstrateWorker(ctx, api, record); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		a.WorkerDrained = true
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "native worker is drained and cannot accept another Actor during checkpoint cleanup")
	}
	if !operation.SuspendIssued {
		if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native Actor stopped before Orka issued the checkpoint; refusing to claim an unrelated snapshot")
		}
		if operation.SuspendStartedAt.IsZero() {
			operation.SuspendStartedAt = metav1.NewTime(r.now())
		}
		operation.SuspendIssued = true
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		if _, err := api.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: nativeSubstrateActorRef(record)}); err != nil {
			if nativeSubstrateControlAuthenticationRejected(err) {
				operation.SuspendIssued = false
				if saveErr := r.saveNativeSubstrateState(ctx, cm, record); saveErr != nil {
					return ctrl.Result{}, saveErr
				}
			}
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("native Data suspension requires observation before recovery: %w", err))
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "native Actor is saving DurableDir data with process memory excluded")
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "observing the native suspension result without replaying its mutation")
	}
	snapshot := actor.GetStatus().GetExternalSnapshot()
	digest := nativeSubstrateSnapshotDigest(snapshot)
	if snapshot.GetContentScope() != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA || snapshot.GetSnapshotUri() == "" || digest == operation.PriorSnapshotDigest {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native suspension did not expose a new complete Data snapshot")
	}
	if operation.SourceSnapshotDigest == "" {
		operation.SourceSnapshotDigest, operation.SourceVersion = digest, actor.GetMetadata().GetVersion()
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "recorded the observed source snapshot before immutable Tag creation")
	}
	if digest != operation.SourceSnapshotDigest || actor.GetMetadata().GetVersion() != operation.SourceVersion {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native source changed while its Tag was being captured; preserving both for explicit recovery")
	}
	tagRef := &ateapipb.ObjectRef{Atespace: record.Atespace, Name: operation.Name}
	tag, err := api.GetTag(ctx, &ateapipb.GetTagRequest{Tag: tagRef})
	if status.Code(err) == codes.NotFound {
		if !operation.TagIssued {
			operation.TagStartedAt = metav1.NewTime(r.now())
			operation.TagIssued = true
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, err
			}
		}
		tag, err = api.CreateTag(ctx, &ateapipb.CreateTagRequest{Tag: &ateapipb.Tag{
			Metadata: &ateapipb.ResourceMetadata{Atespace: record.Atespace, Name: operation.Name}, Scope: ateapipb.TagScope_TAG_SCOPE_ATESPACE, SourceActor: nativeSubstrateActorRef(record),
		}})
		if status.Code(err) == codes.AlreadyExists {
			return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "recovering the native Tag created by the recorded operation")
		}
	}
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("native Tag capture is pending recovery: %w", err))
	}
	if tag.GetMetadata().GetUid() == "" || tag.GetMetadata().GetAtespace() != record.Atespace || tag.GetMetadata().GetName() != operation.Name || tag.GetSourceActor().GetName() != a.Name || tag.GetSourceActor().GetAtespace() != record.Atespace || tag.GetScope() != ateapipb.TagScope_TAG_SCOPE_ATESPACE {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native Tag ownership differs from the recorded capture intent")
	}
	if operation.TagUID == "" {
		operation.TagUID = tag.GetMetadata().GetUid()
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
	} else if operation.TagUID != tag.GetMetadata().GetUid() {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native Tag UID changed while checkpointing; refusing the replacement")
	}
	if tag.GetStatus().GetSnapshot() == nil {
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "waiting for the native Tag's independent snapshot copy")
	}
	if tag.GetStatus().GetSourceActorUid() != a.UID || tag.GetStatus().GetActorTemplateUid() != a.Template.UID {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native Tag provenance does not match the checkpoint source lifetime")
	}
	tagDigest, err := nativeSubstrateTagDigest(tag)
	if err != nil {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native Tag did not contain a complete Data snapshot")
	}
	// CreateTag snapshots the Actor at the time the call executes. Reading the
	// same UID/version and external snapshot afterward proves that the source
	// did not move while the immutable Tag was being copied. This is an observed
	// postcondition, not a claim of atomic Suspend/Tag operation fencing.
	observed, err := getNativeSubstrateActor(ctx, api, record)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if observed == nil || observed.GetMetadata().GetVersion() != operation.SourceVersion || observed.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED || nativeSubstrateSnapshotDigest(observed.GetStatus().GetExternalSnapshot()) != operation.SourceSnapshotDigest {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native source changed during Tag capture; the captured data remains quarantined")
	}
	checkpoint := &substrateNativeCheckpoint{Name: operation.Name, UID: operation.TagUID, Digest: tagDigest, SourceName: a.Name, SourceUID: a.UID, Template: a.Template}
	if err := r.registerSubstrateCheckpointArtifact(ctx, pool, cfg, record, checkpoint); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if record.Checkpoint != nil {
		record.RetiredTags = append(record.RetiredTags, *record.Checkpoint)
	}
	record.Checkpoint = checkpoint
	record.Pending = nil
	record.Phase, record.AfterStop = substrateNativeStopping, substrateNativeSuspended
	if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
		return ctrl.Result{}, err
	}
	return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "verified and recorded the immutable Data Tag; proving workload termination before suspension completes")
}

func verifyNativeSubstrateTemplate(ctx context.Context, api ateapipb.ControlClient, atespace string, expected substrateNativeTemplateRevision) error {
	template, err := api.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: &ateapipb.ObjectRef{Atespace: atespace, Name: expected.Name}})
	if err != nil {
		return err
	}
	digest, err := nativeSubstrateTemplateDigest(template)
	if err != nil {
		return err
	}
	config := template.GetSnapshotsConfig()
	if template.GetMetadata().GetUid() != expected.UID || digest != expected.Digest ||
		config.GetOnPause() != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA || config.GetOnCommit() != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA || config.GetOnResume().GetFromData() != ateapipb.ResumeSource_RESUME_SOURCE_COLD_BOOT {
		return fmt.Errorf("native template identity or explicit Data/Data/ColdBoot policy changed")
	}
	return nil
}

func verifiedNativeSubstrateCheckpoint(ctx context.Context, api ateapipb.ControlClient, atespace string, checkpoint *substrateNativeCheckpoint) error {
	if checkpoint == nil || checkpoint.UID == "" || !validSHA256Digest(checkpoint.Digest) {
		return fmt.Errorf("native checkpoint record is incomplete")
	}
	tag, err := api.GetTag(ctx, &ateapipb.GetTagRequest{Tag: &ateapipb.ObjectRef{Atespace: atespace, Name: checkpoint.Name}})
	if err != nil {
		return err
	}
	digest, err := nativeSubstrateTagDigest(tag)
	if err != nil {
		return err
	}
	if tag.GetMetadata().GetUid() != checkpoint.UID || tag.GetMetadata().GetAtespace() != atespace || tag.GetMetadata().GetName() != checkpoint.Name || tag.GetScope() != ateapipb.TagScope_TAG_SCOPE_ATESPACE ||
		tag.GetSourceActor().GetAtespace() != atespace || tag.GetSourceActor().GetName() != checkpoint.SourceName || tag.GetStatus().GetSourceActorUid() != checkpoint.SourceUID || tag.GetStatus().GetActorTemplateUid() != checkpoint.Template.UID || digest != checkpoint.Digest {
		return fmt.Errorf("native checkpoint Tag identity or immutable provenance changed")
	}
	return nil
}

//nolint:gocyclo // Keep ordered durable transitions and their failure boundaries visible together.
func (r *RuntimePoolReconciler) stopNativeSubstrateRuntime(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, api ateapipb.ControlClient, cm *corev1.ConfigMap, record *substrateNativeState, actor *ateapipb.Actor) (ctrl.Result, error) {
	a := record.Attempt
	if a == nil {
		return r.finishNativeSubstrateStopped(ctx, pool, "native Substrate workload is absent")
	}
	if actor == nil && a.UID == "" && a.CreateIssued {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("native Actor creation is still ambiguous; its journal remains until the unique creation name can be recovered and cleaned up"))
	}
	if actor != nil && a.UID == "" {
		a.UID = actor.GetMetadata().GetUid()
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "recovered the native Actor UID for exact cleanup")
	}
	if actor != nil && actor.GetStatus().GetWorkerAssignment() != nil {
		assignment := actor.GetStatus().GetWorkerAssignment()
		if a.Worker == nil || a.WorkloadAbsent && a.Worker.PodUID != assignment.GetWorkerPodUid() {
			absentWorker, absent, err := r.nativeSubstrateAbsentAssignment(ctx, pool, actor)
			if err != nil {
				return ctrl.Result{}, err
			}
			if absent {
				a.Worker, a.WorkloadAbsent = absentWorker, true
				if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
					return ctrl.Result{}, err
				}
				return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "the exact worker Pod disappeared before its boot identity was recorded")
			}
			worker, err := r.nativeSubstrateWorker(ctx, api, pool, actor)
			if err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
			a.Worker, a.WorkerDrained, a.WorkloadAbsent = worker, false, false
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, err
			}
			return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "recorded the provider worker before exact workload cleanup")
		}
		if a.Worker.PodUID != assignment.GetWorkerPodUid() && !a.WorkloadAbsent {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("native Actor moved while its old worker still needs termination proof"))
		}
	}
	if !a.WorkloadAbsent && a.Worker != nil {
		absent, err := r.nativeSubstrateWorkerPodAbsent(ctx, a.Worker)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !absent {
			if err := drainNativeSubstrateWorker(ctx, api, record); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
			if !a.WorkerDrained {
				a.WorkerDrained = true
				if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
					return ctrl.Result{}, err
				}
				return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "native worker is fenced from new placements before Pod deletion")
			}
			absent, err = r.terminateNativeSubstrateWorker(ctx, record)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !absent {
				return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "waiting for the exact native worker Pod to disappear")
			}
		}
		a.WorkloadAbsent = true
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "independently proved that the previous native workload is absent")
	}
	if a.BootRequested && !a.WorkloadAbsent {
		// Actor disappearance or suspension cannot prove that a booted worker
		// stopped. Preserve the journal until an exact worker fence is recovered.
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("native boot has no independent workload-absence proof; preserving its Actor and journal for recovery"))
	}
	if actor != nil {
		if !a.DeleteIssued {
			a.DeleteIssued = true
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, err
			}
		}
		_, err := api.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: nativeSubstrateActorRef(record), AnyState: true})
		if err != nil && status.Code(err) != codes.NotFound {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "native Actor deletion requested without another snapshot")
	}
	record.Attempt = nil
	if record.AfterStop == substrateNativeSuspended && record.Checkpoint != nil {
		record.Phase = substrateNativeSuspended
	} else if record.Failure != "" {
		record.Phase = substrateNativeFailed
	} else {
		record.Phase = substrateNativeProvisioning
	}
	record.AfterStop = ""
	if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
		return ctrl.Result{}, err
	}
	if record.Phase == substrateNativeSuspended {
		if err := verifiedNativeSubstrateCheckpoint(ctx, api, record.Atespace, record.Checkpoint); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateNativeCheckpointConsent, nativeSubstrateConsentAnnotation(record)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if record.Phase == substrateNativeFailed {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New(record.Failure))
	}
	return r.finishNativeSubstrateStopped(ctx, pool, "native Actor and its workload are absent; durable data remains only in recorded Tags")
}

func (r *RuntimePoolReconciler) nativeSubstrateWorkerPodAbsent(ctx context.Context, fence *substrateNativeWorkerFence) (bool, error) {
	pod := &corev1.Pod{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	err := reader.Get(ctx, types.NamespacedName{Namespace: fence.Namespace, Name: fence.Pod}, pod)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return string(pod.UID) != fence.PodUID, nil
}

//nolint:gocyclo // Keep ordered durable transitions and their failure boundaries visible together.
func (r *RuntimePoolReconciler) deleteNativeSubstrateState(ctx context.Context, pool *corev1alpha1.RuntimePool) (bool, error) {
	cm, record, err := r.readNativeSubstrateState(ctx, pool)
	if err != nil || record == nil {
		return false, err
	}
	if record.Attempt != nil {
		return true, nil
	}
	if record.Pending != nil && record.Pending.TagIssued {
		api, err := r.substrateNativeClient()
		if err != nil {
			return false, err
		}
		defer api.Close() //nolint:errcheck
		tag, err := api.Control.GetTag(ctx, &ateapipb.GetTagRequest{Tag: &ateapipb.ObjectRef{Atespace: record.Atespace, Name: record.Pending.Name}})
		if err == nil {
			if digest, digestErr := nativeSubstrateTagDigest(tag); digestErr == nil {
				_, artifact, readErr := r.readSubstrateCheckpointArtifact(ctx, digest)
				if readErr != nil {
					return false, readErr
				}
				if artifact != nil {
					if err := r.releaseSubstrateCheckpointArtifact(ctx, digest, substratePoolCheckpointOwner(pool)); err != nil {
						return false, err
					}
					record.Pending = nil
					return true, r.saveNativeSubstrateState(ctx, cm, record)
				}
			}
			if record.Pending.TagUID != "" && tag.GetMetadata().GetUid() != record.Pending.TagUID {
				return false, fmt.Errorf("refusing to delete a replaced partial native Tag")
			}
			if tag.GetMetadata().GetAtespace() != record.Atespace || tag.GetMetadata().GetName() != record.Pending.Name {
				return false, fmt.Errorf("native Tag cleanup identity changed")
			}
			if record.Pending.SourceName == "" || record.Pending.SourceUID == "" ||
				tag.GetSourceActor().GetAtespace() != record.Atespace || tag.GetSourceActor().GetName() != record.Pending.SourceName ||
				(tag.GetStatus().GetSourceActorUid() != "" && tag.GetStatus().GetSourceActorUid() != record.Pending.SourceUID) ||
				(tag.GetStatus().GetActorTemplateUid() != "" && tag.GetStatus().GetActorTemplateUid() != record.Pending.TemplateUID) {
				return false, fmt.Errorf("partial native Tag does not match its recorded source lifetime")
			}
			if _, err := api.Control.DeleteTag(ctx, &ateapipb.DeleteTagRequest{Tag: &ateapipb.ObjectRef{Atespace: record.Atespace, Name: record.Pending.Name}}); err != nil {
				return false, err
			}
			return true, nil
		}
		if status.Code(err) != codes.NotFound {
			return false, err
		}
		record.Pending = nil
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return false, err
		}
		return true, nil
	}
	checkpoints := append([]substrateNativeCheckpoint{}, record.RetiredTags...)
	if record.Checkpoint != nil {
		checkpoints = append(checkpoints, *record.Checkpoint)
	}
	for _, checkpoint := range checkpoints {
		if err := r.releaseSubstrateCheckpointArtifact(ctx, checkpoint.Digest, substratePoolCheckpointOwner(pool)); err != nil {
			return false, err
		}
		artifactCM, artifact, err := r.readSubstrateCheckpointArtifact(ctx, checkpoint.Digest)
		if err != nil {
			return false, err
		}
		if artifact != nil && len(artifact.Owners) == 0 {
			return true, r.collectSubstrateCheckpointArtifact(ctx, artifactCM, artifact)
		}
	}
	// An interrupted import/capture may have acquired a catalog reference
	// before recording it in the pool journal. Release those references too.
	artifacts, err := r.listSubstrateCheckpointArtifacts(ctx)
	if err != nil {
		return false, err
	}
	for _, item := range artifacts {
		artifact := &substrateCheckpointArtifact{}
		if err := json.Unmarshal([]byte(item.Data[substrateCatalogKey]), artifact); err != nil {
			return false, err
		}
		if artifact.Owners[substratePoolCheckpointOwner(pool)] {
			return true, r.releaseSubstrateCheckpointArtifact(ctx, artifact.Checkpoint.Digest, substratePoolCheckpointOwner(pool))
		}
		if len(artifact.Owners) == 0 && artifact.SourcePool.UID == pool.UID {
			return true, r.collectSubstrateCheckpointArtifact(ctx, &item, artifact)
		}
	}
	if pool.Annotations[substrateNativeJournalAnnotation] != substrateNativeJournalReleased {
		// Only completed provider cleanup permits journal deletion. Persist
		// that distinction before deleting the recovery record, so a restart
		// can finish finalization without accepting accidental journal loss.
		return true, r.markNativeSubstrateJournal(ctx, pool, substrateNativeJournalReleased)
	}
	if err := r.Delete(ctx, cm, client.Preconditions{UID: &cm.UID, ResourceVersion: &cm.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

func (r *RuntimePoolReconciler) nativeSubstrateAbsentAssignment(ctx context.Context, pool *corev1alpha1.RuntimePool, actor *ateapipb.Actor) (*substrateNativeWorkerFence, bool, error) {
	a := actor.GetStatus().GetWorkerAssignment()
	namespace, workerPool, err := substrateRuntimePoolWorkerPlacementFromAnnotation(pool)
	if err != nil {
		return nil, false, err
	}
	if a.GetWorkerNamespace() != namespace || a.GetWorkerPod() == "" || a.GetWorkerPodUid() == "" {
		return nil, false, fmt.Errorf("native worker assignment lacks the admitted placement identity")
	}
	f := &substrateNativeWorkerFence{Name: a.GetWorker().GetName(), Namespace: namespace, Pool: workerPool, Pod: a.GetWorkerPod(), PodUID: a.GetWorkerPodUid()}
	absent, err := r.nativeSubstrateWorkerPodAbsent(ctx, f)
	return f, absent, err
}

func (r *RuntimePoolReconciler) deleteNativeSubstrateTemplateAndPolicies(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) (bool, error) {
	template, err := r.getSubstrateActorTemplateForCleanup(ctx, pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName))
	if err != nil {
		return false, err
	}
	if template != nil && template.GetLabels()[runtimePoolUIDLabel] != string(pool.UID) {
		return false, fmt.Errorf("native template cleanup owner changed")
	}
	if remaining, err := r.deleteSubstrateRuntimePoolNetworkPolicies(ctx, pool, cfg, template); err != nil || remaining {
		return remaining, err
	}
	if template == nil {
		return false, nil
	}
	if err := r.substrateTemplates().Delete(ctx, template); err != nil {
		return false, err
	}
	return true, nil
}

func (r *RuntimePoolReconciler) requestNativeSubstrateRecycle(ctx context.Context, pool *corev1alpha1.RuntimePool, pod *corev1.Pod) error {
	cm, record, err := r.readNativeSubstrateState(ctx, pool)
	if err != nil || record == nil || record.Attempt == nil {
		return err
	}
	if pod == nil || string(pod.UID) != substrateActorInstanceUID(record.Attempt.Name) {
		return fmt.Errorf("native runtime rotation does not identify the recorded instance")
	}
	if record.Phase == substrateNativeCheckpointing || record.Phase == substrateNativeStopping {
		return nil
	}
	record.RecycleRequested = true
	return r.saveNativeSubstrateState(ctx, cm, record)
}
