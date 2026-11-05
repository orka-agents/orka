//go:build e2e

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
	"os/exec"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/test/fixtures/fakeentra"
	"github.com/orka-agents/orka/test/utils"
)

const (
	fakeEntraImage    = "ghcr.io/orka-agents/orka/e2e-fake-entra:e2e"
	fakeEntraName     = "e2e-fake-entra"
	fakeEntraTrust    = "e2e-fake-entra-trust"
	fakeEntraTenant   = "0e2e0000-0000-4000-8000-000000000001"
	fakeEntraBlueprnt = "0e2e0000-0000-4000-8000-000000000002"
	fakeEntraIdentity = "0e2e0000-0000-4000-8000-000000000003"
	fakeEntraResource = "api://orka-e2e-identity-probe"
	// The controller caches a token until a minute before it expires, so a
	// 70s lifetime keeps each one for 10s and a disabled identity is noticed
	// on the next request.
	fakeEntraTokenLifetime = 70 * time.Second
	fakeEntraTrustDir      = "/etc/orka-e2e-entra"

	agentIdentityE2EAgent  = "e2e-identity-agent"
	agentIdentityE2EPolicy = "e2e-identity-as-agent"
	agentIdentityE2ETool   = "e2eidentityprobe"
)

// The Agent identity path end to end with a fake Entra: the controller is
// configured with an Entra issuer, verifies an Agent's identity, binds it to
// a Task, and the ACP broker calls a Tool with the identity's own token. The
// fake's resource accepts only tokens the fake issued for it, so a passing
// probe proves the whole chain; disabling the identity at the fake proves
// verification and calls stop.
var _ = Describe("Agent identity with a fake Entra", Ordered, Serial, Label("agent-runtime", "agent-identity"), func() {
	var apiBaseURL, apiToken string

	BeforeAll(func() {
		if e2eLiveCopilotProxyBaseURL != "" || e2eFlagEnabled(mockLLMDisableEnvVar) {
			Skip("the Agent identity spec scripts the runtime through aimock, never a live provider")
		}
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(providerProxyMockLLM.delete)
		providerProxyMockLLM.deploy(projectDir)

		By("deploying the fake Entra behind a serving certificate from a fresh test CA")
		assertion := "fx-controller-credential-" + fmt.Sprint(time.Now().UnixNano())
		caPEM := deployFakeEntra(assertion)
		DeferCleanup(deleteE2EObjects, "secret", fakeEntraTrust)
		Expect(createK8sSecret(fakeEntraTrust, namespace, map[string]string{
			"ca.crt": string(caPEM), "assertion": assertion,
		})).To(Succeed())

		By("configuring the controller with the fake Entra as its agent identity issuer")
		configureControllerForFakeEntra()

		var cancel context.CancelFunc
		var pf *exec.Cmd
		apiBaseURL, cancel, pf, err = startControllerAPIPortForward(18141)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { stopPortForward(cancel, pf) })
		apiToken, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		By("declaring the identity policy, the probe Tool, and the identity-bound Agent")
		DeferCleanup(deleteE2EObjects, "outboundaccesspolicy", agentIdentityE2EPolicy)
		Expect(applyManifestJSON(map[string]any{
			"apiVersion": corev1alpha1.GroupVersion.String(), "kind": "OutboundAccessPolicy",
			"metadata": map[string]any{"name": agentIdentityE2EPolicy, "namespace": namespace},
			"spec": map[string]any{"agentIdentity": map[string]any{
				"mode": "Self", "entra": map[string]any{"resource": fakeEntraResource},
			}},
		})).To(Succeed())
		DeferCleanup(deleteE2EObjects, "tool", agentIdentityE2ETool)
		Expect(applyManifestJSON(map[string]any{
			"apiVersion": corev1alpha1.GroupVersion.String(), "kind": "Tool",
			"metadata": map[string]any{"name": agentIdentityE2ETool, "namespace": namespace},
			"spec": map[string]any{
				"description":       "Ask the fixture resource which agent identity called it.",
				"brokeredToolClass": "read",
				"parameters": map[string]any{
					"type": "object", "properties": map[string]any{}, "additionalProperties": false,
				},
				"http": map[string]any{
					"url":                     "https://" + fakeEntraHost() + fakeentra.ProbePath,
					"method":                  "GET",
					"outboundAccessPolicyRef": map[string]any{"name": agentIdentityE2EPolicy},
				},
			},
		})).To(Succeed())
		DeferCleanup(deleteE2EObjects, "agent", agentIdentityE2EAgent)
		agent := runtimeAgentManifest(agentIdentityE2EAgent, "claude", runtimeMatrixMockClaudeModel, 4, nil)
		tools := append(acp.BuiltInRuntimeNativeToolNames("claude"), agentIdentityE2ETool)
		spec := agent["spec"].(map[string]any)
		spec["identity"] = map[string]any{"entra": map[string]any{"agentIdentityID": fakeEntraIdentity}}
		runtime := spec["runtime"].(map[string]any)
		runtime["defaultAllowedTools"] = tools
		runtime["ceiling"] = map[string]any{"allowedTools": tools}
		Expect(applyManifestJSON(agent)).To(Succeed())
	})

	// runTask runs one scripted Task to a terminal phase and returns it.
	runTask := func(name, marker string) string {
		DeferCleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cleanup, err := newE2ECleanup(apiBaseURL, apiToken)
			Expect(err).NotTo(HaveOccurred())
			Expect(cleanup.tasks(ctx, []string{name}, false)).To(Succeed())
		})
		Expect(applyManifestJSON(runtimeAgentTaskManifest(name, agentIdentityE2EAgent,
			marker+" Call the "+agentIdentityE2ETool+" tool once and report what it returns.",
			4, nil, nil, "", nil, nil))).To(Succeed())
		return waitForTaskCompletion(name, 8*time.Minute)
	}

	It("verifies the identity and calls the Tool with the identity's own token", func() {
		By("waiting for the Agent to claim the identity and the issuer to verify it")
		Eventually(func(g Gomega) {
			var agent corev1alpha1.Agent
			g.Expect(getE2EJSON(&agent, "agent", agentIdentityE2EAgent)).To(Succeed())
			g.Expect(agent.Status.IdentityClaim).NotTo(BeNil())
			g.Expect(agent.Status.IdentityClaim.ID).To(Equal(fakeEntraIdentity))
			g.Expect(agentCondition(agent, "IdentityVerified")).To(HaveField("Status", BeEquivalentTo("True")))
		}, 2*time.Minute, time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			var tool corev1alpha1.Tool
			g.Expect(getE2EJSON(&tool, "tool", agentIdentityE2ETool)).To(Succeed())
			g.Expect(tool.Status.Available).To(BeTrue(), "the private fixture Tool must be admitted under the allowance")
		}, 2*time.Minute, time.Second).Should(Succeed())
		verified := fakeEntraState()
		Expect(verified.Requests).To(ContainElement(fakeentra.Request{
			Step: "exchange", ClientID: fakeEntraBlueprnt, Identity: fakeEntraIdentity, Scope: fakeentra.ExchangeScope,
		}), "the blueprint must present the controller's credential for this identity")
		Expect(verified.Requests).To(ContainElement(fakeentra.Request{
			Step: "app", ClientID: fakeEntraIdentity, Identity: fakeEntraIdentity, Scope: "api://AzureADTokenExchange/.default",
		}), "verification must ask for the identity's own token")

		By("running a Task whose model calls the identity-backed Tool")
		before, err := providerProxyMockLLM.evalSnapshot()
		Expect(err).NotTo(HaveOccurred())
		marker := "[e2e:agent-identity-self]"
		Expect(runTask("e2e-identity-self", marker)).To(Equal("Succeeded"))

		state := fakeEntraState()
		Expect(state.Probes).To(Equal([]fakeentra.Probe{{
			Authorized: true, Identity: fakeEntraIdentity, Resource: fakeEntraResource,
		}}), "the resource must see exactly one call, carrying the identity's token for it")
		Expect(state.Requests).To(ContainElement(fakeentra.Request{
			Step: "app", ClientID: fakeEntraIdentity, Identity: fakeEntraIdentity, Scope: fakeEntraResource + "/.default",
		}))
		var requests []mockLLMEvalRequest
		Eventually(func(g Gomega) {
			var captureErr error
			requests, captureErr = providerProxyMockLLM.evalRequestsSince(before, marker)
			g.Expect(captureErr).NotTo(HaveOccurred())
			g.Expect(authorizationToolResultSeen(requests, "mcp__orka__"+agentIdentityE2ETool, fakeentra.ProbeOK)).
				To(BeTrue(), "the probe's answer must reach the model as the tool result")
		}, time.Minute, time.Second).Should(Succeed())
		canaries := map[string]string{}
		for index, token := range state.Issued {
			canaries[fmt.Sprintf("entra-token-%d", index)] = token
		}
		Expect(checkModelContentCanaries(requests, canaries)).To(Succeed(), "no Entra token may reach the model")
	})

	It("stops verifying and calling once the identity is disabled", func() {
		_, err := utils.Run(exec.Command("kubectl", "create", "--raw",
			fakeEntraProxyPath("/fixture/identities/"+fakeEntraIdentity+"/disable"), "-f", "-"))
		Expect(err).NotTo(HaveOccurred())
		// Let the cached verification token lapse, then make the Agent
		// re-verify; an annotation change reconciles it.
		time.Sleep(fakeEntraTokenLifetime - time.Minute + 2*time.Second)
		_, err = utils.Run(exec.Command("kubectl", "annotate", "agent", agentIdentityE2EAgent, "-n", namespace,
			"--overwrite", "e2e.orka.ai/reverify="+fmt.Sprint(time.Now().UnixNano())))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			var agent corev1alpha1.Agent
			g.Expect(getE2EJSON(&agent, "agent", agentIdentityE2EAgent)).To(Succeed())
			condition := agentCondition(agent, "IdentityVerified")
			g.Expect(condition).To(HaveField("Status", BeEquivalentTo("False")))
			g.Expect(condition.Message).To(ContainSubstring(fmt.Sprintf("AADSTS%d", fakeentra.DisabledErrorCode)))
		}, 2*time.Minute, time.Second).Should(Succeed())

		probesBefore := len(fakeEntraState().Probes)
		// The refusal reaches the model as a failed tool call; whatever the
		// Task's outcome, the resource must not have been called.
		runTask("e2e-identity-disabled", "[e2e:agent-identity-disabled]")
		state := fakeEntraState()
		Expect(state.Probes).To(HaveLen(probesBefore), "a disabled identity's call must never reach the resource")
		Expect(state.Requests).To(ContainElement(fakeentra.Request{
			Step: "app", ClientID: fakeEntraIdentity, Identity: fakeEntraIdentity, Scope: fakeEntraResource + "/.default",
			Error: "unauthorized_client", ErrorCode: fakeentra.DisabledErrorCode,
		}), "the call must have been refused at the issuer")
	})
})

