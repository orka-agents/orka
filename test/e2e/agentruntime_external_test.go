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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

var _ = Describe("AgentRuntime external endpoint", Ordered, func() {
	const (
		runtimeName        = "fibey-agentkit-e2e"
		harnessToken       = "x"
		goodSecretName     = runtimeName + "-harness-token"
		badSecretName      = runtimeName + "-unlabeled-token"
		deploymentName     = runtimeName
		serviceName        = runtimeName
		agentName          = "fibey-custom-e2e"
		taskName           = "fibey-custom-e2e-task"
		badRuntimeName     = runtimeName + "-unlabeled"
		harnessServicePort = 8080
	)

	BeforeAll(func() {
		if !agentRuntimeExternalE2EEnabled() {
			Skip(fmt.Sprintf("Skipping AgentRuntime external endpoint E2E: %s is not enabled", agentRuntimeExternalE2EEnvVar))
		}
	})

	AfterAll(func() {
		for _, resource := range []struct {
			kind string
			name string
		}{
			{"task", taskName},
			{"agent", agentName},
			{"agentruntime", runtimeName},
			{"agentruntime", badRuntimeName},
			{"service", serviceName},
			{"deployment", deploymentName},
			{"secret", goodSecretName},
			{"secret", badSecretName},
		} {
			cmd := exec.Command("kubectl", "delete", resource.kind, resource.name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpAgentRuntimeExternalDiagnostics(runtimeName, badRuntimeName)
		}
	})

	It("runs an agent task through a conformance-gated external AgentRuntime", func() {
		By("deploying the mock AgentKit-compatible harness endpoint")
		endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", serviceName, namespace, harnessServicePort)
		Expect(applyManifestJSON(agentRuntimeExternalGoodSecret(goodSecretName, runtimeName, endpoint, harnessToken))).To(Succeed())
		Expect(applyManifestJSON(agentRuntimeExternalHarnessDeployment(deploymentName, serviceName, harnessToken))).To(Succeed())

		cmd := exec.Command("kubectl", "rollout", "status", "deployment/"+deploymentName, "-n", namespace, "--timeout=2m")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "mock harness deployment should become ready")

		By("registering an external AgentRuntime")
		Expect(applyManifestJSON(agentRuntimeExternalRuntime(runtimeName, endpoint, goodSecretName))).To(Succeed())
		waitForAgentRuntimeReady(runtimeName, true, 2*time.Minute)
		assertAgentRuntimeObservedCapabilities(runtimeName, map[string]string{
			"runtimeName":     runtimeName,
			"protocolVersion": "orka.harness.v1",
		})

		By("creating an Agent that selects the runtimeRef")
		Expect(applyManifestJSON(map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind":       "Agent",
			"metadata": map[string]any{
				"name":      agentName,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"runtime": map[string]any{
					"runtimeRef": map[string]any{"name": runtimeName},
				},
				"systemPrompt": map[string]any{
					"inline": "You are a custom Fibey incident scout. Return a concise result.",
				},
			},
		})).To(Succeed())

		By("creating a task that runs through the external runtime")
		Expect(applyManifestJSON(map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind":       "Task",
			"metadata": map[string]any{
				"name":      taskName,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"type":     "agent",
				"agentRef": map[string]any{"name": agentName},
				"prompt":   "Quincy North alert: summarize likely cause and safest next action.",
			},
		})).To(Succeed())

		waitForTaskPhase(taskName, "Succeeded", 2*time.Minute)
		verifyNoJobForTask(taskName, 5*time.Second)
		verifyResultAvailable(taskName)
		assertTaskHarnessRuntimeStatus(taskName, runtimeName, endpoint, goodSecretName)
		verifyHarnessWrapperMetadataForTask(taskName, map[string]string{
			"runtime":         runtimeName,
			"runtimeRef":      runtimeName,
			"wrapper":         "external-endpoint",
			"contractVersion": "orka.harness.v1",
		}, 30*time.Second)

		By("verifying unlabeled auth Secrets are rejected for AgentRuntime readiness")
		Expect(applyManifestJSON(agentRuntimeExternalUnlabeledSecret(badSecretName, harnessToken))).To(Succeed())
		Expect(applyManifestJSON(agentRuntimeExternalRuntime(badRuntimeName, endpoint, badSecretName))).To(Succeed())
		waitForAgentRuntimeReady(badRuntimeName, false, time.Minute)
		assertAgentRuntimeMessageContains(badRuntimeName, "orka.ai/agent-runtime-auth")
	})
})

func agentRuntimeExternalGoodSecret(name, runtimeName, endpoint, value string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]any{
				"orka.ai/agent-runtime-auth": "true",
				"orka.ai/agent-runtime-name": runtimeName,
			},
			"annotations": map[string]any{
				"orka.ai/agent-runtime-endpoint": endpoint,
			},
		},
		"stringData": map[string]any{"token": value},
	}
}

func agentRuntimeExternalUnlabeledSecret(name, value string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"stringData": map[string]any{"token": value},
	}
}

