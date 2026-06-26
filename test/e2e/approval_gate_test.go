//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"bytes"
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

	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("Human Approval Gate", Ordered, func() {
	const (
		approvalProviderName = "e2e-approval-provider"
		approvalAgentName    = "e2e-approval-agent"
		approvalToolName     = "e2e-dispatch-work-order"
		approvalTaskName     = "e2e-approval-task"
		approvalMockName     = "e2e-approval-mock"
		approvalMockPort     = 8080
		approvalAPIProxyPort = 18123
	)

	var (
		apiBaseURL     string
		portForwardCmd *exec.Cmd
		cancelPF       func()
	)

	BeforeAll(func() {
		By("deploying the in-cluster approval mock service")
		applyApprovalMockService(approvalMockName, approvalToolName, approvalMockPort)

		By("creating a Provider backed by the mock OpenAI-compatible service")
		Expect(createK8sSecret("e2e-approval-openai-secret", namespace, map[string]string{"api-key": "test-key"})).To(Succeed())
		mockBaseURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/v1", approvalMockName, namespace, approvalMockPort)
		createProviderCRD(approvalProviderName, "openai", "e2e-approval-openai-secret", "api-key", mockBaseURL, "e2e-approval-model")

		By("starting a controller API port-forward for approval decisions")
		var err error
		var cancel context.CancelFunc
		apiBaseURL, cancel, portForwardCmd, err = startControllerAPIPortForward(approvalAPIProxyPort)
		Expect(err).NotTo(HaveOccurred())
		cancelPF = cancel
	})

	AfterAll(func() {
		By("stopping controller API port-forward")
		if cancelPF != nil {
			cancelPF()
		}
		if portForwardCmd != nil && portForwardCmd.Process != nil {
			_ = portForwardCmd.Wait()
		}

		By("cleaning up approval gate e2e resources")
		for _, args := range [][]string{
			{"delete", "task", approvalTaskName, "-n", namespace, "--ignore-not-found"},
			{"delete", "agent", approvalAgentName, "-n", namespace, "--ignore-not-found"},
			{"delete", "tool", approvalToolName, "-n", namespace, "--ignore-not-found"},
			{"delete", "provider", approvalProviderName, "-n", namespace, "--ignore-not-found"},
			{"delete", "secret", "e2e-approval-openai-secret", "-n", namespace, "--ignore-not-found"},
			{"delete", "deployment", approvalMockName, "-n", namespace, "--ignore-not-found"},
			{"delete", "service", approvalMockName, "-n", namespace, "--ignore-not-found"},
			{"delete", "configmap", approvalMockName, "-n", namespace, "--ignore-not-found"},
		} {
			cmd := exec.Command("kubectl", args...)
			_, _ = utils.Run(cmd)
		}
	})

	AfterEach(func() {
		dumpDebugInfo(approvalTaskName)
	})

	It("parks before a gated custom tool and resumes after approval", func() {
		By("creating the approval-gated custom Tool")
		dispatchURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/dispatch", approvalMockName, namespace, approvalMockPort)
		applyApprovalDispatchTool(approvalToolName, dispatchURL)

		By("creating an autonomous agent that requires human approval for the custom Tool")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"providerRef": {"name": %q},
				"model": {"name": "e2e-approval-model"},
				"coordination": {
					"enabled": true,
					"autonomous": true,
					"maxIterations": 3,
					"approvalRequiredTools": [%q]
				}
			}
		}`, approvalAgentName, namespace, approvalProviderName, approvalToolName)
		applyManifest(agentManifest)

		By("creating an AI task that will call the gated tool")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "ai",
				"agentRef": {"name": %q},
				"ai": {
					"prompt": "Dispatch work order inc-1 with the custom tool, then report done.",
					"model": "e2e-approval-model",
					"providerRef": {"name": %q},
					"tools": [%q]
				}
			}
		}`, approvalTaskName, namespace, approvalAgentName, approvalProviderName, approvalToolName)
		applyManifest(taskManifest)

		By("waiting for the worker to request approval and park without executing the tool")
		approvalID := waitForPendingApproval(apiBaseURL, approvalTaskName, approvalToolName, 3*time.Minute)
		Expect(fetchApprovalMockDispatchCount(approvalMockName, approvalMockPort)).To(Equal(0))

		By("approving the requested action through the controller API")
		decideApproval(apiBaseURL, approvalTaskName, approvalID, "approve")

		By("waiting for the task to resume and succeed")
		phase := waitForTaskCompletion(approvalTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"))
		Expect(fetchApprovalMockDispatchCount(approvalMockName, approvalMockPort)).To(Equal(1))
	})
})

