/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/contexttoken"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const (
	approvalAuthInjectBody                         = "body"
	approvalIdempotencyHeader                      = "Idempotency-Key"
	approvalAuthRefUIDAnnotation                   = "orka.ai/approval-auth-ref-uid"
	approvalAuthRefResourceVersionAnnotation       = "orka.ai/approval-auth-ref-resource-version"
	legacyApprovalAuthRefUIDAnnotation             = "orka.fibey.io/approval-auth-ref-uid"
	legacyApprovalAuthRefResourceVersionAnnotation = "orka.fibey.io/approval-auth-ref-resource-version"
	approvalTargetURLField                         = "__orkaApprovalURL"
)

var approvalMountRoots = []string{"/secrets/task", "/secrets/agent"}
var approvalToolMountRoot = "/secrets/tools"

type approvalGate struct {
	namespace        string
	taskName         string
	taskUID          string
	required         map[string]struct{}
	resolved         []approvals.ResolvedApproval
	blockingOverflow bool
	refreshTarget    func(context.Context, string, *corev1alpha1.Tool)
	firedKeys        map[string]bool
	recorder         common.EventRecorder
}

type approvalBatchDecision struct {
	result      string
	toolResults []llm.Message
	continueLLM bool
}

func newApprovalGateFromEnv(recorder common.EventRecorder, baseToolCtx *tools.ToolContext) (*approvalGate, error) {
	coordinationEnv := workerenv.ParseCoordinationEnv(os.Getenv)
	required := toolNameSet(coordinationEnv.ApprovalRequiredTools)
	resolved, err := parseResolvedApprovals(os.Getenv(workerenv.ResolvedApprovals))
	if err != nil {
		return nil, err
	}
	resolved, blockingOverflow := splitBlockingApprovalOverflow(resolved)
	namespace, taskName, taskUID := approvalScope(baseToolCtx)
	if len(required) > 0 && strings.TrimSpace(taskUID) == "" {
		return nil, fmt.Errorf("%s is required for approval-required tools", workerenv.TaskUID)
	}
	var refreshTarget func(context.Context, string, *corev1alpha1.Tool)
	if baseToolCtx != nil {
		refreshTarget = baseToolCtx.ApprovalTargetRefresh
	}
	return &approvalGate{
		namespace:        namespace,
		taskName:         taskName,
		taskUID:          taskUID,
		required:         required,
		resolved:         resolved,
		blockingOverflow: blockingOverflow,
		refreshTarget:    refreshTarget,
		firedKeys:        map[string]bool{},
		recorder:         recorder,
	}, nil
}

func approvalScope(baseToolCtx *tools.ToolContext) (string, string, string) {
	namespace := strings.TrimSpace(os.Getenv(workerenv.TaskNamespace))
	taskName := strings.TrimSpace(os.Getenv(workerenv.TaskName))
	taskUID := strings.TrimSpace(os.Getenv(workerenv.TaskUID))
	if baseToolCtx != nil {
		if strings.TrimSpace(baseToolCtx.Namespace) != "" {
			namespace = strings.TrimSpace(baseToolCtx.Namespace)
		}
		if strings.TrimSpace(baseToolCtx.TaskID) != "" {
			taskName = strings.TrimSpace(baseToolCtx.TaskID)
		}
		if strings.TrimSpace(baseToolCtx.TaskUID) != "" {
			taskUID = strings.TrimSpace(baseToolCtx.TaskUID)
		}
	}
	return namespace, taskName, taskUID
}

