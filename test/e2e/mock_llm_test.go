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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/orka-agents/orka/test/utils"
)

// The suite backs every provider without a configured key with aimock, a
// fixture-driven LLM server (https://github.com/CopilotKit/aimock). Fixtures
// live in test/e2e/testdata/aimock and match on a marker in each spec's
// prompt, so specs that need a model run on every PR without credentials.
const (
	mockLLMName           = "e2e-aimock"
	mockLLMPort           = 4010
	mockLLMImage          = "ghcr.io/copilotkit/aimock:1.43.0@sha256:163ecad18171e7d28752c386f448cc168bae71b2278d6b9e61a50f454860d8ed"
	mockLLMCaptureImage   = "ghcr.io/orka-agents/orka/e2e-aimock-capture:e2e"
	mockLLMAPIKey         = "aimock-e2e-key"
	mockLLMFixtureDir     = "test/e2e/testdata/aimock"
	mockLLMDisableEnvVar  = "E2E_DISABLE_MOCK_LLM"
	mockLLMFixtureVolume  = "fixtures"
	mockLLMFixtureMountAt = "/fixtures"
)

var (
	// Per-run synthetic upstream proxy bearer. Child-local capabilities are
	// different and legitimately reach the runtime; this must not reach inference.
	mockLLMProviderCanary = "orka-e2e-proxy-canary-" + rand.Text()

	// e2eMockOpenAI and e2eMockAnthropic report whether the matching provider
	// is backed by aimock rather than a live key.
	e2eMockOpenAI    bool
	e2eMockAnthropic bool
)

func e2eMockLLMEnabled() bool {
	return e2eMockOpenAI || e2eMockAnthropic
}

// mockLLMInstance locates one deployed aimock. Its Pods and Service carry
// app.kubernetes.io/name=<name>.
type mockLLMInstance struct {
	namespace string
	name      string
	port      int
}

var (
	// suiteMockLLM backs the native Providers.
	suiteMockLLM = mockLLMInstance{namespace: namespace, name: mockLLMName, port: mockLLMPort}
	// providerProxyMockLLM stands in for Vekil behind the ACP provider proxy,
	// whose NetworkPolicy admits only vekil-system Pods named vekil on 1337.
	providerProxyMockLLM = mockLLMInstance{namespace: "vekil-system", name: "vekil", port: 1337}
)

func mockLLMServiceURL() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", mockLLMName, namespace, mockLLMPort)
}

// configureMockLLMCredentials substitutes aimock for each provider that has no
// live key, unless E2E_DISABLE_MOCK_LLM opts out.
func configureMockLLMCredentials() {
	if e2eFlagEnabled(mockLLMDisableEnvVar) {
		return
	}
	if e2eOpenAIAPIKey == "" {
		e2eMockOpenAI = true
		e2eOpenAIAPIKey = mockLLMAPIKey
		e2eOpenAIBaseURL = mockLLMServiceURL() + "/v1"
	}
	if e2eAnthropicAPIKey == "" {
		e2eMockAnthropic = true
		e2eAnthropicAPIKey = mockLLMAPIKey
		e2eAnthropicBaseURL = mockLLMServiceURL()
	}
}

// mockLLMFixtureFiles lists the fixture files in the order aimock loads them,
// which keeps zz-catch-all.json last.
func mockLLMFixtureFiles(projectDir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(projectDir, mockLLMFixtureDir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no aimock fixture files in %s", mockLLMFixtureDir)
	}
	sort.Strings(files)
	return files, nil
}

