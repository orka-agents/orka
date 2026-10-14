package nativesession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

const (
	copilotProviderKind = "copilot"
	// copilotVersion is the Copilot CLI release Orka's runtime runs. It must
	// equal acp.CopilotCLIVersion.
	copilotVersion      = "1.0.96"
	copilotBundleFormat = "orka/copilot-session-v1"
	copilotEventsEntry  = "events.jsonl"
	// copilotSessionStart is the event that begins every event log.
	copilotSessionStart = "session.start"
	// copilotEventsSchema is the events.jsonl schema that session.start
	// records.
	copilotEventsSchema = 1
)

// copilotManifest describes a captured Copilot CLI conversation.
type copilotManifest struct {
	Format string `json:"format"`
	// Version is the Copilot CLI release that started the session.
	Version   string `json:"version"`
	SessionID string `json:"sessionID"`
}

// copilotFormat carries a Copilot CLI session: its append-only events.jsonl.
// Copilot cannot resume a session, but session/load replays the file in any
// working directory. Copilot spills large tool output to TMPDIR, which is
// not carried; the restored conversation keeps its preview. Dirs.State is
// COPILOT_HOME.
type copilotFormat struct{}

// Later Copilot CLI releases load sessions that earlier ones wrote.
func (copilotFormat) Local() Local {
	return Local{
		Binary: "copilot", VersionLine: regexp.MustCompile(`^GitHub Copilot CLI ([0-9]+\.[0-9]+\.[0-9]+)\.$`),
		Release: copilotVersion, NewerReleases: true, StateVar: "COPILOT_HOME", HomeState: ".copilot",
	}
}

// The runtime supervisor sets COPILOT_HOME to HOME/.copilot.
func (copilotFormat) runtimeState(home, _ string) string { return filepath.Join(home, ".copilot") }

func copilotEventsPath(state, providerSessionID string) string {
	return filepath.Join(state, "session-state", providerSessionID, copilotEventsEntry)
}

func (copilotFormat) Capture(_ context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return nil, err
	}
	if !uuidSessionIDPattern.MatchString(providerSessionID) {
		return nil, errors.New("the Copilot CLI session ID is invalid")
	}
	if !absoluteCleanPath(dirs.State) {
		return nil, errors.New("the Copilot CLI state directory must be an absolute, clean path")
	}
	path := copilotEventsPath(dirs.State, providerSessionID)
	if err := requireRealPath(path, false); err != nil {
		return nil, err
	}
	events, err := readBoundedFile(path, expandedLimit(limit))
	if err != nil {
		return nil, err
	}
	version, err := inspectCopilotEvents(events, providerSessionID)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		err = fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if err != nil {
		return nil, err
	}
	data, err := writeBundle(limit, copilotManifest{Format: copilotBundleFormat, Version: version, SessionID: providerSessionID},
		[]bundleFile{{copilotEventsEntry, events}})
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: Copilot CLI conversation exceeds Orka's %d-byte transport limit", ErrUnsupported, limit)
	}
	return data, nil
}

func (copilotFormat) Inspect(_ context.Context, data []byte, maxBytes int) (Summary, error) {
	manifest, _, err := decodeCopilotBundle(data, maxBytes)
	if err != nil {
		return Summary{}, err
	}
	return Summary{ProviderSessionID: manifest.SessionID, ProviderVersion: manifest.Version, DataDigest: dataDigest(data)}, nil
}

