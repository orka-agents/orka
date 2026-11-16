// Package operatoraccess holds the shared vocabulary of the operator
// workspace debugging surface (`orka task exec` / `orka task cp`): the class
// opt-in, the dedicated authorization resources and scopes, the durable taint
// marker, workspace path validation, and the stream framing shared by the CLI,
// orka-api, and the ACP supervisor.
//
// Everything here fails closed: an unknown opt-in value is "disabled", a
// malformed taint marker is "tainted", and a path that is not a plain relative
// path beneath the workspace root is rejected.
package operatoraccess

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// ClassAccessAnnotation is the administrator opt-in on an
	// ExecutionWorkspaceClass. It is absent by default. Setting or changing it
	// requires the dedicated "operator-access" verb on executionworkspaceclasses
	// (enforced by a ValidatingAdmissionPolicy), so Task authors who can only
	// "use" a class can never enable it.
	ClassAccessAnnotation = "acp.workspace.orka.ai/operator-access"
	// ClassAccessVerb is the RBAC verb required to set ClassAccessAnnotation.
	ClassAccessVerb = "operator-access"

	// TaintAnnotation durably marks an ExecutionWorkspace and the Task whose
	// RuntimeSession received a write-capable operator operation. Both
	// prefixes are controller-protected: ExecutionWorkspace annotations by the
	// core admission policy, Task annotations by the provenance webhook.
	TaintAnnotation = "acp.workspace.orka.ai/operator-tainted"

	// ExecResource and FilesResource are the dedicated virtual subresources of
	// tasks in the core.orka.ai group checked with SubjectAccessReview. They
	// are separate from Task create/get and are not granted by any built-in
	// aggregate role.
	ExecResource  = "tasks/exec"
	FilesResource = "tasks/files"
	// ExecVerb authorizes `orka task exec`.
	ExecVerb = "create"
	// FilesReadVerb authorizes `orka task cp <task>:<path> <local>`.
	FilesReadVerb = "get"
	// FilesWriteVerb authorizes `orka task cp <local> <task>:<path>`.
	FilesWriteVerb = "update"

	// ScopeExec, ScopeFilesRead and ScopeFilesWrite are the explicit
	// context-token scopes. They are never implied by Task scopes and are
	// enforced even when context-token authorization runs in audit mode.
	ScopeExec       = "orka:tasks:exec"
	ScopeFilesRead  = "orka:tasks:files:read"
	ScopeFilesWrite = "orka:tasks:files:write"
)

// AccessLevel is the administrator-granted operator access for a class.
type AccessLevel int

const (
	// AccessNone is the default: no operator access.
	AccessNone AccessLevel = iota
	// AccessRead allows read-only copies out of the workspace.
	AccessRead
	// AccessExec allows exec, copies in, and copies out.
	AccessExec
)

// ParseClassAccess maps the class annotation to an access level. Absent,
// empty, and unknown values are AccessNone.
func ParseClassAccess(annotations map[string]string) AccessLevel {
	switch strings.TrimSpace(annotations[ClassAccessAnnotation]) {
	case "read":
		return AccessRead
	case "exec":
		return AccessExec
	default:
		return AccessNone
	}
}

// Operation is one operator access kind.
type Operation string

const (
	OperationExec      Operation = "exec"
	OperationCopyIn    Operation = "cp-in"
	OperationCopyOut   Operation = "cp-out"
	defaultTaintReason           = "operator workspace access"
)

// Writes reports whether the operation can change workspace or session state
// and therefore taints the session before it starts.
func (o Operation) Writes() bool { return o == OperationExec || o == OperationCopyIn }

// Allowed reports whether the class access level permits the operation.
func (l AccessLevel) Allowed(operation Operation) bool {
	switch operation {
	case OperationCopyOut:
		return l >= AccessRead
	case OperationExec, OperationCopyIn:
		return l >= AccessExec
	default:
		return false
	}
}

// Taint is the durable marker value. It records safe metadata only.
type Taint struct {
	At        time.Time `json:"at"`
	Operation Operation `json:"operation"`
	AuditID   string    `json:"auditID"`
	TaskUID   string    `json:"taskUID,omitempty"`
}

// EncodeTaint renders the marker value.
func EncodeTaint(taint Taint) string {
	if taint.Operation == "" {
		taint.Operation = defaultTaintReason
	}
	raw, err := json.Marshal(taint)
	if err != nil {
		return "true"
	}
	return string(raw)
}

// Tainted reports whether annotations carry the marker. Any present value,
// including a malformed one, counts: the marker can only be removed by
// deleting the object, never by corrupting it.
func Tainted(annotations map[string]string) bool {
	_, ok := annotations[TaintAnnotation]
	return ok
}

// ErrTainted is returned by every gate that refuses a tainted workspace.
var ErrTainted = errors.New("execution workspace was tainted by operator access")

// MaxPathBytes bounds a workspace-relative path.
const MaxPathBytes = 4096

// CleanWorkspacePath validates an operator-supplied path and returns it as a
// slash-separated path relative to the session workspace root. A leading "/"
// names the workspace root itself (the agent's working directory), so
// "/src/main.go" and "src/main.go" are the same file. Traversal segments,
// NUL bytes, invalid UTF-8, and backslashes are rejected rather than cleaned.
func CleanWorkspacePath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("path is required")
	}
	if len(raw) > MaxPathBytes {
		return "", fmt.Errorf("path exceeds %d bytes", MaxPathBytes)
	}
	if !utf8.ValidString(raw) || strings.ContainsAny(raw, "\x00\\") {
		return "", fmt.Errorf("path contains unsupported characters")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("path contains control characters")
		}
	}
	trimmed := strings.TrimLeft(raw, "/")
	if trimmed == "" {
		return "", fmt.Errorf("path must name a file beneath the workspace root")
	}
	for segment := range strings.SplitSeq(trimmed, "/") {
		switch segment {
		case "..":
			return "", fmt.Errorf("path must not contain traversal segments")
		case ".", "":
			// Redundant separators and "." are normalized below.
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", fmt.Errorf("path must name a file beneath the workspace root")
	}
	return cleaned, nil
}

// ArgvDigest is the audit digest of an argv vector. Audit records carry the
// digest and the command basename, never the raw arguments, which may contain
// secrets typed by the operator.
func ArgvDigest(argv []string) string {
	raw, err := json.Marshal(argv)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RedactedCommand returns the basename of argv[0] for audit records.
func RedactedCommand(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	base := path.Base(strings.ReplaceAll(argv[0], "\\", "/"))
	if len(base) > 64 {
		base = base[:64]
	}
	return base
}
