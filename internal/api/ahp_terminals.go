/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
	executionevents "github.com/orka-agents/orka/internal/events"
)

const (
	ahpTerminalScheme     = "ahp-terminal:"
	ahpTerminalFlushEvery = 20 * time.Millisecond
	// ahpTerminalCloseFlushTries bounds the tries of a terminal's last output:
	// together a little over ten seconds.
	ahpTerminalCloseFlushTries = 16
	ahpTerminalChunkMax        = 32 << 10
	// ahpTerminalHoldMax bounds the output kept while the journal cannot
	// take it; past it the shell's output waits.
	ahpTerminalHoldMax  = 8 * ahpTerminalChunkMax
	ahpTerminalInputMax = 64 << 10
	ahpTerminalsPerUser = 32
)

// ahpTerminal is one shell in a user's workspace.
type ahpTerminal struct {
	ctx     context.Context
	channel string
	title   string
	claim   ahptypes.TerminalClaim
	input   chan []byte
	sizes   chan remotecommand.TerminalSize
	cancel  context.CancelFunc
	// pending counts input admitted and not yet written, so admission can
	// refuse input instead of a full queue dropping it.
	pending atomic.Int64
	// pidFile records the shell's session in the workspace, so ending the
	// terminal can stop every process it started.
	pidFile string
}

const ahpTerminalInputQueue = 512

// Next implements remotecommand.TerminalSizeQueue. It returns nil once the
// terminal ends, which releases the resize worker.
func (t *ahpTerminal) Next() *remotecommand.TerminalSize {
	select {
	case size := <-t.sizes:
		return &size
	case <-t.ctx.Done():
		return nil
	}
}

func ahpTerminalKey(userKey, channel string) string { return userKey + "\x00" + channel }

// hasTerminals reports whether the user has a terminal running.
func (b *ahpBackend) hasTerminals(userKey string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.shells {
		if strings.HasPrefix(key, userKey+"\x00") {
			return true
		}
	}
	return false
}

func (b *ahpBackend) terminal(userKey, channel string) *ahpTerminal {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.shells[ahpTerminalKey(userKey, channel)]
}

