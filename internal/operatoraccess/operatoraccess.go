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
	ClassAccessAnnotation = "acp.workspace.orka.ai/operator-access"
	ClassAccessVerb       = "operator-access"

	TaintAnnotation = "acp.workspace.orka.ai/operator-tainted"

	ExecResource   = "tasks/exec"
	FilesResource  = "tasks/files"
	ExecVerb       = "create"
	FilesReadVerb  = "get"
	FilesWriteVerb = "update"

	ScopeExec       = "orka:tasks:exec"
	ScopeFilesRead  = "orka:tasks:files:read"
	ScopeFilesWrite = "orka:tasks:files:write"
)

type AccessLevel int

const (
	AccessNone AccessLevel = iota
	AccessRead
	AccessExec
)

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

type Operation string

const (
	OperationExec      Operation = "exec"
	OperationCopyIn    Operation = "cp-in"
	OperationCopyOut   Operation = "cp-out"
	defaultTaintReason           = "operator workspace access"
)

func (o Operation) Writes() bool { return o == OperationExec || o == OperationCopyIn }

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

type Taint struct {
	At        time.Time `json:"at"`
	Operation Operation `json:"operation"`
	AuditID   string    `json:"auditID"`
	TaskUID   string    `json:"taskUID,omitempty"`
}

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

func Tainted(annotations map[string]string) bool {
	_, ok := annotations[TaintAnnotation]
	return ok
}

var ErrTainted = errors.New("execution workspace was tainted by operator access")

const MaxPathBytes = 4096

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
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", fmt.Errorf("path must name a file beneath the workspace root")
	}
	return cleaned, nil
}

func ArgvDigest(argv []string) string {
	raw, err := json.Marshal(argv)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

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
