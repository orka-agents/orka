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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Orka CLI session and memory binary workflows", Ordered, func() {
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

		By("setting up a controller API port-forward for session and memory CLI commands")
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18112)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")

		By("creating an isolated CLI home with service-account credentials")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
		home = newIsolatedCLIHome(apiBaseURL, token)
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	AfterAll(func() {
		By("stopping session and memory CLI controller API port-forward")
		stopPortForward(cancelPF, portForwardCmd)
	})

	It("lists, reads, and deletes a controlled task session", func() {
		taskName := "e2e-cli-session-task-" + suffix
		sessionName := "e2e-cli-session-" + suffix
		promptSentinel := "cli e2e session prompt " + suffix
		resultSentinel := "cli-e2e-session-result-" + suffix
		tmpDir := GinkgoT().TempDir()

		DeferCleanup(deleteK8sResource, "task", taskName)
		DeferCleanup(func() { _ = runOrka(home, "session", "delete", sessionName) })

		By("seeding a session through a completed container task with sessionRef")
		manifest := writeTempManifest(tmpDir, "session-task.yaml", fmt.Sprintf(`
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: %s
spec:
  type: container
  image: busybox:latest
  command: ["sh", "-c"]
  args: ["echo %s"]
  prompt: %q
  sessionRef:
    name: %s
    create: true
    append: true
`, taskName, resultSentinel, promptSentinel, sessionName))
		expectOrkaSuccess(runOrka(home, "task", "create", "-f", manifest), token, resultSentinel)
		expectOrkaSuccess(runOrkaWithTimeout(4*time.Minute, home, "task", "wait", taskName, "--timeout", "3m"), token)

		By("listing sessions as JSON")
		list := runOrka(home, "session", "list", "-o", "json")
		expectOrkaSuccess(list, token)
		expectListContainsName(expectJSONOutput(list.Stdout), sessionName)

		By("getting the seeded session as JSON")
		var get cliResult
		Eventually(func() error {
			get = runOrka(home, "session", "get", sessionName, "-o", "json")
			expectNoSensitiveOutput(get, token)
			if get.Err != nil {
				return fmt.Errorf("session get failed: %v\n%s", get.Err, redactedCLIOutput(get, token))
			}
			decoded := expectJSONObject(get.Stdout)
			if nestedStringFromMap(decoded, "name") != sessionName {
				return fmt.Errorf("session name %q did not match %q", nestedStringFromMap(decoded, "name"), sessionName)
			}
			transcript := fmt.Sprint(decoded["transcript"])
			if !strings.Contains(transcript, promptSentinel) || !strings.Contains(transcript, resultSentinel) {
				return fmt.Errorf("session transcript missing sentinels: %s", truncateForLog(transcript, 512))
			}
			return nil
		}, 45*time.Second, 2*time.Second).Should(Succeed())
		decoded := expectJSONObject(get.Stdout)
		Expect(nestedStringFromMap(decoded, "namespace")).To(Equal(namespace))
		Expect(decoded["messageCount"]).To(BeNumerically(">=", 2))

		By("deleting the session through the CLI and proving absence")
		deleted := runOrka(home, "session", "delete", sessionName)
		expectOrkaSuccess(deleted, token)
		missing := runOrka(home, "session", "get", sessionName, "-o", "json")
		expectOrkaFailure(missing, token)
		listAfterDelete := runOrka(home, "session", "list", "-o", "json")
		expectOrkaSuccess(listAfterDelete, token)
		expectListDoesNotContainName(expectJSONOutput(listAfterDelete.Stdout), sessionName)
	})

	It("creates, lists, reads, toggles, updates, deletes, and lists memory proposals", func() {
		content := "cli e2e memory " + suffix
		updatedContent := "cli e2e memory updated " + suffix
		var memoryID string
		DeferCleanup(func() {
			if memoryID != "" {
				_ = runOrka(home, "memory", "delete", memoryID)
			}
		})

		By("creating controlled durable memory through the CLI")
		created := runOrka(home, "memory", "create", "--content", content, "--source", "cli-e2e", "--tags", "e2e,cli")
		expectOrkaSuccess(created, token, content)
		memoryID = parseCreatedResourceName(created.Stdout)
		Expect(memoryID).To(HavePrefix("mem-"))

		By("listing the created memory as JSON")
		list := runOrka(home, "memory", "list", "-o", "json", "--query", content)
		expectOrkaSuccess(list, token)
		expectListContainsName(expectJSONOutput(list.Stdout), memoryID)

		By("getting the created memory as JSON")
		get := runOrka(home, "memory", "get", memoryID, "-o", "json")
		expectOrkaSuccess(get, token)
		memory := expectJSONObject(get.Stdout)
		Expect(nestedStringFromMap(memory, "id")).To(Equal(memoryID))
		Expect(nestedStringFromMap(memory, "namespace")).To(Equal(namespace))
		Expect(nestedStringFromMap(memory, "content")).To(Equal(content))
		Expect(nestedStringFromMap(memory, "source")).To(Equal("cli-e2e"))
		Expect(fmt.Sprint(memory["tags"])).To(ContainSubstring("e2e"))
		Expect(fmt.Sprint(memory["tags"])).To(ContainSubstring("cli"))

		By("disabling and enabling the memory")
		expectOrkaSuccess(runOrka(home, "memory", "disable", memoryID), token)
		disabled := expectJSONObject(runSuccessfulOrka(home, []string{token}, "memory", "get", memoryID, "-o", "json").Stdout)
		Expect(disabled["disabled"]).To(Equal(true))
		expectOrkaSuccess(runOrka(home, "memory", "enable", memoryID), token)
		enabled := expectJSONObject(runSuccessfulOrka(home, []string{token}, "memory", "get", memoryID, "-o", "json").Stdout)
		Expect(enabled["disabled"]).To(Equal(false))

		By("updating memory content")
		updated := runOrka(home, "memory", "update", memoryID, "--content", updatedContent)
		expectOrkaSuccess(updated, token, updatedContent)
		updatedGet := expectJSONObject(runSuccessfulOrka(home, []string{token}, "memory", "get", memoryID, "-o", "json").Stdout)
		Expect(nestedStringFromMap(updatedGet, "content")).To(Equal(updatedContent))

		By("deleting the memory and proving normal list output excludes it")
		deleted := runOrka(home, "memory", "delete", memoryID)
		expectOrkaSuccess(deleted, token)
		listAfterDelete := runOrka(home, "memory", "list", "-o", "json", "--query", updatedContent)
		expectOrkaSuccess(listAfterDelete, token)
		expectListDoesNotContainName(expectJSONOutput(listAfterDelete.Stdout), memoryID)

		By("listing memory proposals as parseable JSON")
		proposalList := runOrka(home, "memory", "proposal", "list", "-o", "json")
		expectOrkaSuccess(proposalList, token)
		expectJSONOutput(proposalList.Stdout)
	})
})

func parseCreatedResourceName(output string) string {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSuffix(fields[len(fields)-1], ".")
}
