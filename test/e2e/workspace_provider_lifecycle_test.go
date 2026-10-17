//go:build e2e

package e2e

// Workspace provider lifecycle runs the same Codex Tasks, scripted by aimock,
// against a real out-of-tree provider: agent-sandbox or Agent Substrate.
// E2E_WORKSPACE_PROVIDER selects it; every other lane skips this file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
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
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/orka-agents/orka/test/fixtures/fakeforge"
	"github.com/orka-agents/orka/test/utils"
)

const (
	workspaceProviderEnv = "E2E_WORKSPACE_PROVIDER"
	// E2E_ORKA_WORKSPACE_DIR builds providers from a local checkout, as the
	// orka-workspace CI does, instead of the revision Core pins.
	workspaceSourceEnv = "E2E_ORKA_WORKSPACE_DIR"

	wspData        = "ORKA_WSP_DURABLE_DATA"
	wspProviderNS  = "orka-workspace-system"
	wspSandboxNS   = defaultRuntimeNamespace
	wspSandboxUID  = "sandbox.workspace.orka.ai/workspace-uid"
	wspAtespace    = "orka-system"
	wspNativePool  = "ate-demo"
	wspNativeOwner = "substrate.workspace.orka.ai/workspace-uid"
)

// workspaceBackend holds what differs between providers. Everything else is
// asserted through Orka's provider-neutral Task and workspace objects.
type workspaceBackend struct {
	name, deployment, controllerName string
	apiPort                          int // distinct per provider so both lanes can share a host
	profileGroup, profileKind        string
	profileSpec                      func(suspend bool) map[string]any
	prepare                          func(source string)
	patchProvider                    func(deployment map[string]any)
	loseInstance                     func(ws workspaceIdentity)
	leftovers                        func(g Gomega, workspaceUID string) []string
	// nativeLeftovers lists backend objects that carry no workspace label,
	// checked once every workspace is gone; nil when there are none.
	nativeLeftovers func(g Gomega) []string
	// unbootable is a profile whose instance can never start, for the boot
	// time limit; nil skips that check.
	unbootable map[string]any
}