func toolNameSet(names []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

func parseResolvedApprovals(raw string) ([]approvals.ResolvedApproval, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var resolved []approvals.ResolvedApproval
	if err := json.Unmarshal([]byte(raw), &resolved); err != nil {
		return nil, fmt.Errorf("parse %s: %w", workerenv.ResolvedApprovals, err)
	}
	return resolved, nil
}

func splitBlockingApprovalOverflow(
	resolved []approvals.ResolvedApproval,
) ([]approvals.ResolvedApproval, bool) {
	if len(resolved) == 0 {
		return nil, false
	}
	filtered := make([]approvals.ResolvedApproval, 0, len(resolved))
	blockingOverflow := false
	for _, approval := range resolved {
		if approvals.IsResolvedApprovalBlockingOverflow(approval) {
			blockingOverflow = true
			continue
		}
		filtered = append(filtered, approval)
	}
	return filtered, blockingOverflow
}

func (g *approvalGate) enabled() bool {
	return g != nil && len(g.required) > 0
}

func (g *approvalGate) requiresApproval(toolName string) bool {
	if !g.enabled() {
		return false
	}
	_, ok := g.required[strings.TrimSpace(toolName)]
	return ok
}

func (g *approvalGate) hasResolvedHistoryForTool(toolName string) bool {
	if g == nil {
		return false
	}
	toolName = strings.TrimSpace(toolName)
	for _, decision := range g.resolved {
		if decision.TargetTool == toolName {
			return true
		}
	}
	return false
}

func (g *approvalGate) preScan(
	ctx context.Context,
	calls []llm.ToolCall,
	allowedToolCalls map[string]struct{},
	customTools map[string]*corev1alpha1.Tool,
) (*approvalBatchDecision, error) {
	if g == nil || (!g.enabled() && len(g.resolved) == 0 && !g.blockingOverflow) {
		return nil, nil
	}
	for _, call := range calls {
		toolName := strings.TrimSpace(call.Name)
		requiresApproval := g.requiresApproval(toolName)
		if requiresApproval {
			if _, ok := allowedToolCalls[toolName]; !ok {
				return &approvalBatchDecision{
					continueLLM: true,
					toolResults: approvalValidationBatchToolResults(
						calls,
						call.ID,
						fmt.Errorf("approval-required tool %q is not enabled for this task", toolName),
					),
				}, nil
			}
		}
		target, err := g.targetForCall(ctx, toolName, call.Arguments, customTools[toolName])
		if err != nil {
			if !requiresApproval {
				if g.blockingOverflow && customTools[toolName] != nil {
					return &approvalBatchDecision{
						continueLLM: true,
						toolResults: blockingApprovalOverflowBatchToolResults(calls, call.ID, toolName),
					}, nil
				}
				if g.hasResolvedHistoryForTool(toolName) && customTools[toolName] != nil {
					return &approvalBatchDecision{
						continueLLM: true,
						toolResults: approvalValidationBatchToolResults(calls, call.ID, err),
					}, nil
				}
				continue
			}
			return &approvalBatchDecision{
				continueLLM: true,
				toolResults: approvalValidationBatchToolResults(calls, call.ID, err),
			}, nil
		}
		decision, found := g.resolvedDecision(target)
		if found && decision.Status != approvals.StatusApproved {
			return &approvalBatchDecision{
				continueLLM: true,
				toolResults: deniedBatchToolResults(calls, call.ID, decision),
			}, nil
		}
		if !requiresApproval && !found {
			if staleDecision, stale := g.staleDecisionForTarget(target); stale {
				return &approvalBatchDecision{
					continueLLM: true,
					toolResults: staleApprovalBatchToolResults(calls, call.ID, staleDecision),
				}, nil
			}
			if g.blockingOverflow && customTools[toolName] != nil {
				return &approvalBatchDecision{
					continueLLM: true,
					toolResults: blockingApprovalOverflowBatchToolResults(calls, call.ID, toolName),
				}, nil
			}
			continue
		}
		if found {
			continue
		}
		if err := g.emitApprovalRequest(ctx, target, call.ID); err != nil {
			return nil, err
		}
		return &approvalBatchDecision{
			result: fmt.Sprintf(
				"approval requested for %s (approvalID %s); parked until a human decides",
				target.TargetTool,
				target.ApprovalID,
			),
		}, nil
	}
	return nil, nil
}

func (g *approvalGate) targetForCall(
	ctx context.Context,
	toolName string,
	args json.RawMessage,
	customTool *corev1alpha1.Tool,
) (approvals.ApprovalTarget, error) {
	if customTool != nil && g.refreshTarget != nil {
		g.refreshTarget(ctx, toolName, customTool)
	}
	targetArgs, err := approvalTargetArguments(args, customTool)
	if err != nil {
		return approvals.ApprovalTarget{}, err
	}
	targetSpecDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		return approvals.ApprovalTarget{}, err
	}
	return approvals.NewApprovalTarget(
		g.namespace,
		g.taskName,
		g.taskUID,
		toolName,
		targetArgs,
		fmt.Sprintf("Execute %s", toolName),
		fmt.Sprintf("Human approval is required before executing %s", toolName),
		"warning",
		targetSpecDigest,
	)
}

func approvalTargetArguments(args json.RawMessage, customTool *corev1alpha1.Tool) (json.RawMessage, error) {
	if len(strings.TrimSpace(string(args))) == 0 {
		return args, nil
	}
	var targetArgsObject map[string]json.RawMessage
	if err := json.Unmarshal(args, &targetArgsObject); err != nil || targetArgsObject == nil {
		return nil, fmt.Errorf("target arguments must be a JSON object")
	}
	if _, ok := targetArgsObject[approvalTargetURLField]; ok {
		return nil, fmt.Errorf("target arguments contain reserved %s field", approvalTargetURLField)
	}
	if authBodyKey := approvalAuthBodyKey(customTool); authBodyKey != "" {
		delete(targetArgsObject, authBodyKey)
	}
	if err := approvalApplyURLInterpolationTarget(args, targetArgsObject, customTool); err != nil {
		return nil, err
	}
	out, err := json.Marshal(targetArgsObject)
	if err != nil {
		return nil, fmt.Errorf("sanitize target arguments: %w", err)
	}
	return json.RawMessage(out), nil
}

func approvalApplyURLInterpolationTarget(
	args json.RawMessage,
	targetArgsObject map[string]json.RawMessage,
	customTool *corev1alpha1.Tool,
) error {
	if customTool == nil || customTool.Spec.HTTP == nil || strings.TrimSpace(customTool.Spec.HTTP.URL) == "" {
		return nil
	}
	if customTool.Spec.MCP != nil && customTool.Spec.MCP.SubstrateActor != nil {
		return nil
	}
	if authBodyKey := approvalAuthBodyKey(customTool); authBodyKey != "" {
		if approvalURLUsesPlaceholder(customTool, authBodyKey) {
			return fmt.Errorf(
				"approval-gated tool %q URL must not interpolate body auth key %q",
				customTool.Name,
				authBodyKey,
			)
		}
	}
	params, err := approvalDecodeTargetArgumentValues(args)
	if err != nil {
		return err
	}
	interpolatedParams := map[string]string{}
	for key, val := range params {
		placeholder := "{{" + key + "}}"
		if strings.Contains(customTool.Spec.HTTP.URL, placeholder) {
			interpolatedParams[key] = neturl.PathEscape(fmt.Sprintf("%v", val))
			delete(targetArgsObject, key)
		}
	}
	if len(interpolatedParams) == 0 {
		return nil
	}
	targetURL := map[string]any{
		"template": customTool.Spec.HTTP.URL,
		"params":   interpolatedParams,
	}
	encoded, err := json.Marshal(targetURL)
	if err != nil {
		return fmt.Errorf("sanitize target URL: %w", err)
	}
	targetArgsObject[approvalTargetURLField] = encoded
	return nil
}

