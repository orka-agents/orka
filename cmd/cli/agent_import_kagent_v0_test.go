/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// governedV0Config is a governed Agent's compiled configuration, as kagent
// writes it to the Agent's config Secret.
const governedV0Config = `{"model":{"type":"openai","model":"probe","api_key_passthrough":true,` +
	`"base_url":"http://probe-gov-relay.orka-system.svc:8092/v1"},"description":"Calls Orka tools.",` +
	`"instruction":"You call Orka tools.","http_tools":[{"params":{"url":"http://probe-gov-relay.orka-system.svc:8092/mcp",` +
	`"headers":{},"timeout":1800},"tools":["probe-orka"],"allowed_headers":["Authorization"]}]}`

const governedV0Card = `{"name":"probe-gov","url":"http://probe-gov.kagent:8080"}`

// noSecretsHash is the status.secretHash kagent gives a ModelConfig that
// uses no secrets: the SHA-256 of nothing.
const noSecretsHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// kagentV0ConfigHash is kagent's config hash for a configuration, with the
// fixture's agent card, whose ModelConfig uses no secrets.
func kagentV0ConfigHash(config string) string {
	secretHash, _ := hex.DecodeString(noSecretsHash)
	sum := sha256.Sum256(append([]byte(config+governedV0Card), secretHash...))
	return strconv.FormatUint(binary.BigEndian.Uint64(sum[:8]), 10)
}

// governedV0Input is an export of a governed Agent with its Deployment and
// config Secret, pinned to hash.
func governedV0Input(config, hash string) string {
	return strings.NewReplacer(
		"{{CONFIG}}", base64.StdEncoding.EncodeToString([]byte(config)),
		"{{CARD}}", base64.StdEncoding.EncodeToString([]byte(governedV0Card)),
		"{{HASH}}", hash,
	).Replace(`apiVersion: kagent.dev/v1alpha2
kind: ModelConfig
metadata: {name: relay-model, namespace: kagent}
spec: {provider: OpenAI, model: probe, apiKeyPassthrough: true}
status: {observedGeneration: 1, secretHash: "` + noSecretsHash + `"}
---
apiVersion: v1
kind: Secret
metadata: {name: probe-gov, namespace: kagent}
data:
  config.json: {{CONFIG}}
  agent-card.json: {{CARD}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: probe-gov, namespace: kagent, generation: 4}
spec:
  replicas: 1
  template:
    metadata:
      annotations: {kagent.dev/config-hash: "{{HASH}}"}
    spec:
      containers:
      - name: kagent
        image: "cr.kagent.dev/kagent-dev/kagent/golang-adk:0.10.3"
        args: ["--host", "0.0.0.0", "--port", "8080", "--filepath", "/config"]
        env:
        - {name: OTEL_TRACING_ENABLED, value: "false"}
        - {name: KAGENT_NAMESPACE, valueFrom: {fieldRef: {apiVersion: v1, fieldPath: metadata.namespace}}}
        - {name: KAGENT_NAME, value: probe-gov}
        - {name: KAGENT_URL, value: "http://kagent-controller.kagent:8083"}
        volumeMounts:
        - {name: config, mountPath: /config}
        - {name: kagent-token, mountPath: /var/run/secrets/tokens}
      volumes:
      - {name: config, secret: {secretName: probe-gov}}
      - {name: kagent-token, projected: {sources: [{serviceAccountToken: {audience: kagent, path: kagent-token}}]}}
status: {observedGeneration: 4, replicas: 1, updatedReplicas: 1, availableReplicas: 1}
---
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata: {name: probe-gov, namespace: kagent, generation: 2}
spec:
  type: Declarative
  description: Calls Orka tools.
  declarative:
    runtime: go
    systemMessage: You call Orka tools.
    modelConfig: relay-model
    tools:
    - type: McpServer
      mcpServer: {kind: RemoteMCPServer, apiGroup: kagent.dev, name: orka-relay, toolNames: [probe-orka], allowedHeaders: [Authorization]}
status:
  observedGeneration: 2
  conditions: [{type: Accepted, status: "True", observedGeneration: 2}, {type: Ready, status: "True"}]
`)
}

var governedV0Hash = kagentV0ConfigHash(governedV0Config)

var governedKagentV0Fixture = governedV0Input(governedV0Config, governedV0Hash)

const governedV0Relay = "http://probe-gov-relay.orka-system.svc:8092"

// v0Controller is the --control-plane-url the v0 fixtures persist to; kagent
// renders the same Service as http://kagent-controller.kagent:8083.
const v0Controller = "http://kagent-controller.kagent.svc:8083"

func classifyV0Fixture(t *testing.T, fixture, relay string) kagentMode {
	t.Helper()
	wrapped, _, err := wrapKagentAgents([]byte(fixture), relay, kagentRevisionEvidence{Controller: v0Controller})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 1 || wrapped[0].Version != kagentVersionV0 {
		t.Fatalf("wrapped = %+v, want one v0 Agent", wrapped)
	}
	return wrapped[0].Mode
}