func sandboxWorkspaceBackend() workspaceBackend {
	return workspaceBackend{
		name: "sandbox", deployment: "orka-workspace-sandbox", controllerName: "sandbox.workspace.orka.ai", apiPort: 18096,
		profileGroup: "sandbox.workspace.orka.ai", profileKind: "SandboxWorkspaceProfile",
		profileSpec: func(suspend bool) map[string]any {
			if !suspend {
				return map[string]any{}
			}
			return map[string]any{"suspend": map[string]any{"mode": "DataOnly", "volume": map[string]any{"capacity": "1Gi"}}}
		},
		// Install the agent-sandbox release the provider is built against.
		prepare: func(source string) {
			version, err := utils.Run(exec.Command("go", "-C", filepath.Join(source, "providers"),
				"list", "-m", "-f", "{{.Version}}", "sigs.k8s.io/agent-sandbox"))
			Expect(err).NotTo(HaveOccurred())
			base := "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/" + strings.TrimSpace(version)
			for _, args := range [][]string{
				{"apply", "-f", base + "/sandbox.yaml"},
				{"apply", "-f", base + "/extensions.yaml"},
				{"wait", "--for=condition=Established", "--timeout=90s", "crd/sandboxes.agents.x-k8s.io",
					"crd/sandboxclaims.extensions.agents.x-k8s.io", "crd/sandboxtemplates.extensions.agents.x-k8s.io",
					"crd/sandboxwarmpools.extensions.agents.x-k8s.io"},
				{"-n", "agent-sandbox-system", "rollout", "status", "deployment/agent-sandbox-controller", "--timeout=5m"},
			} {
				_, err := utils.Run(exec.Command("kubectl", args...))
				Expect(err).NotTo(HaveOccurred())
			}
		},
		// Upstream creates the Sandbox from the provider's claim without its
		// labels, so delete the exact runtime Pod Core recorded as active.
		loseInstance: func(ws workspaceIdentity) {
			var pool corev1alpha1.RuntimePool
			kubectlJSON(Default, &pool, "get", "runtimepool", ws.pool, "-n", namespace)
			active := pool.Status.ActiveInstance
			Expect(active).NotTo(BeNil(), "the held prompt's pool has no active runtime instance")
			_, err := utils.Run(exec.Command("kubectl", "-n", active.PodNamespace, "delete", "pod", active.PodName, "--wait=false"))
			Expect(err).NotTo(HaveOccurred())
		},
		leftovers: func(g Gomega, workspaceUID string) []string {
			output, err := utils.Run(exec.Command("kubectl", "-n", wspSandboxNS, "get",
				"sandboxclaims,sandboxwarmpools,sandboxtemplates,sandboxes.agents.x-k8s.io,configmaps",
				"-l", wspSandboxUID+"="+workspaceUID, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			return utils.GetNonEmptyLines(output)
		},
		// Sandboxes and their data volumes carry no workspace label.
		nativeLeftovers: func(g Gomega) []string {
			output, err := utils.Run(exec.Command("kubectl", "-n", wspSandboxNS, "get",
				"sandboxes.agents.x-k8s.io,persistentvolumeclaims", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			return utils.GetNonEmptyLines(output)
		},
	}
}

// The Substrate cluster, WorkerPool, ActorTemplate, and Actor DNS come from
// scripts/workspace-substrate-cluster.sh, which also builds kubectl-ate.
func substrateWorkspaceBackend() workspaceBackend {
	kubectlATE := func(args ...string) (string, error) {
		path := os.Getenv("E2E_SUBSTRATE_KUBECTL_ATE")
		Expect(path).NotTo(BeEmpty(), "E2E_SUBSTRATE_KUBECTL_ATE must name the kubectl-ate binary")
		return utils.Run(exec.Command(path, args...))
	}
	nativeNames := func(g Gomega, kind string) []string {
		output, err := kubectlATE("get", kind, "--atespace", wspAtespace, "-o", "json")
		g.Expect(err).NotTo(HaveOccurred())
		var listed map[string][]struct {
			Metadata struct{ Name string } `json:"metadata"`
		}
		g.Expect(json.Unmarshal([]byte(output), &listed)).To(Succeed())
		var names []string
		for _, item := range listed[kind] {
			names = append(names, kind+"/"+item.Metadata.Name)
		}
		return names
	}
	return workspaceBackend{
		name: "substrate", deployment: "orka-workspace-substrate", controllerName: "v1.substrate.workspace.orka.ai", apiPort: 18097,
		profileGroup: "substrate.workspace.orka.ai", profileKind: "SubstrateWorkspaceProfile",
		profileSpec: func(suspend bool) map[string]any {
			spec := map[string]any{"templateRef": map[string]any{"namespace": wspAtespace, "name": "orka-acp-infra"}}
			if suspend {
				spec["suspend"] = map[string]any{"mode": "DataOnly"}
			}
			return spec
		},
		prepare: func(string) {},
		// The provider authenticates to Substrate with a PodCertificate and
		// dials Actors directly.
		patchProvider: func(deployment map[string]any) {
			pod := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			security, _ := pod["securityContext"].(map[string]any)
			if security == nil {
				security = map[string]any{}
			}
			security["runAsUser"], security["runAsGroup"], security["fsGroup"] = 65532, 65532, 65532
			pod["securityContext"] = security
			pod["volumes"] = []any{
				map[string]any{"name": "native-client", "projected": map[string]any{"sources": []any{map[string]any{"podCertificate": map[string]any{
					"signerName": "podidentity.podcert.ate.dev/identity", "keyType": "ECDSAP256", "credentialBundlePath": "credential-bundle.pem"}}}}},
				map[string]any{"name": "native-server", "projected": map[string]any{"sources": []any{map[string]any{"clusterTrustBundle": map[string]any{
					"signerName": "servicedns.podcert.ate.dev/identity", "path": "trust-bundle.pem",
					"labelSelector": map[string]any{"matchLabels": map[string]any{"podcert.ate.dev/canarying": "live"}}}}}}},
			}
			container := pod["containers"].([]any)[0].(map[string]any)
			container["volumeMounts"] = []any{
				map[string]any{"name": "native-client", "mountPath": "/native/client", "readOnly": true},
				map[string]any{"name": "native-server", "mountPath": "/native/server", "readOnly": true},
			}
			var args []any
			for _, arg := range container["args"].([]any) {
				value := arg.(string)
				if !strings.HasPrefix(value, "--native-ca-file=") && !strings.HasPrefix(value, "--native-token-file=") &&
					!strings.HasPrefix(value, "--native-direct-egress=") && !strings.HasPrefix(value, "--native-boot-timeout=") {
					args = append(args, value)
				}
			}
			// A shorter boot limit keeps the never-starting boot check short.
			container["args"] = append(args, "--native-ca-file=/native/server/trust-bundle.pem",
				"--native-cert-file=/native/client/credential-bundle.pem",
				"--native-key-file=/native/client/credential-bundle.pem", "--native-direct-egress=true",
				"--native-boot-timeout=4m")
		},
		loseInstance: func(workspaceIdentity) {
			var actors []string
			Eventually(func(g Gomega) {
				actors = nativeNames(g, "actors")
				g.Expect(actors).To(HaveLen(1), "exactly the held Task's Actor must run")
			}, time.Minute, 2*time.Second).Should(Succeed())
			// Upstream can report a filesystem cleanup error after it has
			// terminated the Actor; the Task and workspace prove the loss.
			_, _ = kubectlATE("delete", "actor", strings.TrimPrefix(actors[0], "actors/"),
				"--atespace", wspAtespace, "--any-state")
		},
		leftovers: func(g Gomega, workspaceUID string) []string {
			output, err := utils.Run(exec.Command("kubectl", "-n", wspNativePool, "get", "workerpools,pods,networkpolicies",
				"-l", wspNativeOwner+"="+workspaceUID, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			return utils.GetNonEmptyLines(output)
		},
		// Actors and checkpoint Tags are atespace-wide; suspended workspaces
		// keep theirs until deleted.
		// Checkpoint catalogs hold a provider finalizer until their Tags are
		// released, so drain them while the provider is still registered.
		nativeLeftovers: func(g Gomega) []string {
			left := append(nativeNames(g, "actors"), nativeNames(g, "tags")...)
			output, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", "configmaps",
				"-l", "substrate.workspace.orka.ai/checkpoint-catalog", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			return append(left, utils.GetNonEmptyLines(output)...)
		},
		// No worker carries this template's selector; see the cluster script.
		unbootable: map[string]any{"templateRef": map[string]any{"namespace": wspAtespace, "name": "orka-acp-unschedulable"}},
	}
}

var _ = Describe("Workspace Provider Lifecycle", Ordered, Label("workspace-provider"), func() {
	var (
		backend    workspaceBackend
		apiBaseURL string
		token      string
		cancelPF   context.CancelFunc
		pfCmd      *exec.Cmd
		runID      string
		agent      string
		suspend    string // Session-scoped class that suspends on detach
		deleting   string // per-Task class that deletes on detach
		dataWS     workspaceIdentity
		// checkpoints reports whether the provider advertises export and restore.
		checkpoints bool
		checkpoint  workspacev1alpha1.ExecutionWorkspaceCheckpoint
		// writeWorkspace returns a write-intent workspace on the fake forge
		// that publishes to pushBranch, opening a pull request when asked.
		writeWorkspace func(pushBranch string, createPR bool) map[string]any
		// Every workspace UID the specs bound, for the final leftover check.
		workspaceUIDs []string
	)

	name := func(prefix string) string { return prefix + "-" + runID }
	session := func(prefix string) string { return name(prefix + "-s") }
	marker := func(check string) string { return "[e2e:wsp-" + check + "]" }
	// teardown deletes this run's Sessions, Tasks, checkpoints, and workspaces
	// and waits until the backend holds nothing for them. It is idempotent.
	teardown := func() {
		// Tasks go first: an active Task holds its Session's mutation lease.
		output, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", "tasks", "-o", "name"))
		Expect(err).NotTo(HaveOccurred())
		var tasks []string
		for _, task := range utils.GetNonEmptyLines(output) {
			if task = strings.TrimPrefix(task, "task.core.orka.ai/"); strings.HasSuffix(task, "-"+runID) {
				tasks = append(tasks, task)
			}
		}
		deleteE2ETasks(apiBaseURL, token, tasks...)
		for _, s := range []string{session("data"), session("timeout")} {
			deleteSessionViaAPI(apiBaseURL, token, s)
		}
		for _, args := range [][]string{
			{"delete", "executionworkspacecheckpoints", "--all"},
			{"delete", "executionworkspaces", "--all"},
		} {
			_, err := utils.Run(exec.Command("kubectl", append(append([]string{"-n", namespace}, args...), "--timeout=10m")...))
			Expect(err).NotTo(HaveOccurred())
		}
		Eventually(func(g Gomega) {
			output, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", "runtimepools", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(ContainSubstring("acp-ws-"))
			for _, uid := range workspaceUIDs {
				g.Expect(backend.leftovers(g, uid)).To(BeEmpty(), "workspace %s left backend resources", uid)
			}
			if backend.nativeLeftovers != nil {
				g.Expect(backend.nativeLeftovers(g)).To(BeEmpty(), "deleted workspaces left native resources")
			}
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
	}
	// dataTask runs in the data Session. Read-intent workspaces are read-only
	// to the agent, and a session workspace freezes the runtime profile, which
	// includes the intent, so every Task in this Session has write intent and
	// publishes to its own branch.
	dataTask := func(task string, create bool, prompt string) map[string]any {
		manifest := wspTask(task, agent, suspend, session("data"), create, prompt, "15m", nil)
		manifest["spec"].(map[string]any)["workspace"] = writeWorkspace("orka/e2e-"+task, false)
		return manifest
	}
	// restoreTask restores a data Session checkpoint, which keeps its source
	// repository.
	restoreTask := func(task, prompt string, from *workspacev1alpha1.ExecutionWorkspaceCheckpoint) map[string]any {
		manifest := wspTask(task, agent, suspend, "", false, prompt, "15m", from)
		manifest["spec"].(map[string]any)["workspace"] = writeWorkspace("orka/e2e-"+task, false)
		return manifest
	}

	BeforeAll(func() {
		switch os.Getenv(workspaceProviderEnv) {
		case "sandbox":
			backend = sandboxWorkspaceBackend()
		case "substrate":
			backend = substrateWorkspaceBackend()
		default:
			Skip(workspaceProviderEnv + " selects no real workspace provider")
		}
		runID = fmt.Sprintf("%x", time.Now().Unix()%0xfffff)
		var err error
		apiBaseURL, cancelPF, pfCmd, err = startControllerAPIPortForward(backend.apiPort)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { stopPortForward(cancelPF, pfCmd) })
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		By("deploying aimock as the Vekil stand-in behind the provider proxy")
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(providerProxyMockLLM.delete)
		providerProxyMockLLM.deploy(projectDir)

		By("installing the " + backend.name + " backend and provider")
		source := workspaceProviderSource()
		backend.prepare(source)
		features := installWorkspaceProvider(backend, source)
		checkpoints = slices.Contains(features, workspacev1alpha1.WorkspaceFeatureCheckpoint) &&
			slices.Contains(features, workspacev1alpha1.WorkspaceFeatureRestore)

		By("deploying the fake forge and letting the publisher reach it")
		DeferCleanup(deleteE2EObjects, "deployment,service,secret", fakeForgeName)
		caPEM := deployFakeForge()
		DeferCleanup(deleteE2EObjects, "networkpolicy", fakeForgeNetworkPolicy)
		trustFakeForgeInPublisher(caPEM)
		repoURL := "https://" + fakeForgeHost() + "/" + fakeForgeRepository + ".git"
		credentials := map[string]any{}
		for _, role := range fakeforge.Roles {
			DeferCleanup(deleteE2EObjects, "secret", fakeForgeCredential(role))
			Expect(createK8sSecret(fakeForgeCredential(role), namespace,
				map[string]string{"token": fakeForgeToken(role)})).To(Succeed())
			credentials[role] = map[string]any{"name": fakeForgeCredential(role), "key": "token"}
		}
		writeWorkspace = func(pushBranch string, createPR bool) map[string]any {
			workspace := map[string]any{
				"intent": "write", "gitRepo": repoURL, "branch": "main",
				"readCredentialRef":            credentials[fakeforge.RoleRead],
				"publicationGitRepo":           repoURL,
				"publicationReadCredentialRef": credentials[fakeforge.RolePublicationRead],
				"publicationCredentialRef":     credentials[fakeforge.RolePush],
				"pushBranch":                   pushBranch,
			}
			if createPR {
				maps.Copy(workspace, map[string]any{
					"forgeCredentialRef": credentials[fakeforge.RoleForge], "prBaseBranch": "main",
					"prTitle": "E2E workspace provider publication", "createPR": true,
				})
			}
			return workspace
		}

		agent = name("wsp-agent")
		Expect(applyManifestJSON(runtimeAgentManifest(agent, "codex", runtimeMatrixMockCodexModel, 4, nil))).To(Succeed())
		suspend, deleting = name("wsp-suspend"), name("wsp-delete")
		applyWorkspaceClass(backend, suspend, workspaceClassOptions{suspend: true})
		applyWorkspaceClass(backend, deleting, workspaceClassOptions{})
		// A failed spec skips the final teardown spec. Registered last, this
		// runs first: while the fake forge and the provider still run.
		DeferCleanup(teardown)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpWorkspaceProviderDiagnostics(backend)
		}
	})

	It("runs a Task through the workspace class and removes the workspace on detach", func() {
		class := deleting
		task := name("wsp-read")
		manifest := externalCodexReadTaskManifest(task, agent, class)
		Expect(applyManifestJSON(manifest)).To(Succeed())
		ws := waitTaskWorkspace(task, class, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)

		Expect(waitForTaskCompletion(task, 10*time.Minute)).To(Equal("Succeeded"))
		Expect(strings.TrimSpace(fetchTaskResultSummaryViaAPI(apiBaseURL, token, task))).To(HaveSuffix(liveRuntimeRepoSentinel))
		deleteE2ETasks(apiBaseURL, token, task)
		waitWorkspaceGone(backend, ws)
	})

	It("clones, edits, and publishes a branch with a pull request from the workspace", func() {
		const pushBranch, file = "orka/e2e-wsp", "WSP_PUBLISH.md"
		task := name("wsp-publish")
		manifest := wspTask(task, agent, deleting, "", false, "Create "+file+", then report it. "+marker("publish"), "15m", nil)
		manifest["spec"].(map[string]any)["workspace"] = writeWorkspace(pushBranch, true)
		Expect(applyManifestJSON(manifest)).To(Succeed())
		ws := waitTaskWorkspace(task, deleting, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		Expect(waitForTaskCompletion(task, 15*time.Minute)).To(Equal("Succeeded"))
		var delivery corev1alpha1.TaskDeliveryStatus
		Eventually(func(g Gomega) {
			current := fetchTaskForACPAssertion(g, task)
			g.Expect(current.Status.Delivery).NotTo(BeNil())
			delivery = *current.Status.Delivery
			g.Expect(delivery.Outcome).To(Equal(corev1alpha1.TaskDeliveryOutcomeVerifiedExact))
			g.Expect(delivery.PRReceipt).NotTo(BeNil())
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
		Expect(fakeForgeState().Branches[fakeForgeRepository]).To(HaveKeyWithValue(pushBranch, delivery.VerifiedRemoteSHA))
		Expect(fakeForgeFile(pushBranch, file)).To(Equal("ORKA_WSP_PUBLISHED\n"))
		deleteE2ETasks(apiBaseURL, token, task)
		waitWorkspaceGone(backend, ws)
	})

	It("lets the workspace reach the provider proxy but not other destinations", func() {
		task := name("wsp-egress")
		Expect(applyManifestJSON(wspTask(task, agent, deleting, "", false,
			"Check the workspace network. "+marker("egress"), "15m", nil))).To(Succeed())
		ws := waitTaskWorkspace(task, deleting, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		waitTaskExecution(task, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, task)).To(ContainSubstring("ORKA_WSP_EGRESS_OK"),
			"the workspace must reach only the provider proxy")
		deleteE2ETasks(apiBaseURL, token, task)
		waitWorkspaceGone(backend, ws)
	})

	It("keeps workspace data across suspend and cold resume", func() {
		write := name("wsp-write")
		Expect(applyManifestJSON(dataTask(write, true, "Write the durable workspace file. "+marker("write")))).To(Succeed())
		dataWS = waitTaskWorkspace(write, suspend, backend)
		workspaceUIDs = append(workspaceUIDs, dataWS.uid)
		first := waitTaskExecution(write, corev1alpha1.TaskExecutionStateSucceeded)
		Eventually(func(g Gomega) {
			delivery := fetchTaskForACPAssertion(g, write).Status.Delivery
			g.Expect(delivery).NotTo(BeNil())
			g.Expect(delivery.Outcome).To(Equal(corev1alpha1.TaskDeliveryOutcomeVerifiedExact))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
		Expect(fakeForgeFile("orka/e2e-"+write, "orka-wsp-data")).To(Equal("ORKA_WSP_DURABLE_DATA\n"))
		waitWorkspaceState(dataWS.name, workspacev1alpha1.ExecutionWorkspaceStateSuspended)
		waitRuntimePoolStopped(dataWS.pool)

		read := name("wsp-resume")
		Expect(applyManifestJSON(dataTask(read, false,
			"Read the durable workspace file. "+marker("resume")))).To(Succeed())
		second := waitTaskExecution(read, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, read)).To(ContainSubstring("ORKA_WSP_READ_OK"))
		Expect(waitTaskWorkspace(read, suspend, backend).uid).To(Equal(dataWS.uid), "the Session must resume its workspace")
		Expect(second.Status.Execution.RuntimeInstanceID).NotTo(Equal(first.Status.Execution.RuntimeInstanceID),
			"cold resume must start a new runtime instance")
		waitWorkspaceState(dataWS.name, workspacev1alpha1.ExecutionWorkspaceStateSuspended)
	})

	It("exports a checkpoint and restores its data into a new workspace", func() {
		if !checkpoints {
			Skip(backend.name + " does not advertise checkpoint export and restore")
		}
		checkpoint = exportCheckpoint(name("wsp-save"), dataWS, false)
		restore := name("wsp-restore")
		Expect(applyManifestJSON(restoreTask(restore,
			"Read the durable workspace file. "+marker("restore"), &checkpoint))).To(Succeed())
		ws := waitTaskWorkspace(restore, suspend, backend)
		Expect(ws.uid).NotTo(Equal(dataWS.uid))
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		waitTaskExecution(restore, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, restore)).To(ContainSubstring("ORKA_WSP_READ_OK"))
		waitWorkspaceGone(backend, ws)
	})

	It("finishes a suspension across a provider restart", func() {
		task := name("wsp-provider-restart")
		Expect(applyManifestJSON(dataTask(task, false,
			"Read the durable workspace file. "+marker("provider-restart")))).To(Succeed())
		waitTaskExecution(task, corev1alpha1.TaskExecutionStateSucceeded)
		Eventually(func(g Gomega) {
			var workspace workspacev1alpha1.ExecutionWorkspace
			kubectlJSON(g, &workspace, "get", "executionworkspace", dataWS.name, "-n", namespace)
			g.Expect(workspace.Spec.DesiredState).To(Equal(workspacev1alpha1.ExecutionWorkspaceDesiredSuspended))
		}, 5*time.Minute, time.Second).Should(Succeed())

		By("restarting the provider while it suspends the workspace")
		for _, args := range [][]string{{"rollout", "restart"}, {"rollout", "status", "--timeout=5m"}} {
			_, err := utils.Run(exec.Command("kubectl", append([]string{"-n", wspProviderNS}, append(args, "deployment/"+backend.deployment)...)...))
			Expect(err).NotTo(HaveOccurred())
		}
		waitWorkspaceState(dataWS.name, workspacev1alpha1.ExecutionWorkspaceStateSuspended)
		next := name("wsp-after-provider-restart")
		Expect(applyManifestJSON(dataTask(next, false,
			"Read the durable workspace file. "+marker("after-provider-restart")))).To(Succeed())
		waitTaskExecution(next, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, next)).To(ContainSubstring("ORKA_WSP_READ_OK"))
		waitWorkspaceState(dataWS.name, workspacev1alpha1.ExecutionWorkspaceStateSuspended)
	})

	It("cancels a held prompt at its Task timeout without replaying it", func() {
		task := name("wsp-timeout")
		Expect(applyManifestJSON(wspTask(task, agent, suspend, session("timeout"), true,
			"Wait for the model. "+marker("timeout"), "45s", nil))).To(Succeed())
		ws := waitTaskWorkspace(task, suspend, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		final := waitTaskExecution(task, corev1alpha1.TaskExecutionStateCancelled)
		Expect(final.Status.Execution.Reason).To(Equal(corev1alpha1.TaskExecutionReason("TaskTimeout")))
		expectServedOnce(marker("timeout"))
		waitWorkspaceState(ws.name, workspacev1alpha1.ExecutionWorkspaceStateSuspended)
	})

	It("cancels a held prompt when its Task is deleted", func() {
		task := name("wsp-cancel")
		Expect(applyManifestJSON(wspTask(task, agent, deleting, "", false,
			"Wait for the model. "+marker("cancel"), "1h", nil))).To(Succeed())
		ws := waitTaskWorkspace(task, deleting, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		waitHeldPrompt(task, marker("cancel"))
		uid := string(fetchTaskForACPAssertion(Default, task).UID)
		addTaskObserverFinalizer(task, uid)
		// A failed assertion skips the release below and would leave the
		// Task terminating, which teardown cannot clean up.
		DeferCleanup(releaseHeldTaskObserverFinalizer, task, uid)
		_, err := utils.Run(exec.Command("kubectl", "delete", "task", task, "-n", namespace, "--wait=false"))
		Expect(err).NotTo(HaveOccurred())
		waitTaskExecution(task, corev1alpha1.TaskExecutionStateCancelled)
		expectServedOnce(marker("cancel"))
		waitTaskObserverReleaseReady(task, uid)
		releaseTaskObserverFinalizer(task, uid)
		waitTaskAbsent(task)
		waitWorkspaceGone(backend, ws)
	})

	It("settles a held prompt once across a controller restart and continues the Session", func() {
		task := name("wsp-restart")
		Expect(applyManifestJSON(dataTask(task, false,
			"Wait for the model. "+marker("restart")))).To(Succeed())
		waitHeldPrompt(task, marker("restart"))

		By("restarting the controller while the prompt is held")
		stopPortForward(cancelPF, pfCmd)
		deployment, err := controllerManagerDeploymentName()
		Expect(err).NotTo(HaveOccurred())
		for _, args := range [][]string{{"rollout", "restart"}, {"rollout", "status", "--timeout=10m"}} {
			_, err := utils.Run(exec.Command("kubectl", append([]string{"-n", namespace}, append(args, "deployment/"+deployment)...)...))
			Expect(err).NotTo(HaveOccurred())
		}
		apiBaseURL, cancelPF, pfCmd, err = startControllerAPIPortForward(backend.apiPort)
		Expect(err).NotTo(HaveOccurred())

		waitTaskExecution(task, terminalExecutionStates...)
		expectServedOnce(marker("restart"))
		next := name("wsp-after-restart")
		Expect(applyManifestJSON(dataTask(next, false,
			"Read the durable workspace file. "+marker("after-restart")))).To(Succeed())
		waitTaskExecution(next, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, next)).To(ContainSubstring("ORKA_WSP_READ_OK"))
	})

	It("settles a prompt whose runtime instance is lost and fails its workspace", func() {
		task := name("wsp-lost")
		Expect(applyManifestJSON(dataTask(task, false,
			"Wait for the model. "+marker("lost")))).To(Succeed())
		waitHeldPrompt(task, marker("lost"))
		lost := time.Now()
		backend.loseInstance(dataWS)
		// A deleted Sandbox Pod terminates gracefully, so its supervisor reports
		// the prompt failed; an abruptly lost Actor leaves the outcome unknown.
		// Either way the prompt settles once and never succeeds.
		waitTaskExecution(task, corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionStateOutcomeUnknown)
		// The model hold is far longer, so only the loss can settle it this soon.
		Expect(time.Since(lost)).To(BeNumerically("<", 2*time.Minute), "the prompt settled by timing out, not by instance loss")
		// The dying runtime may retry its own in-flight request, so count
		// replays only after the Task settles.
		expectNoReplayAfterSettle(marker("lost"))

		By("failing the workspace instead of restarting the lost instance")
		waitWorkspaceState(dataWS.name, workspacev1alpha1.ExecutionWorkspaceStateFailed)
		if !checkpoints {
			By("failing the Session's next Task instead of leaving it pending")
			next := name("wsp-after-loss")
			Expect(applyManifestJSON(dataTask(next, false,
				"Read the durable workspace file. "+marker("after-loss")))).To(Succeed())
			failed := waitTaskExecution(next, corev1alpha1.TaskExecutionStateFailed)
			Expect(failed.Status.Execution.Reason).To(Equal(corev1alpha1.TaskExecutionReason("WorkspaceFailed")))
			return
		}

		By("exporting its last checkpoint only on explicit recovery")
		recovered := exportCheckpoint(name("wsp-recover"), dataWS, true)
		restore := name("wsp-after-loss")
		Expect(applyManifestJSON(restoreTask(restore,
			"Read the durable workspace file. "+marker("after-loss"), &recovered))).To(Succeed())
		ws := waitTaskWorkspace(restore, suspend, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		waitTaskExecution(restore, corev1alpha1.TaskExecutionStateSucceeded)
		Expect(fetchTaskResultViaAPI(apiBaseURL, token, restore)).To(ContainSubstring("ORKA_WSP_READ_OK"))
		waitWorkspaceGone(backend, ws)
	})

	It("cancels a running Task when its workspace reaches maxLifetime", func() {
		class := name("wsp-lifetime")
		applyWorkspaceClass(backend, class, workspaceClassOptions{maxLifetime: "120s"})
		task := name("wsp-lifetime")
		Expect(applyManifestJSON(wspTask(task, agent, class, "", false,
			"Wait for the model. "+marker("lifetime"), "15m", nil))).To(Succeed())
		ws := waitTaskWorkspace(task, class, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		waitHeldPrompt(task, marker("lifetime"))
		final := waitTaskExecution(task, corev1alpha1.TaskExecutionStateCancelled)
		Expect(final.Status.Execution.Reason).To(Equal(corev1alpha1.TaskExecutionReason("TaskTimeout")))
		expectServedOnce(marker("lifetime"))
		deleteE2ETasks(apiBaseURL, token, task)
		waitWorkspaceGone(backend, ws)
	})

	It("fails a workspace whose instance never starts and leaves nothing behind", func() {
		if backend.unbootable == nil {
			Skip(backend.name + " has no provider boot time limit")
		}
		class := name("wsp-unbootable")
		applyWorkspaceClass(backend, class, workspaceClassOptions{profile: backend.unbootable})
		task := name("wsp-unbootable")
		Expect(applyManifestJSON(wspTask(task, agent, class, "", false,
			"This instance never starts. "+marker("unbootable"), "15m", nil))).To(Succeed())
		ws := waitTaskWorkspace(task, class, backend)
		workspaceUIDs = append(workspaceUIDs, ws.uid)
		waitWorkspaceState(ws.name, workspacev1alpha1.ExecutionWorkspaceStateFailed)
		Eventually(func(g Gomega) {
			current := fetchTaskForACPAssertion(g, task)
			g.Expect(current.Status.Phase).To(BeElementOf(corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled))
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		requests, err := providerProxyMockLLM.requestsFor(marker("unbootable"))
		Expect(err).NotTo(HaveOccurred())
		Expect(requests).To(BeEmpty(), "a never-started instance must not reach the model")
		deleteE2ETasks(apiBaseURL, token, task)
		waitWorkspaceGone(backend, ws)
	})

	It("deletes every workspace and leaves no backend resources", teardown)
})

type workspaceIdentity struct{ name, uid, pool string }

func wspTask(name, agent, class, session string, create bool, prompt, timeout string,
	restore *workspacev1alpha1.ExecutionWorkspaceCheckpoint) map[string]any {
	var createRef *bool
	if session != "" {
		createRef = &create
	}
	manifest := runtimeAgentTaskManifest(name, agent, prompt, 2, nil, nil, session, createRef, nil)
	workspace := map[string]any{"classRef": map[string]any{"name": class}, "reusePolicy": "session"}
	if session == "" {
		workspace["reusePolicy"], workspace["onDetach"] = "none", "Delete"
	}
	if restore != nil {
		workspace["restoreFrom"] = map[string]any{"name": restore.Name, "uid": string(restore.UID), "digest": restore.Status.Digest}
	}
	spec := manifest["spec"].(map[string]any)
	spec["timeout"], spec["execution"] = timeout, map[string]any{"workspace": workspace}
	return manifest
}

// workspaceProviderSource returns the orka-workspace checkout to build from.
func workspaceProviderSource() string {
	if source := os.Getenv(workspaceSourceEnv); source != "" {
		return source
	}
	dir, err := os.MkdirTemp("", "orka-e2e-workspace-source-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, dir)
	Expect(fetchPinnedOrkaWorkspaceSource(dir)).To(Succeed())
	return dir
}

// installWorkspaceProvider builds the provider from orka-workspace source and
// applies its bundled registration, RBAC, and Deployment.
func installWorkspaceProvider(backend workspaceBackend, source string) []workspacev1alpha1.ExecutionWorkspaceFeature {
	image := "orka-workspace-" + backend.name + ":e2e"
	_, err := utils.Run(exec.Command("docker", "build", "-t", image,
		"-f", filepath.Join(source, "providers", backend.name, "Dockerfile"), source))
	Expect(err).NotTo(HaveOccurred())
	ref, err := pushImageToLocalRegistry(e2eRegistryAddress, image, "orka/workspace-provider-"+backend.name+":e2e")
	Expect(err).NotTo(HaveOccurred())

	_, err = utils.Run(exec.Command("kubectl", "apply", "--server-side", "-k", filepath.Join(source, "config", "policy")))
	Expect(err).NotTo(HaveOccurred())
	rendered, err := utils.Run(exec.Command("kubectl", "kustomize", filepath.Join(source, "providers", backend.name, "config")))
	Expect(err).NotTo(HaveOccurred())
	var objects []map[string]any
	decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered), 4096)
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			break
		} else {
			Expect(err).NotTo(HaveOccurred())
		}
		if object != nil {
			objects = append(objects, object)
		}
	}
	// Provider CRDs must be established before their custom resources apply.
	var crds, items []any
	for _, item := range objects {
		switch item["kind"] {
		case "CustomResourceDefinition":
			crds = append(crds, item)
			continue
		case backend.profileKind:
			continue // the bundle's sample profile targets the default namespace
		case "Deployment":
			container := item["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			container["image"], container["imagePullPolicy"] = ref, "IfNotPresent"
			if backend.patchProvider != nil {
				backend.patchProvider(item)
			}
		}
		items = append(items, item)
	}
	Expect(applyManifestJSON(map[string]any{"apiVersion": "v1", "kind": "List", "items": crds})).To(Succeed())
	for _, crd := range crds {
		_, err := utils.Run(exec.Command("kubectl", "wait", "--for=condition=Established", "--timeout=90s",
			"crd/"+crd.(map[string]any)["metadata"].(map[string]any)["name"].(string)))
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(applyManifestJSON(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})).To(Succeed())
	_, err = utils.Run(exec.Command("kubectl", "-n", wspProviderNS, "rollout", "status",
		"deployment/"+backend.deployment, "--timeout=5m"))
	Expect(err).NotTo(HaveOccurred())
	var provider workspacev1alpha1.ExecutionWorkspaceProvider
	Eventually(func(g Gomega) {
		kubectlJSON(g, &provider, "get", "executionworkspaceprovider", backend.name)
		g.Expect(provider.Status.SupportedContracts).To(ContainElement("orka.workspace.lifecycle.v1"))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
	return provider.Status.SupportedFeatures
}

type workspaceClassOptions struct {
	suspend     bool
	maxLifetime string         // default 2h
	profile     map[string]any // default backend.profileSpec(suspend)
}

func applyWorkspaceClass(backend workspaceBackend, name string, options workspaceClassOptions) {
	suspend, maxLifetime, profile := options.suspend, options.maxLifetime, options.profile
	if maxLifetime == "" {
		maxLifetime = "2h"
	}
	if profile == nil {
		profile = backend.profileSpec(suspend)
	}
	lifecycle := map[string]any{
		"defaultOnDetach": "Delete", "allowedOnDetach": []string{"Delete"},
		"detachTimeout": "5m", "maxLifetime": maxLifetime,
		"deletionPolicy": map[string]any{"providerResources": "Delete", "persistentVolumes": "Delete", "checkpoints": "Delete"},
	}
	reuse := []string{"None"}
	if suspend {
		lifecycle["defaultOnDetach"], lifecycle["allowedOnDetach"] = "Suspend", []string{"Suspend", "Delete"}
		reuse = []string{"None", "Session"}
	}
	Expect(applyManifestJSON(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{
		map[string]any{
			"apiVersion": backend.profileGroup + "/v1alpha1", "kind": backend.profileKind,
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"spec":     profile,
		},
		map[string]any{
			"apiVersion": "workspace.orka.ai/v1alpha1", "kind": "ExecutionWorkspaceClass",
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"spec": map[string]any{
				"providerRef":        map[string]any{"name": backend.name},
				"parametersRef":      map[string]any{"group": backend.profileGroup, "kind": backend.profileKind, "name": name},
				"mode":               "Interactive",
				"requiredFeatures":   []string{"acp.runtime.v2"},
				"allowedReuseScopes": reuse,
				"lifecycle":          lifecycle,
			},
		},
	}})).To(Succeed())
	Eventually(func(g Gomega) {
		var class workspacev1alpha1.ExecutionWorkspaceClass
		kubectlJSON(g, &class, "get", "executionworkspaceclass", name, "-n", namespace)
		ready := false
		for _, condition := range class.Status.Conditions {
			ready = ready || condition.Type == "Ready" && condition.Status == "True"
		}
		g.Expect(ready).To(BeTrue(), "class %s is not Ready: %+v", name, class.Status.Conditions)
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

// waitTaskWorkspace returns the workspace a Task bound, after checking that
// the provider-neutral Task projection names the class and nothing native.
func waitTaskWorkspace(task, class string, backend workspaceBackend) workspaceIdentity {
	var ws workspaceIdentity
	Eventually(func(g Gomega) {
		current := fetchTaskForACPAssertion(g, task)
		projection := current.Status.ExecutionWorkspace
		g.Expect(projection).NotTo(BeNil())
		g.Expect(projection.ClassRef).NotTo(BeNil())
		g.Expect(projection.ClassRef.Name).To(Equal(class))
		g.Expect(projection.WorkspaceRef).NotTo(BeNil())
		g.Expect(projection.Placement).To(BeNil(), "Task workspace status must stay provider-neutral")
		g.Expect(current.Status.Execution).NotTo(BeNil())
		g.Expect(current.Status.Execution.RuntimePoolName).To(HavePrefix("acp-ws-"))
		var workspace workspacev1alpha1.ExecutionWorkspace
		kubectlJSON(g, &workspace, "get", "executionworkspace", projection.WorkspaceRef.Name, "-n", namespace)
		g.Expect(string(workspace.UID)).To(Equal(projection.WorkspaceRef.UID))
		g.Expect(workspace.Spec.ProviderBinding.Name).To(Equal(backend.name))
		ws = workspaceIdentity{name: workspace.Name, uid: string(workspace.UID), pool: current.Status.Execution.RuntimePoolName}
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	return ws
}

func waitTaskExecution(task string, states ...corev1alpha1.TaskExecutionState) corev1alpha1.Task {
	var current corev1alpha1.Task
	EventuallyWithOffset(1, func(g Gomega) {
		current = fetchTaskForACPAssertion(g, task)
		g.Expect(current.Status.Execution).NotTo(BeNil())
		g.Expect(current.Status.Execution.State).To(BeElementOf(states), "Task %s execution", task)
	}, 15*time.Minute, 2*time.Second).Should(Succeed())
	return current
}

// waitHeldPrompt waits until aimock holds the Task's prompt in flight.
func waitHeldPrompt(task, promptMarker string) {
	waitTaskExecution(task, corev1alpha1.TaskExecutionStateRunning)
	EventuallyWithOffset(1, func(g Gomega) {
		requests, err := providerProxyMockLLM.requestsFor(promptMarker)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requests).NotTo(BeEmpty(), "aimock has not received %s", promptMarker)
	}, 10*time.Minute, 2*time.Second).Should(Succeed())
}

// expectServedOnce checks that a held prompt reached the model exactly once.
func expectServedOnce(promptMarker string) {
	requests, err := providerProxyMockLLM.requestsFor(promptMarker)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	if len(requests) != 1 {
		var times []int64
		for _, request := range requests {
			times = append(times, request.Timestamp)
		}
		Fail(fmt.Sprintf("%s reached the model %d times, at %v; it must not be replayed", promptMarker, len(requests), times), 1)
	}
}

// expectNoReplayAfterSettle checks that no new request for a settled prompt
// reaches the model.
func expectNoReplayAfterSettle(promptMarker string) {
	settled, err := providerProxyMockLLM.requestsFor(promptMarker)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ConsistentlyWithOffset(1, func(g Gomega) {
		requests, err := providerProxyMockLLM.requestsFor(promptMarker)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requests).To(HaveLen(len(settled)), "%s was replayed after its Task settled", promptMarker)
	}, 30*time.Second, 3*time.Second).Should(Succeed())
}

func waitWorkspaceState(name string, state workspacev1alpha1.ExecutionWorkspaceState) {
	EventuallyWithOffset(1, func(g Gomega) {
		var workspace workspacev1alpha1.ExecutionWorkspace
		kubectlJSON(g, &workspace, "get", "executionworkspace", name, "-n", namespace)
		g.Expect(workspace.Status.State).To(Equal(state))
	}, 10*time.Minute, 2*time.Second).Should(Succeed())
}

func waitWorkspaceGone(backend workspaceBackend, ws workspaceIdentity) {
	EventuallyWithOffset(1, func(g Gomega) {
		for _, kind := range []string{"executionworkspace/" + ws.name, "runtimepool/" + ws.pool} {
			output, err := utils.Run(exec.Command("kubectl", "-n", namespace, "get", kind, "--ignore-not-found", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "%s must be deleted", kind)
		}
		g.Expect(backend.leftovers(g, ws.uid)).To(BeEmpty())
	}, 10*time.Minute, 5*time.Second).Should(Succeed())
}

func exportCheckpoint(name string, ws workspaceIdentity, recover bool) workspacev1alpha1.ExecutionWorkspaceCheckpoint {
	spec := map[string]any{"workspaceRef": map[string]any{"name": ws.name, "uid": ws.uid}}
	if recover {
		spec["recoverLastCheckpoint"] = true
	}
	ExpectWithOffset(1, applyManifestJSON(map[string]any{
		"apiVersion": "workspace.orka.ai/v1alpha1", "kind": "ExecutionWorkspaceCheckpoint",
		"metadata": map[string]any{"name": name, "namespace": namespace}, "spec": spec,
	})).To(Succeed())
	var checkpoint workspacev1alpha1.ExecutionWorkspaceCheckpoint
	EventuallyWithOffset(1, func(g Gomega) {
		kubectlJSON(g, &checkpoint, "get", "executionworkspacecheckpoint", name, "-n", namespace)
		g.Expect(checkpoint.Status.Phase).To(Equal("Ready"))
		g.Expect(checkpoint.Status.Digest).To(MatchRegexp(`^sha256:[a-f0-9]{64}$`))
	}, 10*time.Minute, 2*time.Second).Should(Succeed())
	return checkpoint
}

// deleteE2ETasks finalizes Tasks through the suite's exact cleanup path.
func deleteE2ETasks(apiBaseURL, token string, tasks ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cleanup, err := newE2ECleanup(apiBaseURL, token)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, cleanup.tasks(ctx, tasks, false)).To(Succeed())
}

// releaseHeldTaskObserverFinalizer removes the test observer finalizer if the
// exact Task still holds it.
func releaseHeldTaskObserverFinalizer(name, uid string) {
	output, err := utils.Run(exec.Command("kubectl", "get", "task", name, "-n", namespace,
		"--ignore-not-found", "-o", "json"))
	Expect(err).NotTo(HaveOccurred())
	if strings.TrimSpace(output) == "" {
		return
	}
	var task corev1alpha1.Task
	Expect(json.Unmarshal([]byte(output), &task)).To(Succeed())
	index := slices.Index(task.Finalizers, agentRuntimeObserverFinalizer)
	if string(task.UID) != uid || index < 0 {
		return
	}
	patch := fmt.Sprintf(`[{"op":"test","path":"/metadata/uid","value":%q},`+
		`{"op":"test","path":"/metadata/finalizers/%d","value":%q},{"op":"remove","path":"/metadata/finalizers/%d"}]`,
		uid, index, agentRuntimeObserverFinalizer, index)
	_, err = utils.Run(exec.Command("kubectl", "patch", "task", name, "-n", namespace, "--type=json", "-p", patch))
	Expect(err).NotTo(HaveOccurred())
}

func deleteSessionViaAPI(apiBaseURL, token, session string) {
	url := fmt.Sprintf("%s/api/v1/sessions/%s?namespace=%s", strings.TrimRight(apiBaseURL, "/"), session, namespace)
	EventuallyWithOffset(1, func(g Gomega) {
		body, status, err := doAuthorizedJSONRequest(http.MethodDelete, url, token, "", "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(status).To(BeElementOf(http.StatusNoContent, http.StatusNotFound), body)
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
}

func dumpWorkspaceProviderDiagnostics(backend workspaceBackend) {
	for _, args := range [][]string{
		{"-n", namespace, "get", "tasks,runtimepools,executionworkspaces,executionworkspacecheckpoints", "-o", "yaml"},
		{"-n", wspProviderNS, "logs", "deployment/" + backend.deployment, "--tail=200"},
		{"-n", namespace, "logs", "deployment/" + publisherDeployment, "--tail=100"},
		{"-n", namespace, "logs", "deployment/orka-controller-manager", "-c", "manager", "--tail=200"},
	} {
		output, _ := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n", strings.Join(args, " "), output)
	}
}