func approvalDecodeTargetArgumentValues(args json.RawMessage) (map[string]any, error) {
	var params map[string]any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil || params == nil {
		return nil, fmt.Errorf("target arguments must be a JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("target arguments must be a JSON object")
	}
	return params, nil
}

func approvalAuthBodyKey(customTool *corev1alpha1.Tool) string {
	if customTool == nil || customTool.Spec.HTTP == nil || customTool.Spec.HTTP.AuthSecretRef == nil {
		return ""
	}
	if strings.TrimSpace(customTool.Spec.HTTP.AuthInject) != approvalAuthInjectBody {
		return ""
	}
	return strings.TrimSpace(customTool.Spec.HTTP.AuthBodyKey)
}

func approvalTTSURLIdentity(value string) (string, string) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "", ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return "", ""
	}
	extra := []string{}
	if parsed.User != nil {
		extra = append(extra, "userinfo="+parsed.User.String())
	}
	if parsed.RawQuery != "" {
		extra = append(extra, "query="+parsed.RawQuery)
	}
	if parsed.Fragment != "" {
		extra = append(extra, "fragment="+parsed.Fragment)
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	if len(extra) == 0 {
		return parsed.String(), ""
	}
	sum := sha256.Sum256([]byte(strings.Join(extra, "\n")))
	return parsed.String(), hex.EncodeToString(sum[:])
}

func approvalTargetSpecDigest(customTool *corev1alpha1.Tool) (string, error) {
	if customTool == nil {
		return "", nil
	}
	if err := validateApprovalCustomToolCompatibility(customTool); err != nil {
		return "", err
	}
	uid, resourceVersion := approvalAuthRefVersion(customTool)
	txAuthority, err := approvalTransactionAuthorityIdentity()
	if err != nil {
		return "", err
	}
	if customTool.Spec.MCP == nil || customTool.Spec.MCP.SubstrateActor == nil {
		if uid == "" && resourceVersion == "" && txAuthority == nil {
			digest, err := approvals.TargetSpecDigest(customTool.Spec)
			if err != nil {
				return "", fmt.Errorf("digest tool %q approval target spec: %w", customTool.Name, err)
			}
			return digest, nil
		}
		targetIdentity := struct {
			Spec                   corev1alpha1.ToolSpec              `json:"spec"`
			AuthRefUID             string                             `json:"authRefUID,omitempty"`
			AuthRefResourceVersion string                             `json:"authRefResourceVersion,omitempty"`
			TxAuthority            *approvalTransactionAuthorityShape `json:"txAuthority,omitempty"`
		}{
			Spec:                   customTool.Spec,
			AuthRefUID:             uid,
			AuthRefResourceVersion: resourceVersion,
			TxAuthority:            txAuthority,
		}
		digest, err := approvals.TargetSpecDigest(targetIdentity)
		if err != nil {
			return "", fmt.Errorf("digest tool %q approval target spec: %w", customTool.Name, err)
		}
		return digest, nil
	}
	targetIdentity := struct {
		Spec                   corev1alpha1.ToolSpec              `json:"spec"`
		StatusEndpoint         string                             `json:"statusEndpoint,omitempty"`
		StatusRouteHost        string                             `json:"statusRouteHost,omitempty"`
		AuthRefUID             string                             `json:"authRefUID,omitempty"`
		AuthRefResourceVersion string                             `json:"authRefResourceVersion,omitempty"`
		TxAuthority            *approvalTransactionAuthorityShape `json:"txAuthority,omitempty"`
	}{
		Spec:                   customTool.Spec,
		StatusEndpoint:         strings.TrimSpace(customTool.Status.Endpoint),
		AuthRefUID:             uid,
		AuthRefResourceVersion: resourceVersion,
		TxAuthority:            txAuthority,
	}
	if customTool.Status.Actor != nil {
		targetIdentity.StatusRouteHost = strings.TrimSpace(customTool.Status.Actor.RouteHost)
	}
	digest, err := approvals.TargetSpecDigest(targetIdentity)
	if err != nil {
		return "", fmt.Errorf("digest tool %q approval target spec: %w", customTool.Name, err)
	}
	return digest, nil
}

type approvalTransactionAuthorityShape struct {
	TransactionID              string `json:"transactionID,omitempty"`
	TransactionScope           string `json:"transactionScope,omitempty"`
	TransactionScopes          string `json:"transactionScopes,omitempty"`
	TransactionContextDigest   string `json:"transactionContextDigest,omitempty"`
	TransactionRequesterDigest string `json:"transactionRequesterDigest,omitempty"`
	KontxtOutboundScope        string `json:"kontxtOutboundScope,omitempty"`
	KontxtTTSURL               string `json:"kontxtTTSURL,omitempty"`
	KontxtTTSURLExtraSHA256    string `json:"kontxtTTSURLExtraSHA256,omitempty"`
	KontxtTTSAudience          string `json:"kontxtTTSAudience,omitempty"`
	KontxtTTSSource            string `json:"kontxtTTSSource,omitempty"`
	KontxtSubjectType          string `json:"kontxtSubjectType,omitempty"`
	KontxtToolTTL              string `json:"kontxtToolTTL,omitempty"`
	MountedAuthoritySHA256     string `json:"mountedAuthoritySHA256,omitempty"`
}

