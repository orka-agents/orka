package operatoraccess

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Stream channels. Every WebSocket message is binary: one channel byte
// followed by the payload.
const (
	ChannelStdin   byte = 0
	ChannelStdout  byte = 1
	ChannelStderr  byte = 2
	ChannelControl byte = 3
	ChannelStatus  byte = 4
)

// WebSocketSubprotocol versions the stream framing.
const WebSocketSubprotocol = "orka.operator-exec.v1"

// Exec bounds. The API clamps requested values into these ranges and the
// supervisor enforces them again.
const (
	MaxArgs                = 256
	MaxArgvBytes           = 8 << 10
	DefaultExecTimeout     = 15 * time.Minute
	MaxExecTimeout         = time.Hour
	DefaultExecOutputBytes = 64 << 20
	MaxExecOutputBytes     = 512 << 20
	DefaultFileBytes       = 64 << 20
	MaxFileBytes           = 256 << 20
	MaxFrameBytes          = 64 << 10
	MaxTerminalDimension   = 1000
)

// Control is a client-to-server control message.
type Control struct {
	Type   string `json:"type"`
	Rows   uint16 `json:"rows,omitempty"`
	Cols   uint16 `json:"cols,omitempty"`
	Signal string `json:"signal,omitempty"`
}

const (
	ControlResize = "resize"
	ControlSignal = "signal"
	ControlEOF    = "eof"
)

// AllowedSignals are the signals an operator may forward.
var AllowedSignals = map[string]struct{}{"INT": {}, "TERM": {}, "HUP": {}, "QUIT": {}, "KILL": {}}

// Exit reasons.
const (
	ExitReasonExited      = "exited"
	ExitReasonSignaled    = "signaled"
	ExitReasonTimeout     = "timeout"
	ExitReasonOutputLimit = "output_limit"
	ExitReasonDisconnect  = "disconnected"
	// ExitReasonStdinOverflow means stdin arrived faster than the command
	// consumed it beyond the bounded buffer.
	ExitReasonStdinOverflow = "stdin_overflow"
	ExitReasonError         = "error"
)

// Status is the final server-to-client message.
type Status struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason"`
	Message  string `json:"message,omitempty"`
}

// Frame prefixes payload with its channel.
func Frame(channel byte, payload []byte) []byte {
	out := make([]byte, 1+len(payload))
	out[0] = channel
	copy(out[1:], payload)
	return out
}

// ControlFrame encodes a control message.
func ControlFrame(control Control) []byte {
	raw, _ := json.Marshal(control)
	return Frame(ChannelControl, raw)
}

// StatusFrame encodes the final status.
func StatusFrame(status Status) []byte {
	raw, _ := json.Marshal(status)
	return Frame(ChannelStatus, raw)
}

// ExecOptions is the operator-controlled part of an exec request.
type ExecOptions struct {
	Argv           []string `json:"argv"`
	TTY            bool     `json:"tty,omitempty"`
	Stdin          bool     `json:"stdin,omitempty"`
	Rows           uint16   `json:"rows,omitempty"`
	Cols           uint16   `json:"cols,omitempty"`
	Term           string   `json:"term,omitempty"`
	TimeoutSeconds int64    `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes int64    `json:"maxOutputBytes,omitempty"`
}

// Normalize validates argv and clamps the bounds in place.
func (o *ExecOptions) Normalize() error {
	if len(o.Argv) == 0 || strings.TrimSpace(o.Argv[0]) == "" {
		return fmt.Errorf("a command is required")
	}
	if len(o.Argv) > MaxArgs {
		return fmt.Errorf("command has more than %d arguments", MaxArgs)
	}
	total := 0
	for _, arg := range o.Argv {
		total += len(arg)
		if strings.IndexByte(arg, 0) >= 0 || !utf8.ValidString(arg) {
			return fmt.Errorf("command arguments must be valid UTF-8 without NUL bytes")
		}
	}
	if total > MaxArgvBytes {
		return fmt.Errorf("command arguments exceed %d bytes", MaxArgvBytes)
	}
	switch {
	case o.TimeoutSeconds < 0:
		return fmt.Errorf("timeout must not be negative")
	case o.TimeoutSeconds == 0:
		o.TimeoutSeconds = int64(DefaultExecTimeout / time.Second)
	case o.TimeoutSeconds > int64(MaxExecTimeout/time.Second):
		o.TimeoutSeconds = int64(MaxExecTimeout / time.Second)
	}
	switch {
	case o.MaxOutputBytes < 0:
		return fmt.Errorf("output limit must not be negative")
	case o.MaxOutputBytes == 0:
		o.MaxOutputBytes = DefaultExecOutputBytes
	case o.MaxOutputBytes > MaxExecOutputBytes:
		o.MaxOutputBytes = MaxExecOutputBytes
	}
	if o.Rows > MaxTerminalDimension || o.Cols > MaxTerminalDimension {
		return fmt.Errorf("terminal dimensions exceed %d", MaxTerminalDimension)
	}
	if !o.TTY {
		o.Rows, o.Cols, o.Term = 0, 0, ""
	} else {
		o.Stdin = true
		o.Term = sanitizeTerm(o.Term)
	}
	return nil
}

func sanitizeTerm(term string) string {
	term = strings.TrimSpace(term)
	if term == "" || len(term) > 64 {
		return "xterm"
	}
	for i := 0; i < len(term); i++ {
		c := term[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '+' {
			continue
		}
		return "xterm"
	}
	return term
}

// ClampFileBytes clamps a requested file size bound.
func ClampFileBytes(requested int64) int64 {
	switch {
	case requested <= 0:
		return DefaultFileBytes
	case requested > MaxFileBytes:
		return MaxFileBytes
	default:
		return requested
	}
}
