/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/connectors"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/taskmeta"
)

const (
	contextTaskTypeKey         = "taskType"
	contextAgentKey            = "agent"
	contextAllowedAgentsKey    = "allowedAgents"
	contextRepoKey             = "repo"
	contextBranchKey           = "branch"
	contextRefKey              = "ref"
	contextAllowedToolsKey     = "allowedTools"
	contextAllowedProvidersKey = "allowedProviders"
	contextAllowedModelsKey    = "allowedModels"
)

var safeTransactionContextKeys = []string{
	"purpose",
	toolNamespaceArg,
	contextTaskTypeKey,
	contextAgentKey,
	contextAllowedAgentsKey,
	contextRepoKey,
	contextBranchKey,
	contextRefKey,
	"maxDepth",
	contextAllowedToolsKey,
	chatProviderKey,
	contextAllowedProvidersKey,
	chatModelKey,
	contextAllowedModelsKey,
	"e2e",
	"trace_id",
	"secret",
}

const maxSafeTransactionContextValueLength = 1024

var setValuedContextDigestKeys = map[string]struct{}{
	contextAllowedAgentsKey:    {},
	contextAllowedModelsKey:    {},
	contextAllowedProvidersKey: {},
	contextAllowedToolsKey:     {},
}

var authorizationTransactionContextKeys = map[string]struct{}{
	toolNamespaceArg:           {},
	contextTaskTypeKey:         {},
	contextAgentKey:            {},
	contextAllowedAgentsKey:    {},
	contextRepoKey:             {},
	contextBranchKey:           {},
	contextRefKey:              {},
	"maxDepth":                 {},
	contextAllowedToolsKey:     {},
	chatProviderKey:            {},
	contextAllowedProvidersKey: {},
	chatModelKey:               {},
	contextAllowedModelsKey:    {},
}

// requesterStampKey seals the requester stamp onto Tasks the API creates.
// Without it, stamped Tasks stay unverified for connector use (fail closed).
var requesterStampKey []byte

// SetRequesterStampKey installs the key the API seals requester stamps with.
func SetRequesterStampKey(key []byte) {
	requesterStampKey = append([]byte(nil), key...)
}

