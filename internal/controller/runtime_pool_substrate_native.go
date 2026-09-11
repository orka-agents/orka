package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

//nolint:gocyclo // Keep ordered durable transitions and their failure boundaries visible together.
func (r *RuntimePoolReconciler) reconcileNativeSubstrateRuntimePool(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) (ctrl.Result, error) {
	cm, record, err := r.readNativeSubstrateState(ctx, pool)
	if err != nil {
		if errors.Is(err, errNativeSubstrateJournalMissing) && pool.Annotations[substrateNativeJournalAnnotation] == "" {
			// Preserve the rejection before failure status clears ActiveInstance,
			// which may be the only surviving evidence of the previous runtime.
			if markErr := r.markNativeSubstrateJournal(ctx, pool, substrateNativeJournalRequired); markErr != nil {
				return ctrl.Result{}, markErr
			}
		}
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	deleting := !pool.DeletionTimestamp.IsZero()
	if record == nil {
		if deleting || pool.Spec.DesiredReplicas == 0 {
			return r.finishNativeSubstrateStopped(ctx, pool, "native Substrate runtime is stopped")
		}
		record = &substrateNativeState{Schema: substrateNativeStateSchema, PoolUID: string(pool.UID), Atespace: pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateNamespace, Phase: substrateNativeProvisioning}
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "recorded native Substrate runtime ownership")
	}
	marker := pool.Annotations[substrateNativeJournalAnnotation]
	if marker == "" {
		// Commit a second durable record before any provider operation. Losing
		// the journal after this barrier must never recreate or finalize work.
		if err := r.markNativeSubstrateJournal(ctx, pool, substrateNativeJournalRequired); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "recorded native Substrate journal recovery requirement")
	}
	if marker != substrateNativeJournalRequired && (!deleting || marker != substrateNativeJournalReleased || record.Attempt != nil) {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("native Substrate journal lifecycle marker is invalid"))
	}
	if deleting && record.Attempt == nil {
		if marker == substrateNativeJournalRequired && record.Phase == substrateNativeFailed && record.Failure != "" {
			if err := r.recordFailedNativeSubstrateTaskCleanup(ctx, pool); err != nil {
				return ctrl.Result{}, err
			}
		}
		// No Actor attempt exists. Finalization requests native credentials
		// only if retained provider data still needs collection.
		return r.finishNativeSubstrateStopped(ctx, pool, "native Substrate workload is absent; deleting retained data")
	}
	api, err := r.substrateNativeClient()
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	defer api.Close() //nolint:errcheck
	if len(record.RetiredTags) != 0 {
		if err := r.releaseSubstrateCheckpointArtifact(ctx, record.RetiredTags[0].Digest, substratePoolCheckpointOwner(pool)); err != nil {
			return ctrl.Result{}, err
		}
		record.RetiredTags = record.RetiredTags[1:]
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	actor, err := getNativeSubstrateActor(ctx, api.Control, record)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if deleting {
		record.AfterStop = "Delete"
		// The user requested deletion. Pending or failed checkpoints remain in
		// the journal until workload and provider cleanup complete.
		if record.Phase != substrateNativeStopping {
			ready, result, err := r.drainNativeSubstrateRuntime(ctx, pool, cfg, cm, record, actor, false)
			if err != nil || !ready {
				return result, err
			}
			record.Phase = substrateNativeStopping
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.stopNativeSubstrateRuntime(ctx, pool, cfg, api.Control, cm, record, actor)
	}
	if record.Phase == substrateNativeCheckpointing {
		return r.checkpointNativeSubstrateRuntime(ctx, pool, cfg, api.Control, cm, record, actor)
	}
	if record.Phase == substrateNativeStopping {
		return r.stopNativeSubstrateRuntime(ctx, pool, cfg, api.Control, cm, record, actor)
	}
	if record.Failure != "" || record.Phase == substrateNativeFailed {
		if record.Attempt != nil {
			ready, result, err := r.drainNativeSubstrateRuntime(ctx, pool, cfg, cm, record, actor, false)
			if err != nil || !ready {
				return result, err
			}
			record.Phase, record.AfterStop = substrateNativeStopping, substrateNativeFailed
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, err
			}
			return r.stopNativeSubstrateRuntime(ctx, pool, cfg, api.Control, cm, record, actor)
		}
		if nativeSubstrateFailedPoolInactive(pool) {
			if err := r.recordFailedNativeSubstrateTaskCleanup(ctx, pool); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New(record.Failure))
	}
	if record.Attempt != nil && record.Attempt.UID != "" && actor == nil {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "the recorded Actor disappeared; retained checkpoints are available for explicit recovery, but uncertain work is never replayed")
	}
	if record.Phase == substrateNativeSuspended {
		if err := verifiedNativeSubstrateCheckpoint(ctx, api.Control, record.Atespace, record.Checkpoint); err != nil {
			return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "the retained Data Tag is unavailable or its provenance changed")
		}
		if pool.Spec.DesiredReplicas == 0 {
			if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateNativeCheckpointConsent, nativeSubstrateConsentAnnotation(record)); err != nil {
				return ctrl.Result{}, err
			}
			return r.finishNativeSubstrateStopped(ctx, pool, "workspace data is preserved in a verified native Tag; no Actor workload remains")
		}
	}
	if pool.Spec.DesiredReplicas == 0 {
		if record.Attempt == nil {
			return r.finishNativeSubstrateStopped(ctx, pool, "native Substrate runtime is stopped")
		}
		if pending, err := r.linkedWorkspaceSuspendIntentPending(ctx, pool); err != nil {
			return ctrl.Result{}, err
		} else if pending && !substrateWorkspaceSuspendRequested(pool) {
			// The upgrade coordinator still needs the admitted instance to
			// authenticate drain while Task settlement records the detach intent.
			poolStatus := r.baseRuntimePoolStatus(pool, pool.Status.CurrentReplicas)
			poolStatus.Lifecycle, poolStatus.AdmissionState = corev1alpha1.RuntimePoolLifecycleDraining, corev1alpha1.RuntimePoolAdmissionDraining
			poolStatus.Message = "waiting for the linked workspace suspension intent"
			r.setRuntimePoolCondition(pool, &poolStatus, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, poolStatus.Message)
			return r.finishRuntimePoolStatus(ctx, pool, poolStatus, time.Second)
		}
		suspend := substrateWorkspaceSuspendRequested(pool)
		ready, result, err := r.drainNativeSubstrateRuntime(ctx, pool, cfg, cm, record, actor, suspend)
		if err != nil || !ready {
			return result, err
		}
		if suspend {
			return r.beginNativeSubstrateCheckpoint(ctx, pool, cm, record, actor)
		}
		record.Phase, record.AfterStop = substrateNativeStopping, "Stopped"
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "runtime is quiescent; stopping its native Actor")
	}
	if record.RecycleRequested && record.Attempt != nil {
		suspend := substrateRuntimePoolSuspendCapable(pool) && record.Attempt.BootID != ""
		ready, result, err := r.drainNativeSubstrateRuntime(ctx, pool, cfg, cm, record, actor, suspend)
		if err != nil || !ready {
			return result, err
		}
		if suspend {
			return r.beginNativeSubstrateCheckpoint(ctx, pool, cm, record, actor)
		}
		record.Phase, record.AfterStop, record.RecycleRequested = substrateNativeStopping, substrateNativeProvisioning, false
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStopping, "native runtime rotation is quiescent; replacing its Actor")
	}
	if !r.SubstrateEnabled {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("native Substrate is disabled; runtime admission is closed"))
	}
	if !r.SubstrateConfig.DirectEgressEnabled {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("native ACP requires --substrate-direct-egress-enabled=true after configuring ateapi with --egress-gateway-address=; worker NetworkPolicies cannot confine transparent gateway destinations"))
	}
	if pool.Spec.ExecutionWorkspace.Substrate.RestoreFrom != nil && record.OriginDigest == "" {
		if err := r.importNativeSubstrateCheckpoint(ctx, pool, cm, record); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "retained the explicitly selected Data checkpoint before fresh Actor creation")
	}
	if err := r.ensureRuntimePoolNamespace(ctx, cfg); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	auth, provider, err := r.ensureRuntimePoolSecrets(ctx, pool, cfg)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if record.Attempt == nil {
		rotating, err := r.rotateConsumedWorkspaceRuntimePoolAuthSecret(ctx, pool, cfg, auth)
		if err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if rotating {
			return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "rotating credentials before a fresh native Actor boot")
		}
		suffix, err := r.randomHex(12)
		if err != nil {
			return ctrl.Result{}, err
		}
		record.Attempt = &substrateNativeAttempt{Name: runtimePoolChildName(cfg.baseName, "actor-"+suffix), StartedAt: metav1.NewTime(r.now())}
		record.Phase = substrateNativeProvisioning
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "recorded a unique native Actor creation intent")
	}
	// The journal and consent annotation are separate writes. Retry removal for
	// every running attempt, including recovery after a failed patch or restart.
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateNativeCheckpointConsent, ""); err != nil {
		return ctrl.Result{}, err
	}
	a := record.Attempt
	startedAt := a.StartedAt
	if !a.BootStartedAt.IsZero() {
		startedAt = a.BootStartedAt
	}
	if a.BootID == "" && nativeSubstrateRecoveryExpired(startedAt, r.now(), r.SubstrateConfig.WithDefaults().ClaimTimeout) {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native provisioning exceeded its recovery window; the creation intent is preserved for exact cleanup and explicit recovery")
	}
	template, desired, err := r.nativeSubstrateDesiredTemplate(ctx, pool, cfg, record, auth)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if template != nil {
		observedRevision, err := substrateRuntimeTemplateIntegrity(template)
		if err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if observedRevision != desired.revision {
			if a.CreateIssued && a.UID == "" {
				return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "runtime configuration changed while Actor creation was ambiguous; refusing to change or replay its immutable inputs")
			}
			if a.UID != "" {
				// Reuse the retirement path, which checkpoints only admitted
				// runtimes and preserves queued demand for their replacement.
				record.RecycleRequested = true
				if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
					return ctrl.Result{}, err
				}
				poolStatus := r.baseRuntimePoolStatus(pool, pool.Status.CurrentReplicas)
				poolStatus.Lifecycle, poolStatus.AdmissionState = corev1alpha1.RuntimePoolLifecycleDraining, corev1alpha1.RuntimePoolAdmissionDraining
				poolStatus.Message = "runtime template changed; retiring the current native Actor before replacement"
				r.setRuntimePoolCondition(pool, &poolStatus, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, poolStatus.Message)
				return r.finishRuntimePoolStatus(ctx, pool, poolStatus, time.Second)
			}
			if record.Checkpoint != nil && !nativeSubstrateCompatibleRestore(template, desired.object) {
				return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "runtime infrastructure changed beyond boot identity; preserving the checkpoint instead of restoring under another contract")
			}
			if err := r.substrateTemplates().Update(ctx, template, desired.object); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
			a.Template, a.CreateTemplate = substrateNativeTemplateRevision{}, substrateNativeTemplateRevision{}
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, err
			}
			return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "committed a fresh immutable native template revision")
		}
	} else {
		if record.Checkpoint != nil {
			if err := r.verifyNativeSubstrateRestoreLayout(ctx, pool, record, desired.object); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
		}
		if err := r.substrateTemplates().Create(ctx, pool, desired.object); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "created the immutable native runtime template")
	}
	if err := r.recordSubstrateRuntimePoolWorkerPlacement(ctx, pool, template); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	changed, err := r.ensureSubstrateRuntimePoolNetworkPolicies(ctx, pool, cfg, template)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if changed {
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "waiting for native WorkerPool network confinement")
	}
	// A template commit can succeed even when the following journal update
	// conflicts. Before creation starts, rebind that same intent to the committed
	// desired revision. Once Create was issued its inputs are immutable.
	if a.Template.UID == "" || (!a.CreateIssued && a.UID == "" && a.Template.UID != string(template.GetUID())) {
		_, binding, err := (&nativeSubstrateTemplateStore{r: r}).read(ctx, record.Atespace, template.GetName())
		if err != nil {
			return ctrl.Result{}, err
		}
		if binding == nil || binding.OwnerUID != record.PoolUID || binding.Current.UID == "" {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("native template revision has no committed owner"))
		}
		a.Template, a.CreateTemplate = binding.Current, binding.Current
		if record.Checkpoint != nil {
			a.CreateTemplate = record.Checkpoint.Template
		}
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "bound native Actor creation to an immutable template UID")
	}
	for _, revision := range []substrateNativeTemplateRevision{a.CreateTemplate, a.Template} {
		if err := verifyNativeSubstrateTemplate(ctx, api.Control, record.Atespace, revision); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
	}
	if actor == nil {
		if record.Checkpoint != nil {
			if err := verifiedNativeSubstrateCheckpoint(ctx, api.Control, record.Atespace, record.Checkpoint); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
		}
		if !a.CreateIssued {
			a.CreateIssued = true
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, err
			}
		}
		create := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: record.Atespace, Name: a.Name}, ActorTemplate: &ateapipb.ObjectRef{Atespace: record.Atespace, Name: a.CreateTemplate.Name}}
		if record.Checkpoint != nil {
			create.SourceTag = &ateapipb.ObjectRef{Atespace: record.Atespace, Name: record.Checkpoint.Name}
		}
		actor, err = api.Control.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: create})
		if err != nil {
			switch status.Code(err) {
			case codes.InvalidArgument, codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition:
				a.CreateIssued = false
				if saveErr := r.saveNativeSubstrateState(ctx, cm, record); saveErr != nil {
					return ctrl.Result{}, saveErr
				}
			}
			// Retry the SAME never-deleted creation name and immutable inputs.
			// AlreadyExists is recovered by GET on the next reconciliation.
			if status.Code(err) == codes.AlreadyExists {
				return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "recovering the existing Actor from its recorded creation intent")
			}
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("native Actor creation is pending recovery: %w", err))
		}
	}
	if err := validateNativeSubstrateActor(record, actor); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if a.UID == "" {
		if actor.GetActorTemplate().GetName() != a.CreateTemplate.Name {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New("created native Actor has an unexpected template"))
		}
		if record.Checkpoint != nil {
			if !proto.Equal(actor.GetSourceTag(), &ateapipb.ObjectRef{Atespace: record.Atespace, Name: record.Checkpoint.Name}) ||
				actor.GetStatus().GetCurrentActorTemplateUid() != record.Checkpoint.Template.UID || actor.GetStatus().GetExternalSnapshot().GetContentScope() != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA {
				return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "restored Actor does not identify the expected immutable Data Tag and template")
			}
		}
		a.UID = actor.GetMetadata().GetUid()
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "recorded native Actor UID after creation")
	}
	if actor.GetActorTemplate().GetName() != a.Template.Name {
		if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED || a.BootRequested {
			return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native restore left the suspended state before bootstrap template rotation")
		}
		update := proto.Clone(actor).(*ateapipb.Actor)
		update.ActorTemplate = &ateapipb.ObjectRef{Atespace: record.Atespace, Name: a.Template.Name}
		if _, err := api.Control.UpdateActor(ctx, &ateapipb.UpdateActorRequest{Actor: update}); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "rotated the suspended Actor template using native UID/version preconditions")
	}
	if !a.BootRequested {
		if a.BootStartedAt.IsZero() {
			a.BootStartedAt = metav1.NewTime(r.now())
		}
		a.BootRequested = true
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		_, err := api.Control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: nativeSubstrateActorRef(record), Boot: record.Checkpoint == nil})
		if err != nil {
			if nativeSubstrateControlAuthenticationRejected(err) {
				a.BootRequested = false
				if saveErr := r.saveNativeSubstrateState(ctx, cm, record); saveErr != nil {
					return ctrl.Result{}, saveErr
				}
			}
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("native Actor boot awaits observation: %w", err))
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "native Actor cold boot requested; waiting for its exact worker")
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		if actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_CRASHED || a.BootID != "" {
			return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native Actor stopped outside the recorded lifecycle; admission is closed and uncertain work is never replayed")
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "waiting for the recorded native cold boot; the boot request is not replayed")
	}
	if actor.GetStatus().GetCurrentActorTemplateUid() != a.Template.UID {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "running Actor was booted with another native template UID")
	}
	worker, err := r.nativeSubstrateWorker(ctx, api.Control, pool, actor)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if a.Worker == nil {
		a.Worker = worker
		if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
			return ctrl.Result{}, err
		}
		return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "recorded the exact native worker before credential delivery")
	}
	if *a.Worker != *worker {
		return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native worker placement changed after boot; refusing credential reuse")
	}
	if err := r.bindWorkspaceRuntimePoolBootstrapInstance(ctx, pool, auth, types.UID(a.UID+":"+a.Worker.PodUID)); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	view := nativeSubstrateRuntimeActorView(actor)
	route := substrateActorRouteHost(workspace.SubstrateActorKey(record.Atespace, a.Name), r.SubstrateConfig.ActorDNSSuffix)
	if !a.Seeded {
		complete, err := r.seedNativeSubstrateRuntime(ctx, api.Control, pool, cm, record, actor, route, auth, provider)
		if err != nil {
			if errors.Is(err, errSubstrateCredentialFenceConflict) || errors.Is(err, errSubstrateCredentialConflict) {
				return r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native bootstrap process changed or rejected its credential binding")
			}
			return r.nativeSubstrateProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleStarting, "waiting for process-bound native credential bootstrap")
		}
		if !complete {
			a.Seeded = true
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	synthetic := substrateSyntheticInstancePod(pool, cfg, view, a.Name, route)
	poolStatus := r.baseRuntimePoolStatus(pool, 1)
	r.setRuntimePoolCondition(pool, &poolStatus, corev1alpha1.RuntimePoolConditionPodSecurityReady, metav1.ConditionTrue, "ProviderIsolated", "native gVisor Actor uses worker NetworkPolicies with operator-configured direct egress and process identity isolation")
	r.setRuntimePoolCondition(pool, &poolStatus, corev1alpha1.RuntimePoolConditionQuotaReady, metav1.ConditionTrue, "ResourcesAdmitted", "native worker admitted runtime resource limits")
	postProbe := func(ctx context.Context, active *corev1alpha1.RuntimePoolActiveInstanceStatus) (ctrl.Result, bool, error) {
		current, err := getNativeSubstrateActor(ctx, api.Control, record)
		if err != nil || current == nil || current.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING || current.GetStatus().GetCurrentActorTemplateUid() != a.Template.UID || current.GetStatus().GetWorkerAssignment().GetWorkerPodUid() != a.Worker.PodUID {
			result, finishErr := r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native Actor identity changed during authenticated runtime admission")
			return result, true, finishErr
		}
		if a.BootID != "" && a.BootID != active.BootID {
			result, finishErr := r.failNativeSubstrateRuntime(ctx, pool, cfg, cm, record, "native supervisor restarted; credentials and uncertain work cannot be reused")
			return result, true, finishErr
		}
		if a.BootID == "" {
			a.BootID, a.Seeded, record.Phase = active.BootID, true, substrateNativeServing
			if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
				return ctrl.Result{}, true, err
			}
			if substrateRuntimePoolSuspendCapable(pool) {
				if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateNativeDataProtection, nativeSubstrateRecordID(record)); err != nil {
					return ctrl.Result{}, true, err
				}
			}
		}
		return ctrl.Result{}, false, nil
	}
	return r.reconcileRuntimePoolServingWithPostProbeFence(ctx, pool, cfg, []corev1.Pod{*synthetic}, []corev1.Pod{*synthetic}, auth, poolStatus, postProbe)
}

