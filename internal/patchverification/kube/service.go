package kube

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
)

const validationFinalizer = "patchverification.orka.ai/cleanup"
const helperImageKey = "orka.kubernetes.helper-image"
const helperImageIDKey = "orka.kubernetes.helper-image-id"
const validationTaskKind = "Task"

type Store interface {
	InitializeKubernetesValidationStore(context.Context) error
	CreateKubernetesValidationSubmission(context.Context, pv.KubernetesSubmission) error
	GetKubernetesValidationSubmission(context.Context, string, string) (*pv.KubernetesSubmission, error)
	GetKubernetesValidationSubmissionByTask(context.Context, string, string) (*pv.KubernetesSubmission, error)
	ListActiveKubernetesValidationSubmissions(context.Context, string) ([]pv.KubernetesSubmission, error)
	CompleteKubernetesValidationSubmission(context.Context, string, string, string) error
	RecordKubernetesValidationTaskUID(context.Context, string, string, string, string) error
	BindKubernetesValidationTasks(context.Context, string, string, string, string, pv.Binding) error
	AttachCancelledKubernetesValidationRun(context.Context, string, string, pv.Binding) error
	CreatePatchVerificationRun(context.Context, pv.Manifest, pv.Binding, map[string][]byte) error
	CreateKubernetesValidationDispatch(context.Context, pv.KubernetesDispatch) error
	GetKubernetesValidationDispatch(context.Context, string, string, string) (*pv.KubernetesDispatch, error)
	MarkKubernetesValidationDispatchCreated(context.Context, string, string, string, string) error
	MarkKubernetesValidationDispatchObserved(context.Context, string, string, string, string, string, string) error
	MarkKubernetesValidationDispatchRecorded(context.Context, string, string, string, string, string) error
	MarkKubernetesValidationDispatchFailed(context.Context, string, string, string, string) error
	GetPatchVerificationRun(context.Context, string) (*pv.Record, error)
	RecordPatchVerificationEvidence(context.Context, pv.Binding, pv.ExecutionEvidence) error
	FinalizePatchVerificationRun(context.Context, pv.Binding) (*pv.Record, error)
	CancelPatchVerificationRun(context.Context, pv.Binding) (*pv.Record, error)
	RecoverInterruptedPatchVerificationRun(context.Context, pv.Binding) (*pv.Record, error)
	RequestKubernetesValidationCancellation(context.Context, string, string) error
}

type Config struct {
	Enabled                  bool
	Namespace                string
	HelperImage              string
	HelperImageID            string
	ToolImage                string
	ToolImageID              string
	Platform                 string
	Profile                  string
	NodeSelectorKey          string
	NodeSelectorValue        string
	LocalServices            bool
	CanaryHost               string
	CanaryPort               int
	ReconcileInterval        time.Duration
	RunTimeout               time.Duration
	PreparationConfiguration string
}

type Service struct {
	client        client.Client
	reader        client.Reader
	store         Store
	scheme        *runtime.Scheme
	config        Config
	kubeClient    kubernetes.Interface
	renderJob     func(context.Context, *corev1alpha1.Task) (*batchv1.Job, error)
	reconcileGate chan struct{}
}

func New(c client.Client, reader client.Reader, kubeClient kubernetes.Interface, store Store, scheme *runtime.Scheme, config Config) (*Service, error) {
	if !config.Enabled {
		return &Service{config: config}, nil
	}
	config.HelperImageID = imageDigest(config.HelperImageID)
	if c == nil || reader == nil || kubeClient == nil || store == nil || scheme == nil ||
		config.Namespace == "" || !digestPinned(config.HelperImage) || !digestPinned(config.ToolImage) ||
		config.HelperImageID == "" ||
		imageDigest(config.ToolImageID) == "" ||
		(config.Platform != "linux/amd64" && config.Platform != "linux/arm64") ||
		(config.Profile != pv.Offline && config.Profile != pv.LocalServices) ||
		config.NodeSelectorKey == "" || config.NodeSelectorValue == "" {
		return nil, fmt.Errorf("invalid Kubernetes validation configuration")
	}
	if config.Profile == pv.LocalServices && (!config.LocalServices || net.ParseIP(config.CanaryHost) == nil ||
		config.CanaryPort < 1 || config.CanaryPort > 65535) {
		return nil, fmt.Errorf("local-services validation requires an enabled network canary with a literal IP")
	}
	if err := store.InitializeKubernetesValidationStore(context.Background()); err != nil {
		return nil, fmt.Errorf("initialize Kubernetes validation store: %w", err)
	}
	if config.ReconcileInterval <= 0 {
		config.ReconcileInterval = 2 * time.Second
	}
	if config.RunTimeout <= 0 {
		config.RunTimeout = 30 * time.Minute
	}
	return &Service{client: c, reader: reader, kubeClient: kubeClient, store: store, scheme: scheme, config: config, reconcileGate: make(chan struct{}, 1)}, nil
}

