//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/test/utils"
)

// The model-free soul evals check what each runtime sends to inference, not
// how a model follows a persona. The AI worker and the four built-in runtime
// CLIs run against aimock; the complete request capture is the oracle that the
// persona reached the model once, in its framing, after the role instructions,
// and never as user content. Continuation keeps the pinned revision, and a
// changed revision cannot reach a Session that was bound to the previous one.
const (
	soulEvalPersonaHeading = "## Agent persona (SOUL.md)"
	soulEvalPersonaFooter  = "## End of Agent persona (SOUL.md)"
	soulEvalTaskWait       = 5 * time.Minute
)

// soulEvalReadTools is the strict read-only native tool surface every built-in
// runtime can enforce exactly.
var soulEvalReadTools = []string{"Read", "Glob", "Grep"}

func soulEvalMarker(runtime, stage string) string {
	return "[e2e:soul-eval-" + runtime + "-" + stage + "]"
}

// soulEvalRevision is one persona revision. Its canary is unique per run and
// revision, and its text keeps dollar syntax that must reach the model
// literally.
type soulEvalRevision struct {
	configMap, canary, text string
}

func newSoulEvalRevision(runID, runtime, revision string) soulEvalRevision {
	canary := "SOUL_EVAL_" + strings.ToUpper(runtime+"_"+revision) + "_" + runID
	return soulEvalRevision{
		configMap: runID + "-" + runtime + "-soul-" + revision,
		canary:    canary,
		text: "# Lighthouse keeper\n" + canary + "\n" +
			"Speak calmly and briefly. Keep literal $(HOME) and $$ text unchanged.\n",
	}
}

func (r soulEvalRevision) source() map[string]any {
	return map[string]any{
		"configMapRef": map[string]any{"name": r.configMap, "key": "SOUL.md"},
		"digest":       agentcontext.Digest(r.text),
	}
}

func applySoulEvalConfigMap(revision soulEvalRevision) {
	ExpectWithOffset(1, applyManifestJSON(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": revision.configMap, "namespace": namespace},
		"data":     map[string]any{"SOUL.md": revision.text},
	})).To(Succeed())
}

// soulEvalCheck inspects every captured request. Agent requests carry tools or
// the role instructions, and must carry the persona exactly once, framed after
// the role inside the instruction field of their wire protocol. Orka composes
// the role and persona into one string, so they must arrive in one instruction
// string or content part: a split would change the frozen prompt bytes. Runtimes also
// send tool-free auxiliary requests, such as title generation; one may copy the
// complete Agent instructions, but no request may carry the persona without
// them or carry a revision its Session was not bound to. At least one Agent
// request must be served by the marker's fixture. Errors name properties and
// counts only, never request or persona text.
func soulEvalCheck(
	requests []mockLLMEvalRequest, marker, role string, want soulEvalRevision, absent ...soulEvalRevision,
) error {
	turns := 0
	for index, request := range requests {
		var fixture struct {
			Match struct {
				UserMessage string `json:"userMessage"`
			} `json:"match"`
		}
		served := json.Unmarshal(request.journal.Response.Fixture, &fixture) == nil &&
			fixture.Match.UserMessage == marker
		var body any
		if err := json.Unmarshal([]byte(request.capture.Body), &body); err != nil {
			return fmt.Errorf("request %d is not a JSON body", index)
		}
		for _, revision := range absent {
			if len(soulEvalStringsContaining(body, revision.canary)) != 0 {
				return fmt.Errorf("request %d carries a persona revision its Session was not bound to", index)
			}
		}
		carriers := soulEvalStringsContaining(body, want.canary)
		occurrences := 0
		for _, carrier := range carriers {
			occurrences += strings.Count(carrier, want.canary)
		}
		object, _ := body.(map[string]any)
		tools, _ := object["tools"].([]any)
		if len(tools) == 0 && len(soulEvalStringsContaining(body, role)) == 0 {
			if occurrences != 0 {
				return fmt.Errorf("auxiliary request %d carries the persona without the role instructions", index)
			}
			continue
		}
		if served {
			turns++
		}
		if occurrences != 1 {
			return fmt.Errorf("request %d carries the persona %d times, want exactly once", index, occurrences)
		}
		instructions := carriers[0]
		if !slices.Contains(soulEvalInstructionTexts(request.capture.Path, object), instructions) {
			return fmt.Errorf("request %d carries the persona outside the model instructions", index)
		}
		if !strings.Contains(instructions, strings.TrimSuffix(want.text, "\n")) {
			return fmt.Errorf("request %d changed the persona bytes", index)
		}
		roleAt := strings.Index(instructions, role)
		headingAt := strings.Index(instructions, soulEvalPersonaHeading)
		personaAt := strings.Index(instructions, want.canary)
		footerAt := strings.Index(instructions, soulEvalPersonaFooter)
		if roleAt < 0 || headingAt <= roleAt || personaAt <= headingAt || footerAt <= personaAt {
			return fmt.Errorf("request %d lost the role-then-persona framing (role=%d heading=%d persona=%d footer=%d)",
				index, roleAt, headingAt, personaAt, footerAt)
		}
	}
	if turns == 0 {
		return fmt.Errorf("no request served by the %s fixture carried the Agent instructions", marker)
	}
	return nil
}

