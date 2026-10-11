//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/test/utils"
)

// The fake forge serves real Git smart HTTP and the GitHub REST subset the
// clean-room publisher calls (test/fixtures/fakeforge), so the default lane
// exercises clone, CAS push, verification, and pull request reconciliation
// without github.com. The ACP runtime's model is the aimock stand-in for
// Vekil behind the production provider proxy.
const (
	fakeForgeName       = "e2e-fakeforge"
	fakeForgeImage      = "ghcr.io/orka-agents/orka/e2e-fakeforge:e2e"
	fakeForgeRepository = "e2e/source"
	// Fixture credential accepted only by the fake forge.
	fakeForgeToken         = "e2e-fakeforge-token-0123456789"
	fakeForgeCredential    = "e2e-fakeforge-credential"
	fakeForgeCABundle      = "e2e-fakeforge-ca-bundle"
	fakeForgeNetworkPolicy = "e2e-fakeforge-publisher-egress"
	publisherDeployment    = "orka-workspace-publisher"
	publisherCABundlePath  = "/etc/ssl/certs/ca-certificates.crt"
)

func fakeForgeHost() string {
	return fakeForgeName + "." + namespace + ".svc.cluster.local"
}

var _ = Describe("Clean-room publication against a fake forge", Ordered, func() {
	const (
		agentName    = "e2e-forge-codex"
		taskName     = "e2e-forge-write"
		marker       = "[e2e:forge-write]"
		pushBranch   = "orka/e2e-forge"
		codexModel   = "gpt-5.4-mini"
		writtenFile  = "E2E_FORGE.md"
		writtenLine  = "Published by the scripted model."
		forgeTimeout = 10 * time.Minute
	)

	var (
		apiBaseURL string
		cancelPF   context.CancelFunc
		pfCmd      *exec.Cmd
		token      string
	)

	BeforeAll(func() {
		if e2eLiveCopilotProxyBaseURL != "" {
			Skip("a live Vekil serves vekil-system; the fake forge spec scripts its own")
		}
		if e2eFlagEnabled(mockLLMDisableEnvVar) {
			Skip("Skipping: " + mockLLMDisableEnvVar + " is set")
		}
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())

		// Each cleanup is registered only once its resource exists, so a
		// skipped run never touches a live Vekil in vekil-system.
		By("deploying the fake forge with a test CA")
		DeferCleanup(deleteE2EObjects, "deployment,service,secret", fakeForgeName)
		caPEM := deployFakeForge()

		By("letting the publisher trust and reach the fake forge")
		DeferCleanup(deleteE2EObjects, "networkpolicy", fakeForgeNetworkPolicy)
		trustFakeForgeInPublisher(caPEM)

		By("deploying aimock as the Vekil stand-in behind the provider proxy")
		DeferCleanup(providerProxyMockLLM.delete)
		providerProxyMockLLM.deploy(projectDir)

		apiBaseURL, cancelPF, pfCmd, err = startControllerAPIPortForward(18091)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { stopPortForward(cancelPF, pfCmd) })
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		dumpDebugInfo(taskName)
		if CurrentSpecReport().Failed() {
			for _, args := range [][]string{
				{"logs", "deployment/" + publisherDeployment, "-n", namespace, "--tail=100"},
				{"logs", "deployment/" + fakeForgeName, "-n", namespace, "--tail=100"},
			} {
				output, _ := utils.Run(exec.Command("kubectl", args...))
				_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s\n%s\n", strings.Join(args, " "), output)
			}
		}
	})

	It("clones with a read credential, publishes the scripted change, and opens a pull request", func() {
		repoURL := "https://" + fakeForgeHost() + "/" + fakeForgeRepository + ".git"
		DeferCleanup(deleteE2EObjects, "secret", fakeForgeCredential)
		Expect(createK8sSecret(fakeForgeCredential, namespace, map[string]string{"token": fakeForgeToken})).To(Succeed())

		By("creating a Codex agent and a write Task against the fake forge")
		DeferCleanup(func() { cleanupLiveRuntimeTask(apiBaseURL, token, taskName, agentName) })
		Expect(applyManifestJSON(runtimeAgentManifest(agentName, "codex", codexModel, 6, nil))).To(Succeed())
		credential := map[string]any{"name": fakeForgeCredential, "key": "token"}
		task := runtimeAgentTaskManifest(taskName, agentName,
			"Create "+writtenFile+" containing one line, then report what you wrote. "+marker, 4, nil, nil, "", nil, nil)
		spec := task["spec"].(map[string]any)
		spec["timeout"] = "10m"
		spec["workspace"] = map[string]any{
			"intent":                       "write",
			"gitRepo":                      repoURL,
			"branch":                       "main",
			"readCredentialRef":            credential,
			"publicationGitRepo":           repoURL,
			"publicationReadCredentialRef": credential,
			"publicationCredentialRef":     credential,
			"forgeCredentialRef":           credential,
			"pushBranch":                   pushBranch,
			"prBaseBranch":                 "main",
			"prTitle":                      "E2E fake forge publication",
			"createPR":                     true,
		}
		Expect(applyManifestJSON(task)).To(Succeed())

		Expect(waitForTaskCompletion(taskName, forgeTimeout)).To(Equal("Succeeded"))

		By("verifying the scripted Codex edit ran through the provider proxy")
		Eventually(func(g Gomega) {
			requests, err := providerProxyMockLLM.requestsFor(marker)
			g.Expect(err).NotTo(HaveOccurred())
			var outputs []string
			for _, request := range requests {
				outputs = append(outputs, request.ToolResults("exec_command")...)
			}
			g.Expect(outputs).To(ContainElement(ContainSubstring(writtenLine)))
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("verifying the Task delivered exactly the commit the forge now holds")
		var delivery corev1alpha1.TaskDeliveryStatus
		Eventually(func(g Gomega) {
			output, err := utils.Run(exec.Command("kubectl", "get", "task", taskName, "-n", namespace,
				"-o", "jsonpath={.status.delivery}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(json.Unmarshal([]byte(output), &delivery)).To(Succeed())
			g.Expect(delivery.Outcome).To(Equal(corev1alpha1.TaskDeliveryOutcomeVerifiedExact))
			g.Expect(delivery.PRReceipt).NotTo(BeNil())
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
		Expect(delivery.VerifiedRemoteSHA).NotTo(BeEmpty())
		Expect(delivery.PRReceipt.URL).To(HavePrefix("https://" + fakeForgeHost() + "/" + fakeForgeRepository + "/pull/"))

		state := fakeForgeState()
		Expect(state.Branches[fakeForgeRepository]).To(HaveKeyWithValue(pushBranch, delivery.VerifiedRemoteSHA))
		Expect(state.Pulls).To(ContainElement(SatisfyAll(
			HaveField("HeadBranch", pushBranch),
			HaveField("BaseBranch", "main"),
			HaveField("Title", "E2E fake forge publication"),
		)))
		Expect(fakeForgeFile(pushBranch, writtenFile)).To(Equal(writtenLine + "\n"))

		By("verifying each credential reached only its own forge surface")
		gitRequests, apiRequests := 0, 0
		for _, request := range state.Requests {
			switch {
			case strings.HasPrefix(request.Path, "/"+fakeForgeRepository+".git/"):
				Expect(request.Auth).To(Equal("basic"), "git request %+v", request)
				gitRequests++
			case strings.HasPrefix(request.Path, "/api/v3/"):
				Expect(request.Auth).To(Equal("bearer"), "REST request %+v", request)
				apiRequests++
			}
		}
		Expect(gitRequests).To(BeNumerically(">", 0))
		Expect(apiRequests).To(BeNumerically(">", 0))
	})
})

// deleteE2EObjects deletes the named objects of the given kinds in the test
// namespace.
func deleteE2EObjects(kinds, name string) {
	_, _ = utils.Run(exec.Command("kubectl", "delete", kinds, name, "-n", namespace, "--ignore-not-found"))
}

// deployFakeForge serves the fake forge on port 443 behind a serving
// certificate from a fresh test CA, and returns that CA.
func deployFakeForge() []byte {
	caPEM, certPEM, keyPEM := newFakeForgeTLS(fakeForgeHost())
	ExpectWithOffset(1, createK8sSecret(fakeForgeName, namespace, map[string]string{
		"tls.crt": string(certPEM), "tls.key": string(keyPEM), "token": fakeForgeToken,
	})).To(Succeed())
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: %[1]s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %[1]s
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: forge
          image: %[3]s
          imagePullPolicy: IfNotPresent
          env:
            - {name: FAKEFORGE_HOST, value: %[4]s}
            - {name: FAKEFORGE_ROOT, value: /data/repos}
            - {name: FAKEFORGE_REPOSITORIES, value: %[5]s}
            - {name: FAKEFORGE_TOKEN_FILE, value: /etc/fakeforge/token}
            - {name: FAKEFORGE_TLS_CERT, value: /etc/fakeforge/tls.crt}
            - {name: FAKEFORGE_TLS_KEY, value: /etc/fakeforge/tls.key}
          ports:
            - {name: https, containerPort: 8443}
          readinessProbe:
            httpGet: {path: /healthz, port: https, scheme: HTTPS}
            periodSeconds: 2
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - {name: config, mountPath: /etc/fakeforge, readOnly: true}
            - {name: data, mountPath: /data}
            - {name: tmp, mountPath: /tmp}
      volumes:
        - name: config
          secret:
            secretName: %[1]s
        - {name: data, emptyDir: {}}
        - {name: tmp, emptyDir: {}}
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector:
    app.kubernetes.io/name: %[1]s
  ports:
    - {name: https, port: 443, targetPort: https}
`, fakeForgeName, namespace, fakeForgeImage, fakeForgeHost(), fakeForgeRepository)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+fakeForgeName,
		"-n", namespace, "--timeout=3m"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "fake forge did not become ready")
	return caPEM
}

// trustFakeForgeInPublisher allows the publisher to clone from, push to, and
// reconcile pull requests on the fake forge. The publisher rebuilds Git's
// environment from scratch, so the test CA is appended to the image's system
// bundle, which both libcurl and Go read.
func trustFakeForgeInPublisher(caPEM []byte) {
	cmd := exec.Command("kubectl", "exec", "-n", namespace, "deployment/"+publisherDeployment, "-c", "publisher",
		"--", "cat", publisherCABundlePath)
	bundle, err := cmd.Output()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to read the publisher CA bundle")
	// The system bundle is too large for a command-line literal.
	bundlePath := filepath.Join(GinkgoT().TempDir(), "ca-certificates.crt")
	ExpectWithOffset(1, os.WriteFile(bundlePath, append(bundle, caPEM...), 0o600)).To(Succeed())
	cmd = exec.Command("kubectl", "create", "configmap", fakeForgeCABundle, "-n", namespace,
		"--from-file=ca-certificates.crt="+bundlePath, "--dry-run=client", "-o", "yaml")
	configMap, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(configMap)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	policy := fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %s
  namespace: %s
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: orka
      app.kubernetes.io/component: workspace-publisher
  policyTypes: [Egress]
  egress:
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: %s
      ports:
        - {protocol: TCP, port: 8443}
`, fakeForgeNetworkPolicy, namespace, fakeForgeName)
	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(policy)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	patch := fmt.Sprintf(`{"spec":{"template":{"spec":{
  "containers":[{"name":"publisher",
    "env":[{"name":"ORKA_PUBLISHER_ALLOWED_SCM_HOSTS","value":"github.com,%[1]s"},
           {"name":"ORKA_PUBLISHER_GITHUB_API_BASE_URL","value":"https://%[1]s/api/v3"}],
    "volumeMounts":[{"name":"e2e-fakeforge-ca","mountPath":%[2]q,"subPath":"ca-certificates.crt","readOnly":true}]}],
  "volumes":[{"name":"e2e-fakeforge-ca","configMap":{"name":%[3]q}}]}}}}`,
		fakeForgeHost(), publisherCABundlePath, fakeForgeCABundle)
	_, err = utils.Run(exec.Command("kubectl", "patch", "deployment", publisherDeployment, "-n", namespace,
		"--type=strategic", "-p", patch))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+publisherDeployment,
		"-n", namespace, "--timeout=3m"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "publisher did not roll out with the fake forge trust")
}

// newFakeForgeTLS returns a test CA and a serving certificate for host.
func newFakeForgeTLS(host string) (caPEM, certPEM, keyPEM []byte) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Orka E2E fake forge CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caTemplate, &key.PublicKey, caKey)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(key)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

type fakeForgeObservedState struct {
	Branches map[string]map[string]string `json:"branches"`
	Pulls    []struct {
		Title      string `json:"title"`
		Body       string `json:"body"`
		BaseBranch string `json:"baseBranch"`
		HeadBranch string `json:"headBranch"`
	} `json:"pulls"`
	Requests []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Auth   string `json:"auth"`
		Status int    `json:"status"`
	} `json:"requests"`
}

func fakeForgeProxyPath(suffix string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services/https:%s:443/proxy%s", namespace, fakeForgeName, suffix)
}

func fakeForgeState() fakeForgeObservedState {
	output, err := utils.Run(exec.Command("kubectl", "get", "--raw", fakeForgeProxyPath("/fake/state")))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var state fakeForgeObservedState
	ExpectWithOffset(1, json.Unmarshal([]byte(output), &state)).To(Succeed())
	return state
}

func fakeForgeFile(branch, path string) string {
	output, err := utils.Run(exec.Command("kubectl", "get", "--raw",
		fakeForgeProxyPath("/fake/file?repo="+fakeForgeRepository+"&ref="+branch+"&path="+path)))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return output
}
