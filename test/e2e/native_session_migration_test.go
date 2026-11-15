//go:build e2e && !windows

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/codexstate"
	"github.com/orka-agents/orka/test/utils"
	"github.com/orka-agents/sessionkit/harness/codex/writerlock"
)

const (
	// aimock synthesizes encrypted reasoning for reasoning models even at
	// effort none. Native migration deliberately rejects that format.
	nativeMigrationModel         = "gpt-4.1"
	nativeMigrationThread        = "01a10020-1222-76e3-977d-d5165792ae72"
	nativeMigrationNonce         = "SESSIONKIT_NONCE_7a8381a91bcf"
	nativeMigrationClusterMarker = "[e2e:native-migration-cluster]"
	nativeMigrationLocalMarker   = "[e2e:native-migration-local]"
	nativeMigrationClusterReply  = "NATIVE_CLUSTER_CONTINUATION"
	nativeMigrationLocalReply    = "NATIVE_LOCAL_CONTINUATION"
)

// Unlike the fresh-session lifecycle smoke, this spec imports a supported
// paginated rollout. It must preserve native tool history, not merely replay
// the canonical Orka transcript, through the public CLI and deployed runtime.
var _ = Describe("Native Codex Session migration", Label("agent-runtime"), func() {
	It("imports with the CLI, continues through aimock, exports, and resumes the same native thread locally", func() {
		if runtime.GOOS != "linux" {
			Skip("the pinned runtime image supplies a Linux Codex binary")
		}
		if e2eFlagEnabled(mockLLMDisableEnvVar) || e2eLiveCopilotProxyBaseURL != "" {
			Skip("requires the isolated aimock provider proxy")
		}
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(providerProxyMockLLM.delete)
		providerProxyMockLLM.deploy(projectDir)
		apiURL, cancelAPI, apiPF, err := startControllerAPIPortForward(18095)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { stopPortForward(cancelAPI, apiPF) })
		mockURL, cancelMock, mockPF, err := startHTTPPortForwardAndWait(
			providerProxyMockLLM.namespace, providerProxyMockLLM.name, 18096,
			providerProxyMockLLM.port, "/__aimock/health")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { stopPortForward(cancelMock, mockPF) })
		token, err := cleanupE2EToken(context.Background())
		Expect(err).NotTo(HaveOccurred())

		root, err := os.MkdirTemp("", "orka-native-migration-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(os.RemoveAll(root)).To(Succeed()) })
		cliHome := filepath.Join(root, "cli-home")
		Expect(os.Mkdir(cliHome, 0o700)).To(Succeed())
		localEnv := []string{"HOME=" + cliHome, "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "TMPDIR=" + root}
		orka := filepath.Join(root, "orka")
		By("building the public CLI and taking the pinned Codex executable from the tested runtime image")
		build := exec.Command("go", "build", "-o", orka, "./cmd/cli")
		_, err = utils.Run(build)
		Expect(err).NotTo(HaveOccurred())
		codex := filepath.Join(root, "codex")
		container, err := utils.Run(exec.Command("docker", "create", acpCodexRuntimeImage))
		Expect(err).NotTo(HaveOccurred())
		container = strings.TrimSpace(container)
		DeferCleanup(func() {
			_, err := utils.Run(exec.Command("docker", "rm", "-f", container))
			Expect(err).NotTo(HaveOccurred())
		})
		_, err = utils.Run(exec.Command("docker", "cp", "-L", container+":/opt/codex/bin/codex", codex))
		Expect(err).NotTo(HaveOccurred())
		version := nativeMigrationRun(codex, root, localEnv, "", "--version")
		Expect(version).To(Equal("codex-cli " + acp.CodexCLIVersion + "\n"))

		// The public stdin token command keeps the API credential out of argv,
		// logs, the source home, and the returned Codex home.
		nativeMigrationRun(orka, root, localEnv, token, "config", "set-token", "--file", "-")
		session := fmt.Sprintf("native-migration-%d", time.Now().UnixNano())
		agent, taskName := session+"-agent", session+"-task"
		DeferCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			Expect(cleanupAgentSessionTasks(ctx, apiURL, token, session, []string{taskName}, utils.Run)).To(Succeed())
			_, err := utils.Run(exec.CommandContext(ctx, "kubectl", "delete", "agent", agent, "-n", namespace,
				"--ignore-not-found", "--wait=true", "--timeout=2m"))
			Expect(err).NotTo(HaveOccurred())
		})

		By("importing a stopped source whose nonce exists only in native tool output")
		fixture := "rollout-2026-10-02T21-57-44-" + nativeMigrationThread + ".jsonl"
		original, err := os.ReadFile(filepath.Join(projectDir, "internal/codexstate/testdata", fixture))
		Expect(err).NotTo(HaveOccurred())
		sourceRollout, err := nativeMigrationSourceRollout(original)
		Expect(err).NotTo(HaveOccurred())
		sourceHome := filepath.Join(root, "source")
		sourcePath := filepath.Join(sourceHome, "sessions/2026/10/02", fixture)
		Expect(os.MkdirAll(filepath.Dir(sourcePath), 0o700)).To(Succeed())
		Expect(os.WriteFile(sourcePath, sourceRollout, 0o600)).To(Succeed())
		// Source authentication and policy are deliberately unusable. Neither
		// may become authority in the cluster or the exported destination.
		auth := []byte(`{"OPENAI_API_KEY":"revoked-native-source-test-key"}`)
		Expect(os.WriteFile(filepath.Join(sourceHome, "auth.json"), auth, 0o600)).To(Succeed())
		sourceConfig := []byte("model = \"invalid-source-model\"\napproval_policy = \"untrusted\"\n")
		Expect(os.WriteFile(filepath.Join(sourceHome, "config.toml"), sourceConfig, 0o600)).To(Succeed())
		// A real stopped Codex home already has its native lock namespace.
		// Capture retains these inodes; initialize them without weakening the
		// exact source-file immutability assertion or holding an active writer.
		lock, err := writerlock.Source(context.Background(), sourceHome, nativeMigrationThread)
		Expect(err).NotTo(HaveOccurred())
		Expect(lock.Close()).To(Succeed())
		before, err := nativeMigrationHomeFiles(sourceHome)
		Expect(err).NotTo(HaveOccurred())
		importJournal := filepath.Join(root, "import-journal")
		importArgs := []string{"--server", apiURL, "--namespace", namespace, "session", "migrate", "import", session,
			"--codex-home", sourceHome, "--thread", nativeMigrationThread, "--journal-dir", importJournal, "--source-stopped"}
		Expect(nativeMigrationRun(orka, root, localEnv, "", importArgs...)).To(
			ContainSubstring("Staged thread " + nativeMigrationThread))
		// Exact public retries must reuse the staged import, not duplicate it.
		Expect(nativeMigrationRun(orka, root, localEnv, "", importArgs...)).To(
			ContainSubstring("Staged thread " + nativeMigrationThread))

		By("continuing the imported native thread on the deployed Codex RuntimePool")
		agentManifest := agentRuntimeAgentManifest(agent, "codex", nativeMigrationModel, session, 4, false, nil)
		Expect(applyManifestJSON(agentManifest)).To(Succeed())
		task := agentRuntimeReadTask{Name: taskName, Agent: agent, Provider: "codex", Session: session,
			CreateSession: true, MaxTurns: 4, Timeout: "5m", RepoURL: agentRuntimeRepoURL, Ref: agentRuntimeRepoRef,
			RepoIdentity: agentRuntimeRepoIdentity,
			Prompt: "Acknowledge the nonce from the earlier tool result without running another tool. " +
				nativeMigrationClusterMarker}
		Expect(task.Prompt).NotTo(ContainSubstring(nativeMigrationNonce))
		Expect(applyManifestJSON(task.manifest(session))).To(Succeed())
		pool := waitTaskPoolName(taskName)
		snapshot := captureRuntimePoolSnapshot(pool, "codex", nativeMigrationModel, corev1alpha1.WorkspaceIntentRead)
		assertTaskSucceededRead(taskName, snapshot)
		Expect(fetchTaskResultViaAPI(apiURL, token, taskName)).To(ContainSubstring(nativeMigrationClusterReply))
		expectNativeMigrationInference(nativeMigrationClusterMarker, "")

		By("exporting the committed checkpoint into a fresh private home through the CLI")
		destination, cwd := filepath.Join(root, "destination"), filepath.Join(root, "workspace")
		Expect(os.Mkdir(cwd, 0o700)).To(Succeed())
		exportJournal := filepath.Join(root, "export-journal")
		exportArgs := []string{"--server", apiURL, "--namespace", namespace, "session", "migrate", "export", session,
			"--codex-home", destination, "--cwd", cwd, "--journal-dir", exportJournal, "--codex-bin", codex}
		Expect(nativeMigrationRun(orka, root, localEnv, "", exportArgs...)).To(
			ContainSubstring("Installed thread " + nativeMigrationThread))
		Expect(nativeMigrationRun(orka, root, localEnv, "", exportArgs...)).To(
			ContainSubstring("Installed thread " + nativeMigrationThread))
		for _, name := range []string{"auth.json", "config.toml"} {
			_, err := os.Stat(filepath.Join(destination, name))
			Expect(os.IsNotExist(err)).To(BeTrue(), "source %s must not be copied", name)
		}
		for _, dir := range []string{destination, importJournal, exportJournal} {
			info, err := os.Stat(dir)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(fs.FileMode(0o700)))
		}
		exported := nativeMigrationReadRollout(destination)
		Expect(bytes.HasPrefix(exported, sourceRollout)).To(BeTrue(), "export must retain the original native prefix")
		Expect(string(exported)).To(ContainSubstring(nativeMigrationClusterReply))
		bundle, err := codexstate.Capture(context.Background(), destination, nativeMigrationThread)
		Expect(err).NotTo(HaveOccurred())
		summary, err := codexstate.Inspect(context.Background(), bundle)
		Expect(err).NotTo(HaveOccurred())
		Expect(summary.ThreadID).To(Equal(nativeMigrationThread))

		By("resuming the returned thread with real pinned Codex and newly supplied local policy and authentication")
		const localKey = "aimock-native-return-test-key"
		localProvider, err := nativeMigrationLocalProvider(mockURL, localKey)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(localProvider.Close)
		config := fmt.Sprintf(`model = %q
model_provider = "aimock"
model_reasoning_effort = "none"
approval_policy = "never"
sandbox_mode = "read-only"
[model_providers.aimock]
name = "aimock"
base_url = %q
wire_api = "responses"
env_key = "NATIVE_MIGRATION_LOCAL_KEY"
`, nativeMigrationModel, localProvider.URL+"/v1")
		Expect(os.WriteFile(filepath.Join(destination, "config.toml"), []byte(config), 0o600)).To(Succeed())
		resumeEnv := append(append([]string{}, localEnv...), "CODEX_HOME="+destination,
			"NATIVE_MIGRATION_LOCAL_KEY="+localKey)
		prompt := "Continue the same conversation and acknowledge its last reply. Do not run tools. " +
			nativeMigrationLocalMarker
		Expect(prompt).NotTo(ContainSubstring(nativeMigrationNonce))
		Expect(prompt).NotTo(ContainSubstring(nativeMigrationClusterReply))
		output := nativeMigrationRun(codex, cwd, resumeEnv, "",
			"exec", "resume", "--skip-git-repo-check", "--json", nativeMigrationThread, prompt)
		var resumed bool
		for line := range strings.SplitSeq(output, "\n") {
			var event struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
			}
			if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" {
				Expect(event.ThreadID).To(Equal(nativeMigrationThread))
				resumed = true
			}
		}
		Expect(resumed).To(BeTrue(), "Codex must resume the original UUID, not start a new thread")
		Expect(output).To(ContainSubstring(nativeMigrationLocalReply))
		expectNativeMigrationInference(nativeMigrationLocalMarker, nativeMigrationClusterReply)
		returned := nativeMigrationReadRollout(destination)
		Expect(bytes.HasPrefix(returned, exported)).To(BeTrue(), "local resume must append to the exported checkpoint")
		Expect(string(returned)).To(ContainSubstring(nativeMigrationLocalReply))
		Expect(nativeMigrationCurrentPolicy(returned[len(exported):], cwd)).To(BeTrue(),
			"resumed turn must use the destination cwd, model, approval, and sandbox policy")
		after, err := nativeMigrationHomeFiles(sourceHome)
		Expect(err).NotTo(HaveOccurred())
		Expect(nativeMigrationHomesEqual(before, after)).To(BeTrue(),
			"migration must preserve source contents, file identities, modes, and mtimes")
	})
})

