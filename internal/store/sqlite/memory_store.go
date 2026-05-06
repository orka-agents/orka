package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/sozercan/orka/internal/redact"
	"github.com/sozercan/orka/internal/store"
)

const (
	defaultMemoryLimit          = 50
	maxMemoryLimit              = 200
	defaultProposalLimit        = 100
	maxProposalLimit            = 500
	defaultTranscriptLimit      = 10
	maxTranscriptLimit          = 50
	defaultTranscriptSnippetLen = 360
	maxTranscriptSnippetLen     = 1000
)

type rowScanner interface {
	Scan(dest ...any) error
}

// CreateMemory inserts a durable memory record.
func (s *Store) CreateMemory(ctx context.Context, memory *store.Memory) error {
	if memory == nil {
		return fmt.Errorf("memory is required")
	}
	if strings.TrimSpace(memory.Namespace) == "" {
		return fmt.Errorf("namespace is required")
	}
	memory.Content = redact.SensitiveText(memory.Content)
	if strings.TrimSpace(memory.Content) == "" {
		return fmt.Errorf("content is required")
	}
	if memory.ID == "" {
		memory.ID = "mem-" + uuid.NewString()
	}
	if memory.Source == "" {
		memory.Source = "manual"
	}
	now := time.Now()
	if memory.CreatedAt.IsZero() {
		memory.CreatedAt = now
	}
	if memory.UpdatedAt.IsZero() {
		memory.UpdatedAt = now
	}

	tagsJSON, err := marshalTags(memory.Tags)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO memories
		 (id, namespace, session_name, agent_name, task_name, parent_task, source, content, tags_json,
		  disabled, deleted, created_at, updated_at, last_recalled_at, recalled_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		memory.ID, memory.Namespace, memory.SessionName, memory.AgentName, memory.TaskName, memory.ParentTask,
		memory.Source, memory.Content, tagsJSON, memory.Disabled, memory.Deleted, memory.CreatedAt, memory.UpdatedAt,
		memory.LastRecalledAt, memory.RecalledCount,
	)
	return err
}

// GetMemory fetches a memory by ID within a namespace.
func (s *Store) GetMemory(ctx context.Context, namespace, id string) (*store.Memory, error) {
	memory, err := scanMemory(s.db.QueryRowContext(ctx, selectMemorySQL()+` WHERE namespace = ? AND id = ?`, namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return memory, nil
}

// ListMemories lists memories matching the filter ordered for compact recall/governance.
func (s *Store) ListMemories(ctx context.Context, filter store.MemoryFilter) ([]store.Memory, error) {
	if strings.TrimSpace(filter.Namespace) == "" {
		return nil, fmt.Errorf("namespace is required")
	}

	query := selectMemorySQL() + ` WHERE namespace = ?`
	args := []any{filter.Namespace}
	query = appendMemoryFilters(query, &args, filter)
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, boundedLimit(filter.Limit, defaultMemoryLimit, maxMemoryLimit))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var memories []store.Memory
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, *memory)
	}
	return memories, rows.Err()
}

// UpdateMemory updates mutable memory fields.
func (s *Store) UpdateMemory(ctx context.Context, memory *store.Memory) error {
	if memory == nil {
		return fmt.Errorf("memory is required")
	}
	if memory.Namespace == "" || memory.ID == "" {
		return fmt.Errorf("namespace and id are required")
	}
	memory.Content = redact.SensitiveText(memory.Content)
	if strings.TrimSpace(memory.Content) == "" {
		return fmt.Errorf("content is required")
	}
	tagsJSON, err := marshalTags(memory.Tags)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE memories
		 SET session_name = ?, agent_name = ?, task_name = ?, parent_task = ?, source = ?, content = ?, tags_json = ?,
		     disabled = ?, deleted = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE namespace = ? AND id = ?`,
		memory.SessionName, memory.AgentName, memory.TaskName, memory.ParentTask, memory.Source, memory.Content, tagsJSON,
		memory.Disabled, memory.Deleted, memory.Namespace, memory.ID,
	)
	if err != nil {
		return err
	}
	return ensureRowsAffected(res)
}

// DeleteMemory soft-deletes a memory by ID within a namespace.
func (s *Store) DeleteMemory(ctx context.Context, namespace, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET deleted = TRUE, disabled = TRUE, updated_at = CURRENT_TIMESTAMP WHERE namespace = ? AND id = ?`,
		namespace, id,
	)
	if err != nil {
		return err
	}
	return ensureRowsAffected(res)
}