func nativeSubstrateControlAuthenticationRejected(err error) bool {
	if status.Code(err) != codes.Unauthenticated {
		return false
	}
	// These are the pinned ate-api authentication interceptor's pre-handler
	// rejections. Lifecycle operations can also propagate worker authentication
	// failures after mutation; their wrapped errors must not authorize a replay.
	message := status.Convert(err).Message()
	return message == "missing bearer token" || message == "invalid bearer token" ||
		(strings.HasPrefix(message, "token issuer ") && strings.HasSuffix(message, " not trusted"))
}

func (r *RuntimePoolReconciler) nativeSubstrateDesiredTemplate(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, record *substrateNativeState, auth *corev1.Secret) (*unstructured.Unstructured, substrateRuntimeTemplateRender, error) {
	store := r.substrateTemplates()
	previous, err := store.Get(ctx, record.Atespace, runtimePoolSubstrateTemplateName(cfg.baseName))
	if err != nil {
		return nil, substrateRuntimeTemplateRender{}, err
	}
	base, err := store.Get(ctx, record.Atespace, pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateName)
	if err != nil {
		return nil, substrateRuntimeTemplateRender{}, err
	}
	if base == nil {
		return nil, substrateRuntimeTemplateRender{}, fmt.Errorf("native Substrate infrastructure ActorTemplate is not found")
	}
	nonce := strings.TrimSpace(string(auth.Data[runtimePoolBootstrapNonceKey]))
	if nonce == "" {
		return nil, substrateRuntimeTemplateRender{}, fmt.Errorf("native runtime bootstrap nonce is missing")
	}
	key, err := harnessv2.CredentialBootstrapPublicKey(auth.Data[runtimePoolBootstrapSigningSeedKey])
	if err != nil {
		return nil, substrateRuntimeTemplateRender{}, err
	}
	desired, err := r.renderSubstrateRuntimeTemplate(pool, cfg, base, record.Atespace, record.Attempt.Name, nonce, key)
	return previous, desired, err
}

