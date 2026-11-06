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
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/test/fixtures/aimockauthority"
	"github.com/orka-agents/orka/test/utils"
)

const (
	ahpE2EAgent = "e2e-ahp-codex"
	// ahpE2EProvider is the one AHP agent Orka offers; it runs the codex
	// Agent's model on Codex.
	ahpE2EProvider     = "copilot"
	ahpE2ESubject      = "e2e-ahp-person"
	ahpE2ERepository   = "https://github.com/octocat/Hello-World"
	ahpE2EBaseBranch   = "master"
	ahpE2ETurnMarker   = "[e2e:ahp-turn]"
	ahpE2ECancelMarker = "[e2e:ahp-cancel]"
	ahpE2ETurnReply    = "ORKA_AHP_TURN_OK"
	ahpE2EObserverPort = 18140
	ahpE2EAPIPort      = 18141
	ahpE2ETurnWait     = 8 * time.Minute
)

// The AHP host end to end: a person signed in with OIDC mints a connection
// token, a desktop-style client connects to /ahp with the token in the URL,
// sees the built-in runtime, and runs and cancels turns that are Orka Tasks.
// Tokens stay in memory: they never enter command arguments, failure
// messages or reports.
var _ = Describe("Agent Host Protocol host", Ordered, Serial, Label("agent-runtime", "ahp"), func() {
	var (
		authority  *aimockAuthorityEval
		saToken    string
		tokenID    string
		ahpToken   string
		client     *ahp.Client
		sessionURI string
		chatURI    string
		taskNames  []string
	)

	BeforeAll(func() {
		if e2eLiveCopilotProxyBaseURL != "" || e2eFlagEnabled(mockLLMDisableEnvVar) {
			Skip("the AHP host spec scripts its turns with the synthetic aimock backing")
		}
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(providerProxyMockLLM.delete)
		providerProxyMockLLM.deploy(projectDir)

		By("turning on the AHP host with the fixture's OIDC issuer")
		authority = deployAimockAuthority(ahpE2EObserverPort, ahpE2EAPIPort)
		oidcIssuer := authority.issuerURL + aimockauthority.OIDCPath
		authority.patchArgs(authorizationReplaceFlags(authority.originalArgs, map[string]string{
			"enable-ahp-api":        "true",
			"oidc-issuer":           oidcIssuer,
			"oidc-audience":         aimockauthority.OIDCAudience,
			"oidc-jwks-url":         oidcIssuer + "/jwks",
			"oidc-allowed-subjects": ahpE2ESubject,
			"oidc-namespace":        namespace,
		}))
		authority.connectAPI()
		DeferCleanup(func() {
			stopPortForward(authority.apiCancel, authority.apiPF)
			authority.apiCancel, authority.apiPF = nil, nil
		})
		saToken, err = serviceAccountToken()
		Expect(err).NotTo(HaveOccurred())

		By("offering one built-in runtime model to AHP people")
		// Codex enforces only its unrestricted policy or a read-only tool set,
		// so the Agent keeps the default tools, as the other Codex specs do.
		Expect(applyManifestJSON(runtimeAgentManifest(ahpE2EAgent, "codex", runtimeMatrixMockCodexModel, 4,
			nil))).To(Succeed())
		DeferCleanup(deleteE2EObjects, "agent", ahpE2EAgent)

		// Runs before the Agent is deleted and the controller is restored.
		DeferCleanup(func() {
			if client != nil {
				_ = client.Shutdown(context.Background())
			}
			if len(taskNames) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cleanup, err := newE2ECleanup(authority.apiURL, saToken)
			Expect(err).NotTo(HaveOccurred())
			Expect(cleanup.tasks(ctx, taskNames, false)).To(Succeed())
		})
	})

	It("mints a connection token for a person signed in with OIDC", func() {
		var issued struct {
			Token string `json:"token"`
		}
		authority.request(http.MethodPost, aimockauthority.OIDCPath+"/issue",
			aimockauthority.OIDCIssueRequest{Subject: ahpE2ESubject}, &issued)
		Expect(issued.Token).NotTo(BeEmpty())
		oidcToken := issued.Token

		By("refusing ServiceAccount callers")
		status, _ := ahpE2EAPI(authority.apiURL, saToken, http.MethodPost, "/api/v1/ahp-tokens", `{}`)
		Expect(status).To(Equal(http.StatusForbidden), "ServiceAccounts must not mint AHP tokens")

		By("minting for the OIDC person")
		status, body := ahpE2EAPI(authority.apiURL, oidcToken, http.MethodPost, "/api/v1/ahp-tokens",
			`{"ttl":"1h","description":"e2e device"}`)
		Expect(status).To(Equal(http.StatusCreated), "minting an AHP token failed")
		var minted struct {
			ID        string `json:"id"`
			Token     string `json:"token"`
			Namespace string `json:"namespace"`
		}
		authorizationExpectNoError(json.Unmarshal(body, &minted), "the AHP token response is invalid")
		Expect(minted.ID).NotTo(BeEmpty())
		Expect(strings.HasPrefix(minted.Token, "orka_ahp_")).To(BeTrue(), "AHP tokens carry their scanner prefix")
		Expect(minted.Namespace).To(Equal(namespace))
		tokenID, ahpToken = minted.ID, minted.Token

		By("listing the token without its secret")
		status, body = ahpE2EAPI(authority.apiURL, oidcToken, http.MethodGet, "/api/v1/ahp-tokens", "")
		Expect(status).To(Equal(http.StatusOK))
		Expect(bytes.Contains(body, []byte(ahpToken))).To(BeFalse(), "a token listing returned a token secret")
		Expect(ahpE2EListedTokenIDs(body)).To(ContainElement(tokenID))

		By("refusing the AHP door without a valid token")
		Expect(ahpE2EDoorStatus(authority.apiURL, "")).To(Equal(http.StatusUnauthorized))
		Expect(ahpE2EDoorStatus(authority.apiURL, "orka_ahp_not-a-token")).To(Equal(http.StatusUnauthorized))
		Expect(ahpE2EDoorStatus(authority.apiURL, ahpToken)).NotTo(Equal(http.StatusUnauthorized),
			"a valid token reaches the WebSocket upgrade")
	})

	It("connects over WebSocket with the token in the URL and lists the built-in runtime", func() {
		Expect(ahpToken).NotTo(BeEmpty())
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		transport, err := ahpws.Connect(ctx, ahpE2EHostURL(authority.apiURL, ahpToken))
		// The dial error repeats the URL, which carries the token.
		authorizationExpectNoError(err, "the AHP WebSocket connection failed")
		client, err = ahp.Connect(ctx, transport, ahp.DefaultConfig())
		Expect(err).NotTo(HaveOccurred())
		initialized, err := client.Initialize(ctx, fmt.Sprintf("e2e-ahp-%d", time.Now().UnixNano()), []string{"1.0.0"}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(initialized.ProtocolVersion).To(Equal("1.0.0"))

		root, _, err := client.Subscribe(ctx, ahptypes.RootResourceURI)
		Expect(err).NotTo(HaveOccurred())
		Expect(root.Snapshot).NotTo(BeNil())
		Expect(root.Snapshot.State.Root).NotTo(BeNil())
		var models []string
		for _, agent := range root.Snapshot.State.Root.Agents {
			if agent.Provider != ahpE2EProvider {
				continue
			}
			for _, model := range agent.Models {
				models = append(models, model.Id)
			}
		}
		Expect(models).To(ContainElement(runtimeMatrixMockCodexModel), "the codex Agent is offered as an AHP model")
	})

	It("runs a turn as an Orka Task that completes", func() {
		Expect(client).NotTo(BeNil())
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		By("adding the repository as a project")
		var cloned struct {
			Project struct {
				Path   string `json:"path"`
				Status string `json:"status"`
			} `json:"project"`
		}
		Expect(client.Request(ctx, "extensions/cloneProject", map[string]string{"url": ahpE2ERepository + ".git"},
			&cloned)).To(Succeed())
		Expect(cloned.Project.Status).To(Equal("ready"))
		Expect(cloned.Project.Path).To(Equal("/workspace/src/octocat/Hello-World"))

		By("creating a session in the project")
		sessionURI = fmt.Sprintf("ahp-session:/e2e-%d", time.Now().UnixNano())
		provider := ahpE2EProvider
		Expect(client.Request(ctx, "createSession", ahptypes.CreateSessionParams{
			Channel: sessionURI, Provider: &provider, WorkingDirectories: []string{"file://" + cloned.Project.Path},
			Config: map[string]json.RawMessage{
				"approvalMode": json.RawMessage(`"allow-all"`),
				// A repository the host cannot probe has no default branch.
				"baseBranch": json.RawMessage(`"` + ahpE2EBaseBranch + `"`),
			},
		}, nil)).To(Succeed())
		session, _, err := client.Subscribe(ctx, sessionURI)
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Snapshot.State.Session).NotTo(BeNil())
		Expect(session.Snapshot.State.Session.Chats).To(HaveLen(1))
		chatURI = session.Snapshot.State.Session.Chats[0].Resource

		By("sending a message")
		ahpE2EStartTurn(ctx, client, chatURI, "e2e-turn-1",
			"Reply with "+ahpE2ETurnReply+" and nothing else. "+ahpE2ETurnMarker)
		task := ahpE2EWaitForTurnTask(ahpE2ETurnMarker)
		taskNames = append(taskNames, task.Name)
		Expect(task.Spec.AgentRef).NotTo(BeNil())
		Expect(task.Spec.AgentRef.Name).To(Equal(ahpE2EAgent))
		Expect(task.Spec.Workspace).NotTo(BeNil())
		Expect(task.Spec.Workspace.GitRepo).To(Equal(ahpE2ERepository))
		Expect(task.Spec.Workspace.Branch).To(Equal(ahpE2EBaseBranch))
		Expect(task.Spec.Workspace.Intent).To(Equal(corev1alpha1.WorkspaceIntentRead),
			"without a git credential AHP turns are read-only")
		Expect(task.Spec.SessionRef).NotTo(BeNil())
		Expect(task.Spec.SessionRef.Create).To(BeTrue())

		By("streaming the answer and completing the turn")
		turn := ahpE2EWaitForTurn(client, chatURI, "e2e-turn-1", ahpE2ETurnWait)
		Expect(turn.State).To(Equal(ahptypes.TurnStateComplete), "turn ended: %s", ahpE2ETurnError(turn))
		Expect(ahpE2ETurnText(turn)).To(ContainSubstring(ahpE2ETurnReply))
		Eventually(func(g Gomega) {
			g.Expect(getACPTask(g, task.Name).Status.Phase).To(Equal(corev1alpha1.TaskPhaseSucceeded))
		}, time.Minute, 2*time.Second).Should(Succeed())
		ahpE2EWaitForIdleSession(client, sessionURI)
	})

	It("cancels a running turn and its Task", func() {
		Expect(chatURI).NotTo(BeEmpty())
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		By("sending a message the model holds")
		ahpE2EStartTurn(ctx, client, chatURI, "e2e-turn-2", "Wait for the answer. "+ahpE2ECancelMarker)
		task := ahpE2EWaitForTurnTask(ahpE2ECancelMarker)
		taskNames = append(taskNames, task.Name)
		Expect(task.Spec.SessionRef).NotTo(BeNil())
		Expect(task.Spec.SessionRef.Create).To(BeFalse(), "the next turn continues the Orka Session")
		Eventually(func(g Gomega) {
			current := getACPTask(g, task.Name)
			g.Expect(current.Status.Execution).NotTo(BeNil())
			g.Expect(current.Status.Execution.State).To(Equal(corev1alpha1.TaskExecutionStateRunning))
		}, ahpE2ETurnWait, 2*time.Second).Should(Succeed())

		By("cancelling the turn from the client")
		_, err := client.Dispatch(ctx, chatURI, ahptypes.StateAction{Value: &ahptypes.ChatTurnCancelledAction{
			Type: ahptypes.ActionTypeChatTurnCancelled, TurnId: "e2e-turn-2", Duration: 1,
		}})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			current := getACPTask(g, task.Name)
			_, requested := current.Annotations[labels.AnnotationCancelRequested]
			g.Expect(requested || current.Status.Phase == corev1alpha1.TaskPhaseCancelled).To(BeTrue(),
				"cancelling the turn cancels its Task")
		}, time.Minute, time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			current := getACPTask(g, task.Name)
			g.Expect(current.Status.Phase).To(Equal(corev1alpha1.TaskPhaseCancelled))
			g.Expect(current.Status.Execution).NotTo(BeNil())
			g.Expect(current.Status.Execution.State).To(BeElementOf(terminalExecutionStates))
		}, agentRuntimeSettleBound, 2*time.Second).Should(Succeed())

		turn := ahpE2EWaitForTurn(client, chatURI, "e2e-turn-2", time.Minute)
		Expect(turn.State).To(Equal(ahptypes.TurnStateCancelled))
		ahpE2EWaitForIdleSession(client, sessionURI)
	})

	It("revokes the token and closes its connection", func() {
		Expect(tokenID).NotTo(BeEmpty())
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if client != nil && sessionURI != "" {
			Expect(client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: sessionURI}, nil)).
				To(Succeed())
		}
		var issued struct {
			Token string `json:"token"`
		}
		authority.request(http.MethodPost, aimockauthority.OIDCPath+"/issue",
			aimockauthority.OIDCIssueRequest{Subject: ahpE2ESubject}, &issued)
		status, _ := ahpE2EAPI(authority.apiURL, issued.Token, http.MethodDelete,
			"/api/v1/ahp-tokens/"+url.PathEscape(tokenID), "")
		Expect(status).To(Equal(http.StatusNoContent))
		if client != nil {
			Eventually(client.Done(), time.Minute).Should(BeClosed(), "revoking a token closes its connections")
		}
		Expect(ahpE2EDoorStatus(authority.apiURL, ahpToken)).To(Equal(http.StatusUnauthorized),
			"a revoked token is refused at the door")
	})
})

