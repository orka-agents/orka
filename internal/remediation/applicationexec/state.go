package applicationexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	stateVersion     = 1
	maxStateBytes    = 900 << 10
	maxEffects       = 512
	stateLabel       = "applicationexec.orka.ai/state"
	runLabel         = "applicationexec.orka.ai/run"
	roleLabel        = "applicationexec.orka.ai/role"
	inputAnnotation  = "applicationexec.orka.ai/input"
	ledgerAnnotation = "applicationexec.orka.ai/ledger"
	backendName      = "isolated-application"
	cleanupFinalizer = "applicationexec.orka.ai/cleanup"
	stateKey         = "state.json"
	clusterRoleKind  = "ClusterRole"
)

type effect struct {
	Key             string    `json:"key"`
	APIVersion      string    `json:"apiVersion"`
	Kind            string    `json:"kind"`
	Namespace       string    `json:"namespace,omitempty"`
	Name            string    `json:"name"`
	Digest          string    `json:"digest"`
	UID             types.UID `json:"uid,omitempty"`
	Attempted       bool      `json:"attempted"`
	Deleted         bool      `json:"deleted,omitempty"`
	Rejected        bool      `json:"rejected,omitempty"`
	Failure         string    `json:"failure,omitempty"`
	DeadlineSeconds int64     `json:"deadlineSeconds,omitempty"`
	Actor           bool      `json:"actor,omitempty"`
	GeneratedBy     types.UID `json:"generatedBy,omitempty"`
}

type podReceipt struct {
	Namespace string                          `json:"namespace"`
	JobUID    types.UID                       `json:"jobUID"`
	Identity  pv.ApplicationContainerIdentity `json:"identity"`
}

type taskReceipt struct {
	Name      string    `json:"name"`
	UID       types.UID `json:"uid,omitempty"`
	Attempted bool      `json:"attempted"`
	Rejected  bool      `json:"rejected,omitempty"`
}

type state struct {
	Version                  int                                           `json:"version"`
	Configuration            string                                        `json:"configuration"`
	InputDigest              string                                        `json:"inputDigest"`
	Request                  contract.ExecutionRequest                     `json:"request"`
	Qualification            bool                                          `json:"qualification,omitempty"`
	ControlUID               string                                        `json:"controlUID"`
	TargetUID                string                                        `json:"targetUID"`
	Target                   targetIdentity                                `json:"target"`
	CreatedAt                time.Time                                     `json:"createdAt"`
	Deadline                 time.Time                                     `json:"deadline"`
	Task                     taskReceipt                                   `json:"task"`
	ArmTasks                 map[string]taskReceipt                        `json:"armTasks"`
	Effects                  []effect                                      `json:"effects"`
	Pods                     map[string]podReceipt                         `json:"pods"`
	Manifest                 *pv.Manifest                                  `json:"manifest,omitempty"`
	Binding                  *pv.Binding                                   `json:"binding,omitempty"`
	RecordCreated            bool                                          `json:"recordCreated,omitempty"`
	Artifacts                map[string]pv.ApplicationArtifact             `json:"artifacts"`
	Prepared                 []string                                      `json:"prepared,omitempty"`
	Observed                 []string                                      `json:"observed,omitempty"`
	Boundary                 map[string]string                             `json:"boundary"`
	BoundaryReports          map[string]pv.ApplicationBoundaryReport       `json:"boundaryReports"`
	AuthorizationDigest      string                                        `json:"authorizationDigest,omitempty"`
	ClusterReads             map[string][]clusterReadObject                `json:"clusterReads,omitempty"`
	SubjectTLS               map[string][]pv.ApplicationSubjectTLSIdentity `json:"subjectTLS,omitempty"`
	ScalingAdmission         map[string]string                             `json:"scalingAdmission,omitempty"`
	ReplicaTemplates         map[string]corev1.PodTemplateSpec             `json:"replicaTemplates,omitempty"`
	ReceiverRoutes           map[string]receiverRouteReceipt               `json:"receiverRoutes,omitempty"`
	APIBaselines             map[string]apiBaselineReceipt                 `json:"apiBaselines,omitempty"`
	Credentials              map[string]credentialReceipt                  `json:"credentials"`
	Actions                  map[string]actionReceipt                      `json:"actions"`
	Volumes                  []volumeReceipt                               `json:"volumes,omitempty"`
	DataFinalizationDeadline *time.Time                                    `json:"dataFinalizationDeadline,omitempty"`
	DataFinalizationBlocked  bool                                          `json:"dataFinalizationBlocked,omitempty"`
	Cancelled                bool                                          `json:"cancelled,omitempty"`
	Failed                   string                                        `json:"failed,omitempty"`
	Finished                 bool                                          `json:"finished,omitempty"`
	Settled                  bool                                          `json:"settled,omitempty"`
}

