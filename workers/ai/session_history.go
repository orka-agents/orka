package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
)

const maxHistoryReceiptDepth = 8

type workerHistorySource struct {
	id, role, data string
}

func (s *workerSessionContext) readHistoryPage(
	ctx context.Context, id string, offset, limit int,
) (store.SessionHistoryResult, error) {
	suffix := "/history/" + url.PathEscape(id) + "?offset=" + strconv.Itoa(offset) + "&limit=" + strconv.Itoa(limit)
	var result store.SessionHistoryResult
	if err := s.client.call(ctx, http.MethodGet, suffix, nil, &result, 128*1024); err != nil {
		return result, err
	}
	if result.MessageID != id || result.Role == "" || result.Offset != offset || len(result.Data) > limit ||
		result.NextOffset != offset+len(result.Data) || result.NextOffset > result.TotalBytes ||
		result.TotalBytes > store.MaxSessionContextMessageBytes ||
		result.NextOffset == offset && result.NextOffset != result.TotalBytes {
		return result, fmt.Errorf("history receipt does not match the bounded source request")
	}
	return result, nil
}

func (s *workerSessionContext) loadHistorySource(ctx context.Context, id string) (*workerHistorySource, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionContextIOTimeout)
	defer cancel()
	page, err := s.readHistoryPage(ctx, id, 0, store.MaxSessionHistoryReadBytes)
	if err != nil {
		return nil, err
	}
	var data strings.Builder
	data.Grow(page.TotalBytes)
	data.WriteString(page.Data)
	for offset := page.NextOffset; offset < page.TotalBytes; {
		part, err := s.readHistoryPage(ctx, id, offset, store.MaxSessionHistoryReadBytes)
		if err != nil {
			return nil, err
		}
		if part.TotalBytes != page.TotalBytes || part.Role != page.Role {
			return nil, fmt.Errorf("history source changed during boundary validation")
		}
		data.WriteString(part.Data)
		offset = part.NextOffset
	}
	return &workerHistorySource{id: id, role: page.Role, data: data.String()}, nil
}

func (s *workerSessionContext) validateHistoryPage(
	ctx context.Context, page store.SessionHistoryResult, secrets ...string,
) error {
	if sanitizeConfiguredCheckpointText(page.Data, secrets...) != page.Data {
		return fmt.Errorf("saved history contains a configured secret; its byte cursor cannot be safely rewritten")
	}
	crosses, err := s.historyPageCrossesSecret(ctx, page, secrets...)
	if err != nil {
		return err
	}
	if crosses {
		return fmt.Errorf("saved history contains a configured secret crossing its byte cursor")
	}
	return nil
}

func (s *workerSessionContext) historyPageCrossesSecret(
	ctx context.Context, page store.SessionHistoryResult, secrets ...string,
) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionContextIOTimeout)
	defer cancel()
	values := configuredCheckpointSecrets(secrets...)
	seen := make(map[string]bool)
	for range maxHistoryReceiptDepth {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if page.Offset < 0 || page.NextOffset < page.Offset || page.NextOffset > page.TotalBytes ||
			page.TotalBytes > store.MaxSessionContextMessageBytes {
			return false, fmt.Errorf("history page bounds are invalid")
		}
		if page.Offset == page.NextOffset {
			return false, nil
		}
		if seen[page.MessageID] {
			return false, fmt.Errorf("saved history receipts contain a cycle")
		}
		seen[page.MessageID] = true
		cached, err := s.historySourceForPage(ctx, page)
		if err != nil {
			return false, err
		}
		for _, secret := range values {
			for _, form := range redact.SecretForms(secret, len(cached.data)) {
				for _, boundary := range []int{page.Offset, page.NextOffset} {
					start := max(0, boundary-len(form)+1)
					end := min(len(cached.data), boundary+len(form)-1)
					if strings.Contains(cached.data[start:end], form) {
						return true, nil
					}
				}
			}
		}
		// A receipt is also readable history. Follow its original coordinates so
		// wrapping a saved fragment cannot hide a credential learned later.
		var source struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(cached.data), &source) != nil || source.ID != page.MessageID || source.Role != page.Role {
			return false, fmt.Errorf("saved history identity does not match its source")
		}
		nested, ok := sessioncontext.HistoryPage(source.Content)
		if !ok {
			return false, nil
		}
		page = nested
	}
	return false, fmt.Errorf("saved history receipt nesting exceeds %d source messages", maxHistoryReceiptDepth)
}