// soulEvalInstructionTexts returns the instruction strings of the request's
// wire protocol, chosen by its endpoint: Anthropic Messages system, Responses
// instructions and developer/system input items, or Chat Completions
// system/developer messages. Content parts count only with their protocol's
// text type; an unknown endpoint has no instruction field.
func soulEvalInstructionTexts(path string, body map[string]any) []string {
	var texts []string
	add := func(content any, partType string) {
		switch content := content.(type) {
		case string:
			texts = append(texts, content)
		case []any:
			for _, part := range content {
				if part, ok := part.(map[string]any); ok && part["type"] == partType {
					if text, ok := part["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
		}
	}
	addRoles := func(field, partType string) {
		items, _ := body[field].([]any)
		for _, item := range items {
			if item, ok := item.(map[string]any); ok && (item["role"] == "system" || item["role"] == "developer") {
				add(item["content"], partType)
			}
		}
	}
	path, _, _ = strings.Cut(path, "?")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		addRoles("messages", "text")
	case strings.HasSuffix(path, "/responses"):
		// Top-level instructions are a string; content parts live in input.
		if instructions, ok := body["instructions"].(string); ok {
			texts = append(texts, instructions)
		}
		addRoles("input", "input_text")
	case strings.HasSuffix(path, "/messages"):
		add(body["system"], "text")
	}
	return texts
}

// soulEvalStringsContaining decodes the body, so JSON escaping cannot hide or
// split a persona.
func soulEvalStringsContaining(value any, canary string) []string {
	var found []string
	switch value := value.(type) {
	case string:
		if strings.Contains(value, canary) {
			found = append(found, value)
		}
	case []any:
		for _, item := range value {
			found = append(found, soulEvalStringsContaining(item, canary)...)
		}
	case map[string]any:
		for key, item := range value {
			found = append(found, soulEvalStringsContaining(key, canary)...)
			found = append(found, soulEvalStringsContaining(item, canary)...)
		}
	}
	return found
}

// soulEvalParkRuntimePools stops every running RuntimePool. Each persona
// revision has its own runtime profile and so its own pool, and idle pools
// otherwise hold their runtime Pods past what a small CI node can schedule.
func soulEvalParkRuntimePools() {
	var pools corev1alpha1.RuntimePoolList
	kubectlJSON(Default, &pools, "get", "runtimepools", "-n", namespace)
	for _, pool := range pools.Items {
		if pool.DeletionTimestamp == nil && (pool.Spec.DesiredReplicas != 0 || pool.Status.CurrentReplicas != 0) {
			parkRuntimePool(pool.Name)
		}
	}
}

func soulEvalWaitTask(name, phase string) {
	EventuallyWithOffset(1, func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "get", "task", name, "-n", namespace,
			"--request-timeout=10s", "-o", "jsonpath={.status.phase}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(BeElementOf("Succeeded", "Failed", "Cancelled"))
		if output != phase {
			// A terminal phase is final; report a wrong one immediately.
			StopTrying("Task reached terminal phase " + output).Now()
		}
	}, soulEvalTaskWait, 2*time.Second).Should(Succeed(), "Task %s should reach %s", name, phase)
}

var _ = Describe("Agent soul delivery model-free evals", Ordered, ContinueOnFailure, Serial,
	Label("agent-runtime", "soul-eval"), func() {
		var (
			runID, apiBaseURL, token string
			aiModel                  string
		)

		BeforeAll(func() {
			if e2eLiveCopilotProxyBaseURL != "" || e2eFlagEnabled(mockLLMDisableEnvVar) || !e2eMockOpenAI {
				Skip("soul evals read the complete aimock capture, never a live provider")
			}
			runID = fmt.Sprintf("soul%d", time.Now().UnixNano()%1000000000)
			projectDir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(providerProxyMockLLM.delete)
			providerProxyMockLLM.deploy(projectDir)

			var cancel context.CancelFunc
			var pf *exec.Cmd
			apiBaseURL, cancel, pf, err = startControllerAPIPortForward(18136)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { stopPortForward(cancel, pf) })
			token, err = serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())

			aiModel = e2eOpenAIModel
			if aiModel == "" {
				aiModel = "gpt-4o-mini"
			}
			provider := runID + "-provider"
			createProviderCRD(provider, "openai", "e2e-openai-secret", "api-key", e2eOpenAIBaseURL, aiModel)
			DeferCleanup(deleteE2EObjects, "provider", provider)
		})

		// runSoulEval drives one runtime through delivery, continuation,
		// revision drift in the bound Session, and a new Session for the
		// revision. agent builds the Agent; task builds a Task for a Session.
		// Built-in runtimes run on RuntimePools, which are parked around and
		// between revisions.
		runSoulEval := func(
			runtime string, instance mockLLMInstance, pooled bool,
			agent func(name, role string, soul map[string]any) map[string]any,
			task func(name, agent, prompt, session string, create bool) map[string]any,
		) {
			var tasks []string
			agentName := runID + "-" + runtime + "-agent"
			first := newSoulEvalRevision(runID, runtime, "v1")
			second := newSoulEvalRevision(runID, runtime, "v2")
			DeferCleanup(func() {
				if CurrentSpecReport().Failed() {
					dumpDebugInfo(tasks...)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				if len(tasks) != 0 {
					cleanup, err := newE2ECleanup(apiBaseURL, token)
					Expect(err).NotTo(HaveOccurred())
					Expect(cleanup.tasks(ctx, tasks, false)).To(Succeed())
				}
				deleteE2EObjects("agent", agentName)
				deleteE2EObjects("configmap", first.configMap)
				deleteE2EObjects("configmap", second.configMap)
				if pooled {
					soulEvalParkRuntimePools()
				}
			})
			if pooled {
				soulEvalParkRuntimePools()
			}
			role := "SOUL_EVAL_ROLE_" + strings.ToUpper(runtime) + "_" + runID +
				" Answer the request in one short sentence."
			name := func(stage string) string { return runID + "-" + runtime + "-" + stage }
			// run applies one Task and returns the requests its marker caused.
			run := func(stage, session string, create bool, phase string) ([]mockLLMEvalRequest, mockLLMEvalSnapshot) {
				marker := soulEvalMarker(runtime, stage)
				before, err := instance.evalSnapshot()
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				ExpectWithOffset(1, applyManifestJSON(task(name(stage), agentName,
					"Reply with a short acknowledgement. "+marker, session, create))).To(Succeed())
				tasks = append(tasks, name(stage))
				soulEvalWaitTask(name(stage), phase)
				if phase != "Succeeded" {
					return nil, before
				}
				requests, err := instance.evalRequestsSince(before, marker)
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
				return requests, before
			}

			By("delivering the ConfigMap persona with the role instructions")
			applySoulEvalConfigMap(first)
			applySoulEvalConfigMap(second)
			Expect(applyManifestJSON(agent(agentName, role, first.source()))).To(Succeed())
			session := name("session")
			requests, _ := run("first", session, true, "Succeeded")
			Expect(soulEvalCheck(requests, soulEvalMarker(runtime, "first"), role, first, second)).To(Succeed())

			By("keeping the pinned persona when the Session continues")
			requests, _ = run("second", session, false, "Succeeded")
			Expect(soulEvalCheck(requests, soulEvalMarker(runtime, "second"), role, first, second)).To(Succeed())

			if pooled {
				// The new revision needs a new pool; free the first one's Pod.
				parkRuntimePool(getACPTask(Default, name("second")).Status.Execution.RuntimePoolName)
			}

			By("refusing a changed persona revision in the bound Session before inference")
			Expect(applyManifestJSON(agent(agentName, role, second.source()))).To(Succeed())
			_, before := run("drift", session, false, "Failed")
			Expect(instance.evalNoRequestsSince(before)).To(Succeed())

			By("delivering only the new revision to a new Session")
			requests, _ = run("revised", name("revised-session"), true, "Succeeded")
			Expect(soulEvalCheck(requests, soulEvalMarker(runtime, "revised"), role, second, first)).To(Succeed())
		}

		It("delivers and pins the soul for the AI worker", func() {
			provider := runID + "-provider"
			runSoulEval("ai", suiteMockLLM, false,
				func(name, role string, soul map[string]any) map[string]any {
					return map[string]any{
						"apiVersion": "core.orka.ai/v1alpha1", "kind": "Agent",
						"metadata": map[string]any{"name": name, "namespace": namespace},
						"spec": map[string]any{
							"providerRef":  map[string]any{"name": provider},
							"model":        map[string]any{"name": aiModel},
							"systemPrompt": map[string]any{"inline": role},
							"soul":         soul,
						},
					}
				},
				func(name, agent, prompt, session string, create bool) map[string]any {
					return map[string]any{
						"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task",
						"metadata": map[string]any{"name": name, "namespace": namespace},
						"spec": map[string]any{
							"type": "ai", "agentRef": map[string]any{"name": agent}, "prompt": prompt,
							"sessionRef": map[string]any{"name": session, "create": create, "append": true},
						},
					}
				})
		})

		for _, provider := range runtimeEvalProviders {
			It("delivers and pins the soul for the built-in "+provider.kind+" runtime", func() {
				runSoulEval(provider.kind, providerProxyMockLLM, true,
					func(name, role string, soul map[string]any) map[string]any {
						agent := agentRuntimeAgentManifest(name, provider.kind, provider.model, runID, 3, false,
							&corev1alpha1.ModelTokenLimits{Context: 32768, Output: 4096})
						spec := agent["spec"].(map[string]any)
						runtime := spec["runtime"].(map[string]any)
						runtime["defaultAllowedTools"], runtime["defaultAllowBash"] = soulEvalReadTools, false
						spec["systemPrompt"] = map[string]any{"inline": role}
						spec["soul"] = soul
						return agent
					},
					func(name, agent, prompt, session string, create bool) map[string]any {
						task := runtimeAgentTaskManifest(name, agent, prompt, 3, new(false), nil, session,
							new(create), new(true))
						// Codex accepts its full native surface or this strict
						// read-only surface, never a partial tool policy.
						runtime := task["spec"].(map[string]any)["agentRuntime"].(map[string]any)
						runtime["allowedTools"] = soulEvalReadTools
						return task
					})
			})
		}
	})
