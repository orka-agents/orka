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

var _ = Describe("Provider CRD Lifecycle", Ordered, func() {
	const (
		openaiProviderName    = "e2e-openai-provider"
		anthropicProviderName = "e2e-anthropic-provider"
		openaiTaskName        = "e2e-provider-openai-task"
		anthropicTaskName     = "e2e-provider-anthropic-task"
	)

	AfterAll(func() {
		By("cleaning up provider test resources")
		for _, name := range []string{openaiTaskName, anthropicTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		for _, name := range []string{openaiProviderName, anthropicProviderName} {
			cmd := exec.Command("kubectl", "delete", "provider", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	AfterEach(func() {
		dumpDebugInfo(openaiTaskName, anthropicTaskName)
	})

	It("should complete an AI task using an OpenAI provider", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an OpenAI Provider CRD")
		createProviderCRD(openaiProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("verifying the Provider is created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "provider", openaiProviderName,
				"-n", namespace, "-o", "jsonpath={.metadata.name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(openaiProviderName))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("creating an AI task referencing the OpenAI provider")
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
					"prompt": "What is 2+2? Reply with just the number.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, openaiTaskName, namespace, model, openaiProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create AI task with OpenAI provider")

		By("waiting for the AI task to complete (up to 5 minutes)")
		phase := waitForTaskCompletion(openaiTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "AI task with OpenAI provider should succeed")

		By("verifying the result is available")
		verifyResultAvailable(openaiTaskName)
	})

	It("should complete an AI task using an Anthropic provider", func() {
		skipIfNoKey("E2E_ANTHROPIC_API_KEY")

		model := e2eAnthropicModel
		if model == "" {
			model = "claude-sonnet-4-20250514"
		}

		By("creating an Anthropic Provider CRD")
		createProviderCRD(anthropicProviderName, "anthropic", "e2e-anthropic-secret", "ANTHROPIC_API_KEY", e2eAnthropicBaseURL, model)

		By("creating an AI task referencing the Anthropic provider")
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
					"prompt": "What is 2+2? Reply with just the number.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, anthropicTaskName, namespace, model, anthropicProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create AI task with Anthropic provider")

		By("waiting for the AI task to complete (up to 5 minutes)")
		phase := waitForTaskCompletion(anthropicTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "AI task with Anthropic provider should succeed")

		By("verifying the result is available")
		verifyResultAvailable(anthropicTaskName)
	})
})
