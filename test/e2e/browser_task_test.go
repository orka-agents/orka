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
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

const (
	browserSmokePage    = "e2e-browser-smoke-page"
	browserSmokePageURL = "http://" + browserSmokePage + "." + namespace + ".svc:8080/"
)

// browserSmokePageManifest serves one static page inside the cluster.
func browserSmokePageManifest() string {
	return fmt.Sprintf(`
apiVersion: v1
kind: ConfigMap
metadata: {name: %[1]s, namespace: %[2]s}
data:
  index.html: '<!doctype html><title>Orka browser smoke</title><h1 id="greeting">served inside the cluster</h1>'
---
apiVersion: v1
kind: Pod
metadata: {name: %[1]s, namespace: %[2]s, labels: {app: %[1]s}}
spec:
  securityContext: {runAsNonRoot: true, runAsUser: 1000, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: httpd
      image: busybox:1.37
      command: ["httpd", "-f", "-p", "8080", "-h", "/www"]
      ports: [{containerPort: 8080}]
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: www, mountPath: /www}]
  volumes: [{name: www, configMap: {name: %[1]s}}]
---
apiVersion: v1
kind: Service
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  selector: {app: %[1]s}
  ports: [{port: 8080, targetPort: 8080}]
`, browserSmokePage, namespace)
}

// browserTaskSpec is a browser Task as the docs show it: the script in args,
// command left empty, and explicit resources.
func browserTaskSpec(name string, extra map[string]any, args ...string) string {
	spec := map[string]any{
		"type":    "container",
		"image":   browserWorkerRef,
		"args":    args,
		"timeout": "10m",
		"resources": map[string]any{
			"requests": map[string]string{"cpu": "500m", "memory": "1Gi"},
			"limits":   map[string]string{"cpu": "2", "memory": "2Gi"},
		},
	}
	for key, value := range extra {
		spec[key] = value
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Task",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       spec,
	})
	Expect(err).NotTo(HaveOccurred())
	return string(manifest)
}

var _ = Describe("Browser worker image", Ordered, Label("browser"), func() {
	var (
		apiBaseURL     string
		cancelPF       context.CancelFunc
		portForwardCmd *exec.Cmd
		token          string
		tasks          []string
	)
	suffix := time.Now().UnixNano()
	taskName := func(kind string) string {
		name := fmt.Sprintf("e2e-browser-%s-%d", kind, suffix)
		tasks = append(tasks, name)
		return name
	}
	artifact := func(task, filename string) (string, int) {
		body, status, err := doAuthorizedJSONRequest(http.MethodGet,
			fmt.Sprintf("%s/api/v1/tasks/%s/artifacts/%s?namespace=%s", apiBaseURL, task, filename, namespace), token, "", "")
		Expect(err).NotTo(HaveOccurred())
		return body, status
	}
	expectScreenshot := func(task string) {
		body, status := artifact(task, "page.png")
		Expect(status).To(Equal(http.StatusOK), body)
		Expect(strings.HasPrefix(body, "\x89PNG\r\n\x1a\n")).To(BeTrue(), "page.png is not a PNG")
	}

	BeforeAll(func() {
		if browserWorkerRef == "" {
			Skip(browserWorkerEnvVar + "=skip leaves the browser worker image out of this run")
		}
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18142)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		applyManifest(browserSmokePageManifest())
		_, err = utils.Run(exec.Command("kubectl", "wait", "pod/"+browserSmokePage, "-n", namespace,
			"--for=condition=Ready", "--timeout=2m"))
		Expect(err).NotTo(HaveOccurred(), "the in-cluster page did not start")
	})

	AfterAll(func() {
		for _, name := range tasks {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found"))
		}
		for _, kind := range []string{"pod", "service", "configmap"} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", kind, browserSmokePage, "-n", namespace, "--ignore-not-found"))
		}
		stopPortForward(cancelPF, portForwardCmd)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDebugInfo(tasks...)
		}
	})

	It("screenshots an in-cluster page under the hardened Pod defaults and uploads it", func() {
		name := taskName("smoke")
		applyManifest(browserTaskSpec(name, nil, "sh", "-c",
			"playwright screenshot --browser=chromium "+browserSmokePageURL+" /tmp/artifacts/page.png && echo browser-smoke-ok"))
		Expect(waitForTaskCompletion(name, 8*time.Minute)).To(Equal("Succeeded"))
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, name)).To(ContainSubstring("browser-smoke-ok"))
		expectScreenshot(name)
	})

	It("drives Chromium from a Node script with the preinstalled playwright package", func() {
		name := taskName("node")
		script := `const { chromium } = require("playwright");
(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage();
  await page.goto(process.env.PAGE_URL);
  console.log("heading=" + (await page.textContent("#greeting")));
  await page.screenshot({ path: "/tmp/artifacts/page.png" });
  await browser.close();
})();`
		applyManifest(browserTaskSpec(name, map[string]any{
			"env": []map[string]string{{"name": "PAGE_URL", "value": browserSmokePageURL}},
		}, "node", "-e", script))
		Expect(waitForTaskCompletion(name, 8*time.Minute)).To(Equal("Succeeded"))
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, name)).To(ContainSubstring("heading=served inside the cluster"))
		expectScreenshot(name)
	})

	It("runs in a prepared workspace through the worker entrypoint", func() {
		name := taskName("workspace")
		applyManifest(browserTaskSpec(name, map[string]any{
			"workspace": map[string]any{"gitRepo": "https://github.com/octocat/Hello-World.git"},
		}, "sh", "-c", "git rev-parse HEAD"))
		Expect(waitForTaskCompletion(name, 8*time.Minute)).To(Equal("Succeeded"))
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, name)).To(MatchRegexp(`\b[0-9a-f]{40}\b`))
	})

	It("delivers nothing but logs when command replaces the worker entrypoint", func() {
		name := taskName("command")
		manifest := browserTaskSpec(name, map[string]any{"command": []string{"sh", "-c"}},
			"mkdir -p /tmp/artifacts && echo proof > /tmp/artifacts/proof.txt && echo entrypoint-bypassed")
		applyManifest(manifest)
		Expect(waitForTaskCompletion(name, 8*time.Minute)).To(Equal("Succeeded"))
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, name)).To(ContainSubstring("entrypoint-bypassed"))
		Eventually(func() int {
			_, status := artifact(name, "proof.txt")
			return status
		}, 10*time.Second, time.Second).Should(Equal(http.StatusNotFound),
			"an explicit command must bypass the helper, so nothing uploads /tmp/artifacts")
	})

	It("screenshots under gVisor when the cluster has that RuntimeClass", func() {
		if _, err := utils.Run(exec.Command("kubectl", "get", "runtimeclass", "gvisor")); err != nil {
			Skip("this cluster has no gvisor RuntimeClass")
		}
		name := taskName("gvisor")
		applyManifest(browserTaskSpec(name, map[string]any{
			"execution": map[string]any{"runtimeClassName": "gvisor"},
		}, "sh", "-c", "playwright screenshot --browser=chromium "+browserSmokePageURL+" /tmp/artifacts/page.png"))
		Expect(waitForTaskCompletion(name, 8*time.Minute)).To(Equal("Succeeded"))
		expectScreenshot(name)
		pod, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", namespace, "-l", "orka.ai/task="+name,
			"-o", "jsonpath={.items[0].spec.runtimeClassName}"))
		Expect(err).NotTo(HaveOccurred())
		Expect(regexp.MustCompile(`^gvisor$`).MatchString(strings.TrimSpace(pod))).To(BeTrue(), "the Pod did not run under gvisor")
	})
})
