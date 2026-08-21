package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	RunUIDPrefix           = "run_"
	RunUIDDigestBytes      = 32
	RequestKeyVersion      = "security-request-v1"
	ResolvedKeyVersion     = "security-target-v1"
	repositoryTransportSSH = "ssh"
)

// NewRunUID creates a full-width unpredictable run identity. It is stored in
// SQLite and annotations/env; the shorter public scan ID remains a compatibility alias.
func NewRunUID() (string, error) {
	return newRunUIDFrom(rand.Reader)
}

func newRunUIDFrom(reader io.Reader) (string, error) {
	if reader == nil {
		return "", fmt.Errorf("run UID entropy source is required")
	}
	data := make([]byte, RunUIDDigestBytes)
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", fmt.Errorf("generate run UID: %w", err)
	}
	return RunUIDPrefix + hex.EncodeToString(data), nil
}

func ValidRunUID(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, RunUIDPrefix) {
		return false
	}
	digest := strings.TrimPrefix(value, RunUIDPrefix)
	if len(digest) != RunUIDDigestBytes*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// PublicScanRunID derives the compatibility alias from the full run UID.
func PublicScanRunID(runUID string) string {
	return "scan_" + shortHash("public-scan-run-v1\x00"+runUID)
}

// ScanStageTaskNameForRun derives deterministic Task names from the full run UID.
func ScanStageTaskNameForRun(repositoryScanName, mode, stage, scope, runUID string) string {
	parts := []string{sanitizeName(repositoryScanName), sanitizeName(mode), sanitizeName(stage)}
	if strings.TrimSpace(scope) != "" {
		parts = append(parts, sanitizeName(scope))
	}
	parts = append(parts, shortHash("stage-task-v1\x00"+runUID+"\x00"+stage+"\x00"+scope))
	return boundedTaskName(parts...)
}

type requestIdempotencyInput struct {
	Version             string `json:"version"`
	RepositoryScanUID   string `json:"repositoryScanUID"`
	Generation          int64  `json:"generation"`
	Mode                string `json:"mode"`
	RequestedBranch     string `json:"requestedBranch"`
	RequestedRef        string `json:"requestedRef"`
	BaseWatermark       string `json:"baseWatermark"`
	RequestedHeadCommit string `json:"requestedHeadCommit,omitempty"`
	SubPath             string `json:"subPath"`
	PolicyDigest        string `json:"policyDigest"`
	IsolationPolicy     string `json:"isolationPolicy"`
	CompletionPolicy    string `json:"completionPolicy"`
	DeepScanConfigHash  string `json:"deepScanConfigHash"`
}

// RequestIdempotencyKey is computed before mutable target resolution.
func RequestIdempotencyKey(scan *corev1alpha1.RepositoryScan, mode, baseWatermark, headCommit, policyDigest string) string {
	if scan == nil {
		return ""
	}
	deepHash := ""
	if scan.Spec.DeepScan != nil {
		if data, err := json.Marshal(scan.Spec.DeepScan); err == nil {
			digest := sha256.Sum256(data)
			deepHash = hex.EncodeToString(digest[:])
		}
	}
	input := requestIdempotencyInput{
		Version:             RequestKeyVersion,
		RepositoryScanUID:   string(scan.UID),
		Generation:          scan.Generation,
		Mode:                strings.TrimSpace(mode),
		RequestedBranch:     strings.TrimSpace(scan.Spec.Branch),
		RequestedRef:        strings.TrimSpace(scan.Spec.Ref),
		BaseWatermark:       strings.TrimSpace(baseWatermark),
		RequestedHeadCommit: strings.ToLower(strings.TrimSpace(headCommit)),
		SubPath:             canonicalTargetSubPath(scan.Spec.SubPath),
		PolicyDigest:        strings.TrimSpace(policyDigest),
		IsolationPolicy:     EffectiveAnalysisIsolationPolicy(scan),
		CompletionPolicy:    EffectiveCompletionPolicy(scan),
		DeepScanConfigHash:  deepHash,
	}
	data, _ := json.Marshal(input)
	digest := sha256.Sum256(data)
	return "req_" + hex.EncodeToString(digest[:])
}

func canonicalTargetSubPath(subPath string) string {
	// Git path components may contain leading or trailing whitespace. Preserve it
	// while canonicalizing separator spelling and redundant boundary separators.
	return strings.Trim(strings.ReplaceAll(subPath, "\\", "/"), "/")
}

// ResolvedTargetKey binds immutable target identity separately from the
// pre-resolution request key.
func ResolvedTargetKey(targetID, baseCommit, headCommit, subPath, policyDigest string) string {
	input := strings.Join([]string{
		ResolvedKeyVersion,
		strings.TrimSpace(targetID),
		strings.ToLower(strings.TrimSpace(baseCommit)),
		strings.ToLower(strings.TrimSpace(headCommit)),
		canonicalTargetSubPath(subPath),
		strings.TrimSpace(policyDigest),
	}, "\x00")
	digest := sha256.Sum256([]byte(input))
	return "target_" + hex.EncodeToString(digest[:])
}