func fakeEntraHost() string { return fmt.Sprintf("%s.%s.svc", fakeEntraName, namespace) }

func fakeEntraProxyPath(suffix string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services/https:%s:443/proxy%s", namespace, fakeEntraName, suffix)
}

func fakeEntraState() fakeentra.State {
	output, err := exec.Command("kubectl", "get", "--raw", fakeEntraProxyPath("/fixture/state")).Output()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var state fakeentra.State
	ExpectWithOffset(1, json.Unmarshal(output, &state)).To(Succeed())
	return state
}

func getE2EJSON(out any, kind, name string) error {
	output, err := exec.Command("kubectl", "get", kind, name, "-n", namespace, "-o", "json").Output()
	if err != nil {
		return err
	}
	return json.Unmarshal(output, out)
}

func agentCondition(agent corev1alpha1.Agent, conditionType string) *metav1.Condition {
	for i := range agent.Status.Conditions {
		if agent.Status.Conditions[i].Type == conditionType {
			return &agent.Status.Conditions[i]
		}
	}
	return nil
}

// deployFakeEntra serves the fake on port 443 behind a serving certificate
// from a fresh test CA, and returns that CA.
func deployFakeEntra(assertion string) []byte {
	caPEM, certPEM, keyPEM := newFakeForgeTLS(fakeEntraHost())
	DeferCleanup(deleteE2EObjects, "deployment,service,secret", fakeEntraName)
	ExpectWithOffset(1, createK8sSecret(fakeEntraName, namespace, map[string]string{
		"tls.crt": string(certPEM), "tls.key": string(keyPEM), "assertion": assertion,
	})).To(Succeed())
	selector := map[string]any{"app.kubernetes.io/name": fakeEntraName}
	ExpectWithOffset(1, applyManifestJSON(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{
		map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": fakeEntraName, "namespace": namespace},
			"spec": map[string]any{
				"replicas": 1, "selector": map[string]any{"matchLabels": selector},
				"template": map[string]any{"metadata": map[string]any{"labels": selector}, "spec": map[string]any{
					"automountServiceAccountToken": false,
					"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532,
						"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
					"containers": []any{map[string]any{
						"name": "entra", "image": fakeEntraImage, "imagePullPolicy": "IfNotPresent",
						"args": []string{
							"--tenant=" + fakeEntraTenant, "--blueprint=" + fakeEntraBlueprnt,
							"--identities=" + fakeEntraIdentity, "--resource=" + fakeEntraResource,
							"--assertion-file=/etc/fakeentra/assertion", "--token-lifetime=" + fakeEntraTokenLifetime.String(),
							"--tls-cert=/etc/fakeentra/tls.crt", "--tls-key=/etc/fakeentra/tls.key",
						},
						"ports": []any{map[string]any{"name": "https", "containerPort": 8443}},
						"readinessProbe": map[string]any{"periodSeconds": 2,
							"httpGet": map[string]any{"path": "/healthz", "port": "https", "scheme": "HTTPS"}},
						"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": map[string]any{"drop": []string{"ALL"}}},
						"volumeMounts": []any{map[string]any{"name": "config", "mountPath": "/etc/fakeentra", "readOnly": true}},
					}},
					"volumes": []any{map[string]any{"name": "config", "secret": map[string]any{"secretName": fakeEntraName}}},
				}},
			},
		},
		map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": fakeEntraName, "namespace": namespace},
			"spec": map[string]any{"selector": selector,
				"ports": []any{map[string]any{"name": "https", "port": 443, "targetPort": "https"}}},
		},
	}})).To(Succeed())
	_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+fakeEntraName,
		"-n", namespace, "--timeout=3m"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "the fake Entra did not become ready")
	return caPEM
}