// CreateTerminal starts a shell in the user's workspace.
func (b *ahpBackend) CreateTerminal(ctx context.Context, principal ahphost.Principal, params ahptypes.CreateTerminalParams, emit ahphost.Emitter) (ahptypes.TerminalState, error) {
	if !b.workspaces.enabled() {
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "terminals are not enabled on this host"}
	}
	if !strings.HasPrefix(params.Channel, ahpTerminalScheme) {
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "terminal URIs use the ahp-terminal scheme"}
	}
	// A name is the terminal's first title, so it is bounded as titles are.
	if params.Name != nil && len(*params.Name) > ahpTerminalTitleMax {
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "the name is too long"}
	}
	cwd := ahpWorkspaceRoot
	if params.Cwd != nil {
		if dir, ok := ahpWorkspacePath(*params.Cwd); ok {
			cwd = dir
		}
	}
	title := "Terminal"
	if params.Name != nil && strings.TrimSpace(*params.Name) != "" {
		title = *params.Name
	}
	termCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	term := &ahpTerminal{
		ctx: termCtx, channel: params.Channel, title: title, claim: params.Claim, cancel: cancel,
		input: make(chan []byte, ahpTerminalInputQueue), sizes: make(chan remotecommand.TerminalSize, 4),
		pidFile: ahpTerminalPIDFile(principal.UserKey, params.Channel),
	}
	if params.Cols != nil && params.Rows != nil && *params.Cols > 0 && *params.Rows > 0 {
		term.sizes <- remotecommand.TerminalSize{Width: uint16(min(*params.Cols, 1000)), Height: uint16(min(*params.Rows, 1000))} //nolint:gosec // bounded above
	}
	// Claim the terminal's slot before anything starts, so concurrent creates
	// can neither replace a running shell nor pass the limit together, and
	// hold the user's lifecycle lock until the shell runs, so disposing the
	// last session cannot remove the workspace under it.
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	key := ahpTerminalKey(principal.UserKey, params.Channel)
	b.mu.Lock()
	count := 0
	for existing := range b.shells {
		if strings.HasPrefix(existing, principal.UserKey+"\x00") {
			count++
		}
	}
	_, taken := b.shells[key]
	if _, used := b.retired[key]; used {
		taken = true
	}
	if taken || count >= ahpTerminalsPerUser {
		b.mu.Unlock()
		cancel()
		if taken {
			return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "the terminal URI is already used; use a new one"}
		}
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "too many terminals are open"}
	}
	b.shells[key] = term
	b.mu.Unlock()
	pod, err := b.workspaces.ensure(ctx, principal, ahpWorkspaceStartWait)
	if err != nil {
		b.mu.Lock()
		if b.shells[key] == term {
			delete(b.shells, key)
		}
		b.mu.Unlock()
		cancel()
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: err.Error()}
	}
	if err := b.workspaces.prepare(ctx, principal, pod, cwd); err != nil {
		b.mu.Lock()
		if b.shells[key] == term {
			delete(b.shells, key)
		}
		b.mu.Unlock()
		cancel()
		if errors.Is(err, errAHPNoSuchDirectory) {
			return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "the terminal's directory does not exist in the workspace"}
		}
		log.Error(err, "prepare an AHP terminal's directory", "directory", cwd)
		return ahptypes.TerminalState{}, &ahphost.Rejection{Reason: "the terminal's directory could not be set up; try again"}
	}

	stdin, stdinWriter := io.Pipe()
	// Ending the terminal unblocks a write the exec stream will never read.
	context.AfterFunc(termCtx, func() {
		_ = stdinWriter.CloseWithError(io.EOF)
		_ = stdin.Close()
	})
	go func() {
		defer func() { _ = stdinWriter.Close() }()
		for {
			select {
			case <-termCtx.Done():
				return
			case data := <-term.input:
				_, err := stdinWriter.Write(data)
				term.pending.Add(-1)
				if err != nil {
					return
				}
			}
		}
	}()
	output := newAHPTerminalOutput(termCtx, emit, params.Channel, func() []string { return b.workspaces.tokensFor(principal.UserKey) })
	go func() {
		// The shell leads its own session; its PID names that session, so
		// ending the terminal can stop everything started in it.
		err := b.workspaces.stream(termCtx, pod,
			[]string{ahpShell, "-c", `dir=$1 pidfile=$2; ` + ahpTerminalRecordSession + `; cd "$dir" 2>/dev/null || cd /workspace; exec /bin/sh -i`,
				"sh", cwd, term.pidFile},
			stdin, output, true, term)
		output.close()
		var exitCode *int64
		var exit interface{ ExitStatus() int }
		if errors.As(err, &exit) {
			code := int64(exit.ExitStatus())
			exitCode = &code
		} else if err == nil {
			zero := int64(0)
			exitCode = &zero
		}
		b.endTerminal(context.WithoutCancel(ctx), principal, term, emit, exitCode)
	}()
	cwdURI := ahpDirectoryURI(cwd)
	isPty := true
	state := ahptypes.TerminalState{
		Title: title, Cwd: &cwdURI, Cols: params.Cols, Rows: params.Rows, Content: []ahptypes.TerminalContentPart{},
		Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}},
		Claim:     params.Claim, IsPty: &isPty,
	}
	return state, nil
}

func (b *ahpBackend) endTerminal(ctx context.Context, principal ahphost.Principal, term *ahpTerminal, emit ahphost.Emitter, exitCode *int64) {
	_ = b.stopShell(ctx, principal, term)
	b.mu.Lock()
	key := ahpTerminalKey(principal.UserKey, term.channel)
	current := b.shells[key] == term
	if current {
		delete(b.shells, key)
		b.retireLocked(key)
	}
	b.mu.Unlock()
	if !current {
		return
	}
	_ = emit.Emit(ctx, term.channel, ahptypes.StateAction{Value: &ahptypes.TerminalExitedAction{Type: ahptypes.ActionTypeTerminalExited, ExitCode: exitCode}})
	if b.updateTerminals != nil {
		_ = b.updateTerminals(ctx, principal)
	}
}

// DisposeTerminal stops a shell and every process it started, then frees its slot.
// A stop that fails is returned, and the terminal kept, so disposing of it
// again finishes: its stream may be gone by then, but the record of its
// session in the workspace stays until the session is stopped.
func (b *ahpBackend) DisposeTerminal(ctx context.Context, principal ahphost.Principal, channel string) error {
	term := b.terminal(principal.UserKey, channel)
	pidFile := ahpTerminalPIDFile(principal.UserKey, channel)
	if term != nil {
		term.cancel()
		pidFile = term.pidFile
	}
	if err := b.stopSession(ctx, principal, pidFile); err != nil {
		return fmt.Errorf("stop the terminal's shell: %w", err)
	}
	if term == nil {
		return nil
	}
	b.mu.Lock()
	key := ahpTerminalKey(principal.UserKey, channel)
	if b.shells[key] == term {
		delete(b.shells, key)
		b.retireLocked(key)
	}
	b.mu.Unlock()
	return nil
}

