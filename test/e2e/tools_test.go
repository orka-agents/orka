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

	"github.com/orka-agents/orka/test/utils"
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
					"prompt": "Use the web_search tool to search for 'Kubernetes container orchestration'. Summarize what you find in 2-3 sentences. [e2e:tools-web-search]",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					},
					"tools": ["web_search"]
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

		if e2eMockOpenAI {
			By("verifying the web_search results reached the model")
			// Without SEARCH_API_URL the worker searches DuckDuckGo and falls
			// back to canned results, so only the result shape is stable.
			output := expectMockLLMToolResult("[e2e:tools-web-search]", "web_search", 30*time.Second)
			var results []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
			}
			Expect(json.Unmarshal([]byte(output), &results)).To(Succeed(), "web_search output: %s", output)
			Expect(results).NotTo(BeEmpty(), "web_search output: %s", output)
			Expect(results[0].URL).NotTo(BeEmpty())
		}
	})

	// Test: AI task using file_read tool
	It("should execute an AI task that uses the file_read tool", func() {
		skipIfNoKey("E2E_OPENAI_API_KEY")

		model := e2eOpenAIModel
		if model == "" {
			model = "gpt-4o-mini"
		}

		By("creating an AI task that reads a file")
		// code_exec runs in its own Pod by default, so a file it writes is not
		// visible to file_read in the worker. Stage the file with file_write.
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
					"prompt": "First use the file_write tool to create a file at /tmp/e2e-read-test.txt with the content 'hello from file_read test'. Then use the file_read tool to read /tmp/e2e-read-test.txt and tell me what it contains. [e2e:tools-file-read]",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					},
					"tools": ["file_write", "file_read"]
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

		if e2eMockOpenAI {
			By("verifying file_read returned the requested byte range")
			// The fixture reads 9 bytes at offset 11 of "hello from file_read test".
			output := expectMockLLMToolResult("[e2e:tools-file-read]", "file_read", 30*time.Second)
			var result struct {
				Content   string `json:"content"`
				Truncated bool   `json:"truncated"`
			}
			Expect(json.Unmarshal([]byte(output), &result)).To(Succeed(), "file_read output: %s", output)
			Expect(result.Content).To(Equal("file_read"))
			Expect(result.Truncated).To(BeTrue(), "a partial read should report truncation")
		}
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
					"prompt": "Use the %s tool to echo the message 'hello from custom tool'. Report what the tool returned. [e2e:tools-custom-tool]",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					},
					"tools": [%q]
				}
			}
		}`, customToolTask, namespace, customToolName, model, toolProviderName, customToolName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for task to complete")
		phase := waitForTaskCompletion(customToolTask, 5*time.Minute)
		Expect(phase).To(BeElementOf("Succeeded", "Failed"),
			"Custom tool task should reach terminal phase")

		if e2eMockOpenAI {
			Expect(phase).To(Equal("Succeeded"), "Scripted custom tool task should succeed")

			By("verifying the echo receiver's response reached the model")
			output := expectMockLLMToolResult("[e2e:tools-custom-tool]", customToolName, 30*time.Second)
			Expect(output).To(ContainSubstring("echo"), "custom tool output: %s", output)
			Expect(output).To(ContainSubstring("hello from custom tool"), "custom tool output: %s", output)
		}
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
					"contractVersion": "orka.harness.v2",
					"type": "claude",
					"defaultMaxTurns": 3,
					"defaultAllowBash": false
				},
				"model": {"name": "claude-sonnet-4-20250514"}
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

		By("verifying the tool policy is represented by an Orka harness v2 RuntimePool profile")
		verifyACPTaskRuntimeForTask(filterTaskName, acpTaskExpectation{
			ProviderKind:    "claude",
			WorkspaceIntent: "read",
			MaxTurns:        acpInt32(1),
			AllowBash:       acpBool(false),
			AllowedTools:    []string{"Read", "Grep"},
			DisallowedTools: []string{"Bash", "Write"},
		}, 2*time.Minute)
	})

	// Test: PriorTaskRef
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
					"contractVersion": "orka.harness.v2",
					"type": "claude",
					"defaultMaxTurns": 3,
					"defaultAllowBash": false
				},
				"model": {"name": "claude-sonnet-4-20250514"}
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

		By("verifying priorTaskRef fails at the ACP hard-cutover gate before dispatch")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", priorTask2Name, "-n", namespace,
				"-o", "jsonpath={.status.phase}{\"/\"}{.status.message}{\"/\"}{.status.execution.runtimePoolName}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(HavePrefix("Failed/"))
			g.Expect(output).To(ContainSubstring("priorTaskRef continuation is not supported by the ACP core runtime; use sessionRef"))
			g.Expect(output).To(HaveSuffix("/"), "rejected task must not select a RuntimePool")
		}, 2*time.Minute, time.Second).Should(Succeed())
		verifyNoJobForTask(priorTask2Name, 5*time.Second)
	})

	// Test: AI task using web_fetch tool
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
					"prompt": "Use the web_fetch tool to fetch https://httpbin.org/get and summarize the response. [e2e:tools-web-fetch]",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					},
					"tools": ["web_fetch"]
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

		if e2eMockOpenAI {
			// The fixture fetches the link-local metadata address and the
			// in-cluster aimock Service instead of the internet, so this
			// checks the SSRF guard on both an IP literal and a resolved name.
			By("verifying web_fetch refused non-public destinations")
			outputs := expectMockLLMToolResults("[e2e:tools-web-fetch]", "web_fetch", 2, 30*time.Second)
			Expect(outputs[0]).To(ContainSubstring("URL must not target private, loopback, or link-local addresses"))
			Expect(outputs[1]).To(ContainSubstring("non-public address"))
			for _, output := range outputs {
				Expect(output).NotTo(ContainSubstring(`"status"`), "web_fetch reached a private endpoint: %s", output)
			}
		}
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
					"prompt": "Use the file_write tool to write 'e2e test' to /tmp/e2e-write-test.txt, then file_read it and tell me the contents. [e2e:tools-file-write]",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					},
					"tools": ["file_write", "file_read"]
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

		if e2eMockOpenAI {
			By("verifying file_write created the file and file_read read it back")
			writeOutput := expectMockLLMToolResult("[e2e:tools-file-write]", "file_write", 30*time.Second)
			var written struct {
				Size    int  `json:"size"`
				Created bool `json:"created"`
			}
			Expect(json.Unmarshal([]byte(writeOutput), &written)).To(Succeed(), "file_write output: %s", writeOutput)
			Expect(written.Created).To(BeTrue())
			Expect(written.Size).To(Equal(len("e2e test")))

			readOutput := expectMockLLMToolResult("[e2e:tools-file-write]", "file_read", 30*time.Second)
			var read struct {
				Content string `json:"content"`
			}
			Expect(json.Unmarshal([]byte(readOutput), &read)).To(Succeed(), "file_read output: %s", readOutput)
			Expect(read.Content).To(Equal("e2e test"))
		}
	})
})