func canonicalRepositoryAuthority(host, port string) string {
	host = canonicalRepositoryHost(host)
	port = strings.TrimSpace(port)
	if host == "" {
		return ""
	}
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func canonicalRepositoryHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}

	address := host
	zone := ""
	if delimiter := strings.LastIndexByte(address, '%'); delimiter > 0 && delimiter < len(address)-1 {
		address, zone = address[:delimiter], address[delimiter+1:]
	}
	if strings.Contains(address, ":") {
		if parsed := net.ParseIP(address); parsed != nil {
			canonical := parsed.String()
			if zone != "" {
				canonical += "%" + zone
			}
			return canonical
		}
	}
	return strings.ToLower(host)
}

func canonicalRepositoryTransport(scheme string) string {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if isSSHRepositoryScheme(scheme) {
		return repositoryTransportSSH
	}
	if scheme == "" {
		return "network-path"
	}
	return scheme
}

func canonicalRepositoryPort(transport, port string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return ""
	}
	port = strings.TrimLeft(port, "0")
	if port == "" {
		port = "0"
	}
	defaultPort := ""
	switch transport {
	case "http":
		defaultPort = "80"
	case schemeHTTPS:
		defaultPort = "443"
	case repositoryTransportSSH:
		defaultPort = "22"
	case "git":
		defaultPort = "9418"
	}
	if port == defaultPort {
		return ""
	}
	return port
}

func canonicalParsedRepositoryAuthority(parsed *url.URL) string {
	transport := canonicalRepositoryTransport(parsed.Scheme)
	return canonicalRepositoryAuthority(parsed.Hostname(), canonicalRepositoryPort(transport, parsed.Port()))
}

func canonicalSCPRepositoryAuthority(host string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return canonicalRepositoryAuthority(host, "")
}

func stripRepositoryQueryFragment(value string) string {
	value = strings.TrimSpace(value)
	value = repositoryWithoutQueryFragment(value)
	return strings.TrimSpace(value)
}

func repositoryWithoutQueryFragment(value string) string {
	if delimiter := strings.IndexAny(value, "?#"); delimiter >= 0 {
		value = value[:delimiter]
	}
	return value
}

func splitSCPRepository(value string) (username, host, repoPath string, ok bool) {
	// Do not trim the complete SCP spelling: whitespace after the colon is part
	// of the remote path and must not collapse onto a different repository.
	value = repositoryWithoutQueryFragment(value)
	if value == "" || strings.Contains(value, "://") {
		return "", "", "", false
	}

	delimiter := -1
	inBrackets := false
findDelimiter:
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '[':
			inBrackets = true
		case ']':
			inBrackets = false
		case ':':
			if !inBrackets {
				delimiter = index
				break findDelimiter
			}
		}
	}
	if delimiter <= 0 || delimiter == len(value)-1 {
		return "", "", "", false
	}

	authority := value[:delimiter]
	if at := strings.IndexByte(authority, '@'); at >= 0 {
		username = authority[:at]
		if username == "" || strings.ContainsAny(username, ":/\\") {
			return "", "", "", false
		}
		authority = authority[at+1:]
	}
	host = strings.TrimSpace(authority)
	repoPath = value[delimiter+1:]
	if strings.HasPrefix(host, "[") && (len(host) <= 2 || !strings.HasSuffix(host, "]")) {
		return "", "", "", false
	}
	if host == "" || strings.TrimSpace(repoPath) == "" || strings.ContainsAny(host, "/\\@") || strings.ContainsAny(host, " \t\r\n") {
		return "", "", "", false
	}
	return username, host, repoPath, true
}

func repositoryServerOrigin(repoURL string) string {
	if _, host, _, ok := splitSCPRepository(repoURL); ok {
		return canonicalSCPRepositoryAuthority(host)
	}
	value := strings.TrimSpace(repoURL)
	if parsed, err := url.Parse(value); err == nil && parsed.Hostname() != "" {
		return canonicalParsedRepositoryAuthority(parsed)
	}
	return ""
}

func repositoryTargetOrigin(repoURL string) string {
	origin := repositoryServerOrigin(repoURL)
	if origin == "" {
		return ""
	}
	if username, _, repoPath, ok := splitSCPRepository(repoURL); ok {
		if username != "" && !strings.HasPrefix(repoPath, "/") {
			return origin + "/~user/" + url.PathEscape(username)
		}
		return origin
	}
	value := strings.TrimSpace(repoURL)
	parsed, err := url.Parse(value)
	if err != nil || parsed.User == nil || !isSSHRepositoryScheme(parsed.Scheme) {
		return origin
	}
	repoPath := canonicalParsedRepositoryPath(parsed)
	if !strings.HasPrefix(repoPath, "~/") {
		return origin
	}
	username := parsed.User.Username()
	if username == "" {
		return origin
	}
	return origin + "/~user/" + url.PathEscape(username)
}

