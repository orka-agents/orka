/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

// Model-free evals for the contract between tool schemas, which models read,
// and tool implementations, which must not trust them. They run every chat and
// worker tool against arguments a model can plausibly produce.
//
// A case with knownDefect documents a check that fails on current code. It
// passes while the defect reproduces and fails once the defect is fixed, so the
// fix also removes the knownDefect marker.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/workerenv"
)

// evalToolRegistries returns the tools a model can be offered. Chat and worker
// tools are separate implementations that can share a name.
func evalToolRegistries(fc client.Client) map[string]*Registry {
	chat := NewRegistry()
	RegisterChatTools(chat)
	worker := NewRegistry()
	for _, tool := range []Tool{
		NewWebSearchTool(), NewCodeExecTool(), NewFileReadTool(), NewWebFetchTool(), NewFileWriteTool(),
		NewRequestApprovalTool(), NewReplyInConversationTool(),
		NewDelegateTaskTool(fc), NewWaitForTasksTool(fc), NewCreateContainerTaskTool(fc), NewCancelTaskTool(fc),
		NewSendMessageTool(), NewCheckMessagesTool(), NewCreatePullRequestTool(fc), NewCheckPullRequestCITool(fc),
		NewMergePullRequestTool(fc), NewAutoMergePullRequestTool(fc), NewReviewPullRequestTool(fc),
		NewPostReviewCommentTool(fc), NewCheckPRReviewMarkerTool(fc), NewListIssuesTool(fc), NewListPullRequestsTool(fc),
		NewGetIssueTool(fc), NewCommentOnIssueTool(fc), NewCreateAgentTool(fc, executionmode.HarnessV2),
		NewDeleteAgentTool(fc), NewUpdatePlanTool(), NewRunValidationTool(fc),
		NewRecallMemoryTool(), NewRememberMemoryTool(), NewProposeMemoryTool(), NewSearchTranscriptTool(),
	} {
		worker.Register(tool)
	}
	// The ACP broker builds its own registry. Sweep its tools whose schema
	// differs from the worker tool of the same name; the rest are covered.
	brokeredAll := NewRegistry()
	_ = RegisterBrokeredCoordinationTools(brokeredAll, fc)
	_ = RegisterBrokeredWebTools(brokeredAll)
	brokered := NewRegistry()
	for _, name := range brokeredAll.Names() {
		tool, _ := brokeredAll.Get(name)
		if same, ok := worker.Get(name); !ok || !bytes.Equal(same.Parameters(), tool.Parameters()) {
			brokered.Register(tool)
		}
	}
	evalGuardWebClients(worker, brokered)
	return map[string]*Registry{"chat": chat, "worker": worker, "brokered": brokered}
}

// evalGuardWebClients gives the web tools a client that uses
// http.DefaultTransport, which evalToolSandbox guards. Their own clients dial
// public endpoints directly and would bypass the guard.
func evalGuardWebClients(registries ...*Registry) {
	for _, registry := range registries {
		for _, name := range registry.Names() {
			tool, _ := registry.Get(name)
			switch web := tool.(type) {
			case *WebSearchTool:
				web.client = &http.Client{Timeout: 10 * time.Second}
			case *WebFetchTool:
				web.client = &http.Client{Timeout: 10 * time.Second}
			}
		}
	}
}

// evalToolSandbox clears forge credentials and replaces http.DefaultTransport
// with a clone that dials only loopback addresses, where test servers listen,
// so a tool that gets past validation cannot reach a real service. Every tool
// in evalToolRegistries sends HTTP through http.DefaultTransport (see
// evalGuardWebClients). The test fails if any dial was blocked.
func evalToolSandbox(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	original := http.DefaultTransport
	guarded := original.(*http.Transport).Clone()
	guarded.Proxy = nil
	var mu sync.Mutex
	var blocked []string
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	guarded.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(address); err == nil {
			if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
				return dialer.DialContext(ctx, network, address)
			}
		}
		mu.Lock()
		blocked = append(blocked, address)
		mu.Unlock()
		return nil, fmt.Errorf("eval sandbox blocked dial to %s", address)
	}
	http.DefaultTransport = guarded
	t.Cleanup(func() {
		http.DefaultTransport = original
		mu.Lock()
		defer mu.Unlock()
		if len(blocked) > 0 {
			t.Errorf("tools attempted outbound connections: %v", blocked)
		}
	})
}

// evalToolClusters gives each call a fresh cluster: an empty one, where create
// paths can succeed, and one holding objects named "eval", where update and
// lookup paths can reach validation.
var evalToolClusters = map[string]func() client.Client{
	"empty": func() client.Client { return newFakeClient() },
	"seeded": func() client.Client {
		meta := metav1.ObjectMeta{Name: evalPlaceholder, Namespace: defaultNamespace}
		return newFakeClient(
			&corev1alpha1.Provider{ObjectMeta: meta},
			&corev1alpha1.Agent{ObjectMeta: meta, Spec: corev1alpha1.AgentSpec{ProviderRef: &corev1alpha1.ProviderReference{Name: evalPlaceholder}}},
			&corev1alpha1.Task{ObjectMeta: meta},
			&corev1alpha1.Tool{ObjectMeta: meta},
		)
	},
}

const evalPlaceholder = "eval"

// evalToolContextFor gives chat tools the chat executor's context and worker
// tools the narrower context a worker builds, so each takes its real path.
func evalToolContextFor(label string, fc client.Client) context.Context {
	if label == "worker" {
		return WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
	}
	return evalToolContext(fc)
}

