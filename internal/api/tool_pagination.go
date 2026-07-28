/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"slices"
	"sort"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	toolListCursorVersion    = 1
	maxToolListCursorLength  = 16 * 1024
	maxToolCursorHistorySize = 64
	toolCursorDigestBytes    = 16

	toolCursorModeKubernetes = "k"
	toolCursorModeFiltered   = "f"
)

type toolListCursor struct {
	Version         int      `json:"v"`
	Scope           string   `json:"s"`
	Mode            string   `json:"m"`
	BuiltinOffset   int      `json:"b"`
	Continue        string   `json:"c,omitempty"`
	History         []string `json:"h,omitempty"`
	ResourceVersion string   `json:"r,omitempty"`
	CustomAvailable bool     `json:"a,omitempty"`
	FilteredOffset  int      `json:"o,omitempty"`
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
	if !h.contextTokenAuthorization.enforcing() {
		return nil, false
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil, false
	}
	allowedTools, ok := contextStringList(ui.ContextToken.TransactionContext, "allowedTools")
	if !ok {
		return nil, false
	}

	unique := make(map[string]struct{}, len(allowedTools))
	for _, name := range allowedTools {
		if len(utilvalidation.IsDNS1123Subdomain(name)) != 0 {
			continue
		}
		unique[name] = struct{}{}
	}
	names := make([]string, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
}

func (h *Handlers) toolListCursorScope(c fiber.Ctx, namespace string, builtins []fiber.Map, mode string) string {
	digest := sha256.New()
	writeToolCursorScopePart(digest, "namespace")
	writeToolCursorScopePart(digest, namespace)
	writeToolCursorScopePart(digest, "mode")
	writeToolCursorScopePart(digest, mode)
	writeToolCursorScopePart(digest, "builtins")
	for _, tool := range builtins {
		name, _ := tool["name"].(string)
		writeToolCursorScopePart(digest, name)
	}

	if h.contextTokenAuthorization.enforcing() {
		ui := GetUserInfo(c)
		if ui != nil && ui.AuthType == AuthTypeContextToken && ui.ContextToken != nil {
			if allowedTools, ok := contextStringList(ui.ContextToken.TransactionContext, "allowedTools"); ok {
				writeToolCursorScopePart(digest, "allowedTools")
				allowedTools = append([]string(nil), allowedTools...)
				sort.Strings(allowedTools)
				for _, name := range allowedTools {
					writeToolCursorScopePart(digest, name)
				}
			}
		}
	}

	sum := digest.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:toolCursorDigestBytes])
}

func writeToolCursorScopePart(dst hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = dst.Write(length[:])
	_, _ = io.WriteString(dst, value)
}

func decodeToolListCursor(
	raw string,
	expectedScope string,
	expectedMode string,
	builtinCount int,
	filteredCount int,
) (toolListCursor, error) {
	cursor := toolListCursor{
		Version: toolListCursorVersion,
		Scope:   expectedScope,
		Mode:    expectedMode,
	}
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
	if err := validateToolListCursor(cursor, expectedScope, expectedMode, builtinCount, filteredCount); err != nil {
		return toolListCursor{}, err
	}
	return cursor, nil
}

func validateToolListCursor(
	cursor toolListCursor,
	expectedScope string,
	expectedMode string,
	builtinCount int,
	filteredCount int,
) error {
	if cursor.Version != toolListCursorVersion {
		return fmt.Errorf("invalid tools continue cursor: unsupported version")
	}
	if cursor.Scope != expectedScope || cursor.Mode != expectedMode {
		return fmt.Errorf("invalid tools continue cursor: cursor does not match this request")
	}
	if cursor.BuiltinOffset < 0 || cursor.BuiltinOffset > builtinCount {
		return fmt.Errorf("invalid tools continue cursor: invalid built-in offset")
	}
	if cursor.Mode == toolCursorModeFiltered {
		return validateFilteredToolListCursor(cursor, builtinCount, filteredCount)
	}
	return validateKubernetesToolListCursor(cursor, builtinCount)
}

