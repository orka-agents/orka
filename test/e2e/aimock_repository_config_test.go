//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/test/fixtures/fakeforge"
	"github.com/orka-agents/orka/test/utils"
)

const (
	// repositoryConfigSkillRegistryPath is the only registry path the suite's
	// controller allows for runtime skills.
	repositoryConfigSkillRegistryPath = "orka/e2e-skills"
	repositoryConfigSkillName         = "repo-config-check"
	// Appears only in the skill's description, which each runtime lists to the
	// model when it loaded the skill.
	repositoryConfigSkillNonce = "orka-e2e-skill-listed-5c2d9e17"
)

// runtimeEvalRepositoryConfigCase runs one read-intent Task against a commit
// that carries agent configuration for every runtime (see
// fakeforge.RepositoryConfigBranch). None of it may run: every command in it
// only creates a marker in /tmp, and the Codex developer instructions carry a
// nonce that must never reach the model. Model traffic must still flow
// through the session-local proxy, which the repository's Claude settings try
// to replace with a dead address. OpenCode refuses such a workspace outright.
// With a skill image, the Agent also carries an Orka runtime skill, which must
// still reach the model.
type runtimeEvalRepositoryConfigCase struct {
	provider, model, runID, name, repoURL, ref, skillImage string
	readCredential                                         map[string]any
	applyTask                                              func(map[string]any)
	applyAgent                                             func(map[string]any)
}

func (c runtimeEvalRepositoryConfigCase) run() {
	marker := runtimeEvalMarker(c.provider, "repo-config")
	providerProxyMockLLM.injectFixtures(map[string]any{
		"match":    map[string]any{"userMessage": marker},
		"response": map[string]any{"content": "Repository configuration check completed."},
	})

	agent := agentRuntimeAgentManifest(c.name+"-agent", c.provider, c.model, c.runID, 12, false,
		&corev1alpha1.ModelTokenLimits{Context: 32768, Output: 4096})
	runtime := agent["spec"].(map[string]any)["runtime"].(map[string]any)
	runtime["defaultAllowedTools"], runtime["defaultAllowBash"] = []string{"Read", "Glob", "Grep"}, false
	if c.skillImage != "" {
		runtime["skills"] = []map[string]any{{"name": repositoryConfigSkillName, "image": c.skillImage}}
	}
	c.applyAgent(agent)

	task := (agentRuntimeReadTask{
		Name: c.name + "-task", Agent: c.name + "-agent", Provider: c.provider,
		Prompt: "Report completion without using any tools. " + marker, Timeout: "5m", MaxTurns: 12,
		RepoURL: c.repoURL, Ref: c.ref,
	}).manifest(c.runID)
	task["spec"].(map[string]any)["workspace"].(map[string]any)["readCredentialRef"] = c.readCredential
	before, err := providerProxyMockLLM.evalSnapshot()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	c.applyTask(task)

	if c.provider == "opencode" {
		failed := repositoryConfigWaitFailedTask(c.name + "-task")
		ExpectWithOffset(1, failed.Status.Execution.Reason).To(Equal(corev1alpha1.TaskExecutionReason("RuntimeSessionStartFailed")))
		ExpectWithOffset(1, failed.Status.Message).To(Equal(acp.OpenCodeProjectConfigurationRejectedMessage))
		// OpenCode never started, so the model saw no request at all.
		after, err := providerProxyMockLLM.evalSnapshot()
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, after.instanceID).To(Equal(before.instanceID), "aimock restarted during the check")
		for id := range after.requestIDs {
			ExpectWithOffset(1, before.requestIDs).To(HaveKey(id), "OpenCode reached the model from a refused workspace")
		}
		return
	}

	completed := runtimeEvalWaitTask(c.name+"-task", corev1alpha1.TaskDeliveryOutcomeReadValidated)
	canaries := runtimeEvalCanaries()
	canaries["repository-developer-instructions"] = fakeforge.RepositoryConfigInstructionsNonce
	requests := runtimeEvalRequests(before, marker, canaries)
	// The repository's Claude settings point the provider at a dead address;
	// a model request carrying the marker proves the runtime kept the route
	// through the session-local proxy.
	ExpectWithOffset(1, requests).NotTo(BeEmpty(), "no model request reached the provider proxy")
	pool := getRuntimePool(Default, completed.Status.Execution.RuntimePoolName)
	runtimeEvalVerifyPoolBearer(pool)
	if c.skillImage != "" {
		ExpectWithOffset(1, runtimeEvalHasModelText(requests, marker, repositoryConfigSkillNonce)).To(BeTrue(),
			"the Orka runtime skill did not reach the model")
	}
	repositoryConfigExpectNoMarkers(pool)
}

