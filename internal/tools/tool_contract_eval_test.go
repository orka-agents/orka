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

// evalToolContextFor gives brokered tools the authenticated context the ACP
// broker builds and other tools the chat context, so each takes its real path.
func evalToolContextFor(label string, fc client.Client) context.Context {
	if label == "brokered" {
		return WithToolContext(context.Background(), &ToolContext{Client: fc, Brokered: true, Namespace: defaultNamespace, TaskID: evalPlaceholder, TaskUID: "eval-uid"})
	}
	return evalToolContext(fc)
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

// evalToolExecute runs a tool, converting a panic into a reported failure.
func evalToolExecute(ctx context.Context, tool Tool, args string) (result string, panicked any, err error) {
	defer func() {
		if panicked = recover(); panicked != nil {
			evalPanics.Add(1)
		}
	}()
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
// non-object JSON, missing required fields (all at once and each on its own),
// wrong JSON types, and nested
// objects encoded as strings. A tool must never panic, must never report
// success for a call missing required fields or for an object sent as a
// string, and must always explain a failure. Chat tools must return a
// structured result instead of a Go error, which the chat executor reports to
// the model as an unknown tool.
func TestToolEvalMalformedArguments(t *testing.T) {
	evalToolSandbox(t)
	const (
		ignoredName        = "the create_*_task schemas require name, but the tools ignore it and always generate one"
		droppedWorkspace   = "a workspace that is not an object is ignored, so the Task runs without the repository"
		modelStringAsName  = "a JSON-encoded model object is stored as the model name"
		droppedCoordinator = "a coordination value that is not an object is ignored, so the Agent is created without coordination"
		droppedRuntime     = "a runtime value that is not an object is ignored, so a runtime Agent is created as a plain AI Agent"
	)
	knownDefects := map[string]string{
		"chat/create_agent: coordination as JSON string":         droppedCoordinator,
		"chat/create_agent: model as JSON string":                modelStringAsName,
		"chat/create_agent: runtime as JSON string":              droppedRuntime,
		"chat/update_agent: model as JSON string":                modelStringAsName,
		"chat/create_container_task: empty object":               ignoredName,
		"chat/create_container_task: null":                       ignoredName,
		"chat/create_container_task: workspace as JSON string":   droppedWorkspace,
		"chat/create_agent_task: missing name":                   ignoredName,
		"chat/create_ai_task: missing name":                      ignoredName,
		"worker/create_container_task: empty object":             ignoredName,
		"worker/create_container_task: null":                     ignoredName,
		"worker/create_container_task: workspace as JSON string": droppedWorkspace,
	}
	wrongValue := map[string]string{
		"string": `{"k":"v"}`, "integer": `"many"`, "number": `"many"`, "boolean": `"yes"`, "array": `"a,b"`, "object": `"{\"k\":\"v\"}"`,
	}
	violations := map[string]string{}
	// Panics, Go errors from chat tools, and failures without a message are
	// never accepted as known defects.
	var fatal, inconclusive, untested []string
	for label, registry := range evalToolRegistries(newFakeClient()) {
		names := registry.Names()
		sort.Strings(names)
		for _, name := range names {
			tool, _ := registry.Get(name)
			schema := evalToolSchema(t, tool)
			required, _ := schema["required"].([]any)
			properties, _ := schema["properties"].(map[string]any)

			inputs := map[string]string{"null": `null`, "array": `[]`, "string": `"text"`, "number": `42`, "empty object": `{}`}
			// Omit each required field on its own, so a field the tool checks
			// cannot hide one it ignores. The call with every required field is
			// the baseline: where it fails too, a failure proves nothing.
			if len(required) > 1 {
				baseline, _ := json.Marshal(evalToolRequiredArgs(required, properties))
				inputs["all required"] = string(baseline)
				for i, field := range required {
					args, _ := json.Marshal(evalToolRequiredArgs(slices.Delete(slices.Clone(required), i, i+1), properties))
					inputs[fmt.Sprintf("missing %v", field)] = string(args)
				}
			}
			for prop, raw := range properties {
				propSchema, _ := raw.(map[string]any)
				typ, _ := propSchema["type"].(string)
				if value, ok := wrongValue[typ]; ok {
					inputs[fmt.Sprintf("%s as wrong type", prop)] = fmt.Sprintf(`{%q:%s}`, prop, value)
				}
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

			baselineFailed := map[string]bool{}
			for _, input := range inputNames {
				for _, cluster := range []string{"empty", "seeded"} {
					fc := evalToolClusters[cluster]()
					fresh, _ := evalToolRegistries(fc)[label].Get(name)
					result, panicked, err := evalToolExecute(evalToolContextFor(label, fc), fresh, inputs[input])
					key := fmt.Sprintf("%s/%s: %s", label, name, input)
					failed, message := evalToolFailure(result, err)
					switch {
					case panicked != nil:
						fatal = append(fatal, fmt.Sprintf("%s: panicked: %v", key, panicked))
					case label == "chat" && err != nil:
						fatal = append(fatal, fmt.Sprintf("%s: returned Go error %q, which the chat executor reports as an unknown tool", key, err))
					case failed && strings.TrimSpace(message) == "":
						fatal = append(fatal, fmt.Sprintf("%s: failed without an error message: %q", key, result))
					case (input == "empty object" || input == "null") && len(required) > 0 && !failed:
						violations[key] = fmt.Sprintf("reported success without required fields %v in the %s cluster", required, cluster)
					case input == "all required" && failed:
						baselineFailed[cluster] = true
					case strings.HasPrefix(input, "missing ") && !failed:
						violations[key] = fmt.Sprintf("reported success without required field %s in the %s cluster", strings.TrimPrefix(input, "missing "), cluster)
					case strings.HasPrefix(input, "missing ") && baselineFailed[cluster]:
						untested = append(untested, key+" ("+cluster+")")
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
	t.Logf("missing required fields not tested because the call with every required field also failed: %v", untested)
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
			tool, _ := evalToolRegistries(fc)[label].Get(name)
			result, panicked, err := evalToolExecute(evalToolContextFor(label, fc), tool, string(raw))
			if panicked != nil {
				return true, evalPanicked + fmt.Sprintf(": %v", panicked)
			}
			if label == "chat" && err != nil {
				return true, evalGoError + fmt.Sprintf(" %q, which the chat executor reports as an unknown tool", err)
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

// evalClusterSnapshot renders the Agents and Tasks a tool call left behind.
func evalClusterSnapshot(fc client.Client) string {
	var agents corev1alpha1.AgentList
	var tasks corev1alpha1.TaskList
	_ = fc.List(context.Background(), &agents)
	_ = fc.List(context.Background(), &tasks)
	parts := make([]string, 0, len(agents.Items)+len(tasks.Items))
	for i := range agents.Items {
		spec, _ := json.Marshal(agents.Items[i].Spec)
		parts = append(parts, "agent "+agents.Items[i].Name+" "+string(spec))
	}
	for i := range tasks.Items {
		spec, _ := json.Marshal(tasks.Items[i].Spec)
		parts = append(parts, "task "+tasks.Items[i].Name+" "+string(spec))
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

// evalToolLimits lists every limit (minimum, maximum, enum, length, pattern)
// that a tool schema declares with its value, keyed like
// "chat/create_agent_task.maxTurns maximum=100", so changing a schema value
// also requires updating the case that probes it. It also lists every required
// field of a nested object, keyed like
// "chat/create_agent.coordination.allowedAgents[].name required".
// TestToolEvalMalformedArguments covers top-level required fields.
func evalToolLimits(t *testing.T) []string {
	t.Helper()
	var limits []string
	var walk func(prefix string, schema map[string]any, nested bool)
	walk = func(prefix string, schema map[string]any, nested bool) {
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
				walk(prefix+"."+name, child, true)
			}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			walk(prefix+"[]", items, true)
		}
	}
	for label, registry := range evalToolRegistries(newFakeClient()) {
		for _, name := range registry.Names() {
			tool, _ := registry.Get(name)
			walk(label+"/"+name, evalToolSchema(t, tool), false)
		}
	}
	sort.Strings(limits)
	return limits
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
	case !strings.Contains(strings.ToLower(message), strings.ToLower(field)):
		return false, fmt.Sprintf("rejected %s for another reason: %s", args, message)
	}
	return true, ""
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
		return tool, evalToolContext(fc)
	}
	githubTask := func() client.Client {
		task, secret := githubRepoTaskWithSecret(testOrgTestRepoURL)
		return newFakeClient(task, secret)
	}

	cases := []evalLimitCase{
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
				call := func() (Tool, context.Context) { return chatTool("create_pr_monitor", agent()) }
				return evalAll(
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
				call := func() (Tool, context.Context) { return tool, context.Background() }
				timeout := func(v int) string { return fmt.Sprintf(`{"language":"bash","code":"echo test","timeout":%d}`, v) }
				ok, detail := evalRejectsChange(call, timeout(1), timeout(0), "timeout")
				if ok || strings.HasPrefix(detail, "valid call") {
					return ok, detail
				}
				if sandbox.calls > 0 && sandbox.req.Timeout == time.Second {
					return true, ""
				}
				return false, fmt.Sprintf("timeout 0 ran with %s", sandbox.req.Timeout)
			},
			knownDefect: "code_exec treats a timeout below 1 as unset and uses the 30s default instead of rejecting or clamping it",
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
				// One character past the advertised maxLength. The over-limit
				// max_chars stops the call before any request if the URL passes.
				tool, _ := evalToolRegistries(newFakeClient())["brokered"].Get("web_fetch")
				url := "https://1.1.1.1/" + long(brokeredWebFetchMaxURLBytes/utf8.UTFMax)
				return evalRejects(context.Background(), tool, `{"url":"`+url+`","max_chars":50001}`, "url")
			},
			knownDefect: "brokered web_fetch advertises url maxLength in characters but only enforces its 64 KiB byte limit",
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
			knownDefect: "worker create_agent stores maxTokens below 1, and the Agent CRD sets no minimum for it",
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
					_, _, _ = evalToolExecute(context.Background(), tool, args)
					sent := stub.perPage()
					switch {
					case len(sent) == 0:
						return false, fmt.Sprintf("%s made no list request for per_page=%d", tool.Name(), perPage)
					case slices.ContainsFunc(sent, func(v int) bool { return v < 1 || v > 100 }):
						return false, fmt.Sprintf("%s sent per_page=%v to GitHub for per_page=%d", tool.Name(), sent, perPage)
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
			name:        "worker merge_pull_request merge_method",
			limits:      []string{"worker/merge_pull_request.merge_method enum=merge|squash|rebase"},
			check:       evalMergeMethodEnforced(func(c client.Client, url string) Tool { return &MergePullRequestTool{k8sClient: c, apiBaseURL: url} }),
			knownDefect: "merge_pull_request forwards an unsupported merge_method to GitHub instead of rejecting it",
		},
		{
			name:   "worker auto_merge_pull_request merge_method",
			limits: []string{"worker/auto_merge_pull_request.merge_method enum=merge|squash|rebase"},
			check: evalMergeMethodEnforced(func(c client.Client, url string) Tool {
				return &AutoMergePullRequestTool{k8sClient: c, apiBaseURL: url}
			}),
			knownDefect: "auto_merge_pull_request forwards an unsupported merge_method to GitHub instead of rejecting it",
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
			name:   "worker reply_in_conversation content",
			limits: []string{"worker/reply_in_conversation.content maxLength=16384", "worker/reply_in_conversation.content minLength=1"},
			check: func(t *testing.T) (bool, string) {
				tool, ctx := workerTool("reply_in_conversation", newFakeClient())
				return evalAll(
					func() (bool, string) { return evalRejects(ctx, tool, `{"content":""}`, "content") },
					func() (bool, string) { return evalRejects(ctx, tool, `{"content":"`+long(16385)+`"}`, "content") },
				)
			},
		},
		{
			name:        "worker request_approval severity",
			limits:      []string{"worker/request_approval.severity enum=warning|critical"},
			knownDefect: "request_approval forwards an unsupported severity to the approval emitter instead of rejecting it",
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
			name:        "chat create_agent allowed agent name",
			limits:      []string{"chat/create_agent.coordination.allowedAgents[].name required"},
			check:       evalAllowedAgentNameRequired(chatTool, `"name":"a","systemPrompt":"p"`),
			knownDefect: "chat create_agent drops an allowed agent without a name and reports success",
		},
		{
			name:        "worker create_agent allowed agent name",
			limits:      []string{"worker/create_agent.coordination.allowedAgents[].name required"},
			check:       evalAllowedAgentNameRequired(workerTool, `"role":"coder","systemPrompt":"p"`),
			knownDefect: "worker create_agent stores an allowed agent with an empty name",
		},
		{
			name:        "worker post_review_comment comment path",
			limits:      []string{"worker/post_review_comment.comments[].path required"},
			check:       evalReviewCommentFieldRequired("path"),
			knownDefect: "post_review_comment forwards a line comment without path to GitHub instead of rejecting it",
		},
		{
			name:        "worker post_review_comment comment line",
			limits:      []string{"worker/post_review_comment.comments[].line required"},
			check:       evalReviewCommentFieldRequired("line"),
			knownDefect: "post_review_comment forwards a line comment without line to GitHub instead of rejecting it",
		},
		{
			name:        "worker post_review_comment comment body",
			limits:      []string{"worker/post_review_comment.comments[].body required"},
			check:       evalReviewCommentFieldRequired("body"),
			knownDefect: "post_review_comment forwards a line comment without body to GitHub instead of rejecting it",
		},
		{
			name:        "worker update_plan progress maximum",
			limits:      []string{"worker/update_plan.progress_pct maximum=100"},
			check:       evalProgressEnforced(100, 101),
			knownDefect: "update_plan sends a progress_pct above 100 to the controller instead of rejecting or clamping it",
		},
		{
			name:        "worker update_plan progress minimum",
			limits:      []string{"worker/update_plan.progress_pct minimum=0"},
			check:       evalProgressEnforced(0, -1),
			knownDefect: "update_plan sends a negative progress_pct to the controller instead of rejecting or clamping it",
		},
	}

	evalRunLimitCases(t, cases)
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
// fields, rejects an allowed agent without a name instead of dropping it or
// storing it unnamed. The worker tool resolves its parent Task from the
// environment.
func evalAllowedAgentNameRequired(get func(string, client.Client) (Tool, context.Context), required string) func(*testing.T) (bool, string) {
	return func(t *testing.T) (bool, string) {
		t.Setenv(envOrkaTaskName, "parent")
		call := func() (Tool, context.Context) { return get("create_agent", evalParentTaskCluster()) }
		allowed := func(agent string) string {
			return `{` + required + `,"coordination":{"enabled":true,"allowedAgents":[` + agent + `]}}`
		}
		return evalRejectsChange(call, allowed(`{"name":"coder","namespace":"default"}`), allowed(`{"namespace":"default"}`), "allowedAgents")
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