// deploy installs aimock with every fixture file in mockLLMFixtureDir.
func (m mockLLMInstance) deploy(projectDir string) {
	files, err := mockLLMFixtureFiles(projectDir)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	configMapArgs := []string{"create", "configmap", m.name + "-fixtures", "-n", m.namespace,
		"--dry-run=client", "-o", "yaml"}
	args := []string{
		"--host", "0.0.0.0",
		"--port", fmt.Sprint(m.port + 1),
		"--journal-max", "0",
		"--validate-on-load",
	}
	for _, file := range files {
		configMapArgs = append(configMapArgs, "--from-file="+file)
		// A ConfigMap mount also exposes hidden ..data copies of every file,
		// so pass each file explicitly instead of the mount directory.
		args = append(args, "--fixtures", mockLLMFixtureMountAt+"/"+filepath.Base(file))
	}
	configMap, err := utils.Run(exec.Command("kubectl", configMapArgs...))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to render aimock fixtures")
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(configMap)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to apply aimock fixtures")

	encodedArgs, err := json.Marshal(args)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app.kubernetes.io/name: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: %[1]s
  template:
    metadata:
      labels:
        app.kubernetes.io/name: %[1]s
      annotations:
        e2e.orka.ai/fixtures-revision: %[7]q
        kubectl.kubernetes.io/default-container: aimock
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: aimock
          image: %[3]s
          imagePullPolicy: IfNotPresent
          args: %[4]s
          env:
            # aimock 1.43 otherwise lets a turnIndex fixture match later turns.
            - name: AIMOCK_STRICT_TURN_INDEX
              value: "1"
          readinessProbe:
            httpGet:
              path: /__aimock/health
              port: %[10]d
            periodSeconds: 2
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              memory: 256Mi
          volumeMounts:
            - name: %[6]s
              mountPath: %[8]s
              readOnly: true
            - name: tmp
              mountPath: /tmp
        - name: capture
          image: %[9]s
          imagePullPolicy: IfNotPresent
          args: ["--listen=:%[5]d", "--upstream=http://127.0.0.1:%[10]d"]
          ports:
            - name: http
              containerPort: %[5]d
          readinessProbe:
            httpGet:
              path: /__orka_eval/health
              port: http
            periodSeconds: 2
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: 25m
              memory: 32Mi
            limits:
              memory: 256Mi
      volumes:
        - name: %[6]s
          configMap:
            name: %[1]s-fixtures
        - name: tmp
          emptyDir: {}
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
    - name: http
      port: %[5]d
      targetPort: http
`, m.name, m.namespace, mockLLMImage, string(encodedArgs), m.port, mockLLMFixtureVolume,
		mockLLMFixturesRevision(files), mockLLMFixtureMountAt, mockLLMCaptureImage, m.port+1)
	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy aimock")

	cmd = exec.Command("kubectl", "rollout", "status", "deployment/"+m.name,
		"-n", m.namespace, "--timeout=3m")
	if _, err = utils.Run(cmd); err != nil {
		for _, diag := range [][]string{
			{"describe", "pods", "-l", "app.kubernetes.io/name=" + m.name, "-n", m.namespace},
			{"logs", "deployment/" + m.name, "-n", m.namespace, "--tail=200"},
		} {
			output, _ := utils.Run(exec.Command("kubectl", diag...))
			_, _ = fmt.Fprintf(GinkgoWriter, "aimock diagnostics: kubectl %s\n%s\n", strings.Join(diag, " "), output)
		}
	}
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "aimock did not become ready")
}

// mockLLMFixturesRevision changes whenever a fixture file changes, so a reused
// namespace restarts aimock with the current fixtures.
func mockLLMFixturesRevision(files []string) string {
	digest := sha256.New()
	for _, file := range files {
		data, err := os.ReadFile(file)
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		digest.Write([]byte(filepath.Base(file)))
		digest.Write(data)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16]
}

// mockLLMInjectedFixtures holds fixtures that name resources known only at run
// time, such as generated child Task and Agent names. A fixture replaces an
// earlier one with the same match, so a spec can reuse one scripted step for
// successive children.
var mockLLMInjectedFixtures []json.RawMessage

// injectMockLLMFixtures scripts steps that need generated names. aimock appends
// fixtures posted at run time after its catch-all, so this clears and reloads
// the whole ordered set: fixture files, injected fixtures, then the catch-all.
// Call it while the scripted conversation is parked (for example in a
// wait_for_tasks on a placeholder Task). Other callers, such as a child Task
// that is just starting, can still send a request during the swap; aimock
// answers those with a retryable 429 until the reload completes.
func injectMockLLMFixtures(fixtures ...map[string]any) {
	for _, fixture := range fixtures {
		encoded, err := json.Marshal(fixture)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		matchKey, err := json.Marshal(fixture["match"])
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		replaced := false
		for i, existing := range mockLLMInjectedFixtures {
			var decoded struct {
				Match json.RawMessage `json:"match"`
			}
			if json.Unmarshal(existing, &decoded) == nil && bytes.Equal(decoded.Match, matchKey) {
				mockLLMInjectedFixtures[i] = encoded
				replaced = true
			}
		}
		if !replaced {
			mockLLMInjectedFixtures = append(mockLLMInjectedFixtures, encoded)
		}
	}

	suiteMockLLM.reloadFixtures(mockLLMInjectedFixtures)
}

// injectFixtures installs case-local scripts on either mock instance. Call only
// between cases, when no model request is in flight.
func (m mockLLMInstance) injectFixtures(fixtures ...map[string]any) {
	encoded := make([]json.RawMessage, 0, len(fixtures))
	for _, fixture := range fixtures {
		body, err := json.Marshal(fixture)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		encoded = append(encoded, body)
	}
	m.reloadFixtures(encoded)
}

func (m mockLLMInstance) reloadFixtures(injected []json.RawMessage) {
	projectDir, err := utils.GetProjectDir()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	set, err := mockLLMFixtureSet(projectDir, injected)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	body, err := json.Marshal(map[string]any{"fixtures": set})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	// aimock has no atomic replace. Between the clear and the reload a request
	// matches nothing and gets a 404, which model clients do not retry, so
	// rate-limit every model request (Retry-After: 1) for the whole swap. The
	// control API is not subject to chaos.
	hold := m.controlPath("chaos")
	cmd := exec.Command("kubectl", "create", "--raw", hold, "-f", "-")
	cmd.Stdin = strings.NewReader(`{"rateLimitRate":1}`)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to hold aimock model requests")
	defer func() {
		// Release even after a failed swap so later specs are not throttled.
		_, err := utils.Run(exec.Command("kubectl", "delete", "--raw", hold))
		ExpectWithOffset(2, err).NotTo(HaveOccurred(), "Failed to release aimock model requests")
	}()

	path := m.controlPath("fixtures")
	_, err = utils.Run(exec.Command("kubectl", "delete", "--raw", path))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to clear aimock fixtures")
	cmd = exec.Command("kubectl", "create", "--raw", path, "-f", "-")
	cmd.Stdin = bytes.NewReader(body)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to reload aimock fixtures")

	output, err := utils.Run(exec.Command("kubectl", "get", "--raw", path))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var loaded struct {
		Count int `json:"count"`
	}
	ExpectWithOffset(1, json.Unmarshal([]byte(output), &loaded)).To(Succeed())
	ExpectWithOffset(1, loaded.Count).To(Equal(len(set)), "aimock did not reload every fixture")
}

// mockLLMFixtureSet orders the fixture files' fixtures, then injected, then
// catch-all fixtures.
func mockLLMFixtureSet(projectDir string, injected []json.RawMessage) ([]json.RawMessage, error) {
	files, err := mockLLMFixtureFiles(projectDir)
	if err != nil {
		return nil, err
	}
	var ordered, catchAll []json.RawMessage
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Fixtures []json.RawMessage `json:"fixtures"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil, fmt.Errorf("decode %s: %w", filepath.Base(file), err)
		}
		for _, fixture := range parsed.Fixtures {
			if isMockLLMCatchAll(fixture) {
				catchAll = append(catchAll, fixture)
			} else {
				ordered = append(ordered, fixture)
			}
		}
	}
	ordered = append(ordered, injected...)
	return append(ordered, catchAll...), nil
}

