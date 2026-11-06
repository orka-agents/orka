package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

const operatorTaintNotice = "Note: operator exec and copies into a workspace taint the Task's session: " +
	"it will never be published, reused, checkpointed, or continued.\n"

// operatorExitError carries the remote command's exit status to main.
type operatorExitError struct {
	code    int
	message string
}

func (e *operatorExitError) Error() string { return e.message }

// ExitCode returns the process exit code for the CLI.
func (e *operatorExitError) ExitCode() int { return e.code }

func newTaskExecCmd() *cobra.Command {
	var (
		stdin   bool
		tty     bool
		quiet   bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "exec <task> [-i] [-t] -- <command> [args...]",
		Short: "Run a command in a running Task's execution workspace (operator debugging)",
		Long: `Run one command inside the execution workspace of a running or retained ACP Task.

The command runs as the Task session's own unprivileged child identity, never as
the supervisor, and is bounded by --timeout and an output limit. It requires the
--operator-workspace-access-enabled controller flag, an ExecutionWorkspaceClass
opted in with the acp.workspace.orka.ai/operator-access=exec annotation, and the
"create" permission on the tasks/exec subresource.

Every exec taints the Task's session: it will never be published, reused,
checkpointed for forks, or continued. Every exec is audited.`,
		Example: `  orka task exec my-task -- git status
  orka task exec my-task -it -- bash`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 1 {
				return fmt.Errorf("separate the command from the task name with --, for example: orka task exec my-task -- ls")
			}
			taskName, argv := args[0], args[1:]
			if tty {
				stdin = true
			}
			options := operatoraccess.ExecOptions{
				Argv: argv, TTY: tty, Stdin: stdin, TimeoutSeconds: int64(timeout / time.Second),
			}
			if tty {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("-t requires stdin to be a terminal")
				}
				options.Term = os.Getenv("TERM")
				if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
					options.Rows, options.Cols = clampDimension(rows), clampDimension(cols)
				}
			}
			if !quiet {
				fmt.Fprint(os.Stderr, operatorTaintNotice)
			}
			c := newClientFromCmd(cmd)
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			controls := make(chan operatoraccess.Control, 8)

			var restore func()
			if tty {
				state, err := term.MakeRaw(int(os.Stdin.Fd()))
				if err != nil {
					return fmt.Errorf("set terminal raw mode: %w", err)
				}
				restore = func() { _ = term.Restore(int(os.Stdin.Fd()), state) }
				defer restore()
				stopResize := watchTerminalResize(ctx, controls)
				defer stopResize()
			} else {
				// Without a terminal, Ctrl-C is forwarded to the remote process
				// group as SIGINT; a second Ctrl-C abandons the stream locally.
				interrupts := make(chan os.Signal, 2)
				signal.Notify(interrupts, os.Interrupt)
				defer signal.Stop(interrupts)
				go func() {
					forwarded := false
					for {
						select {
						case <-ctx.Done():
							return
						case <-interrupts:
							if forwarded {
								cancel()
								return
							}
							forwarded = true
							select {
							case controls <- operatoraccess.Control{Type: operatoraccess.ControlSignal, Signal: "INT"}:
							default:
							}
						}
					}
				}()
			}
			var input io.Reader
			if stdin {
				input = os.Stdin
			}
			status, err := c.OperatorExec(ctx, taskName, options, clientExecStreams(input, controls))
			if restore != nil {
				restore()
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return &operatorExitError{code: 130, message: "interrupted"}
				}
				return err
			}
			return operatorStatusError(status)
		},
	}
	// The global --token flag owns the -t shorthand. Re-declaring "token"
	// locally (without a shorthand) shadows it for this command only, so -t
	// can mean --tty as operators expect from kubectl exec -it.
	cmd.Flags().String("token", "", "Bearer token for authentication")
	cmd.Flags().BoolVarP(&stdin, "stdin", "i", false, "Pass stdin to the command")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "Allocate a terminal for the command (implies -i)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Do not print the session taint notice")
	cmd.Flags().DurationVar(&timeout, "timeout", operatoraccess.DefaultExecTimeout,
		fmt.Sprintf("Maximum command duration (at most %s)", operatoraccess.MaxExecTimeout))
	return cmd
}

func clampDimension(value int) uint16 {
	if value <= 0 {
		return 0
	}
	if value > operatoraccess.MaxTerminalDimension {
		return operatoraccess.MaxTerminalDimension
	}
	return uint16(value)
}

func operatorStatusError(status operatoraccess.Status) error {
	switch status.Reason {
	case operatoraccess.ExitReasonExited:
		if status.ExitCode == 0 {
			return nil
		}
		return &operatorExitError{code: status.ExitCode, message: fmt.Sprintf("command exited with status %d", status.ExitCode)}
	case operatoraccess.ExitReasonSignaled:
		return &operatorExitError{code: status.ExitCode, message: fmt.Sprintf("command terminated by signal (status %d)", status.ExitCode)}
	case operatoraccess.ExitReasonTimeout:
		return &operatorExitError{code: 124, message: "command exceeded its timeout and was killed"}
	case operatoraccess.ExitReasonOutputLimit:
		return &operatorExitError{code: 125, message: "command exceeded the output limit and was killed"}
	default:
		message := strings.TrimSpace(status.Message)
		if message == "" {
			message = "operator exec failed (" + status.Reason + ")"
		}
		return &operatorExitError{code: 126, message: message}
	}
}

