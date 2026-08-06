package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	agentExecutionFenceLabel = "orka.ai/agent-execution-ownership-fence"
	legacyFenceLabelValue    = "legacy"
	globalFenceLabelValue    = "global"
)

// AgentExecutionOwnershipLockConfig identifies the one controller Pod and
// Task watch scope that may participate in the coexistence ownership election.
type AgentExecutionOwnershipLockConfig struct {
	Identity            string
	CurrentPodNamespace string
	CurrentPodName      string
	WatchNamespace      string
}

// AgentExecutionOwnershipSnapshot is an immutable observation of the complete
// global-plus-legacy Lease fence set acquired by this process.
type AgentExecutionOwnershipSnapshot struct {
	GlobalLease AgentExecutionLeaseFence
	Legacy      []AgentExecutionLeaseFence
}

// AgentExecutionLeaseFence identifies an exact Lease incarnation. A same-name
// recreation is a different fence and must never be adopted by a live owner.
type AgentExecutionLeaseFence struct {
	Namespace       string
	Name            string
	UID             types.UID
	ResourceVersion string
}

// AgentExecutionOwnershipLock composes the fixed global election Lease with
// every legacy controller Lease into one controller-runtime resource lock.
// Create/Update return success only after the whole fence set is held.
type AgentExecutionOwnershipLock struct {
	kube    kubernetes.Interface
	primary resourcelock.Interface
	config  AgentExecutionOwnershipLockConfig

	mu          sync.RWMutex
	established bool
	expected    map[string]types.UID
	snapshot    AgentExecutionOwnershipSnapshot
	ready       bool
	lastErr     error
}

// NewAgentExecutionOwnershipLock creates the fixed ownership lock. Discovery
// and overlap checks intentionally happen in Get/Create/Update so every
// acquisition and renewal uses live, uncached Kubernetes state.
func NewAgentExecutionOwnershipLock(
	kube kubernetes.Interface,
	config AgentExecutionOwnershipLockConfig,
) (*AgentExecutionOwnershipLock, error) {
	if kube == nil {
		return nil, fmt.Errorf("kubernetes client is required for agent execution ownership")
	}
	config.Identity = strings.TrimSpace(config.Identity)
	config.CurrentPodNamespace = strings.TrimSpace(config.CurrentPodNamespace)
	config.CurrentPodName = strings.TrimSpace(config.CurrentPodName)
	config.WatchNamespace = strings.TrimSpace(config.WatchNamespace)
	if config.Identity == "" {
		return nil, fmt.Errorf("agent execution ownership identity is required")
	}
	if config.CurrentPodNamespace == "" || config.CurrentPodName == "" {
		return nil, fmt.Errorf("current Pod namespace and name are required for ownership overlap preflight")
	}

	primary, err := resourcelock.NewWithLabels(
		resourcelock.LeasesResourceLock,
		corev1alpha1.AgentExecutionControlNamespace,
		corev1alpha1.AgentExecutionOwnershipLeaseName,
		kube.CoreV1(),
		kube.CoordinationV1(),
		resourcelock.ResourceLockConfig{Identity: config.Identity},
		map[string]string{agentExecutionFenceLabel: globalFenceLabelValue},
	)
	if err != nil {
		return nil, fmt.Errorf("create global agent execution ownership lock: %w", err)
	}
	return &AgentExecutionOwnershipLock{
		kube: kube, primary: primary, config: config, expected: make(map[string]types.UID),
	}, nil
}

func (l *AgentExecutionOwnershipLock) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	if err := l.preflight(ctx); err != nil {
		l.markFailure(err)
		return nil, nil, err
	}
	return l.primary.Get(ctx)
}

func (l *AgentExecutionOwnershipLock) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	if err := l.primary.Create(ctx, record); err != nil {
		l.markFailure(err)
		return err
	}
	return l.reconcileCompleteFenceSet(ctx, record)
}

func (l *AgentExecutionOwnershipLock) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	// The global Lease is always acquired or renewed first. Returning success is
	// delayed until every legacy fence has also been renewed and reverified.
	if err := l.primary.Update(ctx, record); err != nil {
		l.markFailure(err)
		return err
	}
	return l.reconcileCompleteFenceSet(ctx, record)
}

func (l *AgentExecutionOwnershipLock) RecordEvent(message string) { l.primary.RecordEvent(message) }
func (l *AgentExecutionOwnershipLock) Identity() string           { return l.primary.Identity() }

func (l *AgentExecutionOwnershipLock) Describe() string {
	return l.primary.Describe() + "+legacy-fence-set"
}

