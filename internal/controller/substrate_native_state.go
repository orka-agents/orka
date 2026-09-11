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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	substrateOwnedLabelValue         = "true"
	substrateNativeReady             = "Ready"
	substrateNativeObjectName        = "name"
	substrateNativeObjectUID         = "uid"
	substrateNativeObjectNamespace   = "namespace"
	substrateNativeIdentityVolume    = "orka-substrate-identity"
	substrateNativeAllCapabilities   = "ALL"
	substrateNativePodUIDEnv         = "ORKA_ACP_POD_UID"
	substrateNativePodNameEnv        = "ORKA_ACP_POD_NAME"
	substrateNativeStateSchema       = "orka.substrate-runtime.v1"
	substrateNativeStateKey          = "runtime.json"
	substrateNativeCheckpointConsent = "orka.ai/substrate-native-checkpoint"
	substrateNativeDataProtection    = "orka.ai/substrate-native-durable-state"
	substrateNativeJournalAnnotation = "orka.ai/substrate-native-journal"
	substrateNativeJournalRequired   = "required"
	substrateNativeJournalReleased   = "released"

	substrateNativeProvisioning  = "Provisioning"
	substrateNativeServing       = "Serving"
	substrateNativeCheckpointing = "Checkpointing"
	substrateNativeStopping      = "Stopping"
	substrateNativeSuspended     = "Suspended"
	substrateNativeFailed        = "Failed"
)

var errNativeSubstrateJournalMissing = errors.New("native Substrate lifecycle journal is missing; restore the original journal or complete operator recovery before continuing")

// These are controller-owned journal records, not provider operation receipts.
// Upstream lifecycle mutations do not take caller UID/version preconditions.
// Never expose these native identifiers or snapshot locations in Task status.
// Native names are random and never reused after deletion. Recovery checks
// immutable native identities and opens runtime admission only after a fresh,
// process-bound credential exchange and authenticated supervisor probe.
type substrateNativeState struct {
	Schema           string                              `json:"schema"`
	PoolUID          string                              `json:"poolUID"`
	Atespace         string                              `json:"atespace"`
	Phase            string                              `json:"phase"`
	AfterStop        string                              `json:"afterStop,omitempty"`
	RecycleRequested bool                                `json:"recycleRequested,omitempty"`
	OriginDigest     string                              `json:"originDigest,omitempty"`
	Attempt          *substrateNativeAttempt             `json:"attempt,omitempty"`
	Pending          *substrateNativeCheckpointOperation `json:"pending,omitempty"`
	Checkpoint       *substrateNativeCheckpoint          `json:"checkpoint,omitempty"`
	RetiredTags      []substrateNativeCheckpoint         `json:"retiredTags,omitempty"`
	Failure          string                              `json:"failure,omitempty"`
}

type substrateNativeAttempt struct {
	Name               string                          `json:"name"`
	StartedAt          metav1.Time                     `json:"startedAt"`
	BootStartedAt      metav1.Time                     `json:"bootStartedAt,omitempty"`
	DrainStartedAt     metav1.Time                     `json:"drainStartedAt,omitempty"`
	UID                string                          `json:"uid,omitempty"`
	Template           substrateNativeTemplateRevision `json:"template"`
	CreateTemplate     substrateNativeTemplateRevision `json:"createTemplate"`
	CreateIssued       bool                            `json:"createIssued,omitempty"`
	BootRequested      bool                            `json:"bootRequested,omitempty"`
	Seeded             bool                            `json:"seeded,omitempty"`
	BootstrapChallenge string                          `json:"bootstrapChallenge,omitempty"`
	BootID             string                          `json:"bootID,omitempty"`
	Worker             *substrateNativeWorkerFence     `json:"worker,omitempty"`
	WorkerDrained      bool                            `json:"workerDrained,omitempty"`
	WorkloadAbsent     bool                            `json:"workloadAbsent,omitempty"`
	DeleteIssued       bool                            `json:"deleteIssued,omitempty"`
}

