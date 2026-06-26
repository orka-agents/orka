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
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
)

const (
	otelCollectorName       = "e2e-otel-collector"
	otelFakeOpenAIName      = "e2e-otel-openai"
	otelProviderName        = "e2e-otel-provider"
	otelProviderAuthRefName = "e2e-otel-openai-auth"
	otelTaskName            = "e2e-otel-genai-task"
	otelModelName           = "e2e-otel-model"
	otelFakeOpenAIImage     = "python:3.14-slim"
)

func otelCollectorServiceAddr() string {
	return fmt.Sprintf("http://%s.%s.svc:4317", otelCollectorName, namespace)
}

var _ = Describe("OpenTelemetry GenAI export", Ordered, Serial, func() {
	var controllerSnapshot otelControllerSnapshot

	BeforeAll(func() {
		By("snapshotting controller telemetry settings")
		controllerSnapshot = captureOTelControllerSnapshot()

		By("deploying a local OTLP collector")
		applyOTelManifest(otelCollectorManifest())
		waitForOTelDeploymentAvailable(otelCollectorName, 2*time.Minute)

		By("deploying a fake OpenAI-compatible endpoint")
		applyOTelManifest(otelFakeOpenAIManifest())
		waitForOTelDeploymentAvailable(otelFakeOpenAIName, 2*time.Minute)

		By("enabling controller telemetry against the local collector")
		enableControllerTelemetryForE2E(controllerSnapshot)
	})

	AfterAll(func() {
		By("cleaning up OpenTelemetry GenAI test resources")
		for _, resource := range []struct {
			kind string
			name string
		}{
			{kind: "task", name: otelTaskName},
			{kind: "provider", name: otelProviderName},
			{kind: "secret", name: otelProviderAuthRefName},
			{kind: "deployment", name: otelCollectorName},
			{kind: "service", name: otelCollectorName},
			{kind: "configmap", name: otelCollectorName + "-config"},
			{kind: "deployment", name: otelFakeOpenAIName},
			{kind: "service", name: otelFakeOpenAIName},
			{kind: "configmap", name: otelFakeOpenAIName + "-server"},
		} {
			cmd := exec.Command("kubectl", "delete", resource.kind, resource.name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}

		By("restoring controller telemetry settings")
		restoreOTelControllerSnapshot(controllerSnapshot)
	})

	AfterEach(func() {
		dumpDebugInfo(otelTaskName)
		dumpOTelCollectorLogsForDiagnostics()
	})

	It("exports GenAI model and tool spans and metrics from an AI worker", func() {
		By("creating a Provider CRD that points to the fake OpenAI endpoint")
		createOTelSecretOrFail(otelProviderAuthRefName, map[string]string{"token": "placeholder"})
		createProviderCRD(
			otelProviderName,
			"openai",
			otelProviderAuthRefName,
			"token",
			fmt.Sprintf("http://%s.%s.svc:8080", otelFakeOpenAIName, namespace),
			otelModelName,
		)

		By("creating an AI task that exercises the model loop and file_write tool")
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
					"prompt": "Use file_write once, then reply otel e2e complete.",
					"model": "%s",
					"providerRef": {
						"name": "%s"
					},
					"tools": ["file_write"]
				}
			}
		}`, otelTaskName, namespace, otelModelName, otelProviderName)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create OpenTelemetry GenAI task")

		By("waiting for the AI task to complete successfully")
		phase := waitForTaskCompletion(otelTaskName, 5*time.Minute)
		Expect(phase).To(Equal("Succeeded"), "OpenTelemetry GenAI task should succeed")
		verifyResultAvailable(otelTaskName)

		By("asserting the worker Job received telemetry enablement and OTLP settings")
		verifyOTelTelemetryEnvForTaskJob(otelTaskName)

		By("asserting the collector received GenAI spans and metrics")
		assertOTelCollectorLogsContain([]string{
			"service.name: Str(orka-ai-worker)",
			"chat " + otelModelName,
			"gen_ai.operation.name: Str(chat)",
			"gen_ai.provider.name: Str(openai)",
			"gen_ai.request.model: Str(" + otelModelName + ")",
			"execute_tool file_write",
			"gen_ai.operation.name: Str(execute_tool)",
			"gen_ai.tool.name: Str(file_write)",
			"gen_ai.client.operation.duration",
			"gen_ai.client.token.usage",
			"gen_ai.execute_tool.duration",
		}, 3*time.Minute)
	})
})

func applyOTelManifest(manifest string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = stringReader(manifest)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func waitForOTelDeploymentAvailable(name string, timeout time.Duration) {
	cmd := exec.Command("kubectl", "rollout", "status", "deployment/"+name, "-n", namespace, "--timeout="+timeout.String())
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "deployment %s did not become available", name)
}

func createOTelSecretOrFail(name string, data map[string]string) {
	cmd := exec.Command("kubectl", "delete", "secret", name, "-n", namespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)
	err := createK8sSecret(name, namespace, data)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to create secret %s", name)
}

func enableControllerTelemetryForE2E(snapshot otelControllerSnapshot) {
	ExpectWithOffset(1, snapshot.Captured).To(BeTrue(), "controller telemetry snapshot must be captured before patching")

	args := append([]string(nil), snapshot.Args...)
	env := append([]otelEnvVar(nil), snapshot.Env...)
	if !slices.Contains(args, "--enable-telemetry") {
		args = append(args, "--enable-telemetry")
	}
	env = upsertOTelEnvVars(env, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":        otelCollectorServiceAddr(),
		"OTEL_EXPORTER_OTLP_INSECURE":        "true",
		"OTEL_EXPORTER_OTLP_PROTOCOL":        "grpc",
		"OTEL_EXPORTER_OTLP_TIMEOUT":         "5s",
		"OTEL_EXPORTER_OTLP_METRICS_TIMEOUT": "5s",
		"OTEL_RESOURCE_ATTRIBUTES":           "orka.e2e.test=otel-genai",
	})

	patchOTelControllerManager(snapshot.DeploymentName, snapshot.ContainerIndex, args, env, "failed to patch controller-manager telemetry settings")
}

type otelControllerSnapshot struct {
	DeploymentName string
	ContainerIndex int
	Args           []string
	Env            []otelEnvVar
	Captured       bool
}

func captureOTelControllerSnapshot() otelControllerSnapshot {
	deploymentName, err := controllerManagerDeploymentName()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	containerIndex, args, env := otelControllerManagerArgsAndEnv(deploymentName)
	return otelControllerSnapshot{
		DeploymentName: deploymentName,
		ContainerIndex: containerIndex,
		Args:           append([]string(nil), args...),
		Env:            append([]otelEnvVar(nil), env...),
		Captured:       true,
	}
}

func restoreOTelControllerSnapshot(snapshot otelControllerSnapshot) {
	if !snapshot.Captured {
		return
	}
	patchOTelControllerManager(
		snapshot.DeploymentName,
		snapshot.ContainerIndex,
		append([]string(nil), snapshot.Args...),
		append([]otelEnvVar(nil), snapshot.Env...),
		"failed to restore controller-manager telemetry settings",
	)
}

func patchOTelControllerManager(deploymentName string, containerIndex int, args []string, env []otelEnvVar, failureMessage string) {
	if args == nil {
		args = []string{}
	}
	if env == nil {
		env = []otelEnvVar{}
	}
	containerPath := fmt.Sprintf("/spec/template/spec/containers/%d", containerIndex)
	patch := []map[string]any{
		{
			"op":    "add",
			"path":  containerPath + "/args",
			"value": args,
		},
		{
			"op":    "add",
			"path":  containerPath + "/env",
			"value": env,
		},
	}
	patchBytes, err := json.Marshal(patch)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	cmd := exec.Command("kubectl", "patch", "deployment", deploymentName, "-n", namespace, "--type=json", "-p", string(patchBytes))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), failureMessage)

	cmd = exec.Command("kubectl", "rollout", "status", "deployment/"+deploymentName, "-n", namespace, "--timeout=5m")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed waiting for controller-manager rollout")
}

type otelEnvVar struct {
	Name      string `json:"name"`
	Value     string `json:"value,omitempty"`
	ValueFrom any    `json:"valueFrom,omitempty"`
}

type otelControllerContainer struct {
	Name string       `json:"name"`
	Args []string     `json:"args"`
	Env  []otelEnvVar `json:"env"`
}

func otelControllerManagerArgsAndEnv(deploymentName string) (int, []string, []otelEnvVar) {
	cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", namespace, "-o", "json")
	output, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to read controller-manager deployment")

	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []otelControllerContainer `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	err = json.Unmarshal([]byte(output), &deployment)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	for i, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "manager" {
			return i, append([]string(nil), container.Args...), append([]otelEnvVar(nil), container.Env...)
		}
	}
	Fail("controller-manager deployment did not contain manager container")
	return 0, nil, nil
}