const (
	ahpRetiredTerminalsMax = 4096
	ahpRetiredTerminalTTL  = 24 * time.Hour
)

// retireLocked remembers a used terminal URI. Clients name terminals with
// fresh random URIs, so refusing reuse costs them nothing. b.mu must be held.
func (b *ahpBackend) retireLocked(key string) {
	if b.retired == nil {
		b.retired = map[string]time.Time{}
	}
	now := b.now()
	if len(b.retired) >= ahpRetiredTerminalsMax {
		for old, at := range b.retired {
			if now.Sub(at) > ahpRetiredTerminalTTL {
				delete(b.retired, old)
			}
		}
	}
	b.retired[key] = now
}

// stopShell ends a terminal's exec stream and kills its session in the
// workspace: closing the stream alone can leave the shell running there.
func (b *ahpBackend) stopShell(ctx context.Context, principal ahphost.Principal, term *ahpTerminal) error {
	term.cancel()
	return b.stopSession(ctx, principal, term.pidFile)
}

// stopSession kills the session a terminal's record names in the workspace,
// if it still runs. A workspace that is not running runs nothing.
func (b *ahpBackend) stopSession(ctx context.Context, principal ahphost.Principal, pidFile string) error {
	pod, ok := b.workspaces.running(ctx, principal)
	if !ok {
		return nil
	}
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return b.workspaces.run(killCtx, pod, ahpShell, "-c", ahpStopSessionScript, "sh", pidFile, "stop")
}

// ahpTerminalPIDFile is where a terminal's shell records its session in the
// workspace, so the terminal can be stopped, also by a later process.
func ahpTerminalPIDFile(userKey, channel string) string {
	return "/tmp/orka-terminal-" + ahpHash(userKey, channel)[:16] + ".pid"
}

// The host stops the shells of terminals an earlier process left behind
// through this backend.
var _ ahphost.TerminalReaper = (*ahpBackend)(nil)

// ReapTerminals stops the shells of terminals an earlier process left running
// in the user's workspace, which outlives the process. A session ID one of
// this process's terminals now holds was reused, and is left alone. A failure
// is returned, so the host keeps the terminals' channels and tries again.
func (b *ahpBackend) ReapTerminals(ctx context.Context, principal ahphost.Principal, channels []string) error {
	pod, err := b.workspaces.runningForCleanup(ctx, principal)
	if err != nil || pod == nil {
		return err
	}
	b.mu.Lock()
	var live []string
	for key, term := range b.shells {
		if strings.HasPrefix(key, principal.UserKey+"\x00") {
			live = append(live, term.pidFile)
		}
	}
	b.mu.Unlock()
	var failed error
	for _, channel := range channels {
		killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		argv := append([]string{ahpShell, "-c", ahpStopSessionScript, "sh", ahpTerminalPIDFile(principal.UserKey, channel), "reap"}, live...)
		failed = errors.Join(failed, b.workspaces.run(killCtx, pod, argv...))
		cancel()
	}
	return failed
}

// ahpTerminalRecordSession records, in the file $pidfile names, the shell's
// PID, which names the session it leads, and the shell's start time, which
// tells that session from a later one that reuses the ID. It replaces the
// positional parameters.
const ahpTerminalRecordSession = `read -r stat < /proc/$$/stat; set -- ${stat##*) }; echo "$$ ${20}" > "$pidfile"`