func approvalTransactionAuthorityIdentity() (*approvalTransactionAuthorityShape, error) {
	ttsURL := strings.TrimSpace(os.Getenv(workerenv.ContextTokenTTSURL))
	ttsSource := strings.TrimSpace(os.Getenv(workerenv.ContextTokenTTSTokenSource))
	if ttsURL != "" && ttsSource == "" {
		ttsSource = contexttoken.TTSTokenSourceServiceAccount
	}
	subjectType := strings.TrimSpace(os.Getenv(workerenv.ContextTokenSubjectTokenType))
	if ttsURL != "" && subjectType == "" {
		subjectType = contexttoken.SubjectTokenTypeForSource(ttsSource)
	}
	ttsSafeURL, ttsURLExtraDigest := approvalTTSURLIdentity(ttsURL)
	identity := &approvalTransactionAuthorityShape{
		TransactionID:              strings.TrimSpace(os.Getenv(workerenv.TransactionID)),
		TransactionScope:           strings.TrimSpace(os.Getenv(workerenv.TransactionScope)),
		TransactionScopes:          strings.TrimSpace(os.Getenv(workerenv.TransactionScopes)),
		TransactionContextDigest:   strings.TrimSpace(os.Getenv(workerenv.TransactionContextDigest)),
		TransactionRequesterDigest: strings.TrimSpace(os.Getenv(workerenv.TransactionRequesterContextDigest)),
		KontxtOutboundScope:        strings.TrimSpace(os.Getenv(workerenv.ContextTokenOutboundScope)),
		KontxtTTSURL:               ttsSafeURL,
		KontxtTTSURLExtraSHA256:    ttsURLExtraDigest,
		KontxtTTSAudience:          strings.TrimSpace(os.Getenv(workerenv.ContextTokenTTSAudience)),
		KontxtTTSSource:            ttsSource,
		KontxtSubjectType:          subjectType,
		KontxtToolTTL:              strings.TrimSpace(os.Getenv(workerenv.ContextTokenToolTokenTTL)),
	}
	if path := strings.TrimSpace(os.Getenv(workerenv.TransactionTokenFile)); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read mounted transaction authority: %w", err)
		}
		sum := sha256.Sum256(bytes.TrimSpace(data))
		identity.MountedAuthoritySHA256 = hex.EncodeToString(sum[:])
	}
	if *identity == (approvalTransactionAuthorityShape{}) {
		return nil, nil
	}
	return identity, nil
}

func validateApprovalCustomToolCompatibility(customTool *corev1alpha1.Tool) error {
	if customTool == nil || customTool.Spec.HTTP == nil {
		return nil
	}
	usesBodyAuth := customTool.Spec.HTTP.AuthSecretRef != nil &&
		strings.TrimSpace(customTool.Spec.HTTP.AuthInject) == approvalAuthInjectBody
	if usesBodyAuth {
		if customTool.Spec.MCP != nil && customTool.Spec.MCP.SubstrateActor != nil {
			return fmt.Errorf("approval-gated MCP tool %q does not support body auth", customTool.Name)
		}
		authBodyKey := customTool.Spec.HTTP.AuthBodyKey
		trimmedAuthBodyKey := strings.TrimSpace(authBodyKey)
		if trimmedAuthBodyKey == "" {
			return fmt.Errorf("approval-gated tool %q authBodyKey is required for body auth", customTool.Name)
		}
		if authBodyKey != trimmedAuthBodyKey {
			return fmt.Errorf("approval-gated tool %q authBodyKey must not contain surrounding whitespace", customTool.Name)
		}
		if approvalURLUsesPlaceholder(customTool, customTool.Spec.HTTP.AuthBodyKey) {
			return fmt.Errorf(
				"approval-gated tool %q URL must not interpolate body auth key %q",
				customTool.Name,
				strings.TrimSpace(customTool.Spec.HTTP.AuthBodyKey),
			)
		}
	}
	for header := range customTool.Spec.HTTP.Headers {
		if strings.EqualFold(strings.TrimSpace(header), contexttoken.HeaderName) {
			return fmt.Errorf(
				"approval-gated tool %q must not set reserved header %q",
				customTool.Name,
				contexttoken.HeaderName,
			)
		}
		if strings.EqualFold(strings.TrimSpace(header), approvalIdempotencyHeader) {
			return fmt.Errorf(
				"approval-gated tool %q must not set reserved header %q",
				customTool.Name,
				approvalIdempotencyHeader,
			)
		}
	}
	if customTool.Spec.HTTP.AuthSecretRef != nil {
		if approvalMountedCredentialExists(customTool.Spec.HTTP.AuthSecretRef.Name, customTool.Spec.HTTP.AuthSecretRef.Key) {
			return fmt.Errorf(
				"approval-gated tool %q mounted credential source cannot be approval-bound",
				customTool.Name,
			)
		}
		uid, resourceVersion := approvalAuthRefVersion(customTool)
		if uid == "" || resourceVersion == "" {
			return fmt.Errorf("approval-gated tool %q auth secret version is not available", customTool.Name)
		}
	}
	return nil
}