// ReadyzChecker closes readiness immediately on an incomplete scan, lost or
// replaced Lease, newly discovered unclassified fence, or ownership loss.
func (l *AgentExecutionOwnershipLock) ReadyzChecker() func(*http.Request) error {
	return func(_ *http.Request) error {
		l.mu.RLock()
		defer l.mu.RUnlock()
		if !l.ready {
			if l.lastErr != nil {
				return l.lastErr
			}
			return errors.New("agent execution ownership fence is not ready")
		}
		return nil
	}
}

// Snapshot returns the exact fence set from the last successful renewal.
func (l *AgentExecutionOwnershipLock) Snapshot() (AgentExecutionOwnershipSnapshot, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.ready {
		return AgentExecutionOwnershipSnapshot{}, false
	}
	result := l.snapshot
	result.Legacy = append([]AgentExecutionLeaseFence(nil), result.Legacy...)
	return result, true
}

func (l *AgentExecutionOwnershipLock) reconcileCompleteFenceSet(
	ctx context.Context,
	record resourcelock.LeaderElectionRecord,
) error {
	if err := l.preflight(ctx); err != nil {
		l.markFailure(err)
		return err
	}

	leases, err := l.discoverLegacyLeases(ctx)
	if err != nil {
		err = fmt.Errorf("discover legacy controller Leases: %w", err)
		l.markFailure(err)
		return err
	}

	l.mu.RLock()
	established := l.established
	expected := cloneLeaseUIDMap(l.expected)
	l.mu.RUnlock()
	if established {
		if err := verifyExpectedLegacySet(expected, leases); err != nil {
			l.markFailure(err)
			return err
		}
	}

	keys := make([]string, 0, len(leases))
	for key := range leases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	legacySnapshot := make([]AgentExecutionLeaseFence, 0, len(keys))
	nextExpected := make(map[string]types.UID, len(keys))
	for _, key := range keys {
		lease, renewErr := l.acquireOrRenewLegacyLease(ctx, leases[key], record)
		if renewErr != nil {
			err = fmt.Errorf("renew legacy controller Lease %s: %w", key, renewErr)
			l.markFailure(err)
			return err
		}
		if established && expected[key] != lease.UID {
			err = fmt.Errorf("legacy controller Lease %s was replaced (expected UID %s, observed %s)", key, expected[key], lease.UID)
			l.markFailure(err)
			return err
		}
		nextExpected[key] = lease.UID
		legacySnapshot = append(legacySnapshot, leaseFenceFromObject(lease))
	}

	global, err := l.kube.CoordinationV1().Leases(corev1alpha1.AgentExecutionControlNamespace).Get(
		ctx, corev1alpha1.AgentExecutionOwnershipLeaseName, metav1.GetOptions{},
	)
	if err != nil {
		err = fmt.Errorf("verify global agent execution ownership Lease: %w", err)
		l.markFailure(err)
		return err
	}
	if global.Spec.HolderIdentity == nil || *global.Spec.HolderIdentity != record.HolderIdentity {
		err = fmt.Errorf("global agent execution ownership Lease holder changed during fence acquisition")
		l.markFailure(err)
		return err
	}

	ready := record.HolderIdentity == l.Identity()
	l.mu.Lock()
	l.established = l.established || ready
	if ready {
		l.expected = nextExpected
		l.snapshot = AgentExecutionOwnershipSnapshot{
			GlobalLease: leaseFenceFromObject(global),
			Legacy:      legacySnapshot,
		}
		l.lastErr = nil
	}
	l.ready = ready
	l.mu.Unlock()
	return nil
}

func (l *AgentExecutionOwnershipLock) discoverLegacyLeases(
	ctx context.Context,
) (map[string]*coordinationv1.Lease, error) {
	list, err := l.kube.CoordinationV1().Leases("").List(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + corev1alpha1.AgentExecutionLegacyLeaseName,
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string]*coordinationv1.Lease, len(list.Items)+1)
	for i := range list.Items {
		lease := &list.Items[i]
		if lease.Name != corev1alpha1.AgentExecutionLegacyLeaseName {
			continue
		}
		result[namespacedLeaseKey(lease.Namespace, lease.Name)] = lease.DeepCopy()
	}
	// Pre-create and continuously retain the legacy fence in the current
	// controller namespace. This blocks an old binary installed back into the
	// same namespace while the coexistence bridge is active.
	key := namespacedLeaseKey(l.config.CurrentPodNamespace, corev1alpha1.AgentExecutionLegacyLeaseName)
	if _, ok := result[key]; !ok {
		result[key] = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Namespace: l.config.CurrentPodNamespace,
			Name:      corev1alpha1.AgentExecutionLegacyLeaseName,
		}}
	}
	return result, nil
}

