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

var _ = Describe("Provider Advanced Features", Ordered, func() {
	const (
		providerName = "e2e-rate-limit-provider"
		secretName   = "e2e-prov-adv-secret"
	)

	AfterEach(func() {
		dumpDebugInfo()
	})

	AfterAll(func() {
		By("cleaning up provider advanced test resources")
		cmd := exec.Command("kubectl", "delete", "provider", providerName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "secret", secretName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("should create a provider with rate limit configuration", func() {
		By("creating a dummy secret for the provider")
		secretManifest := fmt.Sprintf(`{
			"apiVersion": "v1",
			"kind": "Secret",
			"metadata": {"name": "%s", "namespace": "%s"},
			"type": "Opaque",
			"stringData": {"api-key": "dummy-key-for-structural-test"}
		}`, secretName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(secretManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create dummy secret")

		By("creating a Provider with rateLimit config")
		providerManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Provider",
			"metadata": {"name": "%s", "namespace": "%s"},
			"spec": {
				"type": "openai",
				"secretRef": {"name": "%s", "key": "api-key"},
				"rateLimit": {
					"requestsPerMinute": 10
				}
			}
		}`, providerName, namespace, secretName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(providerManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Provider with rateLimit")

		By("verifying the Provider becomes ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "provider", providerName,
				"-n", namespace, "-o", "jsonpath={.status.ready}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("true"), "Provider should become ready")
		}, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the rateLimit config is persisted in the provider spec")
		cmd = exec.Command("kubectl", "get", "provider", providerName,
			"-n", namespace, "-o", "jsonpath={.spec.rateLimit.requestsPerMinute}")
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		Expect(output).To(Equal("10"), "Provider should have requestsPerMinute=10")
	})
})
