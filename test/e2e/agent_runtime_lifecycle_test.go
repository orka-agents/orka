//go:build e2e
// +build e2e

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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/test/utils"
)

// The Agent Runtime Lifecycle specs port scripts/agent-runtime-e2e.sh: the
// built-in Codex, OpenCode, Claude, and Copilot runtimes on harness v2
// RuntimePools behind the production provider proxy. With
// E2E_LIVE_COPILOT_PROXY_BASE_URL set they use the live Vekil in vekil-system;
// otherwise aimock stands in for Vekil and replays
// agent_runtime_lifecycle.json. Mock mode adds journal checks the bash lane
// cannot make: that each continuation, fork, and recovery replayed the prior
// transcript to the model, and that each read reached the model as a tool
// result. They run in their own CI job (label agent-runtime) because they
// restart the controller and park, replace, and scale RuntimePools.
const (
	agentRuntimeRepoURL      = "https://github.com/orka-agents/orka.git"
	agentRuntimeRepoRef      = "d03acb995b6014a6e855181c50b922b65ea8e7ff"
	agentRuntimeRepoIdentity = "github.com/orka-agents/orka"
	agentRuntimeLicenseLine  = "MIT License"
	agentRuntimeMaxTurns     = 24
	agentRuntimeConcurrency  = 6
	agentRuntimeSettleBound  = 120 * time.Second
	// agentRuntimeControllerDeployment is the Deployment make deploy installs.
	agentRuntimeControllerDeployment = "orka-controller-manager"

	// Prompts from scripts/agent-runtime-e2e.sh. Each spec appends a marker so
	// aimock can script it.
	agentRuntimeBlockingPrompt = "Immediately use the Bash tool to run sleep 180 with a command timeout of at " +
		"least 240 seconds (timeout_ms 240000). Do not return a final answer before the command completes. " +
		"Do not modify repository files."
	agentRuntimeLongPrompt = "Read LICENSE, NOTICE.md, go.mod, Makefile, and the first ten Go source files you " +
		"find. Compare their purpose and provide a detailed evidence-backed summary. Do not modify files."
)

// agentRuntimeResultDiagnostics are CLI warnings that must not leak into a
// Task result.
var agentRuntimeResultDiagnostics = regexp.MustCompile(
	`Info: (Disabled tools|Unknown tool name in the tool excludedlist)|Warning: Falling back from WebSockets`)

type agentRuntimeProvider struct {
	kind, model string
	// mockNonce is the session nonce in mock mode, where the fixtures echo it.
	mockNonce string
}