// sealRequesterStamp binds a just-created, API-stamped Task's UID to its
// requester. A failure leaves the Task unverified for connector use rather
// than failing the creation; the Task itself is intact.
func sealRequesterStamp(ctx context.Context, c client.Client, task *corev1alpha1.Task) {
	if task == nil || task.Annotations[labels.AnnotationRequestedBySource] != labels.RequestedBySourceAPI {
		return
	}
	if len(requesterStampKey) == 0 {
		// Connectors are disabled: nothing verifies stamps, so none is sealed.
		return
	}
	// A transient write failure is retried briefly: the seal is the only
	// thing that lets the controller trust the requester, and nothing else
	// repairs it once the creation has been reported.
	backoff := requesterStampSealBackoff
	var err error
	for attempt := range requesterStampSealAttempts {
		if err = connectors.SealRequesterStamp(ctx, c, requesterStampKey, task.DeepCopy()); err == nil {
			return
		}
		if attempt+1 == requesterStampSealAttempts {
			break
		}
		select {
		case <-ctx.Done():
			log.Error(err, "requester stamp could not be sealed before the request ended; the task stays unverified for connector use", "task", task.Name, "namespace", task.Namespace)
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	log.Error(err, "requester stamp could not be sealed; the task stays unverified for connector use", "task", task.Name, "namespace", task.Namespace)
}

// requesterStampSealAttempts and requesterStampSealBackoff bound the retries
// of the post-create seal; tests shorten the backoff.
var (
	requesterStampSealAttempts = 3
	requesterStampSealBackoff  = 200 * time.Millisecond
)

// requesterStampSealer is the tool-side hook that seals Tasks created by
// chat and compatibility tools on the API's behalf.
func requesterStampSealer(ctx context.Context, c client.Client, task *corev1alpha1.Task) error {
	sealRequesterStamp(ctx, c, task)
	return nil
}

// requesterFromUserInfo returns the verified person behind ui as a
// requester identity, or nil when the caller has no personal identity
// (ServiceAccount tokens, anonymous callers).
func requesterFromUserInfo(ui *UserInfo) *corev1alpha1.RequestedBy {
	if ui == nil || (ui.AuthType != AuthTypeOIDC && ui.AuthType != AuthTypeContextToken) ||
		strings.TrimSpace(ui.Issuer) == "" || strings.TrimSpace(ui.Subject) == "" {
		return nil
	}
	return &corev1alpha1.RequestedBy{
		Subject: ui.Subject, Issuer: ui.Issuer, Username: ui.Username, Email: ui.Email,
		Groups: append([]string{}, ui.Groups...), Roles: append([]string{}, ui.Roles...),
	}
}

func stampTaskRequesterFromUserInfo(task *corev1alpha1.Task, ui *UserInfo) {
	if task == nil || ui == nil || (ui.AuthType != AuthTypeOIDC && ui.AuthType != AuthTypeContextToken) {
		return
	}

	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	// Only controller identities may write this annotation, so it proves the
	// requester below came from a verified sign-in rather than a worker.
	task.Annotations[labels.AnnotationRequestedBySource] = labels.RequestedBySourceAPI
	task.Spec.RequestedBy = &corev1alpha1.RequestedBy{
		Subject:  ui.Subject,
		Issuer:   ui.Issuer,
		Username: ui.Username,
		Email:    ui.Email,
		Groups:   append([]string{}, ui.Groups...),
		Roles:    append([]string{}, ui.Roles...),
	}

	if ui.AuthType == AuthTypeContextToken {
		task.Spec.Transaction = taskTransactionFromContextToken(ui.ContextToken)
		taskmeta.ApplyTransactionMetadata(&task.ObjectMeta, task.Spec.Transaction)
	}
}

func taskTransactionFromContextToken(token *ContextToken) *corev1alpha1.TaskTransaction {
	if token == nil {
		return nil
	}

	return &corev1alpha1.TaskTransaction{
		Profile:                token.Profile,
		ID:                     token.TransactionID,
		Issuer:                 token.Issuer,
		Audience:               append([]string{}, token.Audience...),
		Subject:                token.Subject,
		RequestingWorkload:     token.RequestingWorkload,
		Scope:                  token.Scope,
		Scopes:                 append([]string{}, token.Scopes...),
		ContextDigest:          digestMap(token.TransactionContext),
		RequesterContextDigest: digestMap(token.RequesterContext),
		Context:                safeTransactionContext(token.TransactionContext),
	}
}

func digestMap(value map[string]any) string {
	if len(value) == 0 {
		return ""
	}
	encoded, err := json.Marshal(canonicalizeContextDigestValue("", value))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func canonicalizeContextDigestValue(key string, value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for childKey, childValue := range v {
			out[childKey] = canonicalizeContextDigestValue(childKey, childValue)
		}
		return out
	case []any:
		return canonicalizeContextDigestList(key, v)
	case []string:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, item)
		}
		return canonicalizeContextDigestList(key, out)
	default:
		return value
	}
}

func canonicalizeContextDigestList(key string, values []any) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, canonicalizeContextDigestValue(key, value))
	}
	if _, ok := setValuedContextDigestKeys[key]; !ok {
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		return contextDigestSortKey(out[i]) < contextDigestSortKey(out[j])
	})
	return out
}

func contextDigestSortKey(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func safeTransactionContext(value map[string]any) map[string]string {
	if len(value) == 0 {
		return nil
	}

	out := map[string]string{}
	for _, key := range safeTransactionContextKeys {
		raw, ok := value[key]
		if !ok {
			continue
		}
		if rendered, ok := renderSafeTransactionContextValue(key, raw); ok && safeTransactionContextValueAllowed(key, rendered) {
			out[key] = rendered
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func renderSafeTransactionContextValue(key string, value any) (string, bool) {
	switch v := value.(type) {
	case string:
		if v == "" {
			return "", false
		}
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		return v.String(), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case []string:
		if len(v) == 0 && !safeTransactionContextPreservesEmptyList(key) {
			return "", false
		}
		encoded, err := json.Marshal(v)
		return string(encoded), err == nil
	case []any:
		if len(v) == 0 {
			if safeTransactionContextPreservesEmptyList(key) {
				return "[]", true
			}
			return "", false
		}
		if !safeScalarList(v) {
			return "", false
		}
		encoded, err := json.Marshal(v)
		return string(encoded), err == nil
	default:
		return "", false
	}
}

func safeTransactionContextPreservesEmptyList(key string) bool {
	_, ok := setValuedContextDigestKeys[key]
	return ok
}

func safeTransactionContextValueAllowed(key, rendered string) bool {
	if len(rendered) <= maxSafeTransactionContextValueLength {
		return true
	}
	_, ok := authorizationTransactionContextKeys[key]
	return ok
}

func safeScalarList(values []any) bool {
	return slices.IndexFunc(values, func(value any) bool {
		switch value.(type) {
		case string, bool, json.Number, float64, int, int64:
			return false
		default:
			return true
		}
	}) == -1
}
