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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("Autonomous Mode", Ordered, func() {
	const (
		autoProviderName    = "e2e-auto-provider"
		autoWorkerName      = "e2e-auto-worker"
		autoCoordinatorName = "e2e-auto-coordinator"
		autoTaskName        = "e2e-auto-task"
		autoMaxIterTask     = "e2e-auto-maxiter-task"
		autoMaxIterCoord    = "e2e-auto-maxiter-coord"
		autoEnvTaskName     = "e2e-auto-env-task"
		autoEnvCoordName    = "e2e-auto-env-coord"
		autoContainerTask   = "e2e-auto-container-task"
		autoSuspendTask     = "e2e-auto-suspend-task"
		autoSuspendCoord    = "e2e-auto-suspend-coord"
	)

	var (
		apiBaseURL     string
		portForwardCmd *exec.Cmd
		cancelPF       context.CancelFunc
	)

	AfterAll(func() {
		By("stopping port-forward")
		if cancelPF != nil {
			cancelPF()
		}
		if portForwardCmd != nil && portForwardCmd.Process != nil {
			_ = portForwardCmd.Wait()
		}

		By("cleaning up autonomous mode test resources")
		for _, name := range []string{autoTaskName, autoMaxIterTask, autoEnvTaskName, autoContainerTask, autoSuspendTask} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			// Clean up child tasks
			cmd = exec.Command("kubectl", "delete", "tasks", "-l", fmt.Sprintf("orka.ai/parent-task=%s", name),
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		for _, name := range []string{autoWorkerName, autoCoordinatorName, autoMaxIterCoord, autoEnvCoordName, autoSuspendCoord} {
			cmd := exec.Command("kubectl", "delete", "agent", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		cmd := exec.Command("kubectl", "delete", "provider", autoProviderName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(autoTaskName, autoMaxIterTask, autoEnvTaskName, autoContainerTask, autoSuspendTask)
	})

	It("should complete an autonomous loop that reaches the goal", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an OpenAI Provider")
		createProviderCRD(autoProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating a worker agent")
		workerManifest := fmt.Sprintf(`{
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
					"name": "%s"
				}
			}
		}`, autoWorkerName, namespace, autoProviderName, model)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(workerManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create worker agent")

		By("creating a coordinator agent with autonomous mode enabled")
		coordManifest := fmt.Sprintf(`{
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
					"name": "%s"
				},
				"coordination": {
					"enabled": true,
					"autonomous": true,
					"maxIterations": 5,
					"allowedAgents": [
						{"name": "%s"}
					]
				}
			}
		}`, autoCoordinatorName, namespace, autoProviderName, model, autoWorkerName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create coordinator agent")

		By("creating an autonomous AI task")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"agentRef": {
					"name": "%s"
				},
				"ai": {
					"prompt": "You are a coordinator. Delegate the computation of 2+2 to the agent named '%s' using delegate_task. Then delegate the computation of 3+3 to the same agent. Wait for both results using wait_for_tasks. Once you have both results, call update_plan with goal_complete=true and include the results in the summary.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, autoTaskName, namespace, autoCoordinatorName, autoWorkerName, model, autoProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create autonomous task")

		By("waiting for the autonomous task to succeed")
		phase := waitForTaskCompletion(autoTaskName, 10*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Autonomous task should succeed")

		By("verifying iteration count is at least 1")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", autoTaskName,
				"-o", "jsonpath={.status.iteration}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty(), "Iteration count should be set")
			// Iteration is 0-based, so even "0" means at least one iteration ran
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying child tasks were created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tasks",
				"-l", fmt.Sprintf("orka.ai/parent-task=%s", autoTaskName),
				"-o", "jsonpath={.items[*].metadata.name}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).NotTo(BeEmpty(),
				"At least one child task should be created via delegation")
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying result is available")
		verifyResultAvailable(autoTaskName)
	})

	It("should return plan state via Plan API after completion", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18084)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")

		By("getting a service account token for auth")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		By("querying the Plan API for the autonomous task")
		Eventually(func(g Gomega) {
			req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks/"+autoTaskName+"/plan", nil)
			g.Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			g.Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

			body, err := io.ReadAll(resp.Body)
			g.Expect(err).NotTo(HaveOccurred())
			bodyStr := string(body)
			// Plan response should contain goal completion and progress info
			g.Expect(bodyStr).To(SatisfyAny(
				ContainSubstring("goal_complete"),
				ContainSubstring("GoalComplete"),
				ContainSubstring("goalComplete"),
			))
			g.Expect(bodyStr).To(SatisfyAny(
				ContainSubstring("ProgressPct"),
				ContainSubstring("progressPct"),
				ContainSubstring("progress_pct"),
			))
			g.Expect(bodyStr).To(SatisfyAny(
				ContainSubstring("Summary"),
				ContainSubstring("summary"),
				ContainSubstring("PlanDocument"),
				ContainSubstring("planDocument"),
			))
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should terminate the loop when maxIterations is reached", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a coordinator agent with maxIterations=2")
		coordManifest := fmt.Sprintf(`{
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
					"name": "%s"
				},
				"coordination": {
					"enabled": true,
					"autonomous": true,
					"maxIterations": 2,
					"allowedAgents": [
						{"name": "%s"}
					]
				}
			}
		}`, autoMaxIterCoord, namespace, autoProviderName, model, autoWorkerName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create max-iter coordinator agent")

		By("creating a task that never marks goal complete")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"agentRef": {
					"name": "%s"
				},
				"ai": {
					"prompt": "You are a coordinator. Each iteration, delegate a simple math problem (like 1+1) to the agent named '%s' using delegate_task. Wait for the result. Do NOT call update_plan with goal_complete=true. Just keep delegating tasks.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, autoMaxIterTask, namespace, autoMaxIterCoord, autoWorkerName, model, autoProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create max-iter task")

		By("waiting for the task to reach a terminal phase")
		phase := waitForTaskCompletion(autoMaxIterTask, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"),
			"Task should reach terminal phase after maxIterations")

		By("verifying iteration count is 2")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", autoMaxIterTask,
				"-o", "jsonpath={.status.iteration}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("2"),
				"Iteration count should be exactly 2 (maxIterations)")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should set autonomous environment variables on the Job", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a coordinator agent with autonomous mode and maxIterations=10")
		coordManifest := fmt.Sprintf(`{
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
					"name": "%s"
				},
				"coordination": {
					"enabled": true,
					"autonomous": true,
					"maxIterations": 10,
					"allowedAgents": [
						{"name": "%s"}
					]
				}
			}
		}`, autoEnvCoordName, namespace, autoProviderName, model, autoWorkerName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create env-check coordinator agent")

		By("creating a task with prompt 'say hello'")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"agentRef": {
					"name": "%s"
				},
				"ai": {
					"prompt": "say hello",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, autoEnvTaskName, namespace, autoEnvCoordName, model, autoProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create env-check task")

		By("waiting for a Job to be created")
		verifyJobCreatedForTask(autoEnvTaskName, 2*time.Minute)

		By("verifying autonomous env vars on the Job")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", autoEnvTaskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())

			var envVars []envVar
			err = json.Unmarshal([]byte(output), &envVars)
			g.Expect(err).NotTo(HaveOccurred())

			envMap := make(map[string]string)
			for _, e := range envVars {
				envMap[e.Name] = e.Value
			}

			g.Expect(envMap).To(HaveKeyWithValue("ORKA_AUTONOMOUS_MODE", "true"),
				"Job should have ORKA_AUTONOMOUS_MODE=true")
			g.Expect(envMap).To(HaveKey("ORKA_AUTONOMOUS_ITERATION"),
				"Job should have ORKA_AUTONOMOUS_ITERATION set")
			g.Expect(envMap).To(HaveKeyWithValue("ORKA_AUTONOMOUS_MAX_ITERATIONS", "10"),
				"Job should have ORKA_AUTONOMOUS_MAX_ITERATIONS=10")
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("should return 404 from Plan API for a non-autonomous task", func() {
		By("creating a simple container task")
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
				"command": ["echo"],
				"args": ["not-autonomous"]
			}
		}`, autoContainerTask, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create container task")

		By("waiting for the container task to complete")
		waitForTaskCompletion(autoContainerTask, 3*time.Minute)

		// Ensure port-forward is available (may have been set up in earlier test)
		if apiBaseURL == "" {
			By("setting up port-forward to controller API")
			var pfErr error
			apiBaseURL, cancelPF, portForwardCmd, pfErr = startControllerAPIPortForward(18084)
			Expect(pfErr).NotTo(HaveOccurred())
		}

		By("getting a service account token for auth")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		By("querying the Plan API for the non-autonomous task")
		req, err := http.NewRequest("GET", apiBaseURL+"/api/v1/tasks/"+autoContainerTask+"/plan", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
			"Plan API should return 404 for non-autonomous task")
	})

	It("should stop creating new iterations when task is suspended", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating a coordinator agent with autonomous mode and maxIterations=20")
		coordManifest := fmt.Sprintf(`{
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
					"name": "%s"
				},
				"coordination": {
					"enabled": true,
					"autonomous": true,
					"maxIterations": 20,
					"allowedAgents": [
						{"name": "%s"}
					]
				}
			}
		}`, autoSuspendCoord, namespace, autoProviderName, model, autoWorkerName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(coordManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create suspend coordinator agent")

		By("creating a task that never marks goal complete")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "ai",
				"agentRef": {
					"name": "%s"
				},
				"ai": {
					"prompt": "You are a coordinator. Each iteration, delegate a simple task (compute 1+1) to the agent named '%s' using delegate_task. Wait for the result. Do NOT call update_plan with goal_complete=true. Keep delegating each iteration.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, autoSuspendTask, namespace, autoSuspendCoord, autoWorkerName, model, autoProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create suspend task")

		By("waiting for the task to start running")
		waitForTaskPhase(autoSuspendTask, "Running", 5*time.Minute)

		By("suspending the task")
		cmd = exec.Command("kubectl", "patch", "task", autoSuspendTask,
			"-n", namespace, "--type=merge", "-p", `{"spec":{"suspend":true}}`)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to suspend autonomous task")

		By("verifying the task is no longer Running after suspend")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", autoSuspendTask,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(Equal("Running"),
				"Task should not be Running after suspend")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("verifying the task stopped creating new iterations")
		// Record current iteration count
		cmd = exec.Command("kubectl", "get", "task", autoSuspendTask,
			"-o", "jsonpath={.status.iteration}",
			"-n", namespace,
		)
		iterBefore, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		// Wait and verify iteration count doesn't increase
		time.Sleep(15 * time.Second)

		cmd = exec.Command("kubectl", "get", "task", autoSuspendTask,
			"-o", "jsonpath={.status.iteration}",
			"-n", namespace,
		)
		iterAfter, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(iterAfter).To(Equal(iterBefore),
			"Iteration count should not increase after suspend")
	})
})