func (s *workerSessionContext) historySourceForPage(
	ctx context.Context, page store.SessionHistoryResult,
) (*workerHistorySource, error) {
	// A page can split any byte of a credential, including its JSON escapes.
	// Keep one complete, bounded source for adjacent-page checks. Each new read
	// must still authenticate, and the current credential set is checked anew.
	cached := s.historySource
	if cached == nil || cached.id != page.MessageID || cached.role != page.Role || len(cached.data) != page.TotalBytes ||
		page.Data != "" && cached.data[page.Offset:page.NextOffset] != page.Data {
		var err error
		cached, err = s.loadHistorySource(ctx, page.MessageID)
		if err != nil {
			return nil, err
		}
		if cached.role != page.Role || len(cached.data) != page.TotalBytes ||
			page.Data != "" && cached.data[page.Offset:page.NextOffset] != page.Data {
			return nil, fmt.Errorf("history page does not match its complete saved source")
		}
		s.historySource = cached
	}
	return cached, nil
}

// Credentials can become known after a source was committed. Prepare safe
// checkpoint references without changing stored content or the active request.
func (s *workerSessionContext) prepareCheckpointSource(
	ctx context.Context, source *store.SessionMessage, secrets ...string,
) error {
	// Copies can appear in any message role or tool output. Their content, not
	// their role or name, determines whether they carry a history receipt.
	page, ok := s.historyPages[source.ID]
	if !ok {
		page, ok = sessioncontext.HistoryPage(source.Content)
	}
	if source.ID != s.current.ID && source.Metadata[store.SessionContextOutputRefKey] == source.ID {
		full, err := s.loadHistorySource(ctx, source.ID)
		if err != nil {
			return err
		}
		// Only identity and content are needed here. Leave unrelated JSON
		// numbers in their original representation, as the save path does.
		var original struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(full.data), &original) != nil || original.ID != source.ID ||
			original.Role != source.Role || original.Name != source.Name {
			return fmt.Errorf("saved history reference does not match its source")
		}
		page, ok = sessioncontext.HistoryPage(original.Content)
		// A stored preview can cut inside any credential, including one fully
		// contained in a history page. Redact the complete source before cutting
		// a new reference, even when its receipt coordinates are cached.
		clean := sanitizeCheckpointText(original.Content, secrets...)
		if clean != original.Content {
			const notice = "\n[Source excerpt; use read_session_history for the saved result.]"
			source.Content = clean
			if len(clean) > store.MaxSessionContextPreviewBytes {
				source.Content = truncateUTF8(clean, store.MaxSessionContextPreviewBytes-len(notice)) + notice
			}
		}
	}
	if !ok {
		return nil
	}
	if source.ID == s.current.ID {
		// The active request remains exact. Its checkpoint reference includes
		// only decoded receipt fields, excluding shadowed duplicate members.
		canonical, err := json.Marshal(page)
		if err != nil {
			return fmt.Errorf("cannot encode checkpoint history reference")
		}
		source.Content = string(canonical)
	}
	crosses, err := s.historyPageCrossesSecret(ctx, page, secrets...)
	if err != nil {
		return err
	}
	if crosses {
		source.Content = "[REDACTED] Saved history fragment contains configured secret content. " +
			"Use the original source message to retrieve safe history ranges."
	}
	return nil
}
