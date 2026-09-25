//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/test/utils"
)

func gatewayE2EManagerArgs(name string) ([]string, error) {
	var deployment appsv1.Deployment
	if err := gatewayE2EGetKubernetesJSON("deployment", name, true, &deployment); err != nil {
		return nil, err
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "manager" {
			return container.Args, nil
		}
	}
	return nil, fmt.Errorf("manager container not found")
}

func gatewayE2EFixtureArgs(original []string) []string {
	args := make([]string, 0, len(original)+2)
	for i := 0; i < len(original); i++ {
		if original[i] == "--ai-worker-image" || original[i] == "--gateway-terminal-retention" {
			i++
			continue
		}
		if !strings.HasPrefix(original[i], "--ai-worker-image=") && !strings.HasPrefix(original[i], "--gateway-terminal-retention=") {
			args = append(args, original[i])
		}
	}
	return append(args, "--ai-worker-image="+gatewayNativeWorkerImage,
		"--gateway-terminal-retention="+gatewayE2ETerminalRetention.String())
}

func gatewayE2ESetManagerArgs(name string, args []string) error {
	return gatewayE2EPatchDeployment(name, map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "manager", "args": args}}}}}})
}

func gatewayE2ENativeAgentManifest(name string) map[string]any {
	// No runtime or workspace: the gateway must create an ordinary native Job
	// without worker-image init entrypoints or external model credentials.
	// Its SessionRef still requires the standard fetch-session init container.
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1", "kind": "Agent",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     map[string]any{"model": map[string]any{"provider": "openai", "name": "test-only-not-called"}},
	}
}

func waitForGatewayE2ECapability(enabled bool) {
	Eventually(func(g Gomega) {
		var gateway gatewayv1alpha1.Gateway
		g.Expect(gatewayE2EGetKubernetesJSON("gateway", gatewayE2EName, true, &gateway)).To(Succeed())
		g.Expect(gateway.Status.Ready).To(BeTrue())
		g.Expect(gateway.Status.ObservedGeneration).To(Equal(gateway.Generation))
		g.Expect(gateway.Status.ObservedCapabilities).NotTo(BeNil())
		g.Expect(gateway.Status.ObservedCapabilities.Capabilities.InterimDelivery).To(Equal(enabled))
	}, 3*time.Minute, time.Second).Should(Succeed())
}

func gatewayE2EAdmitNativeEvent(baseURL, token, eventName string, cleanup *gatewayE2ECleanup) string {
	envelope := protocol.EventEnvelope{ProtocolVersion: protocol.Version, ExternalEventID: eventName, EventType: protocol.EventTypeText, AccountID: "acct-ci", ContextID: "room-ci", ThreadID: eventName, Sender: protocol.Sender{ID: "sender-ci"}, Text: "Exercise the deterministic native worker.", ReplyTarget: "reply-ci"}
	data, err := json.Marshal(envelope)
	Expect(err).NotTo(HaveOccurred())
	endpoint := fmt.Sprintf("%s/api/v1/gateways/%s/%s/events", strings.TrimRight(baseURL, "/"), namespace, gatewayE2EName)
	Expect(cleanup.beginIngress()).To(Succeed())
	body, status, err := doAuthorizedJSONRequest(http.MethodPost, endpoint, token, string(data), "")
	Expect(err).NotTo(HaveOccurred())
	Expect(status).To(Equal(http.StatusAccepted))
	var accepted protocol.IngressResponse
	Expect(json.Unmarshal([]byte(body), &accepted)).To(Succeed())
	Expect(accepted.Status).To(Equal("accepted"))
	Expect(accepted.EventID).NotTo(BeEmpty())
	Expect(cleanup.finishIngress(accepted.EventID)).To(Succeed())
	return accepted.EventID
}

func gatewayE2EWorkerCommand(pod, action string) (string, error) {
	return utils.Run(exec.Command("kubectl", "exec", pod, "-n", namespace, "-c", "worker", "--", "/worker", "--"+action))
}

