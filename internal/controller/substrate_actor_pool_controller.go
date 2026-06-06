/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/workspace"
)

const (
	substrateActorPoolRequeue   = 30 * time.Second
	substrateActorPoolFinalizer = "orka.ai/substrate-actor-pool-cleanup"
)

// SubstratePoolExecutor is the executor surface the pool controller needs.
type SubstratePoolExecutor interface {
	ConvergeSubstrateActors(ctx context.Context, prefix string, target int, template workspace.TemplateRef) (int, int, error)
	SubstratePoolTelemetry(ctx context.Context, prefix string, template workspace.TemplateRef, workerPool workspace.TemplateRef) (workspace.Density, error)
}

// SubstrateActorPoolReconciler reconciles SubstrateActorPool objects.
type SubstrateActorPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	SubstrateEnabled bool
	SubstrateConfig  SubstrateConfig

	SubstrateExecutorFactory func(SubstrateConfig) (SubstratePoolExecutor, error)
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=substrateactorpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=substrateactorpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=substrateactorpools/finalizers,verbs=update

// Reconcile updates pool density and optionally pre-creates warm actors.
func (r *SubstrateActorPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	pool := &corev1alpha1.SubstrateActorPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	prefix := deterministicSubstratePoolActorPrefix(pool.Namespace, pool.Name)
	if !pool.DeletionTimestamp.IsZero() {
		return r.finalizeSubstrateActorPool(ctx, pool, prefix)
	}
	if !r.SubstrateEnabled {
		return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseFailed, workspace.Density{}, "substrate is disabled")
	}
	template := workspace.TemplateRef{
		Namespace: strings.TrimSpace(pool.Spec.TemplateRef.Namespace),
		Name:      strings.TrimSpace(pool.Spec.TemplateRef.Name),
	}
	if template.Namespace == "" {
		template.Namespace = pool.Namespace
	}
	if template.Name == "" {
		return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseFailed, workspace.Density{}, "spec.templateRef.name is required")
	}
	if err := validateSubstrateActorPoolTargetActors(pool.Namespace, pool.Name, pool.Spec.TargetActors, true); err != nil {
		return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseFailed, workspace.Density{}, err.Error())
	}
	cfg := r.SubstrateConfig.WithDefaults()
	if err := validateSubstrateActorTemplateResource(ctx, r.Client, &ExecutionWorkspaceRequest{
		TemplateName:                 template.Name,
		TemplateNamespace:            template.Namespace,
		SubstrateBootstrapSecretName: cfg.BootstrapSecretName,
		SubstrateBootstrapSecretKey:  cfg.BootstrapSecretKey,
	}); err != nil {
		return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseFailed, workspace.Density{}, err.Error())
	}
	if !controllerutil.ContainsFinalizer(pool, substrateActorPoolFinalizer) {
		controllerutil.AddFinalizer(pool, substrateActorPoolFinalizer)
		if err := r.Update(ctx, pool); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if pool.Spec.PrecreateActors {
		blocked, err := r.activeSubstratePoolActorLeaseCount(ctx, pool.Namespace, prefix, int(pool.Spec.TargetActors))
		if err != nil {
			return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseFailed, workspace.Density{}, err.Error())
		}
		if blocked > 0 {
			return r.updateSubstrateActorPoolStatus(
				ctx,
				pool,
				corev1alpha1.SubstrateActorPoolPhasePending,
				workspace.Density{},
				fmt.Sprintf("waiting for %d active actor lease(s) before scaling pool to %d actors", blocked, pool.Spec.TargetActors),
			)
		}
	}
	executor, err := r.substratePoolExecutor()
	if err != nil {
		return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseFailed, workspace.Density{}, err.Error())
	}
	defer closeSubstratePoolExecutor(ctx, executor)
	if pool.Spec.PrecreateActors {
		if _, _, err := executor.ConvergeSubstrateActors(ctx, prefix, int(pool.Spec.TargetActors), template); err != nil {
			logger.Error(err, "failed to converge substrate pool actors", "pool", pool.Name)
			return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseFailed, workspace.Density{}, err.Error())
		}
	}
	workerPool := workspace.TemplateRef{}
	if pool.Spec.WorkerPoolRef != nil {
		workerPool = workspace.TemplateRef{
			Namespace: strings.TrimSpace(pool.Spec.WorkerPoolRef.Namespace),
			Name:      strings.TrimSpace(pool.Spec.WorkerPoolRef.Name),
		}
		if workerPool.Namespace == "" {
			workerPool.Namespace = pool.Namespace
		}
	}
	density, err := executor.SubstratePoolTelemetry(ctx, prefix, template, workerPool)
	if err != nil {
		return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseFailed, workspace.Density{}, err.Error())
	}
	return r.updateSubstrateActorPoolStatus(ctx, pool, corev1alpha1.SubstrateActorPoolPhaseReady, density, "pool reconciled")
}