type substrateNativeWorkerFence struct {
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Pool      string `json:"pool"`
	Pod       string `json:"pod"`
	PodUID    string `json:"podUID"`
}

type substrateNativeCheckpointOperation struct {
	Name                 string      `json:"name"`
	SourceName           string      `json:"sourceName"`
	SourceUID            string      `json:"sourceUID"`
	TemplateUID          string      `json:"templateUID"`
	StartedAt            metav1.Time `json:"startedAt"`
	SuspendStartedAt     metav1.Time `json:"suspendStartedAt,omitempty"`
	TagStartedAt         metav1.Time `json:"tagStartedAt,omitempty"`
	SuspendIssued        bool        `json:"suspendIssued,omitempty"`
	PriorSnapshotDigest  string      `json:"priorSnapshotDigest,omitempty"`
	SourceVersion        int64       `json:"sourceVersion,omitempty"`
	SourceSnapshotDigest string      `json:"sourceSnapshotDigest,omitempty"`
	TagIssued            bool        `json:"tagIssued,omitempty"`
	TagUID               string      `json:"tagUID,omitempty"`
}

// A Tag is immutable once its external snapshot is populated. Digest includes
// the complete native Tag and its provenance, but only the digest is public.
type substrateNativeCheckpoint struct {
	Name       string                          `json:"name"`
	UID        string                          `json:"uid"`
	Digest     string                          `json:"digest"`
	SourceName string                          `json:"sourceName"`
	SourceUID  string                          `json:"sourceUID"`
	Template   substrateNativeTemplateRevision `json:"template"`
	CreatedAt  metav1.Time                     `json:"createdAt"`
}

func (r *RuntimePoolReconciler) usesNativeSubstrate() bool {
	return r.SubstrateTemplates == nil && r.SubstrateActorControlFactory == nil
}

func (r *RuntimePoolReconciler) substrateNativeStateObject(pool *corev1alpha1.RuntimePool) *corev1.ConfigMap {
	sum := sha256.Sum256([]byte(string(pool.UID)))
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: controllerNamespaceForRuntimePool(r.ControllerNamespace),
		Name:      "substrate-runtime-" + hex.EncodeToString(sum[:16]),
		Labels:    map[string]string{runtimePoolManagedByLabel: runtimePoolManagedByLabelValue, runtimePoolUIDLabel: string(pool.UID)},
	}}
}

func (r *RuntimePoolReconciler) readNativeSubstrateState(ctx context.Context, pool *corev1alpha1.RuntimePool) (*corev1.ConfigMap, *substrateNativeState, error) {
	if pool.UID == "" || pool.Spec.ExecutionWorkspace == nil || pool.Spec.ExecutionWorkspace.Substrate == nil {
		return nil, nil, fmt.Errorf("native Substrate journal requires an exact RuntimePool and infrastructure binding")
	}
	cm := r.substrateNativeStateObject(pool)
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		if apierrors.IsNotFound(err) {
			if err := validateAbsentNativeSubstrateJournal(ctx, reader, pool); err != nil {
				return nil, nil, err
			}
			return cm, nil, nil
		}
		return nil, nil, err
	}
	record := &substrateNativeState{}
	if err := json.Unmarshal([]byte(cm.Data[substrateNativeStateKey]), record); err != nil ||
		record.Schema != substrateNativeStateSchema || record.PoolUID != string(pool.UID) ||
		record.Atespace != pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateNamespace ||
		cm.Labels[runtimePoolUIDLabel] != string(pool.UID) || cm.Labels[runtimePoolManagedByLabel] != runtimePoolManagedByLabelValue {
		return nil, nil, fmt.Errorf("native Substrate journal ownership or schema is invalid")
	}
	return cm, record, nil
}

