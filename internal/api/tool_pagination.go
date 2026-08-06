/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	toolListCursorVersion    = 1
	maxToolListCursorLength  = 16 * 1024
	maxToolCursorHistorySize = 64
)

// toolListCursor contains only logical current-inventory traversal state.
// It never carries a Kubernetes resourceVersion or continuation token, so
// callers cannot use it to select historical API-server snapshots.
type toolListCursor struct {
	Version   int    `json:"v"`
	Namespace string `json:"n"`
	Offset    int    `json:"o"`
	Snapshot  string `json:"s"`
}

func (h *Handlers) allowedBuiltinTools(c fiber.Ctx) ([]fiber.Map, error) {
	allowedTools := make([]fiber.Map, 0, len(builtinToolsList))
	for _, tool := range builtinToolsList {
		name, _ := tool["name"].(string)
		allowed, err := contextTokenAllowsToolMetadata(c, h.contextTokenAuthorization, "listTools", name)
		if err != nil {
			return nil, err
		}
		if allowed {
			allowedTools = append(allowedTools, tool)
		}
	}
	return allowedTools, nil
}

func (h *Handlers) filteredCustomToolNames(c fiber.Ctx) ([]string, bool) {
	if !h.contextTokenAuthorization.enforcing() || !isContextTokenRequest(c) {
		return nil, false
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.ContextToken == nil {
		return nil, false
	}
	allowedTools, ok := contextStringList(ui.ContextToken.TransactionContext, "allowedTools")
	if !ok {
		return nil, false
	}

	unique := make(map[string]struct{}, len(allowedTools))
	for _, name := range allowedTools {
		if name != "" {
			unique[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
}

// filteredToolListAll resolves only names already authorized by the token. It
// intentionally returns one complete logical page: exposing a raw Kubernetes
// continuation after filtering would reveal hidden inventory structure, while
// the signed token already bounds the candidate name set.
func (h *Handlers) filteredToolListAll(
	c fiber.Ctx,
	namespace string,
	builtins []fiber.Map,
	allowedNames []string,
) (ListResponse, error) {
	items := append([]fiber.Map(nil), builtins...)
	builtinNames := make(map[string]struct{}, len(builtins))
	for _, builtin := range builtins {
		if name, _ := builtin["name"].(string); name != "" {
			builtinNames[name] = struct{}{}
		}
	}
	for _, name := range allowedNames {
		if _, builtin := builtinNames[name]; builtin {
			continue
		}
		tool := &corev1alpha1.Tool{}
		if err := h.apiReader.Get(c.Context(), client.ObjectKey{Namespace: namespace, Name: name}, tool); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get tool %q: %v", name, err))
		}
		allowed, err := contextTokenAllowsToolMetadata(c, h.contextTokenAuthorization, "listTools", tool.Name)
		if err != nil {
			return ListResponse{}, err
		}
		if allowed {
			items = append(items, customToolListItem(tool))
		}
	}
	return ListResponse{Items: items, Metadata: ListMeta{}}, nil
}

func (h *Handlers) unpaginatedToolListAll(
	c fiber.Ctx,
	namespace string,
	builtins []fiber.Map,
) (ListResponse, error) {
	toolList := &corev1alpha1.ToolList{}
	if err := h.apiReader.List(c.Context(), toolList, &client.ListOptions{Namespace: namespace}); err != nil {
		return ListResponse{}, paginationListError("tools", err)
	}
	customItems, err := customToolListItems(c, h.contextTokenAuthorization, toolList.Items)
	if err != nil {
		return ListResponse{}, err
	}
	items := append(append([]fiber.Map(nil), builtins...), customItems...)
	return ListResponse{Items: items, Metadata: ListMeta{}}, nil
}

func decodeToolListCursor(raw, namespace string, itemCount int) (toolListCursor, error) {
	cursor := toolListCursor{Version: toolListCursorVersion, Namespace: namespace}
	if raw == "" {
		return cursor, nil
	}
	if len(raw) > maxToolListCursorLength {
		return toolListCursor{}, fmt.Errorf("invalid tools continue cursor: cursor is too large")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return toolListCursor{}, fmt.Errorf("invalid tools continue cursor: malformed encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return toolListCursor{}, fmt.Errorf("invalid tools continue cursor: malformed payload")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return toolListCursor{}, fmt.Errorf("invalid tools continue cursor: trailing payload")
	}
	if cursor.Version != toolListCursorVersion || cursor.Namespace != namespace ||
		cursor.Offset < 0 || (itemCount >= 0 && cursor.Offset > itemCount) || cursor.Snapshot == "" {
		return toolListCursor{}, fmt.Errorf("invalid tools continue cursor: cursor does not match this request")
	}
	return cursor, nil
}

func encodeToolListCursor(cursor toolListCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode tools continue cursor: %w", err)
	}
	if len(data) > maxToolListCursorLength {
		return "", fmt.Errorf("encode tools continue cursor: cursor is too large")
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func toolListSnapshot(items []fiber.Map) (string, error) {
	payload, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("encode tool list snapshot: %w", err)
	}
	sum := sha256.Sum256(payload)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func (h *Handlers) logicalToolListPage(
	c fiber.Ctx,
	namespace string,
	pageSize int,
	builtins []fiber.Map,
	rawCursor string,
) (ListResponse, error) {
	cursor, err := decodeToolListCursor(rawCursor, namespace, -1)
	if err != nil {
		return ListResponse{}, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	toolList := &corev1alpha1.ToolList{}
	if err := h.apiReader.List(c.Context(), toolList, &client.ListOptions{Namespace: namespace}); err != nil {
		return ListResponse{}, paginationListError("tools", err)
	}
	sort.Slice(toolList.Items, func(i, j int) bool { return toolList.Items[i].Name < toolList.Items[j].Name })
	customItems, err := customToolListItems(c, h.contextTokenAuthorization, toolList.Items)
	if err != nil {
		return ListResponse{}, err
	}
	items := append(append([]fiber.Map(nil), builtins...), customItems...)
	snapshot, err := toolListSnapshot(items)
	if err != nil {
		return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	if rawCursor == "" {
		cursor.Snapshot = snapshot
	} else if cursor.Snapshot != snapshot {
		return ListResponse{}, fiber.NewError(fiber.StatusGone, "tools continue cursor expired; restart the list")
	}
	if cursor.Offset > len(items) {
		return ListResponse{}, fiber.NewError(fiber.StatusBadRequest, "invalid tools continue cursor: cursor does not match this request")
	}

	end := min(len(items), cursor.Offset+pageSize)
	pageItems := append(make([]fiber.Map, 0, end-cursor.Offset), items[cursor.Offset:end]...)
	metadata := ListMeta{}
	if end < len(items) {
		next := toolListCursor{
			Version: toolListCursorVersion, Namespace: namespace, Offset: end, Snapshot: snapshot,
		}
		metadata.Continue, err = encodeToolListCursor(next)
		if err != nil {
			return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, err.Error())
		}
		remaining := int64(len(items) - end)
		metadata.RemainingItemCount = &remaining
	}
	return ListResponse{Items: pageItems, Metadata: metadata}, nil
}

func customToolListItems(
	c fiber.Ctx,
	authz ContextTokenAuthorizationConfig,
	tools []corev1alpha1.Tool,
) ([]fiber.Map, error) {
	items := make([]fiber.Map, 0, len(tools))
	for i := range tools {
		tool := &tools[i]
		allowed, err := contextTokenAllowsToolMetadata(c, authz, "listTools", tool.Name)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		items = append(items, customToolListItem(tool))
	}
	return items, nil
}

func customToolListItem(tool *corev1alpha1.Tool) fiber.Map {
	return fiber.Map{
		"name":        tool.Name,
		"namespace":   tool.Namespace,
		"builtin":     false,
		"description": tool.Spec.Description,
		"available":   tool.Status.Available,
		"url":         toolSpecHTTPURL(tool),
	}
}
