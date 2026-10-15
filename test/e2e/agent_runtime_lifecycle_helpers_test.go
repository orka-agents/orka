//go:build e2e
// +build e2e

package e2e

// Go ports of the RuntimePool, Task fence, and cleanup checks in
// scripts/agent-runtime-e2e.sh, used by agent_runtime_lifecycle_test.go.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/test/utils"
)

const (
	agentRuntimeRunLabel          = "orka.ai/acp-e2e-run"
	agentRuntimeObserverFinalizer = "acp-e2e.orka.ai/cancellation-observer"
	agentRuntimeCleanupFinalizer  = "orka.ai/cleanup"
	defaultRuntimeNamespace       = "orka-runtimes"
	harnessV2Protocol             = "orka.harness.v2"
	// agentRuntimeWait and agentRuntimeStateWait match the bash lane's
	// ACP_E2E_WAIT_SECONDS and ACP_E2E_STATE_WAIT_SECONDS defaults.
	agentRuntimeWait      = 15 * time.Minute
	agentRuntimeStateWait = 5 * time.Minute
)

var (
	sha256DigestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	imageDigestPattern      = regexp.MustCompile(`@sha256:[a-f0-9]{64}$`)
	imageIDDigestPattern    = regexp.MustCompile(`sha256:[a-f0-9]{64}$`)
	tokenGenerationPattern  = regexp.MustCompile(`^[a-f0-9]{16}$`)
	terminalExecutionStates = []corev1alpha1.TaskExecutionState{
		corev1alpha1.TaskExecutionStateSucceeded,
		corev1alpha1.TaskExecutionStateFailed,
		corev1alpha1.TaskExecutionStateCancelled,
		corev1alpha1.TaskExecutionStateOutcomeUnknown,
	}
)

// runtimePoolSnapshot is one validated, stable view of a serving RuntimePool
// and its active runtime Pod.
type runtimePoolSnapshot struct {
	PoolName                   string
	PoolUID                    string
	ControllerEpoch            int64
	ProfileDigest              string
	ProfileDigestSchemaVersion string
	RuntimeImage               string
	RuntimeInstanceID          string
	SupervisorBootID           string
	PodNamespace               string
	PodName                    string
	PodUID                     string
	PodAddress                 string
	PodImageID                 string
}

func kubectlJSON(g Gomega, target any, args ...string) {
	output, err := utils.Run(exec.Command("kubectl", append(args, "-o", "json")...))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(json.Unmarshal([]byte(output), target)).To(Succeed())
}

func getRuntimePool(g Gomega, name string) corev1alpha1.RuntimePool {
	var pool corev1alpha1.RuntimePool
	kubectlJSON(g, &pool, "get", "runtimepool", name, "-n", namespace)
	return pool
}

func getACPTask(g Gomega, name string) corev1alpha1.Task {
	return fetchTaskForACPAssertion(g, name)
}

func runtimePoolNamespace(pool corev1alpha1.RuntimePool) string {
	if pool.Spec.RuntimeNamespace != "" {
		return pool.Spec.RuntimeNamespace
	}
	return defaultRuntimeNamespace
}

// runtimePoolPods returns the pool's non-terminating runtime Pods.
func runtimePoolPods(g Gomega, pool corev1alpha1.RuntimePool) []corev1.Pod {
	var pods corev1.PodList
	kubectlJSON(g, &pods, "get", "pods", "-n", runtimePoolNamespace(pool), "-l",
		fmt.Sprintf("orka.ai/runtime-pool-namespace=%s,orka.ai/runtime-pool-name=%s", namespace, pool.Name))
	live := []corev1.Pod{}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil {
			live = append(live, pod)
		}
	}
	return live
}

func runtimePoolAndActivePod(g Gomega, name string) (corev1alpha1.RuntimePool, corev1.Pod) {
	pool := getRuntimePool(g, name)
	g.Expect(pool.Status.ActiveInstance).NotTo(BeNil(), "RuntimePool %s has no active instance", name)
	var pod corev1.Pod
	kubectlJSON(g, &pod, "get", "pod", pool.Status.ActiveInstance.PodName, "-n", pool.Status.ActiveInstance.PodNamespace)
	return pool, pod
}

