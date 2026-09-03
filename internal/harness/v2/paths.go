package v2

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	ProtocolVersion = "orka.harness.v2"
	ACPProfileV1    = "acp.v1"
	NDJSONMediaType = "application/x-ndjson"

	HealthPath          = "/v2/health"
	CapabilitiesPath    = "/v2/capabilities"
	StatusPath          = "/v2/status"
	DrainPath           = "/v2/drain"
	RuntimeSessionsPath = "/v2/runtime-sessions"

	RuntimeSessionPathTemplate                        = "/v2/runtime-sessions/{sessionID}"
	RuntimeSessionPublicationFinalizationPathTemplate = "/v2/runtime-sessions/{sessionID}/publication-finalization"
	PromptPathTemplate                                = "/v2/runtime-sessions/{sessionID}/prompts/{promptID}"
	PromptLeasePathTemplate                           = "/v2/runtime-sessions/{sessionID}/prompts/{promptID}/lease"
	PromptPermissionPathTemplate                      = "/v2/runtime-sessions/{sessionID}/prompts/{promptID}/permissions/{requestID}"
	PromptCancelPathTemplate                          = "/v2/runtime-sessions/{sessionID}/prompts/{promptID}/cancel"
	WorkspaceDeltaPathTemplate                        = "/v2/runtime-sessions/{sessionID}/workspace-deltas/{deltaID}"
)

const maxPathSegmentBytes = 253

// PathSegment validates and escapes an opaque path identifier. Restricting the
// accepted alphabet avoids router-dependent handling of encoded slashes,
// traversal segments, control bytes, and Unicode normalization differences.
func PathSegment(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if len(value) > maxPathSegmentBytes {
		return "", fmt.Errorf("%s exceeds %d bytes", name, maxPathSegmentBytes)
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			continue
		}
		return "", fmt.Errorf("%s contains unsupported path byte 0x%02x", name, c)
	}
	if value == "." || value == ".." {
		return "", fmt.Errorf("%s must not be a traversal segment", name)
	}
	return url.PathEscape(value), nil
}

func RuntimeSessionPath(sessionID RuntimeSessionID) (string, error) {
	session, err := PathSegment("runtime session ID", string(sessionID))
	if err != nil {
		return "", err
	}
	return RuntimeSessionsPath + "/" + session, nil
}

func RuntimeSessionPublicationFinalizationPath(sessionID RuntimeSessionID) (string, error) {
	sessionPath, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	return sessionPath + "/publication-finalization", nil
}

func PromptPath(sessionID RuntimeSessionID, promptID PromptID) (string, error) {
	sessionPath, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	prompt, err := PathSegment("prompt ID", string(promptID))
	if err != nil {
		return "", err
	}
	return sessionPath + "/prompts/" + prompt, nil
}

func PromptLeasePath(sessionID RuntimeSessionID, promptID PromptID) (string, error) {
	path, err := PromptPath(sessionID, promptID)
	if err != nil {
		return "", err
	}
	return path + "/lease", nil
}

func PromptPermissionPath(sessionID RuntimeSessionID, promptID PromptID, requestID PermissionRequestID) (string, error) {
	path, err := PromptPath(sessionID, promptID)
	if err != nil {
		return "", err
	}
	request, err := PathSegment("permission request ID", string(requestID))
	if err != nil {
		return "", err
	}
	return path + "/permissions/" + request, nil
}

func PromptCancelPath(sessionID RuntimeSessionID, promptID PromptID) (string, error) {
	path, err := PromptPath(sessionID, promptID)
	if err != nil {
		return "", err
	}
	return path + "/cancel", nil
}

func WorkspaceDeltaPath(sessionID RuntimeSessionID, deltaID WorkspaceDeltaID) (string, error) {
	sessionPath, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	delta, err := PathSegment("workspace delta ID", string(deltaID))
	if err != nil {
		return "", err
	}
	return sessionPath + "/workspace-deltas/" + delta, nil
}
