package toolbox

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/security"
)

const (
	SupervisorBinaryPath = "/usr/local/bin/orka-acp-runtime"
	HandoffBinaryName    = "orka-toolbox-copy"
	OutputRootName       = "root"
	stagingName          = ".staging"
	completeName         = ".complete"

	HandoffSubcommand = "toolbox-handoff"
	CopySubcommand    = "toolbox-copy"
	CheckSubcommand   = "toolbox-check"

	ImageVolumeCheckTimeout = 2 * time.Minute

	DefaultMaxEntries    int64 = 200_000
	DefaultMaxTotalBytes int64 = 2 << 30
	DefaultMaxFileBytes  int64 = 512 << 20
	DefaultMaxDepth            = 48

	TerminationLogPath = "/dev/termination-log"
)

const (
	ReasonSourceOpen          = "TOOLBOX_SOURCE_OPEN"
	ReasonUnsupportedFileType = "TOOLBOX_UNSUPPORTED_FILE_TYPE"
	ReasonTooDeep             = "TOOLBOX_TOO_DEEP"
	ReasonTooLarge            = "TOOLBOX_TOO_LARGE"
	ReasonFileTooLarge        = "TOOLBOX_FILE_TOO_LARGE"
	ReasonTooManyEntries      = "TOOLBOX_TOO_MANY_ENTRIES"
	ReasonArchMismatch        = "TOOLBOX_ARCH_MISMATCH"
	ReasonMissingPathEntry    = "TOOLBOX_MISSING_PATH_ENTRY"
	ReasonSourceChanged       = "TOOLBOX_SOURCE_CHANGED"
	ReasonCopyFailed          = "TOOLBOX_COPY_FAILED"
	ReasonInvalidArguments    = "TOOLBOX_INVALID_ARGUMENTS"
	ReasonMissingMount        = "TOOLBOX_MISSING_MOUNT"
	ReasonPermissionDenied    = "TOOLBOX_PERMISSION_DENIED"
	ReasonImagePull           = "TOOLBOX_IMAGE_PULL"
	ReasonMountFailed         = "TOOLBOX_MOUNT_FAILED"
	ReasonUnsupported         = "TOOLBOX_UNSUPPORTED_PLATFORM"
)

type Failure struct {
	Reason  string
	Message string
}

func (f *Failure) Error() string {
	return f.Reason + ": " + f.Message
}

func failf(reason, format string, args ...any) *Failure {
	return &Failure{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

const WithheldDiagnostic = "toolbox diagnostic withheld because it appears to contain a secret or token"

func FormatFailureLine(f *Failure) string {
	message := strings.NewReplacer("\n", " ", "\r", " ").Replace(f.Message)
	if security.LooksLikeSecret(f.Message) || security.LooksLikeSecret(message) {
		message = WithheldDiagnostic
	}
	return "FAIL reason=" + f.Reason + " msg=" + message
}

var failureLinePattern = regexp.MustCompile(`FAIL reason=([A-Z0-9_]+) msg=(.*)`)

func ParseFailureLine(text string) (Failure, bool) {
	matches := failureLinePattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return Failure{}, false
	}
	last := matches[len(matches)-1]
	return Failure{Reason: last[1], Message: strings.TrimSpace(last[2])}, true
}

type Limits struct {
	MaxEntries    int64
	MaxTotalBytes int64
	MaxFileBytes  int64
	MaxDepth      int
}

func DefaultLimits() Limits {
	return Limits{
		MaxEntries: DefaultMaxEntries, MaxTotalBytes: DefaultMaxTotalBytes,
		MaxFileBytes: DefaultMaxFileBytes, MaxDepth: DefaultMaxDepth,
	}
}

type Options struct {
	Source      string
	Destination string
	PathEntries []string
	Arch        string
	Limits      Limits
}

type Summary struct {
	Entries         int64  `json:"entries"`
	Bytes           int64  `json:"bytes"`
	Arch            string `json:"arch"`
	AlreadyComplete bool   `json:"alreadyComplete,omitempty"`
}

func NormalizeArch(arch string) (string, error) {
	arch = strings.TrimSpace(arch)
	if arch == "" {
		arch = runtime.GOARCH
	}
	switch arch {
	case "amd64", "arm64":
		return arch, nil
	default:
		return "", failf(ReasonInvalidArguments, "unsupported architecture %q; want amd64 or arm64", arch)
	}
}