// validateRuntimePoolProjection ports pool_profile_projection_valid.
func validateRuntimePoolProjection(
	g Gomega,
	pool corev1alpha1.RuntimePool,
	pod corev1.Pod,
	provider, model string,
	intent corev1alpha1.WorkspaceIntent,
) {
	profile := pool.Spec.Runtime.Profile
	g.Expect(pool.Spec.TrustDomain.Namespace).To(Equal(namespace))
	g.Expect(pool.Name).To(MatchRegexp(`^acp-` + provider + `-[a-f0-9]{16}$`))
	g.Expect(pool.Labels).To(HaveKeyWithValue("orka.ai/acp-runtime-pool", "true"))
	g.Expect(pool.Labels).To(HaveKeyWithValue("orka.ai/acp-trust-domain", namespace))
	g.Expect(profile.Digest).To(MatchRegexp(sha256DigestPattern.String()))
	g.Expect(pool.Labels).To(HaveKeyWithValue("orka.ai/acp-profile", strings.TrimPrefix(profile.Digest, "sha256:")[:16]))
	g.Expect(string(pool.UID)).NotTo(BeEmpty())
	g.Expect(pool.Generation).To(BeNumerically(">", 0))
	g.Expect(pool.Status.ObservedGeneration).To(Equal(pool.Generation))
	g.Expect(pool.Spec.DesiredReplicas).To(Equal(int32(1)))

	g.Expect(string(profile.ProtocolVersion)).To(Equal(harnessV2Protocol))
	g.Expect(profile.ProviderKind).To(Equal(provider))
	g.Expect(profile.Model).To(Equal(model))
	g.Expect(profile.WorkspaceIntent).To(Equal(intent))
	g.Expect(pool.Spec.Runtime.Image).To(MatchRegexp(imageDigestPattern.String()))
	g.Expect(profile.DigestSchemaVersion).NotTo(BeEmpty())
	g.Expect(profile.AdapterDigests).NotTo(BeEmpty())
	for adapter, digest := range profile.AdapterDigests {
		g.Expect(digest).To(MatchRegexp(sha256DigestPattern.String()), "adapter digest %s", adapter)
	}
	for field, digest := range map[string]string{
		"agentConfigurationDigest": profile.AgentConfigurationDigest,
		"toolPolicyDigest":         profile.ToolPolicyDigest,
		"approvalPolicyDigest":     profile.ApprovalPolicyDigest,
		"mcpConfigurationDigest":   profile.MCPConfigurationDigest,
	} {
		g.Expect(digest).To(MatchRegexp(sha256DigestPattern.String()), field)
	}
	g.Expect(profile.ProxyCredentialRole).NotTo(BeEmpty())
	g.Expect(profile.ProxyCredentialScope).NotTo(BeEmpty())

	status := pool.Status
	g.Expect(string(status.Lifecycle)).To(Equal("Serving"))
	g.Expect(string(status.AdmissionState)).To(Equal("Accepting"))
	g.Expect(status.CurrentReplicas).To(Equal(int32(1)))
	g.Expect(status.DesiredReplicas).To(Equal(int32(1)))
	g.Expect(status.ControllerEpoch).To(BeNumerically(">", 0))

	active := status.ActiveInstance
	g.Expect(active).NotTo(BeNil())
	g.Expect(active.ControllerEpoch).To(Equal(status.ControllerEpoch))
	g.Expect(string(active.ProtocolVersion)).To(Equal(harnessV2Protocol))
	g.Expect(active.ProfileDigest).To(Equal(profile.Digest))
	g.Expect(active.ProfileDigestSchemaVersion).To(Equal(profile.DigestSchemaVersion))
	g.Expect(active.ProviderTokenGeneration).To(MatchRegexp(tokenGenerationPattern.String()))
	g.Expect(active.BootID).NotTo(BeEmpty())
	g.Expect(active.PodNamespace).NotTo(BeEmpty())
	g.Expect(active.PodName).NotTo(BeEmpty())
	g.Expect(active.PodUID).NotTo(BeEmpty())
	g.Expect(active.RuntimeInstanceID).To(Equal(active.PodUID + "." + active.BootID))
	g.Expect(active.PodAddress).NotTo(BeEmpty())
	g.Expect(active.LastObservedTime).NotTo(BeNil())

	g.Expect(pod.Namespace).To(Equal(active.PodNamespace))
	g.Expect(pod.Name).To(Equal(active.PodName))
	g.Expect(string(pod.UID)).To(Equal(active.PodUID))
	g.Expect(pod.Annotations["orka.ai/provider-token-generation"]).To(MatchRegexp(tokenGenerationPattern.String()))
	g.Expect(pod.Annotations["orka.ai/provider-token-generation"]).To(Equal(active.ProviderTokenGeneration))

	g.Expect(status.Capacity.MaxResidentSessions).To(BeNumerically(">=", 1))
	g.Expect(status.Capacity.MaxRunningPrompts).To(BeNumerically(">=", 1))
	g.Expect(status.Capacity.MaxRunningPrompts).To(BeNumerically("<=", status.Capacity.MaxResidentSessions))
}

// runtimePoolProjectionFingerprint covers every field the projection check
// reads, so two equal reads prove the projection was stable.
func runtimePoolProjectionFingerprint(g Gomega, pool corev1alpha1.RuntimePool, pod corev1.Pod) string {
	encoded, err := json.Marshal(map[string]any{
		"uid": pool.UID, "name": pool.Name, "generation": pool.Generation,
		"desiredReplicas": pool.Spec.DesiredReplicas, "trustDomain": pool.Spec.TrustDomain,
		"image": pool.Spec.Runtime.Image, "profile": pool.Spec.Runtime.Profile,
		"lifecycle": pool.Status.Lifecycle, "admission": pool.Status.AdmissionState,
		"currentReplicas": pool.Status.CurrentReplicas, "statusDesired": pool.Status.DesiredReplicas,
		"observedGeneration": pool.Status.ObservedGeneration, "epoch": pool.Status.ControllerEpoch,
		"activeInstance": pool.Status.ActiveInstance,
		"maxResident":    pool.Status.Capacity.MaxResidentSessions,
		"maxRunning":     pool.Status.Capacity.MaxRunningPrompts,
		"pod": []string{pod.Namespace, pod.Name, string(pod.UID),
			pod.Annotations["orka.ai/provider-token-generation"]},
	})
	g.Expect(err).NotTo(HaveOccurred())
	return string(encoded)
}

// assertPodImagesDigestPinned ports assert_all_pod_images.
func assertPodImagesDigestPinned(g Gomega, pod corev1.Pod) {
	for _, container := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		g.Expect(container.Image).To(MatchRegexp(imageDigestPattern.String()), "container %s image", container.Name)
	}
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...),
		pod.Status.ContainerStatuses...)
	g.Expect(statuses).NotTo(BeEmpty())
	for _, status := range statuses {
		g.Expect(status.Image).NotTo(BeEmpty(), "container %s status image", status.Name)
		g.Expect(status.ImageID).To(MatchRegexp(imageIDDigestPattern.String()), "container %s imageID", status.Name)
	}
}

