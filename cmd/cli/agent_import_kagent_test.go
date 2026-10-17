/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const kagentImportFixture = `apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata:
  name: k8s-agent
  namespace: kagent
spec:
  template:
    ref:
      name: shared-context
  harnessRef:
    name: kagent
---
apiVersion: api.kagent.dev/v1alpha3
kind: AgentTemplate
metadata:
  name: shared-context
  namespace: kagent
spec:
  modelConfig:
    name: default-model-config
---
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata:
  name: default-model-config
  namespace: kagent
spec:
  provider: OpenAI
  model: gpt-4.1
---
apiVersion: v1
kind: List
items:
- apiVersion: api.kagent.dev/v1alpha3
  kind: Agent
  metadata:
    name: helm-agent
    namespace: kagent
  spec:
    harnessRef:
      name: kagent
    template:
      inline:
        modelConfig:
          name: default-model-config
        systemPrompt: You manage Helm releases.
- apiVersion: api.kagent.dev/v1alpha3
  kind: RemoteMCPServer
  metadata:
    name: kagent-tools
    namespace: kagent
`

func decodeWrappedAgent(t *testing.T, manifest map[string]any) corev1alpha1.Agent {
	t.Helper()
	out, err := sigsyaml.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	var agent corev1alpha1.Agent
	if err := sigsyaml.UnmarshalStrict(out, &agent); err != nil {
		t.Fatalf("manifest is not a strict Orka Agent: %v\n%s", err, out)
	}
	return agent
}

func TestWrapKagentAgentsConvertsSupportedVersions(t *testing.T) {
	wrapped, skipped, err := wrapKagentAgents([]byte(kagentImportFixture), "", kagentRevisionEvidence{})
	if err != nil {
		t.Fatalf("wrapKagentAgents() error: %v", err)
	}
	if want := "api.kagent.dev/v1alpha3 AgentTemplate (1), api.kagent.dev/v1alpha3 ModelConfig (1), api.kagent.dev/v1alpha3 RemoteMCPServer (1)"; skipped != want {
		t.Errorf("skipped = %q, want %q", skipped, want)
	}
	want := []struct{ name, runtime, source string }{
		{"k8s-agent", "k8s-agent-kagent-runtime", "api.kagent.dev/v1alpha3 kagent/k8s-agent"},
		{"helm-agent", "helm-agent-kagent-runtime", "api.kagent.dev/v1alpha3 kagent/helm-agent"},
	}
	if len(wrapped) != len(want) {
		t.Fatalf("wrapped %d agents, want %d", len(wrapped), len(want))
	}
	for i, w := range want {
		agent := decodeWrappedAgent(t, wrapped[i].Manifest)
		if agent.APIVersion != "core.orka.ai/v1alpha1" || agent.Kind != "Agent" {
			t.Errorf("agent %d type = %s %s", i, agent.APIVersion, agent.Kind)
		}
		if agent.Name != w.name || agent.Namespace != "" {
			t.Errorf("agent %d = %s/%s, want name %s and no namespace", i, agent.Namespace, agent.Name, w.name)
		}
		if agent.Annotations[kagentSourceAnnotation] != w.source {
			t.Errorf("agent %d source annotation = %q, want %q", i, agent.Annotations[kagentSourceAnnotation], w.source)
		}
		runtime := agent.Spec.Runtime
		if runtime == nil || runtime.RuntimeRef == nil || runtime.RuntimeRef.Name != w.runtime || runtime.Type != "" {
			t.Errorf("agent %d runtime = %+v, want only runtimeRef %s", i, runtime, w.runtime)
		}
		if wrapped[i].RuntimeName != w.runtime {
			t.Errorf("agent %d RuntimeName = %q, want %q", i, wrapped[i].RuntimeName, w.runtime)
		}
		if agent.Spec.SystemPrompt != nil || agent.Spec.Model != nil || agent.Spec.Tools != nil {
			t.Errorf("agent %d carries behavior that kagent owns: %+v", i, agent.Spec)
		}
	}
}

