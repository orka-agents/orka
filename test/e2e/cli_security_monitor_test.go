//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
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
    contractVersion: orka.harness.v2
    type: claude
    defaultMaxTurns: 1
  model:
    name: claude-opus-5
`, agentName))
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
		cleanupConfig, err := config.GetConfig()
		Expect(err).NotTo(HaveOccurred())
		cleanupConfig.Timeout = 10 * time.Second
		cleanupScheme := runtime.NewScheme()
		Expect(corev1alpha1.AddToScheme(cleanupScheme)).To(Succeed())
		cleanupReader, err := client.New(cleanupConfig, client.Options{Scheme: cleanupScheme})
		Expect(err).NotTo(HaveOccurred())
		cleanupContext, stopCleanup := context.WithTimeout(context.Background(), 3*time.Minute)
		defer stopCleanup()
		scanSnapshot, err := captureCLISecurityChildTasks(cleanupContext, cleanupReader, &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: repositoryScanName}})
		Expect(err).NotTo(HaveOccurred(), "Failed to capture exact RepositoryScan child identities before deletion")
		monitorSnapshot, err := captureCLISecurityChildTasks(cleanupContext, cleanupReader, &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: monitorName}})
		Expect(err).NotTo(HaveOccurred(), "Failed to capture exact RepositoryMonitor child identities before deletion")
		expectOrkaSuccess(runOrka(home, "monitor", "delete", monitorName), token, fakeAnthropicKey)
		expectOrkaSuccess(runOrka(home, "security", "repo", "delete", repositoryScanName), token, fakeAnthropicKey)
		By("waiting for normal deletion of the exact security parents and their child Tasks")
		Expect(waitCLISecurityChildTasksDeleted(cleanupContext, cleanupReader, scanSnapshot, time.Second)).To(Succeed())
		Expect(waitCLISecurityChildTasksDeleted(cleanupContext, cleanupReader, monitorSnapshot, time.Second)).To(Succeed())
	})
})

type cliSecurityTaskSnapshot struct {
	parent    client.Object
	parentUID types.UID
	children  map[string]types.UID
}

func captureCLISecurityChildTasks(ctx context.Context, reader client.Reader, parent client.Object) (*cliSecurityTaskSnapshot, error) {
	if err := reader.Get(ctx, client.ObjectKeyFromObject(parent), parent); err != nil {
		return nil, errors.New("read security parent identity before CLI deletion")
	}
	if parent.GetUID() == "" || !parent.GetDeletionTimestamp().IsZero() {
		return nil, errors.New("security parent identity is absent or already deleting")
	}
	children := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, children, client.InNamespace(parent.GetNamespace())); err != nil {
		return nil, errors.New("inventory security child Tasks before CLI deletion")
	}
	snapshot := &cliSecurityTaskSnapshot{parent: parent.DeepCopyObject().(client.Object), parentUID: parent.GetUID(), children: map[string]types.UID{}}
	for _, child := range children.Items {
		for _, owner := range child.OwnerReferences {
			if owner.UID == snapshot.parentUID {
				if child.UID == "" {
					return nil, errors.New("security child Task has no exact identity")
				}
				snapshot.children[child.Name] = child.UID
			}
		}
	}
	return snapshot, nil
}

// Parent deletion uses normal garbage collection and Task finalization. This
// observer proves absence without adding a finalizer or claiming a receipt.
func waitCLISecurityChildTasksDeleted(ctx context.Context, reader client.Reader, snapshot *cliSecurityTaskSnapshot, interval time.Duration) error {
	if snapshot == nil || snapshot.parentUID == "" {
		return errors.New("exact security parent deletion identity is required")
	}
	return wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		parent := snapshot.parent.DeepCopyObject().(client.Object)
		if err := reader.Get(ctx, client.ObjectKeyFromObject(parent), parent); err == nil {
			if parent.GetUID() != snapshot.parentUID {
				return false, errors.New("security parent was replaced during CLI cleanup")
			}
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, errors.New("observe exact security parent deletion")
		}
		for name, uid := range snapshot.children {
			child := &corev1alpha1.Task{}
			if err := reader.Get(ctx, client.ObjectKey{Namespace: snapshot.parent.GetNamespace(), Name: name}, child); err == nil {
				if child.UID != uid {
					return false, errors.New("security child Task was replaced during CLI cleanup")
				}
				return false, nil
			} else if !apierrors.IsNotFound(err) {
				return false, errors.New("observe exact security child Task deletion")
			}
		}
		children := &corev1alpha1.TaskList{}
		if err := reader.List(ctx, children, client.InNamespace(snapshot.parent.GetNamespace())); err != nil {
			return false, errors.New("verify security parent has no remaining child Tasks")
		}
		for _, child := range children.Items {
			for _, owner := range child.OwnerReferences {
				if owner.UID == snapshot.parentUID {
					return false, nil
				}
			}
		}
		return true, nil
	})
}

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