func nativeSubstrateCompatibleRestore(previous, desired *unstructured.Unstructured) bool {
	// Native continuation creates a new Actor and therefore new synthetic Pod
	// identity. This is the only additional difference beyond boot credentials.
	normalize := func(in *unstructured.Unstructured) *unstructured.Unstructured {
		out := in.DeepCopy()
		containers, _, _ := unstructured.NestedSlice(out.Object, substrateObjectSpecField, "containers")
		for _, item := range containers {
			container, _ := item.(map[string]any)
			envs, _ := container["env"].([]any)
			for _, item := range envs {
				env, _ := item.(map[string]any)
				if env[substrateNativeObjectName] == substrateNativePodUIDEnv || env[substrateNativeObjectName] == substrateNativePodNameEnv {
					env["value"] = ""
				}
			}
		}
		_ = unstructured.SetNestedSlice(out.Object, containers, substrateObjectSpecField, "containers")
		return out
	}
	return substrateRuntimeTemplateChangeIsBootstrapOnly(normalize(previous), normalize(desired))
}

func nativeSubstrateRuntimeActorView(actor *ateapipb.Actor) *workspace.SubstrateRuntimeActor {
	assignment := actor.GetStatus().GetWorkerAssignment()
	return &workspace.SubstrateRuntimeActor{
		Atespace: actor.GetMetadata().GetAtespace(), ActorID: actor.GetMetadata().GetName(), ActorUID: actor.GetMetadata().GetUid(), ActorVersion: actor.GetMetadata().GetVersion(),
		TemplateName: actor.GetActorTemplate().GetName(), TemplateNamespace: actor.GetActorTemplate().GetAtespace(), TemplateUID: actor.GetStatus().GetCurrentActorTemplateUid(),
		Status: "STATUS_" + strings.TrimPrefix(actor.GetStatus().GetState().String(), "ACTOR_STATE_"), PodName: assignment.GetWorkerPod(), PodNamespace: assignment.GetWorkerNamespace(), PodUID: assignment.GetWorkerPodUid(), PodIP: assignment.GetWorkerPodIp(),
	}
}