func TestWrapKagentAgentsRejectsUnsafeInput(t *testing.T) {
	agentDoc := func(apiVersion, namespace, name string) string {
		return "apiVersion: " + apiVersion + "\nkind: Agent\nmetadata:\n  namespace: " + namespace + "\n  name: " + name + "\n"
	}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "unsupported kagent version",
			input: agentDoc("kagent.dev/v1alpha1", "kagent", "old-agent"),
			want:  `unsupported kagent Agent version "kagent.dev/v1alpha1"`,
		},
		{
			name:  "kagent v0 agent the wrapper cannot run",
			input: agentDoc("kagent.dev/v1alpha2", "kagent", "helm-agent"),
			want:  "kagent Agent kagent/helm-agent cannot be wrapped: it sets no spec.type",
		},
		{
			name:  "same name from two namespaces",
			input: agentDoc("api.kagent.dev/v1alpha3", "team-a", "triage") + "---\n" + agentDoc("api.kagent.dev/v1alpha3", "team-b", "triage"),
			want:  `kagent Agents team-a/triage and team-b/triage would both become Orka Agent "triage"`,
		},
		{
			name:  "missing name",
			input: "apiVersion: api.kagent.dev/v1alpha3\nkind: Agent\nmetadata:\n  namespace: kagent\n",
			want:  "missing metadata.name",
		},
		{
			name:  "runtime name too long",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", strings.Repeat("a", 250)),
			want:  "is not a valid Orka resource name",
		},
		{
			name:  "no kagent agents",
			input: "apiVersion: api.kagent.dev/v1alpha3\nkind: ModelConfig\nmetadata:\n  name: m\n",
			want:  "no kagent Agents found",
		},
		{
			name:  "orka agent is not imported",
			input: agentDoc("core.orka.ai/v1alpha1", "default", "native"),
			want:  "no kagent Agents found",
		},
		{
			name:  "malformed yaml",
			input: "apiVersion: [unterminated\n",
			want:  "parsing manifest document 1",
		},
		{
			name:  "duplicate yaml key",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", "first") + "  name: second\n",
			want:  "parsing manifest document 1",
		},
		{
			name:  "duplicate json key",
			input: `{"apiVersion":"kagent.dev/v1alpha1","apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"a"}}`,
			want:  `parsing manifest document 1: duplicate key "apiVersion"`,
		},
		{
			name: "duplicate key in a later json value",
			input: `{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"a"}}` + "\n" +
				`{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"b","name":"c"}}`,
			want: `parsing manifest document 2: duplicate key "name"`,
		},
		{
			name:  "trailing garbage after json",
			input: `{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"a"}} trailing`,
			want:  "parsing manifest document 1",
		},
		{
			name: "comment before concatenated json",
			input: "# export\n" + `{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"a"}}` + "\n" +
				`{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"name":"b"}}`,
			want: "parsing manifest document 1",
		},
		{
			name:  "document end marker hides a second document",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", "a") + "...\n" + agentDoc("api.kagent.dev/v1alpha3", "kagent", "b"),
			want:  "parsing manifest document 1",
		},
		{
			name:  "inline document after separator",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", "a") + "--- {apiVersion: api.kagent.dev/v1alpha3, kind: Agent, metadata: {name: b}}\n",
			want:  "reading manifest document 1",
		},
		{
			name:  "non-object list item",
			input: "apiVersion: v1\nkind: List\nitems:\n- " + strings.ReplaceAll(strings.TrimSuffix(agentDoc("api.kagent.dev/v1alpha3", "kagent", "ok"), "\n"), "\n", "\n  ") + "\n- not-an-object\n",
			want:  "List item 1 is not an object",
		},
		{
			name:  "scalar document",
			input: agentDoc("api.kagent.dev/v1alpha3", "kagent", "ok") + "---\njust-a-string\n",
			want:  "parsing manifest document 2: not an object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := wrapKagentAgents([]byte(tt.input), "", kagentRevisionEvidence{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestRequireSingleYAMLDocument(t *testing.T) {
	if err := requireSingleYAMLDocument([]byte("# only a comment\n")); err != nil {
		t.Errorf("comment-only document: %v", err)
	}
	if err := requireSingleYAMLDocument([]byte("a: 1\n")); err != nil {
		t.Errorf("single document: %v", err)
	}
	if err := requireSingleYAMLDocument([]byte("a: 1\n---\nb: 2\n")); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("two documents: error = %v, want a multiple-document error", err)
	}
}

func TestWrapKagentAgentsReadsEveryJSONValue(t *testing.T) {
	input := `{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"namespace":"kagent","name":"a"}}
{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"namespace":"kagent","name":"b"}}
---
apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata:
  namespace: kagent
  name: c
---
{apiVersion: api.kagent.dev/v1alpha3, kind: Agent, metadata: {namespace: kagent, name: d}}
`
	wrapped, _, err := wrapKagentAgents([]byte(input), "", kagentRevisionEvidence{})
	if err != nil {
		t.Fatalf("wrapKagentAgents() error: %v", err)
	}
	names := make([]string, 0, len(wrapped))
	for _, agent := range wrapped {
		names = append(names, metadataName(agent.Manifest))
	}
	if got := strings.Join(names, ","); got != "a,b,c,d" {
		t.Fatalf("wrapped Agents = %s, want a,b,c,d", got)
	}
}

func TestWrapKagentAgentsAcceptsListsAndJSONEscapes(t *testing.T) {
	input := `apiVersion: api.kagent.dev/v1alpha3
kind: AgentList
items:
- metadata:
    namespace: kagent
    name: typed
---
{"apiVersion":"api.kagent.dev/v1alpha3","kind":"Agent","metadata":{"namespace":"kagent","name":"emoji","annotations":{"icon":"😀"}}}
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: List
  items:
  - apiVersion: api.kagent.dev/v1alpha3
    kind: Agent
    metadata:
      namespace: kagent
      name: nested
`
	wrapped, skipped, err := wrapKagentAgents([]byte(input), "", kagentRevisionEvidence{})
	if err != nil {
		t.Fatalf("wrapKagentAgents() error: %v", err)
	}
	if skipped != "" {
		t.Errorf("skipped = %q, want nothing skipped", skipped)
	}
	if len(wrapped) != 3 || wrapped[0].Source != "kagent/typed" || wrapped[1].Source != "kagent/emoji" || wrapped[2].Source != "kagent/nested" {
		t.Fatalf("wrapped = %+v, want kagent/typed, kagent/emoji and kagent/nested", wrapped)
	}
	if got := decodeWrappedAgent(t, wrapped[0].Manifest).Annotations[kagentSourceAnnotation]; got != "api.kagent.dev/v1alpha3 kagent/typed" {
		t.Errorf("typed list item source = %q, want the list's apiVersion", got)
	}
}

func writeKagentFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(kagentImportFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestAgentImportKagentCmdDryRunPrintsManifests(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeKagentFixture(t)

	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"agent", "import", "kagent", "-f", path, "--dry-run", "-n", "team"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	objects, err := decodeKubernetesObjects(stdout.Bytes())
	if err != nil {
		t.Fatalf("dry-run output is not a manifest stream: %v\n%s", err, stdout.String())
	}
	if len(objects) != 2 {
		t.Fatalf("printed %d manifests, want 2:\n%s", len(objects), stdout.String())
	}
	for _, obj := range objects {
		agent := decodeWrappedAgent(t, obj)
		if agent.Namespace != "team" {
			t.Errorf("Agent %s namespace = %q, want team", agent.Name, agent.Namespace)
		}
	}
	if !strings.Contains(stderr.String(), "Skipped objects that are not kagent Agents") {
		t.Errorf("stderr = %q, want a skipped-objects note", stderr.String())
	}
}

// kagentImportAPIServer is a stateful fake of the Orka Agents API.
type kagentImportAPI struct {
	mu      sync.Mutex
	agents  map[string]map[string]any
	failOn  string
	creates int
	updates int
}

func kagentImportAPIServer(t *testing.T, api *kagentImportAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		name := strings.TrimPrefix(r.URL.Path, agentsAPIPath+"/")
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		switch {
		case r.Method == http.MethodGet && name != r.URL.Path:
			agent, ok := api.agents[name]
			if !ok {
				http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(agent) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == agentsAPIPath:
			if metadataName(body) == api.failOn {
				http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			// The server defaults fields the import never sets.
			spec, _ := body["spec"].(map[string]any)
			spec["resources"] = map[string]any{}
			defaultKagentImportTools(spec)
			api.agents[metadataName(body)] = body
			api.creates++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(body) //nolint:errcheck
		case r.Method == http.MethodPut && name != r.URL.Path:
			spec, _ := body["spec"].(map[string]any)
			defaultKagentImportTools(spec)
			api.agents[name]["spec"] = spec
			api.updates++
			json.NewEncoder(w).Encode(api.agents[name]) //nolint:errcheck
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// defaultKagentImportTools applies the CRD default tools[].enabled=true.
func defaultKagentImportTools(spec map[string]any) {
	tools, _ := spec["tools"].([]any)
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok {
			if _, set := tool["enabled"]; !set {
				tool["enabled"] = true
			}
		}
	}
}

func runKagentImport(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"agent", "import", "kagent"}, args...))
	err := root.Execute()
	return stdout.String(), err
}

func TestAgentImportKagentCmdCreatesAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	api := &kagentImportAPI{agents: map[string]map[string]any{}}
	srv := kagentImportAPIServer(t, api)
	out, err := runKagentImport(t, "-f", writeKagentFixture(t), "--server", srv.URL, "-n", "team")
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if api.creates != 2 {
		t.Fatalf("created %d Agents, want 2", api.creates)
	}
	for _, body := range api.agents {
		if agent := decodeWrappedAgent(t, body); agent.Namespace != "team" {
			t.Errorf("Agent %s namespace = %q, want team", agent.Name, agent.Namespace)
		}
	}
	if !strings.Contains(out, "Agent created: k8s-agent (wraps kagent Agent kagent/k8s-agent; observed: ") ||
		!strings.Contains(out, "needs AgentRuntime k8s-agent-kagent-runtime)") {
		t.Errorf("stdout = %q, want the created Agent, its mode, and its required AgentRuntime", out)
	}
}

func TestAgentImportKagentCmdResumesAfterPartialFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	api := &kagentImportAPI{agents: map[string]map[string]any{}, failOn: "helm-agent"}
	srv := kagentImportAPIServer(t, api)
	path := writeKagentFixture(t)
	if _, err := runKagentImport(t, "-f", path, "--server", srv.URL, "-n", "team"); err == nil || !strings.Contains(err.Error(), `creating Agent "helm-agent"`) {
		t.Fatalf("error = %v, want the failed Agent", err)
	}
	api.failOn = ""
	out, err := runKagentImport(t, "-f", path, "--server", srv.URL, "-n", "team")
	if err != nil {
		t.Fatalf("re-run error: %v", err)
	}
	if !strings.Contains(out, "Agent unchanged: k8s-agent") || !strings.Contains(out, "Agent created: helm-agent") || api.creates != 2 || api.updates != 0 {
		t.Fatalf("re-run = %q (creates %d, updates %d)", out, api.creates, api.updates)
	}
}

func TestAgentImportKagentCmdUpdatesItsOwnAgentsOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	api := &kagentImportAPI{agents: map[string]map[string]any{}}
	srv := kagentImportAPIServer(t, api)
	path := writeKagentFixture(t)
	if _, err := runKagentImport(t, "-f", path, "--server", srv.URL, "-n", "team"); err != nil {
		t.Fatal(err)
	}
	api.agents["k8s-agent"]["spec"] = map[string]any{
		"runtime": map[string]any{"runtimeRef": map[string]any{"name": "drifted"}},
		"session": map[string]any{"maxMessages": float64(10)},
	}
	out, err := runKagentImport(t, "-f", path, "--server", srv.URL, "-n", "team")
	if err != nil || !strings.Contains(out, "Agent updated: k8s-agent") || api.updates != 1 {
		t.Fatalf("drift repair = %q %v (updates %d)", out, err, api.updates)
	}
	// The repair replaces only the fields the import owns.
	if repaired := decodeWrappedAgent(t, api.agents["k8s-agent"]); repaired.Spec.Runtime == nil || repaired.Spec.Runtime.RuntimeRef == nil ||
		repaired.Spec.Runtime.RuntimeRef.Name != "k8s-agent-kagent-runtime" || repaired.Spec.Session == nil || repaired.Spec.Session.MaxMessages != 10 {
		t.Fatalf("repaired spec = %+v", repaired.Spec)
	}
	api.agents["helm-agent"]["metadata"] = map[string]any{"name": "helm-agent", "annotations": map[string]any{}}
	if _, err := runKagentImport(t, "-f", path, "--server", srv.URL, "-n", "team"); err == nil || !strings.Contains(err.Error(), `agent "helm-agent" already exists and does not wrap kagent Agent kagent/helm-agent`) {
		t.Fatalf("foreign Agent error = %v", err)
	}
}