type run struct {
	object *corev1.ConfigMap
	state  state
}

func runName(request contract.ExecutionRequest) string {
	content, _ := json.Marshal([]string{request.Namespace, request.ParentTaskUID, request.ID, string(request.Action)})
	return "app-exec-" + strings.TrimPrefix(pv.Digest(content), "sha256:")[:32]
}

func (b *Backend) load(ctx context.Context, name string) (*run, error) {
	object := &corev1.ConfigMap{}
	if err := b.controlReader.Get(ctx, client.ObjectKey{Namespace: b.config.Namespace, Name: name}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, contract.ErrNotFound
		}
		return nil, safeError(contract.ErrUnavailable, err)
	}
	return decodeRun(object, b.config.Namespace)
}

func decodeRun(object *corev1.ConfigMap, namespace string) (*run, error) {
	r := &run{object: object}
	content := []byte(object.Data[stateKey])
	if object.UID == "" || object.Namespace != namespace || object.Labels[stateLabel] != "v1" ||
		len(object.OwnerReferences) != 0 ||
		len(content) == 0 || len(content) > maxStateBytes || json.Unmarshal(content, &r.state) != nil {
		return nil, contract.ErrConflict
	}
	st := &r.state
	if !validStateIdentity(object, st, namespace) || !validStateProgress(st) || !validEffectLedger(st.Effects) {
		return nil, contract.ErrConflict
	}
	return r, nil
}

func validStateIdentity(object *corev1.ConfigMap, st *state, namespace string) bool {
	return st.Version == stateVersion && st.Request.Namespace == namespace && object.Name == runName(st.Request) &&
		digestPattern.MatchString(st.Configuration) && digestPattern.MatchString(st.InputDigest) &&
		st.InputDigest == executionDigest(st.Configuration, st.Request) &&
		validUID(st.ControlUID) && validUID(st.TargetUID) && st.ControlUID != st.TargetUID &&
		st.Task.Name == object.Name && !st.CreatedAt.IsZero() && st.Deadline.After(st.CreatedAt) &&
		st.Target.NodeName != "" && st.Target.NodeUID != "" && st.Target.StorageClassUID != ""
}

func validStateProgress(st *state) bool {
	if st.Pods == nil || st.ArmTasks == nil ||
		st.Artifacts == nil || st.Boundary == nil || st.BoundaryReports == nil || st.Credentials == nil || st.Actions == nil ||
		(!st.Qualification && (st.Request.Deadline == nil || st.Request.Deadline.IsZero() || st.Deadline.After(*st.Request.Deadline))) ||
		!validTaskReceipts(st) ||
		(st.Settled && !st.Cancelled && !st.Finished && st.Failed == "") {
		return false
	}
	return true
}

func validTaskReceipts(st *state) bool {
	valid := func(receipt taskReceipt) bool {
		return (receipt.UID == "" || receipt.Attempted) &&
			(!receipt.Rejected || receipt.Attempted && receipt.UID == "")
	}
	if !valid(st.Task) {
		return false
	}
	for _, receipt := range st.ArmTasks {
		if !valid(receipt) {
			return false
		}
	}
	return true
}

func validEffectLedger(effects []effect) bool {
	if len(effects) > maxEffects {
		return false
	}
	keys := make(map[string]bool)
	identities := make(map[string]bool)
	for _, entry := range effects {
		identity := entry.APIVersion + "\x00" + entry.Kind + "\x00" + entry.Namespace + "\x00" + entry.Name
		if !validEffect(entry) || keys[entry.Key] || identities[identity] {
			return false
		}
		keys[entry.Key], identities[identity] = true, true
	}
	return true
}

