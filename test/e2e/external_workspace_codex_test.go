//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/orka-agents/orka/test/utils"
)

const (
	externalCodexReadMarker               = "[e2e:runtime-codex-external-workspace-read]"
	externalFakeWorkspaceProviderVersion  = "v0.1.0-alpha.5"
	externalFakeWorkspaceProviderRevision = "c10e4ee498903ce1ae6b79df64e0ccde43b81eb4"
)

func externalCodexReadTaskManifest(taskName, agentName, className string) map[string]any {
	manifest := runtimeAgentTaskManifest(taskName, agentName,
		"Use a tool to read README in the repository root. "+
			"Wait for the read to succeed, then reply with the exact file contents and nothing else. "+
			externalCodexReadMarker,
		4, nil, &runtimeWorkspaceConfig{GitRepo: liveRuntimeRepoURL, Ref: liveRuntimeRepoRef}, "", nil, nil)
	manifest["spec"].(map[string]any)["execution"] = map[string]any{
		"workspace": map[string]any{
			"classRef":    map[string]any{"name": className},
			"reusePolicy": "none",
			"onDetach":    "Delete",
		},
	}
	return manifest
}

type externalCodexWorkspaceIdentity struct {
	poolName, poolUID             string
	workspaceName, workspaceUID   string
	podName, podNamespace, podUID string
	journalNames, secretNames     []string
}

func waitForExternalCodexWorkspace(taskName, className, providerName string) externalCodexWorkspaceIdentity {
	var identity externalCodexWorkspaceIdentity
	Eventually(func(g Gomega) {
		task := fetchTaskForACPAssertion(g, taskName)
		g.Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseRunning))
		g.Expect(task.Status.Execution).NotTo(BeNil())
		projection := task.Status.ExecutionWorkspace
		g.Expect(projection).NotTo(BeNil())
		g.Expect(projection.ClassRef).NotTo(BeNil())
		g.Expect(projection.ClassRef.Name).To(Equal(className))
		g.Expect(projection.WorkspaceRef).NotTo(BeNil())
		g.Expect(projection.AttachedEpoch).To(BeNumerically(">", 0))
		g.Expect(projection.TemplateRef).To(BeNil(), "class dispatch must not project backend templates")
		g.Expect(projection.Placement).To(BeNil(), "Task workspace status must stay provider-neutral")
		g.Expect(projection.Density).To(BeNil())

		pool := getRuntimePool(g, task.Status.Execution.RuntimePoolName)
		g.Expect(pool.Name).To(HavePrefix("acp-ws-codex-"))
		g.Expect(string(pool.UID)).To(Equal(task.Status.Execution.RuntimePoolUID))
		g.Expect(pool.Spec.Runtime.Image).To(Equal(acpCodexRuntimeRef))
		g.Expect(pool.Spec.Capacity.MaxResidentSessions).To(Equal(int32(1)))
		g.Expect(pool.Spec.Capacity.MaxRunningPrompts).To(Equal(int32(1)))
		binding := pool.Spec.ExecutionWorkspace
		g.Expect(binding).NotTo(BeNil())
		g.Expect(string(binding.Provider)).To(Equal(externalFakeWorkspaceProviderControllerName))
		g.Expect(binding.WorkspaceRef).NotTo(BeNil())
		g.Expect(binding.WorkspaceRef.Name).To(Equal(projection.WorkspaceRef.Name))
		g.Expect(pool.Status.ActiveInstance).NotTo(BeNil())

		var workspace workspacev1alpha1.ExecutionWorkspace
		kubectlJSON(g, &workspace, "get", "executionworkspace", binding.WorkspaceRef.Name, "-n", namespace)
		g.Expect(string(workspace.UID)).To(Equal(string(binding.WorkspaceRef.UID)))
		g.Expect(projection.WorkspaceRef.UID).To(Equal(string(workspace.UID)))
		g.Expect(workspace.Spec.ClassBinding.Name).To(Equal(className))
		g.Expect(workspace.Spec.ProviderBinding.Name).To(Equal(providerName))
		g.Expect(workspace.Status.Allocation).NotTo(BeNil())
		g.Expect(workspace.Status.Allocation.Startup).NotTo(BeNil())

		active := pool.Status.ActiveInstance
		var pod corev1.Pod
		kubectlJSON(g, &pod, "get", "pod", active.PodName, "-n", active.PodNamespace)
		g.Expect(string(pod.UID)).To(Equal(active.PodUID))
		g.Expect(pod.Spec.Containers).NotTo(BeEmpty())
		g.Expect(pod.Spec.Containers[0].Image).To(Equal(acpCodexRuntimeRef))
		startup := workspace.Status.Allocation.Startup
		g.Expect(startup.Pod).NotTo(BeNil())
		g.Expect(startup.Pod.Name).To(Equal(pod.Name))
		g.Expect(startup.Pod.Namespace).To(Equal(pod.Namespace))
		g.Expect(startup.Pod.UID).To(Equal(pod.UID))
		g.Expect(startup.Process).To(BeNil())
		g.Expect(active.BootID).NotTo(BeEmpty())
		g.Expect(pod.OwnerReferences).NotTo(ContainElement(HaveField("Kind", "ReplicaSet")),
			"the separate provider, not a Core Deployment, must materialize the runtime Pod")

		identity = externalCodexWorkspaceIdentity{
			poolName: pool.Name, poolUID: string(pool.UID),
			workspaceName: workspace.Name, workspaceUID: string(workspace.UID),
			podName: pod.Name, podNamespace: pod.Namespace, podUID: string(pod.UID),
			secretNames: externalCodexOwnedResourceNames(g, "secrets",
				string(task.UID), string(pool.UID), string(workspace.UID)),
			journalNames: externalCodexOwnedResourceNames(g, "configmaps", string(workspace.UID)),
		}
		g.Expect(identity.secretNames).NotTo(BeEmpty(), "real supervisor execution must use Core-owned credentials")
		g.Expect(identity.journalNames).NotTo(BeEmpty(), "the separate provider must persist its materialization journal")
	}, 3*time.Minute, time.Second).Should(Succeed())
	return identity
}

