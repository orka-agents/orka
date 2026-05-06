package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestSendMessageTool(t *testing.T) {
	tool := NewSendMessageTool()

	if tool.Name() != sendMessageToolName {
		t.Errorf("Name() = %q, want %q", tool.Name(), sendMessageToolName)
	}

	t.Run("missing env vars returns error", func(t *testing.T) {
		t.Setenv(envOrkaControllerURL, "")
		t.Setenv(envOrkaTaskName, "")
		t.Setenv(envOrkaTaskNamespace, "")
		t.Setenv(envOrkaParentTask, "")

		args, _ := json.Marshal(SendMessageArgs{ToTask: "peer", Content: "hi"})
		_, err := tool.Execute(context.Background(), args)
		if err == nil {
			t.Fatal("expected error for missing env vars")
		}
	})

	t.Run("missing required args returns error", func(t *testing.T) {
		t.Setenv(envOrkaControllerURL, localhostURL)
		t.Setenv(envOrkaTaskName, testTaskAName)
		t.Setenv(envOrkaTaskNamespace, "ns")
		t.Setenv(envOrkaParentTask, "parent")

		args, _ := json.Marshal(SendMessageArgs{ToTask: "", Content: "hi"})
		_, err := tool.Execute(context.Background(), args)
		if err == nil {
			t.Fatal("expected error for empty to_task")
		}
	})

	t.Run("sends message successfully", func(t *testing.T) {
		var mu sync.Mutex
		var receivedBody map[string]string

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		t.Setenv(envOrkaControllerURL, server.URL)
		t.Setenv(envOrkaTaskName, testTaskAName)
		t.Setenv(envOrkaTaskNamespace, defaultNamespace)
		t.Setenv(envOrkaParentTask, testCoordinatorTaskName)

		args, _ := json.Marshal(SendMessageArgs{ToTask: testTaskBName, Content: "found a bug"})
		result, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result != "Message sent to task-b" {
			t.Errorf("result = %q, want %q", result, "Message sent to task-b")
		}

		mu.Lock()
		defer mu.Unlock()
		if receivedBody["fromTask"] != testTaskAName {
			t.Errorf("fromTask = %q, want %q", receivedBody["fromTask"], testTaskAName)
		}
		if receivedBody["toTask"] != testTaskBName {
			t.Errorf("toTask = %q, want %q", receivedBody["toTask"], testTaskBName)
		}
		if receivedBody["parentTask"] != testCoordinatorTaskName {
			t.Errorf("parentTask = %q, want %q", receivedBody["parentTask"], testCoordinatorTaskName)
		}
	})

	t.Run("broadcast to all siblings", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		t.Setenv(envOrkaControllerURL, server.URL)
		t.Setenv(envOrkaTaskName, testTaskAName)
		t.Setenv(envOrkaTaskNamespace, defaultNamespace)
		t.Setenv(envOrkaParentTask, testCoordinatorTaskName)

		args, _ := json.Marshal(SendMessageArgs{ToTask: "*", Content: "heads up everyone"})
		result, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result != "Message sent to all siblings" {
			t.Errorf("result = %q, want %q", result, "Message sent to all siblings")
		}
	})

	t.Run("server error returns error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("db error")) //nolint:errcheck
		}))
		defer server.Close()

		t.Setenv(envOrkaControllerURL, server.URL)
		t.Setenv(envOrkaTaskName, testTaskAName)
		t.Setenv(envOrkaTaskNamespace, defaultNamespace)
		t.Setenv(envOrkaParentTask, testCoordinatorTaskName)

		args, _ := json.Marshal(SendMessageArgs{ToTask: testTaskBName, Content: "hi"})
		_, err := tool.Execute(context.Background(), args)
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
	})
}

func TestCheckMessagesTool(t *testing.T) {
	tool := NewCheckMessagesTool()

	if tool.Name() != checkMessagesToolName {
		t.Errorf("Name() = %q, want %q", tool.Name(), checkMessagesToolName)
	}

	t.Run("missing env vars returns error", func(t *testing.T) {
		t.Setenv(envOrkaControllerURL, "")
		t.Setenv(envOrkaTaskName, "")
		t.Setenv(envOrkaTaskNamespace, "")
		t.Setenv(envOrkaParentTask, "")

		_, err := tool.Execute(context.Background(), nil)
		if err == nil {
			t.Fatal("expected error for missing env vars")
		}
	})

	t.Run("returns no messages", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]")) //nolint:errcheck
		}))
		defer server.Close()

		t.Setenv(envOrkaControllerURL, server.URL)
		t.Setenv(envOrkaTaskName, testTaskBName)
		t.Setenv(envOrkaTaskNamespace, defaultNamespace)
		t.Setenv(envOrkaParentTask, testCoordinatorTaskName)

		result, err := tool.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result != noNewMessagesText {
			t.Errorf("result = %q, want %q", result, noNewMessagesText)
		}
	})

	t.Run("returns messages", func(t *testing.T) {
		msgs := `[{"id":1,"fromTask":"task-a","content":"hello"}]`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("markRead") != trueStr {
				t.Errorf("markRead = %q, want %q", r.URL.Query().Get("markRead"), trueStr)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(msgs)) //nolint:errcheck
		}))
		defer server.Close()

		t.Setenv(envOrkaControllerURL, server.URL)
		t.Setenv(envOrkaTaskName, testTaskBName)
		t.Setenv(envOrkaTaskNamespace, defaultNamespace)
		t.Setenv(envOrkaParentTask, testCoordinatorTaskName)

		result, err := tool.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result != msgs {
			t.Errorf("result = %q, want %q", result, msgs)
		}
	})

	t.Run("mark_read false passes through", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("markRead") != falseStr {
				t.Errorf("markRead = %q, want %q", r.URL.Query().Get("markRead"), falseStr)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]")) //nolint:errcheck
		}))
		defer server.Close()

		t.Setenv(envOrkaControllerURL, server.URL)
		t.Setenv(envOrkaTaskName, testTaskBName)
		t.Setenv(envOrkaTaskNamespace, defaultNamespace)
		t.Setenv(envOrkaParentTask, testCoordinatorTaskName)

		markRead := false
		args, _ := json.Marshal(CheckMessagesArgs{MarkRead: &markRead})
		_, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
}