func approvalURLUsesPlaceholder(customTool *corev1alpha1.Tool, key string) bool {
	if customTool == nil || customTool.Spec.HTTP == nil {
		return false
	}
	key = strings.TrimSpace(key)
	return key != "" && strings.Contains(customTool.Spec.HTTP.URL, "{{"+key+"}}")
}

func approvalMountedCredentialExists(secretName, key string) bool {
	secretName = strings.TrimSpace(secretName)
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	for _, root := range approvalMountRoots {
		if _, err := os.Stat(filepath.Join(root, key)); err == nil {
			return true
		}
	}
	if secretName != "" && strings.TrimSpace(approvalToolMountRoot) != "" {
		if _, err := os.Stat(filepath.Join(approvalToolMountRoot, secretName, key)); err == nil {
			return true
		}
	}
	return false
}

func approvalAuthRefVersion(customTool *corev1alpha1.Tool) (string, string) {
	if customTool == nil || customTool.Spec.HTTP == nil || customTool.Spec.HTTP.AuthSecretRef == nil {
		return "", ""
	}
	uid := strings.TrimSpace(customTool.Annotations[approvalAuthRefUIDAnnotation])
	resourceVersion := strings.TrimSpace(customTool.Annotations[approvalAuthRefResourceVersionAnnotation])
	if uid != "" || resourceVersion != "" {
		return uid, resourceVersion
	}
	return strings.TrimSpace(customTool.Annotations[legacyApprovalAuthRefUIDAnnotation]),
		strings.TrimSpace(customTool.Annotations[legacyApprovalAuthRefResourceVersionAnnotation])
}

func approvalTargetSpecDigestFromCustomTools(
	customTools map[string]*corev1alpha1.Tool,
	refresh ...func(context.Context, string, *corev1alpha1.Tool),
) func(context.Context, string) (string, error) {
	return func(ctx context.Context, targetTool string) (string, error) {
		targetTool = strings.TrimSpace(targetTool)
		customTool := customTools[targetTool]
		if customTool == nil {
			return "", fmt.Errorf("targetTool %q is not an enabled custom tool", targetTool)
		}
		if len(refresh) > 0 && refresh[0] != nil {
			refresh[0](ctx, targetTool, customTool)
		}
		return approvalTargetSpecDigest(customTool)
	}
}

func approvalTargetArgumentsFromCustomTools(
	customTools map[string]*corev1alpha1.Tool,
) func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return func(_ context.Context, targetTool string, args json.RawMessage) (json.RawMessage, error) {
		return approvalTargetArguments(args, customTools[strings.TrimSpace(targetTool)])
	}
}

func (g *approvalGate) resolvedDecision(target approvals.ApprovalTarget) (approvals.ResolvedApproval, bool) {
	for _, decision := range g.resolved {
		if decision.ID != target.ApprovalID {
			continue
		}
		if resolvedDecisionMatchesTarget(decision, target) {
			return decision, true
		}
	}
	legacyID := approvals.ApprovalID(
		g.namespace,
		g.taskName,
		"",
		target.TargetTool,
		target.TargetArgsDigest,
		target.TargetSpecDigest,
	)
	for _, decision := range g.resolved {
		if decision.TaskUID != "" || decision.ID != legacyID {
			continue
		}
		if resolvedDecisionMatchesTarget(decision, target) {
			return decision, true
		}
	}
	return approvals.ResolvedApproval{}, false
}

func resolvedDecisionMatchesTarget(decision approvals.ResolvedApproval, target approvals.ApprovalTarget) bool {
	if decision.TaskUID != "" && decision.TaskUID != target.TaskUID {
		return false
	}
	return decision.TargetTool == target.TargetTool &&
		decision.TargetArgsDigest == target.TargetArgsDigest &&
		decision.TargetSpecDigest == target.TargetSpecDigest
}

func (g *approvalGate) staleDecisionForTarget(target approvals.ApprovalTarget) (approvals.ResolvedApproval, bool) {
	for _, decision := range g.resolved {
		if decision.TaskUID != "" && decision.TaskUID != target.TaskUID {
			continue
		}
		if decision.TargetTool != target.TargetTool || decision.TargetArgsDigest != target.TargetArgsDigest {
			continue
		}
		if target.TargetSpecDigest != "" && decision.TargetSpecDigest != target.TargetSpecDigest {
			return decision, true
		}
	}
	return approvals.ResolvedApproval{}, false
}

func deniedBatchToolResults(
	calls []llm.ToolCall,
	deniedToolCallID string,
	decision approvals.ResolvedApproval,
) []llm.Message {
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		content := fmt.Sprintf(
			"Not executed because approval %s for %s is %s",
			decision.ID,
			decision.TargetTool,
			decision.Status,
		)
		if call.ID != deniedToolCallID {
			content = fmt.Sprintf(
				"Not executed because the same tool-call batch contained denied approval %s for %s",
				decision.ID,
				decision.TargetTool,
			)
		}
		results = append(results, llm.Message{Role: "tool", Content: content, ToolCallID: call.ID, Name: call.Name})
	}
	return results
}