// evalRegistryExecute runs a tool through Registry.Execute, the path chat and
// worker calls take, converting a panic into a reported failure.
func evalRegistryExecute(ctx context.Context, registry *Registry, name, args string) (result string, panicked any, err error) {
	defer func() { panicked = recover() }()
	result, err = registry.Execute(ctx, name, json.RawMessage(args))
	return result, nil, err
}

// evalToolContext is the chat tool context, with the execution mode a real
// installation selects so runtime Agents can be created.
func evalToolContext(fc client.Client) context.Context {
	ctx := newCreateAgentTaskToolCtx(fc)
	GetToolContext(ctx).ExecutionMode = executionmode.HarnessV2
	return ctx
}

func evalToolSchema(t *testing.T, tool Tool) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("%s: invalid parameters schema: %v", tool.Name(), err)
	}
	return schema
}

// evalToolExecute runs a tool, converting a panic into a reported failure.
func evalToolExecute(ctx context.Context, tool Tool, args string) (result string, panicked any, err error) {
	defer func() { panicked = recover() }()
	result, err = tool.Execute(ctx, json.RawMessage(args))
	return result, nil, err
}

// evalToolFailure reports whether a tool call failed and the message the model
// would receive for it.
func evalToolFailure(result string, err error) (bool, string) {
	if err != nil {
		return true, err.Error()
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(result), &decoded) == nil {
		message, _ := decoded["error"].(string)
		if success, ok := decoded["success"].(bool); ok && !success {
			return true, message
		}
		if message != "" {
			return true, message
		}
	}
	return false, ""
}

func TestToolEvalRegistriesCoverKnownTools(t *testing.T) {
	registered := map[string]bool{}
	for _, registry := range evalToolRegistries(newFakeClient()) {
		for _, name := range registry.Names() {
			registered[name] = true
		}
	}
	var missing []string
	for _, name := range slices.Concat(KnownBuiltInToolNames(), aitools.MemoryToolNames(), aitools.CoordinationToolNames()) {
		if !registered[name] && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("evalToolRegistries misses tools a model can be offered; add them: %v", missing)
	}
}

// TestToolEvalMalformedArguments sends each tool arguments a model gets wrong:
// non-object JSON, missing required fields, wrong JSON types, and nested
// objects encoded as strings. A tool must never panic, must never report
// success for a call missing required fields or with a wrong-typed value, must
// not drop an object sent as a string, and must always explain a failure. Chat
// tools must return a structured result, or a ToolArgumentError that the chat
// executor turns into one, instead of another Go error.
func TestToolEvalMalformedArguments(t *testing.T) {
	evalToolSandbox(t)
	// Worker tools resolve their parent Task from the environment; the seeded
	// cluster holds it.
	t.Setenv(envOrkaTaskName, evalPlaceholder)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	knownDefects := map[string]string{}
	wrongValue := map[string]string{
		"string": `{"k":"v"}`, "integer": `"many"`, "number": `"many"`, "boolean": `"yes"`, "array": `"a,b"`, "object": `"{\"k\":\"v\"}"`,
	}
	violations := map[string]string{}
	// Panics, Go errors from chat tools, and failures without a message are
	// never accepted as known defects.
	var fatal, inconclusive []string
	for label, registry := range evalToolRegistries(newFakeClient()) {
		names := registry.Names()
		sort.Strings(names)
		for _, name := range names {
			tool, _ := registry.Get(name)
			schema := evalToolSchema(t, tool)
			required, _ := schema["required"].([]any)
			properties, _ := schema["properties"].(map[string]any)

			inputs := map[string]string{"null": `null`, "array": `[]`, "string": `"text"`, "number": `42`, "empty object": `{}`}
			wrongTypeAllowed := map[string]bool{}
			for prop, raw := range properties {
				propSchema, _ := raw.(map[string]any)
				typ, _ := propSchema["type"].(string)
				if value, ok := wrongValue[typ]; ok {
					input := fmt.Sprintf("%s as wrong type", prop)
					inputs[input] = fmt.Sprintf(`{%q:%s}`, prop, value)
					// An object sent as a JSON string is decoded and honored.
					wrongTypeAllowed[input] = typ == "object"
				}
				if typ == "object" {
					key := fmt.Sprintf("%s/%s: %s as JSON string", label, name, prop)
					switch verdict := evalObjectStringHandling(label, name, required, properties, prop); {
					case verdict == "":
					case verdict == evalInconclusive:
						inconclusive = append(inconclusive, key)
					case strings.HasPrefix(verdict, evalPanicked):
						fatal = append(fatal, key+": "+verdict)
					default:
						violations[key] = verdict
					}
				}
			}
			inputNames := make([]string, 0, len(inputs))
			for input := range inputs {
				inputNames = append(inputNames, input)
			}
			sort.Strings(inputNames)

			for _, input := range inputNames {
				for _, cluster := range []string{"empty", "seeded"} {
					fc := evalToolClusters[cluster]()
					result, panicked, err := evalRegistryExecute(evalToolContextFor(label, fc), evalToolRegistries(fc)[label], name, inputs[input])
					key := fmt.Sprintf("%s/%s: %s", label, name, input)
					failed, message := evalToolFailure(result, err)
					switch {
					case panicked != nil:
						fatal = append(fatal, fmt.Sprintf("%s: panicked: %v", key, panicked))
					case label == "chat" && err != nil && !errors.As(err, new(*ToolArgumentError)):
						fatal = append(fatal, fmt.Sprintf("%s: returned Go error %q instead of a structured result", key, err))
					case failed && strings.TrimSpace(message) == "":
						fatal = append(fatal, fmt.Sprintf("%s: failed without an error message: %q", key, result))
					case (input == "empty object" || input == "null") && len(required) > 0 && !failed:
						violations[key] = fmt.Sprintf("reported success without required fields %v in the %s cluster", required, cluster)
					case strings.HasSuffix(input, "as wrong type") && !wrongTypeAllowed[input] && !failed:
						violations[key] = fmt.Sprintf("reported success with a wrong-typed value in the %s cluster", cluster)
					}
				}
			}
		}
	}
	for _, failure := range fatal {
		t.Error(failure)
	}
	sort.Strings(inconclusive)
	t.Logf("object-as-string handling not observable without more setup: %v", inconclusive)
	evalToolReportViolations(t, violations, knownDefects)
}