func gatewayE2EVerifyNativeMessage(baseURL, token, eventID, taskName string, capable bool) {
	By("verifying the gateway-created Task and its authentic current Job/Pod identity")
	waitForTaskPhase(taskName, "Running", 3*time.Minute)
	task, err := gatewayE2EGetTask(taskName)
	Expect(err).NotTo(HaveOccurred())
	Expect(task.Spec.Type).To(Equal(corev1alpha1.TaskTypeAI))
	Expect(task.Spec.Prompt).To(BeEmpty())
	Expect(task.Spec.SessionRef).NotTo(BeNil())
	Expect(task.Spec.SessionRef.PromptIncluded).To(BeTrue())
	Expect(task.Status.JobName).NotTo(BeEmpty())
	Expect(task.Status.JobUID).NotTo(BeEmpty())
	var job batchv1.Job
	Expect(gatewayE2EGetKubernetesJSON("job", task.Status.JobName, true, &job)).To(Succeed())
	Expect(string(job.UID)).To(Equal(task.Status.JobUID))
	Expect(job.OwnerReferences).To(ContainElement(HaveField("UID", task.UID)))
	var pod corev1.Pod
	Eventually(func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", namespace, "-l", "batch.kubernetes.io/job-name="+job.Name, "-o", "json"))
		g.Expect(err).NotTo(HaveOccurred())
		var pods corev1.PodList
		g.Expect(json.Unmarshal([]byte(output), &pods)).To(Succeed())
		g.Expect(pods.Items).To(HaveLen(1))
		pod = pods.Items[0]
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		g.Expect(pod.OwnerReferences).To(ContainElement(HaveField("UID", job.UID)))
		g.Expect(pod.Spec.Containers[0].Image).To(Equal(gatewayNativeWorkerImage))
	}, time.Minute, time.Second).Should(Succeed())
	// The test drives only fixture-local fences using Kubernetes exec. All writes
	// to the controller are made by /worker with the real Pod-bound projected token.
	_, err = gatewayE2EWorkerCommand(pod.Name, "start")
	Expect(err).NotTo(HaveOccurred())
	var report struct {
		First     gatewayruntime.TaskMessageReceipt `json:"first"`
		Replay    gatewayruntime.TaskMessageReceipt `json:"replay"`
		ErrorCode string                            `json:"errorCode"`
	}
	Eventually(func(g Gomega) {
		output, err := gatewayE2EWorkerCommand(pod.Name, "inspect")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(json.Unmarshal([]byte(output), &report)).To(Succeed())
	}, 2*time.Minute, time.Second).Should(Succeed())
	var interim store.GatewayDelivery
	if capable {
		Expect(report.ErrorCode).To(BeEmpty())
		Expect(report.First.Created).To(BeTrue())
		Expect(report.First.DeliveryID).NotTo(BeEmpty())
		Expect(report.Replay.Created).To(BeFalse())
		Expect(report.Replay.DeliveryID).To(Equal(report.First.DeliveryID))
		By("observing the provider receipt before allowing final completion")
		interim = waitForGatewayE2EDelivery(baseURL, token, eventID, 3*time.Minute)
		Expect(interim.ID).To(Equal(report.First.DeliveryID))
		Expect(interim.Kind).To(Equal(protocol.DeliveryKindMessage))
		Expect(interim.Text).To(Equal("Gateway interim E2E message"))
		Expect(interim.AttemptCount).To(Equal(1))
		Expect(interim.ProviderMessageID).To(Equal("reference:" + interim.ID))
	} else {
		Expect(report.ErrorCode).To(Equal("interim_delivery_unsupported"))
		Expect(report.First.DeliveryID).To(BeEmpty())
		Expect(listGatewayE2EDeliveries(baseURL, token, eventID)).To(BeEmpty())
	}
	active, err := gatewayE2EGetTask(taskName)
	Expect(err).NotTo(HaveOccurred())
	Expect(active.Status.Phase).To(Equal(corev1alpha1.TaskPhaseRunning))
	Expect(active.Status.ExecutionOutcome).To(BeNil())
	Expect(active.Spec.Prompt).To(BeEmpty())
	Expect(active.Annotations).NotTo(HaveKey(gatewayruntime.TaskGatewayDelivery))
	Expect(active.Annotations).NotTo(HaveKey(gatewayruntime.TaskGatewayProviderMessage))
	event, err := getGatewayE2EEvent(baseURL, token, eventID)
	Expect(err).NotTo(HaveOccurred())
	Expect(event.State).To(Equal(store.GatewayEventTaskCreated))
	Expect(event.DeliveryID).To(BeEmpty())
	Expect(event.ProviderMessageID).To(BeEmpty())
	By("releasing the fixture final only after the active-task assertions")
	_, err = gatewayE2EWorkerCommand(pod.Name, "release")
	Expect(err).NotTo(HaveOccurred())
	waitForTaskPhase(taskName, "Succeeded", 3*time.Minute)
	completed := waitForGatewayE2ECompletedEvent(baseURL, token, eventID, 3*time.Minute)
	var terminal store.GatewayDelivery
	Eventually(func(g Gomega) {
		deliveries, err := getGatewayE2EDeliveries(baseURL, token, eventID)
		g.Expect(err).NotTo(HaveOccurred())
		want := 1
		if capable {
			want = 2
		}
		g.Expect(deliveries).To(HaveLen(want))
		// The API is newest-first, not provider order. The active receipt/release
		// fence above is the ordering proof; never infer send order from this list.
		for _, delivery := range deliveries {
			g.Expect(delivery.State).To(Equal(store.GatewayDeliveryDelivered))
			if delivery.Kind == protocol.DeliveryKindFinal {
				terminal = delivery
			} else {
				g.Expect(capable).To(BeTrue())
				g.Expect(delivery.ID).To(Equal(interim.ID))
			}
		}
		g.Expect(terminal.ID).NotTo(BeEmpty())
	}, 3*time.Minute, time.Second).Should(Succeed())
	Expect(terminal.Text).To(Equal("Gateway interim E2E final"))
	Expect(terminal.AttemptCount).To(Equal(1))
	Expect(terminal.ProviderMessageID).To(Equal("reference:" + terminal.ID))
	Expect(completed.DeliveryID).To(Equal(terminal.ID))
	Expect(completed.ProviderMessageID).To(Equal(terminal.ProviderMessageID))
	Eventually(func(g Gomega) {
		current, err := gatewayE2EGetTask(taskName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(current.Annotations).To(HaveKeyWithValue(gatewayruntime.TaskGatewayDelivery, terminal.ID))
	}, time.Minute, time.Second).Should(Succeed())
}