const governedKagentFixture = `apiVersion: api.kagent.dev/v1alpha3
kind: Harness
metadata: {name: kagent, namespace: kagent, uid: harness-uid, generation: 1}
spec: {kagent: {}}
---
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata: {name: relay-model, namespace: kagent, uid: model-uid, generation: 2}
spec:
  provider: OpenAI
  model: probe
  apiKeyPassthrough: true
  openAI: {baseUrl: "http://probe-gov-relay.orka-system.svc:8092/v1"}
---
apiVersion: api.kagent.dev/v1alpha3
kind: RemoteMCPServer
metadata:
  name: orka-relay
  namespace: kagent
  uid: mcp-uid
  generation: 1
  labels: {kagent.dev/discovery: disabled}
spec:
  protocol: STREAMABLE_HTTP
  url: http://probe-gov-relay.orka-system.svc:8092/mcp
  timeout: 30m
---
apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata: {name: probe-gov, namespace: kagent, uid: agent-uid, generation: 3}
spec:
  harnessRef: {name: kagent}
  template:
    inline:
      modelConfig: {name: relay-model}
      tools:
      - mcp:
          server: {kind: RemoteMCPServer, name: orka-relay}
          tools: [probe-orka]
status:
  observedGeneration: 3
  desiredRevision: 11d7665dfb79fda2112d61af3b32b10f9c8a196f5741e1f7e8e8e686147b927b
  latestSuccessfulRevision: 11d7665dfb79fda2112d61af3b32b10f9c8a196f5741e1f7e8e8e686147b927b
`

const governedRelay = "http://{name}-relay.orka-system.svc:8092"

const governedRevision = "11d7665dfb79fda2112d61af3b32b10f9c8a196f5741e1f7e8e8e686147b927b"

var governedEvidence = kagentRevisionEvidence{
	Revisions: map[string][]string{governedRevision: {
		"http://probe-cpfilter.probe.svc:8083", "http://probe-gov-relay.orka-system.svc:8092", "https://api.openai.com:443",
	}},
	Sources: map[string]kagentRevisionSource{governedRevision: {AgentUID: "agent-uid", Inputs: []kagentRevisionInput{
		{APIVersion: kagentV1APIVersion, Kind: "Harness", Name: "kagent", UID: "harness-uid", Generation: 1},
		{APIVersion: kagentV1APIVersion, Kind: "ModelConfig", Name: "relay-model", UID: "model-uid", Generation: 2},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "prompt", UID: "configmap-uid"},
		{APIVersion: kagentV1APIVersion, Kind: "RemoteMCPServer", Name: "orka-relay", UID: "mcp-uid", Generation: 1},
	}}},
	Accepted: []string{"http://probe-cpfilter.probe.svc:8083", "https://api.openai.com:443"},
}

// A v1 binding that requires kagent approval pauses every call for a prompt
// the wrapper cannot answer, so the Agent cannot be wrapped in either mode,
// whether its template is inline or referenced.
func TestKagentV1ApprovalBindingsAreRefused(t *testing.T) {
	inline := strings.Replace(governedKagentFixture, "tools: [probe-orka]", "tools: [probe-orka]\n          requireApproval: [probe-orka]", 1)
	referenced := strings.Replace(inline, `  template:
    inline:
      modelConfig: {name: relay-model}
      tools:
      - mcp:
          server: {kind: RemoteMCPServer, name: orka-relay}
          tools: [probe-orka]
          requireApproval: [probe-orka]
`, "  template:\n    ref: {name: gated}\n", 1) + `---
apiVersion: api.kagent.dev/v1alpha3
kind: AgentTemplate
metadata: {name: gated, namespace: kagent}
spec:
  modelConfig: {name: relay-model}
  tools:
  - mcp:
      server: {kind: RemoteMCPServer, name: orka-relay}
      tools: [probe-orka]
      requireApproval: [probe-orka]
`
	for name, input := range map[string]string{"inline": inline, "referenced": referenced} {
		if !strings.Contains(input, "requireApproval") || (name == "referenced" && !strings.Contains(input, "ref: {name: gated}")) {
			t.Fatalf("%s fixture did not change", name)
		}
		for _, relay := range []string{governedRelay, ""} {
			if _, _, err := wrapKagentAgents([]byte(input), relay, governedEvidence); err == nil || !strings.Contains(err.Error(), "requires kagent approval") {
				t.Fatalf("%s template, relay %q: err = %v", name, relay, err)
			}
		}
	}
}