// SetMemoryDisabled toggles memory recall without deleting the provenance record.
func (s *Store) SetMemoryDisabled(ctx context.Context, namespace, id string, disabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memories SET disabled = ?, updated_at = CURRENT_TIMESTAMP WHERE namespace = ? AND id = ? AND deleted = FALSE`,
		disabled, namespace, id,
	)
	if err != nil {
		return err
	}
	return ensureRowsAffected(res)
}

// MarkMemoriesRecalled records recall statistics for memories injected into a prompt.
func (s *Store) MarkMemoriesRecalled(ctx context.Context, namespace string, ids []string) error {
	ids = compactStrings(ids)
	if namespace == "" || len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(ids))
	args := []any{namespace}
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE memories
		 SET last_recalled_at = CURRENT_TIMESTAMP, recalled_count = recalled_count + 1
		 WHERE namespace = ? AND id IN (`+strings.Join(placeholders, ",")+")",
		args...,
	)
	return err
}

// SearchTranscript searches transcript content and returns compact snippets.
func (s *Store) SearchTranscript(ctx context.Context, filter store.TranscriptSearchFilter) ([]store.TranscriptSearchResult, error) {
	if strings.TrimSpace(filter.Namespace) == "" {
		return nil, fmt.Errorf("namespace is required")
	}

	query := `SELECT id, session_name, role, COALESCE(name, ''), content, created_at
		FROM session_messages
		WHERE namespace = ? AND content <> ''`
	args := []any{filter.Namespace}

	searchTerm := strings.TrimSpace(filter.Query)
	searchTerms := transcriptSearchTerms(searchTerm)
	for _, term := range searchTerms {
		query += ` AND lower(content) LIKE ?`
		args = append(args, "%"+strings.ToLower(term)+"%")
	}
	if filter.SessionName != "" {
		query += ` AND session_name = ?`
		args = append(args, filter.SessionName)
	}
	if filter.ExcludeSessionName != "" {
		query += ` AND session_name <> ?`
		args = append(args, filter.ExcludeSessionName)
	}
	roles := compactStrings(filter.Roles)
	if len(roles) > 0 {
		placeholders := make([]string, 0, len(roles))
		for _, role := range roles {
			placeholders = append(placeholders, "?")
			args = append(args, role)
		}
		query += ` AND role IN (` + strings.Join(placeholders, ",") + `)`
	}

	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, boundedLimit(filter.Limit, defaultTranscriptLimit, maxTranscriptLimit))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	snippetLen := boundedLimit(filter.MaxSnippetLength, defaultTranscriptSnippetLen, maxTranscriptSnippetLen)
	snippetTerm := searchTerm
	if len(searchTerms) > 0 {
		snippetTerm = searchTerms[0]
	}
	var results []store.TranscriptSearchResult
	for rows.Next() {
		var result store.TranscriptSearchResult
		var content string
		if err := rows.Scan(&result.MessageID, &result.SessionName, &result.Role, &result.Name, &content, &result.CreatedAt); err != nil {
			return nil, err
		}
		result.Snippet = buildSnippet(content, snippetTerm, snippetLen)
		results = append(results, result)
	}
	return results, rows.Err()
}