func applyApprovalMockService(name, toolName string, port int) {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s
  namespace: %[2]s
data:
  server.py: |
    import json
    from http.server import BaseHTTPRequestHandler, HTTPServer

    dispatch_count = 0

    def write_json(handler, status, payload):
        body = json.dumps(payload).encode("utf-8")
        handler.send_response(status)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(body)))
        handler.end_headers()
        handler.wfile.write(body)

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            return

        def do_GET(self):
            if self.path == "/readyz":
                write_json(self, 200, {"ready": True})
                return
            if self.path == "/count":
                write_json(self, 200, {"count": dispatch_count})
                return
            write_json(self, 404, {"error": "not found"})

        def do_POST(self):
            global dispatch_count
            raw = self.rfile.read(int(self.headers.get("Content-Length", "0") or "0"))
            if self.path == "/v1/responses":
                write_json(self, 404, {"error": {"message": "responses not supported"}})
                return
            if self.path == "/v1/chat/completions":
                request = json.loads(raw.decode("utf-8") or "{}")
                messages = request.get("messages") or []
                if any(message.get("role") == "tool" for message in messages):
                    write_json(self, 200, {
                        "id": "chatcmpl-approval-final",
                        "object": "chat.completion",
                        "created": 1,
                        "model": request.get("model", "e2e-approval-model"),
                        "choices": [{"index": 0, "message": {"role": "assistant", "content": "approval e2e complete"}, "finish_reason": "stop"}],
                        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                    })
                    return
                write_json(self, 200, {
                    "id": "chatcmpl-approval-tool",
                    "object": "chat.completion",
                    "created": 1,
                    "model": request.get("model", "e2e-approval-model"),
                    "choices": [{
                        "index": 0,
                        "message": {
                            "role": "assistant",
                            "content": "",
                            "tool_calls": [{
                                "id": "call-dispatch-1",
                                "type": "function",
                                "function": {"name": "%[3]s", "arguments": "{\"incident\":\"inc-1\"}"},
                            }],
                        },
                        "finish_reason": "tool_calls",
                    }],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                })
                return
            if self.path == "/dispatch":
                dispatch_count += 1
                write_json(self, 200, {"ok": True, "dispatchCount": dispatch_count})
                return
            write_json(self, 404, {"error": "not found"})

    HTTPServer(("0.0.0.0", %[4]d), Handler).serve_forever()
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
      - name: mock
        image: python:3.12-alpine
        imagePullPolicy: IfNotPresent
        command: ["python", "-B", "/app/server.py"]
        ports:
        - containerPort: %[4]d
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop: ["ALL"]
        volumeMounts:
        - name: app
          mountPath: /app
          readOnly: true
      volumes:
      - name: app
        configMap:
          name: %[1]s
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector:
    app: %[1]s
  ports:
  - port: %[4]d
    targetPort: %[4]d
`, name, namespace, toolName, port)
	applyManifest(manifest)

	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "rollout", "status", "deployment/"+name, "-n", namespace, "--timeout=10s")
		_, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
	}, 2*time.Minute, 5*time.Second).Should(Succeed())
}

func applyApprovalDispatchTool(name, dispatchURL string) {
	manifest := fmt.Sprintf(`{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind": "Tool",
		"metadata": {"name": %q, "namespace": %q},
		"spec": {
			"description": "Dispatch a work order after human approval.",
			"parameters": {
				"type": "object",
				"properties": {"incident": {"type": "string"}},
				"required": ["incident"]
			},
			"http": {"url": %q, "method": "POST", "timeout": "10s"}
		}
	}`, name, namespace, dispatchURL)
	applyManifest(manifest)
}

func applyManifest(manifest string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

func waitForPendingApproval(apiBaseURL, taskName, targetTool string, timeout time.Duration) string {
	var approvalID string
	Eventually(func(g Gomega) {
		approvalsList := listTaskApprovals(apiBaseURL, taskName)
		for _, approval := range approvalsList.Approvals {
			if approval.TargetTool == targetTool && approval.Status == approvals.StatusPending {
				approvalID = approval.ID
				return
			}
		}
		g.Expect(approvalID).NotTo(BeEmpty(), "approval should be pending for %s", targetTool)
	}, timeout, 2*time.Second).Should(Succeed())
	return approvalID
}

type taskApprovalList struct {
	Approvals []struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		TargetTool string `json:"targetTool"`
	} `json:"approvals"`
}

func listTaskApprovals(apiBaseURL, taskName string) taskApprovalList {
	url := fmt.Sprintf("%s/api/v1/tasks/%s/approvals?namespace=%s", strings.TrimRight(apiBaseURL, "/"), taskName, namespace)
	resp, err := http.Get(url)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	body, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	var out taskApprovalList
	Expect(json.Unmarshal(body, &out)).To(Succeed())
	return out
}

func decideApproval(apiBaseURL, taskName, approvalID, decision string) {
	body := []byte(fmt.Sprintf(`{"decision":%q,"reason":"approval gate e2e"}`, decision))
	url := fmt.Sprintf(
		"%s/api/v1/tasks/%s/approvals/%s/decision?namespace=%s",
		strings.TrimRight(apiBaseURL, "/"), taskName, approvalID, namespace,
	)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
}

func fetchApprovalMockDispatchCount(serviceName string, servicePort int) int {
	body, err := fetchServiceProxyBody(namespace, serviceName, servicePort, "/count")
	Expect(err).NotTo(HaveOccurred())
	var payload struct {
		Count int `json:"count"`
	}
	Expect(json.Unmarshal([]byte(body), &payload)).To(Succeed())
	return payload.Count
}