func (l *AgentExecutionOwnershipLock) acquireOrRenewLegacyLease(
	ctx context.Context,
	lease *coordinationv1.Lease,
	record resourcelock.LeaderElectionRecord,
) (*coordinationv1.Lease, error) {
	client := l.kube.CoordinationV1().Leases(lease.Namespace)
	if lease.UID == "" {
		created, err := client.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: lease.Namespace,
				Name:      lease.Name,
				Labels:    map[string]string{agentExecutionFenceLabel: legacyFenceLabelValue},
			},
			Spec: resourcelock.LeaderElectionRecordToLeaseSpec(&record),
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("lease appeared after the closed-world discovery scan")
		}
		return created, err
	}

	current := resourcelock.LeaseSpecToLeaderElectionRecord(&lease.Spec)
	if record.HolderIdentity == "" {
		if current.HolderIdentity != "" && current.HolderIdentity != l.Identity() {
			return nil, fmt.Errorf("cannot release fence held by %q", current.HolderIdentity)
		}
	} else if current.HolderIdentity != "" && current.HolderIdentity != l.Identity() && !leaderRecordExpired(*current, time.Now()) {
		return nil, fmt.Errorf("active legacy controller holder %q has not expired", current.HolderIdentity)
	}

	updated := lease.DeepCopy()
	updated.Spec = resourcelock.LeaderElectionRecordToLeaseSpec(&record)
	if updated.Labels == nil {
		updated.Labels = make(map[string]string)
	}
	updated.Labels[agentExecutionFenceLabel] = legacyFenceLabelValue
	return client.Update(ctx, updated, metav1.UpdateOptions{})
}

func (l *AgentExecutionOwnershipLock) preflight(ctx context.Context) error {
	currentDeploymentUID, err := l.currentDeploymentUID(ctx)
	if err != nil {
		return fmt.Errorf("resolve current controller Deployment: %w", err)
	}
	deployments, err := l.kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list controller Deployments for overlap preflight: %w", err)
	}
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		if deployment.UID == currentDeploymentUID {
			if err := l.validateCurrentDeployment(deployment); err != nil {
				return err
			}
			continue
		}
		if !activeControllerDeployment(deployment) || !watchScopesOverlap(l.config.WatchNamespace, controllerWatchNamespace(deployment.Spec.Template.Spec.Containers)) {
			continue
		}
		if !controllerLeaderElectionEnabled(deployment.Spec.Template.Spec.Containers) {
			return fmt.Errorf("overlapping controller Deployment %s/%s has leader election disabled", deployment.Namespace, deployment.Name)
		}
		return fmt.Errorf("overlapping controller Deployment %s/%s watches the same Task population", deployment.Namespace, deployment.Name)
	}

	pods, err := l.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list controller Pods for overlap preflight: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Namespace == l.config.CurrentPodNamespace && pod.Name == l.config.CurrentPodName {
			continue
		}
		if !activeControllerPod(pod) || !watchScopesOverlap(l.config.WatchNamespace, controllerWatchNamespace(pod.Spec.Containers)) {
			continue
		}
		if !controllerLeaderElectionEnabled(pod.Spec.Containers) {
			return fmt.Errorf("overlapping controller Pod %s/%s has leader election disabled", pod.Namespace, pod.Name)
		}
		return fmt.Errorf("overlapping controller Pod %s/%s watches the same Task population", pod.Namespace, pod.Name)
	}
	return nil
}

func (l *AgentExecutionOwnershipLock) currentDeploymentUID(ctx context.Context) (types.UID, error) {
	pod, err := l.kube.CoreV1().Pods(l.config.CurrentPodNamespace).Get(ctx, l.config.CurrentPodName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Deployment" && owner.UID != "" {
			return owner.UID, nil
		}
		if owner.Kind != "ReplicaSet" || owner.Name == "" {
			continue
		}
		replicaSet, getErr := l.kube.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if getErr != nil {
			return "", getErr
		}
		for _, replicaOwner := range replicaSet.OwnerReferences {
			if replicaOwner.Kind == "Deployment" && replicaOwner.UID != "" {
				return replicaOwner.UID, nil
			}
		}
	}
	return "", fmt.Errorf("current Pod %s/%s is not owned by a Deployment", pod.Namespace, pod.Name)
}

