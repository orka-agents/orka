//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

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
		var tokenBase64 string
		var tokenDigest string
		var runID string
		var dynamicClient dynamic.Interface
		taskGVR := schema.GroupVersionResource{Group: "core.orka.ai", Version: "v1alpha1", Resource: "tasks"}

		asUser := func(account string) string {
			return "system:serviceaccount:" + namespace + ":" + account
		}
		taskName := func(base string) string {
			return base + "-" + runID
		}
		taskManifest := func(name string, extra map[string]any) map[string]any {
			verifyCmd := fmt.Sprintf(
				`set -eu; f=$(cat /secrets/task/token); test -n "$f" && test "$f" = "$BOT_TOKEN" && `+
					`test "$(printf '%%s' "$f" | sha256sum | awk '{print $1}')" = "%s" && echo delivered`,
				tokenDigest,
			)
			spec := map[string]any{
				"type":      "container",
				"image":     "busybox:latest",
				"command":   []string{"sh", "-c", verifyCmd},
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
		// createAs creates a Task without updating an existing one and registers
		// UID-preconditioned cleanup only if the create succeeded.
		createAs := func(user string, manifest any) (string, error) {
			body, err := json.Marshal(manifest)
			Expect(err).NotTo(HaveOccurred())
			cmd := exec.Command("kubectl", "create", "--as="+user, "-f", "-", "-o", "json")
			cmd.Stdin = bytes.NewReader(body)
			out, err := utils.Run(cmd)
			if err != nil {
				return out, err
			}
			var created struct {
				Metadata struct {
					Name string `json:"name"`
					UID  string `json:"uid"`
				} `json:"metadata"`
			}
			Expect(json.Unmarshal([]byte(out), &created)).To(Succeed())
			if created.Metadata.Name != "" && created.Metadata.UID != "" {
				name := created.Metadata.Name
				uid := types.UID(created.Metadata.UID)
				DeferCleanup(func() {
					opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
					deleteErr := dynamicClient.Resource(taskGVR).Namespace(namespace).Delete(
						context.Background(), name, opts)
					if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
						Expect(deleteErr).NotTo(HaveOccurred(), "failed to delete test Task %s", name)
					}
				})
			}
			return out, nil
		}

		BeforeAll(func() {
			projectDir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			name, err := controllerManagerDeploymentName()
			Expect(err).NotTo(HaveOccurred())
			var deployment appsv1.Deployment
			Expect(getE2EJSON(&deployment, "deployment", name)).To(Succeed())

			cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
			).ClientConfig()
			Expect(err).NotTo(HaveOccurred())
			clientset, err := kubernetes.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
			dynamicClient, err = dynamic.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
			idBytes := make([]byte, 4)
			_, err = rand.Read(idBytes)
			Expect(err).NotTo(HaveOccurred())
			runID = hex.EncodeToString(idBytes)

			By("installing the admission policy for this controller's ServiceAccount")
			const policyName = "container-task-secret-use.orka.ai"
			canonical, err := os.ReadFile(filepath.Join(projectDir,
				"config", "container-task-secret-use-policy", "container_task_secret_use_policy.yaml"))
			Expect(err).NotTo(HaveOccurred())
			policy := strings.NewReplacer("ORKA_NAMESPACE", namespace,
				"CONTROLLER_SA", deployment.Spec.Template.Spec.ServiceAccountName).Replace(string(canonical))
			// Create each cluster-wide resource without overwriting any existing
			// installation, and register cleanup only for objects this run created
			// using an atomic UID precondition on delete. Once the controller mode
			// is enabled, keep the policy installed unless controller rollback succeeds.
			controllerRestored := true
			for doc := range strings.SplitSeq(policy, "\n---\n") {
				doc = strings.TrimSpace(doc)
				if doc == "" {
					continue
				}
				cmd := exec.Command("kubectl", "create", "-f", "-", "-o", "json")
				cmd.Stdin = strings.NewReader(doc)
				createdJSON, createErr := utils.Run(cmd)
				Expect(createErr).NotTo(HaveOccurred(),
					"refusing to overwrite pre-existing %s; run this E2E spec on a cluster without it installed",
					policyName)
				var created struct {
					Kind     string `json:"kind"`
					Metadata struct {
						Name string `json:"name"`
						UID  string `json:"uid"`
					} `json:"metadata"`
				}
				Expect(json.Unmarshal([]byte(createdJSON), &created)).To(Succeed())
				Expect(created.Metadata.UID).NotTo(BeEmpty())
				kind := created.Kind
				resName := created.Metadata.Name
				uid := types.UID(created.Metadata.UID)
				DeferCleanup(func() {
					if !controllerRestored {
						return
					}
					opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
					var deleteErr error
					switch kind {
					case "ValidatingAdmissionPolicy":
						deleteErr = clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies().
							Delete(context.Background(), resName, opts)
					case "ValidatingAdmissionPolicyBinding":
						deleteErr = clientset.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().
							Delete(context.Background(), resName, opts)
					default:
						Fail("unexpected admission resource kind: " + kind)
					}
					if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
						Expect(deleteErr).NotTo(HaveOccurred(), "failed to delete test %s/%s", kind, resName)
					}
				})
			}

			By("turning the mode on in the controller")
			index := slices.IndexFunc(deployment.Spec.Template.Spec.Containers,
				func(c corev1.Container) bool { return c.Name == "manager" })
			Expect(index).To(BeNumerically(">=", 0))
			original := deployment.Spec.Template.Spec.Containers[index].Args
			updated := authorizationReplaceFlags(original, map[string]string{
				"container-task-secret-use-authorization-enabled": "true",
			})
			applyArgsPatch := func(expected, next []string) {
				argsPath := fmt.Sprintf("/spec/template/spec/containers/%d/args", index)
				ops := []map[string]any{
					{"op": "test", "path": "/metadata/name", "value": name},
					{"op": "test", "path": argsPath, "value": expected},
					{"op": "add", "path": argsPath, "value": next},
				}
				body, err := json.Marshal(ops)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				cmd := exec.Command("kubectl", "patch", "deployment", name, "-n", namespace,
					"--type=json", "--patch-file=/dev/stdin")
				cmd.Stdin = bytes.NewReader(body)
				_, err = utils.Run(cmd)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
			}
			waitRollout := func() {
				_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+name, "-n", namespace,
					"--timeout="+controllerRolloutTimeout(name).String()))
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
			}
			applyArgsPatch(original, updated)
			controllerRestored = false
			DeferCleanup(func() {
				applyArgsPatch(updated, original)
				waitRollout()
				controllerRestored = true
			})
			waitRollout()

			By("creating the Secret, an author who may use it, and a Task writer who may not")
			raw := make([]byte, 16)
			_, err = rand.Read(raw)
			Expect(err).NotTo(HaveOccurred())
			token = "e2e-" + hex.EncodeToString(raw)
			tokenBase64 = base64.StdEncoding.EncodeToString([]byte(token))
			sum := sha256.Sum256([]byte(token))
			tokenDigest = hex.EncodeToString(sum[:])
			createdSecret, err := clientset.CoreV1().Secrets(namespace).Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretUseE2ESecret, Namespace: namespace},
				StringData: map[string]string{"token": token},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			secretUID := createdSecret.UID
			DeferCleanup(func() {
				opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &secretUID}}
				deleteErr := clientset.CoreV1().Secrets(namespace).Delete(
					context.Background(), secretUseE2ESecret, opts)
				if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
					Expect(deleteErr).NotTo(HaveOccurred(), "failed to delete test secret %s", secretUseE2ESecret)
				}
			})
			for _, account := range []string{secretUseE2EAuthor, secretUseE2EWriter} {
				rules := []rbacv1.PolicyRule{{
					APIGroups: []string{"core.orka.ai"},
					Resources: []string{"tasks"},
					Verbs:     []string{"get", "list", "create", "update", "patch", "delete"},
				}}
				if account == secretUseE2EAuthor {
					rules = append(rules, rbacv1.PolicyRule{
						APIGroups:     []string{""},
						Resources:     []string{"secrets"},
						Verbs:         []string{"use"},
						ResourceNames: []string{secretUseE2ESecret},
					})
				}
				sa, err := clientset.CoreV1().ServiceAccounts(namespace).Create(context.Background(), &corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: namespace},
				}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				saUID := sa.UID
				DeferCleanup(func() {
					opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &saUID}}
					deleteErr := clientset.CoreV1().ServiceAccounts(namespace).Delete(
						context.Background(), account, opts)
					if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
						Expect(deleteErr).NotTo(HaveOccurred(), "failed to delete test ServiceAccount %s", account)
					}
				})
				role, err := clientset.RbacV1().Roles(namespace).Create(context.Background(), &rbacv1.Role{
					ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: namespace},
					Rules:      rules,
				}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				roleUID := role.UID
				DeferCleanup(func() {
					deleteErr := clientset.RbacV1().Roles(namespace).Delete(context.Background(), account, metav1.DeleteOptions{
						Preconditions: &metav1.Preconditions{UID: &roleUID},
					})
					if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
						Expect(deleteErr).NotTo(HaveOccurred(), "failed to delete test Role %s", account)
					}
				})
				rb, err := clientset.RbacV1().RoleBindings(namespace).Create(context.Background(), &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: namespace},
					RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: account},
					Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: namespace}},
				}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				rbUID := rb.UID
				DeferCleanup(func() {
					deleteErr := clientset.RbacV1().RoleBindings(namespace).Delete(context.Background(), account, metav1.DeleteOptions{
						Preconditions: &metav1.Preconditions{UID: &rbUID},
					})
					if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !apierrors.IsConflict(deleteErr) {
						Expect(deleteErr).NotTo(HaveOccurred(), "failed to delete test RoleBinding %s", account)
					}
				})
			}

			By("waiting out the window in which the controller does not yet trust the new policy")
			time.Sleep(secretUseE2EPropagation)
		})

		assertNoSecretLeak := func(deliveredTask string, taskNames ...string) {
			By("keeping the Secret value out of Task status, events, and controller logs")
			checkClean := func(source, content string) {
				if strings.Contains(content, token) || strings.Contains(content, tokenBase64) {
					Fail("fixture Secret value leaked into " + source)
				}
			}
			args := append([]string{"get", "task"}, taskNames...)
			args = append(args, "-n", namespace, "-o", "json")
			output, err := utils.Run(exec.Command("kubectl", args...))
			if err != nil {
				Fail("failed to read Task JSON for leak check")
			}
			checkClean("Task status/spec", output)
			output, err = utils.Run(exec.Command("kubectl", "get", "events", "-n", namespace, "-o", "json"))
			if err != nil {
				Fail("failed to read namespace events for leak check")
			}
			checkClean("namespace events", output)
			name, err := controllerManagerDeploymentName()
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			output, err = utils.Run(exec.Command("kubectl", "logs", "deployment/"+name, "-n", namespace,
				"-c", "manager", "--all-pods", "--tail=-1"))
			if err != nil {
				Fail("failed to read controller logs for leak check")
			}
			checkClean("controller logs", output)
			matchedDelivery := false
			for line := range strings.SplitSeq(output, "\n") {
				if strings.Contains(line, "delivering Secrets to container Task under the Secret use policy") &&
					strings.Contains(line, deliveredTask) &&
					strings.Contains(line, secretUseE2ESecret) {
					matchedDelivery = true
					break
				}
			}
			ExpectWithOffset(1, matchedDelivery).To(BeTrue(),
				"expected policy delivery log for Task %s naming Secret %s", deliveredTask, secretUseE2ESecret)
		}

		It("delivers the Secret as a file and an env value to a Task its author may use", func() {
			name := taskName(secretUseE2ETask)
			_, err := createAs(asUser(secretUseE2EAuthor), taskManifest(name, nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(waitForTaskCompletion(name, 3*time.Minute)).To(Equal("Succeeded"),
				"the command succeeds only if both the file and the env value hold the Secret")
			assertNoSecretLeak(name, name)

			By("refusing a writer without use who changes the image, even with the same Secrets")
			_, err = utils.Run(exec.Command("kubectl", "patch", "task", name, "-n", namespace,
				"--as="+asUser(secretUseE2EWriter), "--type=merge", "-p", `{"spec":{"image":"busybox:1.36"}}`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not authorized to use a Secret"))
		})

		It("refuses a writer who may create Tasks but not use the Secret", func() {
			for _, tc := range []struct {
				name   string
				suffix string
				drop   string
			}{
				{name: "file secretRef only", suffix: "deny-file", drop: "env"},
				{name: "env secretKeyRef only", suffix: "deny-env", drop: "secretRef"},
			} {
				By("refusing unauthorized writer with " + tc.name)
				manifest := taskManifest(taskName(secretUseE2ETask+"-"+tc.suffix), nil)
				delete(manifest["spec"].(map[string]any), tc.drop)
				_, err := createAs(asUser(secretUseE2EWriter), manifest)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("not authorized to use a Secret"))
			}
		})

		It("refuses a Task an agent writes with the AI worker identity", func() {
			for _, tc := range []struct {
				name   string
				suffix string
				drop   string
			}{
				{name: "file secretRef only", suffix: "worker-file", drop: "env"},
				{name: "env secretKeyRef only", suffix: "worker-env", drop: "secretRef"},
			} {
				By("refusing AI worker identity with " + tc.name)
				manifest := taskManifest(taskName(secretUseE2ETask+"-"+tc.suffix), nil)
				delete(manifest["spec"].(map[string]any), tc.drop)
				_, err := createAs(asUser("orka-ai-worker"), manifest)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("not authorized to use a Secret"))
			}
		})

		It("delivers the Secret to the runs of a scheduled Task its author may use", func() {
			scheduledName := taskName(secretUseE2EScheduled)
			_, err := createAs(asUser(secretUseE2EAuthor), taskManifest(scheduledName, map[string]any{
				"schedule": "* * * * *", "concurrencyPolicy": "Forbid",
			}))
			Expect(err).NotTo(HaveOccurred())

			var run string
			Eventually(func(g Gomega) {
				output, err := utils.Run(exec.Command("kubectl", "get", "tasks", "-n", namespace,
					"-l", "orka.ai/scheduled-run=true,orka.ai/parent-task="+scheduledName,
					"-o", "jsonpath={.items[0].metadata.name}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(BeEmpty())
				run = output
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
			DeferCleanup(deleteE2EObjects, "task", run)
			Expect(waitForTaskCompletion(run, 3*time.Minute)).To(Equal("Succeeded"))
			assertNoSecretLeak(run, scheduledName, run)
		})
	})