// captureRuntimePoolSnapshot ports capture_pool_snapshot: two equal valid
// reads 250ms apart, then the Pod, image, and single-replica checks.
func captureRuntimePoolSnapshot(
	poolName, provider, model string,
	intent corev1alpha1.WorkspaceIntent,
) runtimePoolSnapshot {
	var snapshot runtimePoolSnapshot
	EventuallyWithOffset(1, func(g Gomega) {
		pool, pod := runtimePoolAndActivePod(g, poolName)
		validateRuntimePoolProjection(g, pool, pod, provider, model, intent)
		first := runtimePoolProjectionFingerprint(g, pool, pod)
		time.Sleep(250 * time.Millisecond)
		pool, pod = runtimePoolAndActivePod(g, poolName)
		validateRuntimePoolProjection(g, pool, pod, provider, model, intent)
		g.Expect(runtimePoolProjectionFingerprint(g, pool, pod)).To(Equal(first), "RuntimePool projection changed")

		var runtimeImage, runtimeImageID string
		for _, container := range pod.Spec.Containers {
			if container.Name == "runtime" {
				runtimeImage = container.Image
			}
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "runtime" {
				runtimeImageID = status.ImageID
			}
		}
		active := pool.Status.ActiveInstance
		g.Expect(runtimeImage).To(Equal(pool.Spec.Runtime.Image))
		g.Expect(pod.Status.PodIP).To(Equal(active.PodAddress))
		g.Expect(runtimePoolPods(g, pool)).To(HaveLen(1))
		assertPodImagesDigestPinned(g, pod)
		snapshot = runtimePoolSnapshot{
			PoolName: pool.Name, PoolUID: string(pool.UID), ControllerEpoch: pool.Status.ControllerEpoch,
			ProfileDigest:              pool.Spec.Runtime.Profile.Digest,
			ProfileDigestSchemaVersion: pool.Spec.Runtime.Profile.DigestSchemaVersion,
			RuntimeImage:               pool.Spec.Runtime.Image, RuntimeInstanceID: active.RuntimeInstanceID,
			SupervisorBootID: active.BootID, PodNamespace: active.PodNamespace, PodName: active.PodName,
			PodUID: active.PodUID, PodAddress: active.PodAddress, PodImageID: runtimeImageID,
		}
	}, agentRuntimeWait, 250*time.Millisecond).Should(Succeed())
	return snapshot
}

// assertTaskFence ports assert_task_fence; after a controller restart the
// epoch may only advance (assert_restart_task_fence).
func assertTaskFence(g Gomega, task corev1alpha1.Task, snapshot runtimePoolSnapshot, afterRestart bool) {
	execution := task.Status.Execution
	g.Expect(execution).NotTo(BeNil())
	g.Expect(execution.RuntimePoolName).To(Equal(snapshot.PoolName))
	g.Expect(task.Labels).To(HaveKeyWithValue("orka.ai/runtime-pool", snapshot.PoolName))
	g.Expect(execution.RuntimePoolUID).To(Equal(snapshot.PoolUID))
	g.Expect(execution.RuntimeInstanceID).To(Equal(snapshot.RuntimeInstanceID))
	if afterRestart {
		g.Expect(execution.ControllerEpoch).To(BeNumerically(">=", snapshot.ControllerEpoch))
	} else {
		g.Expect(execution.ControllerEpoch).To(Equal(snapshot.ControllerEpoch))
	}
	g.Expect(execution.PromptID).NotTo(BeEmpty())
	g.Expect(execution.RequestDigest).To(MatchRegexp(sha256DigestPattern.String()))
	g.Expect(execution.RuntimeSessionUID).NotTo(BeEmpty())
	g.Expect(execution.RuntimeSessionGeneration).To(BeNumerically(">=", 1))
	g.Expect(execution.Attempt).To(Equal(int32(1)), "the prompt must not be replayed")
}

// waitTaskTerminalProjection ports task_terminal_projection_ready.
func waitTaskTerminalProjection(name string) corev1alpha1.Task {
	var task corev1alpha1.Task
	EventuallyWithOffset(1, func(g Gomega) {
		task = getACPTask(g, name)
		g.Expect(task.Status.Execution).NotTo(BeNil())
		state, phase := task.Status.Execution.State, task.Status.Phase
		g.Expect(
			(state == corev1alpha1.TaskExecutionStateSucceeded && phase == corev1alpha1.TaskPhaseSucceeded) ||
				(state == corev1alpha1.TaskExecutionStateFailed && phase == corev1alpha1.TaskPhaseFailed) ||
				(state == corev1alpha1.TaskExecutionStateCancelled && phase == corev1alpha1.TaskPhaseCancelled) ||
				(state == corev1alpha1.TaskExecutionStateOutcomeUnknown && phase == corev1alpha1.TaskPhaseFailed),
		).To(BeTrue(), "execution %s / phase %s is not a terminal projection", state, phase)
	}, agentRuntimeWait, 2*time.Second).Should(Succeed())
	return task
}

// waitTaskPoolName waits for the Task's RuntimePool assignment.
func waitTaskPoolName(name string) string {
	var pool string
	EventuallyWithOffset(1, func(g Gomega) {
		task := getACPTask(g, name)
		g.Expect(task.Status.Execution).NotTo(BeNil())
		pool = task.Status.Execution.RuntimePoolName
		g.Expect(pool).NotTo(BeEmpty())
	}, agentRuntimeStateWait, 2*time.Second).Should(Succeed())
	return pool
}

