package toolbox

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// IsSubcommand reports whether name is one of the toolbox subcommands handled
// before the supervisor hardens its process or reads its configuration.
func IsSubcommand(name string) bool {
	switch name {
	case HandoffSubcommand, CopySubcommand, CheckSubcommand:
		return true
	default:
		return false
	}
}

// Run executes one toolbox subcommand and returns the process exit code. On
// failure it prints exactly one stable FAIL line to stderr and, best effort,
// to the container termination log.
func Run(name string, args []string, stdout, stderr io.Writer) int {
	err := run(name, args, stdout)
	if err == nil {
		return 0
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		failure = &Failure{Reason: ReasonCopyFailed, Message: err.Error()}
	}
	line := FormatFailureLine(failure)
	_, _ = fmt.Fprintln(stderr, line)
	writeTerminationLog(line)
	return 1
}

func run(name string, args []string, stdout io.Writer) error {
	switch name {
	case HandoffSubcommand:
		flags := flag.NewFlagSet(name, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		dst := flags.String("dst", "", "handoff folder")
		if err := flags.Parse(args); err != nil {
			return failf(ReasonInvalidArguments, "%v", err)
		}
		if *dst == "" {
			return failf(ReasonInvalidArguments, "--dst is required")
		}
		final, err := Handoff(*dst)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "OK handoff=%s\n", final)
		return nil
	case CopySubcommand:
		flags := flag.NewFlagSet(name, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		opts := Options{Limits: DefaultLimits()}
		var pathEntries string
		flags.StringVar(&opts.Source, "src", "", "toolbox folder inside the image")
		flags.StringVar(&opts.Destination, "dst", "", "output volume")
		flags.StringVar(&pathEntries, "path-entries", "", "comma-separated PATH folders relative to --src")
		flags.StringVar(&opts.Arch, "arch", "", "expected ELF architecture (amd64 or arm64); defaults to the node architecture")
		flags.Int64Var(&opts.Limits.MaxEntries, "max-entries", opts.Limits.MaxEntries, "maximum number of entries")
		flags.Int64Var(&opts.Limits.MaxTotalBytes, "max-total-bytes", opts.Limits.MaxTotalBytes, "maximum total bytes")
		flags.Int64Var(&opts.Limits.MaxFileBytes, "max-file-bytes", opts.Limits.MaxFileBytes, "maximum bytes per file")
		flags.IntVar(&opts.Limits.MaxDepth, "max-depth", opts.Limits.MaxDepth, "maximum folder depth")
		if err := flags.Parse(args); err != nil {
			return failf(ReasonInvalidArguments, "%v", err)
		}
		if opts.Source == "" || opts.Destination == "" {
			return failf(ReasonInvalidArguments, "--src and --dst are required")
		}
		opts.PathEntries = splitList(pathEntries)
		summary, err := Copy(opts)
		if err != nil {
			return err
		}
		if summary.AlreadyComplete {
			_, _ = fmt.Fprintf(stdout, "OK already-complete entries=%d bytes=%d arch=%s\n", summary.Entries, summary.Bytes, summary.Arch)
			return nil
		}
		_, _ = fmt.Fprintf(stdout, "OK entries=%d bytes=%d arch=%s\n", summary.Entries, summary.Bytes, summary.Arch)
		return nil
	case CheckSubcommand:
		flags := flag.NewFlagSet(name, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		var mounts mountFlags
		arch := flags.String("arch", "", "expected ELF architecture (amd64 or arm64); defaults to the node architecture")
		flags.Var(&mounts, "mount", "<mountPath>:<entry,entry> (repeatable)")
		if err := flags.Parse(args); err != nil {
			return failf(ReasonInvalidArguments, "%v", err)
		}
		if len(mounts) == 0 {
			return failf(ReasonInvalidArguments, "at least one --mount is required")
		}
		for _, mount := range mounts {
			if err := CheckMounted(mount.mountPath, mount.entries, *arch); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintf(stdout, "OK checked=%d\n", len(mounts))
		return nil
	default:
		return failf(ReasonInvalidArguments, "unknown toolbox subcommand %q", name)
	}
}

type mountFlag struct {
	mountPath string
	entries   []string
}

type mountFlags []mountFlag

func (m *mountFlags) String() string {
	parts := make([]string, 0, len(*m))
	for _, mount := range *m {
		parts = append(parts, mount.mountPath+":"+strings.Join(mount.entries, ","))
	}
	return strings.Join(parts, " ")
}

func (m *mountFlags) Set(value string) error {
	mountPath, entries, _ := strings.Cut(value, ":")
	if strings.TrimSpace(mountPath) == "" {
		return fmt.Errorf("mount %q is missing a mount path", value)
	}
	*m = append(*m, mountFlag{mountPath: strings.TrimSpace(mountPath), entries: splitList(entries)})
	return nil
}

// MountFlagValue renders one --mount argument for CheckSubcommand.
func MountFlagValue(mountPath string, pathEntries []string) string {
	return mountPath + ":" + strings.Join(pathEntries, ",")
}

func splitList(value string) []string {
	var result []string
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func writeTerminationLog(line string) {
	file, err := os.OpenFile(TerminationLogPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return
	}
	_, _ = file.WriteString(line + "\n")
	_ = file.Close()
}
