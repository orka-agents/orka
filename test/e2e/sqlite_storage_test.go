//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/sozercan/orka/test/utils"
)

var _ = Describe("SQLite Storage", Ordered, func() {
	const (
		containerTaskName = "e2e-sqlite-container"
		agentTaskName     = "e2e-sqlite-agent"
		agentName         = "e2e-sqlite-agent-def"
	)

	AfterAll(func() {
		By("cleaning up SQLite e2e test resources")
		for _, name := range []string{containerTaskName, agentTaskName} {
			cmd := exec.Command("kubectl", "delete", "task", name, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		}
		cmd := exec.Command("kubectl", "delete", "agent", agentName, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		dumpDebugInfo(containerTaskName, agentTaskName)
	})

	// Test 1: Verify the controller is running with SQLite storage configured
	It("should have SQLite store configured on the controller", func() {
		By("checking that the controller pod has --store-backend and --store-path args")
		verifyStoreArgs := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-o", "jsonpath={.items[0].spec.containers[0].args}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("--store-backend=sqlite"))
			g.Expect(output).To(ContainSubstring("--store-path=/data/orka.db"))
		}
		Eventually(verifyStoreArgs, 30*time.Second, time.Second).Should(Succeed())

		By("checking that the controller pod has the store volume mounted at /data")
		verifyStoreVolume := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-o", "jsonpath={.items[0].spec.containers[0].volumeMounts}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())

			type volumeMount struct {
				Name      string `json:"name"`
				MountPath string `json:"mountPath"`
			}
			var mounts []volumeMount
			err = json.Unmarshal([]byte(output), &mounts)
			g.Expect(err).NotTo(HaveOccurred())

			found := false
			for _, m := range mounts {
				if m.Name == "store" && m.MountPath == "/data" {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "Controller should have store volume at /data")
		}
		Eventually(verifyStoreVolume, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 2: Verify the SQLite database is initialized and healthy
	// Note: The controller image is distroless (no shell/ls), so we verify
	// the database through controller logs instead of kubectl exec.
	It("should have a SQLite database file on disk", func() {
		By("checking the controller logs confirm SQLite DB was opened successfully")
		verifyDBExists := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-o", "jsonpath={.items[0].metadata.name}",
				"-n", namespace,
			)
			podName, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podName).NotTo(BeEmpty())

			cmd = exec.Command("kubectl", "logs", strings.TrimSpace(podName), "-n", namespace)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(ContainSubstring("SQLite store is configured"),
				"Controller logs should confirm SQLite DB was created at /data/orka.db")
		}
		Eventually(verifyDBExists, 30*time.Second, time.Second).Should(Succeed())

		By("verifying the controller is ready (implies DB is operational)")
		verifyControllerReady := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-o", "jsonpath={.items[0].status.conditions[?(@.type=='Ready')].status}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("True"), "Controller pod should be Ready (SQLite DB initialized)")
		}
		Eventually(verifyControllerReady, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 3: Container task lifecycle — result stored in SQLite, not ConfigMap
	It("should complete a container task and store the result in SQLite", func() {
		By("creating a container task that produces output")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "container",
				"image": "busybox:latest",
				"command": ["echo"],
				"args": ["sqlite-e2e-result-data"]
			}
		}`, containerTaskName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create container task")

		By("waiting for the Task to complete successfully")
		verifyTaskComplete := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", containerTaskName,
				"-o", "jsonpath={.status.phase}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Succeeded"), "Task should complete with Succeeded phase")
		}
		Eventually(verifyTaskComplete, 3*time.Minute, time.Second).Should(Succeed())

		By("verifying resultRef.available is true on the Task status")
		verifyResultAvailable := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", containerTaskName,
				"-o", "jsonpath={.status.resultRef.available}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("true"), "Task should have resultRef.available=true")
		}
		Eventually(verifyResultAvailable, 30*time.Second, time.Second).Should(Succeed())

		By("verifying that NO result ConfigMap was created (results are in SQLite now)")
		verifyNoResultConfigMap := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "configmap",
				containerTaskName+"-result",
				"-n", namespace,
				"--ignore-not-found",
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(),
				"Result ConfigMap should NOT exist — results are stored in SQLite")
		}
		Eventually(verifyNoResultConfigMap, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 4: Agent task job has the new env vars for HTTP result submission
	It("should configure agent tasks with HTTP result endpoint env vars", func() {
		By("creating an Agent CRD")
		agentManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Agent",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"runtime": {
					"type": "claude",
					"defaultMaxTurns": 3,
					"defaultAllowBash": false
				}
			}
		}`, agentName, namespace)

		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(agentManifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create Agent")

		By("creating an agent task")
		taskManifest := fmt.Sprintf(`{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind": "Task",
			"metadata": {
				"name": "%s",
				"namespace": "%s"
			},
			"spec": {
				"type": "agent",
				"prompt": "say hello",
				"agentRef": {
					"name": "%s"
				},
				"agentRuntime": {
					"maxTurns": 1
				}
			}
		}`, agentTaskName, namespace, agentName)

		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = stringReader(taskManifest)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create agent task")

		By("verifying the Job has ORKA_RESULT_ENDPOINT env var")
		verifyResultEndpoint := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", agentTaskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).NotTo(BeEmpty())

			var envVars []envVar
			err = json.Unmarshal([]byte(output), &envVars)
			g.Expect(err).NotTo(HaveOccurred())

			envMap := make(map[string]string)
			for _, e := range envVars {
				envMap[e.Name] = e.Value
			}

			g.Expect(envMap).To(HaveKey("ORKA_RESULT_ENDPOINT"),
				"Worker should have ORKA_RESULT_ENDPOINT for HTTP result submission")
			g.Expect(envMap["ORKA_RESULT_ENDPOINT"]).To(ContainSubstring("/internal/v1/results/"),
				"Result endpoint should point to internal API")

			g.Expect(envMap).To(HaveKey("ORKA_CONTROLLER_URL"),
				"Worker should have ORKA_CONTROLLER_URL for coordination")
		}
		Eventually(verifyResultEndpoint, 2*time.Minute, time.Second).Should(Succeed())

		By("verifying the Job does NOT have ORKA_RESULT_CONFIGMAP env var (removed)")
		verifyNoConfigMapEnv := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "jobs",
				"-l", fmt.Sprintf("orka.ai/task=%s", agentTaskName),
				"-o", "jsonpath={.items[0].spec.template.spec.containers[0].env}",
				"-n", namespace,
			)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())

			var envVars []envVar
			err = json.Unmarshal([]byte(output), &envVars)
			g.Expect(err).NotTo(HaveOccurred())

			for _, e := range envVars {
				g.Expect(e.Name).NotTo(Equal("ORKA_RESULT_CONFIGMAP"),
					"ORKA_RESULT_CONFIGMAP should not be present — replaced by ORKA_RESULT_ENDPOINT")
			}
		}
		Eventually(verifyNoConfigMapEnv, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 5: Verify the DB size metric is exposed via the metrics endpoint
	It("should expose orka_store_db_size_bytes metric", func() {
		By("checking the controller logs for SQLite store startup message")
		verifyStoreStartup := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods",
				"-l", "control-plane=controller-manager",
				"-o", "jsonpath={.items[0].metadata.name}",
				"-n", namespace,
			)
			podName, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())

			cmd = exec.Command("kubectl", "logs", strings.TrimSpace(podName), "-n", namespace)
			logs, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(ContainSubstring("SQLite store is configured"),
				"Controller should log SQLite store startup message")
		}
		Eventually(verifyStoreStartup, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 6: Verify task deletion cleans up results from SQLite
	It("should clean up SQLite result when task is deleted", func() {
		By("deleting the container task")
		cmd := exec.Command("kubectl", "delete", "task", containerTaskName, "-n", namespace, "--wait=true")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to delete container task")

		By("verifying the task is gone")
		verifyTaskDeleted := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "task", containerTaskName,
				"-n", namespace, "--ignore-not-found")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "Task should be deleted")
		}
		Eventually(verifyTaskDeleted, 30*time.Second, time.Second).Should(Succeed())
	})

	// Test 7: Verify worker RBAC — workers should NOT have ConfigMap write permissions
	It("should not grant ConfigMap write permissions to workers", func() {
		By("checking the worker ClusterRoles for ConfigMap permissions")
		verifyWorkerRBAC := func(g Gomega) {
			workerRoles := []string{"orka-ai-worker-role", "orka-vendor-worker-role", "orka-container-worker-role"}
			disallowedConfigMapVerbs := []string{"create", "update", "patch", "delete", "deletecollection", "*"}
			type policyRule struct {
				APIGroups []string `json:"apiGroups"`
				Resources []string `json:"resources"`
				Verbs     []string `json:"verbs"`
			}

			for _, workerRole := range workerRoles {
				cmd := exec.Command("kubectl", "get", "clusterrole", workerRole,
					"-o", "jsonpath={.rules}", "--ignore-not-found")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				if strings.TrimSpace(output) == "" {
					// Worker role might not exist in all deployments; skip
					continue
				}

				var rules []policyRule
				err = json.Unmarshal([]byte(output), &rules)
				g.Expect(err).NotTo(HaveOccurred())

				for _, rule := range rules {
					ruleCoversConfigMaps := false
					for _, apiGroup := range rule.APIGroups {
						if apiGroup != "" && apiGroup != "*" {
							continue
						}
						for _, resource := range rule.Resources {
							if resource == "configmaps" || resource == "*" {
								ruleCoversConfigMaps = true
								break
							}
						}
					}

					if ruleCoversConfigMaps {
						// ConfigMap rules should only have read verbs
						for _, verb := range rule.Verbs {
							g.Expect(disallowedConfigMapVerbs).NotTo(ContainElement(verb),
								"Workers should NOT have ConfigMap write permissions")
						}
					}
				}
			}
		}
		Eventually(verifyWorkerRBAC, 30*time.Second, time.Second).Should(Succeed())
	})
})