// assertTaskSucceededRead ports assert_task_succeeded_read for a read of the
// pinned agentRuntimeRepoRef.
func assertTaskSucceededRead(name string, snapshot runtimePoolSnapshot) corev1alpha1.Task {
	task := waitTaskTerminalProjection(name)
	g := NewWithT(GinkgoT())
	g.Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseSucceeded), "task %s: %s", name, task.Status.Message)
	g.Expect(task.Status.Execution.State).To(Equal(corev1alpha1.TaskExecutionStateSucceeded))
	g.Expect(task.Status.Execution.Outcome).To(Equal(corev1alpha1.TaskExecutionOutcomeSucceeded))
	g.Expect(task.Status.Delivery).NotTo(BeNil())
	g.Expect(task.Status.Delivery.State).To(Equal(corev1alpha1.TaskDeliveryStateReadValidated))
	g.Expect(task.Status.Delivery.Outcome).To(Equal(corev1alpha1.TaskDeliveryOutcomeReadValidated))
	g.Expect(task.Status.Delivery.StartingSHA).To(Equal(agentRuntimeRepoRef))
	g.Expect(task.Status.ResultRef).NotTo(BeNil())
	g.Expect(task.Status.ResultRef.Available).To(BeTrue())
	g.Expect(task.Status.JobName).To(BeEmpty())
	assertTaskFence(g, task, snapshot, false)
	return task
}

// assertCancelledTask ports assert_cancelled_task.
func assertCancelledTask(name string, snapshot runtimePoolSnapshot, reason corev1alpha1.TaskExecutionReason) {
	task := waitTaskTerminalProjection(name)
	g := NewWithT(GinkgoT())
	g.Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseCancelled), "task %s: %s", name, task.Status.Message)
	g.Expect(task.Status.Execution.State).To(Equal(corev1alpha1.TaskExecutionStateCancelled))
	g.Expect(task.Status.Execution.Outcome).To(Equal(corev1alpha1.TaskExecutionOutcomeCancelled))
	g.Expect(task.Status.Execution.Reason).To(Equal(reason))
	g.Expect(task.Status.JobName).To(BeEmpty())
	assertTaskFence(g, task, snapshot, false)
}

// assertRestartTaskSettled ports assert_restart_task_settled: a prompt in
// flight across a controller restart settles once, without replay.
func assertRestartTaskSettled(name string, snapshot runtimePoolSnapshot) {
	task := waitTaskTerminalProjection(name)
	g := NewWithT(GinkgoT())
	execution := task.Status.Execution
	succeeded := task.Status.Phase == corev1alpha1.TaskPhaseSucceeded &&
		execution.State == corev1alpha1.TaskExecutionStateSucceeded &&
		execution.Outcome == corev1alpha1.TaskExecutionOutcomeSucceeded &&
		task.Status.Delivery != nil &&
		task.Status.Delivery.State == corev1alpha1.TaskDeliveryStateReadValidated &&
		task.Status.Delivery.Outcome == corev1alpha1.TaskDeliveryOutcomeReadValidated
	cancelled := task.Status.Phase == corev1alpha1.TaskPhaseCancelled &&
		execution.State == corev1alpha1.TaskExecutionStateCancelled &&
		execution.Outcome == corev1alpha1.TaskExecutionOutcomeCancelled
	unknown := task.Status.Phase == corev1alpha1.TaskPhaseFailed &&
		execution.State == corev1alpha1.TaskExecutionStateOutcomeUnknown &&
		execution.Outcome == corev1alpha1.TaskExecutionOutcomeOutcomeUnknown
	g.Expect(succeeded || cancelled || unknown).To(BeTrue(),
		"restart task settled as phase %s, execution %s/%s", task.Status.Phase, execution.State, execution.Outcome)
	assertTaskFence(g, task, snapshot, true)
}

func taskSessionIdentity(g Gomega, name string) (string, int64) {
	task := getACPTask(g, name)
	g.Expect(task.Status.Execution).NotTo(BeNil())
	g.Expect(task.Status.Execution.RuntimeSessionUID).NotTo(BeEmpty())
	return task.Status.Execution.RuntimeSessionUID, task.Status.Execution.RuntimeSessionGeneration
}

// runtimeSessionProcessesScript prints the owner of one RuntimeSession's
// private home, then the status of every process in the runtime container.
// The home stays child-owned even when a read-intent workspace protects the
// session root and workspace.
const runtimeSessionProcessesScript = `set -- /sessions/session-"$1"-*
if [ "$#" -ne 1 ] || [ ! -d "$1" ]; then echo "no single RuntimeSession tree $1" >&2; exit 3; fi
echo "Owner: $(stat -c %u "$1/home")"
cat /proc/[0-9]*/status 2>/dev/null
exit 0`

// runtimeSessionAdapters returns the PIDs of the adapters the supervisor (PID
// 1 in the runtime container) runs for RuntimeSession uid/generation. The
// supervisor starts each adapter as the leader of its own process group under
// the session's private UID, which also owns the session's private home.
func runtimeSessionAdapters(g Gomega, snapshot runtimePoolSnapshot, uid string, generation int64) []int {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%d", uid, generation))
	output, err := utils.Run(exec.Command("kubectl", "exec", "-n", snapshot.PodNamespace, snapshot.PodName,
		"-c", "runtime", "--", "sh", "-c", runtimeSessionProcessesScript, "sh", hex.EncodeToString(sum[:])))
	g.Expect(err).NotTo(HaveOccurred())
	owner, adapters, err := parseRuntimeSessionAdapters(output)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(owner).NotTo(BeElementOf("", "0"), "RuntimeSession %s generation %d has no child-owned home", uid, generation)
	return adapters
}

// parseRuntimeSessionAdapters reads runtimeSessionProcessesScript output: the
// home owner, then the processes that are children of PID 1, run as that
// owner, and lead their own process group.
func parseRuntimeSessionAdapters(output string) (string, []int, error) {
	owner := ""
	process := map[string]string{}
	var adapters []int
	var parseErr error
	flush := func() {
		if process["Pid:"] != "" && process["PPid:"] == "1" && process["Uid:"] == owner &&
			process["NSpgid:"] == process["Pid:"] {
			pid, err := strconv.Atoi(process["Pid:"])
			if err != nil {
				parseErr = err
			}
			adapters = append(adapters, pid)
		}
		clear(process)
	}
	for line := range strings.Lines(output) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "Owner:":
			owner = fields[1]
		case "Name:":
			flush()
		case "Pid:", "PPid:", "Uid:", "NSpgid:":
			process[fields[0]] = fields[1]
		}
	}
	flush()
	return owner, adapters, parseErr
}

