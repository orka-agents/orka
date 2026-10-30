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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/labels"
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
		NewDelegateTaskTool(fc), NewWaitForTasksTool(fc), NewCreateContainerTaskTool(fc), NewCancelTaskTool(),
		NewSendMessageTool(), NewCheckMessagesTool(), NewCreatePullRequestTool(fc), NewCheckPullRequestCITool(fc),
		NewMergePullRequestTool(fc), NewAutoMergePullRequestTool(fc), NewReviewPullRequestTool(fc),
		NewPostReviewCommentTool(fc), NewCheckPRReviewMarkerTool(fc), NewListIssuesTool(fc), NewListPullRequestsTool(fc),
		NewGetIssueTool(fc), NewCommentOnIssueTool(fc), NewCreateAgentTool(fc, executionmode.HarnessV2),
		NewDeleteAgentTool(fc), NewUpdatePlanTool(),
		NewRecallMemoryTool(), NewRememberMemoryTool(), NewProposeMemoryTool(), NewSearchTranscriptTool(),
	} {
		worker.Register(tool)
	}
	// The ACP broker builds its own registry. Sweep the tools only it offers,
	// such as run_validation, and those whose schema differs from the worker
	// tool of the same name; the rest are covered.
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
	t.Setenv("ORKA_WORK_DIR", t.TempDir())
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

// evalToolContextFor gives each tool the context its caller builds, so each
// takes its real path: chat tools the chat executor's, worker tools a
// worker's narrower one, and brokered tools the ACP broker's authenticated one.
func evalToolContextFor(label string, fc client.Client) context.Context {
	switch label {
	case "worker":
		return WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
	case "brokered":
		return WithToolContext(context.Background(), &ToolContext{Client: fc, Brokered: true, Namespace: defaultNamespace, TaskID: evalPlaceholder, TaskUID: "eval-uid"})
	}
	return evalToolContext(fc)
}

// evalRegistryExecute runs a tool through Registry.Execute, the path chat,
// worker, and broker calls take, converting a panic into a reported failure.
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

// evalPanics counts the panics evalToolExecute recovers, so a check that
// reports failure cannot pass a panic off as a known defect.
var evalPanics atomic.Int64

// evalToolExecute uses the production registry entrypoint, converting a panic
// into a reported failure. Register the fixture instance so its local clients
// and recording executor are preserved.
func evalToolExecute(ctx context.Context, tool Tool, args string) (result string, panicked any, err error) {
	defer func() {
		if panicked = recover(); panicked != nil {
			evalPanics.Add(1)
		}
	}()
	registry := NewRegistry()
	registry.Register(tool)
	result, err = registry.Execute(ctx, tool.Name(), json.RawMessage(args))
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

func TestToolEvalFailureClassification(t *testing.T) {
	cases := []struct {
		name    string
		result  string
		err     error
		failed  bool
		message string
	}{
		{name: "merge_pull_request", result: `{"merged":true,"checks_passed":true,"message":"Pull request merged successfully"}`},
		{name: "auto_merge_pull_request", result: `{"merged":true,"checks_passed":true,"outcome":"merged","message":"Pull request merged successfully"}`},
		{name: "check_pull_request_ci", result: `{"status":"passed","checks_passed":true,"message":"all CI checks passed"}`},
		{name: "structured error", result: `{"success":false,"error":"invalid timeout","message":"informational"}`, failed: true, message: "invalid timeout"},
		{name: "executor error", result: `{"error":"execution failed"}`, failed: true, message: "execution failed"},
		{name: "failure without error", result: `{"success":false,"message":"not an error field"}`, failed: true},
		{name: "Go error", result: `{"message":"informational"}`, err: fmt.Errorf("request failed"), failed: true, message: "request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failed, message := evalToolFailure(tc.result, tc.err)
			if failed != tc.failed || message != tc.message {
				t.Fatalf("evalToolFailure = (%v, %q), want (%v, %q)", failed, message, tc.failed, tc.message)
			}
			if !tc.failed {
				before := evalBaselineFailures.Load()
				call := func() (Tool, context.Context) {
					return &mockTool{
						name:       tc.name,
						parameters: json.RawMessage(`{"type":"object"}`),
						executeFunc: func(context.Context, json.RawMessage) (string, error) {
							return tc.result, nil
						},
					}, context.Background()
				}
				if ok, detail := evalSucceeds(call, `{}`); !ok {
					t.Fatalf("successful message payload failed its valid control: %s", detail)
				}
				if evalBaselineFailures.Load() != before {
					t.Fatal("successful message payload incremented evalBaselineFailures")
				}
			}
		})
	}
}