// mockLLMControlPath addresses an aimock control route through the API
// server's Service proxy.
func mockLLMControlPath(route string) string {
	return suiteMockLLM.controlPath(route)
}

func (m mockLLMInstance) controlPath(route string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services/http:%s:%d/proxy/__aimock/%s",
		m.namespace, m.name, m.port, route)
}

func (m mockLLMInstance) delete() error {
	for _, args := range [][]string{
		{"delete", "deployment", m.name, "-n", m.namespace, "--ignore-not-found", "--wait=true", "--timeout=30s"},
		{"delete", "service", m.name, "-n", m.namespace, "--ignore-not-found", "--wait=true", "--timeout=20s"},
		{"delete", "configmap", m.name + "-fixtures", "-n", m.namespace, "--ignore-not-found", "--wait=true", "--timeout=20s"},
	} {
		if err := runBoundedE2ECleanup(45*time.Second, "kubectl", args...); err != nil {
			return err
		}
	}
	return nil
}

type mockLLMToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type mockLLMMessage struct {
	Role       string            `json:"role"`
	Content    json.RawMessage   `json:"content"`
	ToolCalls  []mockLLMToolCall `json:"tool_calls"`
	ToolCallID string            `json:"tool_call_id"`
}

// Text flattens string and multi-part message content.
func (m mockLLMMessage) Text() string {
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Text)
		}
		return b.String()
	}
	return ""
}

