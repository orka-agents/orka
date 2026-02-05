# Unit Test Coverage Plan

**Goal:** Achieve 100% test coverage across all packages (currently at 0.8%)

## Current Coverage Status

| Package | Coverage |
|---------|----------|
| `internal/controller` | 3.3% |
| `api/v1alpha1` | 0.0% |
| `cmd` | 0.0% |
| `internal/api` | 0.0% |
| `internal/llm` | 0.0% |
| `internal/llm/anthropic` | 0.0% |
| `internal/llm/openai` | 0.0% |
| `internal/metrics` | 0.0% |
| `internal/tools` | 0.0% |
| `internal/worker` | 0.0% |
| `workers/ai` | 0.0% |
| `workers/general` | 0.0% |
| **Total** | **0.8%** |

---

## Packages to Test

### 1. `internal/api/` (5 files, 0% coverage)

**Files to create:** `internal/api/pagination_test.go`, `internal/api/auth_test.go`, `internal/api/handlers_test.go`, `internal/api/middleware_test.go`, `internal/api/server_test.go`

| Function | Test Cases |
|----------|------------|
| `ParsePagination` | Valid limit, invalid limit, limit exceeds max, empty limit, negative limit |
| `NewAuthMiddleware` | Missing auth header, invalid format, empty token, valid token, invalid token |
| `validateToken` | Successful validation, API error, unauthenticated token |
| `GetUserInfo` | Valid context, nil context |
| `Handlers.Healthz/Readyz` | Returns ok |
| `Handlers.CreateTask` | Valid request, missing name, missing type, namespace scope, already exists |
| `Handlers.ListTasks` | With pagination, namespace filter, watch namespace scope |
| `Handlers.GetTask` | Found, not found, API error |
| `Handlers.DeleteTask` | Success, not found |
| `Handlers.GetTaskLogs` | No job, with job |
| `Handlers.GetTaskResult` | Success, no result, result not found |
| `Handlers.ListSessions/GetSession/DeleteSession` | CRUD operations |
| `Handlers.ListTools/GetTool` | Built-in tools, custom tools |
| `Handlers.ListAgents/GetAgent` | List and get operations |
| `parseDuration` | Valid formats, invalid format |
| `customErrorHandler` | Fiber error, generic error |
| `NewLoggingMiddleware` | Logs request |
| `NewMetricsMiddleware` | Records metrics |

**Mocking:** Use `sigs.k8s.io/controller-runtime/pkg/client/fake` for K8s client, create mock `fiber.Ctx`

---

### 2. `internal/llm/` (1 file, 0% coverage)

**File to create:** `internal/llm/provider_test.go`

| Function | Test Cases |
|----------|------------|
| `RegisterProvider` | Register new provider |
| `NewProvider` | Known provider, unknown provider |
| `ProviderError.Error` | Returns message |

---

### 3. `internal/llm/anthropic/` (1 file, 0% coverage)

**File to create:** `internal/llm/anthropic/provider_test.go`

| Function | Test Cases |
|----------|------------|
| `NewProvider` | With API key, without API key, with base URL |
| `Provider.Name` | Returns "anthropic" |
| `Provider.Complete` | User message, assistant message, tool calls, tool results, system prompt, API error |
| `Provider.Stream` | Streaming chunks, error handling |

**Mocking:** Create interface wrapper for Anthropic client, use httptest server

---

### 4. `internal/llm/openai/` (1 file, 0% coverage)

**File to create:** `internal/llm/openai/provider_test.go`

| Function | Test Cases |
|----------|------------|
| `NewProvider` | With API key, without API key, with base URL |
| `Provider.Name` | Returns "openai" |
| `Provider.Complete` | Various message roles, tool calls, no choices, API error |
| `Provider.Stream` | Streaming, EOF, error |

**Mocking:** Create interface wrapper for OpenAI client, use httptest server

---

### 5. `internal/metrics/` (1 file, 0% coverage)

**File to create:** `internal/metrics/metrics_test.go`