// ahpE2EHostURL is the URL a desktop AHP client is given: the token travels
// in the query because such clients send no Authorization header.
func ahpE2EHostURL(apiURL, token string) string {
	base := strings.TrimRight(apiURL, "/")
	base = "ws" + strings.TrimPrefix(base, "http")
	return base + "/ahp?tkn=" + url.QueryEscape(token)
}

// ahpE2EAPI calls the Orka API with a bearer token. Errors and bodies are
// never echoed, since requests and responses can carry tokens.
func ahpE2EAPI(apiURL, bearer, method, path, body string) (int, []byte) {
	req, err := http.NewRequest(method, strings.TrimRight(apiURL, "/")+path, strings.NewReader(body))
	authorizationExpectNoError(err, "the Orka API request is invalid")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	authorizationExpectNoError(err, "the Orka API request failed")
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	authorizationExpectNoError(err, "the Orka API response could not be read")
	return resp.StatusCode, data
}

func ahpE2EListedTokenIDs(body []byte) []string {
	var listed struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	authorizationExpectNoError(json.Unmarshal(body, &listed), "the AHP token listing is invalid")
	ids := make([]string, 0, len(listed.Items))
	for _, item := range listed.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// ahpE2EDoorStatus asks the AHP door without upgrading, so a valid token gets
// a WebSocket handshake error and an invalid one gets 401.
func ahpE2EDoorStatus(apiURL, token string) int {
	target := strings.TrimRight(apiURL, "/") + "/ahp"
	if token != "" {
		target += "?tkn=" + url.QueryEscape(token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(target)
	// The error repeats the URL, which carries the token.
	authorizationExpectNoError(err, "the AHP door request failed")
	_ = resp.Body.Close()
	return resp.StatusCode
}

func ahpE2EStartTurn(ctx context.Context, client *ahp.Client, chat, turnID, text string) {
	model := runtimeMatrixMockCodexModel
	_, err := client.Dispatch(ctx, chat, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: turnID, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: ahptypes.Message{Text: text, Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser},
			Model: &ahptypes.ModelSelection{Id: model}},
	}})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

// ahpE2EWaitForTurnTask finds the person's Task whose prompt carries marker.
func ahpE2EWaitForTurnTask(marker string) corev1alpha1.Task {
	var found corev1alpha1.Task
	EventuallyWithOffset(1, func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "get", "tasks", "-n", namespace, "-o", "json"))
		g.Expect(err).NotTo(HaveOccurred())
		var tasks corev1alpha1.TaskList
		g.Expect(json.Unmarshal([]byte(output), &tasks)).To(Succeed())
		index := slices.IndexFunc(tasks.Items, func(task corev1alpha1.Task) bool {
			return strings.Contains(task.Spec.Prompt, marker) && task.Spec.RequestedBy != nil &&
				task.Spec.RequestedBy.Subject == ahpE2ESubject
		})
		g.Expect(index).To(BeNumerically(">=", 0), "no AHP turn Task for %s", marker)
		found = tasks.Items[index]
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	return found
}