// Run without utils.Run: it replaces Env/Dir and logs argv and failure output.
// These subprocesses must never inherit the user's Codex home or credentials.
func nativeMigrationRun(binary, cwd string, env []string, stdin string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = cwd, env, strings.NewReader(stdin)
	cmd.WaitDelay = 5 * time.Second
	// Codex can warn about PATH aliases on stderr in temporary homes. Keep
	// machine-readable stdout separate and never retain credential-bearing diagnostics.
	cmd.Stderr = io.Discard
	output, err := cmd.Output()
	// Native history and authentication are not safe failure diagnostics.
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "native migration subprocess failed (output omitted)")
	return string(output)
}

func expectNativeMigrationInference(marker, previousReply string) {
	EventuallyWithOffset(1, func(g Gomega) {
		requests, err := providerProxyMockLLM.servedFor(marker)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requests).NotTo(BeEmpty(), "aimock served no native continuation")
		for _, request := range requests {
			g.Expect(request.Body.Truncated).To(BeFalse(), "native-history proof must not be skipped for truncated journals")
			g.Expect(request.Response.Status).To(Equal(200))
			g.Expect(request.Body.Model).To(Equal(nativeMigrationModel))
			g.Expect(strings.Join(request.ToolResults("exec_command"), "\n")).To(ContainSubstring(nativeMigrationNonce),
				"the original native tool result must reach inference")
			g.Expect(request.RoleText("user")).NotTo(ContainSubstring(nativeMigrationNonce))
			if previousReply != "" {
				g.Expect(request.RoleText("assistant")).To(ContainSubstring(previousReply))
			}
		}
	}, time.Minute, 2*time.Second).Should(Succeed())
}

