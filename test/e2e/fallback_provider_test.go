//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Model Fallback Providers", Ordered, func() {
	const (
		primaryProviderName  = "e2e-fallback-primary"
		fallbackProviderName = "e2e-fallback-secondary"
		agentName            = "e2e-fallback-agent"
		taskName             = "e2e-fallback-task"
	)

	AfterAll(func() {
		By("cleaning up fallback test resources")
		cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		for _, p := range []string{primaryProviderName, fallbackProviderName} {
			cmd = exec.Command("kubectl", "delete", "provider", p, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
	})

	It("should inject fallback provider env vars into the Job", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a primary Provider CRD")
		createProviderCRD(primaryProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating a fallback Provider CRD")
		createProviderCRD(fallbackProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating an Agent with fallback configuration")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"providerRef": {
					"name": "%s"
				},
				"model": {
					"name": "%s",
					"fallbacks": [
						{
							"providerRef": "%s",
							"model": "%s"
						}
					]
				}
			}
		}`, agentName, namespace, primaryProviderName, model, fallbackProviderName, model)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent with fallbacks")

		By("creating an AI task referencing the agent")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"ai": {
					"prompt": "What is 5+5? Reply with just the number.",
					"providerRef": {
						"name": "%s"
					},
					"model": "%s"
				},
				"agentRef": {
					"name": "%s"
				}
			}
		}`, taskName, namespace, primaryProviderName, model, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create fallback test task")

		By("verifying the Job has fallback env vars")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
				"-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("ORKA_AI_FALLBACK_COUNT"),
				"Job should have ORKA_AI_FALLBACK_COUNT env var")
			g.Expect(output).To(ContainSubstring("ORKA_AI_FALLBACK_0_PROVIDER"),
				"Job should have ORKA_AI_FALLBACK_0_PROVIDER env var")
			g.Expect(output).To(ContainSubstring("ORKA_AI_FALLBACK_0_MODEL"),
				"Job should have ORKA_AI_FALLBACK_0_MODEL env var")
		}, 60*time.Second, 2*time.Second).Should(Succeed())

		By("waiting for the task to complete successfully")
		phase := waitForTaskCompletion(taskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Task with fallback providers should succeed")
	})
})