// killRuntimeSessionAdapter kills the process group of the idle adapter behind
// RuntimeSession uid/generation, as an adapter crash between prompts would, and
// waits until the supervisor has reaped it. It returns the killed PID.
func killRuntimeSessionAdapter(snapshot runtimePoolSnapshot, uid string, generation int64) int {
	adapters := runtimeSessionAdapters(Default, snapshot, uid, generation)
	ExpectWithOffset(1, adapters).To(HaveLen(1),
		"RuntimeSession %s generation %d needs exactly one adapter", uid, generation)
	_, err := utils.Run(exec.Command("kubectl", "exec", "-n", snapshot.PodNamespace, snapshot.PodName,
		"-c", "runtime", "--", "sh", "-c", `kill -9 -"$1"`, "sh", strconv.Itoa(adapters[0])))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		g.Expect(runtimeSessionAdapters(g, snapshot, uid, generation)).To(BeEmpty())
	}, agentRuntimeSettleBound, time.Second).Should(Succeed())
	return adapters[0]
}

func runtimePoolServing(g Gomega, name string) {
	pool := getRuntimePool(g, name)
	g.Expect(string(pool.Status.Lifecycle) + "/" + string(pool.Status.AdmissionState)).To(Equal("Serving/Accepting"))
}

func waitRuntimePoolServing(name string) {
	EventuallyWithOffset(1, func(g Gomega) { runtimePoolServing(g, name) },
		agentRuntimeWait, 2*time.Second).Should(Succeed())
}

// waitTaskRunningOnPool ports pool_running_for_task.
func waitTaskRunningOnPool(task, pool string) {
	EventuallyWithOffset(1, func(g Gomega) { taskRunningOnPool(g, task, pool) },
		agentRuntimeStateWait, 2*time.Second).Should(Succeed())
}

func taskRunningOnPool(g Gomega, task, pool string) {
	current := getACPTask(g, task)
	g.Expect(current.Status.Execution).NotTo(BeNil())
	g.Expect(current.Status.Execution.State).To(Equal(corev1alpha1.TaskExecutionStateRunning))
	g.Expect(getRuntimePool(g, pool).Status.Capacity.RunningPrompts).To(BeNumerically(">", 0))
}

// waitRuntimePoolInstanceReplaced ports pool_instance_changed_and_serving.
func waitRuntimePoolInstanceReplaced(name, previous string) {
	EventuallyWithOffset(1, func(g Gomega) {
		pool := getRuntimePool(g, name)
		g.Expect(pool.Status.ActiveInstance).NotTo(BeNil())
		g.Expect(pool.Status.ActiveInstance.RuntimeInstanceID).NotTo(BeEmpty())
		g.Expect(pool.Status.ActiveInstance.RuntimeInstanceID).NotTo(Equal(previous))
		runtimePoolServing(g, name)
	}, agentRuntimeWait, 2*time.Second).Should(Succeed())
}

// runtimePoolLifecycleTargets ports pool_lifecycle_reached: each target also
// accepts the states after it.
var runtimePoolLifecycleTargets = map[string][]string{
	"Draining":  {"Draining/Draining", "Quiescent/Draining", "Stopping/Closed", "Stopped/Closed"},
	"Quiescent": {"Quiescent/Draining", "Stopping/Closed", "Stopped/Closed"},
	"Stopping":  {"Stopping/Closed", "Stopped/Closed"},
	"Stopped":   {"Stopped/Closed"},
}

func waitRuntimePoolLifecycle(name, target string) {
	EventuallyWithOffset(1, func(g Gomega) {
		pool := getRuntimePool(g, name)
		g.Expect(string(pool.Status.Lifecycle) + "/" + string(pool.Status.AdmissionState)).
			To(BeElementOf(runtimePoolLifecycleTargets[target]))
	}, agentRuntimeStateWait, 250*time.Millisecond).Should(Succeed())
}

// waitRuntimePoolCountersZero ports pool_transient_counters_zero.
func waitRuntimePoolCountersZero(name string, expectedResident int32) {
	EventuallyWithOffset(1, func(g Gomega) {
		capacity := getRuntimePool(g, name).Status.Capacity
		g.Expect(capacity.MaxResidentSessions).To(BeNumerically(">", 0))
		g.Expect(capacity.MaxRunningPrompts).To(BeNumerically(">", 0))
		g.Expect(capacity.ResidentSessions).To(Equal(expectedResident))
		g.Expect(capacity.RunningPrompts).To(BeZero())
		g.Expect(capacity.QueuedTasks).To(BeZero())
		g.Expect(capacity.ReservedSessions).To(BeZero())
		g.Expect(capacity.ReservedPrompts).To(BeZero())
		g.Expect(capacity.PendingPermissions).To(BeZero())
		g.Expect(capacity.FinalizingSessions).To(BeZero())
		g.Expect(capacity.LiveDescendants).To(BeNumerically("<=", expectedResident))
		g.Expect(capacity.Reservations).To(BeEmpty())
	}, agentRuntimeStateWait, 2*time.Second).Should(Succeed())
}