// ahpStopSessionScript kills every process in the session the file $1
// records, then removes the file. $2 is stop for a terminal of this process,
// or reap for one an earlier process left behind; further arguments are the
// files of terminals still running, whose session IDs are never killed.
//
// The leader's start time proves the session is the recorded one. A leftover
// record whose leader is gone has no owner to prove, so it is not reaped. A
// running terminal's shell can exit before what it started; those processes
// still hold the session ID, which Linux then cannot reuse, so stop kills
// them. It reads /proc with shell builtins, so a workspace image needs no
// more than sh: pkill is not one of its requirements.
const ahpStopSessionScript = `file=$1 mode=$2
shift 2
[ -f "$file" ] || exit 0
read -r sid started < "$file" || :
case "$sid" in ''|*[!0-9]*) rm -f "$file"; exit 0;; esac
for live do
  read -r other _ < "$live" 2>/dev/null || continue
  if [ "$other" = "$sid" ]; then rm -f "$file"; exit 0; fi
done
if read -r line < "/proc/$sid/stat" 2>/dev/null; then
  # Fields after the parenthesized command: the start time is the 20th.
  set -- ${line##*) }
  if [ -z "$started" ] || [ "${20}" != "$started" ]; then rm -f "$file"; exit 0; fi
elif [ "$mode" != stop ]; then
  rm -f "$file"
  exit 0
fi
for stat in /proc/[0-9]*/stat; do
  read -r line < "$stat" 2>/dev/null || continue
  # Fields after the parenthesized command: state, ppid, pgrp, session.
  set -- ${line##*) }
  [ "$4" = "$sid" ] || continue
  pid=${stat#/proc/}
  kill -KILL "${pid%/stat}" 2>/dev/null || :
done
rm -f "$file"`

// Terminals lists the user's running shells.
func (b *ahpBackend) Terminals(_ context.Context, principal ahphost.Principal) ([]ahptypes.TerminalInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var terminals []ahptypes.TerminalInfo
	for key, term := range b.shells {
		if !strings.HasPrefix(key, principal.UserKey+"\x00") {
			continue
		}
		terminals = append(terminals, ahptypes.TerminalInfo{
			Resource: term.channel, Title: term.title, Claim: term.claim,
			Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}},
		})
	}
	sort.Slice(terminals, func(i, j int) bool { return terminals[i].Resource < terminals[j].Resource })
	return terminals, nil
}

// authorizeTerminalAction admits input and resizes for the user's own shells.
func (b *ahpBackend) authorizeTerminalAction(principal ahphost.Principal, channel string, action ahptypes.StateAction) error {
	term := b.terminal(principal.UserKey, channel)
	if term == nil {
		return &ahphost.Rejection{Reason: "the terminal is not running"}
	}
	switch a := action.Value.(type) {
	case *ahptypes.TerminalInputAction:
		if len(a.Data) > ahpTerminalInputMax {
			return &ahphost.Rejection{Reason: "the input is too large"}
		}
		// Reserve room for the input now, on the terminal just checked: once
		// admitted it is always delivered.
		if term.pending.Add(1) > ahpTerminalInputQueue {
			term.pending.Add(-1)
			return &ahphost.Rejection{Reason: "the terminal is not reading input; try again"}
		}
		return nil
	case *ahptypes.TerminalResizedAction:
		if a.Cols < 1 || a.Rows < 1 || a.Cols > 1000 || a.Rows > 1000 {
			return &ahphost.Rejection{Reason: "the size is not valid"}
		}
		return nil
	case *ahptypes.TerminalTitleChangedAction:
		if len(a.Title) > ahpTerminalTitleMax {
			return &ahphost.Rejection{Reason: "the title is too long"}
		}
		return nil
	default:
		return &ahphost.Rejection{Reason: "not supported on this host"}
	}
}

// ReleaseClientAction gives back the input room an admitted terminal input
// reserved when the host could not apply it, so a storage outage cannot fill
// the queue with input that will never be written.
func (b *ahpBackend) ReleaseClientAction(_ context.Context, principal ahphost.Principal, channel string, action ahptypes.StateAction) {
	if _, ok := action.Value.(*ahptypes.TerminalInputAction); !ok {
		return
	}
	if term := b.terminal(principal.UserKey, channel); term != nil {
		term.pending.Add(-1)
	}
}

// handleTerminalAction delivers input and resizes in the order they came.
func (b *ahpBackend) handleTerminalAction(principal ahphost.Principal, channel string, action ahptypes.StateAction) {
	term := b.terminal(principal.UserKey, channel)
	if term == nil {
		return
	}
	switch a := action.Value.(type) {
	case *ahptypes.TerminalInputAction:
		// Admission reserved room, so this never blocks.
		select {
		case term.input <- []byte(a.Data):
		case <-term.ctx.Done():
		}
		b.workspaces.refreshCredentialForTerminal(principal)
	case *ahptypes.TerminalResizedAction:
		select {
		case term.sizes <- remotecommand.TerminalSize{Width: uint16(a.Cols), Height: uint16(a.Rows)}: //nolint:gosec // validated at admission
		default:
		}
	case *ahptypes.TerminalTitleChangedAction:
		// The terminal list is built from the backend's terminals, so a
		// renamed terminal keeps its name there too.
		b.mu.Lock()
		term.title = a.Title
		b.mu.Unlock()
		if b.updateTerminals != nil {
			_ = b.updateTerminals(term.ctx, principal)
		}
	}
}

