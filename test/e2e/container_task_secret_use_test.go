//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/orka-agents/orka/test/utils"
)

const (
	secretUseE2ESecret    = "e2e-secret-use-bot-key"
	secretUseE2EAuthor    = "e2e-secret-use-author"
	secretUseE2EWriter    = "e2e-secret-use-writer"
	secretUseE2ETask      = "e2e-secret-use-task"
	secretUseE2EScheduled = "e2e-secret-use-scheduled"
	// secretUseE2EPropagation outlasts the controller's window for API
	// servers to load the policy before it trusts a Task's admission.
	secretUseE2EPropagation = 35 * time.Second
)

// The container Task Secret use mode lets a Task whose writer holds `use` on a
// Secret receive it, and refuses everyone else, including Orka's own worker
// identity. It restarts the controller, so it runs alone.
var _ = Describe("Container Task Secret use", Ordered, Serial,
	Label("agent-runtime", "container-task-secret-use"), func() {
		var token string

		asUser := func(account string) string {
			return "system:serviceaccount:" + namespace + ":" + account
		}
		taskManifest := func(name string, extra map[string]any) map[string]any {
			spec := map[string]any{
				"type":      "container",
				"image":     "busybox:latest",
				"command":   []string{"sh", "-c", `test "$(cat /secrets/task/token)" = "$BOT_TOKEN" && echo delivered`},
				"secretRef": map[string]any{"name": secretUseE2ESecret},
				"env": []any{map[string]any{"name": "BOT_TOKEN", "valueFrom": map[string]any{
					"secretKeyRef": map[string]any{"name": secretUseE2ESecret, "key": "token"},
				}}},
			}
			maps.Copy(spec, extra)
			return map[string]any{
				"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
				"metadata": map[string]any{"name": name, "namespace": namespace},
				"spec":     spec,
			}
		}
		applyAs := func(user string, manifest any) (string, error) {
			body, err := json.Marshal(manifest)
			Expect(err).NotTo(HaveOccurred())
			cmd := exec.Command("kubectl", "apply", "--as="+user, "-f", "-")
			cmd.Stdin = bytes.NewReader(body)
			return utils.Run(cmd)
		}

		BeforeAll(func() {
			projectDir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			name, err := controllerManagerDeploymentName()
			Expect(err).NotTo(HaveOccurred())
			var deployment appsv1.Deployment
			Expect(getE2EJSON(&deployment, "deployment", name)).To(Succeed())

			By("installing the admission policy for this controller's ServiceAccount")
			canonical, err := os.ReadFile(filepath.Join(projectDir,
				"config", "container-task-secret-use-policy", "container_task_secret_use_policy.yaml"))
			Expect(err).NotTo(HaveOccurred())
			policy := strings.NewReplacer("ORKA_NAMESPACE", namespace,
				"CONTROLLER_SA", deployment.Spec.Template.Spec.ServiceAccountName).Replace(string(canonical))
			DeferCleanup(func() {
				cmd := exec.Command("kubectl", "delete", "--ignore-not-found", "-f", "-")
				cmd.Stdin = strings.NewReader(policy)
				_, _ = utils.Run(cmd)
			})
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(policy)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("turning the mode on in the controller")
			index := slices.IndexFunc(deployment.Spec.Template.Spec.Containers,
				func(c corev1.Container) bool { return c.Name == "manager" })
			Expect(index).To(BeNumerically(">=", 0))
			original := deployment.Spec.Template.Spec.Containers[index].Args
			patchArgs := func(args []string) {
				ops := []map[string]any{
					{"op": "test", "path": "/metadata/name", "value": name},
					{"op": "add", "path": fmt.Sprintf("/spec/template/spec/containers/%d/args", index), "value": args},
				}
				body, err := json.Marshal(ops)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				cmd := exec.Command("kubectl", "patch", "deployment", name, "-n", namespace,
					"--type=json", "--patch-file=/dev/stdin")
				cmd.Stdin = bytes.NewReader(body)
				_, err = utils.Run(cmd)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+name, "-n", namespace,
					"--timeout="+controllerRolloutTimeout(name).String()))
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
			}
			DeferCleanup(func() { patchArgs(original) })
			patchArgs(authorizationReplaceFlags(original, map[string]string{
				"container-task-secret-use-authorization-enabled": "true",
			}))

			By("creating the Secret, an author who may use it, and a Task writer who may not")
			raw := make([]byte, 16)
			_, err = rand.Read(raw)
			Expect(err).NotTo(HaveOccurred())
			token = "e2e-" + hex.EncodeToString(raw)
			DeferCleanup(deleteE2EObjects, "secret", secretUseE2ESecret)
			Expect(createK8sSecret(secretUseE2ESecret, namespace, map[string]string{"token": token})).To(Succeed())
			for _, account := range []string{secretUseE2EAuthor, secretUseE2EWriter} {
				rules := []any{map[string]any{
					"apiGroups": []string{"core.orka.ai"}, "resources": []string{"tasks"},
					"verbs": []string{"get", "list", "create", "update", "patch", "delete"},
				}}
				if account == secretUseE2EAuthor {
					rules = append(rules, map[string]any{
						"apiGroups": []string{""}, "resources": []string{"secrets"},
						"verbs": []string{"use"}, "resourceNames": []string{secretUseE2ESecret},
					})
				}
				DeferCleanup(deleteE2EObjects, "serviceaccount,role,rolebinding", account)
				for _, object := range []map[string]any{
					{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": account, "namespace": namespace}},
					{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
						"metadata": map[string]any{"name": account, "namespace": namespace}, "rules": rules},
					{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
						"metadata": map[string]any{"name": account, "namespace": namespace},
						"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": account},
						"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": account, "namespace": namespace}}},
				} {
					Expect(applyManifestJSON(object)).To(Succeed())
				}
			}

			By("waiting out the window in which the controller does not yet trust the new policy")
			time.Sleep(secretUseE2EPropagation)
		})

		AfterEach(func() {
			dumpDebugInfo(secretUseE2ETask, secretUseE2EScheduled)
		})

		It("delivers the Secret as a file and an env value to a Task its author may use", func() {
			DeferCleanup(deleteE2EObjects, "task", secretUseE2ETask)
			_, err := applyAs(asUser(secretUseE2EAuthor), taskManifest(secretUseE2ETask, nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForTaskCompletion(secretUseE2ETask, 3*time.Minute)).To(Equal("Succeeded"),
				"the command succeeds only if both the file and the env value hold the Secret")

			By("refusing a writer without use who changes the image, even with the same Secrets")
			_, err = utils.Run(exec.Command("kubectl", "patch", "task", secretUseE2ETask, "-n", namespace,
				"--as="+asUser(secretUseE2EWriter), "--type=merge", "-p", `{"spec":{"image":"busybox:1.36"}}`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not authorized to use a Secret"))
		})

		It("refuses a writer who may create Tasks but not use the Secret", func() {
			_, err := applyAs(asUser(secretUseE2EWriter), taskManifest(secretUseE2ETask, nil))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not authorized to use a Secret"))
		})

		It("refuses a Task an agent writes with the AI worker identity", func() {
			_, err := applyAs(asUser("orka-ai-worker"), taskManifest(secretUseE2ETask, nil))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not authorized to use a Secret"))
		})

		It("delivers the Secret to the runs of a scheduled Task its author may use", func() {
			DeferCleanup(deleteE2EObjects, "task", secretUseE2EScheduled)
			_, err := applyAs(asUser(secretUseE2EAuthor), taskManifest(secretUseE2EScheduled, map[string]any{
				"schedule": "* * * * *", "concurrencyPolicy": "Forbid",
			}))
			Expect(err).NotTo(HaveOccurred())

			var run string
			Eventually(func(g Gomega) {
				output, err := utils.Run(exec.Command("kubectl", "get", "tasks", "-n", namespace,
					"-l", "orka.ai/scheduled-run=true,orka.ai/parent-task="+secretUseE2EScheduled,
					"-o", "jsonpath={.items[0].metadata.name}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				run = output
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
			Expect(waitForTaskCompletion(run, 3*time.Minute)).To(Equal("Succeeded"))
		})

		It("keeps the Secret value out of Task status, events, and controller logs", func() {
			output, err := utils.Run(exec.Command("kubectl", "get", "tasks", "-n", namespace, "-o", "json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(output).NotTo(ContainSubstring(token))
			output, err = utils.Run(exec.Command("kubectl", "get", "events", "-n", namespace, "-o", "json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(output).NotTo(ContainSubstring(token))
			name, err := controllerManagerDeploymentName()
			Expect(err).NotTo(HaveOccurred())
			output, err = utils.Run(exec.Command("kubectl", "logs", "deployment/"+name, "-n", namespace,
				"-c", "manager", "--tail=-1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(output).NotTo(ContainSubstring(token))
			Expect(output).To(ContainSubstring("delivering Secrets to container Task under the Secret use policy"))
		})
	})