func TestToolEvalRegistriesCoverKnownTools(t *testing.T) {
	evalToolSandbox(t)
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
// non-object JSON, omitted required fields, and nested objects encoded as
// strings. RequiredFields isolates required fields; TopLevelArgumentTypes and
// NestedArgumentTypes probe types with executable controls. A tool must never
// panic, accept a non-null non-object root, or accept missing required fields.
// Object strings must be rejected or decoded equivalently, and every failure
// must have a diagnostic. Chat tools return structured results or typed
// ToolArgumentError values that the chat executor turns into structured errors;
// other Go errors are failures.
func TestToolEvalMalformedArguments(t *testing.T) {
	evalToolSandbox(t)
	// Worker tools resolve their parent Task from the environment; the seeded
	// cluster holds it.
	t.Setenv(envOrkaTaskName, evalPlaceholder)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	knownDefects := map[string]string{}
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

			inputs := map[string]string{"null": `null`, "array": `[]`, "string": `"text"`, "number": `42`, "boolean": `true`, "empty object": `{}`}
			for prop, raw := range properties {
				propSchema, _ := raw.(map[string]any)
				typ, _ := propSchema["type"].(string)
				if typ == "object" {
					key := fmt.Sprintf("%s/%s: %s as JSON string", label, name, prop)
					switch verdict := evalObjectStringHandling(label, name, required, properties, prop); {
					case verdict == "":
					case verdict == evalInconclusive:
						inconclusive = append(inconclusive, key)
					case strings.HasPrefix(verdict, evalPanicked), strings.HasPrefix(verdict, evalGoError):
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
					case (input == "array" || input == "string" || input == "number" || input == "boolean") && !failed:
						violations[key] = fmt.Sprintf("reported success for a non-object root in the %s cluster", cluster)
					case (input == "empty object" || input == "null") && len(required) > 0 && !failed:
						violations[key] = fmt.Sprintf("reported success without required fields %v in the %s cluster", required, cluster)
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
	evalGoError      = "returned Go error"
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
			if label == "chat" && err != nil && !errors.As(err, new(*ToolArgumentError)) {
				return true, evalGoError + fmt.Sprintf(" %q instead of a structured result", err)
			}
			failed, _ := evalToolFailure(result, err)
			return failed, evalClusterSnapshot(fc)
		}
		stringFailed, asString := run(string(encoded))
		objectFailed, asObject := run(payload)
		_, omitted := run(nil)
		for _, snapshot := range []string{asString, asObject, omitted} {
			if strings.HasPrefix(snapshot, evalPanicked) || strings.HasPrefix(snapshot, evalGoError) {
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

// evalClusterSnapshot compares effective specs, not generated names or other
// per-call metadata. Sorting retains object kind and multiplicity without
// depending on the order the client returns objects.
func evalClusterSnapshot(fc client.Client) string {
	var agents corev1alpha1.AgentList
	var tasks corev1alpha1.TaskList
	_ = fc.List(context.Background(), &agents)
	_ = fc.List(context.Background(), &tasks)
	parts := make([]string, 0, len(agents.Items)+len(tasks.Items))
	for i := range agents.Items {
		spec, _ := json.Marshal(agents.Items[i].Spec)
		parts = append(parts, "agent "+agents.Items[i].Namespace+" "+string(spec))
	}
	for i := range tasks.Items {
		spec, _ := json.Marshal(tasks.Items[i].Spec)
		parts = append(parts, "task "+tasks.Items[i].Namespace+" "+string(spec))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func TestToolEvalClusterSnapshotIgnoresGeneratedMetadata(t *testing.T) {
	objects := func(suffix string) []client.Object {
		meta := metav1.ObjectMeta{Name: "generated-" + suffix, Namespace: defaultNamespace, ResourceVersion: suffix}
		return []client.Object{
			&corev1alpha1.Agent{ObjectMeta: meta, Spec: corev1alpha1.AgentSpec{ProviderRef: &corev1alpha1.ProviderReference{Name: "provider"}}},
			&corev1alpha1.Task{ObjectMeta: meta, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI}},
		}
	}
	baseline := evalClusterSnapshot(newFakeClient(objects("one")...))
	if got := evalClusterSnapshot(newFakeClient(objects("two")...)); got != baseline {
		t.Fatalf("identical specs with different generated metadata compare unequal: %s != %s", got, baseline)
	}
	for _, kind := range []string{"agent", "task", "namespace", "multiplicity"} {
		t.Run(kind, func(t *testing.T) {
			changed := objects("two")
			switch kind {
			case "agent":
				changed[0].(*corev1alpha1.Agent).Spec.ProviderRef.Name = "other-provider"
			case "task":
				changed[1].(*corev1alpha1.Task).Spec.Type = corev1alpha1.TaskTypeContainer
			case "namespace":
				changed[0].SetNamespace("other-namespace")
			case "multiplicity":
				changed = append(changed, objects("three")...)
			}
			if got := evalClusterSnapshot(newFakeClient(changed...)); got == baseline {
				t.Fatal("snapshot hid an effective spec or object-count difference")
			}
		})
	}
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

// evalToolRequiredArgs builds schema-typed placeholders. Executable controls
// must also use evalArgumentControl for semantic values and dependencies.
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

// evalToolLimits lists every limit (minimum, maximum, enum, length, pattern,
// and closed-object constraints)
// that a tool schema declares with its value, keyed like
// "chat/create_agent_task.maxTurns maximum=100", so changing a schema value
// also requires updating the case that probes it. It also lists every required
// field of a nested object, keyed like
// "chat/create_agent.coordination.allowedAgents[].name required".
// TestToolEvalMalformedArguments covers top-level required fields.
func evalToolLimits(t *testing.T) []string {
	t.Helper()
	var limits []string
	for label, registry := range evalToolRegistries(newFakeClient()) {
		for _, name := range registry.Names() {
			tool, _ := registry.Get(name)
			limits = append(limits, evalSchemaLimits(label+"/"+name, evalToolSchema(t, tool), false)...)
		}
	}
	sort.Strings(limits)
	return limits
}

// evalSchemaLimits follows the same properties, items, and typed-map surfaces
// as the malformed-argument sweep. Map entries use {*} in both inventories.
func evalSchemaLimits(prefix string, schema map[string]any, nested bool) []string {
	var limits []string
	for _, keyword := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "enum", "minLength", "maxLength", "minItems", "maxItems", "pattern"} {
		if value, ok := schema[keyword]; ok {
			limits = append(limits, prefix+" "+keyword+"="+evalLimitValue(value))
		}
	}
	if required, _ := schema["required"].([]any); nested {
		for _, field := range required {
			limits = append(limits, fmt.Sprintf("%s.%v required", prefix, field))
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, raw := range properties {
		if child, ok := raw.(map[string]any); ok {
			limits = append(limits, evalSchemaLimits(prefix+"."+name, child, true)...)
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		limits = append(limits, evalSchemaLimits(prefix+"[]", items, true)...)
	}
	switch entries := schema["additionalProperties"].(type) {
	case map[string]any:
		limits = append(limits, evalSchemaLimits(prefix+"{*}", entries, true)...)
	case bool:
		if !entries {
			limits = append(limits, prefix+" additionalProperties=false")
		}
	}
	return limits
}

func TestToolEvalLimitsIncludeTypedMapValues(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{
		"type":"object",
		"additionalProperties":false,
		"required":["settings"],
		"properties":{
			"settings":{"type":"object","additionalProperties":{
				"type":"string","minLength":1,"maxLength":8,"enum":["dev","prod"]
			}},
			"batches":{"type":"array","items":{"type":"object","additionalProperties":{
				"type":"object","required":["value"],"properties":{
					"value":{"type":"string","minLength":2,"maxLength":12,"enum":["staging","prod"]}
				}
			}}},
			"free":{"type":"object","additionalProperties":true},
			"closed":{"type":"object","additionalProperties":false}
		}
	}`), &schema); err != nil {
		t.Fatal(err)
	}
	got := evalSchemaLimits("fixture/tool", schema, false)
	sort.Strings(got)
	want := []string{
		"fixture/tool additionalProperties=false",
		"fixture/tool.batches[]{*}.value enum=staging|prod",
		"fixture/tool.batches[]{*}.value maxLength=12",
		"fixture/tool.batches[]{*}.value minLength=2",
		"fixture/tool.batches[]{*}.value required",
		"fixture/tool.closed additionalProperties=false",
		"fixture/tool.settings{*} enum=dev|prod",
		"fixture/tool.settings{*} maxLength=8",
		"fixture/tool.settings{*} minLength=1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("schema limits = %v, want %v", got, want)
	}
}

// evalLimitValue formats a schema keyword's value for a limit key: a number in
// plain notation, or enum values joined with "|".
func evalLimitValue(value any) string {
	switch v := value.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = fmt.Sprint(item)
		}
		return strings.Join(parts, "|")
	}
	return fmt.Sprint(value)
}

// evalBaselineFailures counts calls that a check expected to succeed but that
// failed, so a case whose valid call breaks cannot pass or be excused as a
// known defect.
var evalBaselineFailures atomic.Int64

// evalCall returns a fresh tool and context for one call, so one call's side
// effects cannot cause another call's failure.
type evalCall func() (Tool, context.Context)

// evalSucceeds runs a call that must succeed, counting a failure in
// evalBaselineFailures.
func evalSucceeds(call evalCall, args string) (bool, string) {
	tool, ctx := call()
	result, panicked, err := evalToolExecute(ctx, tool, args)
	if panicked != nil {
		return false, fmt.Sprintf("panicked: %v", panicked)
	}
	if failed, message := evalToolFailure(result, err); failed {
		evalBaselineFailures.Add(1)
		return false, fmt.Sprintf("valid call %s failed: %s", args, message)
	}
	return true, ""
}

// evalRejectsChange checks one change to a valid call: the valid call must
// succeed, and the same call with one field changed must fail with a message
// naming that field.
func evalRejectsChange(call evalCall, valid, invalid, field string) (bool, string) {
	if ok, detail := evalSucceeds(call, valid); !ok {
		return false, detail
	}
	tool, ctx := call()
	return evalRejects(ctx, tool, invalid, field)
}

// evalRejects reports whether a tool call failed with a message naming the
// limited field, so a rejection for an unrelated reason does not count. Use it
// directly only where no call can succeed in the eval, such as tools that need
// a real network or gateway; otherwise use evalRejectsChange.
func evalRejects(ctx context.Context, tool Tool, args, field string) (bool, string) {
	result, panicked, err := evalToolExecute(ctx, tool, args)
	if panicked != nil {
		return false, fmt.Sprintf("panicked: %v", panicked)
	}
	failed, message := evalToolFailure(result, err)
	switch {
	case !failed:
		return false, "accepted " + args
	case !evalFieldMentioned(message, field):
		return false, fmt.Sprintf("rejected %s for another reason: %s", args, message)
	}
	return true, ""
}

// evalWebFetchURLLimit changes only URL length, keeping max_chars valid. The
// control is exactly at the advertised character limit, not just a short URL.
func evalWebFetchURLLimit(call evalCall) (bool, string) {
	const prefix = "https://1.1.1.1/"
	maxLength := brokeredWebFetchMaxURLBytes / utf8.UTFMax
	args := func(length int) string {
		return fmt.Sprintf(`{"url":%q,"max_chars":%d}`, prefix+strings.Repeat("x", length-len(prefix)), brokeredWebFetchMaxChars)
	}
	ok, detail := evalRejectsChange(call, args(maxLength), args(maxLength+1), "url")
	if strings.HasPrefix(detail, "accepted ") {
		detail = fmt.Sprintf("accepted URL with %d characters and max_chars=%d", maxLength+1, brokeredWebFetchMaxChars)
	}
	return ok, detail
}

func TestToolEvalWebFetchURLLimit(t *testing.T) {
	cases := []struct {
		name      string
		rejection string
		ok        bool
		detail    string
	}{
		{name: "wrong-reason rejection", rejection: "max_chars must be no greater than 50000", detail: "for another reason: max_chars"},
		{name: "URL rejection", rejection: "url must be no greater than 16384 characters", ok: true},
		{name: "accepted overlong URL", detail: "accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			call := func() (Tool, context.Context) {
				return &mockTool{executeFunc: func(_ context.Context, raw json.RawMessage) (string, error) {
					var args WebFetchArgs
					if err := json.Unmarshal(raw, &args); err != nil {
						t.Fatal(err)
					}
					wantLength := brokeredWebFetchMaxURLBytes/utf8.UTFMax + calls
					if utf8.RuneCountInString(args.URL) != wantLength || args.MaxChars != brokeredWebFetchMaxChars {
						t.Fatalf("call %d: URL length = %d, max_chars = %d, want %d and %d", calls, utf8.RuneCountInString(args.URL), args.MaxChars, wantLength, brokeredWebFetchMaxChars)
					}
					calls++
					if calls == 2 && tc.rejection != "" {
						return "", fmt.Errorf("%s", tc.rejection)
					}
					return `{"content":"eval"}`, nil
				}}, context.Background()
			}
			ok, detail := evalWebFetchURLLimit(call)
			if ok != tc.ok || (tc.detail == "" && detail != "") || !strings.Contains(detail, tc.detail) {
				t.Errorf("URL limit check = (%v, %q), want (%v, detail containing %q)", ok, detail, tc.ok, tc.detail)
			}
			if calls != 2 {
				t.Errorf("calls = %d, want valid control and invalid probe", calls)
			}
		})
	}
}

// evalEnumEnforced checks that every declared value succeeds and that a value
// outside the enum fails with a message naming the field.
func evalEnumEnforced(call evalCall, args func(value string) string, field, invalid string, declared ...string) (bool, string) {
	for _, value := range declared {
		if ok, detail := evalSucceeds(call, args(value)); !ok {
			return false, detail
		}
	}
	tool, ctx := call()
	return evalRejects(ctx, tool, args(invalid), field)
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

// evalCodeExecMinimumTimeout accepts a timeout-specific rejection or a clamp
// observed in the invalid call's own executor request, never the valid control's.
func evalCodeExecMinimumTimeout(tool Tool, sandbox *recordingCodeExecutor) (bool, string) {
	call := func() (Tool, context.Context) { return tool, context.Background() }
	timeout := func(v int) string { return fmt.Sprintf(`{"language":"bash","code":"echo test","timeout":%d}`, v) }
	if ok, detail := evalSucceeds(call, timeout(1)); !ok {
		return false, detail
	}
	before := sandbox.calls
	ok, detail := evalRejects(context.Background(), tool, timeout(0), "timeout")
	if ok {
		return true, ""
	}
	if sandbox.calls != before+1 {
		return false, detail
	}
	if sandbox.req.Timeout == time.Second {
		return true, ""
	}
	return false, fmt.Sprintf("timeout 0 ran with %s", sandbox.req.Timeout)
}

func TestToolEvalCodeExecMinimumTimeout(t *testing.T) {
	cases := []struct {
		name      string
		rejection string
		timeout   time.Duration
		ok        bool
		calls     int
		detail    string
	}{
		{name: "wrong-reason rejection", rejection: "unsupported language: bash", calls: 1, detail: "for another reason: unsupported language: bash"},
		{name: "timeout rejection", rejection: "timeout must be at least 1", ok: true, calls: 1},
		{name: "clamped timeout", timeout: time.Second, ok: true, calls: 2},
		{name: "default timeout is not a clamp", timeout: defaultCodeExecTimeout, calls: 2, detail: "timeout 0 ran with 30s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, sandbox := evalRecordingCodeExec(t)
			control := &mockTool{
				name:       tool.Name(),
				parameters: tool.Parameters(),
				executeFunc: func(ctx context.Context, raw json.RawMessage) (string, error) {
					var args struct {
						Timeout int `json:"timeout"`
					}
					if err := json.Unmarshal(raw, &args); err != nil {
						return "", err
					}
					if args.Timeout != 0 {
						return tool.Execute(ctx, raw)
					}
					if tc.rejection != "" {
						return "", fmt.Errorf("%s", tc.rejection)
					}
					result := sandbox.Execute(ctx, CodeExecutionRequest{Timeout: tc.timeout})
					encoded, err := json.Marshal(result)
					return string(encoded), err
				},
			}
			ok, detail := evalCodeExecMinimumTimeout(control, sandbox)
			if ok != tc.ok || (tc.detail == "" && detail != "") || !strings.Contains(detail, tc.detail) {
				t.Errorf("minimum timeout check = (%v, %q), want (%v, detail containing %q)", ok, detail, tc.ok, tc.detail)
			}
			if sandbox.calls != tc.calls {
				t.Errorf("executor calls = %d, want %d", sandbox.calls, tc.calls)
			}
			if tc.calls == 1 && sandbox.req.Timeout != time.Second {
				t.Errorf("valid control timeout = %s, want 1s", sandbox.req.Timeout)
			}
		})
	}
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
		case strings.HasSuffix(r.URL.Path, "/files"), (r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews")):
			_, _ = fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost || strings.Contains(r.URL.Path, "/issues/"):
			_, _ = fmt.Fprint(w, `{"number":1,"id":1,"html_url":"https://github.com/testorg/testrepo/pull/1"}`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = fmt.Fprint(w, `{"number":1,"state":"open","mergeable":true,"head":{"sha":"abc123","ref":"feature"},"base":{"ref":"main"}}`)
		default:
			_, _ = fmt.Fprint(w, `[]`)
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}

// perPage returns the per_page values the stub received.
func (s *evalHTTPStub) perPage() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var values []int
	for _, request := range s.requests {
		for _, match := range evalPerPagePattern.FindAllStringSubmatch(request, -1) {
			value, _ := strconv.Atoi(match[1])
			values = append(values, value)
		}
	}
	return values
}

var evalPerPagePattern = regexp.MustCompile(`[?&]per_page=(-?[0-9]+)`)
var evalMonitorPerPagePattern = regexp.MustCompile(`(?m)^1\. Call list_pull_requests [^\n]*per_page (-?[0-9]+)\.$`)

func (s *evalHTTPStub) sent(fragment string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.requests, func(request string) bool { return strings.Contains(request, fragment) })
}

// evalMonitorPagesEnforced accepts a per_page-specific rejection or verifies
// the actual list_pull_requests command in the generated monitor Task.
func evalMonitorPagesEnforced(fc client.Client, tool Tool, ctx context.Context, args string, perPage int) (bool, string) {
	result, panicked, err := evalToolExecute(ctx, tool, args)
	if panicked != nil {
		return false, fmt.Sprintf("per_page=%d: panic=%v", perPage, panicked)
	}
	if failed, message := evalToolFailure(result, err); failed {
		if (perPage < 1 || perPage > 100) && strings.Contains(message, "per_page") {
			return true, ""
		}
		return false, fmt.Sprintf("per_page=%d failed for another reason: %s", perPage, message)
	}
	var tasks corev1alpha1.TaskList
	if err := fc.List(context.Background(), &tasks); err != nil {
		return false, fmt.Sprintf("list monitor Tasks: %v", err)
	}
	if len(tasks.Items) != 1 {
		return false, fmt.Sprintf("per_page=%d created %d monitor Tasks: %s", perPage, len(tasks.Items), result)
	}
	matches := evalMonitorPerPagePattern.FindAllStringSubmatch(tasks.Items[0].Spec.Prompt, -1)
	if len(matches) != 1 {
		return false, "monitor prompt must contain exactly one list_pull_requests per_page command"
	}
	page, err := strconv.Atoi(matches[0][1])
	if err != nil || page < 1 || page > 100 {
		return false, fmt.Sprintf("per_page=%d produced monitor per_page=%s outside 1..100", perPage, matches[0][1])
	}
	if perPage >= 1 && perPage <= 100 && page != perPage {
		return false, fmt.Sprintf("valid per_page=%d changed to %d", perPage, page)
	}
	return true, ""
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

	replyCall := func() (Tool, context.Context) {
		return NewReplyInConversationTool(), replyContext(&replySenderStub{budget: GatewayReplyBudget{Limit: 10}})
	}

	cases := []evalLimitCase{
		{
			name:   "chat wait_for_task timeout bounds",
			limits: []string{"chat/wait_for_task.timeout minimum=1", "chat/wait_for_task.timeout maximum=60"},
			check: func(t *testing.T) (bool, string) {
				call := func() (Tool, context.Context) {
					task := &corev1alpha1.Task{
						ObjectMeta: metav1.ObjectMeta{Name: evalPlaceholder, Namespace: defaultNamespace},
						Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
					}
					return chatTool("wait_for_task", newFakeClient(task))
				}
				// The actual maximum wait is pinned with virtual time by
				// TestWaitForTaskTool_Execute_TimeoutClampsToMax.
				return evalRejectsChange(call, `{"name":"eval","timeout":1}`, `{"name":"eval","timeout":0}`, "timeout")
			},
		},
		{
			name:   "chat create_agent runtime turns",
			limits: []string{"chat/create_agent.runtime.defaultMaxTurns minimum=1", "chat/create_agent.runtime.defaultMaxTurns maximum=1000"},
			check: func(t *testing.T) (bool, string) {
				call := func() (Tool, context.Context) { return chatTool("create_agent", newFakeClient()) }
				turns := func(v int) string {
					return fmt.Sprintf(`{"name":"a","model":{"name":"test-model"},"runtime":{"type":"claude","defaultMaxTurns":%d}}`, v)
				}
				return evalAll(
					func() (bool, string) { return evalRejectsChange(call, turns(1), turns(0), "runtime.defaultMaxTurns") },
					func() (bool, string) {
						return evalRejectsChange(call, turns(1000), turns(1001), "runtime.defaultMaxTurns")
					},
				)
			},
		},
		{
			name:   "chat create_agent runtime contract",
			limits: []string{"chat/create_agent.runtime.contractVersion enum=orka.harness.v2"},
			check: func(t *testing.T) (bool, string) {
				call := func() (Tool, context.Context) { return chatTool("create_agent", newFakeClient()) }
				contract := func(v string) string {
					return fmt.Sprintf(`{"name":"a","model":{"name":"test-model"},"runtime":{"type":"claude","contractVersion":%q}}`, v)
				}
				return evalRejectsChange(call, contract("orka.harness.v2"), contract("orka.harness.v1"), "contractVersion")
			},
		},
		{
			name:   "chat create_agent model counts",
			limits: []string{"chat/create_agent.model.contextWindow minimum=1", "chat/create_agent.model.maxTokens minimum=1"},
			check: func(t *testing.T) (bool, string) {
				call := func() (Tool, context.Context) { return chatTool("create_agent", newFakeClient()) }
				model := func(field string, v int) string {
					return fmt.Sprintf(`{"name":"a","model":{"name":"m","%s":%d}}`, field, v)
				}
				return evalAll(
					func() (bool, string) {
						return evalRejectsChange(call, model("contextWindow", 1), model("contextWindow", 0), "contextWindow")
					},
					func() (bool, string) {
						return evalRejectsChange(call, model("maxTokens", 1), model("maxTokens", 0), "maxTokens")
					},
				)
			},
		},
		{
			name:   "chat create_agent model temperature",
			limits: []string{"chat/create_agent.model.temperature maximum=2", "chat/create_agent.model.temperature minimum=0"},
			check: func(t *testing.T) (bool, string) {
				// The tool stores temperature as given; the Agent CRD rejects it on create.
				limits := evalCRDModelField(t, "temperature")
				ok := limits["minimum"] == float64(0) && limits["maximum"] == float64(2)
				return ok, fmt.Sprintf("Agent CRD temperature limits = %v", limits)
			},
		},
		{
			name:   "chat update_agent model",
			limits: []string{"chat/update_agent.model.contextWindow minimum=1", "chat/update_agent.model.maxTokens minimum=1", "chat/update_agent.model.temperature maximum=2", "chat/update_agent.model.temperature minimum=0"},
			check: func(t *testing.T) (bool, string) {
				call := func() (Tool, context.Context) { return chatTool("update_agent", evalToolClusters["seeded"]()) }
				model := func(field, v string) string { return fmt.Sprintf(`{"name":"eval","model":{"%s":%s}}`, field, v) }
				return evalAll(
					func() (bool, string) {
						return evalRejectsChange(call, model("contextWindow", "1"), model("contextWindow", "0"), "contextWindow")
					},
					func() (bool, string) {
						return evalRejectsChange(call, model("maxTokens", "1"), model("maxTokens", "0"), "maxTokens")
					},
					func() (bool, string) {
						return evalRejectsChange(call, model("temperature", "2"), model("temperature", "2.5"), "temperature")
					},
					func() (bool, string) {
						return evalRejectsChange(call, model("temperature", "0"), model("temperature", "-1"), "temperature")
					},
				)
			},
		},
		{
			name:   "chat create_agent_task maxTurns",
			limits: []string{"chat/create_agent_task.maxTurns maximum=1000", "chat/create_agent_task.maxTurns minimum=1"},
			check: func(t *testing.T) (bool, string) {
				call := func() (Tool, context.Context) { return chatTool("create_agent_task", evalRuntimeAgentCluster()) }
				turns := func(v int) string { return fmt.Sprintf(`{"name":"t","prompt":"p","agentRef":"a","maxTurns":%d}`, v) }
				return evalAll(
					func() (bool, string) { return evalRejectsChange(call, turns(1000), turns(1001), "maxTurns") },
					func() (bool, string) { return evalRejectsChange(call, turns(1), turns(0), "maxTurns") },
				)
			},
		},
		{
			name:   "chat create_agent_task workspace",
			limits: []string{"chat/create_agent_task.workspace.intent enum=read|write", "chat/create_agent_task.workspace.prBody maxLength=32768", "chat/create_agent_task.workspace.prTitle maxLength=256"},
			check: func(t *testing.T) (bool, string) {
				call := func() (Tool, context.Context) { return chatTool("create_agent_task", evalRuntimeAgentCluster()) }
				base := `{"name":"t","prompt":"p","agentRef":"a","workspace":{"gitRepo":"https://github.com/acme/api",%s}}`
				pr := func(field string, n int) string {
					return fmt.Sprintf(base, `"intent":"write","publicationCredentialRef":"git-write","createPR":true,"prBaseBranch":"main","forgeCredentialRef":"forge","`+field+`":"`+long(n)+`"`)
				}
				return evalAll(
					func() (bool, string) {
						return evalEnumEnforced(call, evalWorkspaceIntent(base), "intent", "delete", "read", "write")
					},
					func() (bool, string) {
						return evalRejectsChange(call, pr("prTitle", 256), pr("prTitle", 257), "prTitle")
					},
					func() (bool, string) {
						return evalRejectsChange(call, pr("prBody", 32768), pr("prBody", 32769), "prBody")
					},
				)
			},
		},
		{
			name: "task priority",
			limits: []string{
				"chat/create_ai_task.priority maximum=1000", "chat/create_ai_task.priority minimum=0",
				"chat/create_container_task.priority maximum=1000", "chat/create_container_task.priority minimum=0",
				"worker/create_container_task.priority maximum=1000", "worker/create_container_task.priority minimum=0",
			},
			check: func(t *testing.T) (bool, string) {
				t.Setenv(envOrkaTaskName, "parent")
				ai := func() (Tool, context.Context) { return chatTool("create_ai_task", newFakeClient()) }
				chatContainer := func() (Tool, context.Context) { return chatTool("create_container_task", newFakeClient()) }
				workerContainer := func() (Tool, context.Context) { return workerTool("create_container_task", evalParentTaskCluster()) }
				aiTask := func(p int) string { return fmt.Sprintf(`{"prompt":"p","priority":%d}`, p) }
				containerTask := func(p int) string {
					return fmt.Sprintf(`{"image":"cgr.dev/chainguard/bash:latest","command":["echo","hi"],"priority":%d}`, p)
				}
				checks := make([]func() (bool, string), 0, 6)
				for _, c := range []struct {
					call evalCall
					args func(int) string
				}{{ai, aiTask}, {chatContainer, containerTask}, {workerContainer, containerTask}} {
					checks = append(checks,
						func() (bool, string) { return evalRejectsChange(c.call, c.args(1000), c.args(1001), "priority") },
						func() (bool, string) { return evalRejectsChange(c.call, c.args(0), c.args(-1), "priority") },
					)
				}
				return evalAll(checks...)
			},
		},
		{
			name:   "chat create_pr_monitor",
			limits: []string{"chat/create_pr_monitor.per_page maximum=100", "chat/create_pr_monitor.per_page minimum=1", "chat/create_pr_monitor.review_event enum=COMMENT|APPROVE|REQUEST_CHANGES"},
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
					return evalMonitorPagesEnforced(fc, tool, ctx, fmt.Sprintf(base, fmt.Sprintf(`,"per_page":%d`, perPage)), perPage)
				}
				call := func() (Tool, context.Context) { return chatTool("create_pr_monitor", agent()) }
				return evalAll(
					func() (bool, string) { return pagesClamped(1) },
					func() (bool, string) { return pagesClamped(100) },
					func() (bool, string) { return pagesClamped(500) },
					func() (bool, string) { return pagesClamped(-5) },
					func() (bool, string) {
						event := func(v string) string { return fmt.Sprintf(base, `,"review_event":"`+v+`"`) }
						return evalEnumEnforced(call, event, "review_event", "MAYBE", "COMMENT", "APPROVE", "REQUEST_CHANGES")
					},
				)
			},
		},
		{
			name:   "worker cancel_task reason",
			limits: []string{"worker/cancel_task.reason maxLength=512"},
			check: func(t *testing.T) (bool, string) {
				requests := startCancelTaskController(t, http.StatusOK, `{"taskName":"child","status":"Cancelled"}`)
				call := func() (Tool, context.Context) { return workerTool("cancel_task", newFakeClient()) }
				for _, character := range []string{"x", "界"} {
					reason := func(n int) string {
						return fmt.Sprintf(`{"task_name":"child","reason":%q}`, strings.Repeat(character, n))
					}
					before := len(*requests)
					if ok, detail := evalRejectsChange(call, reason(MaxCancelReasonLength), reason(MaxCancelReasonLength+1), "reason"); !ok {
						return false, detail
					}
					if len(*requests) != before+1 {
						return false, "an overlong cancellation reason reached the controller"
					}
					if (*requests)[before].reason != strings.Repeat(character, MaxCancelReasonLength) {
						return false, "the valid cancellation reason changed before reaching the controller"
					}
				}
				return true, ""
			},
		},
		{
			name:   "chat list_tasks result bound",
			limits: []string{"chat/list_tasks.limit maximum=100"},
			check:  evalTaskListBound,
		},
		{
			name:   "worker code_exec language and maximum timeout",
			limits: []string{"worker/code_exec.language enum=python|python3|javascript|node|bash|sh", "worker/code_exec.timeout maximum=60"},
			check: func(t *testing.T) (bool, string) {
				// Clamping an over-maximum timeout is pinned by
				// TestCodeExecTool_Execute_TimeoutClampsToMax.
				tool, _ := evalRecordingCodeExec(t)
				call := func() (Tool, context.Context) { return tool, context.Background() }
				language := func(v string) string { return `{"language":"` + v + `","code":"echo test"}` }
				return evalEnumEnforced(call, language, "language", "cobol", "python", "python3", "javascript", "node", "bash", "sh")
			},
		},
		{
			name:   "worker code_exec minimum timeout",
			limits: []string{"worker/code_exec.timeout minimum=1"},
			check: func(t *testing.T) (bool, string) {
				// Zero is the value just below the minimum.
				tool, sandbox := evalRecordingCodeExec(t)
				return evalCodeExecMinimumTimeout(tool, sandbox)
			},
		},
		{
			name:   "brokered web_search",
			limits: []string{"brokered/web_search.limit maximum=10", "brokered/web_search.query maxLength=4096"},
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
			limits: []string{"brokered/web_fetch.max_chars maximum=50000"},
			check: func(t *testing.T) (bool, string) {
				// A public IP literal needs no DNS lookup, and max_chars is
				// checked before any request.
				tool, _ := evalToolRegistries(newFakeClient())["brokered"].Get("web_fetch")
				return evalRejects(context.Background(), tool, `{"url":"https://1.1.1.1/","max_chars":50001}`, "max_chars")
			},
		},
		{
			name:   "brokered web_fetch url",
			limits: []string{"brokered/web_fetch.url maxLength=16384"},
			check: func(t *testing.T) (bool, string) {
				return evalWebFetchURLLimit(func() (Tool, context.Context) {
					tool, _ := evalToolRegistries(newFakeClient())["brokered"].Get("web_fetch")
					// A stubbed transport lets the boundary control succeed and
					// an accepted overlong URL expose the defect without dialing.
					tool.(*WebFetchTool).client = evalWebClient()
					return tool, context.Background()
				})
			},
		},
		{
			name:   "worker create_agent model contextWindow",
			limits: []string{"worker/create_agent.model.contextWindow minimum=1"},
			check: func(t *testing.T) (bool, string) {
				// The tool stores contextWindow as given; the Agent CRD rejects it on create.
				limits := evalCRDModelField(t, "contextWindow")
				return limits["minimum"] == float64(1), fmt.Sprintf("Agent CRD contextWindow limits = %v", limits)
			},
		},
		{
			name:   "worker create_agent runtime contract",
			limits: []string{"worker/create_agent.runtime.contractVersion enum=orka.harness.v2"},
			check: func(t *testing.T) (bool, string) {
				t.Setenv(envOrkaTaskName, "parent")
				call := func() (Tool, context.Context) { return workerTool("create_agent", evalParentTaskCluster()) }
				contract := func(v string) string {
					return fmt.Sprintf(`{"role":"coder","systemPrompt":"s","model":{"name":"m"},"runtime":{"type":"claude","contractVersion":%q}}`, v)
				}
				return evalRejectsChange(call, contract("orka.harness.v2"), contract("orka.harness.v1"), "contractVersion")
			},
		},
		{
			name:   "worker create_agent model maxTokens",
			limits: []string{"worker/create_agent.model.maxTokens minimum=1"},
			check: func(t *testing.T) (bool, string) {
				if limits := evalCRDModelField(t, "maxTokens"); limits["minimum"] == float64(1) {
					return true, ""
				}
				t.Setenv(envOrkaTaskName, "parent")
				call := func() (Tool, context.Context) { return workerTool("create_agent", evalParentTaskCluster()) }
				tokens := func(v int) string {
					return fmt.Sprintf(`{"role":"coder","systemPrompt":"s","model":{"name":"m","maxTokens":%d}}`, v)
				}
				return evalRejectsChange(call, tokens(1), tokens(0), "maxTokens")
			},
		},
		{
			name:   "worker delegate_task workspace",
			limits: []string{"worker/delegate_task.workspace.intent enum=read|write", "worker/delegate_task.workspace.prBody maxLength=32768", "worker/delegate_task.workspace.prTitle maxLength=256"},
			check: func(t *testing.T) (bool, string) {
				call := func() (Tool, context.Context) { return workerTool("delegate_task", evalDelegateCluster()) }
				base := `{"agent":"coder","prompt":"p","workspace":{"gitRepo":"https://github.com/acme/api",%s}}`
				pr := func(field string, n int) string {
					return fmt.Sprintf(base, `"intent":"write","publicationCredentialRef":"git-write","createPR":true,"prBaseBranch":"main","forgeCredentialRef":"forge","`+field+`":"`+long(n)+`"`)
				}
				return evalAll(
					func() (bool, string) {
						return evalEnumEnforced(call, evalWorkspaceIntent(base), "intent", "delete", "read", "write")
					},
					func() (bool, string) {
						return evalRejectsChange(call, pr("prTitle", 256), pr("prTitle", 257), "prTitle")
					},
					func() (bool, string) {
						return evalRejectsChange(call, pr("prBody", 32768), pr("prBody", 32769), "prBody")
					},
				)
			},
		},
		{
			name:   "worker file_write mode",
			limits: []string{"worker/file_write.mode enum=write|append"},
			check: func(t *testing.T) (bool, string) {
				dir := t.TempDir()
				tool := &FileWriteTool{workDir: dir, maxFileSize: 1 << 20, allowedPaths: []string{dir}}
				call := func() (Tool, context.Context) { return tool, context.Background() }
				mode := func(v string) string { return `{"path":"eval.txt","content":"x","mode":"` + v + `"}` }
				return evalEnumEnforced(call, mode, "mode", "truncate", "write", "append")
			},
		},
		{
			name:   "worker list pages",
			limits: []string{"worker/list_issues.per_page maximum=100", "worker/list_issues.per_page minimum=1", "worker/list_pull_requests.per_page maximum=100", "worker/list_pull_requests.per_page minimum=1"},
			check: func(t *testing.T) (bool, string) {
				clamped := func(make func(client.Client, string) Tool, perPage int) (bool, string) {
					stub := newEvalHTTPStub(t)
					tool := make(githubTask(), stub.URL)
					args := fmt.Sprintf(`{"task_name":%q,"repo_url":%q,"per_page":%d}`, testCoderTaskName, testOrgTestRepoURL, perPage)
					result, panicked, err := evalToolExecute(context.Background(), tool, args)
					if panicked != nil {
						return false, fmt.Sprintf("%s panicked: %v", tool.Name(), panicked)
					}
					if failed, message := evalToolFailure(result, err); failed {
						if (perPage < 1 || perPage > 100) && strings.Contains(message, "per_page") {
							return true, ""
						}
						return false, fmt.Sprintf("%s failed for another reason: %s", tool.Name(), message)
					}
					sent := stub.perPage()
					switch {
					case len(sent) == 0:
						return false, fmt.Sprintf("%s made no list request for per_page=%d", tool.Name(), perPage)
					case slices.ContainsFunc(sent, func(v int) bool { return v < 1 || v > 100 }):
						return false, fmt.Sprintf("%s sent per_page=%v to GitHub for per_page=%d", tool.Name(), sent, perPage)
					}
					if perPage >= 1 && perPage <= 100 && slices.ContainsFunc(sent, func(v int) bool { return v != perPage }) {
						return false, fmt.Sprintf("%s changed valid per_page=%d to %v", tool.Name(), perPage, sent)
					}
					return true, ""
				}
				issues := func(c client.Client, url string) Tool { return &ListIssuesTool{k8sClient: c, apiBaseURL: url} }
				pulls := func(c client.Client, url string) Tool { return &ListPullRequestsTool{k8sClient: c, apiBaseURL: url} }
				return evalAll(
					func() (bool, string) { return clamped(issues, 1) },
					func() (bool, string) { return clamped(issues, 100) },
					func() (bool, string) { return clamped(issues, 500) },
					func() (bool, string) { return clamped(issues, -5) },
					func() (bool, string) { return clamped(pulls, 1) },
					func() (bool, string) { return clamped(pulls, 100) },
					func() (bool, string) { return clamped(pulls, 500) },
					func() (bool, string) { return clamped(pulls, -5) },
				)
			},
		},
		{
			name:   "worker merge_pull_request merge_method",
			limits: []string{"worker/merge_pull_request.merge_method enum=merge|squash|rebase"},
			check:  evalMergeMethodEnforced(func(c client.Client, url string) Tool { return &MergePullRequestTool{k8sClient: c, apiBaseURL: url} }),
		},
		{
			name:   "worker auto_merge_pull_request merge_method",
			limits: []string{"worker/auto_merge_pull_request.merge_method enum=merge|squash|rebase"},
			check: evalMergeMethodEnforced(func(c client.Client, url string) Tool {
				return &AutoMergePullRequestTool{k8sClient: c, apiBaseURL: url}
			}),
		},
		{
			name:   "worker post_review_comment event",
			limits: []string{"worker/post_review_comment.event enum=APPROVE|REQUEST_CHANGES|COMMENT"},
			check: func(t *testing.T) (bool, string) {
				task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
				stub := newEvalHTTPStub(t)
				t.Setenv(envOrkaTaskName, testCoderTaskName)
				tool := &PostReviewCommentTool{k8sClient: newFakeClient(task, secret), apiBaseURL: stub.URL}
				event := func(v string) string {
					return fmt.Sprintf(`{"task_name":%q,"pr_number":1,"body":"x","event":%q}`, testCoderTaskName, v)
				}
				call := func() (Tool, context.Context) { return tool, context.Background() }
				return evalEnumEnforced(call, event, "event", "MAYBE", "APPROVE", "REQUEST_CHANGES", "COMMENT")
			},
		},
		{
			name:   "worker memory limits",
			limits: []string{"worker/recall_memory.limit minimum=0", "worker/search_transcript.limit minimum=0", "worker/search_transcript.max_snippet_length minimum=0"},
			check: func(t *testing.T) (bool, string) {
				controller := newEvalHTTPStub(t)
				t.Setenv(envOrkaControllerURL, controller.URL)
				t.Setenv(envOrkaTaskName, "task")
				t.Setenv(workerenv.ServiceAccountToken, "token")
				recall := func() (Tool, context.Context) { return workerTool("recall_memory", newFakeClient()) }
				search := func() (Tool, context.Context) { return workerTool("search_transcript", newFakeClient()) }
				return evalAll(
					func() (bool, string) {
						return evalRejectsChange(recall, `{"query":"q","limit":0}`, `{"query":"q","limit":-1}`, "limit")
					},
					func() (bool, string) {
						return evalRejectsChange(search, `{"query":"q","limit":0}`, `{"query":"q","limit":-1}`, "limit")
					},
					func() (bool, string) {
						return evalRejectsChange(search, `{"query":"q","max_snippet_length":0}`, `{"query":"q","max_snippet_length":-1}`, "snippet")
					},
				)
			},
		},
		{
			name:   "worker reply_in_conversation additionalProperties",
			limits: []string{"worker/reply_in_conversation additionalProperties=false"},
			check: func(t *testing.T) (bool, string) {
				// The generic invalid-request diagnostic names content, not
				// the unknown key. Only the extra field changes from the control.
				return evalRejectsChange(replyCall, `{"content":"hello"}`, `{"content":"hello","target":"other-room"}`, "content")
			},
		},
		{
			name:   "worker reply_in_conversation content minimum",
			limits: []string{"worker/reply_in_conversation.content minLength=1"},
			check: func(t *testing.T) (bool, string) {
				return evalRejectsChange(replyCall, `{"content":"x"}`, `{"content":""}`, "content")
			},
		},
		{
			name:   "worker reply_in_conversation content ASCII maximum",
			limits: []string{"worker/reply_in_conversation.content maxLength=16384"},
			check: func(t *testing.T) (bool, string) {
				return evalRejectsChange(replyCall, `{"content":"`+long(16384)+`"}`, `{"content":"`+long(16385)+`"}`, "content")
			},
		},
		{
			name:        "worker reply_in_conversation content Unicode maximum",
			limits:      []string{"worker/reply_in_conversation.content maxLength=16384"},
			knownDefect: "reply_in_conversation advertises a 16384-character maximum but rejects schema-valid multibyte content above 16384 UTF-8 bytes",
			check: func(t *testing.T) (bool, string) {
				args := func(content string) string { return `{"content":"` + content + `"}` }
				// Prove authenticated multibyte replies work up to the byte
				// boundary before probing schema-valid strings beyond it.
				for _, content := range []string{strings.Repeat("é", 8192), strings.Repeat("界", 5461)} {
					if ok, detail := evalSucceeds(replyCall, args(content)); !ok {
						return false, detail
					}
				}
				var rejections []string
				for _, characters := range []int{5462, 16384} {
					content := strings.Repeat("界", characters)
					tool, ctx := replyCall()
					if failed, message := evalArgumentFailure(t, "worker", ctx, tool, args(content)); failed {
						if !evalFieldMentioned(message, "content") {
							t.Errorf("schema-valid content rejected for another reason: %s", message)
						}
						rejections = append(rejections, fmt.Sprintf("schema-valid content with %d characters/%d UTF-8 bytes rejected: %s", characters, len(content), message))
					}
				}
				return len(rejections) == 0, strings.Join(rejections, "; ")
			},
		},
		{
			name:   "worker request_approval severity",
			limits: []string{"worker/request_approval.severity enum=warning|critical"},
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
				call := func() (Tool, context.Context) { return tool, ctx }
				ok, detail := evalEnumEnforced(call, severity, "severity", "meh", "warning", "critical")
				if !ok && slices.ContainsFunc(emitted, func(target approvals.ApprovalTarget) bool { return target.Severity == "meh" }) {
					detail = `emitted an approval with severity "meh"`
				}
				return ok, detail
			},
		},
		{
			name:   "brokered run_validation command",
			limits: []string{"brokered/run_validation.command maxLength=8192", "brokered/run_validation.command minLength=1"},
			check: func(t *testing.T) (bool, string) {
				fc := newFakeClient()
				tool, _ := evalToolRegistries(fc)["brokered"].Get("run_validation")
				ctx := evalToolContextFor("brokered", fc)
				return evalAll(
					func() (bool, string) { return evalRejects(ctx, tool, `{"command":""}`, "command") },
					func() (bool, string) { return evalRejects(ctx, tool, `{"command":"`+long(8193)+`"}`, "command") },
				)
			},
		},
		{
			name: "chat create_agent allowed agent name",
			limits: []string{
				"chat/create_agent.coordination.allowedAgents[].name required",
				"chat/create_agent.coordination.allowedAgents[].name minLength=1",
				"chat/create_agent.coordination.allowedAgents[].name pattern=\\S",
			},
			check: evalAllowedAgentNameRequired(chatTool, `"name":"a","systemPrompt":"p"`),
		},
		{
			name: "worker create_agent allowed agent name",
			limits: []string{
				"worker/create_agent.coordination.allowedAgents[].name required",
				"worker/create_agent.coordination.allowedAgents[].name minLength=1",
				"worker/create_agent.coordination.allowedAgents[].name pattern=\\S",
			},
			check: evalAllowedAgentNameRequired(workerTool, `"role":"coder","systemPrompt":"p"`),
		},
		{
			name:   "chat create_agent soul",
			limits: evalSoulLimits("chat/create_agent"),
			check:  evalSoulEnforced(chatTool, "create_agent", `"name":"a","systemPrompt":"p"`, evalParentTaskCluster),
		},
		{
			name:   "chat update_agent soul",
			limits: evalSoulLimits("chat/update_agent"),
			check:  evalSoulEnforced(chatTool, "update_agent", `"name":"eval"`, evalToolClusters["seeded"]),
		},
		{
			name:   "worker create_agent soul",
			limits: evalSoulLimits("worker/create_agent"),
			check:  evalSoulEnforced(workerTool, "create_agent", `"role":"coder","systemPrompt":"p"`, evalParentTaskCluster),
		},
		{
			name:   "worker post_review_comment comment path",
			limits: []string{"worker/post_review_comment.comments[].path required"},
			check:  evalReviewCommentFieldRequired("path"),
		},
		{
			name:   "worker post_review_comment comment line",
			limits: []string{"worker/post_review_comment.comments[].line required"},
			check:  evalReviewCommentFieldRequired("line"),
		},
		{
			name: "worker post_review_comment comment text",
			limits: []string{
				"worker/post_review_comment.comments[].path minLength=1", "worker/post_review_comment.comments[].path pattern=\\S",
				"worker/post_review_comment.comments[].body minLength=1", "worker/post_review_comment.comments[].body pattern=\\S",
			},
			check: func(t *testing.T) (bool, string) {
				return evalAll(
					func() (bool, string) { return evalReviewCommentText(t, "path", "") },
					func() (bool, string) { return evalReviewCommentText(t, "path", " ") },
					func() (bool, string) { return evalReviewCommentText(t, "body", "") },
					func() (bool, string) { return evalReviewCommentText(t, "body", " ") },
				)
			},
		},
		{
			name:   "worker post_review_comment comment line minimum",
			limits: []string{"worker/post_review_comment.comments[].line minimum=1"},
			check:  evalReviewCommentLine(0),
		},
		{
			name:   "worker post_review_comment comment body",
			limits: []string{"worker/post_review_comment.comments[].body required"},
			check:  evalReviewCommentFieldRequired("body"),
		},
		{
			name:   "worker update_plan progress maximum",
			limits: []string{"worker/update_plan.progress_pct maximum=100"},
			check:  evalProgressEnforced(100, 101),
		},
		{
			name:   "worker update_plan progress minimum",
			limits: []string{"worker/update_plan.progress_pct minimum=0"},
			check:  evalProgressEnforced(0, -1),
		},
	}

	evalRunLimitCases(t, slices.Concat(cases, evalMeetingDraftLimitCases()))
}

// evalMeetingDraftArgs supplies semantic values that the attendee union and
// timestamp format cannot derive from primitive schema types.
func evalMeetingDraftArgs() map[string]any {
	return map[string]any{
		"subject": "Planning", "agenda": []any{"Next steps"}, "durationMinutes": 30,
		"attendees":       map[string]any{"mode": "everyone"},
		"preferredWindow": "Tomorrow morning", "suggestedStart": "2026-07-01T09:00:00Z",
	}
}

func evalMeetingDraftCall() (Tool, context.Context) {
	sender := &meetingReplySender{replySenderStub: replySenderStub{budget: GatewayReplyBudget{Limit: 10}}}
	return NewReplyInConversationTool(), replyContext(sender)
}

// Meeting validation intentionally does not echo the leaf or its value. Only
// use this for a single meetingDraft mutation of a successful control; require
// the typed, bounded invalid-request diagnostic, not an unsupported host or an
// unrelated admission failure. Generic all-tool field checks stay unchanged.
func evalRejectsMeetingDraft(ctx context.Context, tool Tool, args string) (bool, string) {
	result, panicked, err := evalToolExecute(ctx, tool, args)
	if panicked != nil {
		return false, fmt.Sprintf("panicked: %v", panicked)
	}
	failed, message := evalToolFailure(result, err)
	if !failed {
		return false, "accepted " + args
	}
	var rejection *GatewayReplyRejection
	if !errors.As(err, &rejection) || rejection.Error() != NewGatewayReplyRejection("invalid_request", nil).Error() {
		return false, fmt.Sprintf("rejected %s for another reason: %s", args, message)
	}
	return true, ""
}

func TestToolEvalMeetingDraftRejection(t *testing.T) {
	invalid := NewGatewayReplyRejection("invalid_request", nil)
	for _, tc := range []struct {
		name, result string
		err          error
		ok           bool
	}{
		{name: "invalid request", err: invalid, ok: true},
		{name: "wrapped invalid request", err: fmt.Errorf("reply: %w", invalid), ok: true},
		{name: "unsupported sender", err: NewGatewayReplyRejection("meeting_drafts_unsupported", nil)},
		{name: "quota rejection", err: NewGatewayReplyRejection("limit_reached", nil)},
		{name: "untyped diagnostic", err: errors.New(invalid.Error())},
		{name: "structured diagnostic", result: fmt.Sprintf(`{"error":%q}`, invalid.Error())},
		{name: "accepted", result: `{"success":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &mockTool{
				name: "reply_in_conversation", parameters: json.RawMessage(`{"type":"object"}`),
				executeFunc: func(context.Context, json.RawMessage) (string, error) { return tc.result, tc.err },
			}
			ok, detail := evalRejectsMeetingDraft(context.Background(), tool, `{}`)
			if ok != tc.ok || (ok && detail != "") || (!ok && detail == "") {
				t.Errorf("meeting rejection check = (%v, %q), want accepted rejection %v", ok, detail, tc.ok)
			}
		})
	}
}

func evalMeetingDraftLimitCases() []evalLimitCase {
	args := func(field string, value any, omit bool) string {
		draft := evalMeetingDraftArgs()
		if omit {
			delete(draft, field)
		} else {
			draft[field] = value
		}
		raw, _ := json.Marshal(map[string]any{"content": "Draft proposal", "meetingDraft": draft})
		return string(raw)
	}
	specs := []struct {
		field, limit   string
		valid, invalid any
		omit           bool
	}{
		{"extra", " additionalProperties=false", nil, "other-room", false},
		{"subject", ".subject required", "Planning", nil, true},
		{"agenda", ".agenda required", []any{"Next steps"}, nil, true},
		{"attendees", ".attendees required", map[string]any{"mode": "everyone"}, nil, true},
		{"subject", ".subject minLength=1", "x", "", false},
		{"agenda", ".agenda minItems=1", []any{"x"}, []any{}, false},
		{"agenda", ".agenda maxItems=5", []any{"a", "b", "c", "d", "e"}, []any{"a", "b", "c", "d", "e", "f"}, false},
		{"agenda", ".agenda[] minLength=1", []any{"x"}, []any{""}, false},
		{"durationMinutes", ".durationMinutes minimum=1", 1, 0, false},
		{"durationMinutes", ".durationMinutes maximum=480", 480, 481, false},
		{"preferredWindow", ".preferredWindow minLength=1", "x", "", false},
	}
	cases := make([]evalLimitCase, 0, len(specs))
	for _, tc := range specs {
		cases = append(cases, evalLimitCase{
			name:   "worker reply_in_conversation meetingDraft" + tc.limit,
			limits: []string{"worker/reply_in_conversation.meetingDraft" + tc.limit},
			check: func(t *testing.T) (bool, string) {
				valid := args(tc.field, tc.valid, tc.field == "extra")
				if ok, detail := evalSucceeds(evalMeetingDraftCall, valid); !ok {
					return false, detail
				}
				tool, ctx := evalMeetingDraftCall()
				return evalRejectsMeetingDraft(ctx, tool, args(tc.field, tc.invalid, tc.omit))
			},
		})
	}
	return cases
}

// evalLimitCase checks that one or more declared schema limits are enforced.
type evalLimitCase struct {
	name        string
	limits      []string
	check       func(t *testing.T) (bool, string)
	knownDefect string
}

// evalRunLimitCases runs each case and requires the cases to cover exactly the
// limits the tool schemas declare.
func evalRunLimitCases(t *testing.T, cases []evalLimitCase) {
	t.Helper()
	covered := map[string]bool{}
	for _, tc := range cases {
		for _, limit := range tc.limits {
			covered[limit] = true
		}
		t.Run(tc.name, func(t *testing.T) {
			if tc.knownDefect != "" && len(tc.limits) != 1 {
				t.Fatalf("a known-defect case must cover exactly one limit, or its failure can hide a regression in the others")
			}
			panics, baselines := evalPanics.Load(), evalBaselineFailures.Load()
			ok, detail := tc.check(t)
			switch {
			case evalPanics.Load() != panics:
				t.Errorf("a tool panicked: %s", detail)
			case evalBaselineFailures.Load() != baselines:
				t.Errorf("a valid call failed, so the case does not test its limit: %s", detail)
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
		call := func() (Tool, context.Context) { return tool, context.Background() }
		ok, detail := evalEnumEnforced(call, method, "merge_method", "fast-forward", "merge", "squash", "rebase")
		if !ok && stub.sent(`"merge_method":"fast-forward"`) {
			detail = tool.Name() + " sent merge_method=fast-forward to GitHub"
		}
		return ok, detail
	}
}

// evalWorkspaceIntent builds a workspace with the given intent; write intent
// also names the publication credential it requires.
func evalWorkspaceIntent(base string) func(string) string {
	return func(v string) string {
		if v == "write" {
			return fmt.Sprintf(base, `"intent":"write","publicationCredentialRef":"git-write"`)
		}
		return fmt.Sprintf(base, `"intent":"`+v+`"`)
	}
}

// evalRuntimeAgentCluster holds a runtime Agent named "a" for create_agent_task.
func evalRuntimeAgentCluster() client.Client {
	return newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: defaultNamespace},
		Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}},
	})
}

// evalParentTaskCluster holds the parent Task worker tools resolve from
// ORKA_TASK_NAME=parent.
func evalParentTaskCluster() client.Client {
	return newFakeClient(&corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: defaultNamespace}})
}

// evalDelegateCluster holds a runtime Agent named "coder" for delegate_task.
func evalDelegateCluster() client.Client {
	return newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: defaultNamespace},
		Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}},
	})
}