func canonicalRepositoryURLCoordinate(repoURL string) string {
	if username, host, repoPath, ok := splitSCPRepository(repoURL); ok {
		authority := canonicalSCPRepositoryAuthority(host)
		rooted := strings.HasPrefix(repoPath, "/")
		repoPath = strings.Trim(repoPath, "/")
		if !rooted {
			repoPath = strings.TrimPrefix(repoPath, "~/")
		}
		repoPath = strings.TrimSuffix(repoPath, ".git")
		if rooted {
			username = ""
		}
		return encodeRepositoryURLCoordinate(repositoryURLCoordinate{
			Kind:      "remote",
			Transport: repositoryTransportSSH,
			Authority: authority,
			Rooted:    rooted,
			Username:  canonicalRepositoryUsername(username),
			Path:      canonicalEscapedRepositoryPath(repoPath),
		})
	}
	value := strings.TrimSpace(repoURL)
	if parsed, err := url.Parse(value); err == nil && parsed.Hostname() != "" {
		transport := canonicalRepositoryTransport(parsed.Scheme)
		repoPath := canonicalParsedRepositoryPath(parsed)
		rooted := true
		username := ""
		if transport == repositoryTransportSSH && strings.HasPrefix(repoPath, "~/") {
			rooted = false
			repoPath = strings.TrimPrefix(repoPath, "~/")
			if parsed.User != nil {
				username = parsed.User.Username()
			}
		}
		return encodeRepositoryURLCoordinate(repositoryURLCoordinate{
			Kind:      "remote",
			Transport: transport,
			Authority: canonicalParsedRepositoryAuthority(parsed),
			Rooted:    rooted,
			Username:  canonicalRepositoryUsername(username),
			Path:      repoPath,
		})
	}
	value = stripRepositoryQueryFragment(value)
	rooted := strings.HasPrefix(value, "/")
	return encodeRepositoryURLCoordinate(repositoryURLCoordinate{
		Kind:   "raw",
		Rooted: rooted,
		Path:   canonicalEscapedRepositoryPath(strings.TrimSuffix(strings.Trim(value, "/"), ".git")),
	})
}

type repositoryURLCoordinate struct {
	Kind      string `json:"kind"`
	Transport string `json:"transport"`
	Authority string `json:"authority"`
	Rooted    bool   `json:"rooted"`
	Username  string `json:"username"`
	Path      string `json:"path"`
}

func encodeRepositoryURLCoordinate(coordinate repositoryURLCoordinate) string {
	data, _ := json.Marshal(coordinate)
	return string(data)
}

func canonicalParsedRepositoryPath(parsed *url.URL) string {
	return strings.TrimSuffix(strings.Trim(canonicalizePercentEscapes(parsed.EscapedPath()), "/"), ".git")
}

func canonicalEscapedRepositoryPath(repoPath string) string {
	escaped := (&url.URL{Path: repoPath}).EscapedPath()
	return canonicalizePercentEscapes(escaped)
}

func canonicalRepositoryUsername(username string) string {
	return canonicalizePercentEscapes(url.PathEscape(username))
}

func canonicalizePercentEscapes(value string) string {
	var canonical strings.Builder
	canonical.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '%' || index+2 >= len(value) {
			canonical.WriteByte(value[index])
			continue
		}
		high, highOK := hexadecimalValue(value[index+1])
		low, lowOK := hexadecimalValue(value[index+2])
		if !highOK || !lowOK {
			canonical.WriteByte(value[index])
			continue
		}
		decoded := high<<4 | low
		if isURLUnreserved(decoded) {
			canonical.WriteByte(decoded)
		} else {
			const hexDigits = "0123456789ABCDEF"
			canonical.WriteByte('%')
			canonical.WriteByte(hexDigits[decoded>>4])
			canonical.WriteByte(hexDigits[decoded&0x0f])
		}
		index += 2
	}
	return canonical.String()
}

func hexadecimalValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func isURLUnreserved(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '-' || value == '.' || value == '_' || value == '~'
}

func isSSHRepositoryScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case repositoryTransportSSH, "git+ssh", "ssh+git":
		return true
	default:
		return false
	}
}

// RepositoryTargetID returns a versioned, credential-free stable repository identity.
func RepositoryTargetID(scan *corev1alpha1.RepositoryScan) string {
	if scan == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(scan.Spec.Provider))
	owner := strings.TrimSpace(scan.Spec.Owner)
	repository := strings.TrimSpace(scan.Spec.Repository)
	repoURL := scan.Spec.RepoURL
	coordinate := provider + "\x00" + canonicalRepositoryURLCoordinate(repoURL)
	if owner != "" && repository != "" {
		coordinate = provider + "\x00" + repositoryTargetOrigin(repoURL) + "\x00" + owner + "\x00" + repository
	}
	digest := sha256.Sum256([]byte("repository-target-v1\x00" + coordinate))
	return "repo_" + hex.EncodeToString(digest[:])
}

func PatchProposalIDForOccurrence(runUID, occurrenceID string) string {
	digest := sha256.Sum256([]byte("patch-proposal-v1\x00" + runUID + "\x00" + occurrenceID))
	return "patch_" + hex.EncodeToString(digest[:])
}