| Function | Test Cases |
|----------|------------|
| `RecordTaskCreated` | Increments counters |
| `RecordTaskCompleted` | Increments counters, records duration |
| `RecordTaskRetry` | Increments retry counter |
| `RecordWebhookDelivery` | Success status, failure status |
| `RecordAPIRequest` | 2xx status, 4xx status, 5xx status |
| `RecordToolCall` | Success, failure, duration |

---

### 6. `internal/tools/` (4 files, 0% coverage)

**Files to create:** `internal/tools/registry_test.go`, `internal/tools/web_search_test.go`, `internal/tools/code_exec_test.go`, `internal/tools/file_read_test.go`

| Function | Test Cases |
|----------|------------|
| `NewRegistry` | Creates empty registry |
| `Registry.Register` | Adds tool |
| `Registry.Get` | Found, not found |
| `Registry.List` | Returns all tools |
| `Registry.Execute` | Tool found, tool not found |
| `Registry.ToLLMTools` | Converts specified tools |
| `WebSearchTool.Name/Description/Parameters` | Return correct values |
| `WebSearchTool.Execute` | Valid query, empty query, mock search, API search, API error |
| `CodeExecTool.Name/Description/Parameters` | Return correct values |
| `CodeExecTool.Execute` | Python, JavaScript, Bash, unsupported lang, empty code, timeout |
| `FileReadTool.Name/Description/Parameters` | Return correct values |
| `FileReadTool.Execute` | Read file, file not found, directory, path traversal, offset/limit |

**Mocking:** Use httptest for web search, temp files for code exec and file read

---

### 7. `internal/worker/` (1 file, 0% coverage)

**File to create:** `internal/worker/tool_executor_test.go`

| Function | Test Cases |
|----------|------------|
| `NewToolExecutor` | Default namespace, custom namespace |
| `ToolExecutor.Execute` | POST request, custom method, auth header, auth body, HTTP error, timeout |
| `ToolExecutor.getSecretKey` | Mounted secret, K8s API secret, not found |

**Mocking:** Use httptest server, fake K8s client, temp files for mounted secrets

---

### 8. `internal/controller/` (Extend existing, 3.3% coverage)

**Files to create:** `internal/controller/session_manager_test.go`, `internal/controller/job_builder_test.go`, `internal/controller/priority_queue_test.go`, `internal/controller/webhook_test.go`

| Function | Test Cases |
|----------|------------|
| `SessionManager.IsLocked` | No session ref, session not found, locked by other, locked by self, not locked |
| `SessionManager.AcquireLock` | Create session, acquire existing, locked by other |
| `SessionManager.ReleaseLock` | Release own lock, session not found, not owner |
| `SessionManager.AppendMessages` | Append prompt and response, no session ref |
| `SessionManager.LoadTranscript` | Load messages, max messages limit, malformed lines |
| `SessionManager.GetSession/DeleteSession/ListSessions` | CRUD operations |
| `JobBuilder.Build` | Container task, AI task, with timeout, with secrets, with session |
| `JobBuilder.buildPodSecurityContext` | Returns secure context |
| `JobBuilder.buildContainerSecurityContext` | Returns secure context |
| `JobBuilder.buildContainer` | AI type, container type with/without image |
| `JobBuilder.buildResources` | Task resources, agent resources, defaults |
| `JobBuilder.buildEnvVars` | Basic vars, AI vars with agent, AI vars with provider |
| `PriorityQueue.Enqueue` | New item, update priority |
| `PriorityQueue.Dequeue` | Non-empty, empty |
| `PriorityQueue.Remove` | Existing, non-existing |
| `PriorityQueue.Peek` | Non-empty, empty |
| `PriorityQueue.Len/Contains/GetDepth` | Various states |
| `WebhookNotifier.Notify` | No URL, success, HTTP error, non-2xx response |

---

### 9. `workers/ai/` (1 file, 0% coverage)

**File to create:** `workers/ai/main_test.go`