func (s *Service) SetJobRenderer(renderer func(context.Context, *corev1alpha1.Task) (*batchv1.Job, error)) {
	s.renderJob = renderer
}

func imageDigest(image string) string {
	_, digest, found := strings.Cut(image, "@")
	if !found {
		digest = strings.TrimPrefix(image, "containerd://")
	}
	raw, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(raw) != 64 || strings.ToLower(raw) != raw {
		return ""
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return ""
	}
	return digest
}

func digestPinned(image string) bool {
	name, _, found := strings.Cut(image, "@")
	return found && name != "" && imageDigest(image) != "" && !strings.ContainsAny(image, "\r\n\x00 ")
}

func (s *Service) NeedLeaderElection() bool { return true }

func (s *Service) Start(ctx context.Context) error {
	if s == nil || !s.config.Enabled {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(s.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "validation reconciliation failed", "namespace", s.config.Namespace)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Only the elected runnable advances executions, so the two Task arms cannot
// independently finalize the shared record.
func (s *Service) RunOnce(ctx context.Context) error {
	if s == nil || !s.config.Enabled {
		return pv.ErrBinding
	}
	if err := s.lockReconciliation(ctx); err != nil {
		return err
	}
	defer func() { <-s.reconcileGate }()
	submissions, err := s.store.ListActiveKubernetesValidationSubmissions(ctx, s.config.Namespace)
	if err != nil {
		return err
	}

	for index := range submissions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		identity := submissions[index]
		submission, err := s.store.GetKubernetesValidationSubmission(ctx, identity.Namespace, identity.RequestID)
		if err != nil {
			return err
		}
		if submission.State == pv.SubmissionTerminal {
			continue
		}
		if err := s.reconcileSubmission(ctx, submission); err != nil {
			log.FromContext(ctx).Error(err, "validation run reconciliation failed",
				"namespace", submission.Namespace, "requestID", submission.RequestID)
		}
	}
	return nil
}

// Advance shares serialization with the elected runnable while allowing the
// standalone coordinator to observe errors for its exact durable submission.
func (s *Service) Advance(ctx context.Context, namespace, requestID string) error {
	if s == nil || !s.config.Enabled || namespace != s.config.Namespace {
		return pv.ErrBinding
	}
	if err := s.lockReconciliation(ctx); err != nil {
		return err
	}
	defer func() { <-s.reconcileGate }()
	submission, err := s.store.GetKubernetesValidationSubmission(ctx, namespace, requestID)
	if err != nil {
		return err
	}
	if submission.State == pv.SubmissionTerminal {
		return nil
	}
	return s.reconcileSubmission(ctx, submission)
}

func (s *Service) lockReconciliation(ctx context.Context) error {
	select {
	case s.reconcileGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) prepareSubmission(ctx context.Context, submission *pv.KubernetesSubmission) error {
	for _, side := range pv.ActionSides(submission.Manifest.Action) {
		name, uid := submission.OriginalTaskName, submission.OriginalTaskUID
		if side == pv.Patched {
			name, uid = submission.PatchedTaskName, submission.PatchedTaskUID
		}
		task := &corev1alpha1.Task{}
		err := s.reader.Get(ctx, client.ObjectKey{Namespace: submission.Namespace, Name: name}, task)
		if apierrors.IsNotFound(err) {
			if uid != "" {
				return s.failSubmission(ctx, submission, "bound validation Task disappeared during preparation")
			}
			if err := s.reservePreparedTask(ctx, submission, name); err != nil {
				return s.failSubmission(ctx, submission, "validation Task admission cannot be replayed")
			}
			task = s.validationTask(submission, side, name)
			if err := s.client.Create(ctx, task); err != nil {
				if permanentAPIError(err) {
					return s.failSubmission(ctx, submission, "Kubernetes rejected validation Task admission")
				}
				return err
			}
		} else if err != nil {
			return err
		}
		if task.UID == "" || (uid != "" && string(task.UID) != uid) ||
			!reflect.DeepEqual(task.Spec, s.validationTask(submission, side, name).Spec) ||
			!reflect.DeepEqual(task.OwnerReferences, s.validationTask(submission, side, name).OwnerReferences) ||
			!validationAnnotationsMatch(task, s.validationTask(submission, side, name)) {
			return s.failSubmission(ctx, submission, "validation Task identity changed during preparation")
		}
		if err := s.store.RecordKubernetesValidationTaskUID(ctx, submission.Namespace, submission.RequestID, side, string(task.UID)); err != nil {
			return err
		}
		if side == pv.Original {
			submission.OriginalTaskUID = string(task.UID)
		} else {
			submission.PatchedTaskUID = string(task.UID)
		}
	}
	binding, err := pv.NewRunBinding(submission.Manifest, submission.AttemptID, submission.OriginalTaskUID, submission.PatchedTaskUID)
	if err != nil {
		return err
	}
	if err := s.store.CreatePatchVerificationRun(ctx, submission.Manifest, binding, submission.Provenance); err != nil {
		return s.failSubmission(ctx, submission, "frozen source provenance or earlier validation is unavailable")
	}
	return s.store.BindKubernetesValidationTasks(ctx, submission.Namespace, submission.RequestID,
		submission.OriginalTaskUID, submission.PatchedTaskUID, binding)
}

func permanentAPIError(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)
}

func (s *Service) validationTask(submission *pv.KubernetesSubmission, side, name string) *corev1alpha1.Task {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: submission.Namespace,
		Finalizers: []string{validationFinalizer}, Annotations: map[string]string{
			pv.ValidationRequestKey: submission.RequestID, pv.ValidationSideKey: side,
			pv.ValidationManifestKey: mustManifestDigest(submission.Manifest), pv.ValidationBackendKey: pv.KubernetesBackend,
		}}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, Image: submission.Manifest.Environment.Image,
		Command: []string{"/bin/sh", "-c"}, Args: []string{"exit 125"},
		Priority: ptr.To[int32](500), ConcurrencyPolicy: corev1alpha1.ConcurrencyPolicy("Forbid"),
		StartingDeadlineSeconds: ptr.To[int64](100), SuccessfulRunsHistoryLimit: ptr.To[int32](3), FailedRunsHistoryLimit: ptr.To[int32](1),
		RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 0, BackoffMultiplier: 2}}}
	if preparedSubmission(submission) {
		task.OwnerReferences = []metav1.OwnerReference{preparationOwner(submission)}
	}
	return task
}