func TestClassifyKagentV0AgentPinsTheRunningDeployment(t *testing.T) {
	mode := classifyV0Fixture(t, governedKagentV0Fixture, governedRelay)
	if mode.Mode != kagentModeGoverned || mode.RelayOrigin != governedV0Relay || mode.Revision != governedV0Hash {
		t.Fatalf("mode = %+v", mode)
	}
	if mode := classifyV0Fixture(t, governedKagentV0Fixture, ""); mode.Mode != kagentModeObserved ||
		!strings.Contains(strings.Join(mode.Reasons, ";"), "no relay was named") || mode.Revision != governedV0Hash {
		t.Fatalf("an Agent was governed without a named relay, or lost its pin: %+v", mode)
	}
	for name, test := range map[string]struct{ old, new, reason string }{
		"unready Agent":       {`{type: Ready, status: "True"}`, `{type: Ready, status: "False"}`, "the Agent is not Ready"},
		"rejected generation": {`{type: Accepted, status: "True", observedGeneration: 2}`, `{type: Accepted, status: "False", observedGeneration: 2}`, "has not accepted the Agent's current generation"},
		"accepted earlier":    {`{type: Accepted, status: "True", observedGeneration: 2}`, `{type: Accepted, status: "True", observedGeneration: 1}`, "has not accepted the Agent's current generation"},
		"unobserved Agent":    {"  observedGeneration: 2\n", "  observedGeneration: 1\n", "has not observed the Agent's current generation"},
		"python deployment":   {"kagent/golang-adk:0.10.3", "kagent/app:0.10.3", `runs "cr.kagent.dev/kagent-dev/kagent/app:0.10.3", not kagent's Go runtime`},
		"rolling out":         {"updatedReplicas: 1", "updatedReplicas: 0", "has not finished rolling out"},
		"unavailable":         {"availableReplicas: 1", "availableReplicas: 0", "has not finished rolling out"},
		"unobserved rollout":  {"status: {observedGeneration: 4", "status: {observedGeneration: 3", "has not been observed at its current generation"},
		"no config hash":      {`annotations: {kagent.dev/config-hash: "` + governedV0Hash + `"}`, "annotations: {}", "has no kagent.dev/config-hash annotation"},
		"hash out of range":   {`"` + governedV0Hash + `"`, `"99999999999999999999"`, "has no kagent.dev/config-hash annotation"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := strings.Replace(governedKagentV0Fixture, test.old, test.new, 1)
			if changed == governedKagentV0Fixture {
				t.Fatalf("fixture has no %q", test.old)
			}
			mode := classifyV0Fixture(t, changed, governedRelay)
			if mode.Mode != kagentModeObserved || mode.Revision != "" || !strings.Contains(strings.Join(mode.PinProblems, "; "), test.reason) {
				t.Fatalf("mode = %+v, want no pin because %s", mode, test.reason)
			}
		})
	}
}

