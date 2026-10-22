//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

// containerDeadlineScript leaves progress behind and then hangs, as a stuck
// browser run would.
const containerDeadlineScript = `mkdir -p /tmp/artifacts && printf before-the-hang > /tmp/artifacts/progress.txt && echo still-working && sleep 600`

func applyContainerDeadlineTask(name, timeout string) {
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Task",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"type":    "container",
			"command": []string{"sh", "-c"},
			"args":    []string{containerDeadlineScript},
			"timeout": timeout,
		},
	})
	Expect(err).NotTo(HaveOccurred())
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(manifest))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to create Task %s", name)
}

var _ = Describe("Container task deadline delivery", Ordered, func() {
	var (
		apiBaseURL     string
		cancelPF       context.CancelFunc
		portForwardCmd *exec.Cmd
		token          string
	)
	suffix := time.Now().UnixNano()
	deadlineTask := fmt.Sprintf("e2e-container-deadline-%d", suffix)
	cancelledTask := fmt.Sprintf("e2e-container-cancelled-%d", suffix)

	BeforeAll(func() {
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18141)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		for _, name := range []string{deadlineTask, cancelledTask} {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found"))
		}
		stopPortForward(cancelPF, portForwardCmd)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDebugInfo(deadlineTask, cancelledTask)
		}
	})

	It("delivers partial output and artifacts when the command outlives its timeout", func() {
		applyContainerDeadlineTask(deadlineTask, "30s")
		Expect(waitForTaskCompletion(deadlineTask, 3*time.Minute)).To(Equal("Failed"))

		raw := fetchTaskResultViaAPI(apiBaseURL, token, deadlineTask)
		var result struct {
			Version   int            `json:"version"`
			Outcome   string         `json:"outcome"`
			Summary   string         `json:"summary"`
			Data      map[string]any `json:"data"`
			Artifacts []struct {
				Filename string `json:"filename"`
			} `json:"artifacts"`
		}
		Expect(json.Unmarshal([]byte(raw), &result)).To(Succeed(), "result is not the worker's partial accounting: %s", raw)
		Expect(result.Version).To(Equal(1))
		Expect(result.Outcome).To(Equal("unknown_outcome"))
		Expect(result.Summary).To(ContainSubstring("still-working"))
		Expect(result.Data).To(HaveKeyWithValue("reason", "soft_deadline"))
		Expect(result.Data).To(HaveKeyWithValue("timeoutSeconds", BeNumerically("==", 30)))
		Expect(result.Artifacts).To(ContainElement(HaveField("Filename", "progress.txt")))

		body, status, err := doAuthorizedJSONRequest(http.MethodGet,
			fmt.Sprintf("%s/api/v1/tasks/%s/artifacts/progress.txt?namespace=%s", apiBaseURL, deadlineTask, namespace), token, "", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(http.StatusOK), body)
		Expect(body).To(Equal("before-the-hang"))
	})

	It("delivers nothing when a person cancels the Task", func() {
		applyContainerDeadlineTask(cancelledTask, "2m")
		var jobName string
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", namespace,
				"-l", "orka.ai/task="+cancelledTask, "-o", "jsonpath={.items[0].status.phase} {.items[0].metadata.labels.job-name}"))
			g.Expect(err).NotTo(HaveOccurred())
			fields := strings.Fields(out)
			g.Expect(fields).To(HaveLen(2))
			g.Expect(fields[0]).To(Equal("Running"))
			jobName = fields[1]
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("following the worker log through the cancellation")
		var logs bytes.Buffer
		follow := exec.Command("kubectl", "logs", "-f", "job/"+jobName, "-n", namespace)
		follow.Stdout = &logs
		follow.Stderr = &logs
		Expect(follow.Start()).To(Succeed())
		followed := make(chan error, 1)
		go func() { followed <- follow.Wait() }()
		time.Sleep(5 * time.Second)

		_, status, err := doAuthorizedJSONRequest(http.MethodDelete,
			fmt.Sprintf("%s/api/v1/tasks/%s?namespace=%s", apiBaseURL, cancelledTask, namespace), token, "", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(BeElementOf(http.StatusOK, http.StatusNoContent, http.StatusAccepted))

		select {
		case <-followed:
		case <-time.After(2 * time.Minute):
			_ = follow.Process.Kill()
			Fail("worker log did not end after the cancellation:\n" + logs.String())
		}
		Expect(logs.String()).To(ContainSubstring("still-working"), "the worker log was not captured")
		Expect(logs.String()).NotTo(ContainSubstring("soft deadline"), "cancellation triggered the soft deadline")
		Expect(logs.String()).NotTo(ContainSubstring("artifact: uploaded"), "a cancelled run uploaded artifacts")
	})
})