const (
	evalInconclusive = "inconclusive"
	evalPanicked     = "panicked"
)

// evalObjectStringHandling runs a tool three ways: with a nested object built
// from its schema, with the same object encoded as a JSON string, and with the
// field omitted. A tool may reject the string or treat it like the object; it
// must not drop it or store it differently. It returns "" when handling is
// correct, evalInconclusive when the object itself changes nothing observable,
// and a violation otherwise.
func evalObjectStringHandling(label, name string, required []any, properties map[string]any, prop string) string {
	propSchema, _ := properties[prop].(map[string]any)
	payload := evalObjectPayload(prop, propSchema)
	encoded, _ := json.Marshal(payload)
	verdict := evalInconclusive
	for _, cluster := range []string{"empty", "seeded"} {
		run := func(value any) (bool, string) {
			args := evalToolRequiredArgs(required, properties)
			maps.Copy(args, evalPayloadSiblings[name+"."+prop])
			delete(args, prop)
			if value != nil {
				args[prop] = value
			}
			raw, _ := json.Marshal(args)
			fc := evalToolClusters[cluster]()
			result, panicked, err := evalRegistryExecute(evalToolContextFor(label, fc), evalToolRegistries(fc)[label], name, string(raw))
			if panicked != nil {
				return true, evalPanicked + fmt.Sprintf(": %v", panicked)
			}
			failed, _ := evalToolFailure(result, err)
			return failed, evalClusterSnapshot(fc)
		}
		stringFailed, asString := run(string(encoded))
		objectFailed, asObject := run(payload)
		_, omitted := run(nil)
		for _, snapshot := range []string{asString, asObject, omitted} {
			if strings.HasPrefix(snapshot, evalPanicked) {
				return snapshot
			}
		}
		switch {
		case objectFailed || asObject == omitted:
			continue
		case stringFailed || asString == asObject:
			verdict = ""
		case asString == omitted:
			return fmt.Sprintf("dropped a JSON-string %s that takes effect as an object (%s cluster)", prop, cluster)
		default:
			return fmt.Sprintf("stored a JSON-string %s differently from the same object (%s cluster)", prop, cluster)
		}
	}
	return verdict
}

// evalPayloadOverrides supplies valid values for nested fields whose schema
// has no enum or format to derive one from.
var evalPayloadOverrides = map[string]any{
	"runtime.type":       "codex",
	"resources.requests": map[string]any{"cpu": "100m"},
}

// evalPayloadSiblings adds the other arguments a nested object needs to take
// effect, keyed by "tool.property".
var evalPayloadSiblings = map[string]map[string]any{
	"create_agent.runtime": {"model": map[string]any{"name": "eval-model"}},
}

// evalObjectPayload builds an object that should change what a tool does:
// booleans true, numbers at their minimum or 1, strings from their enum.
func evalObjectPayload(prop string, schema map[string]any) map[string]any {
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) == 0 {
		return map[string]any{evalPlaceholder: evalPlaceholder}
	}
	payload := map[string]any{}
	for name, raw := range properties {
		if value, ok := evalPayloadOverrides[prop+"."+name]; ok {
			payload[name] = value
			continue
		}
		propSchema, _ := raw.(map[string]any)
		switch propSchema["type"] {
		case "boolean":
			payload[name] = true
		case "integer", "number":
			if minimum, ok := propSchema["minimum"].(float64); ok && minimum > 0 {
				payload[name] = minimum
			} else {
				payload[name] = 1
			}
		case "string":
			lower := strings.ToLower(name)
			switch enum, _ := propSchema["enum"].([]any); {
			case len(enum) > 0:
				payload[name] = enum[0]
			case strings.Contains(lower, "repo") || strings.Contains(lower, "url"):
				payload[name] = "https://github.com/acme/api"
			default:
				payload[name] = evalPlaceholder
			}
		}
	}
	return payload
}

// evalClusterSnapshot renders the specs of the Agents and Tasks a tool call
// left behind. Names are left out because worker tools add random suffixes.
func evalClusterSnapshot(fc client.Client) string {
	var agents corev1alpha1.AgentList
	var tasks corev1alpha1.TaskList
	_ = fc.List(context.Background(), &agents)
	_ = fc.List(context.Background(), &tasks)
	parts := make([]string, 0, len(agents.Items)+len(tasks.Items))
	for i := range agents.Items {
		spec, _ := json.Marshal(agents.Items[i].Spec)
		parts = append(parts, "agent "+string(spec))
	}
	for i := range tasks.Items {
		spec, _ := json.Marshal(tasks.Items[i].Spec)
		parts = append(parts, "task "+string(spec))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// evalToolReportViolations applies the knownDefect contract to a sweep.
func evalToolReportViolations(t *testing.T, violations, knownDefects map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(violations))
	for key := range violations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if defect, ok := knownDefects[key]; ok {
			t.Logf("known defect still present: %s: %s (%s)", key, defect, violations[key])
			continue
		}
		t.Errorf("%s: %s", key, violations[key])
	}
	for key, defect := range knownDefects {
		if _, ok := violations[key]; !ok {
			t.Errorf("known defect no longer reproduces; remove knownDefect %q: %s", key, defect)
		}
	}
}