func validateAbsentNativeSubstrateJournal(ctx context.Context, reader client.Reader, pool *corev1alpha1.RuntimePool) error {
	// Journal reads bypass the cache. Read the pool marker the same way so a
	// delayed pool watch cannot turn journal loss into a fresh provisioning run.
	current := &corev1alpha1.RuntimePool{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(pool), current); err != nil {
		return err
	}
	if current.UID != pool.UID {
		return fmt.Errorf("native Substrate journal lookup belongs to a replaced RuntimePool")
	}
	marker := current.Annotations[substrateNativeJournalAnnotation]
	if marker == substrateNativeJournalReleased && !current.DeletionTimestamp.IsZero() {
		return nil
	}
	if marker != "" || current.Status.ActiveInstance != nil || current.Annotations[runtimePoolWorkspaceResumeLostAnnotation] != "" {
		return errNativeSubstrateJournalMissing
	}
	for key, value := range current.Annotations {
		// These controller-owned annotations include both the old provider
		// lifecycle and native checkpoint/placement records. Neither may be
		// silently adopted as an empty native workspace after an upgrade.
		if strings.HasPrefix(key, "orka.ai/substrate-") && value != "" {
			return errNativeSubstrateJournalMissing
		}
	}
	return nil
}

func (r *RuntimePoolReconciler) markNativeSubstrateJournal(ctx context.Context, pool *corev1alpha1.RuntimePool, marker string) error {
	if pool.Annotations[substrateNativeJournalAnnotation] == marker {
		return nil
	}
	base := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[substrateNativeJournalAnnotation] = marker
	return r.Patch(ctx, pool, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *RuntimePoolReconciler) saveNativeSubstrateState(ctx context.Context, cm *corev1.ConfigMap, record *substrateNativeState) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(data) > 256<<10 {
		return fmt.Errorf("native Substrate journal exceeds its recovery record limit")
	}
	cm.Data = map[string]string{substrateNativeStateKey: string(data)}
	if cm.ResourceVersion == "" {
		return r.Create(ctx, cm)
	}
	// ResourceVersion is the controller journal's CAS, not a provider fence.
	return r.Update(ctx, cm)
}

func nativeSubstrateActorRef(record *substrateNativeState) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: record.Atespace, Name: record.Attempt.Name}
}

