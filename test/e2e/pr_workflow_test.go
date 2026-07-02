//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("PR Workflow Tools", Ordered, func() {
	var (
		timestamp      string
		branchName     string
		prNumber       string
		apiBaseURL     string
		token          string
		cancelPF       context.CancelFunc
		portForwardCmd *exec.Cmd
	)

	const (
		prProviderName = "e2e-pr-provider"
		prCoordAgent   = "e2e-pr-coordinator"
		prCoderAgent   = "e2e-pr-coder"
	)

	BeforeAll(func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")
		skipIfNoKey("E2E_GITHUB_TOKEN")

		timestamp = fmt.Sprintf("%d", time.Now().Unix())
		branchName = "e2e/orka-" + timestamp

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a shared provider for PR workflow tests")
		createProviderCRD(prProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating the coder agent with workspace config")
		coderManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"runtime": {
					"type": "copilot",
					"defaultMaxTurns": 20,
					"defaultAllowBash": true
				},
				"secretRef": {"name": "e2e-github-secret"}
			}
		}`, prCoderAgent, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coderManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create coder agent")

		By("creating the coordinator agent with coordination and PR tools")
		coordManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"providerRef": {"name": "%s"},
				"model": {"name": "%s"},
				"coordination": {
					"enabled": true,
					"maxDepth": 2,
					"maxConcurrentChildren": 3,
					"allowedAgents": [{"name": "%s"}]
				}
			}
		}`, prCoordAgent, namespace, prProviderName, model, prCoderAgent)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create coordinator agent")

		By("setting up port-forward to controller API")
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18087)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")

		By("getting a service account token")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
	})

	AfterAll(func() {
		By("cleaning up PR workflow resources")
		// Clean up child tasks
		for _, taskPrefix := range []string{"e2e-pr-create", "e2e-pr-review", "e2e-pr-comment", "e2e-pr-merge"} {
			cmd := exec.Command("kubectl", "delete", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskPrefix), "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}

		// Clean up tasks
		for _, t := range []string{"e2e-pr-create", "e2e-pr-review", "e2e-pr-comment", "e2e-pr-merge", "e2e-pr-wsenv-task"} {
			cmd := exec.Command("kubectl", "delete", "task", t, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}

		// Clean up agents
		for _, a := range []string{prCoordAgent, prCoderAgent, "e2e-pr-wsenv-agent"} {
			cmd := exec.Command("kubectl", "delete", "agent", a, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}

		// Clean up provider
		cmd := exec.Command("kubectl", "delete", "provider", prProviderName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Stop port-forward
		if cancelPF != nil {
			cancelPF()
		}
		if portForwardCmd != nil && portForwardCmd.Process != nil {
			_ = portForwardCmd.Wait()
		}
	})

	AfterEach(func() {
		dumpDebugInfo("e2e-pr-create", "e2e-pr-review", "e2e-pr-comment", "e2e-pr-merge")
	})

	// ── Test 1: create_pull_request ──

	It("should create a pull request using create_pull_request tool", func() {
		const taskName = "e2e-pr-create"

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a coordinator task to clone, push, and create a PR")
		prompt := fmt.Sprintf(
			"Delegate to agent '%s': Clone https://github.com/sozercan/ayna, create a new file e2e-test-%s.txt with content 'e2e test file', commit with message 'e2e: add test file', and push to branch %s. After the coder task completes, use create_pull_request with the coder's task name, head_branch='%s', base_branch='main', title='E2E Test PR %s'.",
			prCoderAgent, timestamp, branchName, branchName, timestamp,
		)

		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "ai",
				"agentRef": {"name": "%s"},
				"ai": {
					"prompt": "%s",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName, namespace, prCoordAgent, prompt, model, prProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for coordinator task to complete")
		phase := waitForTaskCompletion(taskName, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"))

		By("verifying result is available")
		verifyResultAvailable(taskName)

		By("verifying at least one child task was created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks", "-l",
				fmt.Sprintf("orka.ai/parent-task=%s", taskName),
				"-o", "jsonpath={.items[*].metadata.name}", "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
				"At least one child task should be created")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("extracting PR number from result")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks/"+taskName+"/result", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		// Extract PR number from result text
		re := regexp.MustCompile(`#(\d+)|PR\s+(\d+)|pull/(\d+)|number["\s:]+(\d+)`)
		matches := re.FindStringSubmatch(string(body))
		for _, m := range matches[1:] {
			if m != "" {
				prNumber = m
				break
			}
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "Extracted PR number: %s\n", prNumber)
		Expect(prNumber).NotTo(BeEmpty(), "Should extract a PR number from result: %s", string(body))
	})

	// ── Test 2: review_pull_request ──

	It("should review a pull request using review_pull_request tool", func() {
		Expect(prNumber).NotTo(BeEmpty(), "PR number must be set by previous test")

		const taskName = "e2e-pr-review"

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a coordinator task to review the PR")
		prompt := fmt.Sprintf(
			"Use review_pull_request to review PR number %s from the coder task. Report the diff and file list.",
			prNumber,
		)

		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "ai",
				"agentRef": {"name": "%s"},
				"ai": {
					"prompt": "%s",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName, namespace, prCoordAgent, prompt, model, prProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for coordinator task to complete")
		phase := waitForTaskCompletion(taskName, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"))

		By("verifying result is available")
		verifyResultAvailable(taskName)
	})

	// ── Test 3: post_review_comment ──

	It("should post a review comment using post_review_comment tool", func() {
		Expect(prNumber).NotTo(BeEmpty(), "PR number must be set by previous test")

		const taskName = "e2e-pr-comment"

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a coordinator task to post a review comment")
		prompt := fmt.Sprintf(
			"Use post_review_comment on PR number %s with body='E2E test review - LGTM' and event='COMMENT'.",
			prNumber,
		)

		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "ai",
				"agentRef": {"name": "%s"},
				"ai": {
					"prompt": "%s",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName, namespace, prCoordAgent, prompt, model, prProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for coordinator task to complete")
		phase := waitForTaskCompletion(taskName, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"))

		By("verifying result is available")
		verifyResultAvailable(taskName)
	})

	// ── Test 4: merge_pull_request ──

	It("should merge a pull request using merge_pull_request tool", func() {
		Expect(prNumber).NotTo(BeEmpty(), "PR number must be set by previous test")

		const taskName = "e2e-pr-merge"

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a coordinator task to merge the PR")
		prompt := fmt.Sprintf(
			"Use merge_pull_request to merge PR number %s with merge_method='squash'.",
			prNumber,
		)

		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "ai",
				"agentRef": {"name": "%s"},
				"ai": {
					"prompt": "%s",
					"model": "%s",
					"providerRef": {"name": "%s"}
				}
			}
		}`, taskName, namespace, prCoordAgent, prompt, model, prProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for coordinator task to complete")
		phase := waitForTaskCompletion(taskName, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"))

		By("verifying result is available")
		verifyResultAvailable(taskName)
	})

	// ── Test 5: Workspace env vars (structural – no LLM needed) ──

	It("should set workspace env vars on the Job for PR workflow fields", func() {
		const (
			agentName = "e2e-pr-wsenv-agent"
			taskName  = "e2e-pr-wsenv-task"
		)

		defer func() {
			cmd := exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}()

		By("creating an Agent with claude runtime")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"runtime": {
					"type": "claude",
					"defaultMaxTurns": 5,
					"defaultAllowBash": false
				}
			}
		}`, agentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent")

		By("creating a Task with workspace gitRepo, pushBranch, forkRepo, and prBaseBranch")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "list files",
				"agentRef": {"name": "%s"},
				"agentRuntime": {
					"maxTurns": 3,
					"workspace": {
						"gitRepo": "https://github.com/sozercan/ayna",
						"pushBranch": "e2e/test-branch",
						"forkRepo": "https://github.com/fork/ayna",
						"prBaseBranch": "develop"
					}
				}
			}
		}`, taskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Task")

		By("verifying a Job is created")
		verifyJobCreatedForTask(taskName, 2*time.Minute)

		By("verifying the Job has workspace env vars")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", taskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
				"-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())

			var envVars []envVar
			err = json.Unmarshal([]byte(output), &envVars)
			g.Expect(err).NotTo(HaveOccurred(), "Failed to parse env vars JSON")

			envMap := make(map[string]string)
			for _, e := range envVars {
				envMap[e.Name] = e.Value
			}

			g.Expect(envMap).To(HaveKey("ORKA_GIT_REPO"))
			g.Expect(envMap["ORKA_GIT_REPO"]).To(Equal("https://github.com/sozercan/ayna"))
			g.Expect(envMap).To(HaveKey("ORKA_PUSH_BRANCH"))
			g.Expect(envMap["ORKA_PUSH_BRANCH"]).To(Equal("e2e/test-branch"))
			g.Expect(envMap).To(HaveKey("ORKA_FORK_REPO"))
			g.Expect(envMap["ORKA_FORK_REPO"]).To(Equal("https://github.com/fork/ayna"))
			g.Expect(envMap).To(HaveKey("ORKA_PR_BASE_BRANCH"))
			g.Expect(envMap["ORKA_PR_BASE_BRANCH"]).To(Equal("develop"))
		}, 30*time.Second, time.Second).Should(Succeed())
	})
})
