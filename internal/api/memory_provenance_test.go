package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/orka-agents/orka/internal/store"
)

// These tests drive the router from NewServer: TokenReview authentication,
// SubjectAccessReview authorization, the handlers and the SQLite store. Only
// the Kubernetes review backends are fixtures.

var memoryCreateRoutes = []string{"/api/v1/memories", "/internal/v1/memories/default"}

func memoryPermission(verb, resource, name string) authorizationv1.ResourceAttributes {
	return authorizationv1.ResourceAttributes{Namespace: "default", Group: "core.orka.ai", Resource: resource, Verb: verb, Name: name}
}

// signInAs makes later requests authenticate as the named ServiceAccount,
// holding only the given permissions, and returns its username.
func (f *externalAuthorizationFixture) signInAs(t *testing.T, name string, permissions ...authorizationv1.ResourceAttributes) string {
	t.Helper()
	f.user.Username = "system:serviceaccount:default:" + name
	f.user.UID = name + "-uid"
	f.allowOnly(t, permissions...)
	return f.user.Username
}

func TestMemoryCreateRejectsClientProposalProvenance(t *testing.T) {
	for _, path := range memoryCreateRoutes {
		for _, tc := range []struct {
			name, body, field string
		}{
			{"proposal source", `{"content":"x","source":"memory_proposal"}`, "source memory_proposal"},
			{"proposal source folded", `{"content":"x","source":" Memory_Proposal "}`, "source memory_proposal"},
			{"proposal id", `{"content":"x","sourceProposalId":"protected"}`, "sourceProposalId"},
			{"proposal id with another source", `{"content":"x","source":"user","sourceProposalId":"protected"}`, "sourceProposalId"},
			{"blank proposal id", `{"content":"x","sourceProposalId":" "}`, "sourceProposalId"},
			{"proposal id folded key", `{"content":"x","SourceProposalID":"protected"}`, "sourceProposalId"},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				f := newExternalAuthorizationFixture(t)
				f.allowRoute(t, "POST /api/v1/memories")
				before := f.changes(t)
				status, body := f.request(t, http.MethodPost, path, tc.body)
				require.Equal(t, http.StatusBadRequest, status, body)
				require.Contains(t, body, tc.field+" cannot be set by clients")
				require.Equal(t, before, f.changes(t))
			})
		}
	}
}

func TestMemoryCreateKeepsClientSource(t *testing.T) {
	for _, path := range memoryCreateRoutes {
		t.Run(path, func(t *testing.T) {
			f := newExternalAuthorizationFixture(t)
			f.allowRoute(t, "POST /api/v1/memories")
			for _, tc := range []struct{ body, source string }{
				{`{"content":"named source","source":"user"}`, "user"},
				{`{"content":"default source"}`, "manual"},
				// Older clients send every key; empty provenance names nothing.
				{`{"content":"empty provenance","source":"","sourceProposalId":""}`, "manual"},
			} {
				status, body := f.request(t, http.MethodPost, path, tc.body)
				require.Equal(t, http.StatusCreated, status, body)
				var created store.Memory
				require.NoError(t, json.Unmarshal([]byte(body), &created))
				stored, err := f.store.GetMemory(t.Context(), "default", created.ID)
				require.NoError(t, err)
				require.Equal(t, tc.source, stored.Source)
				require.Empty(t, stored.SourceProposalID)
			}
		})
	}
}

// memoryUpdateRoutes prefix the memory ID on the public and internal routes.
var memoryUpdateRoutes = []string{"/api/v1/memories/", "/internal/v1/memories/default/"}