func nativeSubstrateSnapshotDigest(snapshot *ateapipb.ExternalSnapshot) string {
	if snapshot == nil {
		return ""
	}
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(snapshot)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func nativeSubstrateTagDigest(tag *ateapipb.Tag) (string, error) {
	if tag.GetMetadata().GetUid() == "" || tag.GetStatus().GetSnapshot().GetSnapshotUri() == "" ||
		tag.GetStatus().GetSnapshot().GetContentScope() != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA ||
		tag.GetStatus().GetSourceActorUid() == "" || tag.GetStatus().GetActorTemplateUid() == "" {
		return "", fmt.Errorf("native Substrate Tag has no complete immutable Data snapshot provenance")
	}
	// Ignore mutable visibility/version/timestamps. Scope is separately required
	// to stay private; the identity and snapshot itself cannot change.
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(&ateapipb.Tag{
		Metadata:    &ateapipb.ResourceMetadata{Atespace: tag.GetMetadata().GetAtespace(), Name: tag.GetMetadata().GetName(), Uid: tag.GetMetadata().GetUid()},
		SourceActor: tag.GetSourceActor(), Status: tag.GetStatus(),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateNativeSubstrateActor(record *substrateNativeState, actor *ateapipb.Actor) error {
	a := record.Attempt
	if a == nil || actor == nil || actor.GetMetadata().GetAtespace() != record.Atespace || actor.GetMetadata().GetName() != a.Name || actor.GetMetadata().GetUid() == "" {
		return fmt.Errorf("native Substrate Actor does not match its controller creation intent")
	}
	if a.UID != "" && actor.GetMetadata().GetUid() != a.UID {
		return fmt.Errorf("native Substrate Actor UID changed; refusing to modify the replacement")
	}
	ref := actor.GetActorTemplate()
	if ref.GetAtespace() != record.Atespace || (ref.GetName() != a.Template.Name && ref.GetName() != a.CreateTemplate.Name) {
		return fmt.Errorf("native Substrate Actor template differs from its committed revision")
	}
	return nil
}

func getNativeSubstrateActor(ctx context.Context, api ateapipb.ControlClient, record *substrateNativeState) (*ateapipb.Actor, error) {
	if record.Attempt == nil {
		return nil, nil
	}
	actor, err := api.GetActor(ctx, &ateapipb.GetActorRequest{Actor: nativeSubstrateActorRef(record)})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get native Substrate Actor: %w", err)
	}
	if err := validateNativeSubstrateActor(record, actor); err != nil {
		return nil, err
	}
	return actor, nil
}

func (r *RuntimePoolReconciler) nativeSubstrateWorker(ctx context.Context, api ateapipb.ControlClient, pool *corev1alpha1.RuntimePool, actor *ateapipb.Actor) (*substrateNativeWorkerFence, error) {
	assignment := actor.GetStatus().GetWorkerAssignment()
	if assignment.GetWorker().GetName() == "" || assignment.GetWorkerPodUid() == "" {
		return nil, fmt.Errorf("native Substrate Actor has no exact worker assignment")
	}
	worker, err := api.GetWorker(ctx, &ateapipb.GetWorkerRequest{Worker: assignment.GetWorker()})
	if err != nil {
		return nil, err
	}
	namespace, workerPool, err := substrateRuntimePoolWorkerPlacementFromAnnotation(pool)
	if err != nil {
		return nil, err
	}
	if worker.GetMetadata().GetUid() == "" || worker.GetWorkerNamespace() != namespace || worker.GetWorkerPool() != workerPool ||
		worker.GetWorkerPodUid() != assignment.GetWorkerPodUid() || worker.GetWorkerPod() != assignment.GetWorkerPod() ||
		worker.GetStatus().GetCapacity().GetActors() != 1 {
		return nil, fmt.Errorf("native Substrate requires an exact worker with capacity for one Actor in the admitted WorkerPool")
	}
	pod := &corev1.Pod{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: worker.GetWorkerPod()}, pod); err != nil {
		return nil, err
	}
	if string(pod.UID) != worker.GetWorkerPodUid() || pod.DeletionTimestamp != nil || pod.Labels[substrateWorkerPoolLabel] != workerPool {
		return nil, fmt.Errorf("native Substrate worker Pod identity or placement changed")
	}
	return &substrateNativeWorkerFence{Name: worker.GetMetadata().GetName(), UID: worker.GetMetadata().GetUid(), Namespace: namespace, Pool: workerPool, Pod: pod.Name, PodUID: string(pod.UID)}, nil
}

// Drain prevents new assignments without suspending or evicting the current
// Actor. Check the exact worker after the unfenced native call, and enumerate
// its assignments before any Pod deletion. No other Actor may share it.
func drainNativeSubstrateWorker(ctx context.Context, api ateapipb.ControlClient, record *substrateNativeState) error {
	f := record.Attempt.Worker
	if f == nil {
		return fmt.Errorf("native Substrate worker fence is missing")
	}
	ref := &ateapipb.ObjectRef{Name: f.Name}
	worker, err := api.GetWorker(ctx, &ateapipb.GetWorkerRequest{Worker: ref})
	if err != nil {
		return err
	}
	verify := func(w *ateapipb.Worker) bool {
		return w.GetMetadata().GetUid() == f.UID && w.GetWorkerPodUid() == f.PodUID && w.GetWorkerNamespace() == f.Namespace && w.GetWorkerPod() == f.Pod
	}
	if !verify(worker) {
		return fmt.Errorf("native Substrate worker lifetime changed before drain")
	}
	if worker.GetStatus().GetState() != ateapipb.WorkerState_WORKER_STATE_DRAINING {
		worker, err = api.DrainWorker(ctx, &ateapipb.DrainWorkerRequest{Worker: ref})
		if err != nil {
			return err
		}
	}
	if !verify(worker) || worker.GetStatus().GetState() != ateapipb.WorkerState_WORKER_STATE_DRAINING {
		return fmt.Errorf("native Substrate worker drain did not preserve its exact lifetime")
	}
	page := ""
	seen := map[string]bool{}
	for {
		assignments, err := api.ListWorkerActorAssignments(ctx, &ateapipb.ListWorkerActorAssignmentsRequest{Worker: ref, PageSize: 1000, PageToken: page})
		if err != nil {
			return err
		}
		for _, assignment := range assignments.GetActorAssignments() {
			if assignment.GetActorUid() != record.Attempt.UID || !proto.Equal(assignment.GetActor(), nativeSubstrateActorRef(record)) {
				return fmt.Errorf("native Substrate worker has another Actor assignment; refusing shared-worker teardown")
			}
		}
		page = assignments.GetNextPageToken()
		if page == "" {
			return nil
		}
		if seen[page] {
			return fmt.Errorf("native Substrate worker assignment pagination repeated a token")
		}
		seen[page] = true
	}
}

func (r *RuntimePoolReconciler) terminateNativeSubstrateWorker(ctx context.Context, record *substrateNativeState) (bool, error) {
	a := record.Attempt
	if a.Worker == nil {
		return !a.BootRequested, nil
	}
	if !a.WorkerDrained {
		return false, fmt.Errorf("native Substrate worker must be drained before workload termination")
	}
	f := a.Worker
	pod := &corev1.Pod{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	err := reader.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: f.Pod}, pod)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if string(pod.UID) != f.PodUID {
		return true, nil
	}
	if pod.DeletionTimestamp != nil {
		return false, nil
	}
	if pod.Labels[substrateWorkerPoolLabel] != f.Pool {
		return false, fmt.Errorf("native Substrate worker Pod ownership changed")
	}
	return false, client.IgnoreNotFound(r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}))
}

