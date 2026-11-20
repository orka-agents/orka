package operatoraccess

import (
	"strings"
	"testing"
	"time"
)

func TestClassAccessDefaultsToNone(t *testing.T) {
	cases := map[string]AccessLevel{"": AccessNone, "true": AccessNone, "EXEC": AccessNone, "read": AccessRead, "exec": AccessExec, " exec ": AccessExec}
	for value, want := range cases {
		if got := ParseClassAccess(map[string]string{ClassAccessAnnotation: value}); got != want {
			t.Fatalf("ParseClassAccess(%q) = %v, want %v", value, got, want)
		}
	}
	if got := ParseClassAccess(nil); got != AccessNone {
		t.Fatalf("absent annotation must disable operator access, got %v", got)
	}
}

func TestAccessLevelAllowed(t *testing.T) {
	for _, tc := range []struct {
		level     AccessLevel
		operation Operation
		want      bool
	}{
		{AccessNone, OperationCopyOut, false},
		{AccessNone, OperationExec, false},
		{AccessRead, OperationCopyOut, true},
		{AccessRead, OperationCopyIn, false},
		{AccessRead, OperationExec, false},
		{AccessExec, OperationExec, true},
		{AccessExec, OperationCopyIn, true},
		{AccessExec, OperationCopyOut, true},
		{AccessExec, Operation("unknown"), false},
	} {
		if got := tc.level.Allowed(tc.operation); got != tc.want {
			t.Fatalf("%v.Allowed(%s) = %v, want %v", tc.level, tc.operation, got, tc.want)
		}
	}
	if OperationCopyOut.Writes() || !OperationCopyIn.Writes() || !OperationExec.Writes() {
		t.Fatal("only exec and copy-in may be write-capable")
	}
}

func TestTaintedTreatsAnyValueAsTainted(t *testing.T) {
	if Tainted(nil) || Tainted(map[string]string{"other": "x"}) {
		t.Fatal("unmarked objects must not be tainted")
	}
	for _, value := range []string{"", "garbage", EncodeTaint(Taint{At: time.Unix(1, 0), Operation: OperationExec, AuditID: "op-1"})} {
		if !Tainted(map[string]string{TaintAnnotation: value}) {
			t.Fatalf("marker value %q must taint", value)
		}
	}
	if encoded := EncodeTaint(Taint{AuditID: "op-1", Operation: OperationCopyIn}); !strings.Contains(encoded, `"auditID":"op-1"`) {
		t.Fatalf("taint marker lost its audit ID: %s", encoded)
	}
}

func TestCleanWorkspacePath(t *testing.T) {
	valid := map[string]string{
		"src/main.go":     "src/main.go",
		"/src/main.go":    "src/main.go",
		"//a//b/./c":      "a/b/c",
		"README":          "README",
		"dir/":            "dir",
		"a/./b":           "a/b",
		"name with space": "name with space",
	}
	for raw, want := range valid {
		got, err := CleanWorkspacePath(raw)
		if err != nil || got != want {
			t.Fatalf("CleanWorkspacePath(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"", "/", ".", "..", "../etc/passwd", "a/../../b", "a/..", "/..", "a\\..\\b", "a\x00b", "a\nb",
		"\xff", strings.Repeat("a", MaxPathBytes+1),
	} {
		if got, err := CleanWorkspacePath(raw); err == nil {
			t.Fatalf("CleanWorkspacePath(%q) = %q, want an error", raw, got)
		}
	}
}

func TestArgvAuditIsRedacted(t *testing.T) {
	argv := []string{"/usr/bin/curl", "-H", "Authorization: Bearer secret-token"}
	digest := ArgvDigest(argv)
	if !strings.HasPrefix(digest, "sha256:") || strings.Contains(digest, "secret") {
		t.Fatalf("unexpected digest %q", digest)
	}
	if digest == ArgvDigest([]string{"/usr/bin/curl"}) {
		t.Fatal("digest must cover every argument")
	}
	if got := RedactedCommand(argv); got != "curl" {
		t.Fatalf("RedactedCommand = %q", got)
	}
}

func TestExecOptionsNormalize(t *testing.T) {
	options := ExecOptions{Argv: []string{"ls"}}
	if err := options.Normalize(); err != nil {
		t.Fatal(err)
	}
	if options.TimeoutSeconds != int64(DefaultExecTimeout/time.Second) || options.MaxOutputBytes != DefaultExecOutputBytes {
		t.Fatalf("defaults not applied: %+v", options)
	}
	clamped := ExecOptions{Argv: []string{"ls"}, TimeoutSeconds: 1 << 40, MaxOutputBytes: 1 << 40, TTY: true, Term: "bad term;"}
	if err := clamped.Normalize(); err != nil {
		t.Fatal(err)
	}
	if clamped.TimeoutSeconds != int64(MaxExecTimeout/time.Second) || clamped.MaxOutputBytes != MaxExecOutputBytes {
		t.Fatalf("bounds not clamped: %+v", clamped)
	}
	if !clamped.Stdin || clamped.Term != "xterm" {
		t.Fatalf("TTY must imply stdin and sanitize TERM: %+v", clamped)
	}
	for _, bad := range []ExecOptions{
		{},
		{Argv: []string{" "}},
		{Argv: []string{"ls", "a\x00b"}},
		{Argv: strings.Split(strings.Repeat("a,", MaxArgs+1), ",")},
		{Argv: []string{strings.Repeat("a", MaxArgvBytes+1)}},
		{Argv: []string{"ls"}, TimeoutSeconds: -1},
		{Argv: []string{"ls"}, MaxOutputBytes: -1},
		{Argv: []string{"ls"}, TTY: true, Rows: MaxTerminalDimension + 1},
	} {
		if err := bad.Normalize(); err == nil {
			t.Fatalf("Normalize(%+v) must fail", bad)
		}
	}
	if got := ClampFileBytes(0); got != DefaultFileBytes {
		t.Fatalf("ClampFileBytes(0) = %d", got)
	}
	if got := ClampFileBytes(MaxFileBytes * 2); got != MaxFileBytes {
		t.Fatalf("ClampFileBytes(huge) = %d", got)
	}
}

func TestFramePrefixesACopyOfThePayload(t *testing.T) {
	payload := []byte("hello")
	frame := Frame(ChannelStdout, payload)
	if len(frame) != 1+len(payload) || frame[0] != ChannelStdout || string(frame[1:]) != "hello" {
		t.Fatalf("frame = %q", frame)
	}
	payload[0] = 'j'
	if string(frame[1:]) != "hello" {
		t.Fatal("frame aliases the caller's payload buffer")
	}
	if empty := Frame(ChannelControl, nil); len(empty) != 1 || empty[0] != ChannelControl {
		t.Fatalf("empty frame = %q", empty)
	}
}