func upsertOTelEnvVars(env []otelEnvVar, values map[string]string) []otelEnvVar {
	for name, value := range values {
		updated := false
		for i := range env {
			if env[i].Name == name {
				env[i].Value = value
				env[i].ValueFrom = nil
				updated = true
				break
			}
		}
		if !updated {
			env = append(env, otelEnvVar{Name: name, Value: value})
		}
	}
	return env
}

func verifyOTelTelemetryEnvForTaskJob(taskName string) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "jobs", "-l", fmt.Sprintf("orka.ai/task=%s", taskName),
			"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}", "-n", namespace)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(ContainSubstring("ORKA_ENABLE_TELEMETRY"))
		g.Expect(output).To(ContainSubstring(otelCollectorServiceAddr()))
		g.Expect(output).To(ContainSubstring("OTEL_EXPORTER_OTLP_INSECURE"))
		g.Expect(output).To(ContainSubstring("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}, 30*time.Second, time.Second).Should(Succeed())
}

func assertOTelCollectorLogsContain(needles []string, timeout time.Duration) {
	Eventually(func(g Gomega) {
		logs := otelCollectorLogs()
		for _, needle := range needles {
			g.Expect(logs).To(ContainSubstring(needle), "collector logs should contain %q", needle)
		}
	}, timeout, 2*time.Second).Should(Succeed())
}

func otelCollectorLogs() string {
	cmd := exec.Command("kubectl", "logs", "deployment/"+otelCollectorName, "-n", namespace, "--tail=5000")
	output, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to read collector logs")
	return output
}

func dumpOTelCollectorLogsForDiagnostics() {
	if !CurrentSpecReport().Failed() {
		return
	}

	cmd := exec.Command("kubectl", "logs", "deployment/"+otelCollectorName, "-n", namespace, "--tail=200")
	output, err := utils.Run(cmd)
	if err == nil && strings.TrimSpace(output) != "" {
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- %s logs ---\n%s\n", otelCollectorName, output)
	}
}

func otelCollectorManifest() string {
	return fmt.Sprintf(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-config
  namespace: %[2]s
data:
  collector.yaml: |
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: 0.0.0.0:4317
          http:
            endpoint: 0.0.0.0:4318
    processors:
      batch:
        timeout: 1s
    exporters:
      debug:
        verbosity: detailed
    service:
      pipelines:
        traces:
          receivers: [otlp]
          processors: [batch]
          exporters: [debug]
        metrics:
          receivers: [otlp]
          processors: [batch]
          exporters: [debug]
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
      app.kubernetes.io/name: %[1]s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %[1]s
    spec:
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: collector
          image: otel/opentelemetry-collector:0.111.0
          args:
            - --config=/etc/otelcol/collector.yaml
          ports:
            - name: otlp-grpc
              containerPort: 4317
            - name: otlp-http
              containerPort: 4318
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: config
              mountPath: /etc/otelcol
              readOnly: true
      volumes:
        - name: config
          configMap:
            name: %[1]s-config
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector:
    app.kubernetes.io/name: %[1]s
  ports:
    - name: otlp-grpc
      port: 4317
      targetPort: otlp-grpc
    - name: otlp-http
      port: 4318
      targetPort: otlp-http
`, otelCollectorName, namespace)
}

func otelFakeOpenAIManifest() string {
	return fmt.Sprintf(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-server
  namespace: %[2]s
data:
  server.py: |
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    import json

    TOOL_ARGS = json.dumps({
        "path": "otel-e2e.txt",
        "content": "otel-e2e",
        "mode": "write",
        "create_dirs": True,
    })

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            return

        def _json(self, status, payload):
            body = json.dumps(payload).encode("utf-8")
            self.send_response(status)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path == "/readyz":
                self._json(200, {"ok": True})
                return
            if self.path.endswith("/models"):
                self._json(200, {"data": [{"id": "%[3]s", "object": "model"}]})
                return
            self._json(404, {"error": {"message": "not found", "code": "not_found"}})

        def do_POST(self):
            length = int(self.headers.get("content-length", "0") or "0")
            raw = self.rfile.read(length) if length else b"{}"
            try:
                request = json.loads(raw.decode("utf-8"))
            except Exception:
                request = {}

            if self.path.endswith("/responses"):
                self._json(404, {"error": {"message": "Not Found", "type": "invalid_request_error", "code": "invalid_url"}})
                return

            if self.path.endswith("/chat/completions"):
                model = request.get("model") or "%[3]s"
                messages = request.get("messages") or []
                saw_tool_result = any(m.get("role") == "tool" for m in messages if isinstance(m, dict))
                if not saw_tool_result:
                    self._json(200, {
                        "id": "chatcmpl-otel-tool-call",
                        "object": "chat.completion",
                        "model": model,
                        "choices": [{
                            "index": 0,
                            "message": {
                                "role": "assistant",
                                "content": "",
                                "tool_calls": [{
                                    "id": "call_otel_file_write",
                                    "type": "function",
                                    "function": {"name": "file_write", "arguments": TOOL_ARGS},
                                }],
                            },
                            "finish_reason": "tool_calls",
                        }],
                        "usage": {"prompt_tokens": 17, "completion_tokens": 11, "total_tokens": 28},
                    })
                    return

                self._json(200, {
                    "id": "chatcmpl-otel-final",
                    "object": "chat.completion",
                    "model": model,
                    "choices": [{
                        "index": 0,
                        "message": {"role": "assistant", "content": "otel e2e complete"},
                        "finish_reason": "stop",
                    }],
                    "usage": {"prompt_tokens": 23, "completion_tokens": 5, "total_tokens": 28},
                })
                return

            self._json(404, {"error": {"message": "not found", "code": "not_found"}})

    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
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
      app.kubernetes.io/name: %[1]s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %[1]s
    spec:
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: fake-openai
          image: %[4]s
          imagePullPolicy: IfNotPresent
          command: ["python", "/etc/fake-openai/server.py"]
          ports:
            - name: http
              containerPort: 8080
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
            periodSeconds: 2
            failureThreshold: 30
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: server
              mountPath: /etc/fake-openai
              readOnly: true
      volumes:
        - name: server
          configMap:
            name: %[1]s-server
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector:
    app.kubernetes.io/name: %[1]s
  ports:
    - name: http
      port: 8080
      targetPort: http
`, otelFakeOpenAIName, namespace, otelModelName, otelFakeOpenAIImage)
}