func (g *approvalGate) emitApprovalRequest(
	ctx context.Context,
	target approvals.ApprovalTarget,
	modelToolCallID string,
) error {
	return emitApprovalRequest(ctx, g.recorder, target, modelToolCallID)
}

func emitApprovalRequest(
	ctx context.Context,
	recorder common.EventRecorder,
	target approvals.ApprovalTarget,
	modelToolCallID string,
) error {
	content, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("marshal approval request: %w", err)
	}
	return common.RecordEventStrict(ctx, recorder, events.ExecutionEventTypeApprovalRequested,
		common.WithEventSeverity(events.ExecutionEventSeverityWarning),
		common.WithEventToolName(target.TargetTool),
		common.WithEventToolCallID(target.ApprovalID),
		common.WithEventSummary(target.Action),
		common.WithEventContent(json.RawMessage(content)),
		common.WithEventContentText(fmt.Sprintf(
			"approval requested for model tool call %s",
			strings.TrimSpace(modelToolCallID),
		)),
	)
}

func approvalEmitterFromRecorder(recorder common.EventRecorder) func(context.Context, approvals.ApprovalTarget) error {
	return func(ctx context.Context, target approvals.ApprovalTarget) error {
		return emitApprovalRequest(ctx, recorder, target, "")
	}
}

func (g *approvalGate) prepareApprovedCall(
	ctx context.Context,
	toolName string,
	args json.RawMessage,
	customTool *corev1alpha1.Tool,
) (json.RawMessage, string, bool, error) {
	requiresApproval := g.requiresApproval(toolName)
	if g == nil {
		return args, "", false, nil
	}
	if !requiresApproval && len(g.resolved) == 0 && !g.blockingOverflow {
		return args, "", false, nil
	}
	target, err := g.targetForCall(ctx, toolName, args, customTool)
	if err != nil {
		if !requiresApproval {
			if g.blockingOverflow && customTool != nil {
				return nil, "", false, fmt.Errorf(
					"approval history omitted blocking decisions; request approval again before executing %s",
					strings.TrimSpace(toolName),
				)
			}
			if g.hasResolvedHistoryForTool(toolName) && customTool != nil {
				return nil, "", false, err
			}
			return args, "", false, nil
		}
		return nil, "", false, err
	}
	decision, found := g.resolvedDecision(target)
	if found {
		if decision.Status != approvals.StatusApproved {
			return nil, "", false, fmt.Errorf("approval %s for %s is %s", decision.ID, decision.TargetTool, decision.Status)
		}
		if g.firedKeys[target.ApprovalID] {
			return nil, target.ApprovalID, true, nil
		}
		return args, target.ApprovalID, false, nil
	}
	if !requiresApproval {
		if decision, stale := g.staleDecisionForTarget(target); stale {
			return nil, "", false, fmt.Errorf(
				"approval %s for %s no longer matches the current tool spec; request approval again",
				decision.ID,
				decision.TargetTool,
			)
		}
		if g.blockingOverflow && customTool != nil {
			return nil, "", false, fmt.Errorf(
				"approval history omitted blocking decisions; request approval again before executing %s",
				strings.TrimSpace(toolName),
			)
		}
		return args, "", false, nil
	}
	return nil, "", false, fmt.Errorf("approval %s for %s is not resolved", target.ApprovalID, target.TargetTool)
}

func (g *approvalGate) markFired(key string) {
	if g == nil || strings.TrimSpace(key) == "" {
		return
	}
	g.firedKeys[key] = true
}

func injectIdempotencyKey(args json.RawMessage, key string) (json.RawMessage, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return args, nil
	}
	params := map[string]any{}
	if len(strings.TrimSpace(string(args))) > 0 {
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, fmt.Errorf("parse tool arguments for idempotency key injection: %w", err)
		}
	}
	if existing, ok := params["idempotencyKey"]; ok {
		existingString := strings.TrimSpace(fmt.Sprint(existing))
		if existingString != "" && existingString != key {
			return nil, fmt.Errorf("tool arguments contain conflicting reserved idempotencyKey")
		}
	}
	params["idempotencyKey"] = key
	out, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal tool arguments with idempotency key: %w", err)
	}
	return json.RawMessage(out), nil
}