func TestMemoryUpdateRejectsClientProposalSource(t *testing.T) {
	for _, route := range memoryUpdateRoutes {
		for _, tc := range []struct{ name, body string }{
			{"proposal source", `{"source":"memory_proposal"}`},
			{"proposal source folded", `{"content":"relabeled","source":" Memory_Proposal "}`},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				f := newExternalAuthorizationFixture(t)
				f.allowOnly(t, memoryPermission("update", "memories", "protected"))
				before := f.changes(t)
				status, body := f.request(t, http.MethodPut, route+"protected", tc.body)
				require.Equal(t, http.StatusBadRequest, status, body)
				require.Contains(t, body, "source memory_proposal cannot be set by clients")
				require.Equal(t, before, f.changes(t))
				stored, err := f.store.GetMemory(t.Context(), "default", "protected")
				require.NoError(t, err)
				require.Equal(t, "manual", stored.Source)
				require.Equal(t, "protected-content", stored.Content)
			})
		}
		t.Run(route+"/named source", func(t *testing.T) {
			f := newExternalAuthorizationFixture(t)
			f.allowOnly(t, memoryPermission("update", "memories", "protected"))
			status, body := f.request(t, http.MethodPut, route+"protected", `{"source":"user"}`)
			require.Equal(t, http.StatusOK, status, body)
			stored, err := f.store.GetMemory(t.Context(), "default", "protected")
			require.NoError(t, err)
			require.Equal(t, "user", stored.Source)
		})
	}
}

// An applied memory can still be edited, including by sending back the memory
// as read, and its proposal provenance stays as apply recorded it.
func TestMemoryUpdateKeepsAppliedProvenance(t *testing.T) {
	for _, route := range memoryUpdateRoutes {
		t.Run(route, func(t *testing.T) {
			f := newExternalAuthorizationFixture(t)
			require.NoError(t, f.store.ReviewMemoryProposal(t.Context(), store.MemoryProposalReview{
				Namespace: "default", ID: "protected", Status: "accepted", Reviewer: "reviewer",
			}))
			applied, err := f.store.ApplyMemoryProposal(t.Context(), store.MemoryProposalApply{Namespace: "default", ID: "protected", AppliedBy: "applier"})
			require.NoError(t, err)
			f.allowOnly(t, memoryPermission("update", "memories", applied.ID))

			read := *applied
			read.Content = "edited after review"
			roundTrip, err := json.Marshal(read)
			require.NoError(t, err)
			for _, body := range []string{
				string(roundTrip),
				`{"content":"edited again"}`,
				`{"content":"edited with another link","sourceProposalId":"other"}`,
			} {
				status, response := f.request(t, http.MethodPut, route+applied.ID, body)
				require.Equal(t, http.StatusOK, status, response)
				stored, err := f.store.GetMemory(t.Context(), "default", applied.ID)
				require.NoError(t, err)
				require.Equal(t, memorySourceProposal, stored.Source)
				require.Equal(t, "protected", stored.SourceProposalID)
			}
			stored, err := f.store.GetMemory(t.Context(), "default", applied.ID)
			require.NoError(t, err)
			require.Equal(t, "edited with another link", stored.Content)
		})
	}
}