// buildRepositoryConfigSkillImage publishes a one-skill FROM scratch image to
// the suite's local registry and returns its digest reference.
func buildRepositoryConfigSkillImage() (string, error) {
	dir, err := os.MkdirTemp("", "orka-e2e-skill-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	skillDir := filepath.Join(dir, "skills", repositoryConfigSkillName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return "", err
	}
	skill := "---\nname: " + repositoryConfigSkillName + "\ndescription: Checks repository configuration. " +
		repositoryConfigSkillNonce + "\n---\n\nReport what you checked.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		return "", err
	}
	// The copier runs as a fixed non-root identity, so everything must be
	// world-readable.
	for _, path := range []string{dir, filepath.Join(dir, "skills"), skillDir} {
		if err := os.Chmod(path, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(filepath.Join(skillDir, "SKILL.md"), 0o644); err != nil {
		return "", err
	}
	dockerfile := "FROM scratch\nCOPY skills/ /skills/\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}
	image := "ghcr.io/orka-agents/orka/e2e-repo-config-skill:e2e"
	if _, err := utils.Run(exec.Command("docker", "build", "-t", image, dir)); err != nil {
		return "", err
	}
	return pushImageToLocalRegistry(e2eRegistryAddress, image,
		repositoryConfigSkillRegistryPath+"/"+repositoryConfigSkillName+":e2e")
}

// repositoryConfigWaitFailedTask waits for a terminal Task and requires it to
// have failed.
func repositoryConfigWaitFailedTask(name string) corev1alpha1.Task {
	var task corev1alpha1.Task
	EventuallyWithOffset(2, func(g Gomega) {
		body, err := exec.Command("kubectl", "get", "task", name, "-n", namespace,
			"--request-timeout=10s", "-o", "json").Output()
		g.Expect(err).NotTo(HaveOccurred(), "could not read repository config Task")
		g.Expect(json.Unmarshal(body, &task)).To(Succeed())
		g.Expect(task.Status.Phase).To(BeElementOf(corev1alpha1.TaskPhaseSucceeded,
			corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled))
	}, runtimeEvalTaskWait, 2*time.Second).Should(Succeed())
	ExpectWithOffset(2, task.Status.Phase).To(Equal(corev1alpha1.TaskPhaseFailed))
	ExpectWithOffset(2, task.Status.Execution).NotTo(BeNil(), "missing execution")
	return task
}

// repositoryConfigExpectNoMarkers lists /tmp in the runtime Pod. Session
// children run as distinct unprivileged identities, so the check also requires
// /tmp to be writable by them; otherwise a missing marker would prove nothing.
func repositoryConfigExpectNoMarkers(pool corev1alpha1.RuntimePool) {
	_, pod := runtimePoolAndActivePod(Default, pool.Name)
	container := ""
	for _, candidate := range pod.Spec.Containers {
		for _, env := range candidate.Env {
			if env.Name == "ORKA_ACP_PROVIDER_TOKEN_FILE" {
				container = candidate.Name
			}
		}
	}
	ExpectWithOffset(2, container).NotTo(BeEmpty(), "runtime Pod has no supervisor container")
	output, err := exec.Command("kubectl", "exec", "-n", pod.Namespace, pod.Name, "-c", container, "--",
		"sh", "-c", "stat -c %a /tmp && ls -A /tmp").Output()
	ExpectWithOffset(2, err).NotTo(HaveOccurred(), "could not list /tmp in the runtime Pod")
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	ExpectWithOffset(2, lines[0]).To(HaveSuffix("777"), "session children cannot write /tmp, so the marker check is void")
	var markers []string
	for _, entry := range lines[1:] {
		if entry = strings.TrimSpace(entry); strings.HasPrefix(entry, fakeforge.RepositoryConfigMarkerPrefix) {
			markers = append(markers, entry)
		}
	}
	ExpectWithOffset(2, markers).To(BeEmpty(), "repository agent configuration ran")
}
