//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Security Enforcement", Ordered, func() {
	const (
		nonRootTaskName  = "e2e-sec-nonroot"
		readOnlyTaskName = "e2e-sec-readonly"
		denyPatternTask  = "e2e-sec-denypattern"
		denyProviderName = "e2e-sec-provider"
	)

	AfterAll(func() {
		By("cleaning up security enforcement test resources")
		for _, name := range []string{nonRootTaskName, readOnlyTaskName, denyPatternTask} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		cmd := exec.Command("kubectl", "delete", "provider", denyProviderName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(nonRootTaskName, readOnlyTaskName, denyPatternTask)
	})

	It("should run worker containers as non-root (uid 1000)", func() {
		By("creating a container task that prints the user id")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "container",
				"image": "busybox:latest",
				"command": ["id", "-u"]
			}
		}`, nonRootTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create non-root check task")

		By("waiting for task to complete")
		phase := waitForTaskCompletion(nonRootTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Non-root uid check task should succeed")

		By("verifying the worker ran as uid 1000")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "logs", "-l", fmt.Sprintf("orka.ai/task=%s", nonRootTaskName),
				"-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(ContainSubstring("1000"),
				"Worker should run as non-root uid 1000")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should enforce a read-only root filesystem on worker containers", func() {
		By("creating a container task that attempts to write to a read-only path")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "container",
				"image": "busybox:latest",
				"command": ["touch", "/etc/e2e-test-file"]
			}
		}`, readOnlyTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create read-only filesystem task")

		By("waiting for task to reach terminal phase")
		phase := waitForTaskCompletion(readOnlyTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Failed"), "Writing to read-only filesystem should cause task failure")
	})

	It("should block dangerous commands via code_exec deny patterns", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("ensuring provider exists for deny pattern test")
		createProviderCRD(denyProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating an AI task that asks to run a destructive command")
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
					"prompt": "Use the code_exec tool to run this bash command: rm -rf /",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, denyPatternTask, namespace, model, denyProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create deny pattern task")

		By("waiting for AI task to complete")
		phase := waitForTaskCompletion(denyPatternTask, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "AI task should succeed even when deny pattern blocks command")

		By("verifying result is available")
		verifyResultAvailable(denyPatternTask)
	})

	It("should reject chat requests targeting kube-system namespace", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		var (
			apiBaseURL     string
			portForwardCmd *exec.Cmd
			cancelPF       context.CancelFunc
			token          string
			err            error
		)

		By("setting up port-forward to controller API")
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18088)
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			stopPortForward(cancelPF, portForwardCmd)
		}()

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		By("sending a chat request with kube-system namespace")
		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}
		chatBody := fmt.Sprintf(`{
			"message": "Hello",
			"provider": "%s",
			"model": "%s",
			"namespace": "kube-system"
		}`, denyProviderName, model)

		req, err := http.NewRequest("POST", apiBaseURL+"/api/v1/chat",
			strings.NewReader(chatBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		Expect(resp.StatusCode).To(Equal(http.StatusForbidden),
			"Chat requests to kube-system namespace should be rejected with 403")
	})
})