// operatorCopyTarget splits "<task>:<path>". Only a valid Task name before
// the first colon counts, so local paths such as ./a:b stay local.
func operatorCopyTarget(arg string) (string, string, bool) {
	name, path, ok := strings.Cut(arg, ":")
	if !ok || name == "" || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return "", "", false
	}
	return name, path, true
}

func newTaskCopyCmd() *cobra.Command {
	var (
		maxBytes int64
		quiet    bool
	)
	cmd := &cobra.Command{
		Use:   "cp <task>:<path> <local> | <local> <task>:<path>",
		Short: "Copy one file out of or into a Task's execution workspace (operator debugging)",
		Long: `Copy a single regular file between the local machine and the execution
workspace of a running or retained ACP Task. Workspace paths are relative to the
workspace root (the agent's working directory); a leading "/" names that root.
Use "-" as the local path for stdout or stdin.

Copying out requires the class opt-in "read" or "exec" and the "get" permission
on tasks/files; it does not taint the session. Copying in requires the class
opt-in "exec" and the "update" permission on tasks/files, and taints the session:
it will never be published, reused, checkpointed, or continued.`,
		Example: `  orka task cp my-task:src/main.go ./main.go
  orka task cp ./fix.patch my-task:/tmp-fix.patch`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcTask, srcPath, srcRemote := operatorCopyTarget(args[0])
			dstTask, dstPath, dstRemote := operatorCopyTarget(args[1])
			if srcRemote == dstRemote {
				return fmt.Errorf("exactly one of the source and destination must be <task>:<path>")
			}
			c := newClientFromCmd(cmd)
			ctx := cmd.Context()
			if srcRemote {
				cleaned, err := operatoraccess.CleanWorkspacePath(srcPath)
				if err != nil {
					return fmt.Errorf("invalid workspace path: %w", err)
				}
				return copyOutOfWorkspace(ctx, cmd, c.OperatorCopyFrom, srcTask, cleaned, args[1], maxBytes)
			}
			cleaned, err := operatoraccess.CleanWorkspacePath(dstPath)
			if err != nil {
				return fmt.Errorf("invalid workspace path: %w", err)
			}
			if !quiet {
				fmt.Fprint(os.Stderr, operatorTaintNotice)
			}
			return copyIntoWorkspace(ctx, cmd, c.OperatorCopyTo, args[0], dstTask, cleaned)
		},
	}
	cmd.Flags().Int64Var(&maxBytes, "max-bytes", 0,
		fmt.Sprintf("Refuse files larger than this when copying out (default %d, at most %d)", operatoraccess.DefaultFileBytes, operatoraccess.MaxFileBytes))
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Do not print the session taint notice")
	return cmd
}

type copyFromFunc func(ctx context.Context, task, path string, maxBytes int64, w io.Writer) (int64, error)
type copyToFunc func(ctx context.Context, task, path string, r io.Reader, size int64) (int64, error)

func copyOutOfWorkspace(ctx context.Context, cmd *cobra.Command, copyFrom copyFromFunc, task, path, local string, maxBytes int64) error {
	if local == "-" {
		_, err := copyFrom(ctx, task, path, maxBytes, cmd.OutOrStdout())
		return err
	}
	if info, err := os.Stat(local); err == nil && info.IsDir() {
		local = filepath.Join(local, filepath.Base(filepath.FromSlash(path)))
	}
	// Write to a temporary sibling and rename only after a complete copy, so
	// a refused or truncated transfer never clobbers the local file.
	temporary, err := os.CreateTemp(filepath.Dir(local), "."+filepath.Base(local)+".orka-cp-*")
	if err != nil {
		return fmt.Errorf("create local file: %w", err)
	}
	defer os.Remove(temporary.Name()) //nolint:errcheck
	copied, copyErr := copyFrom(ctx, task, path, maxBytes, temporary)
	closeErr := temporary.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return fmt.Errorf("write local file: %w", closeErr)
	}
	if err := os.Rename(temporary.Name(), local); err != nil {
		return fmt.Errorf("write local file: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "copied %d bytes to %s\n", copied, local)
	return nil
}

func copyIntoWorkspace(ctx context.Context, cmd *cobra.Command, copyTo copyToFunc, local, task, path string) error {
	var (
		reader io.Reader
		size   int64
	)
	if local == "-" {
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), operatoraccess.MaxFileBytes+1))
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		if int64(len(data)) > operatoraccess.MaxFileBytes {
			return fmt.Errorf("stdin exceeds %d bytes", operatoraccess.MaxFileBytes)
		}
		reader, size = strings.NewReader(string(data)), int64(len(data))
	} else {
		file, err := os.Open(local)
		if err != nil {
			return fmt.Errorf("open local file: %w", err)
		}
		defer file.Close() //nolint:errcheck
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("inspect local file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file; operator copies transfer single files", local)
		}
		if info.Size() > operatoraccess.MaxFileBytes {
			return fmt.Errorf("%s is %d bytes; the limit is %d", local, info.Size(), operatoraccess.MaxFileBytes)
		}
		reader, size = file, info.Size()
	}
	written, err := copyTo(ctx, task, path, reader, size)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "copied %d bytes to %s:%s\n", written, task, path)
	return nil
}