// configureControllerForFakeEntra points the controller's Entra issuer at the
// fake, trusts the fake's CA, mounts the credential the fake expects, and
// turns on what identity-bound Agents need. The fixture-only private-endpoint
// allowance lets the identity-backed Tool reach the in-cluster resource; it
// needs a plain-http localhost callback base, which nothing here uses. The
// original pod template is restored when the spec ends.
func configureControllerForFakeEntra() {
	name, err := controllerManagerDeploymentName()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var deployment appsv1.Deployment
	ExpectWithOffset(1, getE2EJSON(&deployment, "deployment", name)).To(Succeed())
	podSpec := deployment.Spec.Template.Spec
	index := slices.IndexFunc(podSpec.Containers, func(c corev1.Container) bool { return c.Name == "manager" })
	ExpectWithOffset(1, index).To(BeNumerically(">=", 0))
	manager := podSpec.Containers[index]
	original := []any{manager.Args, manager.Env, manager.VolumeMounts, podSpec.Volumes}

	args := authorizationReplaceFlags(manager.Args, map[string]string{
		"agent-identity-entra-tenant-id":           fakeEntraTenant,
		"agent-identity-entra-blueprint-client-id": fakeEntraBlueprnt,
		"agent-identity-entra-authority":           "https://" + fakeEntraHost(),
		"agent-identity-token-file":                fakeEntraTrustDir + "/assertion",
		"agent-use-authorization-enabled":          "true",
		"connectors-allow-private-endpoints":       "i-understand-tokens-may-leave-the-cluster",
		"connector-callback-base-url":              "http://localhost:18080",
	})
	// SSL_CERT_DIR adds the fake's CA to the system roots; the default CA
	// bundle file is still read.
	env := append(slices.Clone(manager.Env), corev1.EnvVar{Name: "SSL_CERT_DIR", Value: fakeEntraTrustDir + "/ca"})
	mounts := append(slices.Clone(manager.VolumeMounts),
		corev1.VolumeMount{Name: fakeEntraTrust, MountPath: fakeEntraTrustDir, ReadOnly: true})
	volumes := append(slices.Clone(podSpec.Volumes), corev1.Volume{Name: fakeEntraTrust, VolumeSource: corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{SecretName: fakeEntraTrust, Items: []corev1.KeyToPath{
			{Key: "ca.crt", Path: "ca/ca.crt"}, {Key: "assertion", Path: "assertion"},
		}},
	}})
	patch := func(values []any) {
		container := fmt.Sprintf("/spec/template/spec/containers/%d", index)
		paths := []string{container + "/args", container + "/env", container + "/volumeMounts", "/spec/template/spec/volumes"}
		ops := make([]map[string]any, 0, len(paths)+1)
		ops = append(ops, map[string]any{"op": "test", "path": "/metadata/name", "value": name})
		for i, path := range paths {
			ops = append(ops, map[string]any{"op": "add", "path": path, "value": values[i]})
		}
		body, err := json.Marshal(ops)
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		cmd := exec.Command("kubectl", "patch", "deployment", name, "-n", namespace, "--type=json", "--patch-file=/dev/stdin")
		cmd.Stdin = bytes.NewReader(body)
		_, err = utils.Run(cmd)
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+name, "-n", namespace,
			"--timeout="+controllerRolloutTimeout(name).String()))
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
	}
	DeferCleanup(func() { patch(original) })
	patch([]any{args, env, mounts, volumes})
}