func formatResolvedApprovalsContext(resolved []approvals.ResolvedApproval) string {
	if len(resolved) == 0 {
		return ""
	}
	resolved, _ = splitBlockingApprovalOverflow(resolved)
	if len(resolved) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Resolved Human Approvals\n\n")
	for _, approval := range resolved {
		status := strings.ToUpper(strings.TrimSpace(approval.Status))
		if status == "" {
			status = "RESOLVED"
		}
		sb.WriteString("- ")
		sb.WriteString(status)
		sb.WriteString(" ")
		sb.WriteString(approval.ID)
		if approval.TargetTool != "" {
			sb.WriteString(" for ")
			sb.WriteString(approval.TargetTool)
		}
		if approval.Actor != "" {
			sb.WriteString(" by ")
			sb.WriteString(approval.Actor)
		}
		if approval.DecisionTime != "" {
			sb.WriteString(" at ")
			sb.WriteString(approval.DecisionTime)
		}
		if len(approval.TargetArgsPreview) > 0 {
			sb.WriteString(" args=")
			sb.WriteString(strings.TrimSpace(string(approval.TargetArgsPreview)))
		}
		if approval.Reason != "" {
			sb.WriteString(": ")
			sb.WriteString(approval.Reason)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func prepareApprovalToolContext(baseToolCtx *tools.ToolContext, recorder common.EventRecorder) *tools.ToolContext {
	if baseToolCtx == nil {
		return nil
	}
	if baseToolCtx.ApprovalEmitter != nil && baseToolCtx.TaskUID != "" {
		return baseToolCtx
	}
	baseToolCtxCopy := *baseToolCtx
	if baseToolCtxCopy.ApprovalEmitter == nil {
		baseToolCtxCopy.ApprovalEmitter = approvalEmitterFromRecorder(recorder)
	}
	if baseToolCtxCopy.TaskUID == "" {
		baseToolCtxCopy.TaskUID = os.Getenv(workerenv.TaskUID)
	}
	if baseToolCtxCopy.ApprovalTargetRefresh == nil && baseToolCtxCopy.Client != nil {
		baseToolCtxCopy.ApprovalTargetRefresh = func(ctx context.Context, _ string, tool *corev1alpha1.Tool) {
			bindApprovalAuthRefVersion(ctx, baseToolCtxCopy.Client, baseToolCtxCopy.Namespace, tool)
		}
	}
	return &baseToolCtxCopy
}

func handleExplicitRequestApprovalBatch(
	ctx context.Context,
	calls []llm.ToolCall,
	gate *approvalGate,
	allowedToolCalls map[string]struct{},
	customTools map[string]*corev1alpha1.Tool,
	eventRecorder common.EventRecorder,
	baseToolCtx *tools.ToolContext,
) (*approvalBatchDecision, error) {
	for _, call := range calls {
		if strings.TrimSpace(call.Name) != "request_approval" {
			continue
		}
		if _, ok := allowedToolCalls["request_approval"]; !ok {
			return nil, nil
		}
		if gate != nil && len(gate.resolved) > 0 {
			if target, err := explicitApprovalTargetForCall(ctx, call, customTools, baseToolCtx); err == nil {
				if decision, found := gate.resolvedDecision(target); found {
					return &approvalBatchDecision{
						continueLLM: true,
						toolResults: terminalApprovalBatchToolResults(calls, call.ID, decision),
					}, nil
				}
			}
		}
		result, err := executeRequestApprovalToolCall(ctx, call, customTools, eventRecorder, baseToolCtx)
		if err != nil {
			var validationErr *tools.RequestApprovalValidationError
			if errors.As(err, &validationErr) {
				return &approvalBatchDecision{
					continueLLM: true,
					toolResults: approvalValidationBatchToolResults(calls, call.ID, err),
				}, nil
			}
			return nil, err
		}
		return &approvalBatchDecision{result: result}, nil
	}
	return nil, nil
}

func blockingApprovalOverflowBatchToolResults(
	calls []llm.ToolCall,
	blockedToolCallID string,
	toolName string,
) []llm.Message {
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		content := fmt.Sprintf(
			"Not executed because approval history omitted blocking decisions; request approval again before executing %s",
			toolName,
		)
		if call.ID != blockedToolCallID {
			content = fmt.Sprintf(
				"Not executed because the same tool-call batch contained approval-history overflow for %s",
				toolName,
			)
		}
		results = append(results, llm.Message{Role: "tool", Content: content, ToolCallID: call.ID, Name: call.Name})
	}
	return results
}

func staleApprovalBatchToolResults(
	calls []llm.ToolCall,
	staleToolCallID string,
	decision approvals.ResolvedApproval,
) []llm.Message {
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		content := fmt.Sprintf(
			"Not executed because approval %s for %s no longer matches the current tool spec; request approval again",
			decision.ID,
			decision.TargetTool,
		)
		if call.ID != staleToolCallID {
			content = fmt.Sprintf(
				"Not executed because the same tool-call batch contained stale approval %s for %s",
				decision.ID,
				decision.TargetTool,
			)
		}
		results = append(results, llm.Message{Role: "tool", Content: content, ToolCallID: call.ID, Name: call.Name})
	}
	return results
}

func approvalValidationBatchToolResults(
	calls []llm.ToolCall,
	invalidToolCallID string,
	err error,
) []llm.Message {
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		content := err.Error()
		if call.ID != invalidToolCallID {
			content = fmt.Sprintf(
				"Not executed because the same tool-call batch contained invalid request_approval call %s",
				invalidToolCallID,
			)
		}
		results = append(results, llm.Message{Role: "tool", Content: content, ToolCallID: call.ID, Name: call.Name})
	}
	return results
}

type requestApprovalCallArgs struct {
	Action          string          `json:"action"`
	RiskSummary     string          `json:"riskSummary"`
	Severity        string          `json:"severity"`
	TargetTool      string          `json:"targetTool"`
	TargetArguments json.RawMessage `json:"targetArguments"`
}