// evalAllowedAgentNameRequired checks that create_agent, given its required
// fields, rejects missing, empty, or whitespace-only allowed agent names
// instead of dropping or storing them. The worker tool resolves its parent
// Task from the environment.
func evalAllowedAgentNameRequired(get func(string, client.Client) (Tool, context.Context), required string) func(*testing.T) (bool, string) {
	return func(t *testing.T) (bool, string) {
		t.Setenv(envOrkaTaskName, "parent")
		call := func() (Tool, context.Context) { return get("create_agent", evalParentTaskCluster()) }
		allowed := func(agent string) string {
			return `{` + required + `,"coordination":{"enabled":true,"allowedAgents":[` + agent + `]}}`
		}
		valid := allowed(`{"name":"coder","namespace":"default"}`)
		for _, agent := range []string{
			`{"namespace":"default"}`,
			`{"name":"","namespace":"default"}`,
			`{"name":" \t\r\n ","namespace":"default"}`,
		} {
			if ok, detail := evalRejectsChange(call, valid, allowed(agent), "allowedAgents"); !ok {
				return false, detail
			}
		}
		return true, ""
	}
}

// evalSoulLimits lists the limits soulParameterSchema declares for one tool.
func evalSoulLimits(tool string) []string {
	return []string{
		tool + ".soul.inline minLength=1",
		tool + ".soul.inline pattern=" + soulNonWhitespacePattern,
		tool + ".soul.inline maxLength=" + strconv.Itoa(agentcontext.MaxSoulBytes),
		tool + ".soul.digest pattern=^sha256:[0-9a-f]{64}$",
		tool + ".soul.configMapRef.name required",
		tool + ".soul.configMapRef.name minLength=1",
		tool + ".soul.configMapRef.name pattern=" + soulNonWhitespacePattern,
		tool + ".soul.configMapRef.key required",
		tool + ".soul.configMapRef.key minLength=1",
		tool + ".soul.configMapRef.key pattern=" + soulNonWhitespacePattern,
	}
}