func TestClassifyKagentV0AgentReadsTheCompiledConfiguration(t *testing.T) {
	// The verdict is bound to the pinned configuration: a config Secret kagent
	// has not rolled out yet, or one that depends on secret material, does
	// not hash to the pin.
	stale := governedV0Input(strings.Replace(governedV0Config, "8092/mcp", "9000/mcp", 1), governedV0Hash)
	if mode := classifyV0Fixture(t, stale, governedRelay); mode.Mode != kagentModeObserved ||
		!strings.Contains(strings.Join(mode.Reasons, ";"), "not the pinned "+governedV0Hash) || mode.Revision != governedV0Hash {
		t.Fatalf("an unbound configuration was governed: %+v", mode)
	}
	for name, test := range map[string]struct{ old, new, reason string }{
		"no config Secret": {"metadata: {name: probe-gov, namespace: kagent}\ndata:", "metadata: {name: other, namespace: kagent}\ndata:", `config Secret "probe-gov" is not in the input`},
		"no model config":  {"metadata: {name: relay-model, namespace: kagent}", "metadata: {name: other-model, namespace: kagent}", `model config "relay-model" is not in the input; kagent folds its status.secretHash`},
		"stale secrets":    {noSecretsHash, strings.Repeat("ab", 32), "not the pinned " + governedV0Hash},
		"bad secret hash":  {noSecretsHash, "not-hex", "has a status.secretHash the import cannot read"},
	} {
		changed := strings.Replace(governedKagentV0Fixture, test.old, test.new, 1)
		if mode := classifyV0Fixture(t, changed, governedRelay); mode.Mode != kagentModeObserved || !strings.Contains(strings.Join(mode.Reasons, ";"), test.reason) {
			t.Errorf("%s: mode = %+v, want observed because %s", name, mode, test.reason)
		}
	}
	for name, test := range map[string]struct{ old, new, reason string }{
		"anthropic model":     {`"type":"openai"`, `"type":"anthropic"`, `the compiled model is "anthropic"`},
		"no passthrough":      {`"api_key_passthrough":true`, `"api_key_passthrough":false`, "does not pass the caller's bearer through"},
		"model elsewhere":     {`8092/v1"`, `8092/other"`, "does not point at a relay's /v1"},
		"responses API":       {`"api_key_passthrough":true`, `"api_key_passthrough":true,"api_format":"responses"`, "uses the responses API"},
		"model headers":       {`"api_key_passthrough":true`, `"api_key_passthrough":true,"headers":{"X-Team":"a"}`, "the compiled model sends static headers"},
		"token exchange":      {`"api_key_passthrough":true`, `"api_key_passthrough":true,"token_exchange":{"type":"GDCHServiceAccount"}`, "exchanges its own tokens"},
		"proxied tools":       {"http://probe-gov-relay.orka-system.svc:8092/mcp", "http://kagent-proxy.kagent.svc:8080/mcp", "points at http://kagent-proxy.kagent.svc:8080, not the relay"},
		"tool path":           {"8092/mcp", "8092/other", "does not point at a relay's /mcp"},
		"tool headers":        {`"headers":{}`, `"headers":{"Authorization":"Bearer static"}`, "compiled tool server 0 sends static headers"},
		"short timeout":       {`"timeout":1800`, `"timeout":30`, "needs a timeout longer than 20m0s"},
		"relay timeout":       {`"timeout":1800`, `"timeout":1200`, "needs a timeout longer than 20m0s"},
		"no bearer":           {`"allowed_headers":["Authorization"]`, `"allowed_headers":["x-user-email"]`, "does not forward Authorization"},
		"kagent approval":     {`"allowed_headers":["Authorization"]`, `"allowed_headers":["Authorization"],"require_approval":["probe-orka"]`, "requires kagent approval"},
		"sse tools":           {`"instruction"`, `"sse_tools":[{"params":{"url":"http://mcp.example.com/sse"}}],"instruction"`, "uses SSE tool servers"},
		"sub-agents":          {`"instruction"`, `"remote_agents":[{"name":"helper","url":"http://helper.kagent:8080"}],"instruction"`, "calls other Agents"},
		"code blocks":         {`"instruction"`, `"execute_code":true,"instruction"`, "executes code blocks"},
		"share tools":         {`"instruction"`, `"share_tools":true,"instruction"`, "share-link tools"},
		"memory":              {`"instruction"`, `"memory":{"ttl_days":15},"instruction"`, "enables memory"},
		"context compaction":  {`"instruction"`, `"context_config":{"compaction":{}},"instruction"`, "compacts its context"},
		"local sessions":      {`"instruction"`, `"session_db_url":"sqlite:////data/sessions.db","instruction"`, "keeps sessions in a local store (session_db_url)"},
		"unknown field":       {`"instruction"`, `"future_setting":true,"instruction"`, "sets future_setting, which the import does not know"},
		"tool on second host": {`"tools":["probe-orka"]`, `"tools":["probe-orka"]},{"params":{"url":"http://tools.example.com:80/mcp","timeout":1800},"allowed_headers":["Authorization"],"tools":["x"]`, "points at http://tools.example.com:80, not the relay"},
	} {
		t.Run(name, func(t *testing.T) {
			config := strings.Replace(governedV0Config, test.old, test.new, 1)
			if config == governedV0Config {
				t.Fatalf("configuration has no %q", test.old)
			}
			// The pin matches, so only the configuration itself can decide.
			mode := classifyV0Fixture(t, governedV0Input(config, kagentV0ConfigHash(config)), governedRelay)
			if mode.Mode != kagentModeObserved || !strings.Contains(strings.Join(mode.Reasons, "; "), test.reason) {
				t.Fatalf("mode = %+v, want observed because %s", mode, test.reason)
			}
		})
	}
	for name, change := range map[string][2]string{
		"lowercase header":     {`"allowed_headers":["Authorization"]`, `"allowed_headers":["authorization"]`},
		"explicit chat format": {`"api_key_passthrough":true`, `"api_key_passthrough":true,"api_format":"chatCompletions"`},
	} {
		config := strings.Replace(governedV0Config, change[0], change[1], 1)
		if mode := classifyV0Fixture(t, governedV0Input(config, kagentV0ConfigHash(config)), governedRelay); mode.Mode != kagentModeGoverned {
			t.Errorf("%s: mode = %+v, want governed", name, mode)
		}
	}
	// A tool URL comes from the config Secret and may carry credentials; the
	// reasons, which the import prints, must not repeat it.
	leaky := strings.Replace(governedV0Config, "http://probe-gov-relay.orka-system.svc:8092/mcp", "http://user:s3cret@tools.example.com/mcp?token=t0ken", 1)
	if mode := classifyV0Fixture(t, governedV0Input(leaky, kagentV0ConfigHash(leaky)), governedRelay); mode.Mode != kagentModeObserved ||
		strings.Contains(strings.Join(mode.Reasons, ";"), "s3cret") || strings.Contains(strings.Join(mode.Reasons, ";"), "t0ken") {
		t.Fatalf("a credential-bearing tool URL reached the reasons: %+v", mode)
	}
	// The Deployment must load the configuration the import judged, as kagent
	// renders it.
	for name, test := range map[string]struct{ old, new, reason string }{
		"other config":     {"secret: {secretName: probe-gov}", "secret: {secretName: elsewhere}", `does not load its config from Secret "probe-gov"`},
		"remapped key":     {"secret: {secretName: probe-gov}", "secret: {secretName: probe-gov, items: [{key: alternate.json, path: config.json}]}", "does not load its config from Secret"},
		"sub path":         {"        - {name: config, mountPath: /config}", "        - {name: config, mountPath: /config, subPath: alternate}", "does not load its config from Secret"},
		"other file":       {`"--filepath", "/config"`, `"--filepath", "/other"`, "does not run kagent's arguments"},
		"extra argument":   {`"/config"]`, `"/config", "--verbose", "1"]`, "does not run kagent's arguments"},
		"command override": {"        args: [", "        command: [/bin/sh]\n        args: [", "overrides the container command"},
		"sts environment":  {"        - {name: KAGENT_NAME, value: probe-gov}", "        - {name: STS_WELL_KNOWN_URI, value: http://sts}\n        - {name: KAGENT_NAME, value: probe-gov}", "sets environment variable STS_WELL_KNOWN_URI"},
		"other field":      {"fieldPath: metadata.namespace", "fieldPath: metadata.name", "takes KAGENT_NAMESPACE from another source"},
		"environment from": {"        volumeMounts:", "        envFrom: [{secretRef: {name: extra}}]\n        volumeMounts:", "loads environment from envFrom"},
		"extra mount":      {"        - {name: kagent-token, mountPath: /var/run/secrets/tokens}", "        - {name: kagent-token, mountPath: /var/run/secrets/tokens}\n        - {name: config, mountPath: /skills}", "mounts /skills"},
		"sidecar":          {"      volumes:", "      - {name: proxy, image: example.com/proxy:1}\n      volumes:", "runs containers besides kagent's"},
		"host aliases":     {"      volumes:", "      hostAliases: [{ip: 203.0.113.9, hostnames: [probe-gov-relay.orka-system.svc]}]\n      volumes:", "sets pod field hostAliases"},
		"dns override":     {"      volumes:", "      dnsPolicy: None\n      volumes:", "uses DNS policy None"},
		"lifecycle hook":   {"        volumeMounts:", "        lifecycle: {postStart: {exec: {command: [sh]}}}\n        volumeMounts:", "sets container field lifecycle"},
		"exec probe":       {"        volumeMounts:", "        readinessProbe: {exec: {command: [sh]}}\n        volumeMounts:", "runs a command in its readinessProbe"},
		"other Agent name": {"{name: KAGENT_NAME, value: probe-gov}", "{name: KAGENT_NAME, value: probe}", "sets KAGENT_NAME to another Agent"},
		"telemetry export": {`        - {name: OTEL_TRACING_ENABLED, value: "false"}`, `        - {name: OTEL_TRACING_ENABLED, value: "false"}` + "\n" + `        - {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "http://collector.example.com:4317"}`, "sets environment variable OTEL_EXPORTER_OTLP_ENDPOINT"},
	} {
		changed := strings.Replace(governedKagentV0Fixture, test.old, test.new, 1)
		if changed == governedKagentV0Fixture {
			t.Fatalf("%s: fixture has no %q", name, test.old)
		}
		if mode := classifyV0Fixture(t, changed, governedRelay); mode.Mode != kagentModeObserved || !strings.Contains(strings.Join(mode.Reasons, "; "), test.reason) {
			t.Errorf("%s: mode = %+v, want observed because %s", name, mode, test.reason)
		}
	}
	// What is outside the compiled configuration still counts.
	for name, test := range map[string]struct{ old, new, reason string }{
		"skills":         {"  type: Declarative\n", "  type: Declarative\n  skills: {refs: [ghcr.io/x/skills:1]}\n", "loads skills"},
		"deployment env": {"    runtime: go\n", "    runtime: go\n    deployment: {env: [{name: STS_WELL_KNOWN_URI, value: http://sts}]}\n", "deployment sets env"},
		"image registry": {"    runtime: go\n", "    runtime: go\n    deployment: {imageRegistry: example.com}\n", "deployment sets imageRegistry"},
	} {
		changed := strings.Replace(governedKagentV0Fixture, test.old, test.new, 1)
		if mode := classifyV0Fixture(t, changed, governedRelay); mode.Mode != kagentModeObserved || !strings.Contains(strings.Join(mode.Reasons, "; "), test.reason) {
			t.Errorf("%s: mode = %+v, want observed because %s", name, mode, test.reason)
		}
	}
	// The pinned instruction is the compiled one, wherever kagent read it from.
	fromConfigMap := strings.Replace(governedKagentV0Fixture, "systemMessage: You call Orka tools.", "systemMessageFrom: {type: ConfigMap, name: prompts, key: gov}", 1)
	if mode := classifyV0Fixture(t, fromConfigMap, governedRelay); mode.Mode != kagentModeGoverned {
		t.Errorf("a system message from a ConfigMap was not governed: %+v", mode)
	}
}