func (r *RuntimePoolReconciler) seedNativeSubstrateRuntime(
	ctx context.Context, api ateapipb.ControlClient, pool *corev1alpha1.RuntimePool,
	cm *corev1.ConfigMap, record *substrateNativeState, actor *ateapipb.Actor,
	route string, auth, provider *corev1.Secret,
) (bool, error) {
	request := harnessv2.CredentialBootstrapRequest{ControllerToken: string(auth.Data[runtimePoolControllerTokenKey]), CapabilitySecret: string(auth.Data[runtimePoolCapabilitySecretKey]), ProviderToken: string(provider.Data[runtimePoolProviderTokenKey])}
	if err := request.Validate(); err != nil {
		return false, err
	}
	nonce, seed := string(auth.Data[runtimePoolBootstrapNonceKey]), auth.Data[runtimePoolBootstrapSigningSeedKey]
	if r.SubstrateCredentialSeeder != nil {
		err := r.SubstrateCredentialSeeder(ctx, route, nonce, seed, request)
		if errors.Is(err, errSubstrateCredentialAlreadyComplete) {
			return true, nil
		}
		return false, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	transport, err := r.substrateSupervisorHTTPClient()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, runtimePoolProbeTimeout)
	defer cancel()
	identity := harnessv2.SubstrateActorIdentity{Atespace: record.Atespace, Name: record.Attempt.Name, UID: actor.GetMetadata().GetUid()}
	return seedSealedSubstrateCredentials(ctx, transport, "http://"+route+harnessv2.CredentialBootstrapPath, nonce, seed, body, identity, func(challenge harnessv2.SealedBootstrapChallenge) error {
		// SystemInfo exposes Actor identity, not worker identity. Recheck the
		// authoritative placement AFTER receiving the process key and BEFORE
		// committing it or sending credentials. Version equality rejects an
		// assignment ABA; worker and Pod reads retain the exact placement fence.
		// A subsequent reroute cannot decrypt this process's sealed payload.
		observed, err := getNativeSubstrateActor(ctx, api, record)
		if err != nil {
			return err
		}
		if observed == nil || observed.GetMetadata().GetVersion() != actor.GetMetadata().GetVersion() ||
			observed.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING ||
			observed.GetStatus().GetCurrentActorTemplateUid() != record.Attempt.Template.UID ||
			!proto.Equal(observed.GetStatus().GetWorkerAssignment(), actor.GetStatus().GetWorkerAssignment()) {
			return errSubstrateCredentialFenceConflict
		}
		worker, err := r.nativeSubstrateWorker(ctx, api, pool, observed)
		if err != nil {
			return err
		}
		if record.Attempt.Worker == nil || *record.Attempt.Worker != *worker {
			return errSubstrateCredentialFenceConflict
		}
		data, err := json.Marshal(challenge)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if existing := record.Attempt.BootstrapChallenge; existing != "" {
			if existing != digest {
				return errSubstrateCredentialFenceConflict
			}
			return nil
		}
		record.Attempt.BootstrapChallenge = digest
		return r.saveNativeSubstrateState(ctx, cm, record)
	})
}

