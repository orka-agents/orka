//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Orka CLI security and monitor binary workflows", Ordered, func() {
	var (
		apiBaseURL     string
		portForwardCmd *exec.Cmd
		cancelPF       context.CancelFunc
		token          string
		home           string
		suffix         string
	)

	BeforeAll(func() {
		By("building the orka CLI binary")
		buildOrkaCLI()

		By("setting up a controller API port-forward for security and monitor CLI commands")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18113)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")

		By("creating an isolated CLI home with service-account credentials")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
		home = newIsolatedCLIHome(apiBaseURL, token)
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	AfterAll(func() {
		By("stopping security and monitor CLI controller API port-forward")
		stopPortForward(cancelPF, portForwardCmd)
	})

	It("creates, reads, lists, and deletes security repository scans and repository monitors", func() {
		tmpDir := GinkgoT().TempDir()
		agentName := "e2e-cli-secmon-agent-" + suffix
		secretName := "e2e-cli-secmon-secret-" + suffix
		repositoryScanName := "e2e-cli-security-" + suffix
		monitorName := "e2e-cli-monitor-" + suffix
		fakeAnthropicKey := "fake-secmon-anthropic-key-" + suffix
		repoURL := "https://github.com/orka-agents/orka"

		DeferCleanup(deleteK8sResource, "repositorymonitor", monitorName)
		DeferCleanup(deleteK8sResource, "repositoryscan", repositoryScanName)
		DeferCleanup(deleteK8sResource, "agent", agentName)
		DeferCleanup(deleteK8sResource, "secret", secretName)

		By("creating a fake local credential Secret and Claude runtime Agent for monitor validation")
		Expect(createK8sSecret(secretName, namespace, map[string]string{"ANTHROPIC_API_KEY": fakeAnthropicKey})).To(Succeed())
		agentManifest := writeTempManifest(tmpDir, "agent.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: %s
spec:
  runtime:
    type: claude
    defaultMaxTurns: 1
  secretRef:
    name: %s
`, agentName, secretName))
		expectOrkaSuccess(runOrka(home, "agent", "create", "-f", agentManifest), token, fakeAnthropicKey)

		By("creating and reading a RepositoryScan with required repoURL and analysisAgentRef fields")
		scanManifest := writeTempManifest(tmpDir, "repository-scan.yaml", repositoryScanManifest(repositoryScanName, repoURL, agentName))
		expectOrkaSuccess(runOrka(home, "security", "repo", "create", "-f", scanManifest), token, fakeAnthropicKey)
		scanGet := runOrka(home, "security", "repo", "get", repositoryScanName, "-o", "json")
		expectOrkaSuccess(scanGet, token, fakeAnthropicKey)
		scan := expectJSONObject(scanGet.Stdout)
		Expect(nestedStringFromMap(scan, "metadata", "name")).To(Equal(repositoryScanName))
		Expect(nestedStringFromMap(scan, "metadata", "namespace")).To(Equal(namespace))
		Expect(nestedStringFromMap(scan, "spec", "repoURL")).To(Equal(repoURL))
		Expect(nestedStringFromMap(scan, "spec", "branch")).To(Equal("main"))
		Expect(nestedStringFromMap(scan, "spec", "analysisAgentRef", "name")).To(Equal(agentName))

		By("listing repository scans as parseable JSON")
		scanList := runOrka(home, "security", "repo", "list", "-o", "json")
		expectOrkaSuccess(scanList, token, fakeAnthropicKey)
		expectListContainsName(expectJSONOutput(scanList.Stdout), repositoryScanName)

		By("updating and reading a controlled threat model")
		threatContent := "cli e2e threat model " + suffix
		threatUpdate := runOrka(home, "security", "threat-model", "update", repositoryScanName, "--content", threatContent, "--source", "cli-e2e")
		expectOrkaSuccess(threatUpdate, token, fakeAnthropicKey)
		threatGet := runOrka(home, "security", "threat-model", "get", repositoryScanName, "-o", "json")
		expectOrkaSuccess(threatGet, token, fakeAnthropicKey)
		threat := expectJSONObject(threatGet.Stdout)
		Expect(nestedStringFromMap(threat, "repositoryScan")).To(Equal(repositoryScanName))
		Expect(nestedStringFromMap(threat, "content")).To(Equal(threatContent))
		Expect(nestedStringFromMap(threat, "source")).To(Equal("cli-e2e"))

		By("listing security scan, finding, slice, and dropped-finding paths as parseable JSON")
		for _, args := range [][]string{
			{"security", "scan", "list", repositoryScanName, "-o", "json"},
			{"security", "finding", "list", repositoryScanName, "-o", "json"},
			{"security", "slice", "list", repositoryScanName, "-o", "json"},
			{"security", "dropped-findings", "list", repositoryScanName, "-o", "json"},
		} {
			result := runOrka(home, args...)
			expectOrkaSuccess(result, token, fakeAnthropicKey)
			expectJSONObject(result.Stdout)
		}

		if os.Getenv("ORKA_CLI_E2E_LIVE_ACTIONS") == "1" {
			By("triggering an explicitly enabled manual security scan run")
			scanRun := runOrka(home, "security", "scan", "run", repositoryScanName)
			expectOrkaSuccess(scanRun, token, fakeAnthropicKey)
		}

		By("creating and reading a RepositoryMonitor with required repoURL and reviewer agent fields")
		monitorManifest := writeTempManifest(tmpDir, "repository-monitor.yaml", repositoryMonitorManifest(monitorName, repoURL, agentName))
		expectOrkaSuccess(runOrka(home, "monitor", "create", "-f", monitorManifest), token, fakeAnthropicKey)
		monitorGet := runOrka(home, "monitor", "get", monitorName, "-o", "json")
		expectOrkaSuccess(monitorGet, token, fakeAnthropicKey)
		monitor := expectJSONObject(monitorGet.Stdout)
		Expect(nestedStringFromMap(monitor, "metadata", "name")).To(Equal(monitorName))
		Expect(nestedStringFromMap(monitor, "metadata", "namespace")).To(Equal(namespace))
		Expect(nestedStringFromMap(monitor, "spec", "repoURL")).To(Equal(repoURL))
		Expect(nestedStringFromMap(monitor, "spec", "branch")).To(Equal("main"))
		Expect(nestedStringFromMap(monitor, "spec", "agents", "reviewer", "name")).To(Equal(agentName))

		By("listing repository monitors as parseable JSON")
		monitorList := runOrka(home, "monitor", "list", "-o", "json")
		expectOrkaSuccess(monitorList, token, fakeAnthropicKey)
		expectListContainsName(expectJSONOutput(monitorList.Stdout), monitorName)

		By("listing monitor runs, items, and events as parseable JSON")
		for _, args := range [][]string{
			{"monitor", "runs", monitorName, "-o", "json"},
			{"monitor", "items", monitorName, "-o", "json"},
			{"monitor", "events", monitorName, "-o", "json"},
		} {
			result := runOrka(home, args...)
			expectOrkaSuccess(result, token, fakeAnthropicKey)
			expectJSONObject(result.Stdout)
		}

		if os.Getenv("ORKA_CLI_E2E_LIVE_ACTIONS") == "1" {
			By("triggering an explicitly enabled repository monitor run")
			monitorRun := runOrka(home, "monitor", "run", monitorName, "--target-kind", "pull_request", "--target-number", "1")
			expectOrkaSuccess(monitorRun, token, fakeAnthropicKey)
		}

		By("deleting the monitor and repository scan through the CLI")
		expectOrkaSuccess(runOrka(home, "monitor", "delete", monitorName), token, fakeAnthropicKey)
		expectOrkaSuccess(runOrka(home, "security", "repo", "delete", repositoryScanName), token, fakeAnthropicKey)
	})
})

// repositoryScanManifest returns a minimal REST-compatible RepositoryScan.
// The API requires spec.repoURL and spec.analysisAgentRef.name; branch and validationMode are fixed for stable e2e reads.
func repositoryScanManifest(name, repoURL, analysisAgent string) string {
	return fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryScan
metadata:
  name: %s
spec:
  repoURL: %s
  branch: main
  validationMode: "off"
  analysisAgentRef:
    name: %s
`, name, repoURL, analysisAgent)
}

// repositoryMonitorManifest returns a minimal REST-compatible RepositoryMonitor.
// The API requires spec.repoURL plus spec.agents.reviewer.name when pull-request monitoring is enabled.
func repositoryMonitorManifest(name, repoURL, reviewerAgent string) string {
	return fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryMonitor
metadata:
  name: %s
spec:
  repoURL: %s
  branch: main
  targets:
    pullRequests:
      enabled: true
  agents:
    reviewer:
      name: %s
`, name, repoURL, reviewerAgent)
}