func nativeMigrationReadRollout(home string) []byte {
	files, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "*"+nativeMigrationThread+".jsonl"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, files).To(HaveLen(1), "exactly the original native thread must be present")
	data, err := os.ReadFile(files[0])
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return data
}

type nativeMigrationFileSnapshot struct {
	Digest string
	Info   fs.FileInfo
}

func nativeMigrationHomeFiles(home string) (map[string]nativeMigrationFileSnapshot, error) {
	files := map[string]nativeMigrationFileSnapshot{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		// Keep only digests and metadata, never credential-bearing file contents.
		files[rel] = nativeMigrationFileSnapshot{Digest: codexstate.DataDigest(data), Info: info}
		return nil
	})
	return files, err
}

func nativeMigrationHomesEqual(before, after map[string]nativeMigrationFileSnapshot) bool {
	if len(before) != len(after) {
		return false
	}
	for path, original := range before {
		current, ok := after[path]
		if !ok || original.Digest != current.Digest || !os.SameFile(original.Info, current.Info) ||
			original.Info.Mode() != current.Info.Mode() || !original.Info.ModTime().Equal(current.Info.ModTime()) {
			return false
		}
	}
	return true
}

// Remove the nonce from the historical command arguments so only a genuine
// native function_call_output (not a prompt or assistant summary) proves replay.
func nativeMigrationSourceRollout(original []byte) ([]byte, error) {
	var out bytes.Buffer
	for line := range bytes.SplitSeq(bytes.TrimSpace(original), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, err
		}
		payload, ok := record["payload"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("native source record has no payload")
		}
		if payload["type"] == "function_call" {
			payload["arguments"] = `{"cmd":"cat source-nonce.txt","yield_time_ms":1000,"max_output_tokens":100}`
		}
		if item, ok := payload["item"].(map[string]any); ok && item["type"] == "CommandExecution" {
			item["command"] = []string{"/bin/sh", "-lc", "cat source-nonce.txt"}
			item["parsed_cmd"] = []any{map[string]any{"cmd": "cat source-nonce.txt", "type": "unknown"}}
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// aimock does not require authentication. This gate makes the local return leg
// fail unless real Codex uses the freshly supplied destination credential.
func nativeMigrationLocalProvider(upstreamURL, key string) (*httptest.Server, error) {
	upstream, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "current destination credential required", http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(w, r)
	})), nil
}

func nativeMigrationCurrentPolicy(appended []byte, cwd string) bool {
	for line := range bytes.SplitSeq(appended, []byte("\n")) {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				CWD            string `json:"cwd"`
				Model          string `json:"model"`
				ApprovalPolicy string `json:"approval_policy"`
				SandboxPolicy  struct {
					Type string `json:"type"`
				} `json:"sandbox_policy"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &record) == nil && record.Type == "turn_context" &&
			record.Payload.CWD == cwd && record.Payload.Model == nativeMigrationModel &&
			record.Payload.ApprovalPolicy == "never" && record.Payload.SandboxPolicy.Type == "read-only" {
			return true
		}
	}
	return false
}
