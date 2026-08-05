package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/publisher"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

var credentialNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (m OperationMetadata) validateFor(operation Operation) error {
	identity := artifactcap.Identity{Namespace: m.Namespace, TaskID: m.TaskID, PublicationID: m.PublicationID}
	if err := identity.Validate(); err != nil {
		return invalidRequest("operation identity is invalid", err)
	}
	if err := validateOpaqueID("operation ID", m.OperationID, 512); err != nil {
		return err
	}
	switch operation {
	case OperationWorkspaceResolve, OperationWorkspacePrepare:
		if m.TaskID == "" || m.PublicationID != "" {
			return invalidRequest("workspace operation requires exactly one Task identity", nil)
		}
	case OperationPublicationPreflight, OperationPublicationPrepare, OperationPublicationPublish,
		OperationPublicationVerify, OperationPublicationReclaim, OperationPullRequestReconcile:
		if m.PublicationID == "" || m.TaskID != "" {
			return invalidRequest("publication operation requires exactly one Publication identity", nil)
		}
	default:
		return invalidRequest("unsupported operation", nil)
	}
	return nil
}

func validateOpaqueID(field, value string, max int) error {
	if value == "" || len(value) > max || strings.TrimSpace(value) != value || !utf8.ValidString(value) || hasControl(value) {
		return invalidRequest(field+" is empty, non-canonical, or too long", nil)
	}
	return nil
}

func validateCredentialReference(reference *CredentialReference, expected CredentialKind) error {
	if reference == nil {
		return nil
	}
	if !credentialNamePattern.MatchString(reference.Name) {
		return apiError(ErrCredential, "invalid_credential_ref", "credential reference name is invalid", 400, false, nil)
	}
	if reference.Kind != expected {
		return apiError(ErrCredential, "invalid_credential_ref", "credential reference kind is invalid for this operation", 400, false, nil)
	}
	switch reference.Role {
	case "", CredentialRoleSourceRead, CredentialRoleTargetRead, CredentialRoleTargetWrite, CredentialRoleForge:
	default:
		return apiError(ErrCredential, "invalid_credential_ref", "credential reference role is invalid", 400, false, nil)
	}
	return nil
}

//nolint:gocyclo // Repository URL policy is intentionally explicit and fail-closed.
func validateRepository(repository publisher.Repository, allowFile bool, allowedHosts []string) error {
	if err := validateOpaqueID("repository provider", repository.Provider, 63); err != nil {
		return err
	}
	if repository.Provider != strings.ToLower(repository.Provider) {
		return invalidRequest("repository provider must be lower-case", nil)
	}
	if err := validateOpaqueID("repository ID", repository.ID, 512); err != nil {
		return err
	}
	if repository.URL == "" || strings.TrimSpace(repository.URL) != repository.URL || !utf8.ValidString(repository.URL) || hasControl(repository.URL) {
		return invalidRequest("repository URL is invalid", nil)
	}
	parsed, err := url.Parse(repository.URL)
	if err != nil || parsed.Scheme == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return invalidRequest("repository URL must be an absolute credential-free URL", err)
	}
	if parsed.RawPath != "" && parsed.EscapedPath() != parsed.Path {
		return invalidRequest("repository URL escaped path is non-canonical", nil)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "file":
		if !allowFile || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || path.Clean(parsed.Path) != parsed.Path || parsed.Path == "/" {
			return invalidRequest("file repositories are disabled or non-canonical", nil)
		}
	case schemeHTTPS:
		hostname := strings.ToLower(parsed.Hostname())
		if hostname == "" || parsed.Host != strings.ToLower(parsed.Host) || parsed.Path == "" || parsed.Path == "/" ||
			!strings.HasPrefix(parsed.Path, "/") || path.Clean(parsed.Path) != parsed.Path {
			return invalidRequest("HTTPS repository URL is non-canonical", nil)
		}
		if ip := net.ParseIP(hostname); ip != nil && (ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
			return invalidRequest("repository URL uses a forbidden IP literal", nil)
		}
		if len(allowedHosts) > 0 && !slices.Contains(allowedHosts, hostname) {
			return invalidRequest("repository host is outside the configured SCM allowlist", nil)
		}
	default:
		return invalidRequest("only HTTPS repositories are allowed", nil)
	}
	return nil
}

func validateSourceRefBaseline(sourceRef, baselineOID string) error {
	if validateObjectID(sourceRef) == nil && sourceRef != baselineOID {
		return invalidRequest("exact source commit must equal the frozen baseline", nil)
	}
	return nil
}

func validateSourceRef(ref string) error {
	if validateObjectID(ref) == nil {
		return nil
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return validateTagRef(ref)
	}
	return validateBranchRef(ref)
}

