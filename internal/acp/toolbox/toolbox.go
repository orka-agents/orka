// Package toolbox implements the untrusted-image toolbox copier and checks
// used by the ACP runtime Pod. Nothing here ever executes, follows, or trusts
// a file from a toolbox image: directories are opened relative to file
// descriptors with O_NOFOLLOW, regular files are copied byte for byte with
// sanitized modes, symlink text is copied without being followed, and every
// other file type fails the copy.
package toolbox

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

const (
	// SupervisorBinaryPath is where every built-in runtime image installs the
	// supervisor. The toolbox subcommands live in the same static binary.
	SupervisorBinaryPath = "/usr/local/bin/orka-acp-runtime"
	// HandoffBinaryName is the copier file name inside the handoff volume.
	HandoffBinaryName = "orka-toolbox-copy"
	// OutputRootName is the completed copy folder inside the output volume.
	// The runtime container mounts it with subPath so staging never shows.
	OutputRootName = "root"
	stagingName    = ".staging"
	completeName   = ".complete"

	HandoffSubcommand = "toolbox-handoff"
	CopySubcommand    = "toolbox-copy"
	CheckSubcommand   = "toolbox-check"

	// Default copy limits. Flags can lower or raise them.
	DefaultMaxEntries    int64 = 200_000
	DefaultMaxTotalBytes int64 = 2 << 30
	DefaultMaxFileBytes  int64 = 512 << 20
	DefaultMaxDepth            = 48

	// TerminationLogPath receives the stable failure line so the controller
	// can classify the failure from the container status.
	TerminationLogPath = "/dev/termination-log"
)

// Stable failure reasons. They appear verbatim in Task and RuntimePool status.
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

// Failure is a classified toolbox error with a stable reason.
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

// FormatFailureLine renders the single stable line printed on failure.
func FormatFailureLine(f *Failure) string {
	message := strings.NewReplacer("\n", " ", "\r", " ").Replace(f.Message)
	return "FAIL reason=" + f.Reason + " msg=" + message
}

var failureLinePattern = regexp.MustCompile(`FAIL reason=([A-Z0-9_]+) msg=(.*)`)

// ParseFailureLine finds the stable failure line in a termination message or
// log tail. It returns the last FAIL line when several are present.
func ParseFailureLine(text string) (Failure, bool) {
	matches := failureLinePattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return Failure{}, false
	}
	last := matches[len(matches)-1]
	return Failure{Reason: last[1], Message: strings.TrimSpace(last[2])}, true
}

// Limits bounds one toolbox copy.
type Limits struct {
	MaxEntries    int64
	MaxTotalBytes int64
	MaxFileBytes  int64
	MaxDepth      int
}

// DefaultLimits returns the documented defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxEntries: DefaultMaxEntries, MaxTotalBytes: DefaultMaxTotalBytes,
		MaxFileBytes: DefaultMaxFileBytes, MaxDepth: DefaultMaxDepth,
	}
}

// Options configures one copy.
type Options struct {
	// Source is the absolute toolbox folder inside the toolbox image.
	Source string
	// Destination is the scratch volume; the copy lands in Destination/root.
	Destination string
	// PathEntries are folders relative to Source that must exist and whose
	// ELF files must match Arch.
	PathEntries []string
	// Arch is amd64 or arm64. Empty means the copier's own architecture,
	// which is the node architecture because the copier runs on the node.
	Arch   string
	Limits Limits
}

// Summary describes a completed copy.
type Summary struct {
	Entries int64  `json:"entries"`
	Bytes   int64  `json:"bytes"`
	Arch    string `json:"arch"`
	// AlreadyComplete reports that a previous run finished and nothing was
	// copied again.
	AlreadyComplete bool `json:"alreadyComplete,omitempty"`
}

// NormalizeArch validates the architecture selector.
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