func mustManifestDigest(manifest pv.Manifest) string {
	digest, _ := pv.ManifestDigest(manifest)
	return digest
}

func validationAnnotationsMatch(task, expected *corev1alpha1.Task) bool {
	for key, value := range expected.Annotations {
		if task.Annotations[key] != value {
			return false
		}
	}
	return true
}

func (s *Service) ReconcileValidationTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, error) {
	reserved := false
	for key := range task.Annotations {
		reserved = reserved || strings.HasPrefix(key, pv.ValidationTaskPrefix)
	}
	if s == nil || !s.config.Enabled {
		if reserved {
			return ctrl.Result{}, true, fmt.Errorf("standalone validation execution is disabled")
		}
		return ctrl.Result{}, false, nil
	}
	submission, err := s.store.GetKubernetesValidationSubmissionByTask(ctx, task.Namespace, task.Name)
	if errors.Is(err, pv.ErrRunNotFound) && !reserved {
		return ctrl.Result{}, false, nil
	}
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if !task.DeletionTimestamp.IsZero() && submission.State != pv.SubmissionTerminal {
		err = s.store.RequestKubernetesValidationCancellation(ctx, task.Namespace, submission.RequestID)
	}
	return ctrl.Result{}, true, err
}

func (s *Service) validateTask(submission *pv.KubernetesSubmission, task *corev1alpha1.Task) error {
	side, expectedUID := pv.Original, submission.OriginalTaskUID
	if task.Name == submission.PatchedTaskName {
		side, expectedUID = pv.Patched, submission.PatchedTaskUID
	}
	expected := s.validationTask(submission, side, task.Name)
	if expectedUID == "" || string(task.UID) != expectedUID ||
		!validationAnnotationsMatch(task, expected) || !reflect.DeepEqual(task.Spec, expected.Spec) ||
		!reflect.DeepEqual(task.OwnerReferences, expected.OwnerReferences) {
		return fmt.Errorf("validation Task identity or immutable execution placeholder changed")
	}
	return nil
}

func (s *Service) OwnJob(task *corev1alpha1.Task, job *batchv1.Job) error {
	return controllerutil.SetControllerReference(task, job, s.scheme)
}

func (s *Service) failSubmission(ctx context.Context, submission *pv.KubernetesSubmission, reason string) error {
	if submission.Binding != nil {
		if _, err := s.store.RecoverInterruptedPatchVerificationRun(ctx, *submission.Binding); err != nil &&
			!errors.Is(err, pv.ErrIntegrity) {
			return err
		}
	}
	done, err := s.cleanupSubmission(ctx, submission, reason)
	if err != nil || !done {
		return err
	}
	return s.store.CompleteKubernetesValidationSubmission(ctx, submission.Namespace, submission.RequestID, reason)
}
