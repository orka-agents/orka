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

	"github.com/orka-agents/orka/internal/store"
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

		// aimock fixture markers (test/e2e/testdata/aimock/autonomous_mode.json).
		autoGoalMarker    = "[e2e:auto-goal]"
		autoMaxIterMarker = "[e2e:auto-maxiter]"
		autoEnvMarker     = "[e2e:auto-env]"
		autoSuspendMarker = "[e2e:auto-suspend]"
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
					"prompt": "You are a coordinator. Delegate the computation of 2+2 to the agent named '%s' using delegate_task. Then delegate the computation of 3+3 to the same agent. Wait for both results using wait_for_tasks. Once you have both results, call update_plan with goal_complete=true and include the results in the summary. %s",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, autoTaskName, namespace, autoCoordinatorName, autoWorkerName, autoGoalMarker, model, autoProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create autonomous task")

		By("waiting for the autonomous task to succeed")
		phase := waitForTaskCompletion(autoTaskName, 10*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Autonomous task should succeed")

		By("verifying the loop recorded how it ended")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", autoTaskName,
				"-o", "jsonpath={.status.message}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			// status.iteration is 0-based and omitted at 0, so the completion
			// message is the record that at least one iteration ran.
			g.Expect(output).To(SatisfyAny(
				HavePrefix("goal complete after "),
				HavePrefix("reached max iterations"),
			))
		}, 30*time.Second, time.Second).Should(Succeed())

		if e2eMockOpenAI {
			By("verifying the scripted loop completed its goal on the second iteration")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "task", autoTaskName,
					"-o", `jsonpath={.status.iteration}{"/"}{.status.message}`,
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal(
					"1/goal complete after 2 iterations: Both delegated computations finished: 2+2=4 and 3+3=6."))
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying the second iteration received the plan saved by the first")
			Eventually(func(g Gomega) {
				g.Expect(autonomousMockMessages(g, autoGoalMarker, "user")).To(ContainElement(SatisfyAll(
					ContainSubstring("## Previous Plan State"),
					ContainSubstring("Delegated 2+2 and 3+3 to the worker agent."),
					ContainSubstring("Phase 1: delegated both computations"),
				)))
				g.Expect(autonomousMockToolResults(g, autoGoalMarker, "update_plan")).To(ConsistOf(
					"Plan updated: Delegated 2+2 and 3+3 to the worker agent. (progress: 50%)",
					"Plan updated: Both delegated computations finished: 2+2=4 and 3+3=6. (progress: 100%, goal marked as COMPLETE)",
				))
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying both delegate_task calls created child tasks that succeeded")
			Eventually(func(g Gomega) {
				g.Expect(autonomousMockToolResults(g, autoGoalMarker, "delegate_task")).To(HaveLen(2))
				g.Expect(autonomousChildPhases(g, autoTaskName)).To(Equal([]string{"Succeeded", "Succeeded"}))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
		}

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

	It("should clean up plan state once the autonomous task completes", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		By("setting up port-forward to controller API")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18084)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")

		By("getting a service account token for auth")
		token, err := serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		// The controller deletes plan state when a Task completes, so the
		// Plan API serves it only while the loop is still running (see the
		// suspend spec).
		By("querying the Plan API for the completed autonomous task")
		Eventually(func(g Gomega) {
			status, _ := fetchAutonomousPlan(g, apiBaseURL, token, autoTaskName)
			g.Expect(status).To(Equal(http.StatusNotFound))
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
					"prompt": "You are a coordinator. Each iteration, delegate a simple math problem (like 1+1) to the agent named '%s' using delegate_task. Wait for the result. Do NOT call update_plan with goal_complete=true. Just keep delegating tasks. %s",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, autoMaxIterTask, namespace, autoMaxIterCoord, autoWorkerName, autoMaxIterMarker, model, autoProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create max-iter task")

		By("waiting for the task to reach a terminal phase")
		phase := waitForTaskCompletion(autoMaxIterTask, 10*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"),
			"Task should reach terminal phase after maxIterations")

		By("verifying the loop stopped on its last allowed iteration")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", autoMaxIterTask,
				"-o", "jsonpath={.status.iteration}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			// status.iteration is 0-based: maxIterations=2 runs iterations 0 and 1.
			g.Expect(output).To(Equal("1"),
				"The final iteration index should be maxIterations-1")
		}, 30*time.Second, time.Second).Should(Succeed())

		if e2eMockOpenAI {
			By("verifying the controller ended the loop at maxIterations")
			Expect(phase).To(Equal("Succeeded"))
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "task", autoMaxIterTask,
					"-o", "jsonpath={.status.message}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("reached max iterations (2)"))
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying both iterations ran and neither marked the goal complete")
			Eventually(func(g Gomega) {
				g.Expect(autonomousMockMessages(g, autoMaxIterMarker, "system")).To(SatisfyAll(
					ContainElement(ContainSubstring("Current iteration: 0 of 2")),
					ContainElement(ContainSubstring("Current iteration: 1 of 2")),
					Not(ContainElement(ContainSubstring("Current iteration: 2 of 2"))),
				))
				g.Expect(autonomousMockToolResults(g, autoMaxIterMarker, "update_plan")).To(ConsistOf(
					"Plan updated: Delegated another 1+1 computation; the goal stays open. (progress: 10%)",
				))
				g.Expect(autonomousChildPhases(g, autoMaxIterTask)).To(Equal([]string{"Succeeded", "Succeeded"}))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
		}
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
					"prompt": "say hello %s",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, autoEnvTaskName, namespace, autoEnvCoordName, autoEnvMarker, model, autoProviderName)

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

		if e2eMockOpenAI {
			By("verifying the scripted update_plan ends the loop after one iteration")
			Expect(waitForTaskCompletion(autoEnvTaskName, 5*time.Minute)).To(Equal("Succeeded"))
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "task", autoEnvTaskName,
					"-o", "jsonpath={.status.message}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("goal complete after 1 iterations: Said hello."))
				g.Expect(autonomousMockToolResults(g, autoEnvMarker, "update_plan")).To(ConsistOf(
					"Plan updated: Said hello. (progress: 100%, goal marked as COMPLETE)",
				))
			}, 30*time.Second, time.Second).Should(Succeed())
		}
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
					"prompt": "You are a coordinator. Each iteration, delegate a simple task (compute 1+1) to the agent named '%s' using delegate_task. Wait for the result. Do NOT call update_plan with goal_complete=true. Keep delegating each iteration. %s",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, autoSuspendTask, namespace, autoSuspendCoord, autoWorkerName, autoSuspendMarker, model, autoProviderName)

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

		By("waiting for the controller to park the loop at the next iteration boundary")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", autoSuspendTask,
				"-o", `jsonpath={.status.phase}{"/"}{.status.message}`,
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			// Suspend takes effect between iterations and deliberately keeps the
			// task Running so it can resume when spec.suspend is cleared.
			g.Expect(output).To(HavePrefix("Running/autonomous task suspended at iteration "),
				"Task should be parked as suspended")
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

		if e2eMockOpenAI {
			By("verifying every completed iteration delegated and kept the goal open")
			Eventually(func(g Gomega) {
				g.Expect(autonomousMockToolResults(g, autoSuspendMarker, "update_plan")).To(ConsistOf(
					"Plan updated: Delegated another 1+1 computation; the goal stays open. (progress: 10%)",
				))
				g.Expect(autonomousMockToolResults(g, autoSuspendMarker, "delegate_task")).NotTo(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying the Plan API serves the parked loop's latest plan")
			if apiBaseURL == "" {
				var pfErr error
				apiBaseURL, cancelPF, portForwardCmd, pfErr = startControllerAPIPortForward(18084)
				Expect(pfErr).NotTo(HaveOccurred())
			}
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				status, body := fetchAutonomousPlan(g, apiBaseURL, token, autoSuspendTask)
				g.Expect(status).To(Equal(http.StatusOK), "plan body: %s", body)
				var plan store.PlanState
				g.Expect(json.Unmarshal(body, &plan)).To(Succeed())
				g.Expect(plan.GoalComplete).To(BeFalse())
				g.Expect(plan.ProgressPct).To(Equal(10))
				g.Expect(plan.Summary).To(Equal("Delegated another 1+1 computation; the goal stays open."))
			}, 30*time.Second, time.Second).Should(Succeed())
		}
	})
})