// ahpTerminalOutput turns a shell's output into terminal/data actions,
// coalesced and cut on UTF-8 boundaries. Terminal data is journaled, so it
// never carries one of the workspace's git tokens, nor
// ahpMinRedactedToken or more leading bytes of a token's secret, however the
// output is split, delayed, or cut short.
type ahpTerminalOutput struct {
	ctx     context.Context
	emit    ahphost.Emitter
	channel string
	// credentials returns the tokens the workspace holds.
	credentials func() []string

	mu      sync.Mutex
	pending []byte
	// recent ends what was emitted, so a token that began there is still
	// recognized as the rest of it arrives.
	recent []byte
	timer  *time.Timer
	closed bool
}

func newAHPTerminalOutput(ctx context.Context, emit ahphost.Emitter, channel string, credentials func() []string) *ahpTerminalOutput {
	return &ahpTerminalOutput{ctx: ctx, emit: emit, channel: channel, credentials: credentials}
}

// ahpTerminalTitleMax bounds a terminal's title.
const ahpTerminalTitleMax = 256

// ahpMinRedactedToken is the shortest run of a token's secret that output
// never carries. Shorter values are not treated as tokens: replacing them
// would mangle ordinary output.
const ahpMinRedactedToken = 8

// ahpTokenHoldMin is the shortest start of a token's secret that output waits
// on for more: shorter ones are too common in ordinary output to delay it.
const ahpTokenHoldMin = 4

// ahpTokenPublicStart is the length of a token's type prefix through its
// first underscore, such as "ghu_", which reveals nothing about the token.
func ahpTokenPublicStart(token string) int {
	return strings.IndexByte(token, '_') + 1
}

// ahpTokenSecret is the part of a token after its public start, or "" for a
// token too short to redact.
func ahpTokenSecret(token string) string {
	if secret := token[ahpTokenPublicStart(token):]; len(secret) >= ahpMinRedactedToken {
		return secret
	}
	return ""
}

// ahpRedactTokens replaces what pending carries of a token, reading pending
// after recent, the end of what was already emitted. Each run that begins
// with ahpMinRedactedToken or more bytes of a token's secret is replaced,
// with the token's public start before it, however the run ends; a run that
// reaches pending's end and could go on is left for ahpTokenHeld.
func ahpRedactTokens(recent, pending []byte, tokens []string) []byte {
	data := append(slices.Clip(recent), pending...)
	var spans [][2]int
	for _, token := range tokens {
		secret := ahpTokenSecret(token)
		if secret == "" {
			continue
		}
		public := token[:ahpTokenPublicStart(token)]
		lead := []byte(secret[:ahpMinRedactedToken])
		for i := 0; ; {
			at := bytes.Index(data[i:], lead)
			if at < 0 {
				break
			}
			at += i
			i = at + 1
			end := at + len(lead)
			for end < len(data) && end-at < len(secret) && data[end] == secret[end-at] {
				end++
			}
			if end == len(data) && end-at < len(secret) {
				continue
			}
			from := at
			if bytes.HasSuffix(data[:at], []byte(public)) {
				from -= len(public)
			}
			spans = append(spans, [2]int{from, end})
		}
	}
	if len(spans) == 0 {
		return pending
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	out := make([]byte, 0, len(pending))
	// at is the next byte of data to copy; replaced is where the last
	// replacement ended, so overlapping runs are replaced once.
	at, replaced := len(recent), -1
	for _, span := range spans {
		from, end := span[0], span[1]
		if end <= at {
			continue
		}
		if from > replaced {
			from = max(from, at)
			out = append(out, data[at:from]...)
			out = append(out, executionevents.ExecutionEventRedactedValue...)
		}
		at, replaced = end, end
	}
	return append(out, data[at:]...)
}

// ahpTokenHeld says how much of pending's end, read after recent, is the
// start of a token's secret, at least ahpTokenHoldMin bytes of it: that much
// waits for more output, however long.
func ahpTokenHeld(recent, pending []byte, tokens []string) int {
	data := append(slices.Clip(recent), pending...)
	held := 0
	for _, token := range tokens {
		secret := ahpTokenSecret(token)
		for k := min(len(secret)-1, len(data)); k >= ahpTokenHoldMin; k-- {
			if bytes.HasSuffix(data, []byte(secret[:k])) {
				held = max(held, k)
				break
			}
		}
	}
	return min(held, len(pending))
}

func (o *ahpTerminalOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, io.ErrClosedPipe
	}
	o.pending = append(o.pending, p...)
	if len(o.pending) >= ahpTerminalChunkMax {
		o.flushLocked()
	} else {
		o.scheduleLocked()
	}
	// Output the journal could not take yet stays pending. Past the hold
	// bound the shell waits for it, rather than output being dropped or held
	// without end.
	for len(o.pending) > ahpTerminalHoldMax {
		o.mu.Unlock()
		select {
		case <-o.ctx.Done():
			o.mu.Lock()
			return 0, io.ErrClosedPipe
		case <-time.After(ahpTerminalFlushEvery):
		}
		o.mu.Lock()
		if o.closed {
			return 0, io.ErrClosedPipe
		}
		o.flushLocked()
	}
	return len(p), nil
}

