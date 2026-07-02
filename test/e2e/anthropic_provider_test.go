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

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Anthropic Provider AI Tasks", Ordered, func() {
	const (
		providerName = "e2e-anthropic-ai-provider"
		taskName     = "e2e-anthropic-ai-task"
	)

	AfterAll(func() {
		By("cleaning up Anthropic provider test resources")
		for _, name := range []string{taskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		cmd := exec.Command("kubectl", "delete", "provider", providerName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
	})

	It("should complete an AI task using an Anthropic provider via OpenAI-compatible proxy", func() {
		skipIfNoKey("E2E_ANTHROPIC_API_KEY")

		model := e2eAnthropicModel
		if model == "" {
			model = "claude-sonnet-4"
		}

		By("creating an Anthropic Provider CRD pointing to the proxy")
		createProviderCRD(providerName, "anthropic", "e2e-anthropic-secret", "ANTHROPIC_API_KEY", e2eAnthropicBaseURL, model)

		By("creating an AI task with an Anthropic model")
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
					"prompt": "What is 3+3? Reply with only the number.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, taskName, namespace, model, providerName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create AI task with Anthropic provider")

		By("waiting for the AI task to complete (up to 3 minutes)")
		phase := waitForTaskCompletion(taskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "AI task with Anthropic provider should succeed")

		By("verifying the result is available")
		verifyResultAvailable(taskName)
	})
})