// waitRuntimePoolStopped ports pool_stopped, including the absence of runtime
// Pods.
func waitRuntimePoolStopped(name string) {
	EventuallyWithOffset(1, func(g Gomega) {
		pool := getRuntimePool(g, name)
		g.Expect(pool.Spec.DesiredReplicas).To(BeZero())
		g.Expect(pool.Status.DesiredReplicas).To(BeZero())
		g.Expect(pool.Status.CurrentReplicas).To(BeZero())
		g.Expect(string(pool.Status.Lifecycle)).To(Equal("Stopped"))
		g.Expect(string(pool.Status.AdmissionState)).To(Equal("Closed"))
		g.Expect(pool.Status.ActiveInstance).To(BeNil())
		capacity := pool.Status.Capacity
		for _, counter := range []int32{capacity.ResidentSessions, capacity.RunningPrompts, capacity.QueuedTasks,
			capacity.ReservedSessions, capacity.ReservedPrompts, capacity.PendingPermissions,
			capacity.FinalizingSessions, capacity.LiveDescendants} {
			g.Expect(counter).To(BeZero())
		}
		g.Expect(capacity.Reservations).To(BeEmpty())
		g.Expect(runtimePoolPods(g, pool)).To(BeEmpty())
	}, agentRuntimeStateWait, 2*time.Second).Should(Succeed())
}

func patchRuntimePoolReplicas(name string, replicas int) {
	_, err := utils.Run(exec.Command("kubectl", "patch", "runtimepool", name, "-n", namespace, "--type=merge",
		"-p", fmt.Sprintf(`{"spec":{"desiredReplicas":%d}}`, replicas)))
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
}

// parkRuntimePool and resumeRuntimePool port park_runtimepool and
// resume_runtimepool.
func parkRuntimePool(name string) {
	patchRuntimePoolReplicas(name, 0)
	waitRuntimePoolStopped(name)
}

func resumeRuntimePool(name string) {
	patchRuntimePoolReplicas(name, 1)
	waitRuntimePoolServing(name)
}

// parkProviderRuntimePoolsExcept ports park_provider_runtimepools_except.
func parkProviderRuntimePoolsExcept(provider, keep string) {
	var pools corev1alpha1.RuntimePoolList
	kubectlJSON(Default, &pools, "get", "runtimepools", "-n", namespace)
	for _, pool := range pools.Items {
		if pool.Spec.Runtime.Profile.ProviderKind == provider && pool.Name != keep {
			parkRuntimePool(pool.Name)
		}
	}
}

// addTaskObserverFinalizer ports add_task_observer_finalizer: the test holds
// the Task after deletion so it can see the controller's cleanup finish.
func addTaskObserverFinalizer(name, uid string) {
	task := getACPTask(Default, name)
	ExpectWithOffset(1, string(task.UID)).To(Equal(uid))
	ExpectWithOffset(1, task.DeletionTimestamp).To(BeNil())
	ExpectWithOffset(1, task.Finalizers).To(ContainElement(agentRuntimeCleanupFinalizer))
	ExpectWithOffset(1, task.Finalizers).NotTo(ContainElement(agentRuntimeObserverFinalizer))
	patch := fmt.Sprintf(`[{"op":"test","path":"/metadata/uid","value":%q},`+
		`{"op":"add","path":"/metadata/finalizers/-","value":%q}]`, uid, agentRuntimeObserverFinalizer)
	_, err := utils.Run(exec.Command("kubectl", "patch", "task", name, "-n", namespace, "--type=json", "-p", patch))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

// waitTaskObserverReleaseReady ports task_observer_release_ready: execution
// and delivery settled, and only the observer finalizer left.
func waitTaskObserverReleaseReady(name, uid string) {
	EventuallyWithOffset(1, func(g Gomega) {
		task := getACPTask(g, name)
		g.Expect(string(task.UID)).To(Equal(uid))
		g.Expect(task.DeletionTimestamp).NotTo(BeNil())
		g.Expect(task.Finalizers).To(Equal([]string{agentRuntimeObserverFinalizer}),
			"the controller must remove %s before the observer", agentRuntimeCleanupFinalizer)
		if task.Status.Execution != nil && task.Status.Execution.State != "" {
			g.Expect(task.Status.Execution.State).To(BeElementOf(terminalExecutionStates))
		}
		if task.Status.Delivery != nil && task.Status.Delivery.State != "" {
			g.Expect(task.Status.Delivery.State).To(BeElementOf(
				corev1alpha1.TaskDeliveryStateNotRequested, corev1alpha1.TaskDeliveryStateVerifiedExact,
				corev1alpha1.TaskDeliveryStateDeliveredSuperseded, corev1alpha1.TaskDeliveryStateReadValidated,
				corev1alpha1.TaskDeliveryStateNoChange, corev1alpha1.TaskDeliveryStateCancelledBeforePublish,
				corev1alpha1.TaskDeliveryStateReadOnlyWorkspaceModified, corev1alpha1.TaskDeliveryStateDeliveryConflict,
				corev1alpha1.TaskDeliveryStateCredentialBlocked, corev1alpha1.TaskDeliveryStatePublicationOutcomeUnknown,
			))
		}
	}, agentRuntimeStateWait, 2*time.Second).Should(Succeed())
}

func releaseTaskObserverFinalizer(name, uid string) {
	patch := fmt.Sprintf(`[{"op":"test","path":"/metadata/uid","value":%q},`+
		`{"op":"test","path":"/metadata/finalizers/0","value":%q},{"op":"remove","path":"/metadata/finalizers/0"}]`,
		uid, agentRuntimeObserverFinalizer)
	_, err := utils.Run(exec.Command("kubectl", "patch", "task", name, "-n", namespace, "--type=json", "-p", patch))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func waitTaskAbsent(name string) {
	EventuallyWithOffset(1, func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "get", "task", name, "-n", namespace,
			"--ignore-not-found", "-o", "name"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(output)).To(BeEmpty())
	}, agentRuntimeStateWait, 2*time.Second).Should(Succeed())
}

