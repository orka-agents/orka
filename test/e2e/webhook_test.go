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

var _ = Describe("Webhook Delivery", Ordered, func() {
	const (
		webhookTaskName     = "e2e-webhook-task"
		webhookReceiverName = "e2e-webhook-receiver"
		webhookServiceName  = "e2e-webhook-svc"
		webhookPort         = 8888
	)

	BeforeAll(func() {
		By("deploying a webhook receiver pod")
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
					"name": "receiver",
					"image": "python:3-alpine",
					"command": ["python3", "-c"],
					"args": ["import http.server, json, sys\nclass H(http.server.BaseHTTPRequestHandler):\n  def do_POST(self):\n    length = int(self.headers.get('Content-Length', 0))\n    body = self.rfile.read(length)\n    print(f'WEBHOOK_RECEIVED: {body.decode()}', flush=True)\n    self.send_response(200)\n    self.end_headers()\n    self.wfile.write(b'ok')\nhttp.server.HTTPServer(('', %d), H).serve_forever()"],
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
		}`, webhookReceiverName, namespace, webhookReceiverName, webhookPort, webhookPort)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(receiverManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create webhook receiver pod")

		By("creating a Service for the webhook receiver")
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
		}`, webhookServiceName, namespace, webhookReceiverName, webhookPort, webhookPort)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(svcManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create webhook service")

		By("waiting for the webhook receiver to be ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pod", webhookReceiverName,
				"-n", namespace, "-o", "jsonpath={.status.phase}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Running"))
		}, 2*time.Minute, time.Second).Should(Succeed())
	})

	AfterAll(func() {
		By("cleaning up webhook test resources")
		cmd := exec.Command("kubectl", "delete", "task", webhookTaskName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "service", webhookServiceName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "pod", webhookReceiverName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(webhookTaskName)
	})

	It("should deliver a webhook on task completion", func() {
		webhookURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/webhook",
			webhookServiceName, namespace, webhookPort)

		By("creating a task with a webhookURL")
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
				"args": ["webhook-test-output"],
				"webhookURL": "%s"
			}
		}`, webhookTaskName, namespace, webhookURL)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create webhook task")

		By("waiting for task to succeed")
		phase := waitForTaskCompletion(webhookTaskName, 3*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "Webhook task should succeed")

		By("verifying webhookDelivered is true on the task status")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", webhookTaskName,
				"-o", "jsonpath={.status.webhookDelivered}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("true"), "webhookDelivered should be true")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("verifying the webhook receiver got the POST request")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "logs", webhookReceiverName, "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("WEBHOOK_RECEIVED"))
			g.Expect(output).To(ContainSubstring(webhookTaskName))
			g.Expect(output).To(ContainSubstring("Succeeded"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})