// kagent compiles a subagent template into its parent's runtime, so an
// approval binding anywhere in the tree pauses the parent's turn.
func TestKagentV1SubagentApprovalBindingsAreRefused(t *testing.T) {
	subagent := "      - subAgent: {name: helper, description: Helps., templateRef: {name: helper}}\n"
	templates := `---
apiVersion: api.kagent.dev/v1alpha3
kind: AgentTemplate
metadata: {name: helper, namespace: kagent}
spec:
  modelConfig: {name: relay-model}
  tools:
  - subAgent: {name: nested, description: Nests., templateRef: {name: nested}}
---
apiVersion: api.kagent.dev/v1alpha3
kind: AgentTemplate
metadata: {name: nested, namespace: kagent}
spec:
  modelConfig: {name: relay-model}
  tools:
  - subAgent: {name: helper, description: Loops., templateRef: {name: helper}}
  - mcp:
      server: {kind: RemoteMCPServer, name: orka-relay}
      requireApproval: true
`
	inline := strings.Replace(governedKagentFixture, "      tools:\n      - mcp:", "      tools:\n"+subagent+"      - mcp:", 1) + templates
	referenced := strings.Replace(inline, `  template:
    inline:
      modelConfig: {name: relay-model}
      tools:
`+subagent+`      - mcp:
          server: {kind: RemoteMCPServer, name: orka-relay}
          tools: [probe-orka]
`, "  template:\n    ref: {name: root}\n", 1) + `---
apiVersion: api.kagent.dev/v1alpha3
kind: AgentTemplate
metadata: {name: root, namespace: kagent}
spec:
  modelConfig: {name: relay-model}
  tools:
  - subAgent: {name: helper, description: Helps., templateRef: {name: helper}}
`
	for name, input := range map[string]string{"inline": inline, "referenced": referenced} {
		if input == governedKagentFixture+templates || (name == "referenced" && !strings.Contains(input, "ref: {name: root}")) {
			t.Fatalf("%s fixture did not change", name)
		}
		want := `subagent template "nested" tool 1 requires kagent approval`
		if _, _, err := wrapKagentAgents([]byte(input), "", governedEvidence); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s template: err = %v, want %q", name, err, want)
		}
	}
	// Without the approval binding the same tree wraps.
	clean := strings.Replace(inline, "      requireApproval: true\n", "", 1)
	if _, _, err := wrapKagentAgents([]byte(clean), "", governedEvidence); err != nil {
		t.Fatalf("a subagent tree without approval was refused: %v", err)
	}
}

func TestClassifyKagentAgentRequiresEveryEndpointOnOneRelay(t *testing.T) {
	wrapped, _, err := wrapKagentAgents([]byte(governedKagentFixture), governedRelay, governedEvidence)
	if err != nil {
		t.Fatal(err)
	}
	if mode := wrapped[0].Mode; mode.Mode != kagentModeGoverned || mode.RelayOrigin != "http://probe-gov-relay.orka-system.svc:8092" {
		t.Fatalf("mode = %+v", mode)
	}
	if wrapped, _, _ := wrapKagentAgents([]byte(governedKagentFixture), "", governedEvidence); wrapped[0].Mode.Mode != kagentModeObserved ||
		!strings.Contains(strings.Join(wrapped[0].Mode.Reasons, ";"), "no relay was named") {
		t.Fatalf("an Agent was governed without a named relay: %+v", wrapped[0].Mode)
	}
	for name, test := range map[string]struct{ old, new, reason string }{
		"claude harness":       {"spec: {kagent: {}}", "spec: {claude: {}}", `harness "kagent" is not the ADK (kagent) harness`},
		"memory":               {"spec: {kagent: {}}", "spec: {kagent: {memory: {modelConfig: {name: m}}}}", "enables memory"},
		"no passthrough":       {"apiKeyPassthrough: true", "apiKeyPassthrough: false", "does not pass the caller's bearer through"},
		"model elsewhere":      {"8092/v1", "8092/other", "does not point at a relay's /v1"},
		"short timeout":        {"timeout: 30m", "timeout: 30s", "needs a timeout longer than 20m0s"},
		"relay timeout":        {"timeout: 30m", "timeout: 20m", "needs a timeout longer than 20m0s"},
		"responses API":        {"8092/v1\"}", "8092/v1\", apiFormat: responses}", "uses the responses API"},
		"stale model config":   {"uid: model-uid, generation: 2", "uid: model-uid, generation: 3", `not compiled from the ModelConfig "relay-model"`},
		"replaced tools":       {"uid: mcp-uid", "uid: other-uid", `not compiled from the RemoteMCPServer "orka-relay"`},
		"recreated Agent":      {"uid: agent-uid", "uid: new-agent-uid", "not compiled for this Agent's metadata.uid"},
		"discovery enabled":    {"labels: {kagent.dev/discovery: disabled}", "labels: {}", "does not disable discovery"},
		"two relays":           {"svc:8092/mcp", "svc:9000/mcp", "not the relay http://probe-gov-relay.orka-system.svc:8092"},
		"lookalike endpoint":   {"http://probe-gov-relay.orka-system.svc:8092/v1", "http://ollama:11434/v1", "points at http://ollama:11434"},
		"stale revision":       {"  desiredRevision: 11d7665d", "  desiredRevision: 22d7665d", "not the one the current spec compiles to"},
		"unobserved spec":      {"observedGeneration: 3", "observedGeneration: 2", "has not observed the Agent's current generation"},
		"static model headers": {"apiKeyPassthrough: true", "apiKeyPassthrough: true\n  defaultHeaders: {Authorization: Bearer static}", "sends static headers"},
		"missing tool server":  {"name: orka-relay\n  namespace", "name: other\n  namespace", `tool server "orka-relay" is not in the input`},
	} {
		t.Run(name, func(t *testing.T) {
			changed := strings.Replace(governedKagentFixture, test.old, test.new, 1)
			if changed == governedKagentFixture {
				t.Fatalf("fixture has no %q", test.old)
			}
			wrapped, _, err := wrapKagentAgents([]byte(changed), governedRelay, governedEvidence)
			if err != nil {
				t.Fatal(err)
			}
			mode := wrapped[0].Mode
			if mode.Mode != kagentModeObserved || !strings.Contains(strings.Join(mode.Reasons, "; "), test.reason) {
				t.Fatalf("mode = %+v, want observed because %s", mode, test.reason)
			}
		})
	}
	override := strings.Replace(governedKagentFixture, "spec: {kagent: {}}", "spec: {kagent: {}, workload: {image: custom/adk:latest}}", 1)
	if wrapped, _, _ := wrapKagentAgents([]byte(override), governedRelay, governedEvidence); !strings.Contains(strings.Join(wrapped[0].Mode.Reasons, ";"), "overrides the workload image") {
		t.Fatalf("a workload override was governed: %+v", wrapped[0].Mode)
	}
	explicit := strings.Replace(governedKagentFixture, "8092/v1\"}", "8092/v1\", apiFormat: chatCompletions}", 1)
	if wrapped, _, _ := wrapKagentAgents([]byte(explicit), governedRelay, governedEvidence); wrapped[0].Mode.Mode != kagentModeGoverned {
		t.Fatalf("kagent's default chat completions format was not governed: %+v", wrapped[0].Mode)
	}
	// The pinned revision's compiled egress and source snapshot are the
	// boundary, not the current specs.
	for name, evidence := range map[string]kagentRevisionEvidence{
		"no evidence":   {Accepted: governedEvidence.Accepted, Sources: governedEvidence.Sources},
		"direct egress": {Revisions: map[string][]string{governedRevision: {"http://mcp.example.com:80"}}, Sources: governedEvidence.Sources},
		"no sources":    {Revisions: governedEvidence.Revisions, Accepted: governedEvidence.Accepted},
	} {
		if wrapped, _, _ := wrapKagentAgents([]byte(governedKagentFixture), governedRelay, evidence); wrapped[0].Mode.Mode != kagentModeObserved {
			t.Fatalf("%s: governed without proof of the revision's egress: %+v", name, wrapped[0].Mode)
		}
	}
}

