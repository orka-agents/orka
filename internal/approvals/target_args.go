// Copyright Orka Contributors.
// SPDX-License-Identifier: Apache-2.0

package approvals

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	neturl "net/url"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	// AuthInjectBody is the HTTP tool auth injection mode that places the
	// credential in the request body.
	AuthInjectBody = "body"
	// TargetURLField is the reserved target-argument field that carries the
	// interpolated URL identity of a templated HTTP tool.
	TargetURLField = "__orkaApprovalURL"
)

// TargetArguments returns the approval-bound view of a tool call's arguments:
// the body auth key is removed, and parameters interpolated into a templated
// URL are folded into the reserved TargetURLField together with the template.
// The worker and the controller both digest this view, so an approval granted
// for one call binds exactly the request the worker asks the controller to
// execute.
func TargetArguments(args json.RawMessage, customTool *corev1alpha1.Tool) (json.RawMessage, error) {
	if len(strings.TrimSpace(string(args))) == 0 {
		return args, nil
	}
	var targetArgsObject map[string]json.RawMessage
	if err := json.Unmarshal(args, &targetArgsObject); err != nil || targetArgsObject == nil {
		return nil, fmt.Errorf("target arguments must be a JSON object")
	}
	if _, ok := targetArgsObject[TargetURLField]; ok {
		return nil, fmt.Errorf("target arguments contain reserved %s field", TargetURLField)
	}
	if authBodyKey := AuthBodyKey(customTool); authBodyKey != "" {
		delete(targetArgsObject, authBodyKey)
	}
	if err := applyURLInterpolationTarget(args, targetArgsObject, customTool); err != nil {
		return nil, err
	}
	out, err := json.Marshal(targetArgsObject)
	if err != nil {
		return nil, fmt.Errorf("sanitize target arguments: %w", err)
	}
	return json.RawMessage(out), nil
}

func applyURLInterpolationTarget(
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
	if authBodyKey := AuthBodyKey(customTool); authBodyKey != "" {
		if URLUsesPlaceholder(customTool, authBodyKey) {
			return fmt.Errorf(
				"approval-gated tool %q URL must not interpolate body auth key %q",
				customTool.Name,
				authBodyKey,
			)
		}
	}
	params, err := decodeTargetArgumentValues(args)
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
	targetArgsObject[TargetURLField] = encoded
	return nil
}

func decodeTargetArgumentValues(args json.RawMessage) (map[string]any, error) {
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

// AuthBodyKey returns the body field an HTTP tool injects its credential
// into, or "" when the tool does not use body auth.
func AuthBodyKey(customTool *corev1alpha1.Tool) string {
	if customTool == nil || customTool.Spec.HTTP == nil || customTool.Spec.HTTP.AuthSecretRef == nil {
		return ""
	}
	if strings.TrimSpace(customTool.Spec.HTTP.AuthInject) != AuthInjectBody {
		return ""
	}
	return strings.TrimSpace(customTool.Spec.HTTP.AuthBodyKey)
}

// URLUsesPlaceholder reports whether the tool URL interpolates key.
func URLUsesPlaceholder(customTool *corev1alpha1.Tool, key string) bool {
	if customTool == nil || customTool.Spec.HTTP == nil {
		return false
	}
	key = strings.TrimSpace(key)
	return key != "" && strings.Contains(customTool.Spec.HTTP.URL, "{{"+key+"}}")
}

// ConnectorTargetSpecDigest digests a connector-backed Tool's spec together
// with the connection-mode policy that injects the credential (output header
// and prefix) and the identity of the Connection frozen for that policy at
// dispatch. An approval for such a tool therefore binds the person's link
// and the injection configuration as they were when the approval was
// requested: a Job re-created after the decision against a re-linked account
// or a changed policy produces a different digest and needs a fresh approval.
func ConnectorTargetSpecDigest(spec corev1alpha1.ToolSpec, policy corev1alpha1.OutboundAccessPolicySpec, connectionUID string, connectionGeneration, grantSequence int64) (string, error) {
	type connectionIdentity struct {
		UID        string `json:"uid"`
		Generation int64  `json:"generation"`
		// GrantSequence binds the approval to one consent: a re-link of
		// the same Connection object needs a fresh decision.
		GrantSequence int64 `json:"grantSequence"`
	}
	return TargetSpecDigest(struct {
		Spec       corev1alpha1.ToolSpec                 `json:"spec"`
		Policy     corev1alpha1.OutboundAccessPolicySpec `json:"policy"`
		Connection connectionIdentity                    `json:"connection"`
	}{
		Spec:       spec,
		Policy:     policy,
		Connection: connectionIdentity{UID: connectionUID, Generation: connectionGeneration, GrantSequence: grantSequence},
	})
}