type mockLLMRequest struct {
	// RawBody is populated only by complete-capture evals. aimock normalizes
	// protocol shapes and caps its own Body, so it is not the raw-wire oracle.
	RawBody   json.RawMessage `json:"-"`
	RequestID string          `json:"requestId"`
	// Only the public correlation header is decoded; authentication headers
	// retained by aimock itself must not enter these typed receipts.
	Headers struct {
		RequestID string `json:"x-request-id"`
	} `json:"headers"`
	Timestamp int64  `json:"timestamp"`
	Path      string `json:"path"`
	Body      struct {
		// Truncated is set when the body exceeded aimock's 64 KB journal cap
		// and was not recorded.
		Truncated bool             `json:"__aimock_truncated"`
		Model     string           `json:"model"`
		Messages  []mockLLMMessage `json:"messages"`
		Tools     []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	} `json:"body"`
	Response struct {
		Status  int             `json:"status"`
		Fixture json.RawMessage `json:"fixture"`
	} `json:"response"`
}

// Mentions reports whether any user or system message carries marker.
func (r mockLLMRequest) Mentions(marker string) bool {
	if len(r.RawBody) > 0 && strings.Contains(string(r.RawBody), marker) {
		return true
	}
	for _, message := range r.Body.Messages {
		if (message.Role == "user" || message.Role == "system") && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// RoleText joins the text of every message with role.
func (r mockLLMRequest) RoleText(role string) string {
	var b strings.Builder
	for _, message := range r.evalMessages() {
		if message.Role == role {
			b.WriteString(message.Text())
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ToolResults returns, in call order, the outputs the client sent back for
// calls to toolName in this request.
func (r mockLLMRequest) ToolResults(toolName string) []string {
	calls := map[string]string{}
	var results []string
	for _, message := range r.evalMessages() {
		for _, call := range message.ToolCalls {
			calls[call.ID] = call.Function.Name
		}
		if message.Role == "tool" && calls[message.ToolCallID] == toolName {
			results = append(results, message.Text())
		}
	}
	return results
}

// journal reads every request aimock has served from inside its Pod, which
// works behind ingress NetworkPolicies such as the one guarding Vekil.
func (m mockLLMInstance) journal() ([]mockLLMRequest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mockLLMCaptureReadTimeout)
	defer cancel()
	return m.journalWithin(ctx)
}

func (m mockLLMInstance) journalWithin(ctx context.Context) ([]mockLLMRequest, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", m.namespace, "deployment/"+m.name, "-c", "aimock",
		"--", "node", "-e", mockLLMReadScript(m.port, "/__aimock/journal", mockLLMCaptureFetchTimeout))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// Bound inherited pipe waits as well as kubectl itself.
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("read %s/%s journal expired: %w", m.namespace, m.name, ctx.Err())
		}
		return nil, fmt.Errorf("read %s/%s journal: %w: %s", m.namespace, m.name, err, stderr.String())
	}
	var requests []mockLLMRequest
	if err := json.Unmarshal(output, &requests); err != nil {
		return nil, fmt.Errorf("decode aimock journal: %w", err)
	}
	return requests, nil
}

// mockLLMRequestsFor returns the suite aimock's requests that carry marker.
func mockLLMRequestsFor(marker string) ([]mockLLMRequest, error) {
	return suiteMockLLM.requestsFor(marker)
}

// servedFor returns the journal entries aimock answered with a fixture that
// matches promptMarker as its user message. Unlike requestsFor, it also finds
// requests whose body exceeded the journal cap.
func (m mockLLMInstance) servedFor(promptMarker string) ([]mockLLMRequest, error) {
	requests, err := m.journal()
	if err != nil {
		return nil, err
	}
	var served []mockLLMRequest
	for _, request := range requests {
		var fixture struct {
			Match struct {
				UserMessage string `json:"userMessage"`
			} `json:"match"`
		}
		if json.Unmarshal(request.Response.Fixture, &fixture) == nil && fixture.Match.UserMessage == promptMarker {
			served = append(served, request)
		}
	}
	return served, nil
}

// requestsFor returns the journal entries whose conversation carries marker.
func (m mockLLMInstance) requestsFor(marker string) ([]mockLLMRequest, error) {
	requests, err := m.journal()
	if err != nil {
		return nil, err
	}
	var matched []mockLLMRequest
	for _, request := range requests {
		if request.Mentions(marker) {
			matched = append(matched, request)
		}
	}
	return matched, nil
}

// expectMockLLMToolResult waits until the model received the result of a
// toolName call in the conversation marked by marker, proving Orka executed
// the scripted tool and fed its output back. It returns that output.
func expectMockLLMToolResult(marker, toolName string, timeout time.Duration) string {
	return expectMockLLMToolResultsWithOffset(2, marker, toolName, 1, timeout)[0]
}

// expectMockLLMToolResults waits until one request for marker carries at least
// count results of toolName and returns them in call order.
func expectMockLLMToolResults(marker, toolName string, count int, timeout time.Duration) []string {
	return expectMockLLMToolResultsWithOffset(2, marker, toolName, count, timeout)
}

func expectMockLLMToolResultsWithOffset(offset int, marker, toolName string, count int, timeout time.Duration) []string {
	var results []string
	EventuallyWithOffset(offset, func(g Gomega) {
		requests, err := mockLLMRequestsFor(marker)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requests).NotTo(BeEmpty(), "aimock saw no request for %q", marker)
		for _, request := range requests {
			if found := request.ToolResults(toolName); len(found) >= count {
				results = found
				return
			}
		}
		g.Expect(false).To(BeTrue(), "fewer than %d %s tool results reached the model for %q", count, toolName, marker)
	}, timeout, 2*time.Second).Should(Succeed())
	return results
}