func (r *SubstrateActorPoolReconciler) finalizeSubstrateActorPool(
	ctx context.Context,
	pool *corev1alpha1.SubstrateActorPool,
	prefix string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(pool, substrateActorPoolFinalizer) {
		return ctrl.Result{}, nil
	}
	blocked, err := r.activeSubstratePoolActorLeaseCount(ctx, pool.Namespace, prefix, 0)
	if err != nil {
		return ctrl.Result{}, err
	}
	if blocked > 0 {
		logger.Info("waiting for active substrate pool actor leases before deleting actors", "pool", pool.Name, "activeLeases", blocked)
		return ctrl.Result{RequeueAfter: substrateActorPoolRequeue}, nil
	}
	executor, err := r.substratePoolExecutor()
	if err != nil {
		return ctrl.Result{}, err
	}
	defer closeSubstratePoolExecutor(ctx, executor)
	if _, _, err := executor.ConvergeSubstrateActors(ctx, prefix, 0, workspace.TemplateRef{}); err != nil {
		logger.Error(err, "failed to delete substrate pool actors", "pool", pool.Name)
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(pool, substrateActorPoolFinalizer)
	if err := r.Update(ctx, pool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SubstrateActorPoolReconciler) activeSubstratePoolActorLeaseCount(
	ctx context.Context,
	namespace string,
	prefix string,
	target int,
) (int, error) {
	var leases coordinationv1.LeaseList
	if err := r.List(ctx, &leases, client.InNamespace(namespace), client.MatchingLabels{
		labels.LabelPurpose: substratePoolActorLeasePurpose,
	}); err != nil {
		return 0, err
	}

	active := 0
	for i := range leases.Items {
		lease := &leases.Items[i]
		actorID := substratePoolActorLeaseActorID(lease)
		ordinal, ok := substratePoolActorOrdinalFromID(actorID, prefix)
		if !ok || ordinal < target {
			continue
		}
		busy, err := substratePoolActorLeaseHasActiveHolder(ctx, r.Client, lease)
		if err != nil {
			return active, err
		}
		if busy {
			active++
		}
	}
	return active, nil
}

func (r *SubstrateActorPoolReconciler) substratePoolExecutor() (SubstratePoolExecutor, error) {
	cfg := r.SubstrateConfig.WithDefaults()
	if r.SubstrateExecutorFactory != nil {
		return r.SubstrateExecutorFactory(cfg)
	}
	return workspace.NewSubstrateExecutor(workspace.SubstrateConfig{
		APIEndpoint:           cfg.APIEndpoint,
		APICAFile:             cfg.APICAFile,
		APIInsecureSkipVerify: cfg.APIInsecureSkipVerify,
		RouterURL:             cfg.RouterURL,
		ActorDNSSuffix:        cfg.ActorDNSSuffix,
	})
}

func closeSubstratePoolExecutor(ctx context.Context, executor SubstratePoolExecutor) {
	closer, ok := executor.(interface{ Close() error })
	if !ok {
		return
	}
	if err := closer.Close(); err != nil {
		log.FromContext(ctx).Error(err, "failed to close substrate pool executor")
	}
}

func (r *SubstrateActorPoolReconciler) updateSubstrateActorPoolStatus(
	ctx context.Context,
	pool *corev1alpha1.SubstrateActorPool,
	phase corev1alpha1.SubstrateActorPoolPhase,
	density workspace.Density,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	pool.Status.Phase = phase
	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.WorkerCount = int32(max(density.WorkerCount, 0))
	pool.Status.ActorCount = int32(max(density.ActorCount, 0))
	pool.Status.RunningActorCount = int32(max(density.RunningActorCount, 0))
	pool.Status.SuspendedActorCount = int32(max(density.SuspendedActorCount, 0))
	pool.Status.ActorsPerWorker = density.ActorsPerWorker
	pool.Status.Message = sanitizeSubstrateActorPoolMessage(message)
	condition := metav1.Condition{
		Type:               "Ready",
		LastTransitionTime: now,
		ObservedGeneration: pool.Generation,
	}
	if phase == corev1alpha1.SubstrateActorPoolPhaseReady {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "PoolReady"
		condition.Message = "Substrate actor pool is ready"
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "PoolNotReady"
		condition.Message = pool.Status.Message
	}
	meta.SetStatusCondition(&pool.Status.Conditions, condition)
	if err := r.Status().Update(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: substrateActorPoolRequeue}, nil
}

func deterministicSubstratePoolActorPrefix(namespace, name string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(name)))
	return fmt.Sprintf("orka-p-%s", hex.EncodeToString(sum[:])[:24])
}

func sanitizeSubstrateActorPoolMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 1024 {
		return message[:1024]
	}
	return message
}

func validateSubstrateActorPoolTargetActors(poolNamespace, poolName string, targetActors int32, allowZero bool) error {
	minTarget := int32(1)
	if allowZero {
		minTarget = 0
	}
	if targetActors < minTarget {
		if allowZero {
			return fmt.Errorf("substrate actor pool %q in namespace %q must set spec.targetActors between 0 and %d", poolName, poolNamespace, corev1alpha1.MaxSubstrateActorPoolTargetActors)
		}
		return fmt.Errorf("substrate actor pool %q in namespace %q must set spec.targetActors between 1 and %d", poolName, poolNamespace, corev1alpha1.MaxSubstrateActorPoolTargetActors)
	}
	if targetActors > corev1alpha1.MaxSubstrateActorPoolTargetActors {
		return fmt.Errorf("substrate actor pool %q in namespace %q must set spec.targetActors no greater than %d", poolName, poolNamespace, corev1alpha1.MaxSubstrateActorPoolTargetActors)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SubstrateActorPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.SubstrateActorPool{}).
		Named("substrateactorpool").
		Complete(r)
}
