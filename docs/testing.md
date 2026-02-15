# Testing

Orka has comprehensive test coverage across all packages, including unit tests, integration tests (envtest), end-to-end tests (Kind cluster), and frontend tests.

## Running Tests

```bash
# Run all Go unit tests (uses envtest for K8s API + etcd)
make test

# Run Go tests with coverage report
make test
go tool cover -func=cover.out | grep total

# Run frontend tests
make ui-test                # or: cd ui && bun run test
make ui-test-coverage       # or: cd ui && bun run test:coverage

# Run E2E tests (requires isolated Kind cluster)
make test-e2e

# Lint
make lint
make lint-fix
make ui-lint
```

## Test Structure

### Go Tests

Tests use **Ginkgo + Gomega** (BDD style) for controller/integration tests and standard Go `testing` for unit tests.

| Package | Test Files | Coverage Areas |
|---------|-----------|----------------|
| `internal/api/` | `handlers_test.go`, `auth_test.go`, `middleware_test.go`, `pagination_test.go`, `server_test.go`, `openai_compat_test.go` | REST API handlers, authentication, middleware, pagination, OpenAI compatibility |
| `internal/controller/` | `task_controller_test.go`, `agent_controller_test.go`, `tool_controller_test.go`, `session_manager_test.go`, `job_builder_test.go`, `priority_queue_test.go`, `webhook_test.go` | Reconciliation logic, session management, job building, coordination enforcement |
| `internal/llm/` | `provider_test.go` | Provider registry |
| `internal/llm/anthropic/` | `provider_test.go` | Anthropic API integration |
| `internal/llm/openai/` | `provider_test.go` | OpenAI API integration |
| `internal/metrics/` | `metrics_test.go` | Prometheus metrics recording |
| `internal/tools/` | `registry_test.go`, `web_search_test.go`, `code_exec_test.go`, `file_read_test.go`, `delegate_task_test.go`, `wait_for_tasks_test.go`, `create_pull_request_test.go`, `merge_pull_request_test.go`, `review_pull_request_test.go`, `post_review_comment_test.go`, `create_agent_test.go`, `delete_agent_test.go`, `integration_test.go` | Built-in tool implementations, coordination tools, PR tools, agent management tools |
| `internal/worker/` | `tool_executor_test.go` | Custom Tool CRD executor |
| `workers/ai/` | `main_test.go` | AI worker functions |
| `workers/general/` | `main_test.go` | General worker functions |
| `workers/agent/copilot/` | `main_test.go` | Copilot agent worker |
| `workers/agent/claude/` | `main_test.go` | Claude agent worker |

### E2E Tests

End-to-end tests run against a dedicated Kind cluster:

| Test File | Coverage |
|-----------|----------|
| `test/e2e/e2e_test.go` | Core task lifecycle |
| `test/e2e/agent_test.go` | Agent task execution |
| `test/e2e/agent_copilot_test.go` | Copilot runtime |
| `test/e2e/agent_claude_test.go` | Claude runtime |
| `test/e2e/agent_workspace_test.go` | Workspace/git clone |
| `test/e2e/agent_session_test.go` | Session continuity |

### Frontend Tests

Frontend tests use **Vitest + Testing Library + MSW**. Coverage thresholds are enforced in `vite.config.ts`.

```bash
cd ui && bun run test:coverage
```

## Testing Patterns

### Table-Driven Tests

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

### Fake Kubernetes Client

```go
scheme := runtime.NewScheme()
corev1alpha1.AddToScheme(scheme)
corev1.AddToScheme(scheme)
client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
```

### HTTP Mocking

```go
server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte(`{"result": "ok"}`))
}))
defer server.Close()
```

### Fiber Test App

```go
app := fiber.New()
app.Get("/test", handler)
req := httptest.NewRequest(http.MethodGet, "/test", nil)
resp, _ := app.Test(req)
```

### Frontend Test Mocking

```typescript
// Mock zustand persist middleware
vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))

// Use test utils with QueryClient wrapper
import { render } from '@/test/test-utils'
```

## Testing with Chat

When testing features via the chat endpoint, use **natural prompts** — the kind a human would actually type. Never reference internal concepts like agent names, tool names, or implementation details. Describe what you want done, not how the system should do it. The chat should infer the right agents, tools, delegation patterns, and cancellation logic on its own.

Good examples:
- "Research the benefits of Kubernetes and write a technical guide based on the findings."
- "What's the best container orchestration tool? Get me an answer as fast as possible."
- "Draft an outline for a blog post about containers and turn it into a full post."
- "Compare microservices vs monoliths from three angles, then synthesize into a recommendation."

Bad examples:
- "Create a coordinator agent and a researcher agent, then delegate two tasks..."
- "Use the send_message tool to send a message to task msg-receiver..."
- "Have three researchers race to answer..." (users don't think in terms of "researchers")
- "Use the first answer and cancel the others." (the system should infer this automatically)