func validateFilteredToolListCursor(cursor toolListCursor, builtinCount, filteredCount int) error {
	if cursor.FilteredOffset < 0 || cursor.FilteredOffset > filteredCount {
		return fmt.Errorf("invalid tools continue cursor: invalid filtered offset")
	}
	if cursor.Continue != "" || len(cursor.History) != 0 || cursor.ResourceVersion != "" || cursor.CustomAvailable {
		return fmt.Errorf("invalid tools continue cursor: mixed filtered and Kubernetes state")
	}
	if cursor.BuiltinOffset < builtinCount && cursor.FilteredOffset != 0 {
		return fmt.Errorf("invalid tools continue cursor: filtered pagination started before built-ins completed")
	}
	return nil
}

func validateKubernetesToolListCursor(cursor toolListCursor, builtinCount int) error {
	if cursor.FilteredOffset != 0 {
		return fmt.Errorf("invalid tools continue cursor: unexpected filtered offset")
	}
	if cursor.ResourceVersion == "" {
		return fmt.Errorf("invalid tools continue cursor: missing resource version")
	}
	if len(cursor.History) > maxToolCursorHistorySize {
		return fmt.Errorf("invalid tools continue cursor: continuation history is too large")
	}

	seen := make(map[string]struct{}, len(cursor.History))
	for _, digest := range cursor.History {
		decoded, err := base64.RawURLEncoding.DecodeString(digest)
		if err != nil || len(decoded) != toolCursorDigestBytes {
			return fmt.Errorf("invalid tools continue cursor: malformed continuation history")
		}
		if _, duplicate := seen[digest]; duplicate {
			return fmt.Errorf("invalid tools continue cursor: duplicate continuation history")
		}
		seen[digest] = struct{}{}
	}

	if !cursor.CustomAvailable {
		if cursor.Continue != "" || len(cursor.History) != 0 {
			return fmt.Errorf("invalid tools continue cursor: continuation exists for an empty snapshot")
		}
		return nil
	}
	if cursor.BuiltinOffset < builtinCount {
		if cursor.Continue != "" || len(cursor.History) != 0 {
			return fmt.Errorf("invalid tools continue cursor: custom pagination started before built-ins completed")
		}
		return nil
	}
	if cursor.Continue == "" {
		if len(cursor.History) != 0 {
			return fmt.Errorf("invalid tools continue cursor: continuation history has no current token")
		}
		return nil
	}
	if len(cursor.History) == 0 || cursor.History[len(cursor.History)-1] != hashToolContinuation(cursor.Continue) {
		return fmt.Errorf("invalid tools continue cursor: current continuation is not in history")
	}
	return nil
}

func encodeToolListCursor(cursor toolListCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode tools continue cursor: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	if len(encoded) > maxToolListCursorLength {
		return "", fmt.Errorf("encode tools continue cursor: cursor is too large")
	}
	return encoded, nil
}

func advanceToolListCursor(cursor toolListCursor, nextContinue string) (toolListCursor, error) {
	if nextContinue == "" {
		return toolListCursor{}, fmt.Errorf("advance tools continue cursor: empty continuation")
	}
	nextDigest := hashToolContinuation(nextContinue)
	if slices.Contains(cursor.History, nextDigest) {
		return toolListCursor{}, fmt.Errorf("tools continuation cycle detected")
	}

	cursor.Continue = nextContinue
	cursor.History = append(cursor.History, nextDigest)
	if len(cursor.History) > maxToolCursorHistorySize {
		cursor.History = append([]string(nil), cursor.History[len(cursor.History)-maxToolCursorHistorySize:]...)
	}
	return cursor, nil
}

func hashToolContinuation(continuation string) string {
	sum := sha256.Sum256([]byte(continuation))
	return base64.RawURLEncoding.EncodeToString(sum[:toolCursorDigestBytes])
}

func (h *Handlers) filteredToolListPage(
	c fiber.Ctx,
	namespace string,
	pageSize int,
	builtins []fiber.Map,
	customNames []string,
	cursor toolListCursor,
	items []fiber.Map,
) (ListResponse, error) {
	for len(items) < pageSize && cursor.FilteredOffset < len(customNames) {
		name := customNames[cursor.FilteredOffset]
		cursor.FilteredOffset++
		tool := &corev1alpha1.Tool{}
		err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: name}, tool)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ListResponse{}, fiber.NewError(
				fiber.StatusInternalServerError,
				fmt.Sprintf("failed to get authorized tool: %v", err),
			)
		}
		items = append(items, customToolListItem(tool))
	}

	metadata := ListMeta{}
	more := cursor.BuiltinOffset < len(builtins) || cursor.FilteredOffset < len(customNames)
	if more {
		continuation, err := encodeToolListCursor(cursor)
		if err != nil {
			return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, err.Error())
		}
		metadata.Continue = continuation
	}
	return ListResponse{Items: items, Metadata: metadata}, nil
}