// evalSoulEnforced checks that an Agent tool, given its required fields,
// rejects empty, whitespace-only, or oversized inline souls, incomplete
// ConfigMap references, and malformed digests instead of storing them.
func evalSoulEnforced(get func(string, client.Client) (Tool, context.Context), name, required string, cluster func() client.Client) func(*testing.T) (bool, string) {
	return func(t *testing.T) (bool, string) {
		t.Setenv(envOrkaTaskName, "parent")
		call := func() (Tool, context.Context) { return get(name, cluster()) }
		soul := func(value string) string { return `{` + required + `,"soul":` + value + `}` }
		inline := func(text string) string { return soul(fmt.Sprintf(`{"inline":%q}`, text)) }
		digest := agentcontext.Digest(evalPlaceholder)
		ref := func(fields, digest string) string {
			return soul(fmt.Sprintf(`{"configMapRef":{%s},"digest":%q}`, fields, digest))
		}
		validRef := ref(`"name":"eval","key":"eval"`, digest)
		for _, change := range []struct{ valid, invalid string }{
			{inline("eval"), inline("")},
			{inline("eval"), inline(" \t\r\n ")},
			{inline(strings.Repeat("x", agentcontext.MaxSoulBytes)), inline(strings.Repeat("x", agentcontext.MaxSoulBytes+1))},
			{validRef, ref(`"name":"eval","key":"eval"`, "sha256:"+strings.Repeat("X", 64))},
			{validRef, ref(`"key":"eval"`, digest)},
			{validRef, ref(`"name":"","key":"eval"`, digest)},
			{validRef, ref(`"name":" \t ","key":"eval"`, digest)},
			{validRef, ref(`"name":"eval"`, digest)},
			{validRef, ref(`"name":"eval","key":""`, digest)},
			{validRef, ref(`"name":"eval","key":" \t "`, digest)},
		} {
			if ok, detail := evalRejectsChange(call, change.valid, change.invalid, "soul"); !ok {
				return false, detail
			}
		}
		return true, ""
	}
}