// archiveSessionViaAPI ports archive_test_sessions for one Session: the API
// must describe the same RuntimeSession every owning Task ran on, and was
// created no earlier than the Task that asked for it.
func archiveSessionViaAPI(apiBaseURL, token, session, runID string) {
	url := fmt.Sprintf("%s/api/v1/sessions/%s?namespace=%s", strings.TrimRight(apiBaseURL, "/"), session, namespace)
	body, status, err := doAuthorizedJSONRequest(http.MethodGet, url, token, "", "")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	if status == http.StatusNotFound {
		return
	}
	ExpectWithOffset(1, status).To(Equal(http.StatusOK), body)
	var described struct {
		Name             string    `json:"name"`
		Namespace        string    `json:"namespace"`
		CreatedAt        time.Time `json:"createdAt"`
		ExecutionControl struct {
			SessionUID string `json:"sessionUID"`
		} `json:"executionControl"`
	}
	ExpectWithOffset(1, json.Unmarshal([]byte(body), &described)).To(Succeed())
	ExpectWithOffset(1, described.Name).To(Equal(session))
	ExpectWithOffset(1, described.Namespace).To(Equal(namespace))

	var tasks corev1alpha1.TaskList
	kubectlJSON(Default, &tasks, "get", "tasks", "-n", namespace)
	uids := map[string]bool{}
	createdBy := false
	for _, task := range tasks.Items {
		if task.Spec.SessionRef == nil || task.Spec.SessionRef.Name != session {
			continue
		}
		ExpectWithOffset(1, task.Labels).To(HaveKeyWithValue(agentRuntimeRunLabel, runID),
			"Task %s uses the run's Session", task.Name)
		if task.Status.Execution != nil && task.Status.Execution.RuntimeSessionUID != "" {
			uids[task.Status.Execution.RuntimeSessionUID] = true
		}
		if task.Spec.SessionRef.Create && !task.CreationTimestamp.IsZero() &&
			!task.CreationTimestamp.After(described.CreatedAt) {
			createdBy = true
		}
	}
	ExpectWithOffset(1, uids).To(HaveLen(1), "Session %s owners ran on one RuntimeSession", session)
	ExpectWithOffset(1, uids).To(HaveKey(described.ExecutionControl.SessionUID))
	ExpectWithOffset(1, createdBy).To(BeTrue(), "a creating Task must predate Session %s", session)

	EventuallyWithOffset(1, func(g Gomega) {
		body, status, err := doAuthorizedJSONRequest(http.MethodDelete, url, token, "", "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(status).To(BeElementOf(http.StatusNoContent, http.StatusNotFound), body)
	}, agentRuntimeStateWait, 2*time.Second).Should(Succeed())
}

// agentRuntimeAgentManifest ports apply_agent.
func agentRuntimeAgentManifest(name, provider, model, runID string, maxTurns int, allowBash bool,
	opencodeLimits *corev1alpha1.ModelTokenLimits) map[string]any {
	runtime := map[string]any{"type": provider, "contractVersion": harnessV2Protocol, "defaultMaxTurns": maxTurns}
	if provider != "codex" {
		runtime["defaultAllowBash"] = allowBash
		switch {
		case provider == "opencode" && allowBash:
			runtime["defaultAllowedTools"] = []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
		case allowBash:
			runtime["defaultAllowedTools"] = []string{"Read", "Glob", "Grep", "Bash"}
		default:
			runtime["defaultAllowedTools"] = []string{"Read", "Glob", "Grep"}
		}
	}
	modelSpec := map[string]any{"name": model}
	if provider == "opencode" && opencodeLimits != nil {
		modelSpec["contextWindow"] = opencodeLimits.Context
		modelSpec["maxTokens"] = opencodeLimits.Output
	}
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1", "kind": "Agent",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{
			agentRuntimeRunLabel: runID, "app.kubernetes.io/managed-by": "agent-runtime-e2e",
		}},
		"spec": map[string]any{"runtime": runtime, "model": modelSpec},
	}
}

// agentRuntimeReadTask describes one read-intent Task, as apply_read_task
// builds it.
type agentRuntimeReadTask struct {
	Name, Agent, Provider, Prompt, Timeout string
	Session                                string
	CreateSession                          bool
	AllowBash                              bool
	MaxTurns                               int
	RepoURL, Ref, RepoIdentity             string
}

func (t agentRuntimeReadTask) manifest(runID string) map[string]any {
	agentRuntime := map[string]any{"maxTurns": t.MaxTurns}
	if t.Provider != "codex" || !t.AllowBash {
		agentRuntime["allowBash"] = t.AllowBash
		if t.AllowBash {
			agentRuntime["allowedTools"] = []string{"Read", "Glob", "Grep", "Bash"}
		} else {
			agentRuntime["allowedTools"] = []string{"Read", "Glob", "Grep"}
		}
	}
	workspace := map[string]any{"intent": "read", "gitRepo": t.RepoURL, "ref": t.Ref}
	if t.RepoIdentity != "" {
		workspace["sourceRepository"] = map[string]any{"provider": "github", "id": t.RepoIdentity}
	}
	spec := map[string]any{
		"type": "agent", "agentRef": map[string]any{"name": t.Agent}, "prompt": t.Prompt,
		"workspace": workspace, "agentRuntime": agentRuntime, "timeout": t.Timeout,
	}
	if t.Session != "" {
		spec["sessionRef"] = map[string]any{"name": t.Session, "create": t.CreateSession, "append": true}
	}
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
		"metadata": map[string]any{"name": t.Name, "namespace": namespace,
			"labels": map[string]any{agentRuntimeRunLabel: runID}},
		"spec": spec,
	}
}

func deploymentContainer(deployment appsv1.Deployment, names ...string) corev1.Container {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if slices.Contains(names, container.Name) {
			return container
		}
	}
	Fail(fmt.Sprintf("Deployment %s has none of the containers %v", deployment.Name, names))
	return corev1.Container{}
}

