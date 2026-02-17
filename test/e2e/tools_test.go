//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Tools and Configuration", Ordered, func() {
	const (
		searchTaskName    = "e2e-tool-search"
		fileReadTaskName  = "e2e-tool-fileread"
		customToolName    = "e2e-custom-echo"
		customToolTask    = "e2e-custom-tool-task"
		filterAgentName   = "e2e-filter-agent"
		filterTaskName    = "e2e-filter-task"
		priorTask1Name    = "e2e-prior-task-1"
		priorTask2Name    = "e2e-prior-task-2"
		priorAgentName    = "e2e-prior-agent"
		toolProviderName  = "e2e-tool-provider"
		webFetchTaskName  = "e2e-tool-webfetch"
		fileWriteTaskName = "e2e-tool-filewrite"
	)

	AfterAll(func() {
		By("cleaning up tools test resources")
		for _, name := range []string{searchTaskName, fileReadTaskName, customToolTask, filterTaskName, priorTask1Name, priorTask2Name, webFetchTaskName, fileWriteTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		for _, name := range []string{filterAgentName, priorAgentName} {
			cmd := exec.Command("kubectl", "delete", "agent", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		cmd := exec.Command("kubectl", "delete", "tool", customToolName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "provider", toolProviderName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(searchTaskName, fileReadTaskName, customToolTask, filterTaskName, priorTask1Name, priorTask2Name, webFetchTaskName, fileWriteTaskName)
	})

	// Test: AI task using web_search tool
	It("should execute an AI task that uses the web_search tool", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("ensuring provider exists")
		createProviderCRD(toolProviderName, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, model)

		By("creating an AI task that asks to search the web")
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
					"prompt": "Use the web_search tool to search for 'Kubernetes container orchestration'. Summarize what you find in 2-3 sentences.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, searchTaskName, namespace, model, toolProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for task to complete")
		phase := waitForTaskCompletion(searchTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Web search AI task should succeed")

		By("verifying result is stored")
		verifyResultAvailable(searchTaskName)
	})

	// Test: AI task using file_read tool
	It("should execute an AI task that uses the file_read tool", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an AI task that reads a file")
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
					"prompt": "First use the code_exec tool to create a file at /tmp/e2e-read-test.txt with the content 'hello from file_read test'. Then use the file_read tool to read /tmp/e2e-read-test.txt and tell me what it contains.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, fileReadTaskName, namespace, model, toolProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for task to complete")
		phase := waitForTaskCompletion(fileReadTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "File read AI task should succeed")

		By("verifying result is stored")
		verifyResultAvailable(fileReadTaskName)
	})

	// Test: Custom Tool CRD
	It("should create and use a custom Tool CRD", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("deploying an in-cluster echo receiver for the custom tool")
		echoReceiverName := "e2e-echo-receiver"
		echoSvcName := "e2e-echo-svc"
		echoPort := 9090
		receiverManifest := fmt.Sprintf(`{
			"apiVersion": "v1",
			"kind": "Pod",
			"metadata": {
				"name": "%s",
				"namespace": "%s",
				"labels": {"app": "%s"}
			},
			"spec": {
				"containers": [{
					"name": "echo",
					"image": "python:3-alpine",
					"command": ["python3", "-c"],
					"args": ["import http.server, json\nclass H(http.server.BaseHTTPRequestHandler):\n  def do_POST(self):\n    length = int(self.headers.get('Content-Length', 0))\n    body = self.rfile.read(length)\n    self.send_response(200)\n    self.send_header('Content-Type','application/json')\n    self.end_headers()\n    self.wfile.write(json.dumps({'echo': body.decode()}).encode())\nhttp.server.HTTPServer(('', %d), H).serve_forever()"],
					"ports": [{"containerPort": %d}],
					"securityContext": {
						"readOnlyRootFilesystem": true,
						"allowPrivilegeEscalation": false,
						"capabilities": {"drop": ["ALL"]},
						"runAsNonRoot": true,
						"runAsUser": 1000,
						"seccompProfile": {"type": "RuntimeDefault"}
					}
				}],
				"securityContext": {
					"runAsNonRoot": true,
					"seccompProfile": {"type": "RuntimeDefault"}
				}
			}
		}`, echoReceiverName, namespace, echoReceiverName, echoPort, echoPort)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(receiverManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create echo receiver pod")

		svcManifest := fmt.Sprintf(`{
			"apiVersion": "v1",
			"kind": "Service",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"selector": {"app": "%s"},
				"ports": [{"port": %d, "targetPort": %d}]
			}
		}`, echoSvcName, namespace, echoReceiverName, echoPort, echoPort)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(svcManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create echo service")

		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", echoReceiverName,
				"-n", namespace, "-o", "jsonpath={.status.phase}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"))
		}, 2*time.Minute, time.Second).Should(Succeed())

		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "pod", echoReceiverName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "service", echoSvcName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		echoURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", echoSvcName, namespace, echoPort)

		By("creating a custom Tool CRD that calls the in-cluster echo endpoint")
		toolManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Tool",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"description": "A test tool that echoes back the input. Use this when asked to echo something.",
				"parameters": {
					"type": "object",
					"properties": {
						"message": {
							"type": "string",
							"description": "The message to echo"
						}
					},
					"required": ["message"]
				},
				"http": {
					"url": "%s",
					"method": "POST",
					"timeout": "10s"
				}
			}
		}`, customToolName, namespace, echoURL)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(toolManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create custom Tool CRD")

		By("verifying the Tool CRD is created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "tool", customToolName,
				"-n", namespace, "-o", "jsonpath={.metadata.name}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(customToolName))
		}, 30*time.Second, time.Second).Should(Succeed())

		By("creating an AI task that uses the custom tool")
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
					"prompt": "Use the %s tool to echo the message 'hello from custom tool'. Report what the tool returned.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, customToolTask, namespace, customToolName, model, toolProviderName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for task to complete")
		phase := waitForTaskCompletion(customToolTask, 5*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"),
			"Custom tool task should reach terminal phase")
	})

	// Test: Agent tool filtering via allowedTools/disallowedTools
	It("should pass tool filtering configuration to the Job", func() {
		By("creating an Agent with tool filtering")
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
					"defaultMaxTurns": 3,
					"defaultAllowBash": false
				}
			}
		}`, filterAgentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating a Task with allowedTools and disallowedTools")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "say hello",
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 1,
					"allowedTools": ["Read", "Grep"],
					"disallowedTools": ["Bash", "Write"]
				}
			}
		}`, filterTaskName, namespace, filterAgentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("verifying the Job has tool filtering env vars")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", filterTaskName),
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

			g.Expect(envMap).To(HaveKey("ORKA_ALLOWED_TOOLS"),
				"Job should have ORKA_ALLOWED_TOOLS env var")
			g.Expect(envMap["ORKA_ALLOWED_TOOLS"]).To(ContainSubstring("Read"))
			g.Expect(envMap["ORKA_ALLOWED_TOOLS"]).To(ContainSubstring("Grep"))

			g.Expect(envMap).To(HaveKey("ORKA_DISALLOWED_TOOLS"),
				"Job should have ORKA_DISALLOWED_TOOLS env var")
			g.Expect(envMap["ORKA_DISALLOWED_TOOLS"]).To(ContainSubstring("Bash"))
			g.Expect(envMap["ORKA_DISALLOWED_TOOLS"]).To(ContainSubstring("Write"))
		}, 2*time.Minute, time.Second).Should(Succeed())
	})

	// Test: PriorTaskRef chaining
	It("should set ORKA_PRIOR_TASK env var when priorTaskRef is specified", func() {
		By("creating an Agent for prior task test")
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
					"defaultMaxTurns": 3,
					"defaultAllowBash": false
				}
			}
		}`, priorAgentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating the first task (will be referenced as prior)")
		task1Manifest := fmt.Sprintf(`{
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
				"args": ["prior-task-output"]
			}
		}`, priorTask1Name, namespace)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task1Manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the first task to complete")
		waitForTaskCompletion(priorTask1Name, 3*time.Minute)

		By("creating a second task with priorTaskRef")
		task2Manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "continue from prior task",
				"agentRef": {
					"name": "%s"
				},
				"priorTaskRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 1
				}
			}
		}`, priorTask2Name, namespace, priorAgentName, priorTask1Name)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(task2Manifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("verifying the Job has ORKA_PRIOR_TASK env var")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", priorTask2Name),
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

			g.Expect(envMap).To(HaveKey("ORKA_PRIOR_TASK"),
				"Job should have ORKA_PRIOR_TASK env var")
			g.Expect(envMap["ORKA_PRIOR_TASK"]).To(Equal(priorTask1Name))
			g.Expect(envMap).To(HaveKey("ORKA_PRIOR_TASK_NAMESPACE"))
			g.Expect(envMap["ORKA_PRIOR_TASK_NAMESPACE"]).To(Equal(namespace))
		}, 2*time.Minute, time.Second).Should(Succeed())
	})

	// Test: AI task using web_fetch tool
	It("should execute an AI task that uses the web_fetch tool", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an AI task that uses web_fetch")
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
					"prompt": "Use the web_fetch tool to fetch https://httpbin.org/get and summarize the response.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, webFetchTaskName, namespace, model, toolProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for task to complete")
		phase := waitForTaskCompletion(webFetchTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Web fetch AI task should succeed")

		By("verifying result is stored")
		verifyResultAvailable(webFetchTaskName)
	})

	// Test: AI task using file_write tool
	It("should execute an AI task that uses the file_write tool", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an AI task that uses file_write")
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
					"prompt": "Use the file_write tool to write 'e2e test' to /tmp/e2e-write-test.txt, then file_read it and tell me the contents.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					}
				}
			}
		}`, fileWriteTaskName, namespace, model, toolProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for task to complete")
		phase := waitForTaskCompletion(fileWriteTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "File write AI task should succeed")

		By("verifying result is stored")
		verifyResultAvailable(fileWriteTaskName)
	})
})