// evalReviewCommentFieldRequired checks that post_review_comment rejects a line
// comment missing a required field, reporting whether it reached the GitHub
// stub.
func evalReviewCommentFieldRequired(field string) func(*testing.T) (bool, string) {
	return func(t *testing.T) (bool, string) {
		task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
		stub := newEvalHTTPStub(t)
		t.Setenv(envOrkaTaskName, testCoderTaskName)
		tool := &PostReviewCommentTool{k8sClient: newFakeClient(task, secret), apiBaseURL: stub.URL}
		review := func(drop string) string {
			comment := map[string]any{"path": "main.go", "line": 1, "body": "nit"}
			delete(comment, drop)
			raw, _ := json.Marshal(map[string]any{"task_name": testCoderTaskName, "pr_number": 1, "body": "x", "event": "COMMENT", "comments": []any{comment}})
			return string(raw)
		}
		call := func() (Tool, context.Context) { return tool, context.Background() }
		ok, detail := evalRejectsChange(call, review(""), review(field), "comments")
		if !ok && stub.sent(`"comments":[`) {
			detail = "sent a line comment without " + field + " to GitHub"
		}
		return ok, detail
	}
}

// evalReviewCommentText checks that post_review_comment rejects a line comment
// whose path or body is the given text.
func evalReviewCommentText(t *testing.T, field, text string) (bool, string) {
	task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
	stub := newEvalHTTPStub(t)
	t.Setenv(envOrkaTaskName, testCoderTaskName)
	tool := &PostReviewCommentTool{k8sClient: newFakeClient(task, secret), apiBaseURL: stub.URL}
	review := func(value string) string {
		comment := map[string]any{"path": "main.go", "line": 1, "body": "nit"}
		comment[field] = value
		raw, _ := json.Marshal(map[string]any{"task_name": testCoderTaskName, "pr_number": 1, "body": "x", "event": "COMMENT", "comments": []any{comment}})
		return string(raw)
	}
	call := func() (Tool, context.Context) { return tool, context.Background() }
	return evalRejectsChange(call, review(map[string]string{"path": "main.go", "body": "nit"}[field]), review(text), field)
}