func (l *AgentExecutionOwnershipLock) validateCurrentDeployment(deployment *appsv1.Deployment) error {
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		return fmt.Errorf("current controller Deployment %s/%s must have exactly one replica while SQLite is used", deployment.Namespace, deployment.Name)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		return fmt.Errorf("current controller Deployment %s/%s must use Recreate while SQLite is used", deployment.Namespace, deployment.Name)
	}
	if !controllerLeaderElectionEnabled(deployment.Spec.Template.Spec.Containers) {
		return fmt.Errorf("current controller Deployment %s/%s has leader election disabled", deployment.Namespace, deployment.Name)
	}
	return nil
}

func (l *AgentExecutionOwnershipLock) markFailure(err error) {
	l.mu.Lock()
	l.ready = false
	l.lastErr = err
	l.mu.Unlock()
}

func verifyExpectedLegacySet(expected map[string]types.UID, observed map[string]*coordinationv1.Lease) error {
	if len(expected) != len(observed) {
		return fmt.Errorf("legacy controller Lease fence set changed after ownership was established")
	}
	for key, uid := range expected {
		lease, ok := observed[key]
		if !ok {
			return fmt.Errorf("legacy controller Lease %s disappeared after ownership was established", key)
		}
		if lease.UID != uid {
			return fmt.Errorf("legacy controller Lease %s was replaced (expected UID %s, observed %s)", key, uid, lease.UID)
		}
	}
	return nil
}

func activeControllerDeployment(deployment *appsv1.Deployment) bool {
	if deployment == nil || deployment.DeletionTimestamp != nil || !controllerLabels(deployment.Spec.Template.Labels) {
		return false
	}
	return deployment.Spec.Replicas == nil || *deployment.Spec.Replicas > 0
}

func activeControllerPod(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil || !controllerLabels(pod.Labels) {
		return false
	}
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

func controllerLabels(values map[string]string) bool {
	return values["app.kubernetes.io/component"] == AgentSandboxNamespaceStrategyController ||
		values["control-plane"] == "controller-manager" ||
		values["orka.ai/network-role"] == AgentSandboxNamespaceStrategyController
}

func controllerLeaderElectionEnabled(containers []corev1.Container) bool {
	for _, container := range containers {
		if !controllerContainer(container) {
			continue
		}
		if value, present := commandFlag(container.Args, "leader-elect"); present {
			parsed := strings.TrimSpace(strings.ToLower(value))
			return parsed == "" || parsed == "true" || parsed == "1"
		}
	}
	return false
}

func controllerWatchNamespace(containers []corev1.Container) string {
	for _, container := range containers {
		if !controllerContainer(container) {
			continue
		}
		if value, present := commandFlag(container.Args, "watch-namespace"); present {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func controllerContainer(container corev1.Container) bool {
	if container.Name == AgentSandboxNamespaceStrategyController || container.Name == "manager" {
		return true
	}
	for _, command := range container.Command {
		if strings.HasSuffix(command, "/manager") || command == "manager" {
			return true
		}
	}
	return false
}

func commandFlag(args []string, name string) (string, bool) {
	prefix := "--" + name + "="
	exact := "--" + name
	for i, arg := range args {
		if arg == exact {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				return args[i+1], true
			}
			return "", true
		}
		if after, ok := strings.CutPrefix(arg, prefix); ok {
			return after, true
		}
	}
	return "", false
}

func watchScopesOverlap(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	return left == "" || right == "" || left == right
}

func leaderRecordExpired(record resourcelock.LeaderElectionRecord, now time.Time) bool {
	if record.HolderIdentity == "" || record.LeaseDurationSeconds <= 0 {
		return true
	}
	lastRenewal := record.RenewTime.Time
	if lastRenewal.IsZero() {
		lastRenewal = record.AcquireTime.Time
	}
	return lastRenewal.Add(time.Duration(record.LeaseDurationSeconds) * time.Second).Before(now)
}

func leaseFenceFromObject(lease *coordinationv1.Lease) AgentExecutionLeaseFence {
	return AgentExecutionLeaseFence{
		Namespace: lease.Namespace, Name: lease.Name, UID: lease.UID, ResourceVersion: lease.ResourceVersion,
	}
}

func namespacedLeaseKey(namespace, name string) string { return namespace + "/" + name }

func cloneLeaseUIDMap(source map[string]types.UID) map[string]types.UID {
	result := make(map[string]types.UID, len(source))
	maps.Copy(result, source)
	return result
}

var _ resourcelock.Interface = (*AgentExecutionOwnershipLock)(nil)