func nativeSubstrateOperationExpired(operation *substrateNativeCheckpointOperation, now time.Time, timeout time.Duration) bool {
	if operation == nil {
		return false
	}
	startedAt := operation.StartedAt
	if !operation.SuspendStartedAt.IsZero() {
		startedAt = operation.SuspendStartedAt
	}
	if !operation.TagStartedAt.IsZero() {
		startedAt = operation.TagStartedAt
	}
	return nativeSubstrateRecoveryExpired(startedAt, now, timeout)
}

func nativeSubstrateRecoveryExpired(startedAt metav1.Time, now time.Time, timeout time.Duration) bool {
	// Lifecycle RPCs may take five minutes. Leave time to observe their result
	// afterward, even when the configured claim timeout is shorter.
	return !startedAt.IsZero() && now.Sub(startedAt.Time) > max(timeout*3, 6*time.Minute)
}

func nativeSubstrateRecordID(record *substrateNativeState) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{record.Schema, record.PoolUID, record.Atespace}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type substrateNativeConsent struct {
	PoolUID          string `json:"poolUID"`
	RecordDigest     string `json:"recordDigest"`
	CheckpointDigest string `json:"checkpointDigest"`
}

func nativeSubstrateConsentAnnotation(record *substrateNativeState) string {
	if record.Checkpoint == nil {
		return ""
	}
	data, _ := json.Marshal(substrateNativeConsent{PoolUID: record.PoolUID, RecordDigest: nativeSubstrateRecordID(record), CheckpointDigest: record.Checkpoint.Digest})
	return string(data)
}

func nativeSubstrateConsentRecorded(pool *corev1alpha1.RuntimePool) bool {
	if !substrateRuntimePoolSuspendCapable(pool) {
		return false
	}
	consent := &substrateNativeConsent{}
	if json.Unmarshal([]byte(pool.Annotations[substrateNativeCheckpointConsent]), consent) != nil {
		return false
	}
	expected := nativeSubstrateRecordID(&substrateNativeState{Schema: substrateNativeStateSchema, PoolUID: string(pool.UID), Atespace: pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateNamespace})
	return pool.UID != "" && consent.PoolUID == string(pool.UID) && consent.RecordDigest == expected &&
		pool.Annotations[substrateNativeDataProtection] == expected && validSHA256Digest(consent.CheckpointDigest)
}