// evalReviewCommentLine checks that post_review_comment rejects a line comment
// with an out-of-range line, reporting whether it reached the GitHub stub.
func evalReviewCommentLine(line int) func(*testing.T) (bool, string) {
	return func(t *testing.T) (bool, string) {
		task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
		stub := newEvalHTTPStub(t)
		t.Setenv(envOrkaTaskName, testCoderTaskName)
		tool := &PostReviewCommentTool{k8sClient: newFakeClient(task, secret), apiBaseURL: stub.URL}
		review := func(line int) string {
			raw, _ := json.Marshal(map[string]any{"task_name": testCoderTaskName, "pr_number": 1, "body": "x", "event": "COMMENT", "comments": []any{map[string]any{"path": "main.go", "line": line, "body": "nit"}}})
			return string(raw)
		}
		call := func() (Tool, context.Context) { return tool, context.Background() }
		ok, detail := evalRejectsChange(call, review(1), review(line), "line")
		if !ok && stub.sent(`"comments":[`) {
			detail = fmt.Sprintf("sent a line comment with line %d to GitHub", line)
		}
		return ok, detail
	}
}

// evalProgressEnforced checks that update_plan rejects an out-of-range
// progress_pct, reporting whether the value reached the controller stub.
func evalProgressEnforced(valid, progress int) func(*testing.T) (bool, string) {
	return func(t *testing.T) (bool, string) {
		controller := newEvalHTTPStub(t)
		t.Setenv(envOrkaControllerURL, controller.URL)
		t.Setenv(envOrkaTaskName, "task")
		t.Setenv(workerenv.ServiceAccountToken, "token")
		tool, _ := evalToolRegistries(newFakeClient())["worker"].Get("update_plan")
		call := func() (Tool, context.Context) { return tool, context.Background() }
		plan := func(v int) string { return fmt.Sprintf(`{"summary":"s","plan_document":"p","progress_pct":%d}`, v) }
		ok, detail := evalRejectsChange(call, plan(valid), plan(progress), "progress")
		if !ok && controller.sent(fmt.Sprintf(`"progress_pct":%d`, progress)) {
			detail = fmt.Sprintf("sent progress_pct=%d to the controller", progress)
		}
		return ok, detail
	}
}