// The controller-produced pin from kagent v0.10.3's translator golden
// agent_with_passthrough (go/core/internal/controller/translator/agent/
// testdata/outputs), whose ModelConfig has no status.secretHash.
const (
	kagentGoldenPassthroughConfig = `{"model":{"type":"openai","model":"gpt-4o","api_key_passthrough":true,"base_url":"","temperature":0.7},"description":"","instruction":"You are a helpful assistant.","stream":false}`
	kagentGoldenPassthroughCard   = `{
  "defaultInputModes": [
    "text"
  ],
  "defaultOutputModes": [
    "text"
  ],
  "description": "",
  "name": "passthrough_agent",
  "version": "",
  "skills": [],
  "capabilities": {
    "streaming": true
  },
  "supportedInterfaces": [
    {
      "url": "http://passthrough-agent.test:8080",
      "protocolBinding": "JSONRPC",
      "protocolVersion": "0.3"
    },
    {
      "url": "http://passthrough-agent.test:8080",
      "protocolBinding": "JSONRPC",
      "protocolVersion": "1.0"
    }
  ],
  "url": "http://passthrough-agent.test:8080",
  "protocolVersion": "0.3",
  "preferredTransport": "JSONRPC"
}`
	kagentGoldenPassthroughHash = "15376076214432010811"
)