// expectMockLLMServed waits until aimock answered at least one request for
// marker with a scripted (non catch-all) fixture.
func expectMockLLMServed(marker string, timeout time.Duration) []mockLLMRequest {
	var served []mockLLMRequest
	EventuallyWithOffset(1, func(g Gomega) {
		requests, err := mockLLMRequestsFor(marker)
		g.Expect(err).NotTo(HaveOccurred())
		served = served[:0]
		for _, request := range requests {
			if request.Response.Status == 200 && !isMockLLMCatchAll(request.Response.Fixture) {
				served = append(served, request)
			}
		}
		g.Expect(served).NotTo(BeEmpty(), "aimock served no scripted response for %q", marker)
	}, timeout, 2*time.Second).Should(Succeed())
	return served
}

// When a spec fails in mock mode, print what the model saw and answered
// during it, so CI failures show where the scripted conversation diverged.
var _ = AfterEach(func() {
	if !e2eMockLLMEnabled() || !CurrentSpecReport().Failed() {
		return
	}
	requests, err := suiteMockLLM.journal()
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "aimock journal unavailable: %v\n", err)
		return
	}
	since := CurrentSpecReport().StartTime.UnixMilli()
	_, _ = fmt.Fprintf(GinkgoWriter, "\n=== aimock requests during this spec ===\n")
	for _, request := range requests {
		if request.Timestamp < since {
			continue
		}
		var last mockLLMMessage
		if n := len(request.Body.Messages); n > 0 {
			last = request.Body.Messages[n-1]
		}
		text := last.Text()
		if len(text) > 240 {
			text = text[:240] + "..."
		}
		match := "no fixture"
		var fixture struct {
			Match json.RawMessage `json:"match"`
		}
		if json.Unmarshal(request.Response.Fixture, &fixture) == nil && fixture.Match != nil {
			match = string(fixture.Match)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "%s %d match=%s last[%s]=%q\n",
			request.Path, request.Response.Status, match, last.Role, text)
	}
})

func isMockLLMCatchAll(fixture json.RawMessage) bool {
	var decoded struct {
		Match map[string]json.RawMessage `json:"match"`
	}
	return json.Unmarshal(fixture, &decoded) == nil && len(decoded.Match) == 0
}