// Only metadata crosses the command boundary; never read or print Secret data.
func externalCodexOwnedResourceNames(g Gomega, kind string, ownerUIDs ...string) []string {
	output, err := utils.Run(exec.Command("kubectl", "get", kind, "-n", namespace,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.ownerReferences[*].uid}{"\n"}{end}`))
	g.Expect(err).NotTo(HaveOccurred())
	var names []string
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, uid := range ownerUIDs {
			for _, owner := range fields[1:] {
				if owner == uid {
					names = append(names, fields[0])
				}
			}
		}
	}
	return names
}

func waitForExternalCodexWorkspaceCleanup(identity externalCodexWorkspaceIdentity) {
	type ownedResource struct{ kind, name, namespace string }
	resources := make([]ownedResource, 0, 3+len(identity.secretNames)+len(identity.journalNames))
	resources = append(resources,
		ownedResource{"runtimepool", identity.poolName, namespace},
		ownedResource{"executionworkspace", identity.workspaceName, namespace},
		ownedResource{"pod", identity.podName, identity.podNamespace},
	)
	for _, name := range identity.secretNames {
		resources = append(resources, ownedResource{"secret", name, namespace})
	}
	for _, name := range identity.journalNames {
		resources = append(resources, ownedResource{"configmap", name, namespace})
	}
	Eventually(func(g Gomega) {
		for _, resource := range resources {
			output, err := utils.Run(exec.Command("kubectl", "get", resource.kind, resource.name,
				"-n", resource.namespace, "--ignore-not-found", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "owned %s/%s must be absent", resource.kind, resource.name)
		}
	}, 3*time.Minute, time.Second).Should(Succeed())
}

func mockLLMServedExternalCodexRead(requests []mockLLMRequest) bool {
	for _, request := range requests {
		var fixture struct {
			Match struct {
				UserMessage        string `json:"userMessage"`
				ToolResultContains string `json:"toolResultContains"`
			} `json:"match"`
		}
		if request.Response.Status >= 200 && request.Response.Status < 300 &&
			json.Unmarshal(request.Response.Fixture, &fixture) == nil &&
			fixture.Match.UserMessage == externalCodexReadMarker && fixture.Match.ToolResultContains == liveRuntimeRepoSentinel {
			return true
		}
	}
	return false
}

// Providers are nested Go modules and are not included in the shared module zip.
// Fetch the complete immutable source revision, never a floating branch or tag.
func buildExternalFakeWorkspaceProvider() (string, error) {
	versionTemplate := "{{if .Replace}}replaced{{else}}{{.Version}}{{end}}"
	output, err := utils.Run(exec.Command("go", "list", "-m", "-f", versionTemplate,
		"github.com/orka-agents/orka-workspace"))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(output) != externalFakeWorkspaceProviderVersion {
		return "", fmt.Errorf("external fake provider source version does not match the pinned Core workspace module")
	}
	sourceDir, err := os.MkdirTemp("", "orka-e2e-fake-provider-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(sourceDir) }()
	for _, args := range [][]string{
		{"init", "--quiet", sourceDir},
		{"-C", sourceDir, "fetch", "--quiet", "--depth=1", "https://github.com/orka-agents/orka-workspace.git",
			externalFakeWorkspaceProviderRevision},
		{"-C", sourceDir, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if _, err := utils.Run(exec.Command("git", args...)); err != nil {
			return "", err
		}
	}
	output, err = utils.Run(exec.Command("git", "-C", sourceDir, "rev-parse", "HEAD"))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(output) != externalFakeWorkspaceProviderRevision {
		return "", fmt.Errorf("external fake provider checkout did not resolve the exact pinned revision")
	}
	// The module tag must name the exact commit built here.
	if _, err := utils.Run(exec.Command("git", "-C", sourceDir, "fetch", "--quiet", "--depth=1",
		"https://github.com/orka-agents/orka-workspace.git", "refs/tags/"+externalFakeWorkspaceProviderVersion)); err != nil {
		return "", err
	}
	output, err = utils.Run(exec.Command("git", "-C", sourceDir, "rev-parse", "FETCH_HEAD^{commit}"))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(output) != externalFakeWorkspaceProviderRevision {
		return "", fmt.Errorf("pinned Core workspace module tag does not resolve to the external fake provider revision")
	}
	image := "ghcr.io/orka-agents/orka/e2e-external-fake-provider:" + externalFakeWorkspaceProviderRevision[:12]
	if _, err := utils.Run(exec.Command("docker", "build", "-t", image,
		"-f", filepath.Join(sourceDir, "providers/fake/Dockerfile"), sourceDir)); err != nil {
		return "", err
	}
	if err := installExternalFakeWorkspaceProviderAPIs(sourceDir); err != nil {
		return "", err
	}
	return pushImageToLocalRegistry(e2eRegistryAddress, image, "orka/external-fake-workspace-provider:e2e")
}

func installExternalFakeWorkspaceProviderAPIs(sourceDir string) error {
	for _, args := range [][]string{
		{"apply", "--server-side", "-f", filepath.Join(sourceDir, "providers/fake/config/crd")},
		{"apply", "--server-side", "-k", filepath.Join(sourceDir, "config/policy")},
		{"wait", "--for=condition=Established", "crd/fakeproviderconfigs.fake.workspace.orka.ai", "--timeout=60s"},
		{"wait", "--for=condition=Established", "crd/fakepoolparameters.fake.workspace.orka.ai", "--timeout=60s"},
	} {
		if _, err := utils.Run(exec.Command("kubectl", args...)); err != nil {
			return err
		}
	}
	return nil
}

func deployExternalFakeWorkspaceProvider() externalFakeWorkspaceProviderFixtureNames {
	By("building the separately deployed fake provider at Core's pinned workspace revision")
	imageRef, err := buildExternalFakeWorkspaceProvider()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	deployment, err := controllerManagerDeploymentName()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	output, err := utils.Run(exec.Command("kubectl", "get", "deployment", deployment,
		"-n", namespace, "-o", "jsonpath={.spec.template.spec.serviceAccountName}"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	manifests, names, err := externalFakeWorkspaceProviderManifests(namespace, imageRef,
		fmt.Sprintf("codex-%d", time.Now().UnixNano()), strings.TrimSpace(output))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() { cleanupExternalFakeWorkspaceProvider(manifests, names) })
	bundle := map[string]any{"apiVersion": "v1", "kind": "List", "items": manifests}
	ExpectWithOffset(1, applyManifestJSON(bundle)).To(Succeed())
	for _, args := range [][]string{
		{"rollout", "status", "deployment/" + names.Deployment, "-n", namespace, "--timeout=3m"},
		{"wait", "--for=condition=Ready", "executionworkspaceprovider/" + names.Provider, "--timeout=90s"},
		{"wait", "--for=condition=Ready", "executionworkspaceclass/" + names.Class, "-n", namespace, "--timeout=90s"},
	} {
		_, err := utils.Run(exec.Command("kubectl", args...))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	return names
}

func cleanupExternalFakeWorkspaceProvider(manifests []map[string]any, names externalFakeWorkspaceProviderFixtureNames) {
	// Keep the provider and its privileges alive until Core has finalized every
	// workspace of this class, including uncertain-create journals.
	EventuallyWithOffset(1, func(g Gomega) {
		var workspaces workspacev1alpha1.ExecutionWorkspaceList
		kubectlJSON(g, &workspaces, "get", "executionworkspaces", "-n", namespace)
		for _, workspace := range workspaces.Items {
			g.Expect(workspace.Spec.ClassBinding.Name).NotTo(Equal(names.Class), "provider must outlive workspace finalization")
		}
	}, 3*time.Minute, time.Second).Should(Succeed())
	reversed := slices.Clone(manifests)
	slices.Reverse(reversed)
	data, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": reversed})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	cmd := exec.Command("kubectl", "delete", "--ignore-not-found", "--wait=true", "--timeout=30s", "-f", "-")
	cmd.Stdin = strings.NewReader(string(data))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = utils.Run(exec.Command("kubectl", "delete", "lease", names.LeaderElectionLease,
		"-n", namespace, "--ignore-not-found", "--wait=true", "--timeout=30s"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}