// Install writes the conversation into a Copilot CLI state directory that
// does not hold the session. Copilot loads it in the cwd session/load names,
// so nothing is rewritten. A failed install removes what it created, so
// journalDir is unused.
func (copilotFormat) Install(_ context.Context, data []byte, dirs Dirs, _ string, maxBytes int) (err error) {
	manifest, events, err := decodeCopilotBundle(data, maxBytes)
	if err != nil {
		return err
	}
	if !absoluteCleanPath(dirs.State) {
		return errors.New("the Copilot CLI state directory must be an absolute, clean path")
	}
	if err := requireRealPath(filepath.Dir(dirs.State), true); err != nil {
		return err
	}
	var created []string
	defer func() {
		if err != nil {
			for _, path := range slices.Backward(created) {
				_ = os.Remove(path)
			}
		}
	}()
	sessionDir := filepath.Dir(copilotEventsPath(dirs.State, manifest.SessionID))
	for _, dir := range []string{dirs.State, filepath.Dir(sessionDir)} {
		if err = os.Mkdir(dir, 0o700); err == nil {
			created = append(created, dir)
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
		if err = requireRealPath(dir, true); err != nil {
			return err
		}
	}
	destination := filepath.Join(sessionDir, copilotEventsEntry)
	staged := filepath.Join(sessionDir, ".events.jsonl.restore")
	synced := []string{sessionDir, filepath.Dir(sessionDir), dirs.State, filepath.Dir(dirs.State)}
	if err = os.Mkdir(sessionDir, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if err = requireRealPath(sessionDir, true); err != nil {
			return err
		}
		entries, readErr := os.ReadDir(sessionDir)
		switch {
		case readErr != nil:
			return readErr
		case len(entries) == 1 && holdsFile(destination, events):
			// A retry after an install that completed but was not
			// acknowledged finds exactly what it would write, and nothing else.
			return syncDirs(synced...)
		case len(entries) == 0 || len(entries) == 1 && entries[0].Name() == filepath.Base(staged):
			// An install killed before it published the event log left only
			// its own staging, which this one replaces.
			if err = os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		default:
			return errors.New("the Copilot CLI state directory already holds the session")
		}
	}
	created = append(created, sessionDir)
	if err = writeNewFile(staged, events); err != nil {
		return err
	}
	created = append(created, staged)
	if err = os.Rename(staged, destination); err != nil {
		return err
	}
	// Until the directories are synced, a failure still removes the published
	// event log, so a retry finds no partial install.
	created[len(created)-1] = destination
	if err = syncDirs(synced...); err != nil {
		return err
	}
	created = nil
	return nil
}

// copilotEventTypes are the event types Copilot CLI 1.0.96 records: the
// SessionEvent schema that the @github/copilot-sdk release pinning it (1.0.20)
// generates, plus session.usage_record, which its event log also holds.
var copilotEventTypes = map[string]bool{
	"abort": true, "assistant.fusion_phase_activity": true, "assistant.fusion_phase_completed": true,
	"assistant.fusion_phase_failed": true, "assistant.fusion_phase_started": true, "assistant.idle": true,
	"assistant.intent": true, "assistant.message": true, "assistant.message_delta": true,
	"assistant.message_start": true, "assistant.reasoning": true, "assistant.reasoning_delta": true,
	"assistant.server_tool_progress": true, "assistant.streaming_delta": true, "assistant.tool_call_delta": true,
	"assistant.turn_end": true, "assistant.turn_start": true, "assistant.usage": true,
	"auto_mode_switch.completed": true, "auto_mode_switch.requested": true, "capabilities.changed": true,
	"command.completed": true, "command.execute": true, "command.queued": true, "commands.changed": true,
	"elicitation.completed": true, "elicitation.requested": true, "exit_plan_mode.completed": true,
	"exit_plan_mode.requested": true, "external_tool.completed": true, "external_tool.requested": true,
	"hook.end": true, "hook.progress": true, "hook.start": true, "human_response.recorded": true,
	"mcp_app.tool_call_complete": true, "mcp.headers_refresh_completed": true, "mcp.headers_refresh_required": true,
	"mcp.oauth_completed": true, "mcp.oauth_required": true, "mcp.prompts.list_changed": true,
	"mcp.resources.list_changed": true, "mcp.tools.list_changed": true, "model.call_failure": true,
	"model.call_finished": true, "pending_messages.modified": true, "permission.assentDetected": true,
	"permission.carriedForward": true, "permission.completed": true, "permission.contextualAuthorization": true,
	"permission.messageAuthorization": true, "permission.messageAuthorizationDegraded": true,
	"permission.messageAuthorizationRead": true, "permission.requested": true, "sampling.completed": true,
	"sampling.requested": true, "session_limits_exhausted.completed": true, "session_limits_exhausted.requested": true,
	"session.auto_mode_resolved": true, "session.auto_tier_recommendation": true,
	"session.auto_tier_switch_failed": true, "session.autopilot_objective_changed": true,
	"session.background_tasks_changed": true, "session.binary_asset": true, "session.canvas.closed": true,
	"session.canvas.opened": true, "session.canvas.recorded": true, "session.canvas.registry_changed": true,
	"session.canvas.removed": true, "session.canvas.unavailable": true, "session.compaction_complete": true,
	"session.compaction_start": true, "session.completion_receipt": true, "session.context_changed": true,
	"session.context_cleared": true, "session.custom_agents_updated": true, "session.custom_notification": true,
	"session.error": true, "session.extensions_loaded": true, "session.extensions.attachments_pushed": true,
	"session.fusion_completed": true, "session.fusion_resolved": true, "session.fusion_route_failed": true,
	"session.fusion_route_started": true, "session.handoff": true, "session.idle": true, "session.indexed_search": true,
	"session.info": true, "session.managed_plugin_progress": true, "session.managed_settings_enforced": true,
	"session.managed_settings_resolved": true, "session.mcp_server_needs_reconnect": true,
	"session.mcp_server_removed": true, "session.mcp_server_status_changed": true, "session.mcp_servers_loaded": true,
	"session.mode_changed": true, "session.mode_notice_delivered": true, "session.model_change": true,
	"session.model_deselected": true, "session.permission_recovery": true, "session.permissions_changed": true,
	"session.plan_changed": true, "session.quota_observation": true, "session.remote_steerable_changed": true,
	"session.resume": true, "session.schedule_cancelled": true, "session.schedule_created": true,
	"session.schedule_rearmed": true, "session.session_limits_changed": true, "session.shutdown": true,
	"session.skills_loaded": true, "session.snapshot_rewind": true, copilotSessionStart: true,
	"session.task_complete": true, "session.title_changed": true, "session.todos_changed": true,
	"session.tools_updated": true, "session.truncation": true, "session.usage_checkpoint": true,
	"session.usage_info": true, "session.usage_record": true, "session.warning": true,
	"session.workspace_file_changed": true, "skill.invoked": true, "subagent.completed": true,
	"subagent.configured": true, "subagent.deselected": true, "subagent.failed": true, "subagent.selected": true,
	"subagent.started": true, "system.message": true, "system.notification": true, "tool_search.activated": true,
	"tool.execution_complete": true, "tool.execution_partial_result": true, "tool.execution_progress": true,
	"tool.execution_start": true, "tool.shell_output": true, "tool.user_requested": true, "ui.ephemeral_query": true,
	"user_input.completed": true, "user_input.requested": true, "user.message": true, "workflow.run_settled": true,
	"workflow.run_started": true, "workflow.run_updated": true,
}

// inspectCopilotEvents checks that events is JSON lines of events Copilot CLI
// copilotVersion records, starting with providerSessionID's session.start, and
// returns the Copilot CLI release that started the session. A later release's
// session is unsupported.
func inspectCopilotEvents(events []byte, providerSessionID string) (string, error) {
	if len(events) == 0 || events[len(events)-1] != '\n' {
		return "", errors.New("the Copilot CLI event log is empty or ends in a partial event")
	}
	version, unknown := "", ""
	first := true
	for line := range bytes.Lines(events) {
		var event struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if !bytes.HasPrefix(line, []byte("{")) || decodeOnly(json.NewDecoder(bytes.NewReader(line)), &event) != nil || event.Type == "" {
			return "", errors.New("the Copilot CLI event log holds an entry that is not one event")
		}
		if !copilotEventTypes[event.Type] && unknown == "" {
			unknown = event.Type
		}
		switch {
		case first:
			var start struct {
				SessionID      string `json:"sessionId"`
				CopilotVersion string `json:"copilotVersion"`
				Version        int    `json:"version"`
			}
			if event.Type != copilotSessionStart || json.Unmarshal(event.Data, &start) != nil {
				return "", errors.New("the Copilot CLI event log does not begin with session.start")
			}
			if start.SessionID != providerSessionID {
				return "", errors.New("the Copilot CLI event log starts another session")
			}
			if start.Version != copilotEventsSchema {
				return "", fmt.Errorf("the Copilot CLI event log uses schema %d, not %d", start.Version, copilotEventsSchema)
			}
			if _, ok := parseRelease(start.CopilotVersion); !ok {
				return "", errors.New("the Copilot CLI event log does not record a release")
			}
			version, first = start.CopilotVersion, false
		case event.Type == copilotSessionStart:
			return "", errors.New("the Copilot CLI event log starts more than one session")
		}
	}
	if err := requireSupportedCopilotVersion(version); err != nil {
		return "", err
	}
	if unknown != "" {
		return "", fmt.Errorf("the Copilot CLI event log holds a %q event, which Copilot CLI %s does not record", unknown, copilotVersion)
	}
	return version, nil
}

// requireSupportedCopilotVersion refuses a session a later Copilot CLI
// release started than Orka runs, which may hold events that release cannot
// load.
func requireSupportedCopilotVersion(version string) error {
	if order, ok := compareReleases(version, copilotVersion); !ok || order > 0 {
		return fmt.Errorf("%w: Copilot CLI %s started the conversation; Orka runs %s", ErrUnsupported, version, copilotVersion)
	}
	return nil
}

// decodeCopilotBundle reads and verifies a bundle holding a manifest and the
// event log.
func decodeCopilotBundle(data []byte, maxBytes int) (copilotManifest, []byte, error) {
	encoded, files, err := readBundle(data, maxBytes, func(name string) bool { return name == copilotEventsEntry })
	if err != nil {
		return copilotManifest{}, nil, err
	}
	var manifest copilotManifest
	if err := decodeManifest(encoded, &manifest); err != nil {
		return copilotManifest{}, nil, err
	}
	if manifest.Format != copilotBundleFormat || !uuidSessionIDPattern.MatchString(manifest.SessionID) {
		return copilotManifest{}, nil, errors.New("the Copilot CLI bundle manifest is invalid")
	}
	events := files[copilotEventsEntry]
	version, err := inspectCopilotEvents(events, manifest.SessionID)
	if err != nil {
		return copilotManifest{}, nil, err
	}
	if version != manifest.Version {
		return copilotManifest{}, nil, errors.New("the Copilot CLI bundle manifest names another release than its event log")
	}
	return manifest, events, nil
}