var _ = Describe("Agent Runtime Lifecycle", Ordered, Label("agent-runtime"), func() {
	var (
		live       bool
		runID      string
		apiBaseURL string
		token      string
		cancelPF   context.CancelFunc
		pfCmd      *exec.Cmd

		codex    = agentRuntimeProvider{kind: "codex", model: "gpt-5.4-mini", mockNonce: "nonce-art-codex"}
		opencode = agentRuntimeProvider{kind: "opencode", model: "openai/gpt-5.4-mini", mockNonce: "nonce-art-opencode"}
		claude   = agentRuntimeProvider{kind: "claude", model: "claude-haiku-4.5", mockNonce: "nonce-art-claude"}
		copilot  = agentRuntimeProvider{kind: "copilot", model: "gpt-5.3-codex", mockNonce: "nonce-art-copilot"}

		opencodeLimits  = &corev1alpha1.ModelTokenLimits{Context: 32768, Output: 4096}
		timeoutDuration = 90 * time.Second

		// State the Codex checks hand to each other, as the bash globals do.
		codexAgent, codexToolAgent, codexSession, codexPool, codexNonce string
		toolProfilePool                                                 string
		readSmokeSessionUID                                             string
		readSmokeSessionGeneration                                      int64
		replacementSessionUID                                           string
		replacementSessionGeneration                                    int64
	)

	name := func(prefix string) string {
		value := strings.ToLower(prefix + "-" + runID)
		if len(value) > 63 {
			value = strings.TrimRight(value[:63], "-")
		}
		return value
	}
	nonceFor := func(provider agentRuntimeProvider) string {
		if live {
			return fmt.Sprintf("%s-%s-%d", provider.kind, runID, time.Now().UnixNano()%100000)
		}
		return provider.mockNonce
	}
	marker := func(check string) string { return "[e2e:art-" + check + "]" }

	restartPortForward := func() {
		stopPortForward(cancelPF, pfCmd)
		var err error
		apiBaseURL, cancelPF, pfCmd, err = startControllerAPIPortForward(18094)
		Expect(err).NotTo(HaveOccurred())
	}

	applyReadTask := func(task agentRuntimeReadTask) {
		task.RepoURL, task.Ref, task.RepoIdentity = agentRuntimeRepoURL, agentRuntimeRepoRef, agentRuntimeRepoIdentity
		if task.MaxTurns == 0 {
			task.MaxTurns = agentRuntimeMaxTurns
		}
		if task.Timeout == "" {
			task.Timeout = "12m"
		}
		ExpectWithOffset(1, applyManifestJSON(task.manifest(runID))).To(Succeed())
	}

	// expectTaskResult ports the release-mode result checks: the expected
	// text in GET /api/v1/tasks/{task}/result and no CLI diagnostics.
	expectTaskResult := func(task string, expected ...string) {
		result := fetchTaskResultViaAPI(apiBaseURL, token, task)
		for _, text := range expected {
			ExpectWithOffset(1, result).To(ContainSubstring(text), "result of %s", task)
		}
		ExpectWithOffset(1, agentRuntimeResultDiagnostics.MatchString(result)).To(BeFalse(),
			"result of %s leaked a CLI diagnostic: %q", task, result)
	}

	// expectReplayed checks in mock mode that the requests aimock answered for
	// promptMarker, whose own text lacks text, still carried it to the model,
	// so the runtime replayed the prior transcript. aimock does not record
	// bodies over its 64 KB journal cap (Copilot's are), and for those the
	// RuntimeSession identity checks carry the proof, as in the bash lane.
	expectReplayed := func(promptMarker, text string) {
		if live {
			return
		}
		EventuallyWithOffset(1, func(g Gomega) {
			served, err := providerProxyMockLLM.servedFor(promptMarker)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(served).NotTo(BeEmpty(), "aimock answered no request for %s", promptMarker)
			recorded := 0
			replayed := false
			for _, request := range served {
				if request.Body.Truncated {
					continue
				}
				recorded++
				if request.Mentions(text) {
					replayed = true
				}
			}
			if recorded == 0 {
				_, _ = fmt.Fprintf(GinkgoWriter, "%s: request bodies exceed the aimock journal cap; "+
					"relying on RuntimeSession identity for the replay check\n", promptMarker)
				return
			}
			g.Expect(replayed).To(BeTrue(), "%s reached the model without %q from the prior transcript", promptMarker, text)
		}, time.Minute, 2*time.Second).Should(Succeed())
	}

	// expectReadReachedModel checks in mock mode that aimock answered the
	// prompt with the fixture that requires a tool result containing the
	// LICENSE line, so the file content came back to the model whatever the
	// API shape or body size.
	expectReadReachedModel := func(promptMarker string) {
		if live {
			return
		}
		served, err := providerProxyMockLLM.servedFor(promptMarker)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		found := false
		for _, request := range served {
			var fixture struct {
				Match struct {
					ToolResultContains string `json:"toolResultContains"`
				} `json:"match"`
			}
			if json.Unmarshal(request.Response.Fixture, &fixture) == nil &&
				fixture.Match.ToolResultContains == agentRuntimeLicenseLine {
				found = true
			}
		}
		ExpectWithOffset(1, found).To(BeTrue(), "LICENSE never reached the model for %s", promptMarker)
	}

	// readSmoke ports run_read_smoke for one provider: a read that starts a
	// Session, a continuation preserving its identity (cold after recorded retirement),
	// and a fork of the continuation. It returns the Agent, Session, pool, nonce,
	// and the continued RuntimeSession identity.
	readSmoke := func(provider agentRuntimeProvider) (string, string, string, string, string, int64) {
		agent := name("acp-" + provider.kind)
		task := name("acp-" + provider.kind + "-read")
		continuation := name("acp-" + provider.kind + "-cont")
		fork := name("acp-" + provider.kind + "-fork")
		session := name("acp-" + provider.kind + "-session")
		nonce := nonceFor(provider)
		Expect(applyManifestJSON(agentRuntimeAgentManifest(agent, provider.kind, provider.model, runID,
			agentRuntimeMaxTurns, false, opencodeLimits))).To(Succeed())

		By(fmt.Sprintf("reading LICENSE with %s and starting Session %s", provider.kind, session))
		applyReadTask(agentRuntimeReadTask{Name: task, Agent: agent, Provider: provider.kind, Session: session,
			CreateSession: true, Prompt: fmt.Sprintf("Read LICENSE. Remember the session nonce %s. Report the nonce "+
				"and the first non-empty LICENSE line exactly. Do not modify any file. %s", nonce, marker(provider.kind+"-read"))})
		pool := waitTaskPoolName(task)
		snapshot := captureRuntimePoolSnapshot(pool, provider.kind, provider.model, corev1alpha1.WorkspaceIntentRead)
		assertTaskSucceededRead(task, snapshot)
		expectTaskResult(task, nonce, agentRuntimeLicenseLine)
		expectReadReachedModel(marker(provider.kind + "-read"))
		firstUID, firstGeneration := taskSessionIdentity(Default, task)
		firstExecution := getACPTask(Default, task).Status.Execution
		expectedGeneration := firstGeneration
		if firstExecution.RuntimeSessionRecreationPending {
			// Checkpoint settlement retires the writer even when the first native
			// capture is unsupported. Continue the same logical Session at its
			// recorded next generation, without assuming portable native state.
			expectedGeneration++
		}

		By("continuing the same Session on its RuntimePool with the expected runtime generation")
		applyReadTask(agentRuntimeReadTask{Name: continuation, Agent: agent, Provider: provider.kind, Session: session,
			Prompt: "Continue the existing session. Return the session nonce from the prior turn and no " +
				"repository-derived replacement. Do not modify files. " + marker(provider.kind+"-continue")})
		Expect(waitTaskPoolName(continuation)).To(Equal(pool), "the continuation selected a different RuntimePool")
		snapshot = captureRuntimePoolSnapshot(pool, provider.kind, provider.model, corev1alpha1.WorkspaceIntentRead)
		assertTaskSucceededRead(continuation, snapshot)
		expectTaskResult(continuation, nonce)
		expectReplayed(marker(provider.kind+"-continue"), nonce)
		continuedUID, continuedGeneration := taskSessionIdentity(Default, continuation)
		Expect(continuedUID).To(Equal(firstUID))
		Expect(continuedGeneration).To(Equal(expectedGeneration), "continuation must honor the prior turn's runtime retirement fence")

		By("forking the continuation through the API")
		body, status, err := doAuthorizedJSONRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/tasks/%s/fork?namespace=%s", strings.TrimRight(apiBaseURL, "/"), continuation, namespace),
			token, fmt.Sprintf(`{"newTaskName":%q,"prompt":%q}`, fork,
				"Return the session nonce represented in the source task timeline. Do not modify files. "+
					marker(provider.kind+"-fork")), "")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(BeElementOf(http.StatusOK, http.StatusCreated, http.StatusAccepted), body)
		var forked struct {
			Namespace      string `json:"namespace"`
			SourceTaskName string `json:"sourceTaskName"`
			NewTaskName    string `json:"newTaskName"`
			AfterSeq       int64  `json:"afterSeq"`
		}
		Expect(json.Unmarshal([]byte(body), &forked)).To(Succeed())
		Expect(forked.Namespace).To(Equal(namespace))
		Expect(forked.SourceTaskName).To(Equal(continuation))
		Expect(forked.NewTaskName).To(Equal(fork))
		Expect(forked.AfterSeq).To(BeNumerically(">=", 1))
		_, err = utils.Run(exec.Command("kubectl", "label", "task", fork, "-n", namespace,
			agentRuntimeRunLabel+"="+runID, "--overwrite"))
		Expect(err).NotTo(HaveOccurred())
		Expect(waitTaskPoolName(fork)).To(Equal(pool))
		snapshot = captureRuntimePoolSnapshot(pool, provider.kind, provider.model, corev1alpha1.WorkspaceIntentRead)
		forkTask := assertTaskSucceededRead(fork, snapshot)
		Expect(forkTask.Annotations).To(HaveKeyWithValue("orka.ai/fork-source-task", continuation))
		Expect(forkTask.Annotations).To(HaveKeyWithValue("orka.ai/fork-source-seq", fmt.Sprint(forked.AfterSeq)))
		expectTaskResult(fork, nonce)
		expectReplayed(marker(provider.kind+"-fork"), nonce)
		return agent, session, pool, nonce, continuedUID, continuedGeneration
	}

	// removeProviderResources ports remove_provider_resources: delete the run's
	// Tasks and archive their Sessions, stop and delete the provider's
	// RuntimePools once nothing references them, delete the Agents, and
	// release BranchClaims the run owned.
	removeProviderResources := func(provider string, agents ...string) {
		var tasks corev1alpha1.TaskList
		kubectlJSON(Default, &tasks, "get", "tasks", "-n", namespace, "-l", agentRuntimeRunLabel+"="+runID)
		owners := map[string]bool{}
		sessions := map[string]bool{}
		poolUIDs := map[string]bool{}
		for _, task := range tasks.Items {
			owners[string(task.UID)] = true
			if task.Status.Execution != nil {
				owners[task.Status.Execution.RuntimeSessionUID] = true
				poolUIDs[task.Status.Execution.RuntimePoolUID] = true
			}
			if task.Spec.SessionRef != nil {
				sessions[task.Spec.SessionRef.Name] = true
			}
			if task.DeletionTimestamp == nil {
				_, err := utils.Run(exec.Command("kubectl", "delete", "task", task.Name, "-n", namespace, "--wait=false"))
				Expect(err).NotTo(HaveOccurred())
			}
		}
		for session := range sessions {
			archiveSessionViaAPI(apiBaseURL, token, session, runID)
		}
		for _, task := range tasks.Items {
			waitTaskAbsent(task.Name)
		}

		var pools corev1alpha1.RuntimePoolList
		kubectlJSON(Default, &pools, "get", "runtimepools", "-n", namespace)
		var stopped []corev1alpha1.RuntimePool
		for _, pool := range pools.Items {
			if pool.Spec.Runtime.Profile.ProviderKind != provider || pool.Spec.TrustDomain.Namespace != namespace ||
				!poolUIDs[string(pool.UID)] {
				continue
			}
			var remaining corev1alpha1.TaskList
			kubectlJSON(Default, &remaining, "get", "tasks", "-n", namespace)
			for _, task := range remaining.Items {
				if task.Status.Execution != nil {
					Expect(task.Status.Execution.RuntimePoolUID).NotTo(Equal(string(pool.UID)),
						"Task %s still references RuntimePool %s", task.Name, pool.Name)
				}
			}
			_, err := utils.Run(exec.Command("kubectl", "patch", "runtimepool", pool.Name, "-n", namespace,
				"--type=json", "-p", fmt.Sprintf(`[{"op":"test","path":"/metadata/uid","value":%q},`+
					`{"op":"add","path":"/spec/desiredReplicas","value":0}]`, pool.UID)))
			Expect(err).NotTo(HaveOccurred())
			waitRuntimePoolStopped(pool.Name)
			stopped = append(stopped, pool)
		}
		for _, agent := range agents {
			_, err := utils.Run(exec.Command("kubectl", "delete", "agent", agent, "-n", namespace,
				"--ignore-not-found", "--wait=true", "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
		}
		for _, pool := range stopped {
			_, err := utils.Run(exec.Command("kubectl", "delete", "runtimepool", pool.Name, "-n", namespace,
				"--ignore-not-found", "--wait=true", "--timeout=5m"))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				output, err := utils.Run(exec.Command("kubectl", "get",
					"deployment,replicaset,pod,service,secret,configmap,serviceaccount,role,rolebinding,"+
						"networkpolicy,poddisruptionbudget,persistentvolumeclaim",
					"-n", runtimePoolNamespace(pool), "-l",
					fmt.Sprintf("orka.ai/runtime-pool-namespace=%s,orka.ai/runtime-pool-name=%s", namespace, pool.Name),
					"-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "RuntimePool %s left runtime children", pool.Name)
			}, agentRuntimeStateWait, 2*time.Second).Should(Succeed())
		}

		var claims corev1alpha1.BranchClaimList
		kubectlJSON(Default, &claims, "get", "branchclaims")
		for _, claim := range claims.Items {
			if owners[claim.Spec.OwnerUID] {
				_, err := utils.Run(exec.Command("kubectl", "delete", "branchclaim", claim.Name,
					"--wait=true", "--timeout=2m"))
				Expect(err).NotTo(HaveOccurred())
			}
		}
	}

	BeforeAll(func() {
		live = strings.TrimSpace(e2eLiveCopilotProxyBaseURL) != ""
		if !live && e2eFlagEnabled(mockLLMDisableEnvVar) {
			Skip("Skipping: E2E_LIVE_COPILOT_PROXY_BASE_URL not set and " + mockLLMDisableEnvVar + " is set")
		}
		// The park and resume checks assume the production idle TTL: a
		// shorter one scales a resumed pool back to zero before demand returns.
		if e2eACPIdlePoolTTL < 10*time.Minute {
			Skip(fmt.Sprintf("Skipping: needs E2E_ACP_IDLE_POOL_TTL of at least 10m (got %s); "+
				"run with -ginkgo.label-filter=agent-runtime E2E_ACP_IDLE_POOL_TTL=15m", e2eACPIdlePoolTTL))
		}
		runID = fmt.Sprintf("art%d", time.Now().Unix()%100000000)

		var err error
		apiBaseURL, cancelPF, pfCmd, err = startControllerAPIPortForward(18094)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { stopPortForward(cancelPF, pfCmd) })
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		if !live {
			// Mock mode holds blocking prompts in the model call, so a shorter
			// Task timeout still proves the deadline.
			timeoutDuration = 30 * time.Second
			projectDir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(providerProxyMockLLM.delete)
			providerProxyMockLLM.deploy(projectDir)
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() && !live {
			requests, err := providerProxyMockLLM.journal()
			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "provider proxy aimock journal unavailable: %v\n", err)
				return
			}
			since := CurrentSpecReport().StartTime.UnixMilli()
			for _, request := range requests {
				if request.Timestamp < since {
					continue
				}
				var last mockLLMMessage
				if n := len(request.Body.Messages); n > 0 {
					last = request.Body.Messages[n-1]
				}
				text := last.Text()
				if len(text) > 200 {
					text = text[:200] + "..."
				}
				_, _ = fmt.Fprintf(GinkgoWriter, "vekil-aimock %s %d last[%s]=%q\n", request.Path,
					request.Response.Status, last.Role, text)
			}
		}
	})

	It("runs the publisher with brokered authority only, and the controller and publisher on persistent storage", func() {
		expectPublisherBrokeredAuthority()
		expectDeploymentPersistent(agentRuntimeControllerDeployment, "manager", "controller")
		expectDeploymentPersistent(publisherDeployment, "publisher", "workspace-publisher")
	})

	It("runs a Codex read, continues its Session, and forks the continuation", func() {
		codexAgent, codexSession, codexPool, codexNonce, readSmokeSessionUID, readSmokeSessionGeneration = readSmoke(codex)
	})

	It("rejects unsafe workspace URLs at admission and before RuntimePool demand", func() {
		const message = "gitRepo must not contain embedded credentials, query parameters, or fragments"
		const publicationMessage = "publicationGitRepo must not contain embedded credentials, query parameters, or fragments"
		dryRun := func(manifest map[string]any) (string, error) {
			payload, err := json.Marshal(manifest)
			Expect(err).NotTo(HaveOccurred())
			cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
			cmd.Stdin = strings.NewReader(string(payload))
			return utils.Run(cmd)
		}
		readManifest := func(taskName, repo string) map[string]any {
			return map[string]any{"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
				"metadata": map[string]any{"name": taskName, "namespace": namespace},
				"spec": map[string]any{"type": "agent", "agentRef": map[string]any{"name": codexAgent},
					"prompt":    "dry-run URL validation",
					"workspace": map[string]any{"intent": "read", "gitRepo": repo, "ref": agentRuntimeRepoRef}}}
		}
		writeManifest := func(taskName, publication string) map[string]any {
			return map[string]any{"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
				"metadata": map[string]any{"name": taskName, "namespace": namespace},
				"spec": map[string]any{"type": "agent", "agentRef": map[string]any{"name": codexAgent},
					"prompt": "dry-run publication URL validation",
					"workspace": map[string]any{"intent": "write", "gitRepo": agentRuntimeRepoURL,
						"ref": agentRuntimeRepoRef, "publicationGitRepo": publication,
						"publicationCredentialRef": map[string]any{"name": "dry-run-credential"},
						"pushBranch":               "dry-run", "prBaseBranch": "main", "createPR": false}}}
		}
		expectRejected := func(manifest map[string]any, expected string) {
			output, err := dryRun(manifest)
			ExpectWithOffset(1, err).To(HaveOccurred(), "admission accepted %v", manifest["spec"])
			ExpectWithOffset(1, output).To(ContainSubstring("spec.workspace"))
			ExpectWithOffset(1, output).To(ContainSubstring(expected))
		}

		_, err := dryRun(readManifest(name("acp-safe-read"), agentRuntimeRepoURL))
		Expect(err).NotTo(HaveOccurred(), "the safe read workspace must be admitted")
		for check, repo := range map[string]string{
			"query":    agentRuntimeRepoURL + "?unexpected=query",
			"fragment": agentRuntimeRepoURL + "#fragment",
			"user":     "https://user@github.com/orka-agents/orka.git",
			"ssh":      "ssh://git@github.com/orka-agents/orka.git",
		} {
			expectRejected(readManifest(name("acp-unsafe-"+check), repo), message)
		}

		const publication = "https://github.com/orka-fixtures/publication.git"
		_, err = dryRun(writeManifest(name("acp-safe-write"), publication))
		Expect(err).NotTo(HaveOccurred(), "the safe write workspace must be admitted")
		for check, repo := range map[string]string{
			"pub-query":    publication + "?unexpected=query",
			"pub-fragment": publication + "#fragment",
			"pub-user":     "https://user@github.com/orka-fixtures/publication.git",
		} {
			expectRejected(writeManifest(name("acp-unsafe-"+check), repo), publicationMessage)
		}

		By("failing an ext:: transport in the controller before any RuntimePool demand")
		helper := name("acp-unsafe-helper")
		Expect(applyManifestJSON(map[string]any{"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
			"metadata": map[string]any{"name": helper, "namespace": namespace,
				"labels": map[string]any{agentRuntimeRunLabel: runID}},
			"spec": map[string]any{"type": "agent", "agentRef": map[string]any{"name": codexAgent},
				"prompt": "This request must be rejected before RuntimePool demand or prompt submission.",
				"workspace": map[string]any{"intent": "read", "ref": agentRuntimeRepoRef,
					"gitRepo": "ext::https://github.com/orka-agents/orka.git"},
				"timeout": "2m"}})).To(Succeed())
		task := waitTaskTerminalProjection(helper)
		Expect(task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
		Expect(task.Status.Execution.State).To(Equal(corev1alpha1.TaskExecutionStateFailed))
		Expect(task.Status.Execution.Outcome).To(Equal(corev1alpha1.TaskExecutionOutcomeFailed))
		Expect(task.Status.Execution.Reason).To(Equal(corev1alpha1.TaskExecutionReason("InvalidWorkspace")))
		Expect(task.Status.Delivery).NotTo(BeNil())
		Expect(task.Status.Delivery.State).To(Equal(corev1alpha1.TaskDeliveryStateNotRequested))
		Expect(task.Status.Delivery.Outcome).To(Equal(corev1alpha1.TaskDeliveryOutcomeNotRequested))
		Expect(task.Status.Execution.RuntimePoolName).To(BeEmpty())
		Expect(task.Status.Execution.RuntimePoolUID).To(BeEmpty())
		Expect(task.Status.Execution.RuntimeInstanceID).To(BeEmpty())
		Expect(task.Status.Execution.PromptID).To(BeEmpty())
	})

	It("runs concurrent read prompts within RuntimePool capacity on distinct sessions", func() {
		snapshot := captureRuntimePoolSnapshot(codexPool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)
		tasks := make([]string, 0, agentRuntimeConcurrency)
		for i := 1; i <= agentRuntimeConcurrency; i++ {
			task := name(fmt.Sprintf("acp-concurrent-%d", i))
			tasks = append(tasks, task)
			applyReadTask(agentRuntimeReadTask{Name: task, Agent: codexAgent, Provider: codex.kind,
				Prompt: agentRuntimeLongPrompt + " " + marker("concurrent")})
		}

		var maxRunning, queued int32
		deadline := time.Now().Add(agentRuntimeWait)
		for {
			pool := getRuntimePool(Default, codexPool)
			maxRunning = max(maxRunning, pool.Status.Capacity.RunningPrompts)
			queued = max(queued, pool.Status.Capacity.QueuedTasks)
			terminal := 0
			for _, task := range tasks {
				current := getACPTask(Default, task)
				if current.Status.Execution == nil {
					continue
				}
				if assigned := current.Status.Execution.RuntimePoolName; assigned != "" {
					Expect(assigned).To(Equal(codexPool), "concurrent Task %s selected another RuntimePool", task)
				}
				for _, state := range terminalExecutionStates {
					if current.Status.Execution.State == state {
						terminal++
					}
				}
			}
			if terminal == len(tasks) {
				break
			}
			Expect(time.Now().Before(deadline)).To(BeTrue(), "concurrent Tasks did not all reach terminal states")
			time.Sleep(time.Second)
		}

		limit := getRuntimePool(Default, codexPool).Status.Capacity.MaxRunningPrompts
		Expect(maxRunning).To(BeNumerically("<=", limit))
		if agentRuntimeConcurrency > int(limit) {
			Expect(queued).To(BeNumerically(">=", 1), "the capacity burst never exposed durable queued demand")
		}
		Expect(maxRunning).To(BeNumerically(">=", 2), "prompts never ran in parallel")

		sessions := map[string]bool{}
		for _, task := range tasks {
			assertTaskSucceededRead(task, snapshot)
			uid, _ := taskSessionIdentity(Default, task)
			sessions[uid] = true
		}
		Expect(sessions).To(HaveLen(len(tasks)), "each concurrent Task needs its own RuntimeSession")
		// Codex native capture also retires the earlier read-smoke Session;
		// completed one-shot Tasks must not leave any runtime home resident.
		waitRuntimePoolCountersZero(codexPool, 0)
	})

	It("cancels a running prompt at its Task timeout", func() {
		parkRuntimePool(codexPool)
		codexToolAgent = name("acp-codex-tools")
		Expect(applyManifestJSON(agentRuntimeAgentManifest(codexToolAgent, codex.kind, codex.model, runID,
			12, true, nil))).To(Succeed())

		task := name("acp-timeout")
		started := time.Now()
		applyReadTask(agentRuntimeReadTask{Name: task, Agent: codexToolAgent, Provider: codex.kind, AllowBash: true,
			Timeout: fmt.Sprintf("%ds", int(timeoutDuration.Seconds())),
			Prompt:  agentRuntimeBlockingPrompt + " " + marker("timeout")})
		toolProfilePool = waitTaskPoolName(task)
		waitRuntimePoolServing(toolProfilePool)
		waitTaskRunningOnPool(task, toolProfilePool)
		snapshot := captureRuntimePoolSnapshot(toolProfilePool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)
		assertCancelledTask(task, snapshot, corev1alpha1.TaskExecutionReason("TaskTimeout"))
		// Kubernetes timestamps have whole-second precision, so the deadline
		// can land up to a second before timeoutDuration of wall-clock time.
		Expect(time.Since(started)).To(BeNumerically(">=", timeoutDuration-time.Second),
			"the prompt was cancelled before its Task timeout")
		waitRuntimePoolCountersZero(toolProfilePool, 0)
	})

	It("cancels a running prompt when its Task is deleted, before the cleanup finalizer clears", func() {
		task := name("acp-cancel")
		applyReadTask(agentRuntimeReadTask{Name: task, Agent: codexToolAgent, Provider: codex.kind, AllowBash: true,
			Timeout: "1h", Prompt: agentRuntimeBlockingPrompt + " " + marker("cancel")})
		Expect(waitTaskPoolName(task)).To(Equal(toolProfilePool), "the cancel Task left the tool-profile RuntimePool")
		waitRuntimePoolServing(toolProfilePool)
		waitTaskRunningOnPool(task, toolProfilePool)
		snapshot := captureRuntimePoolSnapshot(toolProfilePool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)
		taskRunningOnPool(Default, task, toolProfilePool)
		uid := string(getACPTask(Default, task).UID)
		Expect(uid).NotTo(BeEmpty())
		addTaskObserverFinalizer(task, uid)

		requested := time.Now()
		_, err := utils.Run(exec.Command("kubectl", "delete", "task", task, "-n", namespace, "--wait=false"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			current := getACPTask(g, task)
			g.Expect(current.Status.Execution).NotTo(BeNil())
			g.Expect(current.Status.Execution.State).To(BeElementOf(terminalExecutionStates))
		}, agentRuntimeSettleBound, 2*time.Second).Should(Succeed())
		Expect(time.Since(requested)).To(BeNumerically("<=", agentRuntimeSettleBound+2*time.Second))
		assertCancelledTask(task, snapshot, corev1alpha1.TaskExecutionReason("Cancelled"))
		waitTaskObserverReleaseReady(task, uid)
		releaseTaskObserverFinalizer(task, uid)
		waitTaskAbsent(task)
		waitRuntimePoolCountersZero(toolProfilePool, 0)
	})

	It("settles an in-flight prompt once across a controller restart and continues on a new runtime instance", func() {
		task := name("acp-restart")
		continuation := name("acp-post-restart")
		session := name("acp-restart-session")
		restartNonce := "restart-art"
		if live {
			restartNonce = fmt.Sprintf("restart-%s-%d", runID, time.Now().UnixNano()%100000)
		}
		applyReadTask(agentRuntimeReadTask{Name: task, Agent: codexToolAgent, Provider: codex.kind, AllowBash: true,
			Session: session, CreateSession: true, Timeout: "1h",
			Prompt: fmt.Sprintf("Remember restart nonce %s. %s %s", restartNonce, agentRuntimeBlockingPrompt,
				marker("restart"))})
		pool := waitTaskPoolName(task)
		Expect(pool).To(Equal(toolProfilePool))
		waitRuntimePoolServing(pool)
		waitTaskRunningOnPool(task, pool)
		oldSnapshot := captureRuntimePoolSnapshot(pool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)
		oldUID, oldGeneration := taskSessionIdentity(Default, task)
		taskRunningOnPool(Default, task, pool)

		By("restarting the controller while the prompt runs")
		stopPortForward(cancelPF, pfCmd)
		_, err := utils.Run(exec.Command("kubectl", "-n", namespace, "rollout", "restart",
			"deployment/"+agentRuntimeControllerDeployment))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "-n", namespace, "rollout", "status",
			"deployment/"+agentRuntimeControllerDeployment, "--timeout=10m"))
		Expect(err).NotTo(HaveOccurred())
		restartPortForward()

		assertRestartTaskSettled(task, oldSnapshot)
		resumeRuntimePool(pool)
		waitRuntimePoolInstanceReplaced(pool, oldSnapshot.RuntimeInstanceID)
		newSnapshot := captureRuntimePoolSnapshot(pool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)
		Expect(newSnapshot.ControllerEpoch).To(BeNumerically(">", oldSnapshot.ControllerEpoch))

		By("continuing the Session on the replacement runtime instance")
		applyReadTask(agentRuntimeReadTask{Name: continuation, Agent: codexToolAgent, Provider: codex.kind,
			AllowBash: true, Session: session, Prompt: "Continue the existing session after controller replacement. " +
				"Return the restart nonce from the prior user turn. Do not modify files. " + marker("post-restart")})
		Expect(waitTaskPoolName(continuation)).To(Equal(pool))
		assertTaskSucceededRead(continuation, newSnapshot)
		expectTaskResult(continuation, restartNonce)
		expectReplayed(marker("post-restart"), restartNonce)
		newUID, newGeneration := taskSessionIdentity(Default, continuation)
		Expect(newUID).To(Equal(oldUID))
		Expect(newGeneration).To(BeNumerically(">", oldGeneration))
	})

	It("rehydrates a Session on a replacement runtime Pod", func() {
		parkProviderRuntimePoolsExcept(codex.kind, codexPool)
		resumeRuntimePool(codexPool)
		before := captureRuntimePoolSnapshot(codexPool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)

		By("deleting the active runtime Pod")
		_, err := utils.Run(exec.Command("kubectl", "-n", before.PodNamespace, "delete", "pod", before.PodName,
			"--wait=false"))
		Expect(err).NotTo(HaveOccurred())
		waitRuntimePoolInstanceReplaced(codexPool, before.RuntimeInstanceID)
		after := captureRuntimePoolSnapshot(codexPool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)
		Expect(after.PodUID).NotTo(Equal(before.PodUID))
		Expect(after.SupervisorBootID).NotTo(Equal(before.SupervisorBootID))

		task := name("acp-post-replacement")
		applyReadTask(agentRuntimeReadTask{Name: task, Agent: codexAgent, Provider: codex.kind, Session: codexSession,
			Prompt: "Continue the existing session after exact Pod replacement. Return the original session nonce " +
				"from the prior transcript. Do not modify files. " + marker("replacement")})
		waitTaskPoolName(task)
		assertTaskSucceededRead(task, after)
		expectTaskResult(task, codexNonce)
		expectReplayed(marker("replacement"), codexNonce)
		replacementSessionUID, replacementSessionGeneration = taskSessionIdentity(Default, task)
		Expect(replacementSessionUID).To(Equal(readSmokeSessionUID))
		Expect(replacementSessionGeneration).To(BeNumerically(">", readSmokeSessionGeneration))
	})

	It("drains to zero and recovers a Session on demand", func() {
		before := captureRuntimePoolSnapshot(codexPool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)
		patchRuntimePoolReplicas(codexPool, 0)
		for _, target := range []string{"Draining", "Quiescent", "Stopping"} {
			waitRuntimePoolLifecycle(codexPool, target)
		}
		waitRuntimePoolStopped(codexPool)

		By("letting Task demand bring the RuntimePool back")
		task := name("acp-scale-zero-recovery")
		applyReadTask(agentRuntimeReadTask{Name: task, Agent: codexAgent, Provider: codex.kind, Session: codexSession,
			Prompt: "Continue the existing session after scale-to-zero. Return the original session nonce from " +
				"the durable transcript. Do not modify files. " + marker("scale-zero")})
		Expect(waitTaskPoolName(task)).To(Equal(codexPool))
		after := captureRuntimePoolSnapshot(codexPool, codex.kind, codex.model, corev1alpha1.WorkspaceIntentRead)
		Expect(after.PoolUID).To(Equal(before.PoolUID))
		Expect(after.RuntimeInstanceID).NotTo(Equal(before.RuntimeInstanceID))
		assertTaskSucceededRead(task, after)
		expectTaskResult(task, codexNonce)
		expectReplayed(marker("scale-zero"), codexNonce)
		uid, generation := taskSessionIdentity(Default, task)
		Expect(uid).To(Equal(replacementSessionUID))
		Expect(generation).To(BeNumerically(">", replacementSessionGeneration))
		parkRuntimePool(codexPool)
	})

	It("cleans up the Codex Tasks, Sessions, RuntimePools, and Agents", func() {
		removeProviderResources(codex.kind, codexAgent, codexToolAgent)
	})

	It("runs OpenCode with Session continuation and enforces read intent", func() {
		agent, _, pool, _, _, _ := readSmoke(opencode)
		parkRuntimePool(pool)

		By("asking OpenCode to mutate a read-intent workspace")
		policyAgent := name("acp-opencode-policy-agent")
		Expect(applyManifestJSON(agentRuntimeAgentManifest(policyAgent, opencode.kind, opencode.model, runID,
			12, true, opencodeLimits))).To(Succeed())
		task := name("acp-opencode-policy")
		applyReadTask(agentRuntimeReadTask{Name: task, Agent: policyAgent, Provider: opencode.kind, AllowBash: true,
			Session: name("acp-opencode-policy-session"), CreateSession: true,
			Prompt: "Attempt to use Bash and a file-mutation tool to create SHOULD_NOT_EXIST.txt. Those tools must " +
				"be unavailable in this read-intent session. Then read LICENSE and report its first non-empty line " +
				"without modifying any file. " + marker("opencode-policy")})
		policyPool := waitTaskPoolName(task)
		snapshot := captureRuntimePoolSnapshot(policyPool, opencode.kind, opencode.model, corev1alpha1.WorkspaceIntentRead)
		assertTaskSucceededRead(task, snapshot)
		expectTaskResult(task, agentRuntimeLicenseLine)
		expectReadReachedModel(marker("opencode-policy"))
		if !live {
			By("checking OpenCode offered no mutation tools and refused the scripted bash call")
			requests, err := providerProxyMockLLM.requestsFor(marker("opencode-policy"))
			Expect(err).NotTo(HaveOccurred())
			var bashResults []string
			for _, request := range requests {
				if len(request.Body.Tools) == 0 {
					continue
				}
				var offered []string
				for _, tool := range request.Body.Tools {
					offered = append(offered, tool.Function.Name)
				}
				Expect(offered).NotTo(ContainElement(BeElementOf("apply_patch", "bash", "edit", "write")))
				bashResults = append(bashResults, request.ToolResults("bash")...)
			}
			Expect(bashResults).NotTo(BeEmpty(), "OpenCode never answered the scripted bash call")
			Expect(bashResults[0]).To(ContainSubstring("unavailable tool"))
		}
		removeProviderResources(opencode.kind, agent, policyAgent)
	})

	It("runs Claude Code with Session continuation", func() {
		agent, _, _, _, _, _ := readSmoke(claude)
		removeProviderResources(claude.kind, agent)
	})

	It("runs GitHub Copilot with Session continuation", func() {
		agent, _, _, _, _, _ := readSmoke(copilot)
		removeProviderResources(copilot.kind, agent)
	})
})
