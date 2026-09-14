package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestSessionTranscriptPreservesToolArguments(t *testing.T) {
	for _, tt := range []struct {
		name, arguments string
	}{
		{"nested numbers", `{"big":9223372036854775807,"decimal":0.1234567890123456789,"nested":[9007199254740993,-0,1.2300e+42],"zero":-0}`},
		{"large exponent", `{"value":1e+400}`},
		{"serialized text", `"{\n  \"id\": 9223372036854775807\n}\n"`},
		{"null", `null`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, err := sqlite.NewDB(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			ss := sqlite.NewStore(db, ":memory:")
			ctx := t.Context()
			require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
				Namespace: "default", Name: "precise-tools", SessionType: store.SessionTypeChat,
			}))
			require.NoError(t, ss.AppendMessages(ctx, "default", "precise-tools", []store.SessionMessage{{
				Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: "precise-call", Name: "read_value", Arguments: json.RawMessage(tt.arguments),
				}},
			}}))

			// Continuation must reload the same arguments sent by the provider.
			chat := &ChatHandler{sessionStore: ss}
			messages, err := chat.loadChatSession(ctx, "default", "precise-tools")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			require.Equal(t, tt.arguments, string(messages[0].ToolCalls[0].Arguments))

			handlers := NewHandlers(HandlersConfig{
				Client: fake.NewClientBuilder().WithScheme(newTestScheme()).Build(), SessionStore: ss,
			})
			app := fiber.New()
			app.Get("/sessions/:id", handlers.GetSession)
			response, err := app.Test(httptest.NewRequest(http.MethodGet, "/sessions/precise-tools", nil))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
			require.Equal(t, http.StatusOK, response.StatusCode)
			var result struct {
				Transcript string `json:"transcript"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
			var message struct {
				ToolCalls []struct {
					Arguments     json.RawMessage `json:"arguments"`
					ArgumentsText string          `json:"argumentsText"`
				} `json:"toolCalls"`
			}
			require.NoError(t, json.Unmarshal([]byte(result.Transcript), &message))
			require.Len(t, message.ToolCalls, 1)
			call := message.ToolCalls[0]
			require.Equal(t, tt.arguments, string(call.Arguments))
			var expectedText string
			if tt.arguments == "null" || json.Unmarshal([]byte(tt.arguments), &expectedText) != nil {
				var formatted bytes.Buffer
				require.NoError(t, json.Indent(&formatted, []byte(tt.arguments), "", "  "))
				expectedText = formatted.String()
			}
			require.Equal(t, expectedText, call.ArgumentsText)

			// Display text belongs only to the public projection.
			stored, err := ss.LoadTranscript(ctx, "default", "precise-tools", 0)
			require.NoError(t, err)
			encoded, err := json.Marshal(stored)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "argumentsText")
		})
	}
}