func TestAgentImportKagentCmdWritesDeterministicWrapperConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(governedKagentFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	egress := filepath.Join(t.TempDir(), "egress.json")
	raw, _ := json.Marshal(governedEvidence.Revisions)
	if err := os.WriteFile(egress, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The snapshot is passed exactly as kagent stores it.
	sources := filepath.Join(t.TempDir(), "sources.json")
	snapshot := `{"` + governedRevision + `": {"agentName": "probe-gov", "agentUID": "agent-uid", "spec": {}, "inputs": [
		{"apiVersion": "api.kagent.dev/v1alpha3", "kind": "Harness", "name": "kagent", "uid": "harness-uid", "generation": 1, "hash": "h1"},
		{"apiVersion": "api.kagent.dev/v1alpha3", "kind": "ModelConfig", "name": "relay-model", "uid": "model-uid", "generation": 2, "hash": "h2"},
		{"apiVersion": "api.kagent.dev/v1alpha3", "kind": "RemoteMCPServer", "name": "orka-relay", "uid": "mcp-uid", "generation": 1, "hash": "h3"}]}}`
	if err := os.WriteFile(sources, []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"-f", path, "--dry-run", "--wrapper-config-dir", dir, "--control-plane-url", "http://kagent-controller.kagent.svc:8083",
		"--relay-url", governedRelay, "--revision-egress", egress, "--revision-sources", sources,
		"--accepted-egress", "http://probe-cpfilter.probe.svc:8083", "--accepted-egress", "https://api.openai.com:443"}
	if _, err := runKagentImport(t, base...); err == nil || !strings.Contains(err.Error(), "--model-upstream-url") {
		t.Fatalf("governed Agent without an upstream: %v", err)
	}
	args := append(slices.Clone(base), "--model-upstream-url", "http://vekil.vekil-system.svc:8080/v1", "--model-upstream-model", "gpt-4.1")
	// The wrapper refuses these, so the import must not write them.
	for flag, value := range map[string]string{
		"--control-plane-url":  "http://10.0.0.1:8083",
		"--user-id-prefix":     "Orka_Users",
		"--model-upstream-url": "vekil:8080/v1",
	} {
		if _, err := runKagentImport(t, append(slices.Clone(args), flag, value)...); err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("%s %s error = %v", flag, value, err)
		}
	}
	if _, err := runKagentImport(t, args...); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "probe-gov.kagent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runKagentImport(t, args...); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, "probe-gov.kagent.json"))
	if !bytes.Equal(first, second) {
		t.Fatal("wrapper configuration is not deterministic")
	}
	var config kagentWrapperConfig
	if err := json.Unmarshal(first, &config); err != nil || config.Model != "kagent/kagent/probe-gov" || config.Kagent.Agent["name"] != "probe-gov" ||
		config.Kagent.ExpectedRevision != "11d7665dfb79fda2112d61af3b32b10f9c8a196f5741e1f7e8e8e686147b927b" ||
		config.Governed == nil || config.Governed.UpstreamModel != "gpt-4.1" || config.Kagent.UserIDPrefix != "orka" {
		t.Fatalf("wrapper configuration = %s", first)
	}
	// A v1 configuration carries no version: the wrapper defaults to v1, and
	// existing images keep their configuration digest.
	if golden := `{
  "model": "kagent/kagent/probe-gov",
  "kagent": {
    "controlPlaneURL": "http://kagent-controller.kagent.svc:8083",
    "agent": {
      "name": "probe-gov",
      "namespace": "kagent"
    },
    "expectedRevision": "11d7665dfb79fda2112d61af3b32b10f9c8a196f5741e1f7e8e8e686147b927b",
    "userIDPrefix": "orka"
  },
  "governed": {
    "upstreamBaseURL": "http://vekil.vekil-system.svc:8080/v1",
    "upstreamModel": "gpt-4.1"
  }
}
`; string(first) != golden {
		t.Fatalf("v1 wrapper configuration changed:\n%s", first)
	}
	unpinned := strings.Replace(governedKagentFixture, "  latestSuccessfulRevision: 11d7665dfb79fda2112d61af3b32b10f9c8a196f5741e1f7e8e8e686147b927b\n", "", 1)
	if unpinned == governedKagentFixture {
		t.Fatal("fixture has no latestSuccessfulRevision")
	}
	if err := os.WriteFile(path, []byte(unpinned), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), "no status.latestSuccessfulRevision") {
		t.Fatalf("unpinned Agent error = %v", err)
	}
	unnamespaced := strings.Replace(governedKagentFixture, "{name: probe-gov, namespace: kagent,", "{name: probe-gov,", 1)
	if unnamespaced == governedKagentFixture {
		t.Fatal("fixture has no Agent namespace")
	}
	if err := os.WriteFile(path, []byte(unnamespaced), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runKagentImport(t, args...); err == nil || !strings.Contains(err.Error(), "has no metadata.namespace") {
		t.Fatalf("un-namespaced Agent error = %v", err)
	}
}

