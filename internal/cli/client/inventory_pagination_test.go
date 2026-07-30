/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListAgentsFollowsOpaqueContinuationsAndNamespace(t *testing.T) {
	const (
		namespace    = "inventory-ns"
		continuation = "agent-cursor+/=? segment"
	)

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("namespace"); got != namespace {
			t.Errorf("namespace query = %q, want %q", got, namespace)
		}

		switch requests {
		case 1:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items":    []map[string]any{{"metadata": map[string]any{"name": "agent-1"}}},
				"metadata": map[string]any{"continue": continuation},
			})
		case 2:
			if got := r.URL.Query().Get("continue"); got != continuation {
				t.Errorf("continue query = %q, want %q", got, continuation)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items":    []map[string]any{{"metadata": map[string]any{"name": "agent-2"}}},
				"metadata": map[string]any{},
			})
		default:
			t.Errorf("unexpected request %d", requests)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	agents, err := New(srv.URL, "").ListAgents(context.Background(), ListOptions{Namespace: namespace})
	if err != nil {
		t.Fatalf("ListAgents() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(agents) != 2 || agents[0].Name != "agent-1" || agents[1].Name != "agent-2" {
		t.Fatalf("agents = %#v, want both pages", agents)
	}
}

func TestListSkillsFollowsOpaqueContinuationsAndNamespace(t *testing.T) {
	const (
		namespace    = "inventory-ns"
		continuation = "skill-cursor+/=? segment"
	)

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("namespace"); got != namespace {
			t.Errorf("namespace query = %q, want %q", got, namespace)
		}

		switch requests {
		case 1:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items":    []map[string]any{{"name": "skill-1"}},
				"metadata": map[string]any{"continue": continuation},
			})
		case 2:
			if got := r.URL.Query().Get("continue"); got != continuation {
				t.Errorf("continue query = %q, want %q", got, continuation)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items":    []map[string]any{{"name": "skill-2"}},
				"metadata": map[string]any{},
			})
		default:
			t.Errorf("unexpected request %d", requests)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	skills, err := New(srv.URL, "").ListSkills(context.Background(), ListOptions{Namespace: namespace})
	if err != nil {
		t.Fatalf("ListSkills() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(skills) != 2 || skills[0].Name != "skill-1" || skills[1].Name != "skill-2" {
		t.Fatalf("skills = %#v, want both pages", skills)
	}
}

func TestCollectAllPagesReturnsPartialResultsForUnsafePagination(t *testing.T) {
	t.Run("later page failure", func(t *testing.T) {
		wantErr := errors.New("later page failed")
		calls := 0
		items, err := collectAllPages(context.Background(), "widgets", "", func(
			context.Context,
			string,
		) (*listPage[int], error) {
			calls++
			if calls == 1 {
				return &listPage[int]{Items: []int{1}, Continue: "next"}, nil
			}
			return nil, wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("collectAllPages() error = %v, want wrapped later-page error", err)
		}
		if len(items) != 1 || items[0] != 1 {
			t.Fatalf("items = %#v, want first-page partial result", items)
		}
		if !strings.Contains(err.Error(), "after 1 items on page 2") {
			t.Fatalf("error = %q, want partial-result context", err)
		}
	})

	t.Run("non advancing continuation", func(t *testing.T) {
		items, err := collectAllPages(context.Background(), "widgets", "", func(
			_ context.Context,
			continuation string,
		) (*listPage[int], error) {
			if continuation == "" {
				return &listPage[int]{Items: []int{1}, Continue: "same"}, nil
			}
			return &listPage[int]{Items: []int{2}, Continue: continuation}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "continuation did not advance") {
			t.Fatalf("collectAllPages() error = %v, want non-advancing continuation error", err)
		}
		if len(items) != 2 {
			t.Fatalf("len(items) = %d, want 2 partial items", len(items))
		}
	})

	t.Run("continuation cycle", func(t *testing.T) {
		items, err := collectAllPages(context.Background(), "widgets", "", func(
			_ context.Context,
			continuation string,
		) (*listPage[int], error) {
			switch continuation {
			case "":
				return &listPage[int]{Items: []int{1}, Continue: "a"}, nil
			case "a":
				return &listPage[int]{Items: []int{2}, Continue: "b"}, nil
			default:
				return &listPage[int]{Items: []int{3}, Continue: "a"}, nil
			}
		})
		if err == nil || !strings.Contains(err.Error(), "continuation cycle detected") {
			t.Fatalf("collectAllPages() error = %v, want continuation cycle error", err)
		}
		if len(items) != 3 {
			t.Fatalf("len(items) = %d, want 3 partial items", len(items))
		}
	})

	t.Run("remaining items without continuation", func(t *testing.T) {
		remaining := int64(1)
		items, err := collectAllPages(context.Background(), "widgets", "", func(
			context.Context,
			string,
		) (*listPage[int], error) {
			return &listPage[int]{Items: []int{1}, RemainingItemCount: &remaining}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "remaining items but no continuation") {
			t.Fatalf("collectAllPages() error = %v, want remaining-items error", err)
		}
		if len(items) != 1 {
			t.Fatalf("len(items) = %d, want one partial item", len(items))
		}
	})
}

func TestCollectAllPagesHonorsCancellationAndPageLimit(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		items, err := collectAllPages(ctx, "widgets", "", func(
			context.Context,
			string,
		) (*listPage[int], error) {
			cancel()
			return &listPage[int]{Items: []int{1}, Continue: "next"}, nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("collectAllPages() error = %v, want context canceled", err)
		}
		if len(items) != 1 {
			t.Fatalf("len(items) = %d, want first-page partial item", len(items))
		}
	})

	t.Run("page limit", func(t *testing.T) {
		calls := 0
		items, err := collectAllPages(context.Background(), "widgets", "", func(
			context.Context,
			string,
		) (*listPage[int], error) {
			calls++
			return &listPage[int]{Items: []int{calls}, Continue: fmt.Sprintf("cursor-%d", calls)}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "pagination page limit") {
			t.Fatalf("collectAllPages() error = %v, want page-limit error", err)
		}
		if calls != maxAutoPaginationPages || len(items) != maxAutoPaginationPages {
			t.Fatalf("calls/items = %d/%d, want %d", calls, len(items), maxAutoPaginationPages)
		}
	})
}