// evalArgumentControl supplies executable controls, not just schema-typed
// placeholders. Each call gets fresh resources so an earlier create/delete
// cannot invalidate the next control. HTTP, stores, and code execution stay local.
func evalArgumentControl(t *testing.T, label, name string, schema map[string]any) (evalCall, map[string]any) {
	t.Helper()
	required, _ := schema["required"].([]any)
	properties, _ := schema["properties"].(map[string]any)
	args := evalToolRequiredArgs(required, properties)
	stub := newEvalHTTPStub(t)
	t.Setenv(envOrkaTaskName, "parent")
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv(envOrkaControllerURL, stub.URL)
	t.Setenv(workerenv.ServiceAccountToken, "eval-token")
	maps.Copy(args, evalArgumentSemanticValues(label, name, stub.URL))
	forgeTools := []string{"create_pull_request", "check_pull_request_ci", "merge_pull_request", "auto_merge_pull_request", "review_pull_request", "post_review_comment", "check_pr_review_marker", "list_issues", "list_pull_requests", "get_issue", "comment_on_issue"}
	if slices.Contains(forgeTools, name) {
		args["task_name"] = testCoderTaskName
	}
	call := func() (Tool, context.Context) {
		task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
		parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: defaultNamespace, UID: "eval-uid"}}
		child := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: evalPlaceholder, Namespace: defaultNamespace, UID: "eval-child",
				Labels:          map[string]string{labels.LabelParentTask: labels.SelectorValue(parent.Name)},
				Annotations:     map[string]string{labels.AnnotationParentTaskName: parent.Name, labels.AnnotationParentTaskUID: string(parent.UID)},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, corev1alpha1.GroupVersion.WithKind("Task"))}},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, ResultRef: &corev1alpha1.ResultReference{Available: true}},
		}
		agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: evalPlaceholder, Namespace: defaultNamespace},
			Spec: corev1alpha1.AgentSpec{ProviderRef: &corev1alpha1.ProviderReference{Name: evalPlaceholder}, Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}, Coordination: &corev1alpha1.CoordinationConfig{Enabled: true}}}
		objects := []client.Object{parent, child, task, secret, &corev1alpha1.Provider{ObjectMeta: agent.ObjectMeta}}
		if name != "create_tool" {
			objects = append(objects, &corev1alpha1.Tool{ObjectMeta: agent.ObjectMeta})
		}
		if name != "create_agent_task" && name != "delegate_task" {
			agent.Spec.Runtime = nil
		}
		if name != "create_agent" {
			objects = append(objects, agent)
		}
		if name == "create_pr_monitor" {
			agent.Spec.Runtime = nil
			read := secret.DeepCopy()
			read.Name = "eval-read"
			objects = append(objects, read)
		}
		fc := newFakeClient(objects...)
		tool, _ := evalToolRegistries(fc)[label].Get(name)
		ctx := evalToolContextFor(label, fc)
		if label == "worker" {
			// Worker contexts have no chat hooks; in particular container
			// creation must take its coordination path, not the chat path.
			ctx = WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace, ExecutionMode: executionmode.HarnessV2})
		}
		tc := GetToolContext(ctx)
		tc.TaskID, tc.TaskUID, tc.ParentTaskID, tc.OperationID = parent.Name, string(parent.UID), parent.Name, "eval-operation"
		tc.TaskProvenanceProtected = true
		tc.ResultStore = &fakeResultStore{data: map[string][]byte{defaultNamespace + "/eval": []byte("eval output")}}
		tc.SessionDeleter = &mockSessionDeleter{}
		tc.MessageStore = &recordingMessageStore{}
		memory := &brokerMemoryStore{}
		tc.MemoryReader, tc.MemoryProposalWriter, tc.TranscriptSearcher = memory, memory, memory
		tc.GatewayReplySender = &meetingReplySender{replySenderStub: replySenderStub{budget: GatewayReplyBudget{Limit: 10}}}
		tc.ApprovalEmitter = func(context.Context, approvals.ApprovalTarget) error { return nil }
		tc.Requester = &corev1alpha1.RequestedBy{Issuer: "https://example.com", Subject: "eval-person"}
		switch tool := tool.(type) {
		case *CreatePullRequestTool:
			tool.apiBaseURL = stub.URL
		case *CheckPullRequestCITool:
			tool.apiBaseURL = stub.URL
		case *MergePullRequestTool:
			tool.apiBaseURL = stub.URL
		case *AutoMergePullRequestTool:
			tool.apiBaseURL = stub.URL
		case *ReviewPullRequestTool:
			tool.apiBaseURL = stub.URL
		case *PostReviewCommentTool:
			tool.apiBaseURL = stub.URL
		case *CheckPRReviewMarkerTool:
			tool.apiBaseURL = stub.URL
		case *ListIssuesTool:
			tool.apiBaseURL = stub.URL
		case *ListPullRequestsTool:
			tool.apiBaseURL = stub.URL
		case *GetIssueTool:
			tool.apiBaseURL = stub.URL
		case *CommentOnIssueTool:
			tool.apiBaseURL = stub.URL
		case *FileReadTool:
			tool.allowedPaths = []string{os.Getenv(workerenv.WorkDir)}
			if err := os.WriteFile(args["path"].(string), []byte("eval"), 0600); err != nil {
				t.Fatal(err)
			}
		case *FileWriteTool:
			tool.allowedPaths = []string{os.Getenv(workerenv.WorkDir)}
		case *WebFetchTool:
			tool.client = evalWebClient()
		case *WebSearchTool:
			tool.client = evalWebClient()
		case *RunValidationTool:
			monitor, review := runValidationFixtures()
			fc := newFakeClient(monitor, review)
			bindings := newRunValidationBindingStore()
			tc.Client, tc.Namespace, tc.TaskID, tc.TaskUID = fc, review.Namespace, review.Name, string(review.UID)
			tc.RepositoryValidationBindings = bindings
			seedRepositoryValidationReviewBindingForTest(t, ctx, bindings, review, monitor)
			tool.k8sClient = fc
		}
		if name == "code_exec" {
			tool, _ = evalRecordingCodeExec(t)
		}
		return tool, ctx
	}
	return call, args
}

// evalArgumentSemanticValues supplies valid values that schema types alone
// cannot describe, including local paths and tool-specific enum choices.
func evalArgumentSemanticValues(label, name, localURL string) map[string]any {
	switch name {
	case "reply_in_conversation":
		return map[string]any{"meetingDraft": evalMeetingDraftArgs()}
	case "create_pr_monitor":
		return map[string]any{"repo_url": testOrgTestRepoURL, "schedule": "0 * * * *", "readCredentialRef": "eval-read"}
	case "create_agent":
		if label == "worker" {
			return map[string]any{"systemPrompt": "Eval agent"}
		}
	case "code_exec":
		return map[string]any{"language": "python", "code": "print('eval')"}
	case "file_read", "file_write":
		return map[string]any{"path": filepath.Join(os.Getenv(workerenv.WorkDir), "eval")}
	case "wait_for_tasks":
		return map[string]any{"tasks": []any{evalPlaceholder}}
	case "post_review_comment":
		return map[string]any{"event": "COMMENT"}
	case "create_tool":
		return map[string]any{"url": localURL}
	case "web_fetch":
		return map[string]any{"url": "https://example.com/eval"}
	}
	return nil
}

func evalWebClient() *http.Client {
	return &http.Client{Transport: webSearchRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(`<html><body><div class="result"><a class="result__a" href="https://example.com/eval">Eval page</a><a class="result__snippet" href="#">Eval result</a></div></body></html>`)), Request: r}, nil
	})}
}

func TestToolEvalRequiredFields(t *testing.T) {
	evalToolSandbox(t)
	knownDefects := map[string]string{
		"worker/file_write: missing content":      "the tool treats omitted content as an empty file",
		"worker/request_approval: missing action": "the tool emits an approval without the required action",
	}
	violations := map[string]string{}
	declared, executed := map[string]bool{}, map[string]bool{}
	for label, registry := range evalToolRegistries(newFakeClient()) {
		for _, name := range registry.Names() {
			tool, _ := registry.Get(name)
			schema := evalToolSchema(t, tool)
			required, _ := schema["required"].([]any)
			for _, field := range required {
				declared[fmt.Sprintf("%s/%s: missing %s", label, name, field)] = true
			}
			if len(required) == 0 {
				continue
			}
			t.Run(label+"/"+name, func(t *testing.T) {
				call, args := evalArgumentControl(t, label, name, schema)
				valid, _ := json.Marshal(args)
				if ok, detail := evalSucceeds(call, string(valid)); !ok {
					t.Fatalf("required-field control is not executable: %s", detail)
				}
				for _, field := range required {
					key := fmt.Sprintf("%s/%s: missing %s", label, name, field)
					executed[key] = true
					missing := maps.Clone(args)
					delete(missing, field.(string))
					raw, _ := json.Marshal(missing)
					fresh, ctx := call()
					fieldName := field.(string)
					if fieldName == "tasks" {
						fieldName = "task name"
					}
					failed, message := evalArgumentFailure(t, label, ctx, fresh, string(raw))
					if !failed {
						violations[key] = "accepted " + string(raw)
					} else if !evalFieldMentioned(message, fieldName) {
						t.Errorf("missing %s rejected for another reason: %s", field, message)
					}
				}
			})
		}
	}
	evalToolReportExecutedViolations(t, violations, knownDefects, declared, executed)
}

// evalNestedTypeProbes visits properties, array items, and typed map entries.
// Each probe includes a valid enclosing object/item; sending only the leaf
// would test missing siblings instead of that leaf's declared type.
func evalNestedTypeProbes(schema map[string]any, visit func([]string, map[string]any)) {
	var walk func([]string, map[string]any)
	walk = func(path []string, node map[string]any) {
		if len(path) > 1 {
			visit(path, node)
		}
		properties, _ := node["properties"].(map[string]any)
		for name, raw := range properties {
			child, _ := raw.(map[string]any)
			walk(append(slices.Clone(path), name), child)
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(append(slices.Clone(path), "[]"), items)
		}
		if entries, ok := node["additionalProperties"].(map[string]any); ok {
			walk(append(slices.Clone(path), "*"), entries)
		}
	}
	walk(nil, schema)
}

func evalArgumentValue(path []string, schema map[string]any) any {
	key := strings.Join(path, ".")
	switch key {
	case "meetingDraft":
		return evalMeetingDraftArgs()
	case "meetingDraft.attendees":
		return map[string]any{"mode": "everyone"}
	case "meetingDraft.suggestedStart":
		return "2026-07-01T09:00:00Z"
	case "workspace":
		value := map[string]any{"gitRepo": testOrgTestRepoURL, "intent": "write", "publicationCredentialRef": "eval-write", "prBaseBranch": "main", "forgeCredentialRef": "eval-forge"}
		properties, _ := schema["properties"].(map[string]any)
		for field := range value {
			if _, ok := properties[field]; !ok {
				delete(value, field)
			}
		}
		return value
	case "coordination":
		return map[string]any{"enabled": true}
	case "workspace.intent":
		return "write" // The enclosing workspace includes publication credentials.
	case "workspace.gitRepo", "workspace.publicationGitRepo":
		return testOrgTestRepoURL
	case "model":
		return map[string]any{"name": "eval-model"}
	case "runtime":
		return map[string]any{"type": "codex"}
	case "runtime.type":
		return "codex"
	case "runtime.secretRef":
		return ""
	case "runtime.defaultAllowedTools.[]":
		return "Read"
	case "resources.requests", "resources.limits":
		return map[string]any{"cpu": "100m"}
	case "resources.requests.*", "resources.limits.*":
		return "100m"
	case "roles.[]":
		return "assistant"
	case "soul":
		// The ConfigMap form is the only valid soul with nested fields.
		return map[string]any{"configMapRef": map[string]any{"name": evalPlaceholder, "key": evalPlaceholder}, "digest": agentcontext.Digest(evalPlaceholder)}
	case "soul.digest":
		return agentcontext.Digest(evalPlaceholder)
	}
	if enum, _ := schema["enum"].([]any); len(enum) > 0 {
		return enum[0]
	}
	switch schema["type"] {
	case "object":
		required, _ := schema["required"].([]any)
		properties, _ := schema["properties"].(map[string]any)
		return evalToolRequiredArgs(required, properties)
	case "array":
		items, _ := schema["items"].(map[string]any)
		return []any{evalArgumentValue(append(slices.Clone(path), "[]"), items)}
	case "integer", "number":
		return 1
	case "boolean":
		return true
	default:
		return evalPlaceholder
	}
}