// CreateMemoryProposal inserts a governance proposal.
func (s *Store) CreateMemoryProposal(ctx context.Context, proposal *store.MemoryProposal) error {
	if proposal == nil {
		return fmt.Errorf("proposal is required")
	}
	if strings.TrimSpace(proposal.Namespace) == "" {
		return fmt.Errorf("namespace is required")
	}
	if strings.TrimSpace(proposal.Type) == "" {
		proposal.Type = "skill"
	}
	proposal.Title = redact.SensitiveText(proposal.Title)
	proposal.Description = redact.SensitiveText(proposal.Description)
	proposal.Content = redact.SensitiveText(proposal.Content)
	proposal.Patch = redact.SensitiveText(proposal.Patch)
	proposal.ReviewNote = redact.SensitiveText(proposal.ReviewNote)
	if strings.TrimSpace(proposal.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if proposal.ID == "" {
		proposal.ID = "mprop-" + uuid.NewString()
	}
	if proposal.Status == "" {
		proposal.Status = "pending"
	}
	now := time.Now()
	if proposal.CreatedAt.IsZero() {
		proposal.CreatedAt = now
	}
	if proposal.UpdatedAt.IsZero() {
		proposal.UpdatedAt = now
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_proposals
		 (id, namespace, task_name, agent_name, type, skill_name, title, description, content, patch,
		  status, reviewer, review_note, created_at, updated_at, reviewed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		proposal.ID, proposal.Namespace, proposal.TaskName, proposal.AgentName, proposal.Type, proposal.SkillName,
		proposal.Title, proposal.Description, proposal.Content, proposal.Patch, proposal.Status, proposal.Reviewer,
		proposal.ReviewNote, proposal.CreatedAt, proposal.UpdatedAt, proposal.ReviewedAt,
	)
	return err
}

// GetMemoryProposal fetches a proposal by ID within a namespace.
func (s *Store) GetMemoryProposal(ctx context.Context, namespace, id string) (*store.MemoryProposal, error) {
	proposal, err := scanMemoryProposal(s.db.QueryRowContext(ctx, selectMemoryProposalSQL()+` WHERE namespace = ? AND id = ?`, namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return proposal, nil
}

// ListMemoryProposals lists proposals for governance review.
func (s *Store) ListMemoryProposals(ctx context.Context, filter store.MemoryProposalFilter) ([]store.MemoryProposal, error) {
	if strings.TrimSpace(filter.Namespace) == "" {
		return nil, fmt.Errorf("namespace is required")
	}

	query := selectMemoryProposalSQL() + ` WHERE namespace = ?`
	args := []any{filter.Namespace}
	if filter.TaskName != "" {
		query += ` AND task_name = ?`
		args = append(args, filter.TaskName)
	}
	if filter.AgentName != "" {
		query += ` AND agent_name = ?`
		args = append(args, filter.AgentName)
	}
	if filter.Type != "" {
		query += ` AND type = ?`
		args = append(args, filter.Type)
	}
	if filter.Status != "" {
		query += ` AND status = ?`
		args = append(args, filter.Status)
	}
	if q := strings.TrimSpace(filter.Query); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		query += ` AND (lower(title) LIKE ? OR lower(description) LIKE ? OR lower(content) LIKE ? OR lower(skill_name) LIKE ?)`
		args = append(args, like, like, like, like)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, boundedLimit(filter.Limit, defaultProposalLimit, maxProposalLimit))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var proposals []store.MemoryProposal
	for rows.Next() {
		proposal, err := scanMemoryProposal(rows)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, *proposal)
	}
	return proposals, rows.Err()
}

// ReviewMemoryProposal records a reviewer decision. It never applies the proposal automatically.
func (s *Store) ReviewMemoryProposal(ctx context.Context, review store.MemoryProposalReview) error {
	if review.Namespace == "" || review.ID == "" {
		return fmt.Errorf("namespace and id are required")
	}
	if strings.TrimSpace(review.Status) == "" {
		return fmt.Errorf("status is required")
	}
	review.ReviewNote = redact.SensitiveText(review.ReviewNote)
	res, err := s.db.ExecContext(ctx,
		`UPDATE memory_proposals
		 SET status = ?, reviewer = ?, review_note = ?, reviewed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		 WHERE namespace = ? AND id = ?`,
		review.Status, review.Reviewer, review.ReviewNote, review.Namespace, review.ID,
	)
	if err != nil {
		return err
	}
	return ensureRowsAffected(res)
}

// ArchiveMemoryProposal archives a proposal without applying it.
func (s *Store) ArchiveMemoryProposal(ctx context.Context, namespace, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE memory_proposals
		 SET status = 'archived', updated_at = CURRENT_TIMESTAMP
		 WHERE namespace = ? AND id = ?`,
		namespace, id,
	)
	if err != nil {
		return err
	}
	return ensureRowsAffected(res)
}

func selectMemorySQL() string {
	return `SELECT id, namespace, session_name, agent_name, task_name, parent_task, source, content, tags_json,
		disabled, deleted, created_at, updated_at, last_recalled_at, recalled_count FROM memories`
}

func appendMemoryFilters(query string, args *[]any, filter store.MemoryFilter) string {
	if !filter.IncludeDisabled {
		query += ` AND disabled = FALSE`
	}
	if !filter.IncludeDeleted {
		query += ` AND deleted = FALSE`
	}
	if filter.SessionName != "" {
		query += ` AND session_name = ?`
		*args = append(*args, filter.SessionName)
	}
	if filter.AgentName != "" {
		query += ` AND agent_name = ?`
		*args = append(*args, filter.AgentName)
	}
	if filter.TaskName != "" {
		query += ` AND task_name = ?`
		*args = append(*args, filter.TaskName)
	}
	if filter.ParentTask != "" {
		query += ` AND parent_task = ?`
		*args = append(*args, filter.ParentTask)
	}
	if filter.Source != "" {
		query += ` AND source = ?`
		*args = append(*args, filter.Source)
	}
	if q := strings.TrimSpace(filter.Query); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		query += ` AND (lower(content) LIKE ? OR lower(tags_json) LIKE ?)`
		*args = append(*args, like, like)
	}
	for _, tag := range compactStrings(filter.Tags) {
		query += ` AND lower(tags_json) LIKE ?`
		*args = append(*args, "%\""+strings.ToLower(tag)+"\"%")
	}
	ids := compactStrings(filter.IDs)
	if len(ids) > 0 {
		placeholders := make([]string, 0, len(ids))
		for _, id := range ids {
			placeholders = append(placeholders, "?")
			*args = append(*args, id)
		}
		query += ` AND id IN (` + strings.Join(placeholders, ",") + `)`
	}
	return query
}

func scanMemory(scanner rowScanner) (*store.Memory, error) {
	var memory store.Memory
	var tagsJSON string
	var lastRecalled sql.NullTime
	err := scanner.Scan(
		&memory.ID, &memory.Namespace, &memory.SessionName, &memory.AgentName, &memory.TaskName, &memory.ParentTask,
		&memory.Source, &memory.Content, &tagsJSON, &memory.Disabled, &memory.Deleted, &memory.CreatedAt, &memory.UpdatedAt,
		&lastRecalled, &memory.RecalledCount,
	)
	if err != nil {
		return nil, err
	}
	if lastRecalled.Valid {
		memory.LastRecalledAt = &lastRecalled.Time
	}
	if tagsJSON != "" {
		if err := json.Unmarshal([]byte(tagsJSON), &memory.Tags); err != nil {
			return nil, fmt.Errorf("failed to unmarshal memory tags: %w", err)
		}
	}
	return &memory, nil
}

func selectMemoryProposalSQL() string {
	return `SELECT id, namespace, task_name, agent_name, type, skill_name, title, description, content, patch,
		status, reviewer, review_note, created_at, updated_at, reviewed_at FROM memory_proposals`
}

func scanMemoryProposal(scanner rowScanner) (*store.MemoryProposal, error) {
	var proposal store.MemoryProposal
	var reviewedAt sql.NullTime
	err := scanner.Scan(
		&proposal.ID, &proposal.Namespace, &proposal.TaskName, &proposal.AgentName, &proposal.Type, &proposal.SkillName,
		&proposal.Title, &proposal.Description, &proposal.Content, &proposal.Patch, &proposal.Status, &proposal.Reviewer,
		&proposal.ReviewNote, &proposal.CreatedAt, &proposal.UpdatedAt, &reviewedAt,
	)
	if err != nil {
		return nil, err
	}
	if reviewedAt.Valid {
		proposal.ReviewedAt = &reviewedAt.Time
	}
	return &proposal, nil
}

func marshalTags(tags []string) (string, error) {
	tags = normalizeTags(tags)
	b, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("failed to marshal memory tags: %w", err)
	}
	return string(b), nil
}

func normalizeTags(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func transcriptSearchTerms(query string) []string {
	var terms []string
	var b strings.Builder
	inQuote := rune(0)

	flush := func() {
		term := strings.TrimSpace(b.String())
		if term != "" {
			terms = append(terms, term)
		}
		b.Reset()
	}

	for _, r := range query {
		if inQuote != 0 {
			if r == inQuote {
				flush()
				inQuote = 0
				continue
			}
			b.WriteRune(r)
			continue
		}

		switch {
		case r == '\'' || r == '"':
			flush()
			inQuote = r
		case unicode.IsSpace(r):
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return compactStrings(terms)
}

func compactStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func boundedLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

func ensureRowsAffected(res sql.Result) error {
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return store.ErrNotFound
	}
	return nil
}

func buildSnippet(content, term string, maxLen int) string {
	content = collapseWhitespace(content)
	if maxLen <= 0 || len([]rune(content)) <= maxLen {
		return content
	}

	idx := -1
	lowerContent := strings.ToLower(content)
	term = strings.ToLower(strings.TrimSpace(term))
	if term != "" {
		idx = strings.Index(lowerContent, term)
		if idx < 0 {
			for _, token := range strings.Fields(term) {
				if len(token) < 3 {
					continue
				}
				idx = strings.Index(lowerContent, token)
				if idx >= 0 {
					break
				}
			}
		}
	}

	runes := []rune(content)
	if idx < 0 {
		return string(runes[:maxLen]) + "…"
	}

	center := len([]rune(content[:idx]))
	start := center - maxLen/3
	if start < 0 {
		start = 0
	}
	end := start + maxLen
	if end > len(runes) {
		end = len(runes)
		start = end - maxLen
		if start < 0 {
			start = 0
		}
	}
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return snippet
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
