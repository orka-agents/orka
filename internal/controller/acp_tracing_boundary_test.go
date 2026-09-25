package controller_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/api"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/workers/acp/supervisor"
)

// TestACPTraceBoundary proves the actual creation -> dispatcher -> HTTP ->
// supervisor -> OTLP boundary, including two simultaneously executing children.
// Kubernetes storage is in-process; no API-server behavior is changed or claimed.
func TestACPTraceBoundary(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root fixture lane for production child UID/group fences")
	}
	for key, value := range map[string]string{
		"ORKA_ENABLE_TELEMETRY": "true", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/protobuf",
		"ORKA_TRACEPARENT": "PRIVATE_ENV_CANARY", "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "true",
		"ORKA_TASK_NAME": "parent", "ORKA_TASK_NAMESPACE": "default", "ORKA_COORDINATION_DEPTH": "0",
		"ORKA_COORDINATION_ALLOWED_AGENTS": "agent", "ORKA_COORDINATION_MAX_DEPTH": "3",
	} {
		t.Setenv(key, value)
	}
	var mu sync.Mutex
	var spans []*tracepb.Span
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("collector received a credential-bearing inherited header")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request collectortrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, rs := range request.ResourceSpans {
			for _, a := range rs.Resource.Attributes {
				if strings.Contains(a.Value.GetStringValue(), "PRIVATE_") {
					t.Error("private resource attribute was exported")
				}
			}
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", collector.URL+"/v1/traces")
	exporter, err := otlptracehttp.New(context.Background(), otlptracehttp.WithHeaders(nil))
	if err != nil {
		t.Fatal(err)
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithResource(resource.Empty()))
	oldProvider, oldPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer func() {
		otel.SetTracerProvider(oldProvider)
		otel.SetTextMapPropagator(oldPropagator)
		_ = provider.Shutdown(context.Background())
	}()
	tracer, shutdown, err := supervisor.NewTelemetryFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	// Install inherited canaries after initialization: both exporters have frozen
	// configuration, while real provider children must still exclude these values.
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=PRIVATE_HEADER_CANARY")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "private=PRIVATE_RESOURCE_CANARY")

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1}, Spec: corev1alpha1.AgentSpec{
		Model: &corev1alpha1.ModelConfig{Name: "test-model"}, Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)},
	}}
	agent.Spec.Runtime.DefaultAllowedTools = append(acp.BuiltInRuntimeNativeToolNames("codex"), "send_message", "check_messages")
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI}}
	kc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, parent).Build()
	apiCtx, apiRoot := otel.Tracer("test").Start(context.Background(), "api.creator")
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error { c.SetContext(apiCtx); return c.Next() })
	app.Post("/tasks", api.NewHandlers(api.HandlersConfig{Client: kc, WatchNamespace: "default"}).CreateTask)
	request := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(`{"name":"api-task","namespace":"default","spec":{"type":"agent","prompt":"PRIVATE_PROMPT_CANARY","agentRef":{"name":"agent"},"agentRuntime":{}}}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("API creation status = %d", response.StatusCode)
	}
	_ = response.Body.Close()
	apiRoot.End()
	apiTask := &corev1alpha1.Task{}
	if err := kc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "api-task"}, apiTask); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	if err := tools.RegisterBrokeredCoordinationTools(registry, kc); err != nil {
		t.Fatal(err)
	}
	oldRegistry := tools.DefaultRegistry
	tools.DefaultRegistry = registry
	defer func() { tools.DefaultRegistry = oldRegistry }()
	delegateCtx, delegateRoot := otel.Tracer("test").Start(context.Background(), "delegation.run")
	delegateCtx = tools.WithToolContext(delegateCtx, &tools.ToolContext{TaskID: "parent", Namespace: "default", Tenant: "default"})
	result, err := registry.Execute(delegateCtx, "delegate_task", json.RawMessage(`{"agent":"agent","prompt":"PRIVATE_PROMPT_CANARY"}`))
	if err != nil {
		t.Fatal(err)
	}
	delegateRoot.End()
	var delegated tools.DelegateTaskResult
	if err := json.Unmarshal([]byte(result), &delegated); err != nil {
		t.Fatal(err)
	}
	child := &corev1alpha1.Task{}
	if err := kc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: delegated.TaskName}, child); err != nil {
		t.Fatal(err)
	}

	var barrierMu sync.Mutex
	arrivals := 0
	release := make(chan struct{})
	barrier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		barrierMu.Lock()
		arrivals++
		if arrivals == 2 {
			close(release)
		}
		barrierMu.Unlock()
		select {
		case <-release:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		case <-time.After(15 * time.Second):
			w.WriteHeader(http.StatusGatewayTimeout)
		}
	}))
	defer barrier.Close()
	tasks := []*corev1alpha1.Task{apiTask, child}
	controller.RunACPTraceBoundary(t, tasks, agent, tracer, barrier.URL)
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	barrierMu.Lock()
	gotArrivals := arrivals
	barrierMu.Unlock()
	if gotArrivals != 2 {
		t.Fatalf("concurrent child arrivals = %d", gotArrivals)
	}
	mu.Lock()
	defer mu.Unlock()
	checkACPTraceTrees(t, spans, tasks)
}

func checkACPTraceTrees(t *testing.T, spans []*tracepb.Span, tasks []*corev1alpha1.Task) {
	t.Helper()
	byID := map[string]*tracepb.Span{}
	for _, s := range spans {
		byID[hex.EncodeToString(s.SpanId)] = s
	}
	counts := map[string]int{}
	for _, s := range spans {
		for _, a := range s.Attributes {
			if strings.Contains(a.Value.GetStringValue(), "PRIVATE_") {
				t.Error("span leaked private content")
			}
		}
		if bytes.Contains([]byte(s.String()), []byte("PRIVATE_")) {
			t.Error("serialized span leaked private content")
		}
		if !strings.HasPrefix(s.Name, "acp.supervisor.") {
			continue
		}
		attrs := map[string]string{}
		allowed := map[string]bool{"orka.task.uid": true, "orka.acp.task.attempt": true, "orka.acp.prompt.id": true, "orka.acp.operation.id": true, "orka.acp.runtime_pool.uid": true, "orka.acp.runtime_session.uid": true, "orka.acp.runtime_session.generation": true}
		for _, a := range s.Attributes {
			if !allowed[a.Key] {
				t.Errorf("unexpected supervisor attribute %s", a.Key)
			}
			attrs[a.Key] = a.Value.GetStringValue()
			if a.Key == "orka.acp.task.attempt" {
				attrs[a.Key] = fmt.Sprint(a.Value.GetIntValue())
			}
		}
		if s.Name != "acp.supervisor.prompt" && s.Name != "acp.supervisor.session.create" {
			continue
		}
		p := byID[hex.EncodeToString(s.ParentSpanId)]
		wantParent := "acp.prompt"
		if s.Name == "acp.supervisor.session.create" {
			wantParent = "acp.session.create"
		}
		if p == nil || p.Name != wantParent || !bytes.Equal(p.TraceId, s.TraceId) {
			t.Fatalf("wrong parent for %s", s.Name)
		}
		uid := attrs["orka.task.uid"]
		index := 0
		if uid == "trace-task-1" {
			index = 1
		} else if uid != "trace-task-0" {
			t.Fatalf("unexpected task identity %s", uid)
		}
		if attrs["orka.acp.task.attempt"] != fmt.Sprint(index+1) || attrs["orka.acp.operation.id"] == "" || attrs["orka.acp.runtime_session.uid"] == "" {
			t.Fatalf("operation/attempt identity missing for %s", uid)
		}
		if s.Name == "acp.supervisor.prompt" && attrs["orka.acp.prompt.id"] != tasks[index].Status.Execution.PromptID {
			t.Fatal("prompt crossed Task attempts")
		}
		creator := byID[hex.EncodeToString(p.ParentSpanId)]
		wantCreator := "api.creator"
		if index == 1 {
			wantCreator = "execute_tool delegate_task"
		}
		if creator == nil || creator.Name != wantCreator || !bytes.Equal(creator.TraceId, s.TraceId) {
			t.Fatalf("wrong creator parent for %s", uid)
		}
		counts[uid+"/"+s.Name]++
		t.Logf("span tree: %s -> %s -> %s task=%s attempt=%s trace=%s", creator.Name, p.Name, s.Name, uid, attrs["orka.acp.task.attempt"], hex.EncodeToString(s.TraceId))
	}
	for _, uid := range []string{"trace-task-0", "trace-task-1"} {
		for _, name := range []string{"acp.supervisor.prompt", "acp.supervisor.session.create"} {
			if counts[uid+"/"+name] != 1 {
				t.Fatalf("span count for %s/%s = %d", uid, name, counts[uid+"/"+name])
			}
		}
	}
}

// This real ACP subprocess checks its own environment before reading any prompt.
// The production exec helper, session UID/GID fences and stream client all run.
func TestACPTraceProviderFixture(t *testing.T) {
	if os.Getenv("ORKA_TRACE_FIXTURE") != "true" {
		return
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") || name == "ORKA_ENABLE_TELEMETRY" || name == "ORKA_TRACEPARENT" || strings.Contains(entry, "PRIVATE_") {
			os.Exit(5)
		}
	}
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	encoder := json.NewEncoder(os.Stdout)
	for {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := decoder.Decode(&msg); err != nil {
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"mcpCapabilities": map[string]bool{"http": true}}}
		case "session/new":
			result = map[string]string{"sessionId": "fixture-session"}
		case "session/prompt":
			barrierClient := http.Client{Timeout: 20 * time.Second}
			response, err := barrierClient.Get(os.Getenv("ORKA_TRACE_FIXTURE_BARRIER"))
			if err != nil {
				os.Exit(6)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				os.Exit(7)
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "fixture-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "PRIVATE_COMPLETION_CANARY"}}}})
			result = map[string]string{"stopReason": "end_turn"}
		default:
			continue
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}); err != nil {
			return
		}
	}
}