// evalSetArgumentPath copies the enclosing containers rather than mutating a
// shared control. "[]" uses the first item; "*" uses a valid resource-map key.
func evalSetArgumentPath(value any, schema map[string]any, path, prefix []string, leaf any) any {
	if len(path) == 0 {
		return leaf
	}
	key := path[0]
	var childSchema map[string]any
	if key == "[]" {
		childSchema, _ = schema["items"].(map[string]any)
		items, _ := value.([]any)
		items = slices.Clone(items)
		if len(items) == 0 {
			items = []any{evalArgumentValue(append(slices.Clone(prefix), key), childSchema)}
		}
		items[0] = evalSetArgumentPath(items[0], childSchema, path[1:], append(slices.Clone(prefix), key), leaf)
		return items
	}
	if key == "*" {
		childSchema, _ = schema["additionalProperties"].(map[string]any)
		key = "cpu"
	} else {
		properties, _ := schema["properties"].(map[string]any)
		childSchema, _ = properties[key].(map[string]any)
	}
	object, _ := value.(map[string]any)
	object = maps.Clone(object)
	if object == nil {
		object = map[string]any{}
	}
	child, ok := object[key]
	if !ok {
		child = evalArgumentValue(append(slices.Clone(prefix), path[0]), childSchema)
	}
	object[key] = evalSetArgumentPath(child, childSchema, path[1:], append(slices.Clone(prefix), path[0]), leaf)
	return object
}

// Explicit null is distinct from an omitted property for legacy map parsers.
// Probe required leaves inside valid parents, including array/map entries.
func TestToolEvalNullRequiredFields(t *testing.T) {
	evalToolSandbox(t)
	knownDefects := map[string]string{
		"worker/file_write: null content":      "the tool accepts or coerces explicit null instead of rejecting the required field",
		"worker/request_approval: null action": "the tool accepts or coerces explicit null instead of rejecting the required field",
	}
	violations := map[string]string{}
	declared, executed := map[string]bool{}, map[string]bool{}
	for label, registry := range evalToolRegistries(newFakeClient()) {
		for _, name := range registry.Names() {
			tool, _ := registry.Get(name)
			schema := evalToolSchema(t, tool)
			evalRequiredNullProbes(schema, nil, func(path []string, node map[string]any) {
				field := strings.NewReplacer(".[]", "[]", ".*", "{*}").Replace(strings.Join(path, "."))
				key := label + "/" + name + ": null " + field
				declared[key] = true
				t.Run(key, func(t *testing.T) {
					executed[key] = true
					call, args := evalArgumentControl(t, label, name, schema)
					if path[0] == "runtime" {
						args["model"] = map[string]any{"name": "eval-model"}
					}
					var valid any = maps.Clone(args)
					if len(path) > 1 {
						valid = evalSetArgumentPath(args, schema, path, nil, evalArgumentValue(path, node))
					}
					validRaw, _ := json.Marshal(valid)
					if ok, detail := evalSucceeds(call, string(validRaw)); !ok {
						t.Fatalf("null-required-field control is not executable: %s", detail)
					}
					invalid := evalSetArgumentPath(valid, schema, path, nil, nil)
					invalidRaw, _ := json.Marshal(invalid)
					fresh, ctx := call()
					if name == "reply_in_conversation" && path[0] == "meetingDraft" {
						if ok, detail := evalRejectsMeetingDraft(ctx, fresh, string(invalidRaw)); !ok {
							violations[key] = detail
						}
						return
					}
					failed, message := evalArgumentFailure(t, label, ctx, fresh, string(invalidRaw))
					fieldName := path[len(path)-1]
					if fieldName == "tasks" {
						fieldName = "task name"
					}
					if !failed {
						violations[key] = "accepted " + string(invalidRaw)
					} else if !evalFieldMentioned(message, fieldName) && !strings.Contains(message, "json: cannot unmarshal") {
						violations[key] = "null rejected for another reason: " + message
					}
				})
			})
		}
	}
	evalToolReportExecutedViolations(t, violations, knownDefects, declared, executed)
}

func evalRequiredNullProbes(node map[string]any, path []string, visit func([]string, map[string]any)) {
	properties, _ := node["properties"].(map[string]any)
	required, _ := node["required"].([]any)
	for _, raw := range required {
		name := raw.(string)
		child, _ := properties[name].(map[string]any)
		visit(append(slices.Clone(path), name), child)
	}
	for name, raw := range properties {
		child, _ := raw.(map[string]any)
		evalRequiredNullProbes(child, append(slices.Clone(path), name), visit)
	}
	for _, entry := range []struct{ key, segment string }{{"items", "[]"}, {"additionalProperties", "*"}} {
		if child, ok := node[entry.key].(map[string]any); ok {
			evalRequiredNullProbes(child, append(slices.Clone(path), entry.segment), visit)
		}
	}
}

// Top-level wrong types need executable siblings: a missing required field
// must not mask acceptance of the mutated property. JSON-encoded objects are
// checked separately by evalObjectStringHandling, which permits equivalent
// decoding as well as rejection. Unparseable numeric/boolean strings and
// object/array mismatches avoid intentional string and comma-list coercions.
func TestToolEvalTopLevelArgumentTypes(t *testing.T) {
	evalToolSandbox(t)
	wrong := map[string]any{"string": map[string]any{"k": "v"}, "integer": "many", "number": "many", "boolean": "yes", "array": map[string]any{"k": "v"}, "object": []any{}}
	knownDefects := map[string]string{}
	violations := map[string]string{}
	declared, executed := map[string]bool{}, map[string]bool{}
	for label, registry := range evalToolRegistries(newFakeClient()) {
		for _, name := range registry.Names() {
			tool, _ := registry.Get(name)
			schema := evalToolSchema(t, tool)
			properties, _ := schema["properties"].(map[string]any)
			for field, raw := range properties {
				node, _ := raw.(map[string]any)
				typ, _ := node["type"].(string)
				bad, ok := wrong[typ]
				if !ok {
					continue
				}
				key := label + "/" + name + ": " + field + " as wrong type"
				declared[key] = true
				t.Run(key, func(t *testing.T) {
					executed[key] = true
					call, args := evalArgumentControl(t, label, name, schema)
					valid, _ := json.Marshal(args)
					if ok, detail := evalSucceeds(call, string(valid)); !ok {
						t.Fatalf("top-level-type control is not executable: %s", detail)
					}
					invalid := maps.Clone(args)
					invalid[field] = bad
					invalidRaw, _ := json.Marshal(invalid)
					fresh, ctx := call()
					failed, message := evalArgumentFailure(t, label, ctx, fresh, string(invalidRaw))
					if !failed {
						violations[key] = "accepted " + string(invalidRaw)
					} else if !evalFieldMentioned(message, field) && !strings.Contains(message, "json: cannot unmarshal") {
						violations[key] = "wrong type rejected for another reason: " + message
					}
				})
			}
		}
	}
	evalToolReportExecutedViolations(t, violations, knownDefects, declared, executed)
}

func TestToolEvalNestedArgumentTypes(t *testing.T) {
	evalToolSandbox(t)
	wrong := map[string]any{"string": map[string]any{"k": "v"}, "integer": "many", "number": "many", "boolean": "yes", "array": "a,b", "object": []any{}}
	knownDefects := map[string]string{}
	violations := map[string]string{}
	declared, executed := map[string]bool{}, map[string]bool{}
	for label, registry := range evalToolRegistries(newFakeClient()) {
		for _, name := range registry.Names() {
			tool, _ := registry.Get(name)
			schema := evalToolSchema(t, tool)
			evalNestedTypeProbes(schema, func(path []string, node map[string]any) {
				typ, _ := node["type"].(string)
				bad, ok := wrong[typ]
				if !ok {
					return
				}
				field := strings.NewReplacer(".[]", "[]", ".*", "{*}").Replace(strings.Join(path, "."))
				key := label + "/" + name + ": " + field + " as wrong type"
				declared[key] = true
				t.Run(key, func(t *testing.T) {
					executed[key] = true
					call, args := evalArgumentControl(t, label, name, schema)
					if path[0] == "runtime" {
						args["model"] = map[string]any{"name": "eval-model"}
					}
					if strings.Join(path, ".") == "soul.inline" {
						// An inline soul excludes the default ConfigMap-backed soul.
						args["soul"] = map[string]any{}
					}
					valid := evalSetArgumentPath(args, schema, path, nil, evalArgumentValue(path, node))
					validRaw, _ := json.Marshal(valid)
					if ok, detail := evalSucceeds(call, string(validRaw)); !ok {
						t.Fatalf("nested-type control is not executable: %s", detail)
					}
					invalid := evalSetArgumentPath(valid, schema, path, nil, bad)
					invalidRaw, _ := json.Marshal(invalid)
					fresh, ctx := call()
					failed, message := evalArgumentFailure(t, label, ctx, fresh, string(invalidRaw))
					if !failed {
						violations[key] = "accepted " + string(invalidRaw)
					} else if !evalFieldMentioned(message, path[0]) && !strings.Contains(message, "json: cannot unmarshal") {
						// Custom array decoding can name only its item index, not
						// its parent. Its type error still proves this one mutation.
						t.Errorf("wrong type rejected for another reason: %s", message)
					}
				})
			})
		}
	}
	evalToolReportExecutedViolations(t, violations, knownDefects, declared, executed)
}

// evalArgumentFailure keeps panic, chat Go-error, and diagnostic checks outside
// knownDefect accounting; a different failure must never satisfy a marker.
func evalArgumentFailure(t *testing.T, label string, ctx context.Context, tool Tool, args string) (bool, string) {
	t.Helper()
	result, panicked, err := evalToolExecute(ctx, tool, args)
	if panicked != nil {
		t.Fatalf("tool panicked: %v", panicked)
	}
	if label == "chat" && err != nil && !errors.As(err, new(*ToolArgumentError)) {
		t.Fatalf("chat tool returned a Go error instead of a structured result: %v", err)
	}
	failed, message := evalToolFailure(result, err)
	if failed && strings.TrimSpace(message) == "" {
		t.Fatalf("tool failed without an error message: %s", result)
	}
	return failed, message
}

func evalFieldMentioned(message, field string) bool {
	normalize := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "") }
	return strings.Contains(normalize(message), normalize(field))
}

// Filtered subtest runs cannot prove that a skipped defect was fixed. Schema
// removals are still checked against the complete inventory, even in a focused
// run, so obsolete markers do not silently survive changes to the contract.
func evalToolReportExecutedViolations(t *testing.T, violations, knownDefects map[string]string, declared, executed map[string]bool) {
	t.Helper()
	selected := map[string]string{}
	for key, defect := range knownDefects {
		if !declared[key] {
			t.Errorf("schema no longer declares probe; remove knownDefect %q: %s", key, defect)
		} else if executed[key] {
			selected[key] = defect
		}
	}
	evalToolReportViolations(t, violations, selected)
}

// evalTaskListBound keeps the declared list_tasks maximum paired with a valid control.
func evalTaskListBound(t *testing.T) (bool, string) {
	objects := make([]client.Object, 0, maxListTasksLimit+1)
	for i := 0; i <= maxListTasksLimit; i++ {
		objects = append(objects, &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("bounded-task-%03d", i), Namespace: defaultNamespace,
		}})
	}
	fc := newFakeClient(objects...)
	call := func() (Tool, context.Context) {
		tool, _ := evalToolRegistries(fc)["chat"].Get("list_tasks")
		return tool, evalToolContext(fc)
	}
	args := func(limit int) string { return fmt.Sprintf(`{"limit":%d}`, limit) }
	if ok, detail := evalSucceeds(call, args(maxListTasksLimit)); !ok {
		return false, detail
	}
	tool, ctx := call()
	result, panicked, err := evalToolExecute(ctx, tool, args(maxListTasksLimit+1))
	if panicked != nil {
		return false, fmt.Sprintf("panicked: %v", panicked)
	}
	if failed, message := evalToolFailure(result, err); failed {
		return evalFieldMentioned(message, "limit"), message
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		return false, err.Error()
	}
	items, ok := response.Data.([]any)
	return ok && len(items) == maxListTasksLimit, "list_tasks did not clamp to its declared maximum"
}
