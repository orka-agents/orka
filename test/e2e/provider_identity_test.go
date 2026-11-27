//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/test/fixtures/fakeentra"
	"github.com/orka-agents/orka/test/utils"
)

const (
	keylessProviderE2EName     = "e2e-keyless-azure"
	keylessProviderE2EIdentity = "e2e-models"
	keylessProviderE2EModel    = "gpt-e2e"
	keylessProviderE2ETask     = "e2e-keyless-ai-task"
	keylessProviderE2EReadOnly = "e2e-keyless-read-only-task"
)

var _ = Describe("Keyless Azure OpenAI Provider with a fake Entra", Ordered, Serial, Label("provider-identity"),
	func() {
		var apiBaseURL, apiToken string

		BeforeAll(func() {
			By("deploying the fake Entra behind a serving certificate from a fresh test CA")
			assertion := "fx-controller-credential-" + fmt.Sprint(time.Now().UnixNano())
			caPEM := deployFakeEntra(assertion)
			DeferCleanup(deleteE2EObjects, "secret", fakeEntraTrust)
			Expect(createK8sSecret(fakeEntraTrust, namespace, map[string]string{
				"ca.crt": string(caPEM), "assertion": assertion,
			})).To(Succeed())

			By("configuring the controller with the fake's workload identity for Providers")
			identities, err := json.Marshal(map[string]any{"identities": []any{map[string]any{
				"name": keylessProviderE2EIdentity, "tenantID": fakeEntraTenant, "clientID": fakeEntraWorkload,
				"authority": "https://" + fakeEntraHost(), "allowedEndpoints": []string{"https://" + fakeEntraHost()},
			}}})
			Expect(err).NotTo(HaveOccurred())
			configureControllerWithFakeEntraTrust(map[string]string{
				"provider-identities":          string(identities),
				"provider-identity-token-file": fakeEntraTrustDir + "/assertion",
			})

			var cancel func()
			var pf *exec.Cmd
			apiBaseURL, cancel, pf, err = startControllerAPIPortForward(18147)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { stopPortForward(cancel, pf) })
			apiToken, err = serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			dumpDebugInfo(keylessProviderE2ETask, keylessProviderE2EReadOnly)
		})

		It("verifies the Provider's identity without a Secret", func() {
			Expect(applyManifestJSON(map[string]any{
				"apiVersion": corev1alpha1.GroupVersion.String(), "kind": "Provider",
				"metadata": map[string]any{"name": keylessProviderE2EName, "namespace": namespace},
				"spec": map[string]any{
					"type":    string(corev1alpha1.ProviderTypeAzureOpenAI),
					"baseURL": "https://" + fakeEntraHost(),
					"azure":   map[string]any{"deploymentName": keylessProviderE2EModel},
					"auth":    map[string]any{"entra": map[string]any{"identity": keylessProviderE2EIdentity}},
				},
			})).To(Succeed())

			Eventually(func(g Gomega) {
				var provider corev1alpha1.Provider
				g.Expect(getE2EJSON(&provider, "provider", keylessProviderE2EName)).To(Succeed())
				g.Expect(provider.Status.Ready).To(BeTrue(), provider.Status.Message)
				credential := meta.FindStatusCondition(provider.Status.Conditions, "CredentialReady")
				g.Expect(credential).NotTo(BeNil())
				g.Expect(credential.Status).To(Equal(metav1.ConditionTrue), credential.Message)
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			requests := fakeEntraState().Requests
			Expect(requests).To(ContainElement(SatisfyAll(
				HaveField("Step", "workload"), HaveField("Identity", fakeEntraWorkload),
				HaveField("Scope", fakeEntraModels+"/.default"), HaveField("Error", ""),
			)), "the controller should have obtained a workload token for the model resource")
		})

		It("runs a type: ai Task against the keyless Provider through the controller relay", func() {
			DeferCleanup(deleteE2EObjects, "task", keylessProviderE2ETask)
			marker := fmt.Sprintf("[e2e:keyless-provider-%d]", time.Now().UnixNano())
			task := keylessProviderTask(keylessProviderE2ETask, "Repeat this marker: "+marker, false)
			Expect(applyManifestJSON(task)).To(Succeed())

			Expect(waitForTaskCompletion(keylessProviderE2ETask, 3*time.Minute)).To(Equal("Succeeded"))
			verifyResultAvailable(keylessProviderE2ETask)
			result, err := getTaskResultViaAPI(apiBaseURL, apiToken, keylessProviderE2ETask)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(ContainSubstring(fakeentra.ModelReplyPrefix),
				"the answer should come from the fake Azure OpenAI endpoint")

			By("checking that every model call carried the identity's token and no key")
			state := fakeEntraState()
			Expect(state.Models).NotTo(BeEmpty())
			for _, call := range state.Models {
				Expect(call.Authorized).To(BeTrue(), "model call to %s was not authorized", call.Path)
				Expect(call.Identity).To(Equal(fakeEntraWorkload))
				Expect(call.APIKey).To(BeFalse())
			}
			chatPath := "/openai/deployments/" + keylessProviderE2EModel + "/chat/completions"
			Expect(state.Models).To(ContainElement(HaveField("Path", chatPath)))

			By("checking that no issued token reached the Task, its Job, or its worker Pod")
			var exposed []string
			for _, kind := range []string{"task", "job", "pod"} {
				args := []string{"get", kind, "-n", namespace, "-o", "json"}
				if kind == "task" {
					args = []string{"get", "task", keylessProviderE2ETask, "-n", namespace, "-o", "json"}
				} else {
					args = append(args, "-l", taskSelector(keylessProviderE2ETask))
				}
				output, err := exec.Command("kubectl", args...).Output()
				Expect(err).NotTo(HaveOccurred())
				exposed = append(exposed, string(output))
			}
			logs, err := exec.Command("kubectl", "logs", "-n", namespace, "-l", taskSelector(keylessProviderE2ETask),
				"--all-containers", "--tail=-1").Output()
			Expect(err).NotTo(HaveOccurred(), "the worker logs must be readable to check them for tokens")
			Expect(logs).NotTo(BeEmpty())
			exposed = append(exposed, string(logs))
			Expect(state.Issued).NotTo(BeEmpty())
			for _, token := range state.Issued {
				for _, text := range exposed {
					Expect(strings.Contains(text, token)).To(BeFalse(), "an Entra token reached the Task, Job, Pod, or worker logs")
				}
			}

			By("checking that the worker Pod got the relay and no key")
			var pods corev1.PodList
			output, err := exec.Command("kubectl", "get", "pods", "-n", namespace,
				"-l", taskSelector(keylessProviderE2ETask), "-o", "json").Output()
			Expect(err).NotTo(HaveOccurred())
			Expect(json.Unmarshal(output, &pods)).To(Succeed())
			Expect(pods.Items).NotTo(BeEmpty())
			sawRelayEnv := false
			for _, container := range pods.Items[0].Spec.Containers {
				for _, env := range container.Env {
					Expect(env.Name).NotTo(Equal("OPENAI_API_KEY"))
					if env.Name == "ORKA_AI_PROVIDER_RELAY" {
						Expect(env.Value).To(ContainSubstring(keylessProviderE2EName))
						sawRelayEnv = true
					}
					if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
						Expect(env.ValueFrom.SecretKeyRef.Name).NotTo(ContainSubstring(keylessProviderE2EName))
					}
				}
			}
			Expect(sawRelayEnv).To(BeTrue(), "the worker Pod should receive ORKA_AI_PROVIDER_RELAY for the keyless Provider")
		})

		It("refuses a read-only AI Task, whose worker cannot authenticate to the relay", func() {
			DeferCleanup(deleteE2EObjects, "task", keylessProviderE2EReadOnly)
			Expect(applyManifestJSON(keylessProviderTask(keylessProviderE2EReadOnly, "Say hello.", true))).To(Succeed())
			Expect(waitForTaskCompletion(keylessProviderE2EReadOnly, 2*time.Minute)).To(Equal("Failed"))
			output, err := utils.Run(exec.Command("kubectl", "get", "task", keylessProviderE2EReadOnly, "-n", namespace,
				"-o", "jsonpath={.status.message}"))
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("spec.auth"))
		})

		AfterAll(func() {
			deleteE2EObjects("provider", keylessProviderE2EName)
		})
	})

func keylessProviderTask(name, prompt string, readOnly bool) map[string]any {
	metadata := map[string]any{"name": name, "namespace": namespace}
	if readOnly {
		metadata["annotations"] = map[string]any{labels.AnnotationAgentReadOnly: "true"}
	}
	return map[string]any{
		"apiVersion": corev1alpha1.GroupVersion.String(), "kind": "Task", "metadata": metadata,
		"spec": map[string]any{"type": "ai", "ai": map[string]any{
			"prompt": prompt, "model": keylessProviderE2EModel,
			"providerRef": map[string]any{"name": keylessProviderE2EName},
		}},
	}
}

func taskSelector(taskName string) string {
	return labels.LabelTask + "=" + labels.SelectorValue(taskName)
}