func agentRuntimeExternalHarnessDeployment(deploymentName, serviceName, harnessToken string) map[string]any {
	labels := map[string]any{"app.kubernetes.io/name": deploymentName}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "List",
		"items": []any{
			map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]any{
					"name":      deploymentName,
					"namespace": namespace,
					"labels":    labels,
				},
				"spec": map[string]any{
					"replicas": 1,
					"selector": map[string]any{"matchLabels": labels},
					"template": map[string]any{
						"metadata": map[string]any{"labels": labels},
						"spec": map[string]any{
							"securityContext": map[string]any{
								"runAsNonRoot": true,
								"runAsUser":    65532,
								"runAsGroup":   65532,
								"seccompProfile": map[string]any{
									"type": "RuntimeDefault",
								},
							},
							"containers": []any{
								map[string]any{
									"name":            "harness",
									"image":           agentRuntimeExternalHarnessImage,
									"imagePullPolicy": "IfNotPresent",
									"env": []any{
										map[string]any{"name": "ORKA_EXAMPLE_HARNESS_ADDR", "value": ":8080"},
										map[string]any{"name": "ORKA_EXAMPLE_HARNESS_RUNTIME_NAME", "value": deploymentName},
										map[string]any{"name": "ORKA_EXAMPLE_HARNESS_BEARER_TOKEN", "value": harnessToken},
									},
									"ports": []any{map[string]any{"name": "http", "containerPort": 8080}},
									"securityContext": map[string]any{
										"allowPrivilegeEscalation": false,
										"readOnlyRootFilesystem":   true,
										"capabilities": map[string]any{
											"drop": []any{"ALL"},
										},
									},
									"readinessProbe": map[string]any{
										"httpGet":       map[string]any{"path": "/v1/health", "port": "http"},
										"periodSeconds": 2,
									},
								},
							},
						},
					},
				},
			},
			map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]any{
					"name":      serviceName,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"selector": labels,
					"ports": []any{map[string]any{
						"name":       "http",
						"port":       8080,
						"targetPort": "http",
					}},
				},
			},
		},
	}
}

func agentRuntimeExternalRuntime(name, endpoint, secretName string) map[string]any {
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "AgentRuntime",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"contractVersion": "orka.harness.v1",
			"deployment": map[string]any{
				"mode":     "external-endpoint",
				"endpoint": endpoint,
			},
			"clientAuth": map[string]any{
				"bearerTokenSecretRef": map[string]any{
					"name": secretName,
					"key":  "token",
				},
			},
			"capabilities": map[string]any{
				"toolExecutionModes":      []any{"observed"},
				"supportsCancel":          true,
				"supportsRuntimeSessions": true,
			},
		},
	}
}

func waitForAgentRuntimeReady(name string, ready bool, timeout time.Duration) {
	want := fmt.Sprintf("%t", ready)
	jsonPath := "jsonpath={.status.ready}"
	if !ready {
		jsonPath = "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}"
		want = "False"
	}
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "agentruntime", name,
			"-n", namespace, "-o", jsonPath)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal(want), "AgentRuntime %s readiness", name)
	}, timeout, time.Second).Should(Succeed())
}

func assertAgentRuntimeObservedCapabilities(name string, expected map[string]string) {
	cmd := exec.Command("kubectl", "get", "agentruntime", name, "-n", namespace, "-o", "json")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	var runtime struct {
		Status struct {
			ObservedCapabilities map[string]any `json:"observedCapabilities"`
		} `json:"status"`
	}
	Expect(json.Unmarshal([]byte(output), &runtime)).To(Succeed())
	for key, want := range expected {
		got, _ := runtime.Status.ObservedCapabilities[key].(string)
		Expect(got).To(Equal(want), "observedCapabilities.%s", key)
	}
	modes, ok := runtime.Status.ObservedCapabilities["toolExecutionModes"].([]any)
	Expect(ok).To(BeTrue(), "toolExecutionModes should be present")
	Expect(modes).To(ContainElement("observed"))
}

func assertAgentRuntimeMessageContains(name, want string) {
	cmd := exec.Command("kubectl", "get", "agentruntime", name,
		"-n", namespace, "-o", "jsonpath={.status.message}")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	Expect(output).To(ContainSubstring(want))
}

func assertTaskHarnessRuntimeStatus(taskName, runtimeName, endpoint, authRefName string) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "task", taskName, "-n", namespace, "-o", "json")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		var task struct {
			Status struct {
				HarnessRuntime struct {
					RuntimeRefName  string `json:"runtimeRefName"`
					ContractVersion string `json:"contractVersion"`
					Endpoint        string `json:"endpoint"`
					AuthRefName     string `json:"authRefName"`
				} `json:"harnessRuntime"`
			} `json:"status"`
		}
		g.Expect(json.Unmarshal([]byte(output), &task)).To(Succeed())
		g.Expect(task.Status.HarnessRuntime.RuntimeRefName).To(Equal(runtimeName))
		g.Expect(task.Status.HarnessRuntime.ContractVersion).To(Equal("orka.harness.v1"))
		g.Expect(task.Status.HarnessRuntime.Endpoint).To(Equal(endpoint))
		g.Expect(task.Status.HarnessRuntime.AuthRefName).To(Equal(authRefName))
	}, 30*time.Second, time.Second).Should(Succeed())
}

func dumpAgentRuntimeExternalDiagnostics(runtimeNames ...string) {
	resources := [][]string{
		{"get", "agentruntimes", "-n", namespace, "-o", "wide"},
		{"get", "tasks", "-n", namespace, "-o", "wide"},
		{"get", "pods", "-n", namespace, "-o", "wide"},
		{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
	}
	for _, runtimeName := range runtimeNames {
		if strings.TrimSpace(runtimeName) == "" {
			continue
		}
		resources = append(resources, []string{"describe", "agentruntime", runtimeName, "-n", namespace})
	}
	for _, args := range resources {
		cmd := exec.Command("kubectl", args...)
		output, err := utils.Run(cmd)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "diagnostic failed: kubectl %s: %v\n", strings.Join(args, " "), err)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "diagnostic: kubectl %s\n%s\n", strings.Join(args, " "), output)
	}
}
