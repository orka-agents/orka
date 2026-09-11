package publisher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const repositorySchemeFile = "file"

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func validateIdentifier(field, value string) error {
	if !identifierPattern.MatchString(value) {
		return invalid(field, "%q must match %s", value, identifierPattern.String())
	}
	return nil
}

func validateRepository(repository Repository) error {
	if !providerPattern.MatchString(repository.Provider) {
		return operationError(ErrInvalidRepository, "validate repository provider", "provider must be lower-case canonical ASCII", nil)
	}
	if repository.ID == "" || len(repository.ID) > 512 || strings.TrimSpace(repository.ID) != repository.ID ||
		!utf8.ValidString(repository.ID) || hasControl(repository.ID) {
		return operationError(ErrInvalidRepository, "validate repository ID", "repository ID is empty, non-canonical, or too long", nil)
	}
	canonical, err := canonicalRepositoryURL(repository.URL)
	if err != nil {
		return err
	}
	if repository.URL != canonical {
		return operationError(ErrInvalidRepository, "validate repository URL", fmt.Sprintf("URL is not canonical; expected %q", canonical), nil)
	}
	return nil
}

func canonicalRepositoryURL(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || !utf8.ValidString(raw) || hasControl(raw) {
		return "", operationError(ErrInvalidRepository, "validate repository URL", "URL is empty or non-canonical", nil)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return "", operationError(ErrInvalidRepository, "parse repository URL", "absolute file, https, or ssh URL required", err)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", operationError(ErrInvalidRepository, "validate repository URL", "credentials, opaque URLs, query strings, and fragments are forbidden", nil)
	}
	if parsed.RawPath != "" && parsed.EscapedPath() != parsed.Path {
		return "", operationError(ErrInvalidRepository, "validate repository URL", "escaped path is not canonical", nil)
	}
	switch parsed.Scheme {
	case repositorySchemeFile:
		if parsed.Host != "" && parsed.Host != "localhost" {
			return "", operationError(ErrInvalidRepository, "validate file repository URL", "file URL host must be empty", nil)
		}
		if !filepath.IsAbs(parsed.Path) {
			return "", operationError(ErrInvalidRepository, "validate file repository URL", "file URL path must be absolute", nil)
		}
		cleaned := filepath.Clean(parsed.Path)
		if cleaned != parsed.Path || cleaned == string(filepath.Separator) {
			return "", operationError(ErrInvalidRepository, "validate file repository URL", "file URL path must be clean and non-root", nil)
		}
		return (&url.URL{Scheme: repositorySchemeFile, Path: filepath.ToSlash(cleaned)}).String(), nil
	case "https", "ssh":
		if parsed.Host == "" || parsed.Hostname() == "" {
			return "", operationError(ErrInvalidRepository, "validate repository URL", "network URL host is required", nil)
		}
		if parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/") || path.Clean(parsed.Path) != parsed.Path || parsed.Path == "/" {
			return "", operationError(ErrInvalidRepository, "validate repository URL", "network URL path must be absolute, clean, and non-root", nil)
		}
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		return parsed.String(), nil
	default:
		return "", operationError(ErrInvalidRepository, "validate repository URL", fmt.Sprintf("scheme %q would require an unreviewed remote helper", parsed.Scheme), nil)
	}
}

func validateSourceRefBaseline(sourceRef, baselineOID string) error {
	if validateObjectID("source ref", sourceRef) == nil && sourceRef != baselineOID {
		return invalid("source ref", "exact source commit must equal the frozen baseline")
	}
	return nil
}

func validateSourceRef(ref string) error {
	if validateObjectID("source ref", ref) == nil {
		return nil
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return validateTagRef(ref)
	}
	return validateBranchRef(ref)
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
		return operationError(ErrInvalidRef, "validate "+kind+" ref", fmt.Sprintf("%q is not a canonical full %s ref", ref, kind), nil)
	}
	short := strings.TrimPrefix(ref, prefix)
	if strings.HasPrefix(short, "-") || strings.HasSuffix(short, "/") || strings.HasSuffix(short, ".") ||
		strings.Contains(short, "..") || strings.Contains(short, "@{") || strings.Contains(short, "//") ||
		strings.ContainsAny(short, " ~^:?*[") {
		return operationError(ErrInvalidRef, "validate "+kind+" ref", fmt.Sprintf("%q contains a forbidden ref sequence", ref), nil)
	}
	for component := range strings.SplitSeq(short, "/") {
		if component == "" || component == "." || component == ".." || component == "@" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return operationError(ErrInvalidRef, "validate "+kind+" ref", fmt.Sprintf("%q contains a forbidden ref component", ref), nil)
		}
	}
	return nil
}

func validateObjectID(field, oid string) error {
	if len(oid) != 40 && len(oid) != 64 {
		return operationError(ErrInvalidObjectID, "validate "+field, "object ID must contain 40 or 64 lower-case hexadecimal characters", nil)
	}
	if oid != strings.ToLower(oid) {
		return operationError(ErrInvalidObjectID, "validate "+field, "object ID must be lower-case", nil)
	}
	if _, err := hex.DecodeString(oid); err != nil {
		return operationError(ErrInvalidObjectID, "validate "+field, "object ID is not hexadecimal", err)
	}
	if strings.Trim(oid, "0") == "" {
		return operationError(ErrInvalidObjectID, "validate "+field, "all-zero object ID must be represented as Absent", nil)
	}
	return nil
}

func validateRemoteRef(field string, state RemoteRef) error {
	if state.Absent == (state.OID != "") {
		return invalid(field, "must set exactly one of absent or oid")
	}
	if state.OID != "" {
		return validateObjectID(field+" oid", state.OID)
	}
	return nil
}

func validateClaim(claim BranchClaim, repository Repository, ref string, expected RemoteRef, generation int64) error {
	if claim.RepositoryID != repository.ID || claim.Ref != ref || claim.Generation != generation || !claim.LastVerified.Equal(expected) {
		return operationError(ErrBranchClaimMismatch, "validate branch claim", "repository, ref, generation, or exact baseline differs", nil)
	}
	if claim.OwnerKind != "Task" && claim.OwnerKind != "Session" {
		return invalid("branch claim owner kind", "%q is unsupported", claim.OwnerKind)
	}
	if err := validateIdentifier("branch claim owner UID", claim.OwnerUID); err != nil {
		return err
	}
	return nil
}

func digestCanonical(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode canonical digest input: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

func hasControl(value string) bool {
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return true
		}
	}
	return false
}