// scheduleLocked flushes the pending output soon, once. o.mu must be held.
func (o *ahpTerminalOutput) scheduleLocked() {
	if o.timer != nil || o.closed {
		return
	}
	o.timer = time.AfterFunc(ahpTerminalFlushEvery, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.timer = nil
		o.flushLocked()
	})
}

func (o *ahpTerminalOutput) flushLocked() {
	var tokens []string
	if o.credentials != nil {
		tokens = o.credentials()
	}
	ready := len(o.pending)
	if len(tokens) > 0 {
		o.pending = ahpRedactTokens(o.recent, o.pending, tokens)
		ready = len(o.pending)
		if held := ahpTokenHeld(o.recent, o.pending, tokens); held > 0 && o.closed {
			// The output ended inside a token: what arrived of it is
			// still secret.
			o.pending = append(o.pending[:ready-held], executionevents.ExecutionEventRedactedValue...)
			ready = len(o.pending)
		} else {
			ready -= held
		}
	}
	for ready > 0 {
		cut := min(ready, ahpTerminalChunkMax)
		if cut < ready {
			// Move the cut back to the start of a character, but never past
			// what one character spans: malformed bytes are passed on.
			at := cut
			for back := 0; back < utf8.UTFMax-1 && at > 0 && !utf8.RuneStart(o.pending[at]); back++ {
				at--
			}
			if at > 0 && utf8.RuneStart(o.pending[at]) {
				cut = at
			}
		} else if !o.closed {
			// Hold back a final character whose bytes have not all arrived:
			// at most UTFMax-1 bytes stay pending.
			for back := 1; back < utf8.UTFMax && back <= cut; back++ {
				if i := cut - back; utf8.RuneStart(o.pending[i]) {
					if !utf8.FullRune(o.pending[i:cut]) {
						cut = i
					}
					break
				}
			}
			if cut == 0 {
				return
			}
		}
		data := strings.ToValidUTF8(string(o.pending[:cut]), "\uFFFD")
		if o.emit.Emit(o.ctx, o.channel, ahptypes.StateAction{Value: &ahptypes.TerminalDataAction{Type: ahptypes.ActionTypeTerminalData, Data: data}}) != nil {
			// The journal did not take it: it stays pending, already
			// redacted, and nothing after it goes first.
			o.scheduleLocked()
			return
		}
		o.pending = o.pending[cut:]
		ready -= cut
		o.keepRecent(data, tokens)
	}
}

// keepRecent keeps as much of the end of emitted output as a token could
// have begun in.
func (o *ahpTerminalOutput) keepRecent(data string, tokens []string) {
	keep := 0
	for _, token := range tokens {
		keep = max(keep, len(token)-1)
	}
	o.recent = append(o.recent, data...)
	if len(o.recent) > keep {
		o.recent = slices.Clone(o.recent[len(o.recent)-keep:])
	}
}

// close sends what is pending before the terminal is reported exited. A final
// flush the journal refuses is tried again for a while, since nothing would
// schedule it once the output is closed; a terminal being ended stops trying.
func (o *ahpTerminalOutput) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
	o.closed = true
	wait := ahpTerminalFlushEvery
	for attempt := 0; ; attempt++ {
		o.flushLocked()
		if len(o.pending) == 0 || attempt == ahpTerminalCloseFlushTries || o.ctx.Err() != nil {
			return
		}
		o.mu.Unlock()
		time.Sleep(wait)
		wait = min(2*wait, time.Second)
		o.mu.Lock()
	}
}
