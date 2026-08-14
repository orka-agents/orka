package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	memoryruntime "github.com/orka-agents/orka/internal/memory"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	testMemoryProposalNamespaceKey = "namespace"
	testMemoryProposalTypeValue    = "memory"
)

func TestMemoryOperationCursorRoundTripBindsNamespaceAndFilters(t *testing.T) {
	createdAt := time.Date(2026, 8, 1, 8, 30, 0, 123, time.UTC)
	filter := store.MemoryOperationFilter{
		MemoryID: "mem-a", ProposalID: "proposal-a",
		Kinds:  []store.MemoryOperationKind{store.MemoryOperationDelete, store.MemoryOperationCreate},
		States: []store.MemoryOperationState{store.MemoryOperationQueued, store.MemoryOperationSucceeded},
		Limit:  2,
	}
	operations := []store.MemoryOperation{
		{Sequence: 9, CreatedAt: createdAt.Add(time.Second)},
		{Sequence: 8, CreatedAt: createdAt},
	}

	encoded, err := encodeMemoryOperationCursor("team-a", filter, operations)
	if err != nil || encoded == "" {
		t.Fatalf("encodeMemoryOperationCursor() = %q, %v", encoded, err)
	}
	cursor, err := decodeMemoryOperationCursor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.CreatedAt.Equal(createdAt) || cursor.Sequence != 8 ||
		!memoryOperationCursorMatches(cursor, "team-a", filter) {
		t.Fatalf("decoded cursor = %#v", cursor)
	}
	if memoryOperationCursorMatches(cursor, "team-b", filter) {
		t.Fatal("cursor matched a different namespace")
	}
	changed := filter
	changed.MemoryID = "mem-b"
	if memoryOperationCursorMatches(cursor, "team-a", changed) {
		t.Fatal("cursor matched different filters")
	}
}

func TestAuthorizeMemoryReadVisibilityRequiresOperateForDisabledContent(t *testing.T) {
	h := &Handlers{contextTokenAuthorization: ContextTokenAuthorizationConfig{
		Mode:                ContextTokenAuthorizationModeEnforce,
		MemoryReadScopes:    []string{ContextTokenScopeMemoryRead},
		MemoryOperateScopes: []string{ContextTokenScopeMemoryOperate},
	}}
	user := &UserInfo{AuthType: AuthTypeContextToken, ContextToken: &ContextToken{
		Scopes: []string{ContextTokenScopeMemoryRead},
	}}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		return c.Next()
	})
	app.Get("/authorize", func(c fiber.Ctx) error {
		if err := h.authorizeMemoryReadVisibility(c, "listMemories", c.Query("includeDisabled") == "true"); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	for _, path := range []string{"/authorize", "/authorize?includeDisabled=false"} {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s status = %d, want %d", path, resp.StatusCode, http.StatusNoContent)
		}
	}
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/authorize?includeDisabled=true", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("includeDisabled status = %d, want %d without operate scope", resp.StatusCode, http.StatusForbidden)
	}
	user.ContextToken.Scopes = append(user.ContextToken.Scopes, ContextTokenScopeMemoryOperate)
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/authorize?includeDisabled=true", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("includeDisabled status = %d, want %d with operate scope", resp.StatusCode, http.StatusNoContent)
	}
}