const translatableKagentFixture = `apiVersion: api.kagent.dev/v1alpha3
kind: Harness
metadata: {name: kagent, namespace: kagent}
spec: {kagent: {}}
---
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata: {name: gpt, namespace: kagent}
spec:
  provider: OpenAI
  model: gpt-4.1
  apiKeySecret: openai-key
  apiKeySecretKey: key
  openAI: {temperature: "0.2", maxTokens: 1024, apiFormat: chatCompletions}
---
apiVersion: api.kagent.dev/v1alpha3
kind: Agent
metadata: {name: k8s-agent, namespace: kagent}
spec:
  harnessRef: {name: kagent}
  template:
    inline:
      description: Answers Kubernetes questions.
      systemPrompt: You are a Kubernetes expert.
      modelConfig: {name: gpt}
      tools:
      - mcp:
          server: {kind: RemoteMCPServer, name: k8s}
          tools: [list_pods]
`

func TestTranslateKagentAgentsMapsOnlyKnownFields(t *testing.T) {
	translated, _, err := translateKagentAgents([]byte(translatableKagentFixture), kagentTranslateOptions{Tools: map[string]string{"k8s/list_pods": "k8s-list-pods"}})
	if err != nil {
		t.Fatal(err)
	}
	agent := decodeWrappedAgent(t, translated[0].Manifest)
	if agent.Spec.Runtime != nil || agent.Spec.SystemPrompt == nil || agent.Spec.SystemPrompt.Inline != "You are a Kubernetes expert." ||
		agent.Spec.Model == nil || agent.Spec.Model.Provider != "openai" || agent.Spec.Model.Name != "gpt-4.1" ||
		agent.Spec.Model.Temperature == nil || *agent.Spec.Model.Temperature != 0.2 || agent.Spec.Model.MaxTokens == nil || *agent.Spec.Model.MaxTokens != 1024 ||
		len(agent.Spec.Tools) != 1 || agent.Spec.Tools[0].Name != "k8s-list-pods" || agent.Spec.ProviderRef != nil {
		t.Fatalf("translated spec = %+v", agent.Spec)
	}
	if translated[0].RuntimeName != "" || translated[0].Mode.Mode != kagentModeTranslated {
		t.Fatalf("translated agent = %+v", translated[0])
	}
	for name, harness := range map[string]string{
		"workload command": "spec: {kagent: {}, workload: {command: [python, -m, custom]}}",
		"environment":      "spec: {kagent: {}, env: [{name: MODE, value: custom}]}",
	} {
		changed := strings.Replace(translatableKagentFixture, "spec: {kagent: {}}", harness, 1)
		if _, _, err := translateKagentAgents([]byte(changed), kagentTranslateOptions{Tools: map[string]string{"k8s/list_pods": "k8s-list-pods"}}); err == nil {
			t.Errorf("%s: a harness override was translated away", name)
		}
	}
	placed := strings.Replace(translatableKagentFixture, "spec: {kagent: {}}", "spec: {kagent: {}, substrate: {workerPoolRef: {name: p}}}", 1)
	if _, _, err := translateKagentAgents([]byte(placed), kagentTranslateOptions{Tools: map[string]string{"k8s/list_pods": "k8s-list-pods"}}); err != nil {
		t.Errorf("substrate placement was refused: %v", err)
	}
}

