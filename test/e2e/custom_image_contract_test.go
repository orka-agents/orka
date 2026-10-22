//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
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

// customImageCurlImage is a small third-party image with sh and curl; it has
// no Orka helper, so everything it delivers goes through the raw contract.
const customImageCurlImage = "curlimages/curl:8.17.0"

// customImageContractScript posts a structured result and one artifact with
// the Pod's callback token, after probing what the token must not reach. The
// probe status codes travel in the result so the spec can assert them.
const customImageContractScript = `set -eu
TOKEN="$(cat "$ORKA_SA_TOKEN_PATH")"
AUTH="Authorization: Bearer $TOKEN"
ARTIFACTS="$ORKA_CONTROLLER_URL/internal/v1/artifacts/$ORKA_TASK_NAMESPACE/$ORKA_TASK_NAME"
code() { curl -s -o /dev/null -w '%{http_code}' "$@" || true; }
head -c 11534336 /dev/zero > /tmp/oversize.bin
NESTED=$(code -X POST -H "$AUTH" --data-binary proof "$ARTIFACTS/nested/proof.txt")
OVERSIZE=$(code -X POST -H "$AUTH" --data-binary @/tmp/oversize.bin "$ARTIFACTS/oversize.bin")
KUBE=$(code -k -H "$AUTH" "https://kubernetes.default.svc/api/v1/namespaces/$ORKA_TASK_NAMESPACE/pods")
PLANS=$(code -H "$AUTH" "$ORKA_CONTROLLER_URL/internal/v1/plans/$ORKA_TASK_NAMESPACE/$ORKA_TASK_NAME")
printf 'custom-image-proof' > /tmp/proof.txt
curl -fsS -X POST -H "$AUTH" -H 'Content-Type: text/plain' --data-binary @/tmp/proof.txt "$ARTIFACTS/proof.txt"
curl -fsS -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  --data "{\"version\":1,\"outcome\":\"completed\",\"summary\":\"custom image posted its own result\",\"data\":{\"nested\":$NESTED,\"oversize\":$OVERSIZE,\"kubernetesAPI\":$KUBE,\"plans\":$PLANS}}" \
  "$ORKA_RESULT_ENDPOINT"
echo custom-image-stdout`

var _ = Describe("Custom image result and artifact contract", Ordered, func() {
	var (
		apiBaseURL     string
		cancelPF       context.CancelFunc
		portForwardCmd *exec.Cmd
		token          string
	)
	taskName := fmt.Sprintf("e2e-custom-image-contract-%d", time.Now().UnixNano())

	BeforeAll(func() {
		var err error
		apiBaseURL, cancelPF, portForwardCmd, err = startControllerAPIPortForward(18140)
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller API port-forward")
		token, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "task", taskName, "-n", namespace, "--ignore-not-found"))
		stopPortForward(cancelPF, portForwardCmd)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDebugInfo(taskName)
		}
	})

	It("posts a structured result and an artifact with only its callback token", func() {
		manifest, err := json.Marshal(map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind":       "Task",
			"metadata":   map[string]any{"name": taskName, "namespace": namespace},
			"spec": map[string]any{
				"type":    "container",
				"image":   customImageCurlImage,
				"command": []string{"sh", "-c"},
				"args":    []string{customImageContractScript},
				"timeout": "5m",
			},
		})
		Expect(err).NotTo(HaveOccurred())
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(string(manifest))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the custom-image Task")

		Expect(waitForTaskCompletion(taskName, 4*time.Minute)).To(Equal("Succeeded"))

		By("reading the structured result the image posted, not its logs")
		raw := fetchTaskResultViaAPI(apiBaseURL, token, taskName)
		Expect(raw).NotTo(ContainSubstring("custom-image-stdout"), "the log fallback replaced the posted result")
		var result struct {
			Version int    `json:"version"`
			Outcome string `json:"outcome"`
			Summary string `json:"summary"`
			Data    struct {
				Nested        int `json:"nested"`
				Oversize      int `json:"oversize"`
				KubernetesAPI int `json:"kubernetesAPI"`
				Plans         int `json:"plans"`
			} `json:"data"`
		}
		Expect(json.Unmarshal([]byte(raw), &result)).To(Succeed(), "result is not the posted JSON: %s", raw)
		Expect(result.Version).To(Equal(1))
		Expect(result.Outcome).To(Equal("completed"))
		Expect(result.Summary).To(Equal("custom image posted its own result"))

		By("checking what the callback token was refused")
		Expect(result.Data.Nested).To(BeNumerically(">=", http.StatusBadRequest), "an artifact name with a slash was accepted")
		Expect(result.Data.Oversize).To(Equal(http.StatusRequestEntityTooLarge), "an artifact over 10 MB was not rejected")
		Expect(result.Data.KubernetesAPI).To(Equal(http.StatusUnauthorized), "the callback token reached the Kubernetes API")
		Expect(result.Data.Plans).To(Equal(http.StatusUnauthorized), "the callback token reached another internal route")

		By("downloading the artifact the image uploaded")
		body, status, err := doAuthorizedJSONRequest(http.MethodGet,
			fmt.Sprintf("%s/api/v1/tasks/%s/artifacts/proof.txt?namespace=%s", apiBaseURL, taskName, namespace), token, "", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(http.StatusOK), body)
		Expect(body).To(Equal("custom-image-proof"))

		listing, status, err := doAuthorizedJSONRequest(http.MethodGet,
			fmt.Sprintf("%s/api/v1/tasks/%s/artifacts?namespace=%s", apiBaseURL, taskName, namespace), token, "", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal(http.StatusOK), listing)
		Expect(listing).NotTo(ContainSubstring("oversize.bin"))
		Expect(listing).NotTo(ContainSubstring("nested"))
	})
})