// fetchAutonomousPlan reads a Task's plan through the Plan API and returns the
// HTTP status and body.
func fetchAutonomousPlan(g Gomega, apiBaseURL, token, taskName string) (int, []byte) {
	req, err := http.NewRequest(http.MethodGet, apiBaseURL+"/api/v1/tasks/"+taskName+"/plan", nil)
	g.Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	g.Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	g.Expect(err).NotTo(HaveOccurred())
	return resp.StatusCode, body
}

// autonomousMockMessages returns the text of every role message the model
// received in the conversation marked by marker.
func autonomousMockMessages(g Gomega, marker, role string) []string {
	requests, err := mockLLMRequestsFor(marker)
	g.Expect(err).NotTo(HaveOccurred())
	var texts []string
	for _, request := range requests {
		for _, message := range request.Body.Messages {
			if message.Role == role {
				texts = append(texts, message.Text())
			}
		}
	}
	return texts
}

// autonomousMockToolResults returns each distinct toolName output the model
// received in the conversation marked by marker.
func autonomousMockToolResults(g Gomega, marker, toolName string) []string {
	requests, err := mockLLMRequestsFor(marker)
	g.Expect(err).NotTo(HaveOccurred())
	seen := map[string]bool{}
	var results []string
	for _, request := range requests {
		calls := map[string]string{}
		for _, message := range request.Body.Messages {
			for _, call := range message.ToolCalls {
				calls[call.ID] = call.Function.Name
			}
			if message.Role != "tool" || calls[message.ToolCallID] != toolName || seen[message.Text()] {
				continue
			}
			seen[message.Text()] = true
			results = append(results, message.Text())
		}
	}
	return results
}

// autonomousChildPhases returns the phases of the Tasks delegated by parent.
func autonomousChildPhases(g Gomega, parent string) []string {
	cmd := exec.Command("kubectl", "get", "tasks",
		"-l", fmt.Sprintf("orka.ai/parent-task=%s", parent),
		"-o", `jsonpath={range .items[*]}{.status.phase}{"\n"}{end}`,
		"-n", namespace,
	)
	output, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())
	return utils.GetNonEmptyLines(output)
}