// A memory writer cannot create a memory that names a pending proposal, so
// applying the reviewed proposal writes and links the reviewed content.
func TestReviewedProposalApplyLinksReviewedContent(t *testing.T) {
	const reviewed = "Returns go through the dock 4 intake desk."
	f := newExternalAuthorizationFixture(t)

	f.signInAs(t, "proposer", memoryPermission("create", "memoryproposals", ""))
	status, body := f.request(t, http.MethodPost, "/api/v1/memory-proposals",
		`{"type":"memory","title":"Dock 4 returns","content":"`+reviewed+`"}`)
	require.Equal(t, http.StatusCreated, status, body)
	var proposal store.MemoryProposal
	require.NoError(t, json.Unmarshal([]byte(body), &proposal))
	require.Equal(t, "pending", proposal.Status)

	f.signInAs(t, "memory-writer", memoryPermission("create", "memories", ""))
	for _, path := range memoryCreateRoutes {
		status, body = f.request(t, http.MethodPost, path,
			`{"content":"Unreviewed content.","source":"memory_proposal","sourceProposalId":"`+proposal.ID+`"}`)
		require.Equal(t, http.StatusBadRequest, status, body)
	}

	reviewer := f.signInAs(t, "reviewer", memoryPermission("review", "memoryproposals", proposal.ID))
	status, body = f.request(t, http.MethodPost, "/api/v1/memory-proposals/"+proposal.ID+"/review", `{"status":"accepted"}`)
	require.Equal(t, http.StatusOK, status, body)

	applyPermissions := make([]authorizationv1.ResourceAttributes, 0, 3)
	applyPermissions = append(applyPermissions,
		memoryPermission("apply", "memoryproposals", proposal.ID),
		memoryPermission("create", "memories", ""),
	)
	applier := f.signInAs(t, "applier", applyPermissions...)
	status, body = f.request(t, http.MethodPost, "/api/v1/memory-proposals/"+proposal.ID+"/apply", `{}`)
	require.Equal(t, http.StatusOK, status, body)
	var applied store.Memory
	require.NoError(t, json.Unmarshal([]byte(body), &applied))
	require.Equal(t, reviewed, applied.Content)
	require.Equal(t, "memory_proposal", applied.Source)
	require.Equal(t, proposal.ID, applied.SourceProposalID)

	stored, err := f.store.GetMemoryProposal(t.Context(), "default", proposal.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", stored.Status)
	require.Equal(t, applied.ID, stored.AppliedMemoryID)
	require.Equal(t, reviewer, stored.Reviewer)
	require.NotNil(t, stored.ReviewedAt)
	require.Equal(t, applier, stored.AppliedBy)
	require.NotNil(t, stored.AppliedAt)

	// Applying again returns the same memory once its read is authorized.
	f.signInAs(t, "applier", append(applyPermissions, memoryPermission("get", "memories", applied.ID))...)
	status, body = f.request(t, http.MethodPost, "/api/v1/memory-proposals/"+proposal.ID+"/apply", `{}`)
	require.Equal(t, http.StatusOK, status, body)
	var again store.Memory
	require.NoError(t, json.Unmarshal([]byte(body), &again))
	require.Equal(t, applied.ID, again.ID)
	require.Equal(t, reviewed, again.Content)

	memories, err := f.store.ListMemories(t.Context(), store.MemoryFilter{Namespace: "default", IncludeDisabled: true, IncludeDeleted: true})
	require.NoError(t, err)
	linked := 0
	for _, memory := range memories {
		require.NotEqual(t, "Unreviewed content.", memory.Content)
		if memory.SourceProposalID == proposal.ID {
			linked++
		}
	}
	require.Equal(t, 1, linked)
}

// Apply writes its memory and marks the proposal applied in one transaction,
// so a memory that already names an unapplied proposal was written some other
// way, for example by a direct store caller. Apply refuses to adopt it.
func TestProposalApplyRefusesMemoryItDidNotWrite(t *testing.T) {
	for _, path := range []string{"/api/v1/memory-proposals/protected/apply", "/internal/v1/memory-proposals/default/protected/apply"} {
		t.Run(path, func(t *testing.T) {
			f := newExternalAuthorizationFixture(t)
			require.NoError(t, f.store.ReviewMemoryProposal(t.Context(), store.MemoryProposalReview{
				Namespace: "default", ID: "protected", Status: "accepted", Reviewer: "reviewer",
			}))
			memory, err := f.store.GetMemory(t.Context(), "default", "protected")
			require.NoError(t, err)
			memory.SourceProposalID = "protected"
			require.NoError(t, f.store.UpdateMemory(t.Context(), memory))

			f.allowOnly(t,
				memoryPermission("apply", "memoryproposals", "protected"),
				memoryPermission("create", "memories", ""),
				memoryPermission("get", "memories", "protected"),
			)
			before := f.changes(t)
			status, body := f.request(t, http.MethodPost, path, `{}`)
			require.Equal(t, http.StatusConflict, status, body)
			require.NotContains(t, body, "protected-content")
			require.Equal(t, before, f.changes(t))
			for _, review := range f.reviews {
				attributes := review.ResourceAttributes
				require.False(t, attributes.Resource == "memories" && attributes.Verb == "get", "a refused memory is never read")
			}

			proposal, err := f.store.GetMemoryProposal(t.Context(), "default", "protected")
			require.NoError(t, err)
			require.Equal(t, "accepted", proposal.Status)
			require.Empty(t, proposal.AppliedMemoryID)
			require.Empty(t, proposal.AppliedBy)
			require.Nil(t, proposal.AppliedAt)
		})
	}
}