// CanonicalWorkspaceSourceRef validates a workspace source selector while
// preserving bare names for repository-aware branch/tag disambiguation.
func CanonicalWorkspaceSourceRef(ref string) (string, error) {
	if validateObjectID(ref) == nil {
		return ref, nil
	}
	if looksLikeObjectID(ref) {
		return "", validateObjectID(ref)
	}
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return ref, validateBranchRef(ref)
	case strings.HasPrefix(ref, "refs/tags/"):
		return ref, validateTagRef(ref)
	case strings.HasPrefix(ref, "refs/"):
		return "", invalidRequest("source ref uses an unsupported canonical namespace", nil)
	default:
		return ref, validateShortSourceRef(ref)
	}
}

func validateShortSourceRef(ref string) error {
	if ref == "" || len(ref) > 1024-len("refs/heads/") || strings.TrimSpace(ref) != ref ||
		!utf8.ValidString(ref) || hasControl(ref) || strings.Contains(ref, "\\") {
		return invalidRequest("short source ref is empty, non-canonical, or too long", nil)
	}
	return validateRefPath(ref, "short source")
}

func isBareWorkspaceSourceRef(ref string) bool {
	return validateObjectID(ref) != nil && !strings.HasPrefix(ref, "refs/")
}

func validateBranchRef(ref string) error {
	return validateCanonicalRef(ref, "refs/heads/", "branch")
}

func validateTagRef(ref string) error {
	return validateCanonicalRef(ref, "refs/tags/", "tag")
}

func validateCanonicalRef(ref, prefix, kind string) error {
	if len(ref) <= len(prefix) || len(ref) > 1024 || !strings.HasPrefix(ref, prefix) ||
		strings.TrimSpace(ref) != ref || !utf8.ValidString(ref) || hasControl(ref) || strings.Contains(ref, "\\") {
		return invalidRequest(kind+" ref is not a canonical full "+prefix+" reference", nil)
	}
	return validateRefPath(strings.TrimPrefix(ref, prefix), kind)
}

func validateRefPath(ref, kind string) error {
	if strings.HasPrefix(ref, "-") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") ||
		strings.ContainsAny(ref, " ~^:?*[") {
		return invalidRequest(kind+" ref contains a forbidden sequence", nil)
	}
	for component := range strings.SplitSeq(ref, "/") {
		if component == "" || component == "." || component == ".." || component == "@" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return invalidRequest(kind+" ref contains a forbidden component", nil)
		}
	}
	return nil
}

func looksLikeObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateObjectID(value string) error {
	if len(value) != 40 && len(value) != 64 || value != strings.ToLower(value) {
		return invalidRequest("Git object ID must be lower-case SHA-1 or SHA-256", nil)
	}
	if _, err := hex.DecodeString(value); err != nil || strings.Trim(value, "0") == "" {
		return invalidRequest("Git object ID is invalid", err)
	}
	return nil
}

func validateWorkspacePath(value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") ||
		strings.TrimSpace(value) != value || !utf8.ValidString(value) || hasControl(value) {
		return invalidRequest("repository tree contains an unsafe path", nil)
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return invalidRequest("repository tree path escapes the workspace", nil)
	}
	for component := range strings.SplitSeq(value, "/") {
		lower := strings.ToLower(component)
		if component == "" || component == "." || component == ".." || lower == ".git" || lower == ".gitmodules" {
			return invalidRequest("repository tree contains forbidden Git metadata", nil)
		}
	}
	return nil
}

func validatePullRequestReceipt(receipt publisher.PullRequestReceipt) error {
	if err := validateOpaqueID("pull request forge ID", receipt.ForgeID, 512); err != nil {
		return apiError(ErrOperationConflict, "pr_receipt_invalid", "pull request backend returned an invalid receipt", 502, false, err)
	}
	if len(receipt.URL) < 1 || len(receipt.URL) > maxGitHubReceiptURLBytes || strings.TrimSpace(receipt.URL) != receipt.URL || hasControl(receipt.URL) {
		return apiError(ErrOperationConflict, "pr_receipt_invalid", "pull request backend returned an invalid receipt", 502, false, nil)
	}
	parsed, err := url.Parse(receipt.URL)
	if err != nil || parsed.Scheme != schemeHTTPS || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return apiError(ErrOperationConflict, "pr_receipt_invalid", "pull request backend returned an invalid receipt", 502, false, err)
	}
	return nil
}

func hasControl(value string) bool {
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return true
		}
	}
	return false
}

func hashID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func normalizeAllowedHosts(values []string) ([]string, error) {
	set := map[string]struct{}{}
	for _, value := range values {
		host := strings.ToLower(strings.TrimSpace(value))
		if host == "" || net.ParseIP(host) != nil || strings.ContainsAny(host, "/:@[]") {
			return nil, fmt.Errorf("invalid allowed SCM host %q", value)
		}
		set[host] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for host := range set {
		result = append(result, host)
	}
	slices.Sort(result)
	return result, nil
}