func (r *RuntimePoolReconciler) nativeSubstrateProgress(ctx context.Context, pool *corev1alpha1.RuntimePool, lifecycle corev1alpha1.RuntimePoolLifecycle, message string) (ctrl.Result, error) {
	poolStatus := r.baseRuntimePoolStatus(pool, 0)
	poolStatus.Lifecycle, poolStatus.AdmissionState, poolStatus.Message = lifecycle, corev1alpha1.RuntimePoolAdmissionClosed, message
	poolStatus.ActiveInstance = nil
	r.setRuntimePoolCondition(pool, &poolStatus, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, message)
	return r.finishRuntimePoolStatus(ctx, pool, poolStatus, time.Second)
}

func (r *RuntimePoolReconciler) finishNativeSubstrateStopped(ctx context.Context, pool *corev1alpha1.RuntimePool, message string) (ctrl.Result, error) {
	poolStatus := r.baseRuntimePoolStatus(pool, 0)
	poolStatus.ActiveInstance = nil
	poolStatus.Lifecycle, poolStatus.AdmissionState, poolStatus.Message = corev1alpha1.RuntimePoolLifecycleStopped, corev1alpha1.RuntimePoolAdmissionClosed, message
	r.setRuntimePoolCondition(pool, &poolStatus, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, message)
	r.setRuntimePoolCondition(pool, &poolStatus, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "ScaledToZero", message)
	return r.finishRuntimePoolStatus(ctx, pool, poolStatus, runtimePoolRequeue)
}

func (r *RuntimePoolReconciler) failNativeSubstrateRuntime(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, cm *corev1.ConfigMap, record *substrateNativeState, reason string) (ctrl.Result, error) {
	record.Failure, record.Phase = reason, substrateNativeFailed
	if err := r.saveNativeSubstrateState(ctx, cm, record); err != nil {
		return ctrl.Result{}, err
	}
	if substrateRuntimePoolSuspendCapable(pool) {
		if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, runtimePoolWorkspaceResumeLostAnnotation, reason); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, errors.New(reason))
}