func TestTranslateKagentAgentsRefusesEverythingItCannotMap(t *testing.T) {
	second := strings.NewReplacer("name: k8s-agent", "name: helm-agent", "modelConfig: {name: gpt}", "modelConfig: {name: bedrock}").
		Replace(strings.SplitAfterN(translatableKagentFixture, "---\n", 3)[2])
	input := strings.Replace(translatableKagentFixture, "spec: {kagent: {}}", "spec: {kagent: {memory: {}}}", 1) +
		"---\n" + second + `---
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata: {name: bedrock, namespace: kagent}
spec: {provider: Bedrock, model: claude}
`
	input = strings.Replace(input, "      modelConfig: {name: gpt}\n", "      modelConfig: {name: gpt}\n      skills: [{name: s}]\n", 1)
	input = strings.Replace(input, "          tools: [list_pods]\n", "          tools: [list_pods]\n          requireApproval: [list_pods]\n", 1)
	_, _, err := translateKagentAgents([]byte(input), kagentTranslateOptions{})
	if err == nil {
		t.Fatal("an untranslatable Agent was accepted")
	}
	for _, want := range []string{
		"kagent Agent kagent/k8s-agent", "kagent Agent kagent/helm-agent", "uses memory", `template field "skills" has no Orka equivalent`,
		"tool k8s/list_pods has no Orka Tool", `uses provider "Bedrock"`, "Translation maps only these fields",
		"sets mcp.requireApproval",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestTranslateKagentAgentsNeedsAProviderForCustomEndpoints(t *testing.T) {
	custom := strings.Replace(translatableKagentFixture, `openAI: {temperature: "0.2", maxTokens: 1024, apiFormat: chatCompletions}`, `openAI: {baseUrl: "http://vekil:8080/v1"}`, 1)
	if custom == translatableKagentFixture {
		t.Fatal("fixture has no openAI settings to replace")
	}
	tools := map[string]string{"k8s/list_pods": "k8s-list-pods"}
	if _, _, err := translateKagentAgents([]byte(custom), kagentTranslateOptions{Tools: tools}); err == nil || !strings.Contains(err.Error(), "--provider-ref") {
		t.Fatalf("custom endpoint without a provider = %v", err)
	}
	translated, _, err := translateKagentAgents([]byte(custom), kagentTranslateOptions{Tools: tools, ProviderRef: "vekil"})
	if err != nil {
		t.Fatal(err)
	}
	if agent := decodeWrappedAgent(t, translated[0].Manifest); agent.Spec.ProviderRef == nil || agent.Spec.ProviderRef.Name != "vekil" {
		t.Fatalf("providerRef = %+v", agent.Spec.ProviderRef)
	}
}

func TestAgentImportKagentCmdTranslateRemovesDroppedSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	api := &kagentImportAPI{agents: map[string]map[string]any{}}
	srv := kagentImportAPIServer(t, api)
	path := filepath.Join(t.TempDir(), "agents.yaml")
	importAgents := func(fixture string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := runKagentImport(t, "-f", path, "--translate", "--tool", "k8s/list_pods=k8s-list-pods", "--server", srv.URL, "-n", "team")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	importAgents(translatableKagentFixture)
	// Server defaults on fields the import owns are not drift.
	if out := importAgents(translatableKagentFixture); !strings.Contains(out, "Agent unchanged: k8s-agent") || api.updates != 0 {
		t.Fatalf("re-import = %q (updates %d)", out, api.updates)
	}
	dropped := strings.NewReplacer(
		"      tools:\n      - mcp:\n          server: {kind: RemoteMCPServer, name: k8s}\n          tools: [list_pods]\n", "",
		`temperature: "0.2", `, "",
	).Replace(translatableKagentFixture)
	if out := importAgents(dropped); !strings.Contains(out, "Agent updated: k8s-agent") || api.updates != 1 {
		t.Fatalf("re-import without tools = %q (updates %d)", out, api.updates)
	}
	agent := decodeWrappedAgent(t, api.agents["k8s-agent"])
	if len(agent.Spec.Tools) != 0 || agent.Spec.Model == nil || agent.Spec.Model.Temperature != nil || agent.Spec.Model.MaxTokens == nil {
		t.Fatalf("spec after removing settings = %+v", agent.Spec)
	}
}

func TestAgentImportKagentCmdTranslateTakesNoWrapperFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(translatableKagentFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runKagentImport(t, "-f", path, "--translate", "--dry-run", "--relay-url", "http://relay:8092"); err == nil {
		t.Fatal("--translate accepted a relay")
	}
	if _, err := runKagentImport(t, "-f", path, "--translate", "--dry-run", "--tool", "k8s-list_pods"); err == nil || !strings.Contains(err.Error(), "<RemoteMCPServer>/<tool>=<Orka Tool>") {
		t.Fatalf("malformed --tool = %v", err)
	}
	out, err := runKagentImport(t, "-f", path, "--translate", "--dry-run", "--tool", "k8s/list_pods=k8s-list-pods")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := decodeKubernetesObjects([]byte(out))
	if err != nil || len(objects) != 1 {
		t.Fatalf("dry run = %q %v", out, err)
	}
	if agent := decodeWrappedAgent(t, objects[0]); agent.Spec.Runtime != nil || agent.Spec.Model == nil {
		t.Fatalf("dry-run Agent = %+v", agent.Spec)
	}
}