// expectDeploymentPersistent ports assert_deployment_persistent: the primary
// container mounts at least one PersistentVolumeClaim, and each is Bound.
func expectDeploymentPersistent(name string, containers ...string) {
	var deployment appsv1.Deployment
	kubectlJSON(Default, &deployment, "get", "deployment", name, "-n", namespace)
	mounted := map[string]bool{}
	for _, mount := range deploymentContainer(deployment, containers...).VolumeMounts {
		mounted[mount.Name] = true
	}
	var claims []string
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && mounted[volume.Name] {
			claims = append(claims, volume.PersistentVolumeClaim.ClaimName)
		}
	}
	ExpectWithOffset(1, claims).NotTo(BeEmpty(), "Deployment %s mounts no PersistentVolumeClaim", name)
	for _, claim := range claims {
		var pvc corev1.PersistentVolumeClaim
		kubectlJSON(Default, &pvc, "get", "persistentvolumeclaim", claim, "-n", namespace)
		ExpectWithOffset(1, pvc.Status.Phase).To(Equal(corev1.ClaimBound), "PersistentVolumeClaim %s", claim)
	}
}

// expectPublisherBrokeredAuthority ports assert_publisher_brokered_authority:
// the publisher reaches credentials and artifact authorization only through
// the controller's brokers, reaches SCMs only through the egress proxy, and
// cannot read Secrets itself.
func expectPublisherBrokeredAuthority() {
	var deployment appsv1.Deployment
	kubectlJSON(Default, &deployment, "get", "deployment", publisherDeployment, "-n", namespace)
	publisher := deploymentContainer(deployment, "publisher", "workspace-publisher")
	g := NewWithT(GinkgoT())
	count := func(match func(corev1.EnvVar) bool) int {
		n := 0
		for _, env := range publisher.Env {
			if match(env) {
				n++
			}
		}
		return n
	}
	brokerURL := regexp.MustCompile(`^https?://`)
	g.Expect(count(func(env corev1.EnvVar) bool {
		return env.Name == "ORKA_PUBLISHER_CREDENTIAL_BROKER_URL" && brokerURL.MatchString(env.Value)
	})).To(Equal(1))
	g.Expect(count(func(env corev1.EnvVar) bool {
		return env.Name == "ORKA_PUBLISHER_ARTIFACT_AUTHORIZATION_BROKER_URL" && brokerURL.MatchString(env.Value)
	})).To(Equal(1))
	g.Expect(count(func(env corev1.EnvVar) bool {
		return env.Name == "ORKA_PUBLISHER_CREDENTIAL_ROOT" || env.Name == "ORKA_PUBLISHER_ARTIFACT_CAPABILITY_SECRET_FILE"
	})).To(BeZero(), "the publisher must not hold workspace credentials or the artifact signing secret")
	g.Expect(publisher.EnvFrom).To(BeEmpty())
	g.Expect(count(func(env corev1.EnvVar) bool {
		return env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil
	})).To(Equal(1), "only the egress proxy token may come from a Secret")
	g.Expect(count(func(env corev1.EnvVar) bool {
		return env.Name == "ORKA_SCM_EGRESS_PROXY_TOKEN" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil &&
			env.ValueFrom.SecretKeyRef.Name != "" && env.ValueFrom.SecretKeyRef.Key != ""
	})).To(Equal(1))
	g.Expect(count(func(env corev1.EnvVar) bool {
		return (env.Name == "HTTPS_PROXY" || env.Name == "https_proxy") &&
			strings.Contains(env.Value, "$(ORKA_SCM_EGRESS_PROXY_TOKEN)") && strings.Contains(env.Value, "scm-egress-proxy")
	})).To(Equal(2))
	g.Expect(count(func(env corev1.EnvVar) bool {
		return env.Name == "ORKA_PUBLISHER_SCM_EGRESS_PROXY_REQUIRED" && env.Value == "true"
	})).To(Equal(1))
	g.Expect(strings.ToLower(strings.Join(publisher.Args, " "))).
		NotTo(MatchRegexp("credential-root|artifact-capability-secret"))

	secretVolumes := map[string]corev1.Volume{}
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			secretVolumes[volume.Name] = volume
		}
	}
	mountedSecrets := map[string]bool{}
	var authMounts []corev1.VolumeMount
	for _, mount := range publisher.VolumeMounts {
		if _, ok := secretVolumes[mount.Name]; ok {
			mountedSecrets[mount.Name] = true
		}
		if mount.Name == "publisher-auth" {
			authMounts = append(authMounts, corev1.VolumeMount{
				Name: mount.Name, MountPath: mount.MountPath, SubPath: mount.SubPath, ReadOnly: mount.ReadOnly})
		}
	}
	g.Expect(mountedSecrets).To(Equal(map[string]bool{"publisher-auth": true}))
	authPaths := make([]string, 0, len(secretVolumes["publisher-auth"].Secret.Items))
	for _, item := range secretVolumes["publisher-auth"].Secret.Items {
		authPaths = append(authPaths, item.Path)
	}
	g.Expect(authPaths).To(ConsistOf("controller-token", "operation-capability-secret"))
	g.Expect(authMounts).To(ConsistOf(
		corev1.VolumeMount{Name: "publisher-auth", MountPath: "/var/run/orka/publisher-auth/controller-token",
			SubPath: "controller-token", ReadOnly: true},
		corev1.VolumeMount{Name: "publisher-auth", MountPath: "/var/run/orka/publisher-auth/operation-capability-secret",
			SubPath: "operation-capability-secret", ReadOnly: true},
	))

	account := deployment.Spec.Template.Spec.ServiceAccountName
	if account == "" {
		account = "default"
	}
	for _, verb := range []string{"get", "list"} {
		output, _ := exec.Command("kubectl", "auth", "can-i", verb, "secrets", "--all-namespaces",
			fmt.Sprintf("--as=system:serviceaccount:%s:%s", namespace, account)).CombinedOutput()
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		g.Expect(strings.TrimSpace(lines[len(lines)-1])).To(Equal("no"), "the publisher can %s Secrets", verb)
	}
}