func validEffect(entry effect) bool {
	if entry.Key == "" || entry.Name == "" || entry.Kind == "" || entry.APIVersion == "" ||
		!digestPattern.MatchString(entry.Digest) ||
		(entry.UID != "" && !entry.Attempted) || (entry.Deleted && entry.UID == "" && !entry.Rejected) {
		return false
	}
	return !entry.Rejected || (entry.UID == "" && entry.Attempted &&
		(entry.Failure == effectCreateRejected || entry.Failure == "not-dispatched"))
}

func (b *Backend) save(ctx context.Context, r *run) error {
	content, err := json.Marshal(r.state)
	if err != nil || len(content) > maxStateBytes {
		return contract.ErrInvalid
	}
	base := r.object.DeepCopy()
	next := r.object.DeepCopy()
	next.Data = map[string]string{stateKey: string(content)}
	if err := b.control.Patch(ctx, next, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	r.object = next
	return nil
}

func (b *Backend) fresh(ctx context.Context, r *run) error {
	current, err := b.load(ctx, r.object.Name)
	if err != nil {
		return err
	}
	if current.object.UID != r.object.UID || current.state.Task != r.state.Task ||
		current.state.InputDigest != r.state.InputDigest {
		return contract.ErrConflict
	}
	*r = *current
	return nil
}

func (b *Backend) parent(ctx context.Context, r *run) error {
	if r.state.Qualification {
		return nil
	}
	parent := &corev1alpha1.Task{}
	err := b.controlReader.Get(ctx, client.ObjectKey{Namespace: r.state.Request.Namespace, Name: r.state.Request.ParentTaskName}, parent)
	if err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	_, cancelled := parent.Annotations[labels.AnnotationCancelRequested]
	if string(parent.UID) != r.state.Request.ParentTaskUID || cancelled || !parent.DeletionTimestamp.IsZero() {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) authorizeEffect(ctx context.Context, r *run) error {
	if err := b.fresh(ctx, r); err != nil {
		return err
	}
	if r.state.Cancelled || r.state.Finished || r.state.Failed != "" || r.state.Settled ||
		!r.object.DeletionTimestamp.IsZero() ||
		r.state.Configuration != b.ConfigurationDigest() || !time.Now().Before(r.state.Deadline) {
		return contract.ErrConflict
	}
	if err := b.parent(ctx, r); err != nil {
		return err
	}
	if _, err := b.verifyTarget(ctx, &r.state.Target); err != nil {
		return err
	}
	if err := b.verifyProfile(ctx); err != nil {
		return err
	}
	if err := b.verifyClusterReadSet(ctx, r); err != nil {
		return err
	}
	if err := b.verifyOwnedTarget(ctx, r); err != nil {
		return err
	}
	return b.checkTask(ctx, r, false)
}

func (r *run) receipt() contract.ExecutionReceipt {
	return contract.ExecutionReceipt{Namespace: r.state.Request.Namespace, TaskName: r.state.Task.Name,
		TaskUID: string(r.state.Task.UID), InputDigest: r.state.InputDigest}
}

func (r *run) effect(key string) *effect {
	for i := range r.state.Effects {
		if r.state.Effects[i].Key == key {
			return &r.state.Effects[i]
		}
	}
	return nil
}

func (r *run) namedEffect(apiVersion, kind, namespace, name string) *effect {
	for index := range r.state.Effects {
		entry := &r.state.Effects[index]
		if entry.APIVersion == apiVersion && entry.Kind == kind && entry.Namespace == namespace && entry.Name == name {
			return entry
		}
	}
	return nil
}

func (b *Backend) kind(object client.Object) (schema.GroupVersionKind, error) {
	if untyped, ok := object.(*unstructured.Unstructured); ok {
		gvk := untyped.GroupVersionKind()
		if !b.profileKind(gvk) {
			return schema.GroupVersionKind{}, contract.ErrInvalid
		}
		return gvk, nil
	}
	kinds, _, err := b.scheme.ObjectKinds(object)
	if err != nil || len(kinds) != 1 || kinds[0].Kind == "" {
		return schema.GroupVersionKind{}, contract.ErrInvalid
	}
	return kinds[0], nil
}

func objectDigest(object client.Object) (string, error) {
	copy := object.DeepCopyObject().(client.Object)
	copy.SetResourceVersion("")
	copy.SetUID("")
	copy.SetCreationTimestamp(metav1.Time{})
	copy.SetManagedFields(nil)
	copy.SetGeneration(0)
	content, err := json.Marshal(copy)
	if err != nil {
		return "", contract.ErrInvalid
	}
	return pv.Digest(content), nil
}

// ensureObject records the attempt before crossing the API boundary. An intent
// without a UID is deliberately not a retry authorization: an API timeout may
// have committed the create. That ambiguity remains visible and blocks cleanup
// completion instead of adopting or deleting a potentially replaced object.
func (b *Backend) ensureObject(ctx context.Context, r *run, key string, expected client.Object) (client.Object, error) {
	if err := b.authorizeEffect(ctx, r); err != nil {
		return nil, err
	}
	if deployment, ok := expected.(*appsv1.Deployment); ok && !replicaRunDeadline(r, deployment.Spec.Template.Spec) {
		return nil, contract.ErrConflict
	}
	gvk, err := b.kind(expected)
	if err != nil || (len(expected.GetOwnerReferences()) != 0 && !targetOwnerMatches(r, expected)) {
		return nil, contract.ErrInvalid
	}
	objectLabels := expected.GetLabels()
	if objectLabels == nil {
		objectLabels = make(map[string]string)
	}
	objectLabels[runLabel] = string(r.object.UID)
	expected.SetLabels(objectLabels)
	expected.GetObjectKind().SetGroupVersionKind(gvk)
	digest, err := objectDigest(expected)
	if err != nil {
		return nil, err
	}
	entry := r.effect(key)
	if entry != nil {
		return b.existingEffect(ctx, r, *entry, expected, gvk, digest)
	}
	if len(r.state.Effects) >= maxEffects {
		return nil, contract.ErrInvalid
	}
	entry = &effect{Key: key, APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind,
		Namespace: expected.GetNamespace(), Name: expected.GetName(), Digest: digest, Attempted: true}
	if job, ok := expected.(*batchv1.Job); ok {
		if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds < 1 {
			return nil, contract.ErrInvalid
		}
		entry.DeadlineSeconds = *job.Spec.ActiveDeadlineSeconds
	}
	r.state.Effects = append(r.state.Effects, *entry)
	if err := b.save(ctx, r); err != nil {
		return nil, err
	}
	created := expected.DeepCopyObject().(client.Object)
	if err := b.target.Create(ctx, created); err != nil {
		// Record only a safe classification, not an API error containing a
		// submitted object, credential, synthetic marker, or admission payload.
		failure, kind := "create-outcome-unknown", ErrUncertain
		if definitivelyRejectedCreate(err) {
			failure, kind = effectCreateRejected, contract.ErrConflict
		}
		if recordErr := b.recordEffectResult(context.WithoutCancel(ctx), r, key, "", failure); recordErr != nil {
			return nil, errors.Join(safeError(ErrUncertain, err), recordErr)
		}
		return nil, safeError(kind, err)
	}
	if created.GetUID() == "" {
		return nil, ErrUncertain
	}
	baselines, baselineErr := b.apiBaselinesFromCreate(r, key, created)
	if err := b.recordEffectWithBaselines(context.WithoutCancel(ctx), r, key, created.GetUID(), "", baselines); err != nil {
		return nil, err
	}
	if baselineErr != nil {
		return nil, baselineErr
	}
	if !staticObjectMatches(expected, created) {
		return nil, ErrIsolation
	}
	return created, nil
}

func definitivelyRejectedCreate(err error) bool {
	return apierrors.IsAlreadyExists(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) ||
		apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsMethodNotSupported(err)
}

func (b *Backend) existingEffect(ctx context.Context, r *run, entry effect, expected client.Object,
	gvk schema.GroupVersionKind, digest string) (client.Object, error) {
	if entry.Kind != gvk.Kind || entry.APIVersion != gvk.GroupVersion().String() ||
		entry.Name != expected.GetName() || entry.Namespace != expected.GetNamespace() || entry.Digest != digest || entry.Deleted {
		return nil, contract.ErrConflict
	}
	if entry.UID == "" {
		if entry.Rejected {
			return nil, contract.ErrConflict
		}
		return nil, ErrUncertain
	}
	observed := expected.DeepCopyObject().(client.Object)
	if err := b.targetReader.Get(ctx, client.ObjectKeyFromObject(expected), observed); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, contract.ErrConflict
		}
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if observed.GetUID() != entry.UID || observed.GetLabels()[runLabel] != string(r.object.UID) ||
		!observed.GetDeletionTimestamp().IsZero() || !staticObjectMatches(expected, observed) {
		return nil, contract.ErrConflict
	}
	return observed, nil
}

func (b *Backend) recordEffectResult(ctx context.Context, r *run, key string, uid types.UID, failure string) error {
	return b.recordEffectWithBaselines(ctx, r, key, uid, failure, nil)
}

func (b *Backend) recordEffectWithBaselines(ctx context.Context, r *run, key string, uid types.UID, failure string,
	baselines map[string]apiBaselineReceipt) error {
	ctx, cancel := context.WithTimeout(ctx, b.config.CleanupTimeout)
	defer cancel()
	for range 4 {
		current, err := b.load(ctx, r.object.Name)
		if err != nil {
			return err
		}
		entry := current.effect(key)
		previous := r.effect(key)
		if current.object.UID != r.object.UID || entry == nil || previous == nil ||
			entry.Digest != previous.Digest || entry.Deleted || (entry.UID != "" && entry.UID != uid) {
			return contract.ErrConflict
		}
		entry.UID, entry.Failure, entry.Rejected = uid, failure, failure == effectCreateRejected
		for baselineKey, baseline := range baselines {
			if previous, found := current.state.APIBaselines[baselineKey]; found && !reflect.DeepEqual(previous, baseline) {
				return contract.ErrConflict
			}
			if current.state.APIBaselines == nil {
				current.state.APIBaselines = make(map[string]apiBaselineReceipt)
			}
			current.state.APIBaselines[baselineKey] = baseline
		}
		if err := b.save(ctx, current); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		*r = *current
		return nil
	}
	return contract.ErrUnavailable
}

func (b *Backend) objectFor(entry effect) (client.Object, error) {
	gv, err := schema.ParseGroupVersion(entry.APIVersion)
	if err != nil {
		return nil, contract.ErrConflict
	}
	object, err := b.scheme.New(gv.WithKind(entry.Kind))
	if err != nil {
		gvk := gv.WithKind(entry.Kind)
		if !b.profileKind(gvk) {
			return nil, contract.ErrConflict
		}
		untyped := &unstructured.Unstructured{}
		untyped.SetGroupVersionKind(gvk)
		return untyped, nil
	}
	typed, ok := object.(client.Object)
	if !ok {
		return nil, contract.ErrConflict
	}
	return typed, nil
}

func (b *Backend) boundObject(ctx context.Context, r *run, key string) (client.Object, error) {
	entry := r.effect(key)
	if entry == nil || entry.Deleted {
		return nil, contract.ErrConflict
	}
	if entry.UID == "" {
		return nil, ErrUncertain
	}
	object, err := b.objectFor(*entry)
	if err != nil {
		return nil, err
	}
	if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: entry.Namespace, Name: entry.Name}, object); err != nil {
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if object.GetUID() != entry.UID || (!entry.Actor && object.GetLabels()[runLabel] != string(r.object.UID)) ||
		!object.GetDeletionTimestamp().IsZero() {
		return nil, contract.ErrConflict
	}
	return object, nil
}

func (b *Backend) rememberPod(ctx context.Context, r *run, key string, receipt podReceipt) error {
	if previous, ok := r.state.Pods[key]; ok {
		if previous.Identity.ContainerID == "" {
			previous.Identity.ContainerID = receipt.Identity.ContainerID
		}
		if !reflect.DeepEqual(previous, receipt) {
			return contract.ErrConflict
		}
		if reflect.DeepEqual(r.state.Pods[key], receipt) {
			return nil
		}
	}
	r.state.Pods[key] = receipt
	return b.save(ctx, r)
}

func (b *Backend) recordObserved(ctx context.Context, r *run, key string) error {
	if !slices.Contains(r.state.Observed, key) {
		r.state.Observed = append(r.state.Observed, key)
		return b.save(ctx, r)
	}
	return nil
}

func resourceKey(side, role string) string { return fmt.Sprintf("%s/%s", side, role) }
