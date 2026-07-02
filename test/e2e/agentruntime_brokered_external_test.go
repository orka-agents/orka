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

var _ = Describe("AgentRuntime brokered external endpoint", Ordered, func() {
	const (
		runtimeName        = "agentruntime-brokered-e2e"
		authValue          = "test"
		authRefName        = runtimeName + "-auth-ref"
		deploymentName     = runtimeName
		serviceName        = runtimeName
		agentName          = runtimeName + "-agent"
		taskName           = runtimeName + "-task"
		toolName           = "e2e-read-incident"
		harnessServicePort = 8080
	)

	BeforeAll(func() {
		if !agentRuntimeExternalE2EEnabled() {
			Skip(fmt.Sprintf("Skipping AgentRuntime brokered external endpoint E2E: %s is not enabled", agentRuntimeExternalE2EEnvVar))
		}
	})

	AfterAll(func() {
		for _, resource := range []struct {
			kind string
			name string
		}{
			{"task", taskName},
			{"agent", agentName},
			{"tool", toolName},
			{"agentruntime", runtimeName},
			{"service", serviceName},
			{"deployment", deploymentName},
			{"secret", authRefName},
		} {
			cmd := exec.Command("kubectl", "delete", resource.kind, resource.name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpAgentRuntimeExternalDiagnostics(runtimeName)
		}
	})

	It("runs a brokered read tool through a brokered-only generic HTTP AgentRuntime", func() {
		endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", serviceName, namespace, harnessServicePort)
		Expect(applyManifestJSON(agentRuntimeExternalGoodSecret(authRefName, runtimeName, endpoint, authValue))).To(Succeed())
		Expect(applyManifestJSON(agentRuntimeBrokeredHarnessDeployment(deploymentName, serviceName, authValue, toolName))).To(Succeed())

		cmd := exec.Command("kubectl", "rollout", "status", "deployment/"+deploymentName, "-n", namespace, "--timeout=2m")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "brokered mock harness deployment should become ready")

		Expect(applyManifestJSON(agentRuntimeBrokeredRuntime(runtimeName, endpoint, authRefName))).To(Succeed())
		waitForAgentRuntimeReady(runtimeName, true, 2*time.Minute)
		Expect(applyManifestJSON(agentRuntimeBrokeredReadTool(toolName, endpoint+"/lookup"))).To(Succeed())
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
					"inline": "Use brokered Orka tools for evidence gathering.",
				},
			},
		})).To(Succeed())
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
				"agentRuntime": map[string]any{
					"allowedTools": []any{toolName},
				},
				"prompt": "Investigate Quincy North using brokered read evidence.",
			},
		})).To(Succeed())

		waitForTaskPhase(taskName, "Succeeded", 2*time.Minute)
		verifyNoJobForTask(taskName, 5*time.Second)
		verifyResultAvailable(taskName)
		assertTaskHarnessRuntimeStatus(taskName, runtimeName, endpoint, authRefName)
	})
})

func agentRuntimeBrokeredHarnessDeployment(deploymentName, serviceName, authValue, readToolName string) map[string]any {
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
										map[string]any{"name": "ORKA_REMOTE_HTTP_RUNTIME_ADDR", "value": ":8080"},
										map[string]any{"name": "ORKA_REMOTE_HTTP_RUNTIME_NAME", "value": deploymentName},
										map[string]any{"name": "ORKA_REMOTE_HTTP_RUNTIME_BEARER_TOKEN", "value": authValue},
										map[string]any{"name": "ORKA_REMOTE_HTTP_RUNTIME_BEHAVIOR", "value": "read-tool"},
										map[string]any{"name": "ORKA_REMOTE_HTTP_RUNTIME_BROKERED_ONLY", "value": "true"},
										map[string]any{"name": "ORKA_REMOTE_HTTP_RUNTIME_READ_TOOL_NAME", "value": readToolName},
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

func agentRuntimeBrokeredRuntime(name, endpoint, authRefName string) map[string]any {
	runtime := agentRuntimeExternalRuntime(name, endpoint, authRefName)
	spec := runtime["spec"].(map[string]any)
	spec["capabilities"] = map[string]any{
		"toolExecutionModes":      []any{"brokered"},
		"brokeredToolClasses":     []any{"read"},
		"supportsRuntimeSessions": true,
		"supportsContinuation":    true,
	}
	return runtime
}

func agentRuntimeBrokeredReadTool(name, url string) map[string]any {
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Tool",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"description":       "Read sanitized incident evidence through an Orka-brokered tool.",
			"brokeredToolClass": "read",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"incident": map[string]any{"type": "string"},
				},
			},
			"http": map[string]any{
				"url":    url,
				"method": "POST",
			},
		},
	}
}
