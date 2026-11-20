// Copyright (c) 2026. MIT License - see LICENSE file for details.

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCancelCommandsPostTheReason(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var gotPath, gotReason, gotNamespace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotPath, gotReason, gotNamespace = r.URL.Path, body["reason"], r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/tasks/build/cancel":
			_, _ = w.Write([]byte(`{"name":"build","requested":true}`))
		case "/api/v1/sessions/chat/cancel":
			_, _ = w.Write([]byte(`{"session":"chat","tasks":[{"name":"turn-2","requested":false}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	for _, tc := range []struct {
		args       []string
		path, want string
	}{
		{args: []string{"task", "cancel", "build"}, path: "/api/v1/tasks/build/cancel", want: "Task cancellation requested: build"},
		{args: []string{"session", "cancel", "chat"}, path: "/api/v1/sessions/chat/cancel", want: "Task already cancelling or cancelled: turn-2"},
	} {
		root := newRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetArgs(append(tc.args, "--server", srv.URL, "-n", "team-a", "--reason", "wrong repo"))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if gotPath != tc.path || gotReason != "wrong repo" || gotNamespace != "team-a" {
			t.Fatalf("%v: request = %s reason=%q namespace=%q", tc.args, gotPath, gotReason, gotNamespace)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%v: output = %q, want %q", tc.args, out.String(), tc.want)
		}
	}
}