func TestKagentV0CompiledConfigReproducesKagentsHash(t *testing.T) {
	secret := map[string]any{"data": map[string]any{
		"config.json":     base64.StdEncoding.EncodeToString([]byte(kagentGoldenPassthroughConfig)),
		"agent-card.json": base64.StdEncoding.EncodeToString([]byte(kagentGoldenPassthroughCard)),
	}}
	unreconciled := map[string]any{"status": map[string]any{}}
	if config, problems := kagentV0CompiledConfig(secret, unreconciled, "passthrough-agent", "default-model-config", kagentGoldenPassthroughHash); len(problems) != 0 || config == nil {
		t.Fatalf("kagent's own pin was not reproduced: %v", problems)
	}
	// The ModelConfig's secret hash is part of the pin: a reconciled one with
	// no secrets contributes the SHA-256 of nothing.
	reconciled := map[string]any{"status": map[string]any{"secretHash": noSecretsHash}}
	if _, problems := kagentV0CompiledConfig(secret, reconciled, "passthrough-agent", "default-model-config", kagentGoldenPassthroughHash); !strings.Contains(strings.Join(problems, ";"), "not the pinned") {
		t.Fatalf("a different ModelConfig secret hash still matched: %v", problems)
	}
}

func TestWrapKagentV0AgentsRefusesRuntimesTheWrapperCannotRun(t *testing.T) {
	for name, test := range map[string]struct{ old, new, want string }{
		"python runtime": {"runtime: go", "runtime: python", "the Python runtime cannot cancel a task and saves request headers"},
		"no runtime":     {"    runtime: go\n", "", "sets no declarative.runtime, so kagent runs it on the Python runtime"},
		"byo agent":      {"type: Declarative", "type: BYO", "it is a BYO Agent; only Declarative Agents"},
		"no type":        {"  type: Declarative\n", "", "it sets no spec.type"},
		"kagent approval": {
			"allowedHeaders: [Authorization]}", "allowedHeaders: [Authorization], requireApproval: [probe-orka]}",
			"tool 0 requires kagent approval (requireApproval), which a wrapped Agent cannot answer",
		},
		"replicas asked": {
			"    runtime: go\n", "    runtime: go\n    deployment: {replicas: 2}\n",
			"it asks for 2 replicas (declarative.deployment.replicas); kagent v0 cancels a task only in the pod that runs it",
		},
		"sandbox network": {
			"  type: Declarative\n", "  type: Declarative\n  sandbox: {network: {allowedDomains: [api.github.com]}}\n",
			"it lets the code its model runs reach the network (spec.sandbox.network.allowedDomains)",
		},
		"replicas deployed": {"spec:\n  replicas: 1\n  template:", "spec:\n  replicas: 2\n  template:", "its Deployment runs 1 replicas of 2"},
		"old pods remain":   {"status: {observedGeneration: 4, replicas: 1", "status: {observedGeneration: 4, replicas: 2", "its Deployment runs 2 replicas of 1"},
		"no pods":           {"status: {observedGeneration: 4, replicas: 1", "status: {observedGeneration: 4, replicas: 0", "its Deployment runs 0 replicas of 1"},
		"no deployment": {
			"metadata: {name: probe-gov, namespace: kagent, generation: 4}", "metadata: {name: other, namespace: kagent, generation: 4}",
			"its Deployment is not in the input, so the import cannot confirm it runs exactly one replica",
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := strings.Replace(governedKagentV0Fixture, test.old, test.new, 1)
			if changed == governedKagentV0Fixture {
				t.Fatalf("fixture has no %q", test.old)
			}
			// The refusal applies to observed Agents too: no relay is named.
			if _, _, err := wrapKagentAgents([]byte(changed), "", kagentRevisionEvidence{}); err == nil ||
				!strings.Contains(err.Error(), "kagent Agent kagent/probe-gov cannot be wrapped") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

// The broker reads a v0 Agent's sessions and tasks from the controller, so in
// either mode the Agent must persist them to the one the wrapper calls.
func TestWrapKagentV0AgentsRequireControllerPersistence(t *testing.T) {
	const url = `{name: KAGENT_URL, value: "http://kagent-controller.kagent:8083"}`
	for name, test := range map[string]struct{ old, new, want string }{
		"no persistence":  {"        - " + url + "\n", "", "does not set KAGENT_URL to a value"},
		"indirect value":  {url, "{name: KAGENT_URL, valueFrom: {configMapKeyRef: {name: c, key: k}}}", "does not set KAGENT_URL to a value"},
		"other service":   {url, `{name: KAGENT_URL, value: "http://other.kagent:8083"}`, "persists sessions to a controller other than --control-plane-url"},
		"controller path": {url, `{name: KAGENT_URL, value: "http://kagent-controller.kagent:8083/other"}`, "persists sessions to a controller other than --control-plane-url"},
		// Kubernetes uses the last of repeated entries.
		"repeated value": {"        - " + url + "\n", "        - " + url + "\n" + `        - {name: KAGENT_URL, value: "http://other.kagent:8083"}` + "\n", "sets KAGENT_URL more than once"},
	} {
		changed := strings.Replace(governedKagentV0Fixture, test.old, test.new, 1)
		if changed == governedKagentV0Fixture {
			t.Fatalf("%s: fixture has no %q", name, test.old)
		}
		// No relay is named, so the Agent would otherwise wrap as observed.
		if _, _, err := wrapKagentAgents([]byte(changed), "", kagentRevisionEvidence{Controller: v0Controller}); err == nil ||
			!strings.Contains(err.Error(), "cannot be wrapped") || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v, want %q", name, err, test.want)
		}
	}
	// Without --control-plane-url only the value's presence can be checked.
	other := strings.Replace(governedKagentV0Fixture, url, `{name: KAGENT_URL, value: "http://other.kagent:8083"}`, 1)
	if _, _, err := wrapKagentAgents([]byte(other), "", kagentRevisionEvidence{}); err != nil {
		t.Errorf("an Agent wrapped without --control-plane-url was refused: %v", err)
	}
}

func TestImportKagentAgentsKeepsEachVersionsObjectsApart(t *testing.T) {
	mixed := governedKagentV0Fixture + "---\napiVersion: api.kagent.dev/v1alpha3\nkind: Agent\nmetadata: {name: v1-agent, namespace: kagent}\n"
	wrapped, _, err := wrapKagentAgents([]byte(mixed), governedRelay, kagentRevisionEvidence{Controller: v0Controller})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 2 || wrapped[0].Version != kagentVersionV0 || wrapped[0].Mode.Mode != kagentModeGoverned || wrapped[1].Version != "" {
		t.Fatalf("wrapped = %+v, want a governed v0 and a v1 Agent", wrapped)
	}
	if got := decodeWrappedAgent(t, wrapped[0].Manifest).Annotations[kagentSourceAnnotation]; got != "kagent.dev/v1alpha2 kagent/probe-gov" {
		t.Errorf("v0 source annotation = %q", got)
	}
	// A v1 ModelConfig of the same name does not stand in for a v0 Agent's.
	v1Model := strings.Replace(translatableKagentV0Fixture, "apiVersion: kagent.dev/v1alpha2\nkind: ModelConfig", "apiVersion: api.kagent.dev/v1alpha3\nkind: ModelConfig", 1)
	if _, _, err := translateKagentAgents([]byte(v1Model), kagentTranslateOptions{Tools: map[string]string{"k8s/list_pods": "k8s-list-pods"}}); err == nil ||
		!strings.Contains(err.Error(), `model config "gpt" is not in the input`) {
		t.Fatalf("translate with only a v1 ModelConfig = %v", err)
	}
}

func TestAgentImportKagentCmdWritesV0WrapperConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(governedKagentV0Fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-f", path, "--dry-run", "--wrapper-config-dir", dir, "--control-plane-url", "http://kagent-controller.kagent.svc:8083",
		"--relay-url", governedRelay, "--model-upstream-url", "http://vekil.vekil-system.svc:8080/v1", "--model-upstream-model", "gpt-4.1"}
	if _, err := runKagentImport(t, args...); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, "probe-gov.kagent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if golden := `{
  "model": "kagent/kagent/probe-gov",
  "kagent": {
    "version": "v0",
    "controlPlaneURL": "http://kagent-controller.kagent.svc:8083",
    "agent": {
      "name": "probe-gov",
      "namespace": "kagent"
    },
    "expectedRevision": "` + governedV0Hash + `",
    "userIDPrefix": "orka"
  },
  "governed": {
    "upstreamBaseURL": "http://vekil.vekil-system.svc:8080/v1",
    "upstreamModel": "gpt-4.1"
  }
}
`; string(written) != golden {
		t.Fatalf("v0 wrapper configuration = %s", written)
	}
	rolling := strings.Replace(governedKagentV0Fixture, "updatedReplicas: 1", "updatedReplicas: 0", 1)
	if err := os.WriteFile(path, []byte(rolling), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), `has no config hash to pin (deployment "probe-gov" has not finished rolling out`) {
		t.Fatalf("unpinned v0 Agent error = %v", err)
	}
	// An observed v0 Agent needs no config Secret and is pinned the same way.
	observed := strings.Replace(governedKagentV0Fixture, "metadata: {name: probe-gov, namespace: kagent}\ndata:", "metadata: {name: other, namespace: kagent}\ndata:", 1)
	if err := os.WriteFile(path, []byte(observed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runKagentImport(t, args[:len(args)-4]...); err != nil {
		t.Fatal(err)
	}
	var config kagentWrapperConfig
	raw, _ := os.ReadFile(filepath.Join(dir, "probe-gov.kagent.json"))
	if err := json.Unmarshal(raw, &config); err != nil || config.Governed != nil || config.Kagent.Version != kagentVersionV0 || config.Kagent.ExpectedRevision != governedV0Hash {
		t.Fatalf("observed v0 wrapper configuration = %s (%v)", raw, err)
	}
}

const translatableKagentV0Fixture = `apiVersion: kagent.dev/v1alpha2
kind: ModelConfig
metadata: {name: gpt, namespace: kagent}
spec:
  provider: OpenAI
  model: gpt-4.1
  apiKeySecret: openai-key
  apiKeySecretKey: key
  openAI: {temperature: "0.2", maxTokens: 1024}
---
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata: {name: k8s-agent, namespace: kagent}
spec:
  type: Declarative
  description: Answers Kubernetes questions.
  declarative:
    runtime: python
    stream: true
    systemMessage: You are a Kubernetes expert.
    modelConfig: gpt
    deployment: {replicas: 2, resources: {limits: {cpu: "1"}}}
    tools:
    - type: McpServer
      mcpServer: {kind: RemoteMCPServer, apiGroup: kagent.dev, name: k8s, toolNames: [list_pods]}
`

func TestTranslateKagentV0AgentsMapsOnlyKnownFields(t *testing.T) {
	tools := map[string]string{"k8s/list_pods": "k8s-list-pods"}
	translated, _, err := translateKagentAgents([]byte(translatableKagentV0Fixture), kagentTranslateOptions{Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	agent := decodeWrappedAgent(t, translated[0].Manifest)
	if agent.Spec.Runtime != nil || agent.Spec.SystemPrompt == nil || agent.Spec.SystemPrompt.Inline != "You are a Kubernetes expert." ||
		agent.Spec.Model == nil || agent.Spec.Model.Provider != "openai" || agent.Spec.Model.Name != "gpt-4.1" ||
		agent.Spec.Model.Temperature == nil || *agent.Spec.Model.Temperature != 0.2 || agent.Spec.Model.MaxTokens == nil || *agent.Spec.Model.MaxTokens != 1024 ||
		len(agent.Spec.Tools) != 1 || agent.Spec.Tools[0].Name != "k8s-list-pods" || agent.Annotations[kagentSourceAnnotation] != "kagent.dev/v1alpha2 kagent/k8s-agent" {
		t.Fatalf("translated Agent = %+v", agent)
	}
	fromConfigMap := strings.Replace(translatableKagentV0Fixture, "systemMessage: You are a Kubernetes expert.", "systemMessageFrom: {type: ConfigMap, name: prompts, key: k8s}", 1)
	translated, _, err = translateKagentAgents([]byte(fromConfigMap), kagentTranslateOptions{Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if prompt := decodeWrappedAgent(t, translated[0].Manifest).Spec.SystemPrompt; prompt == nil || prompt.ConfigMapRef == nil ||
		prompt.ConfigMapRef.Name != "prompts" || prompt.ConfigMapRef.Key != "k8s" {
		t.Fatalf("system prompt = %+v", prompt)
	}
}

// Orka resolves a ConfigMap prompt in the Agent's own namespace, so a
// translated Agent must be created beside the ConfigMap it reads.
func TestAgentImportKagentCmdTranslateKeepsConfigMapPromptsBesideTheAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fromConfigMap := strings.Replace(translatableKagentV0Fixture, "systemMessage: You are a Kubernetes expert.", "systemMessageFrom: {type: ConfigMap, name: prompts, key: k8s}", 1)
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(fromConfigMap), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-f", path, "--translate", "--dry-run", "--tool", "k8s/list_pods=k8s-list-pods"}
	for namespace, want := range map[string]string{
		"":     "pass -n kagent",
		"team": `but Orka would look for it in "team"`,
	} {
		flags := args
		if namespace != "" {
			flags = append(slices.Clone(args), "-n", namespace)
		}
		if _, err := runKagentImport(t, flags...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("namespace %q: err = %v, want %q", namespace, err, want)
		}
	}
	if _, err := runKagentImport(t, append(slices.Clone(args), "-n", "kagent")...); err != nil {
		t.Fatalf("an Agent created beside its ConfigMap was refused: %v", err)
	}
}

func TestTranslateKagentV0AgentsRefusesEverythingItCannotMap(t *testing.T) {
	input := strings.NewReplacer(
		"    stream: true\n", "    stream: true\n    memory: {modelConfig: embed}\n    promptTemplate: {dataSources: [{name: p}]}\n",
		"systemMessage: You are a Kubernetes expert.", "systemMessageFrom: {type: Secret, name: prompts, key: k8s}",
		"resources: {limits: {cpu: \"1\"}}}", "env: [{name: KAGENT_PROPAGATE_TOKEN, value: \"true\"}]}",
		"toolNames: [list_pods]}", "toolNames: [list_pods], requireApproval: [list_pods], allowedHeaders: [Authorization]}\n      headersFrom: [{name: X, value: y}]\n    - type: Agent\n      agent: {name: helper}",
		"  apiKeySecretKey: key\n", "  apiKeySecretKey: key\n  apiKeyPassthrough: false\n",
		"  description: Answers Kubernetes questions.\n", "  description: Answers Kubernetes questions.\n  skills: {refs: [s]}\n",
	).Replace(translatableKagentV0Fixture)
	_, _, err := translateKagentAgents([]byte(input), kagentTranslateOptions{})
	if err == nil {
		t.Fatal("an untranslatable v0 Agent was accepted")
	}
	for _, want := range []string{
		"declarative.memory has no Orka equivalent", "declarative.promptTemplate has no Orka equivalent",
		"reads its system message from a Secret", "declarative.deployment.env changes the agent's runtime",
		"sets mcpServer.requireApproval", "sets mcpServer.allowedHeaders", "tool 0 sets headersFrom", `tool 1 is a "Agent" tool`,
		"sets apiKeyPassthrough, which has no Orka equivalent", "spec.skills has no Orka equivalent",
		"tool k8s/list_pods has no Orka Tool", "Translation maps only these fields",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	if _, _, err := translateKagentAgents([]byte(strings.Replace(translatableKagentV0Fixture, "type: Declarative", "type: BYO", 1)), kagentTranslateOptions{}); err == nil ||
		!strings.Contains(err.Error(), "it is a \"BYO\" Agent; only Declarative Agents translate") {
		t.Fatalf("BYO Agent error = %v", err)
	}
}

func TestKagentV0GovernedNeedsTheControllerItPersistsTo(t *testing.T) {
	wrapped, _, err := wrapKagentAgents([]byte(governedKagentV0Fixture), governedRelay, kagentRevisionEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	if mode := wrapped[0].Mode; mode.Mode != kagentModeObserved || !strings.Contains(strings.Join(mode.Reasons, ";"), "persists sessions to a controller other than --control-plane-url") {
		t.Fatalf("a v0 Agent was governed without --control-plane-url: %+v", mode)
	}
	for _, same := range []string{"http://kagent-controller.kagent:8083", "http://kagent-controller.kagent.svc.cluster.local:8083/"} {
		if !kagentSameService(same, v0Controller) {
			t.Errorf("%s does not match %s", same, v0Controller)
		}
	}
	for _, other := range []string{
		"http://kagent-controller.kagent:8084", "https://kagent-controller.kagent:8083", "http://kagent-controller.other:8083", "http://user:pass@kagent-controller.kagent:8083",
		// kagent appends its API paths to the URL as given.
		"http://kagent-controller.kagent:8083/other", "http://kagent-controller.kagent:8083/%2F", "http://kagent-controller.kagent:8083?x=1",
		"http://kagent-controller.kagent:8083/?", "http://kagent-controller.kagent:8083#f",
	} {
		if kagentSameService(other, v0Controller) {
			t.Errorf("%s matched %s", other, v0Controller)
		}
	}
}