func (h *Handlers) kubernetesToolListPage(
	c fiber.Ctx,
	namespace string,
	pageSize int,
	builtins []fiber.Map,
	cursor toolListCursor,
	items []fiber.Map,
) (ListResponse, error) {
	if len(items) == pageSize {
		if cursor.ResourceVersion == "" {
			resourceVersion, available, err := h.probeCustomToolSnapshot(c.Context(), namespace)
			if err != nil {
				return ListResponse{}, err
			}
			cursor.ResourceVersion = resourceVersion
			cursor.CustomAvailable = available
		}
		more := cursor.BuiltinOffset < len(builtins) || cursor.CustomAvailable
		if !more {
			return ListResponse{Items: items, Metadata: ListMeta{}}, nil
		}
		continuation, err := encodeToolListCursor(cursor)
		if err != nil {
			return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, err.Error())
		}
		return ListResponse{Items: items, Metadata: ListMeta{Continue: continuation}}, nil
	}
	if cursor.ResourceVersion != "" && !cursor.CustomAvailable {
		return ListResponse{Items: items, Metadata: ListMeta{}}, nil
	}

	toolList := &corev1alpha1.ToolList{}
	opts := &client.ListOptions{
		Namespace: namespace,
		Limit:     int64(pageSize - len(items)),
		Continue:  cursor.Continue,
	}
	if cursor.Continue == "" && cursor.ResourceVersion != "" {
		opts.Raw = &metav1.ListOptions{
			ResourceVersion:      cursor.ResourceVersion,
			ResourceVersionMatch: metav1.ResourceVersionMatchExact,
		}
	}
	if err := h.apiReader.List(c.Context(), toolList, opts); err != nil {
		return ListResponse{}, paginationListError("tools", err)
	}

	customItems, filtered, err := customToolListItems(c, h.contextTokenAuthorization, toolList.Items)
	if err != nil {
		return ListResponse{}, err
	}
	items = append(items, customItems...)
	metadata := ListMeta{}
	if !filtered {
		metadata.RemainingItemCount = toolList.RemainingItemCount
	}
	if toolList.Continue == "" {
		return ListResponse{Items: items, Metadata: metadata}, nil
	}
	if toolList.ResourceVersion == "" {
		return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, "tool list response omitted resourceVersion")
	}

	cursor.BuiltinOffset = len(builtins)
	cursor.ResourceVersion = toolList.ResourceVersion
	cursor.CustomAvailable = true
	cursor, err = advanceToolListCursor(cursor, toolList.Continue)
	if err != nil {
		return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	metadata.Continue, err = encodeToolListCursor(cursor)
	if err != nil {
		return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	return ListResponse{Items: items, Metadata: metadata}, nil
}

func (h *Handlers) probeCustomToolSnapshot(ctx context.Context, namespace string) (string, bool, error) {
	probe := &corev1alpha1.ToolList{}
	if err := h.apiReader.List(ctx, probe, &client.ListOptions{Namespace: namespace, Limit: 1}); err != nil {
		return "", false, paginationListError("tools", err)
	}
	if probe.ResourceVersion == "" {
		return "", false, fiber.NewError(fiber.StatusInternalServerError, "tool list response omitted resourceVersion")
	}
	return probe.ResourceVersion, len(probe.Items) > 0 || probe.Continue != "", nil
}

func customToolListItems(
	c fiber.Ctx,
	authz ContextTokenAuthorizationConfig,
	tools []corev1alpha1.Tool,
) ([]fiber.Map, bool, error) {
	items := make([]fiber.Map, 0, len(tools))
	filtered := false
	for i := range tools {
		tool := &tools[i]
		allowed, err := contextTokenAllowsToolMetadata(c, authz, "listTools", tool.Name)
		if err != nil {
			return nil, false, err
		}
		if !allowed {
			filtered = true
			continue
		}
		items = append(items, customToolListItem(tool))
	}
	return items, filtered, nil
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