func explicitApprovalTargetForCall(
	ctx context.Context,
	call llm.ToolCall,
	customTools map[string]*corev1alpha1.Tool,
	baseToolCtx *tools.ToolContext,
) (approvals.ApprovalTarget, error) {
	var req requestApprovalCallArgs
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &req); err != nil {
			return approvals.ApprovalTarget{}, err
		}
	}
	baseToolCtx = prepareApprovalToolContext(baseToolCtx, nil)
	if baseToolCtx == nil {
		baseToolCtx = tools.GetToolContext(ctx)
	}
	if baseToolCtx == nil {
		return approvals.ApprovalTarget{}, fmt.Errorf("tool context is not configured")
	}
	targetTool := strings.TrimSpace(req.TargetTool)
	targetSpecDigest, err := approvalTargetSpecDigestFromCustomTools(
		customTools,
		baseToolCtx.ApprovalTargetRefresh,
	)(ctx, targetTool)
	if err != nil {
		return approvals.ApprovalTarget{}, err
	}
	targetArguments, err := approvalTargetArgumentsFromCustomTools(customTools)(ctx, targetTool, req.TargetArguments)
	if err != nil {
		return approvals.ApprovalTarget{}, err
	}
	return approvals.NewApprovalTarget(
		baseToolCtx.Namespace,
		baseToolCtx.TaskID,
		baseToolCtx.TaskUID,
		targetTool,
		targetArguments,
		req.Action,
		req.RiskSummary,
		req.Severity,
		targetSpecDigest,
	)
}

func terminalApprovalBatchToolResults(
	calls []llm.ToolCall,
	terminalToolCallID string,
	decision approvals.ResolvedApproval,
) []llm.Message {
	if decision.Status != approvals.StatusApproved {
		return deniedBatchToolResults(calls, terminalToolCallID, decision)
	}
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		content := fmt.Sprintf("approval %s for %s is already approved", decision.ID, decision.TargetTool)
		if call.ID != terminalToolCallID {
			content = fmt.Sprintf(
				"Not executed because the same tool-call batch contained terminal approval %s for %s",
				decision.ID,
				decision.TargetTool,
			)
		}
		results = append(results, llm.Message{Role: "tool", Content: content, ToolCallID: call.ID, Name: call.Name})
	}
	return results
}

func executeRequestApprovalToolCall(
	ctx context.Context,
	call llm.ToolCall,
	customTools map[string]*corev1alpha1.Tool,
	eventRecorder common.EventRecorder,
	baseToolCtx *tools.ToolContext,
) (string, error) {
	toolName := strings.TrimSpace(call.Name)
	common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallStarted, modelLoopEventTimeout,
		common.WithEventToolName(toolName),
		common.WithEventToolCallID(call.ID),
		common.WithEventSummary("tool call started"),
		common.WithEventContent(eventContent(map[string]any{
			"toolName":      toolName,
			"toolCallID":    call.ID,
			"argumentBytes": len(call.Arguments),
		})),
	)

	execCtx := ctx
	baseToolCtx = prepareApprovalToolContext(baseToolCtx, eventRecorder)
	if baseToolCtx != nil {
		toolCtxCopy := *baseToolCtx
		toolCtxCopy.ToolCallID = call.ID
		if toolCtxCopy.Tenant == "" {
			toolCtxCopy.Tenant = toolCtxCopy.Namespace
		}
		if toolCtxCopy.ApprovalTargetSpecDigest == nil {
			toolCtxCopy.ApprovalTargetSpecDigest = approvalTargetSpecDigestFromCustomTools(
				customTools,
				toolCtxCopy.ApprovalTargetRefresh,
			)
		}
		if toolCtxCopy.ApprovalTargetArguments == nil {
			toolCtxCopy.ApprovalTargetArguments = approvalTargetArgumentsFromCustomTools(customTools)
		}
		execCtx = tools.WithToolContext(ctx, &toolCtxCopy)
	}
	result, err := tools.DefaultRegistry.Execute(execCtx, toolName, call.Arguments)
	if err != nil {
		common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallFailed, modelLoopEventTimeout,
			common.WithEventSeverity(events.ExecutionEventSeverityError),
			common.WithEventToolName(toolName),
			common.WithEventToolCallID(call.ID),
			common.WithEventSummary(err.Error()),
		)
		return "", err
	}
	common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallCompleted, modelLoopEventTimeout,
		common.WithEventToolName(toolName),
		common.WithEventToolCallID(call.ID),
		common.WithEventSummary("tool call completed"),
		common.WithEventContent(eventContent(map[string]any{
			"toolName":     toolName,
			"toolCallID":   call.ID,
			"resultLength": len(result),
		})),
	)
	return result, nil
}

func processApprovalBatch(
	ctx context.Context,
	messages []llm.Message,
	calls []llm.ToolCall,
	gate *approvalGate,
	allowedToolCalls map[string]struct{},
	customTools map[string]*corev1alpha1.Tool,
	eventRecorder common.EventRecorder,
	baseToolCtx *tools.ToolContext,
) ([]llm.Message, string, bool, bool, error) {
	if decision, err := gate.preScan(ctx, calls, allowedToolCalls, customTools); err != nil {
		return messages, "", false, false, err
	} else if decision != nil {
		return applyApprovalBatchDecision(messages, decision)
	}
	if decision, err := handleExplicitRequestApprovalBatch(
		ctx,
		calls,
		gate,
		allowedToolCalls,
		customTools,
		eventRecorder,
		baseToolCtx,
	); err != nil {
		return messages, "", false, false, err
	} else if decision != nil {
		return applyApprovalBatchDecision(messages, decision)
	}
	return messages, "", false, false, nil
}

func applyApprovalBatchDecision(
	messages []llm.Message,
	decision *approvalBatchDecision,
) ([]llm.Message, string, bool, bool, error) {
	if decision.result != "" {
		return messages, decision.result, true, false, nil
	}
	if len(decision.toolResults) > 0 {
		messages = append(messages, decision.toolResults...)
	}
	return messages, "", false, decision.continueLLM, nil
}