// evalToolRequiredArgs fills required fields with placeholder values of the
// declared type, so validation can reach the field under test.
func evalToolRequiredArgs(required []any, properties map[string]any) map[string]any {
	args := map[string]any{}
	for _, raw := range required {
		name, _ := raw.(string)
		propSchema, _ := properties[name].(map[string]any)
		switch propSchema["type"] {
		case "integer", "number":
			args[name] = 1
		case "boolean":
			args[name] = false
		case "array":
			args[name] = []any{}
		case "object":
			args[name] = map[string]any{}
		default:
			args[name] = evalPlaceholder
		}
	}
	return args
}

// evalToolLimits lists every limit (minimum, maximum, enum, length) that a tool
// schema declares, keyed like "chat/create_agent_task.maxTurns maximum".
func evalToolLimits(t *testing.T) []string {
	t.Helper()
	var limits []string
	var walk func(prefix string, schema map[string]any)
	walk = func(prefix string, schema map[string]any) {
		for _, keyword := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "enum", "minLength", "maxLength", "minItems", "maxItems", "pattern"} {
			if _, ok := schema[keyword]; ok {
				limits = append(limits, prefix+" "+keyword)
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for name, raw := range properties {
			if child, ok := raw.(map[string]any); ok {
				walk(prefix+"."+name, child)
			}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			walk(prefix+"[]", items)
		}
	}
	for label, registry := range evalToolRegistries(newFakeClient()) {
		for _, name := range registry.Names() {
			tool, _ := registry.Get(name)
			walk(label+"/"+name, evalToolSchema(t, tool))
		}
	}
	sort.Strings(limits)
	return limits
}

// evalRejects reports whether a tool call failed with a message naming the
// limited field, so a rejection for an unrelated reason does not count.
func evalRejects(ctx context.Context, tool Tool, args, field string) (bool, string) {
	result, panicked, err := evalToolExecute(ctx, tool, args)
	if panicked != nil {
		return false, fmt.Sprintf("panicked: %v", panicked)
	}
	failed, message := evalToolFailure(result, err)
	switch {
	case !failed:
		return false, "accepted " + args
	case !strings.Contains(strings.ToLower(message), strings.ToLower(field)):
		return false, fmt.Sprintf("rejected %s for another reason: %s", args, message)
	}
	return true, ""
}

// evalEnumEnforced checks that a value outside the enum is rejected and that
// no declared value is. A declared value counts as rejected only if it fails
// with the message the invalid value produced, so a call that fails for
// unrelated missing context still accepts the value.
func evalEnumEnforced(ctx context.Context, tool Tool, args func(value string) string, field, invalid string, declared ...string) (bool, string) {
	if ok, detail := evalRejects(ctx, tool, args(invalid), field); !ok {
		return false, detail
	}
	result, _, err := evalToolExecute(ctx, tool, args(invalid))
	_, rejection := evalToolFailure(result, err)
	for _, value := range declared {
		result, panicked, err := evalToolExecute(ctx, tool, args(value))
		if panicked != nil {
			return false, fmt.Sprintf("%s %q panicked: %v", field, value, panicked)
		}
		if failed, message := evalToolFailure(result, err); failed && (message == rejection || message == strings.ReplaceAll(rejection, invalid, value)) {
			return false, fmt.Sprintf("rejected declared %s %q: %s", field, value, message)
		}
	}
	return true, ""
}

// evalRecordingCodeExec returns a code_exec tool whose sandbox only records the
// request, so no code runs.
func evalRecordingCodeExec(t *testing.T) (*CodeExecTool, *recordingCodeExecutor) {
	t.Helper()
	sandbox := &recordingCodeExecutor{result: CodeExecResult{Output: "ok"}}
	return &CodeExecTool{
		workDir:          t.TempDir(),
		timeout:          defaultCodeExecTimeout,
		allowedLangs:     defaultCodeExecAllowedLangs(),
		denyPatterns:     defaultDenyPatterns,
		executor:         sandbox,
		backend:          codeExecBackendKubernetes,
		outputLimitBytes: defaultCodeExecOutputLimitBytes,
	}, sandbox
}

func evalAll(checks ...func() (bool, string)) (bool, string) {
	for _, check := range checks {
		if ok, detail := check(); !ok {
			return false, detail
		}
	}
	return true, ""
}

// evalHTTPStub records requests. It answers the GitHub calls tools make with an
// open PR whose checks pass, and anything else with an empty JSON list.
type evalHTTPStub struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func newEvalHTTPStub(t *testing.T) *evalHTTPStub {
	t.Helper()
	stub := &evalHTTPStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stub.mu.Lock()
		stub.requests = append(stub.requests, fmt.Sprintf("%s %s %s", r.Method, r.URL.RequestURI(), body))
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = fmt.Fprint(w, `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"}]}`)
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = fmt.Fprint(w, `{"state":"success","statuses":[]}`)
		case strings.HasSuffix(r.URL.Path, "/merge"):
			_, _ = fmt.Fprint(w, `{"merged":true,"sha":"def456"}`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = fmt.Fprint(w, `{"number":1,"state":"open","mergeable":true,"head":{"sha":"abc123","ref":"feature"},"base":{"ref":"main"}}`)
		default:
			_, _ = fmt.Fprint(w, `[]`)
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}

func (s *evalHTTPStub) sent(fragment string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.requests, func(request string) bool { return strings.Contains(request, fragment) })
}

// evalCRDModelField returns the generated Agent CRD schema for spec.model.<field>,
// which the Kubernetes API enforces when a tool writes the Agent.
func evalCRDModelField(t *testing.T, field string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "core.orka.ai_agents.yaml"))
	if err != nil {
		t.Fatalf("read Agent CRD: %v", err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Model struct {
										Properties map[string]map[string]any `json:"properties"`
									} `json:"model"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &crd); err != nil || len(crd.Spec.Versions) == 0 {
		t.Fatalf("parse Agent CRD: %v", err)
	}
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Model.Properties[field]
}

// TestToolEvalSchemaLimitsEnforced requires every limit a tool schema declares
// to be enforced: the tool rejects or clamps an out-of-range value, or the
// Kubernetes API rejects it when the tool writes the object. Models treat the
// schema as advice, so a limit nothing enforces is only a suggestion.
func TestToolEvalSchemaLimitsEnforced(t *testing.T) {
	evalToolSandbox(t)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(codeExecBackendEnv, "")
	long := func(n int) string { return strings.Repeat("x", n) }
	chatTool := func(name string, fc client.Client) (Tool, context.Context) {
		tool, _ := evalToolRegistries(fc)["chat"].Get(name)
		return tool, evalToolContext(fc)
	}
	workerTool := func(name string, fc client.Client) (Tool, context.Context) {
		tool, _ := evalToolRegistries(fc)["worker"].Get(name)
		return tool, evalToolContextFor("worker", fc)
	}
	githubTask := func() client.Client {
		task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
		return newFakeClient(task, secret)
	}

	cases := []struct {
		name        string
		limits      []string
		check       func(t *testing.T) (bool, string)
		knownDefect string
	}{
		{
			name:   "chat create_agent model counts",
			limits: []string{"chat/create_agent.model.contextWindow minimum", "chat/create_agent.model.maxTokens minimum"},
			check: func(t *testing.T) (bool, string) {
				tool, ctx := chatTool("create_agent", newFakeClient())
				return evalAll(
					func() (bool, string) {
						return evalRejects(ctx, tool, `{"name":"a","model":{"name":"m","contextWindow":0}}`, "contextWindow")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, `{"name":"a","model":{"name":"m","maxTokens":0}}`, "maxTokens")
					},
				)
			},
		},
		{
			name:   "chat create_agent model temperature",
			limits: []string{"chat/create_agent.model.temperature maximum", "chat/create_agent.model.temperature minimum"},
			check: func(t *testing.T) (bool, string) {
				// The tool stores temperature as given; the Agent CRD rejects it on create.
				limits := evalCRDModelField(t, "temperature")
				ok := limits["minimum"] == float64(0) && limits["maximum"] == float64(2)
				return ok, fmt.Sprintf("Agent CRD temperature limits = %v", limits)
			},
		},
		{
			name:   "chat update_agent model",
			limits: []string{"chat/update_agent.model.contextWindow minimum", "chat/update_agent.model.maxTokens minimum", "chat/update_agent.model.temperature maximum", "chat/update_agent.model.temperature minimum"},
			check: func(t *testing.T) (bool, string) {
				tool, ctx := chatTool("update_agent", evalToolClusters["seeded"]())
				return evalAll(
					func() (bool, string) {
						return evalRejects(ctx, tool, `{"name":"eval","model":{"contextWindow":0}}`, "contextWindow")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, `{"name":"eval","model":{"maxTokens":0}}`, "maxTokens")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, `{"name":"eval","model":{"temperature":2.5}}`, "temperature")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, `{"name":"eval","model":{"temperature":-1}}`, "temperature")
					},
				)
			},
		},
		{
			name:   "chat create_agent_task maxTurns",
			limits: []string{"chat/create_agent_task.maxTurns maximum", "chat/create_agent_task.maxTurns minimum"},
			check: func(t *testing.T) (bool, string) {
				tool, ctx := chatTool("create_agent_task", newFakeClient())
				return evalAll(
					func() (bool, string) {
						return evalRejects(ctx, tool, `{"name":"t","prompt":"p","agentRef":"a","maxTurns":1001}`, "maxTurns")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, `{"name":"t","prompt":"p","agentRef":"a","maxTurns":0}`, "maxTurns")
					},
				)
			},
		},
		{
			name:   "chat create_agent_task workspace",
			limits: []string{"chat/create_agent_task.workspace.intent enum", "chat/create_agent_task.workspace.prBody maxLength", "chat/create_agent_task.workspace.prTitle maxLength"},
			check: func(t *testing.T) (bool, string) {
				tool, ctx := chatTool("create_agent_task", newFakeClient())
				base := `{"name":"t","prompt":"p","agentRef":"a","workspace":{"gitRepo":"https://github.com/acme/api",%s}}`
				return evalAll(
					func() (bool, string) {
						intent := func(v string) string { return fmt.Sprintf(base, `"intent":"`+v+`"`) }
						return evalEnumEnforced(ctx, tool, intent, "intent", "delete", "read", "write")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, fmt.Sprintf(base, `"intent":"write","createPR":true,"prTitle":"`+long(257)+`"`), "prTitle")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, fmt.Sprintf(base, `"intent":"write","createPR":true,"prBody":"`+long(32769)+`"`), "prBody")
					},
				)
			},
		},
		{
			name:   "chat create_pr_monitor",
			limits: []string{"chat/create_pr_monitor.per_page maximum", "chat/create_pr_monitor.per_page minimum", "chat/create_pr_monitor.review_event enum"},
			check: func(t *testing.T) (bool, string) {
				_, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
				base := `{"name":"m","repo_url":"https://github.com/acme/api","schedule":"0 * * * *","agentRef":"a","readCredentialRef":"` + secret.Name + `"%s}`
				agent := func() client.Client {
					return newFakeClient(secret.DeepCopy(), &corev1alpha1.Agent{
						ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: defaultNamespace},
						Spec:       corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{Enabled: true}},
					})
				}
				pagesClamped := func(perPage int) (bool, string) {
					fc := agent()
					tool, ctx := chatTool("create_pr_monitor", fc)
					result, panicked, err := evalToolExecute(ctx, tool, fmt.Sprintf(base, fmt.Sprintf(`,"per_page":%d`, perPage)))
					if err != nil || panicked != nil {
						return false, fmt.Sprintf("per_page=%d: err=%v panic=%v", perPage, err, panicked)
					}
					var tasks corev1alpha1.TaskList
					_ = fc.List(context.Background(), &tasks)
					if len(tasks.Items) == 0 {
						return false, fmt.Sprintf("per_page=%d created no monitor Task: %s", perPage, result)
					}
					for i := range tasks.Items {
						if strings.Contains(tasks.Items[i].Spec.Prompt, fmt.Sprintf("per_page %d.", perPage)) {
							return false, fmt.Sprintf("per_page=%d reached the monitor prompt", perPage)
						}
					}
					return true, ""
				}
				tool, ctx := chatTool("create_pr_monitor", agent())
				return evalAll(
					func() (bool, string) { return pagesClamped(500) },
					func() (bool, string) { return pagesClamped(-5) },
					func() (bool, string) {
						event := func(v string) string { return fmt.Sprintf(base, `,"review_event":"`+v+`"`) }
						return evalEnumEnforced(ctx, tool, event, "review_event", "MAYBE", "COMMENT", "APPROVE", "REQUEST_CHANGES")
					},
				)
			},
		},
		{
			name:   "worker code_exec language and maximum timeout",
			limits: []string{"worker/code_exec.language enum", "worker/code_exec.timeout maximum"},
			check: func(t *testing.T) (bool, string) {
				// Clamping an over-maximum timeout is pinned by
				// TestCodeExecTool_Execute_TimeoutClampsToMax.
				tool, _ := evalRecordingCodeExec(t)
				language := func(v string) string { return `{"language":"` + v + `","code":"echo test"}` }
				return evalEnumEnforced(context.Background(), tool, language, "language", "cobol", "python", "python3", "javascript", "node", "bash", "sh")
			},
		},
		{
			name:   "worker code_exec minimum timeout",
			limits: []string{"worker/code_exec.timeout minimum"},
			check: func(t *testing.T) (bool, string) {
				// Zero is the value just below the minimum.
				tool, sandbox := evalRecordingCodeExec(t)
				if ok, _ := evalRejects(context.Background(), tool, `{"language":"bash","code":"echo test","timeout":0}`, "timeout"); ok {
					return true, ""
				}
				if sandbox.calls > 0 && sandbox.req.Timeout == time.Second {
					return true, ""
				}
				return false, fmt.Sprintf("timeout 0 ran with %s", sandbox.req.Timeout)
			},
		},
		{
			name:   "brokered web_search",
			limits: []string{"brokered/web_search.limit maximum", "brokered/web_search.query maxLength"},
			check: func(t *testing.T) (bool, string) {
				// Both limits are checked before any request.
				tool, _ := evalToolRegistries(newFakeClient())["brokered"].Get("web_search")
				ctx := context.Background()
				return evalAll(
					func() (bool, string) { return evalRejects(ctx, tool, `{"query":"q","limit":11}`, "limit") },
					func() (bool, string) { return evalRejects(ctx, tool, `{"query":"`+long(4097)+`"}`, "query") },
				)
			},
		},
		{
			name:   "brokered web_fetch max_chars",
			limits: []string{"brokered/web_fetch.max_chars maximum"},
			check: func(t *testing.T) (bool, string) {
				// A public IP literal needs no DNS lookup, and max_chars is
				// checked before any request.
				tool, _ := evalToolRegistries(newFakeClient())["brokered"].Get("web_fetch")
				return evalRejects(context.Background(), tool, `{"url":"https://1.1.1.1/","max_chars":50001}`, "max_chars")
			},
		},
		{
			name:   "brokered web_fetch url",
			limits: []string{"brokered/web_fetch.url maxLength"},
			check: func(t *testing.T) (bool, string) {
				// One character past the advertised maxLength. The over-limit
				// max_chars stops the call before any request if the URL passes.
				tool, _ := evalToolRegistries(newFakeClient())["brokered"].Get("web_fetch")
				url := "https://1.1.1.1/" + long(brokeredWebFetchMaxURLBytes/utf8.UTFMax)
				return evalRejects(context.Background(), tool, `{"url":"`+url+`","max_chars":50001}`, "url")
			},
		},
		{
			name:   "worker create_agent model contextWindow",
			limits: []string{"worker/create_agent.model.contextWindow minimum"},
			check: func(t *testing.T) (bool, string) {
				// The tool stores contextWindow as given; the Agent CRD rejects it on create.
				limits := evalCRDModelField(t, "contextWindow")
				return limits["minimum"] == float64(1), fmt.Sprintf("Agent CRD contextWindow limits = %v", limits)
			},
		},
		{
			name:   "worker create_agent model maxTokens",
			limits: []string{"worker/create_agent.model.maxTokens minimum"},
			check: func(t *testing.T) (bool, string) {
				if limits := evalCRDModelField(t, "maxTokens"); limits["minimum"] == float64(1) {
					return true, ""
				}
				t.Setenv(envOrkaTaskName, "parent")
				tool, ctx := workerTool("create_agent", newFakeClient(&corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: defaultNamespace}}))
				return evalRejects(ctx, tool, `{"role":"coder","systemPrompt":"s","model":{"name":"m","maxTokens":0}}`, "maxTokens")
			},
		},
		{
			name:   "worker delegate_task workspace",
			limits: []string{"worker/delegate_task.workspace.intent enum", "worker/delegate_task.workspace.prBody maxLength", "worker/delegate_task.workspace.prTitle maxLength"},
			check: func(t *testing.T) (bool, string) {
				tool, ctx := workerTool("delegate_task", newFakeClient(&corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: defaultNamespace},
					Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}},
				}))
				base := `{"agent":"coder","prompt":"p","workspace":{"gitRepo":"https://github.com/acme/api",%s}}`
				return evalAll(
					func() (bool, string) {
						intent := func(v string) string { return fmt.Sprintf(base, `"intent":"`+v+`"`) }
						return evalEnumEnforced(ctx, tool, intent, "intent", "delete", "read", "write")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, fmt.Sprintf(base, `"intent":"write","createPR":true,"prTitle":"`+long(257)+`"`), "prTitle")
					},
					func() (bool, string) {
						return evalRejects(ctx, tool, fmt.Sprintf(base, `"intent":"write","createPR":true,"prBody":"`+long(32769)+`"`), "prBody")
					},
				)
			},
		},
		{
			name:   "worker file_write mode",
			limits: []string{"worker/file_write.mode enum"},
			check: func(t *testing.T) (bool, string) {
				dir := t.TempDir()
				tool := &FileWriteTool{workDir: dir, maxFileSize: 1 << 20, allowedPaths: []string{dir}}
				mode := func(v string) string { return `{"path":"eval.txt","content":"x","mode":"` + v + `"}` }
				return evalEnumEnforced(context.Background(), tool, mode, "mode", "truncate", "write", "append")
			},
		},
		{
			name:   "worker list pages",
			limits: []string{"worker/list_issues.per_page maximum", "worker/list_issues.per_page minimum", "worker/list_pull_requests.per_page maximum", "worker/list_pull_requests.per_page minimum"},
			check: func(t *testing.T) (bool, string) {
				clamped := func(make func(client.Client, string) Tool, perPage int) (bool, string) {
					stub := newEvalHTTPStub(t)
					tool := make(githubTask(), stub.URL)
					args := fmt.Sprintf(`{"task_name":%q,"repo_url":%q,"per_page":%d}`, testCoderTaskName, testOrgTestRepoURL, perPage)
					_, _, _ = evalToolExecute(context.Background(), tool, args)
					switch {
					case stub.sent(fmt.Sprintf("per_page=%d", perPage)):
						return false, fmt.Sprintf("%s sent per_page=%d to GitHub", tool.Name(), perPage)
					case !stub.sent("per_page="):
						return false, fmt.Sprintf("%s made no list request for per_page=%d", tool.Name(), perPage)
					}
					return true, ""
				}
				issues := func(c client.Client, url string) Tool { return &ListIssuesTool{k8sClient: c, apiBaseURL: url} }
				pulls := func(c client.Client, url string) Tool { return &ListPullRequestsTool{k8sClient: c, apiBaseURL: url} }
				return evalAll(
					func() (bool, string) { return clamped(issues, 500) },
					func() (bool, string) { return clamped(issues, -5) },
					func() (bool, string) { return clamped(pulls, 500) },
					func() (bool, string) { return clamped(pulls, -5) },
				)
			},
		},
		{
			name:   "worker merge_pull_request merge_method",
			limits: []string{"worker/merge_pull_request.merge_method enum"},
			check:  evalMergeMethodEnforced(func(c client.Client, url string) Tool { return &MergePullRequestTool{k8sClient: c, apiBaseURL: url} }),
		},
		{
			name:   "worker auto_merge_pull_request merge_method",
			limits: []string{"worker/auto_merge_pull_request.merge_method enum"},
			check: evalMergeMethodEnforced(func(c client.Client, url string) Tool {
				return &AutoMergePullRequestTool{k8sClient: c, apiBaseURL: url}
			}),
		},
		{
			name:   "worker post_review_comment event",
			limits: []string{"worker/post_review_comment.event enum"},
			check: func(t *testing.T) (bool, string) {
				task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
				stub := newEvalHTTPStub(t)
				t.Setenv(envOrkaTaskName, testCoderTaskName)
				tool := &PostReviewCommentTool{k8sClient: newFakeClient(task, secret), apiBaseURL: stub.URL}
				event := func(v string) string {
					return fmt.Sprintf(`{"task_name":%q,"pr_number":1,"body":"x","event":%q}`, testCoderTaskName, v)
				}
				return evalEnumEnforced(context.Background(), tool, event, "event", "MAYBE", "APPROVE", "REQUEST_CHANGES", "COMMENT")
			},
		},
		{
			name:   "worker memory limits",
			limits: []string{"worker/recall_memory.limit minimum", "worker/search_transcript.limit minimum", "worker/search_transcript.max_snippet_length minimum"},
			check: func(t *testing.T) (bool, string) {
				recall, ctx := workerTool("recall_memory", newFakeClient())
				search, _ := workerTool("search_transcript", newFakeClient())
				return evalAll(
					func() (bool, string) { return evalRejects(ctx, recall, `{"query":"q","limit":-1}`, "limit") },
					func() (bool, string) { return evalRejects(ctx, search, `{"query":"q","limit":-1}`, "limit") },
					func() (bool, string) {
						return evalRejects(ctx, search, `{"query":"q","max_snippet_length":-1}`, "snippet")
					},
				)
			},
		},
		{
			name:   "worker reply_in_conversation content",
			limits: []string{"worker/reply_in_conversation.content maxLength", "worker/reply_in_conversation.content minLength"},
			check: func(t *testing.T) (bool, string) {
				tool, ctx := workerTool("reply_in_conversation", newFakeClient())
				return evalAll(
					func() (bool, string) { return evalRejects(ctx, tool, `{"content":""}`, "content") },
					func() (bool, string) { return evalRejects(ctx, tool, `{"content":"`+long(16385)+`"}`, "content") },
				)
			},
		},
		{
			name:   "worker request_approval severity",
			limits: []string{"worker/request_approval.severity enum"},
			check: func(t *testing.T) (bool, string) {
				var emitted []approvals.ApprovalTarget
				tool, _ := workerTool("request_approval", newFakeClient())
				ctx := WithToolContext(context.Background(), &ToolContext{
					Namespace: defaultNamespace,
					ApprovalEmitter: func(_ context.Context, target approvals.ApprovalTarget) error {
						emitted = append(emitted, target)
						return nil
					},
				})
				severity := func(v string) string {
					return `{"action":"deploy","targetTool":"deploy_service","targetArguments":{},"severity":"` + v + `"}`
				}
				ok, detail := evalEnumEnforced(ctx, tool, severity, "severity", "meh", "warning", "critical")
				if !ok && len(emitted) > 0 && emitted[0].Severity == "meh" {
					detail = fmt.Sprintf("emitted an approval with severity %q", emitted[0].Severity)
				}
				return ok, detail
			},
		},
		{
			name:   "worker run_validation command",
			limits: []string{"worker/run_validation.command maxLength", "worker/run_validation.command minLength"},
			check: func(t *testing.T) (bool, string) {
				tool, _ := workerTool("run_validation", newFakeClient())
				ctx := WithToolContext(context.Background(), &ToolContext{Brokered: true, Namespace: defaultNamespace, TaskID: "task", TaskUID: "task-uid"})
				return evalAll(
					func() (bool, string) { return evalRejects(ctx, tool, `{"command":""}`, "command") },
					func() (bool, string) { return evalRejects(ctx, tool, `{"command":"`+long(8193)+`"}`, "command") },
				)
			},
		},
		{
			name:   "worker update_plan progress maximum",
			limits: []string{"worker/update_plan.progress_pct maximum"},
			check:  evalProgressEnforced(101),
		},
		{
			name:   "worker update_plan progress minimum",
			limits: []string{"worker/update_plan.progress_pct minimum"},
			check:  evalProgressEnforced(-1),
		},
	}

	covered := map[string]bool{}
	for _, tc := range cases {
		for _, limit := range tc.limits {
			covered[limit] = true
		}
		t.Run(tc.name, func(t *testing.T) {
			if tc.knownDefect != "" && len(tc.limits) != 1 {
				t.Fatalf("a known-defect case must cover exactly one limit, or its failure can hide a regression in the others")
			}
			ok, detail := tc.check(t)
			switch {
			case ok && tc.knownDefect != "":
				t.Errorf("known defect no longer reproduces; remove knownDefect: %s", tc.knownDefect)
			case !ok && tc.knownDefect == "":
				t.Errorf("limit not enforced: %s", detail)
			case !ok:
				t.Logf("known defect still present: %s (%s)", tc.knownDefect, detail)
			}
		})
	}
	declared := evalToolLimits(t)
	for _, limit := range declared {
		if !covered[limit] {
			t.Errorf("schema limit %q has no enforcement case; add one", limit)
		}
	}
	for limit := range covered {
		if !slices.Contains(declared, limit) {
			t.Errorf("enforcement case covers %q, which no schema declares; remove it", limit)
		}
	}
}

// evalMergeMethodEnforced checks that a merge tool rejects an unsupported
// merge_method, reporting whether the value reached the GitHub stub.
func evalMergeMethodEnforced(build func(client.Client, string) Tool) func(*testing.T) (bool, string) {
	return func(t *testing.T) (bool, string) {
		task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
		stub := newEvalHTTPStub(t)
		tool := build(newFakeClient(task, secret), stub.URL)
		method := func(v string) string {
			return fmt.Sprintf(`{"task_name":%q,"pr_number":1,"merge_method":%q}`, testCoderTaskName, v)
		}
		ok, detail := evalEnumEnforced(context.Background(), tool, method, "merge_method", "fast-forward", "merge", "squash", "rebase")
		if !ok && stub.sent(`"merge_method":"fast-forward"`) {
			detail = tool.Name() + " sent merge_method=fast-forward to GitHub"
		}
		return ok, detail
	}
}

// evalProgressEnforced checks that update_plan rejects an out-of-range
// progress_pct, reporting whether the value reached the controller stub.
func evalProgressEnforced(progress int) func(*testing.T) (bool, string) {
	return func(t *testing.T) (bool, string) {
		controller := newEvalHTTPStub(t)
		t.Setenv(envOrkaControllerURL, controller.URL)
		t.Setenv(envOrkaTaskName, "task")
		t.Setenv(workerenv.ServiceAccountToken, "token")
		tool, _ := evalToolRegistries(newFakeClient())["worker"].Get("update_plan")
		ok, detail := evalRejects(context.Background(), tool, fmt.Sprintf(`{"summary":"s","plan_document":"p","progress_pct":%d}`, progress), "progress")
		if !ok && controller.sent(fmt.Sprintf(`"progress_pct":%d`, progress)) {
			detail = fmt.Sprintf("sent progress_pct=%d to the controller", progress)
		}
		return ok, detail
	}
}