// ahpE2EWaitForTurn waits until the chat records turnID as ended.
func ahpE2EWaitForTurn(client *ahp.Client, chat, turnID string, timeout time.Duration) ahptypes.Turn {
	var ended ahptypes.Turn
	EventuallyWithOffset(1, func(g Gomega) {
		state := ahpE2ESnapshot(g, client, chat)
		g.Expect(state.Chat).NotTo(BeNil())
		index := slices.IndexFunc(state.Chat.Turns, func(turn ahptypes.Turn) bool { return turn.Id == turnID })
		g.Expect(index).To(BeNumerically(">=", 0), "turn %s has not ended", turnID)
		ended = state.Chat.Turns[index]
	}, timeout, 2*time.Second).Should(Succeed())
	return ended
}

// ahpE2EWaitForIdleSession waits until the session's chat is idle again,
// which clients read as the end of the session's work.
func ahpE2EWaitForIdleSession(client *ahp.Client, session string) {
	EventuallyWithOffset(1, func(g Gomega) {
		state := ahpE2ESnapshot(g, client, session)
		g.Expect(state.Session).NotTo(BeNil())
		g.Expect(state.Session.Chats).NotTo(BeEmpty())
		g.Expect(state.Session.Chats[0].Status).To(Equal(ahptypes.SessionStatusIdle))
	}, time.Minute, 2*time.Second).Should(Succeed())
}

func ahpE2ESnapshot(g Gomega, client *ahp.Client, uri string) ahptypes.SnapshotState {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, sub, err := client.Subscribe(ctx, uri)
	g.Expect(err).NotTo(HaveOccurred())
	sub.Close()
	g.Expect(result.Snapshot).NotTo(BeNil())
	return result.Snapshot.State
}

func ahpE2ETurnText(turn ahptypes.Turn) string {
	var text strings.Builder
	for _, part := range turn.ResponseParts {
		if markdown, ok := part.Value.(*ahptypes.MarkdownResponsePart); ok {
			text.WriteString(markdown.Content)
		}
	}
	return text.String()
}

func ahpE2ETurnError(turn ahptypes.Turn) string {
	for _, part := range turn.ResponseParts {
		if failure, ok := part.Value.(*ahptypes.ErrorResponsePart); ok {
			return failure.Error.ErrorType + ": " + failure.Error.Message
		}
	}
	return string(turn.State)
}