func TestPublicMemoryProposalGovernanceRequiresOperateScope(t *testing.T) {
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	memoryStore := sqlite.NewStore(db, ":memory:")
	h := NewHandlers(HandlersConfig{
		MemoryStore:         memoryStore,
		MemoryProposalStore: memoryStore,
		MemoryService: &memoryruntime.Service{
			Legacy: memoryStore, Proposals: memoryStore, Governed: memoryStore,
		},
		ContextTokenAuthorization: ContextTokenAuthorizationConfig{
			Mode:                ContextTokenAuthorizationModeEnforce,
			MemoryReadScopes:    []string{ContextTokenScopeMemoryRead},
			MemoryWriteScopes:   []string{ContextTokenScopeMemoryWrite},
			MemoryOperateScopes: []string{ContextTokenScopeMemoryOperate},
		},
	})
	user := &UserInfo{
		AuthType: AuthTypeContextToken, Namespace: testDefaultNamespace,
		ContextToken: &ContextToken{TransactionContext: map[string]any{testMemoryProposalNamespaceKey: testDefaultNamespace}},
	}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		return c.Next()
	})
	app.Post("/api/v1/memory-proposals", h.CreateMemoryProposal)
	app.Post("/api/v1/memory-proposals/:id/review", h.ReviewMemoryProposal)
	app.Post("/api/v1/memory-proposals/:id/apply", h.ApplyMemoryProposal)

	request := func(target, body string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		response, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}

	createBody := `{"namespace":"default","type":"memory","title":"Submitted proposal","content":"submitted content"}`
	user.ContextToken.Scopes = []string{ContextTokenScopeMemoryOperate}
	if response := request("/api/v1/memory-proposals", createBody); response.StatusCode != http.StatusForbidden {
		t.Fatalf("operate-only proposal create status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	user.ContextToken.Scopes = []string{ContextTokenScopeMemoryWrite}
	response := request("/api/v1/memory-proposals", createBody)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("write-scoped proposal create status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	var submitted store.MemoryProposal
	if err := json.NewDecoder(response.Body).Decode(&submitted); err != nil {
		t.Fatal(err)
	}

	reviewProposal := &store.MemoryProposal{
		Namespace: testDefaultNamespace, Type: testMemoryProposalTypeValue, Title: "Review proposal", Content: "review content",
	}
	if err := memoryStore.CreateMemoryProposal(context.Background(), reviewProposal); err != nil {
		t.Fatal(err)
	}
	reviewTarget := "/api/v1/memory-proposals/" + reviewProposal.ID + "/review?namespace=default"
	user.ContextToken.Scopes = []string{ContextTokenScopeMemoryWrite}
	if response := request(reviewTarget, `{"status":"accepted"}`); response.StatusCode != http.StatusForbidden {
		t.Fatalf("write-only proposal review status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	pending, err := memoryStore.GetMemoryProposal(context.Background(), testDefaultNamespace, reviewProposal.ID)
	if err != nil || pending.Status != reviewProposal.Status {
		t.Fatalf("proposal after denied review = %#v, %v", pending, err)
	}
	user.ContextToken.Scopes = []string{ContextTokenScopeMemoryOperate}
	if response := request(reviewTarget, `{"status":"accepted"}`); response.StatusCode != http.StatusNoContent {
		t.Fatalf("operate-scoped proposal review status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	applyProposal := &store.MemoryProposal{
		Namespace: testDefaultNamespace, Type: testMemoryProposalTypeValue, Title: "Apply proposal", Content: "apply content",
	}
	if err := memoryStore.CreateMemoryProposal(context.Background(), applyProposal); err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.ReviewMemoryProposal(context.Background(), store.MemoryProposalReview{
		Namespace: testDefaultNamespace, ID: applyProposal.ID, Status: githubCommandStatusAccepted, Reviewer: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	applyTarget := "/api/v1/memory-proposals/" + applyProposal.ID + "/apply?namespace=default"
	for name, scopes := range map[string][]string{
		"write only":   {ContextTokenScopeMemoryWrite},
		"operate only": {ContextTokenScopeMemoryOperate},
	} {
		t.Run(name, func(t *testing.T) {
			user.ContextToken.Scopes = scopes
			if response := request(applyTarget, `{}`); response.StatusCode != http.StatusForbidden {
				t.Fatalf("proposal apply status = %d, want %d", response.StatusCode, http.StatusForbidden)
			}
		})
	}
	accepted, err := memoryStore.GetMemoryProposal(context.Background(), testDefaultNamespace, applyProposal.ID)
	if err != nil || accepted.Status != githubCommandStatusAccepted || accepted.ApplyOperationID != "" {
		t.Fatalf("proposal after denied apply = %#v, %v", accepted, err)
	}
	user.ContextToken.Scopes = []string{ContextTokenScopeMemoryWrite, ContextTokenScopeMemoryOperate}
	if response := request(applyTarget, `{}`); response.StatusCode != http.StatusOK {
		t.Fatalf("write+operate proposal apply status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	applied, err := memoryStore.GetMemoryProposal(context.Background(), testDefaultNamespace, applyProposal.ID)
	if err != nil || applied.Status != "applied" || applied.AppliedMemoryID == "" {
		t.Fatalf("applied proposal = %#v, %v", applied, err)
	}
}

func TestMemoryOperationLocationPreservesSelectedNamespace(t *testing.T) {
	got := memoryOperationLocation("team blue", "operation/1")
	want := "/api/v1/memory-operations/operation%2F1?namespace=team+blue"
	if got != want {
		t.Fatalf("memoryOperationLocation() = %q, want %q", got, want)
	}
}

func TestMemorySearchContextUsesRemoteSearchScopeCallback(t *testing.T) {
	h := &Handlers{contextTokenAuthorization: ContextTokenAuthorizationConfig{
		Mode:                     ContextTokenAuthorizationModeEnforce,
		MemorySearchRemoteScopes: []string{ContextTokenScopeMemorySearchRemote},
	}}
	user := &UserInfo{AuthType: AuthTypeContextToken, ContextToken: &ContextToken{
		Scopes: []string{ContextTokenScopeMemoryRead},
	}}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		return c.Next()
	})
	app.Get("/authorize", func(c fiber.Ctx) error {
		if err := h.memorySearchContext(c).AuthorizeRemote(); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/authorize", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d without remote-search scope", resp.StatusCode, http.StatusForbidden)
	}
	user.ContextToken.Scopes = append(user.ContextToken.Scopes, ContextTokenScopeMemorySearchRemote)
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/authorize", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d with remote-search scope", resp.StatusCode, http.StatusNoContent)
	}
}

func TestGetMemoryDisabledInspectionRequiresOperateScope(t *testing.T) {
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	memoryStore := sqlite.NewStore(db, ":memory:")
	if err := memoryStore.CreateMemory(context.Background(), &store.Memory{
		ID: "mem-disabled", Namespace: "default", Content: "disabled content", Disabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	h := NewHandlers(HandlersConfig{
		MemoryStore:   memoryStore,
		MemoryService: &memoryruntime.Service{Legacy: memoryStore},
		ContextTokenAuthorization: ContextTokenAuthorizationConfig{
			Mode:                ContextTokenAuthorizationModeEnforce,
			MemoryReadScopes:    []string{ContextTokenScopeMemoryRead},
			MemoryOperateScopes: []string{ContextTokenScopeMemoryOperate},
		},
	})
	user := &UserInfo{
		AuthType: AuthTypeContextToken, Namespace: "default",
		ContextToken: &ContextToken{
			Scopes:             []string{ContextTokenScopeMemoryRead},
			TransactionContext: map[string]any{"namespace": "default"},
		},
	}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		return c.Next()
	})
	app.Get("/api/v1/memories/:id", h.GetMemory)

	request := func(target string) (*http.Response, store.Memory) {
		t.Helper()
		response, err := app.Test(httptest.NewRequest(http.MethodGet, target, nil))
		if err != nil {
			t.Fatal(err)
		}
		var memory store.Memory
		if response.StatusCode == http.StatusOK {
			if err := json.NewDecoder(response.Body).Decode(&memory); err != nil {
				_ = response.Body.Close()
				t.Fatal(err)
			}
		}
		_ = response.Body.Close()
		return response, memory
	}

	response, memory := request("/api/v1/memories/mem-disabled?namespace=default")
	if response.StatusCode != http.StatusOK || !memory.Disabled || memory.Content != "" {
		t.Fatalf("default disabled GET status=%d memory=%#v, want suppression metadata", response.StatusCode, memory)
	}
	response, _ = request("/api/v1/memories/mem-disabled?namespace=default&includeDisabled=true")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only includeDisabled status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	user.ContextToken.Scopes = append(user.ContextToken.Scopes, ContextTokenScopeMemoryOperate)
	response, memory = request("/api/v1/memories/mem-disabled?namespace=default&includeDisabled=true")
	if response.StatusCode != http.StatusOK || memory.Content != "disabled content" {
		t.Fatalf("operator includeDisabled status=%d memory=%#v, want content", response.StatusCode, memory)
	}
}