| Function | Test Cases |
|----------|------------|
| `getAPIKey` | Env var, mounted secret, not found |
| `loadSessionContext` | Valid transcript, no file, malformed JSON |
| `buildLLMTools` | Built-in tools, custom tools, mixed |
| `loadCustomTools` | Load from K8s, skip built-in, not found |

**Note:** `run()`, `createK8sClient()`, `executeAgentLoop()`, `writeResult()` require K8s cluster - test indirectly or via integration tests

---

### 10. `workers/general/` (1 file, 0% coverage)

**File to create:** `workers/general/main_test.go`

| Function | Test Cases |
|----------|------------|
| `executeCommand` | Successful command, failed command, command not found |

**Note:** `run()`, `writeResult()` require K8s cluster - test indirectly or via integration tests

---

## Testing Patterns

### 1. Use Table-Driven Tests
```go
tests := []struct {
    name    string
    input   string
    want    string
    wantErr bool
}{
    {"valid", "input", "output", false},
    {"invalid", "bad", "", true},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        // test logic
    })
}
```

### 2. Fake K8s Client
```go
import "sigs.k8s.io/controller-runtime/pkg/client/fake"

scheme := runtime.NewScheme()
corev1alpha1.AddToScheme(scheme)
corev1.AddToScheme(scheme)
client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
```

### 3. HTTP Mocking
```go
server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte(`{"result": "ok"}`))
}))
defer server.Close()
```

### 4. Fiber Test App
```go
app := fiber.New()
app.Get("/test", handler)
req := httptest.NewRequest(http.MethodGet, "/test", nil)
resp, _ := app.Test(req)
```

---

## Implementation Order

1. **`internal/tools/`** - No external dependencies, pure Go
2. **`internal/api/pagination_test.go`** - Simple utility functions
3. **`internal/llm/provider_test.go`** - Registry logic only
4. **`internal/metrics/`** - Simple recording functions
5. **`internal/controller/priority_queue_test.go`** - Pure Go data structure
6. **`internal/controller/webhook_test.go`** - HTTP mocking
7. **`internal/worker/`** - HTTP + file mocking
8. **`internal/api/auth_test.go`** - K8s fake client
9. **`internal/api/handlers_test.go`** - K8s fake + Fiber test
10. **`internal/controller/session_manager_test.go`** - K8s fake client
11. **`internal/controller/job_builder_test.go`** - K8s fake client
12. **`internal/llm/anthropic/` and `internal/llm/openai/`** - HTTP mocking for API
13. **`workers/ai/` and `workers/general/`** - Unit test extractable functions

---

## Files to Create (20 test files)

```
internal/api/pagination_test.go
internal/api/auth_test.go
internal/api/handlers_test.go
internal/api/middleware_test.go
internal/api/server_test.go
internal/llm/provider_test.go
internal/llm/anthropic/provider_test.go
internal/llm/openai/provider_test.go
internal/metrics/metrics_test.go
internal/tools/registry_test.go
internal/tools/web_search_test.go
internal/tools/code_exec_test.go
internal/tools/file_read_test.go
internal/worker/tool_executor_test.go
internal/controller/session_manager_test.go
internal/controller/job_builder_test.go
internal/controller/priority_queue_test.go
internal/controller/webhook_test.go
workers/ai/main_test.go
workers/general/main_test.go
```

---

## Verification

After implementation, run:
```bash
make test
go tool cover -func=cover.out | grep total
```

Target: `total: (statements) 100.0%`

---

## Dependencies Required

The project already has the necessary testing dependencies:
- `github.com/onsi/ginkgo/v2` - BDD testing framework
- `github.com/onsi/gomega` - Matcher library
- `sigs.k8s.io/controller-runtime/pkg/client/fake` - Fake K8s client
- `sigs.k8s.io/controller-runtime/pkg/envtest` - Integration testing

Standard library testing packages:
- `testing` - Go standard testing
- `net/http/httptest` - HTTP test server
- `os` - Temp file creation
