//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

const externalV2RuntimeName = "external-v2-runtime"

var _ = Describe("AgentRuntime external dispatch", func() {
	const (
		agentName = "e2e-external-v2-agent"
		taskName  = "e2e-external-v2-task"
	)

	AfterEach(func() {
		for _, resource := range []struct{ kind, name string }{
			{"task", taskName},
			{"agent", agentName},
			{"agentruntime", externalV2RuntimeName},
		} {
			cmd := exec.Command("kubectl", "delete", resource.kind, resource.name,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
	})

	It("rejects orka.harness.v1 registrations in a harness-v2 namespace", func() {
		manifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "AgentRuntime",
			"metadata": {"name": "e2e-v1-rejected", "namespace": %q},
			"spec": {
				"contractVersion": "orka.harness.v1",
				"clientAuth": {
					"bearerTokenSecretRef": {"name": "runtime-auth", "key": "token"}
				},
				"deployment": {"mode": "external-endpoint", "endpoint": "https://runtime.example.com"}
			}
		}`, namespace)
		cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
		cmd.Stdin = stringReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).To(MatchError(ContainSubstring(
			`AgentRuntime contractVersion must match namespace execution mode "harness-v2"`,
		)))
	})

	It("fails closed at the external Task dispatch support boundary", func() {
		runtimeManifest, manifestErr := externalV2RuntimeManifest()
		Expect(manifestErr).NotTo(HaveOccurred())
		Expect(applyManifestJSON(runtimeManifest)).To(Succeed())

		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"runtime": {"runtimeRef": {"name": %q}}
			}
		}`, agentName, namespace, externalV2RuntimeName)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {"name": %q, "namespace": %q},
			"spec": {
				"type": "agent",
				"agentRef": {"name": %q},
				"prompt": "Prepare a governed change.",
				"workspace": {
					"intent": "write",
					"gitRepo": "https://github.com/example/repo",
					"publicationGitRepo": "https://github.com/example/repo"
				}
			}
		}`, taskName, namespace, agentName)
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", taskName, "-n", namespace,
				"-o", "jsonpath={.status.phase}{\"/\"}{.status.message}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(HavePrefix("Failed/"))
			g.Expect(output).To(ContainSubstring(fmt.Sprintf("external AgentRuntime %q Task dispatch is not supported until the v2 dispatcher is wired", externalV2RuntimeName)))
		}, 2*time.Minute, time.Second).Should(Succeed())

		verifyNoJobForTask(taskName, 5*time.Second)
	})
})

func externalV2RuntimeManifest() (map[string]any, error) {
	manifest, err := gatewayE2ERuntimeManifest()
	if err != nil {
		return nil, err
	}
	manifest["metadata"].(map[string]any)["name"] = externalV2RuntimeName
	return manifest, nil
}
