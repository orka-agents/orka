/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const memoryProposalTestUser = "system:serviceaccount:orka-system:jordan"

// setupMemoryProposalApp serves review and apply as the given identity; nil
// leaves the request without one.
func setupMemoryProposalApp(t *testing.T, ui *UserInfo) (*fiber.App, *sqlite.Store) {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewHandlers(HandlersConfig{MemoryStore: ss, MemoryProposalStore: ss})

	app := fiber.New()
	if ui != nil {
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, ui)
			return c.Next()
		})
	}
	app.Post("/api/v1/memory-proposals/:id/review", h.ReviewMemoryProposal)
	app.Post("/api/v1/memory-proposals/:id/apply", h.ApplyMemoryProposal)
	return app, ss
}

func createTestMemoryProposal(t *testing.T, ss *sqlite.Store, status string) *store.MemoryProposal {
	t.Helper()
	proposal := &store.MemoryProposal{
		Namespace: "default",
		Type:      "memory",
		Title:     "Warehouse return procedure",
		Content:   "Returns go through the dock 4 intake desk.",
	}
	require.NoError(t, ss.CreateMemoryProposal(context.Background(), proposal))
	if status != "" {
		require.NoError(t, ss.ReviewMemoryProposal(context.Background(), store.MemoryProposalReview{
			Namespace: "default",
			ID:        proposal.ID,
			Status:    status,
			Reviewer:  "earlier-reviewer",
		}))
	}
	return proposal
}

func postMemoryProposal(t *testing.T, app *fiber.App, id, action, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memory-proposals/"+id+"/"+action+"?namespace=default", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

func TestReviewMemoryProposalRecordsSignedInIdentity(t *testing.T) {
	for _, body := range []string{
		`{"status":"accepted","reviewNote":"Matches the operations note."}`,
		// Older CLIs always send the key; an empty value names no one.
		`{"status":"accepted","reviewNote":"Matches the operations note.","reviewer":""}`,
		`{"status":"accepted","reviewNote":"Matches the operations note.","reviewer":null}`,
	} {
		t.Run(body, func(t *testing.T) {
			app, ss := setupMemoryProposalApp(t, &UserInfo{Username: memoryProposalTestUser})
			proposal := createTestMemoryProposal(t, ss, "")

			status, raw := postMemoryProposal(t, app, proposal.ID, "review", body)
			require.Equal(t, http.StatusOK, status, raw)

			var record map[string]any
			require.NoError(t, json.Unmarshal([]byte(raw), &record))
			require.Equal(t, proposal.ID, record["id"])
			require.Equal(t, "accepted", record["status"])
			require.Equal(t, memoryProposalTestUser, record["reviewer"])
			require.Equal(t, "Matches the operations note.", record["reviewNote"])
			require.NotEmpty(t, record["reviewedAt"])
			// The review verb does not grant reading the proposal itself.
			require.NotContains(t, record, "content")
			require.NotContains(t, record, "title")

			stored, err := ss.GetMemoryProposal(context.Background(), "default", proposal.ID)
			require.NoError(t, err)
			require.Equal(t, memoryProposalTestUser, stored.Reviewer)
		})
	}
}

func TestApplyMemoryProposalRecordsSignedInIdentity(t *testing.T) {
	for _, body := range []string{``, `{}`, `{"appliedBy":""}`, `{"appliedBy":null}`} {
		t.Run(body, func(t *testing.T) {
			app, ss := setupMemoryProposalApp(t, &UserInfo{Username: memoryProposalTestUser})
			proposal := createTestMemoryProposal(t, ss, "accepted")

			status, raw := postMemoryProposal(t, app, proposal.ID, "apply", body)
			require.Equal(t, http.StatusOK, status, raw)

			stored, err := ss.GetMemoryProposal(context.Background(), "default", proposal.ID)
			require.NoError(t, err)
			require.Equal(t, "applied", stored.Status)
			require.Equal(t, memoryProposalTestUser, stored.AppliedBy)
		})
	}
}

func TestMemoryProposalActorFallsBackToSubject(t *testing.T) {
	app, ss := setupMemoryProposalApp(t, &UserInfo{AuthType: AuthTypeOIDC, Subject: "oidc-subject-1"})
	proposal := createTestMemoryProposal(t, ss, "")

	status, raw := postMemoryProposal(t, app, proposal.ID, "review", `{"status":"rejected"}`)
	require.Equal(t, http.StatusOK, status, raw)

	stored, err := ss.GetMemoryProposal(context.Background(), "default", proposal.ID)
	require.NoError(t, err)
	require.Equal(t, "oidc-subject-1", stored.Reviewer)
}

func TestMemoryProposalRejectsClientRecordedIdentity(t *testing.T) {
	tests := []struct {
		name   string
		action string
		body   string
		field  string
	}{
		{name: "reviewer", action: "review", body: `{"status":"accepted","reviewer":"someone-else"}`, field: "reviewer"},
		{name: "reviewer case folded", action: "review", body: `{"status":"accepted","Reviewer":"someone-else"}`, field: "reviewer"},
		{name: "reviewer matches caller", action: "review", body: `{"status":"accepted","reviewer":"` + memoryProposalTestUser + `"}`, field: "reviewer"},
		{name: "reviewer not a string", action: "review", body: `{"status":"accepted","reviewer":{"name":"someone-else"}}`, field: "reviewer"},
		{name: "appliedBy", action: "apply", body: `{"appliedBy":"someone-else"}`, field: "appliedBy"},
		{name: "appliedBy case folded", action: "apply", body: `{"APPLIEDBY":"someone-else"}`, field: "appliedBy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, ss := setupMemoryProposalApp(t, &UserInfo{Username: memoryProposalTestUser})
			initial := ""
			if tt.action == "apply" {
				initial = "accepted"
			}
			proposal := createTestMemoryProposal(t, ss, initial)

			status, raw := postMemoryProposal(t, app, proposal.ID, tt.action, tt.body)
			require.Equal(t, http.StatusBadRequest, status, raw)
			require.Contains(t, raw, tt.field+" cannot be set by clients; it is recorded from your identity")

			stored, err := ss.GetMemoryProposal(context.Background(), "default", proposal.ID)
			require.NoError(t, err)
			if tt.action == "review" {
				require.Equal(t, "pending", stored.Status)
				require.Empty(t, stored.Reviewer)
			} else {
				require.Equal(t, "accepted", stored.Status)
				require.Empty(t, stored.AppliedBy)
			}
		})
	}
}

func TestMemoryProposalRequiresIdentity(t *testing.T) {
	for _, ui := range []*UserInfo{nil, {Username: " "}} {
		app, ss := setupMemoryProposalApp(t, ui)
		pending := createTestMemoryProposal(t, ss, "")
		accepted := createTestMemoryProposal(t, ss, "accepted")

		status, raw := postMemoryProposal(t, app, pending.ID, "review", `{"status":"accepted"}`)
		require.Equal(t, http.StatusUnauthorized, status, raw)
		status, raw = postMemoryProposal(t, app, accepted.ID, "apply", `{}`)
		require.Equal(t, http.StatusUnauthorized, status, raw)

		stored, err := ss.GetMemoryProposal(context.Background(), "default", pending.ID)
		require.NoError(t, err)
		require.Equal(t, "pending", stored.Status)
		stored, err = ss.GetMemoryProposal(context.Background(), "default", accepted.ID)
		require.NoError(t, err)
		require.Equal(t, "accepted", stored.Status)
	}
}
