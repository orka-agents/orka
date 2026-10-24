/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
)

// The ACP session backend serves Orka's built-in agent runtimes over the Agent
// Host Protocol. An AHP session is an Orka Session in the user's namespace;
// each turn is an agent Task in that Session, run and governed like any other.
const (
	ahpBackendName = "orka"

	ahpRecordProject = "project"
	ahpRecordSession = "session"
	ahpRecordChat    = "chat"
	ahpRecordContent = "content"
	ahpRecordFile    = "file"

	ahpApprovalManual   = "manual"
	ahpApprovalAllowAll = "allow-all"

	ahpConfigApprovalMode     = "approvalMode"
	ahpConfigAvailableModes   = "availableApprovalModes"
	ahpConfigEffectiveMode    = "effectiveApprovalMode"
	ahpConfigBaseBranch       = "baseBranch"
	ahpConfigBranch           = "branch"
	ahpSchemaString           = "string"
	ahpChangesetKindSession   = "session"
	ahpGitCredentialSecretKey = defaultWorkspaceCredentialKey
	ahpToolFallbackName       = "tool"

	// ahpProjectRoot is where projects appear on this host: checkouts in the
	// user's workspace when workspaces are enabled. Each turn's Task still
	// clones the repository itself.
	ahpProjectRoot = "/workspace/src"

	ahpDefaultTurnTimeout = 2 * time.Hour
	ahpMaxPromptBytes     = 256 << 10
	ahpMaxProjects        = 200
	// ahpPendingMax bounds the messages waiting on a session.
	ahpPendingMax   = 10
	ahpPendingIDMax = 128

	// ahpAgentProvider is the one agent this backend offers. It offers every
	// model the user may run and runs each on the runtime that serves it. Its
	// id is copilot because some clients send only to an agent with that id.
	ahpAgentProvider = corev1alpha1.AgentRuntimeCopilot
)

// The Copilot dialect: the extension keys and commands the GitHub Copilot app
// and copilotd use on the wire for projects, plans and approvals.
const (
	ahpCopilotProjectManagementMeta  = "copilot.projectManagement"
	ahpCopilotConfigKey              = "copilot"
	ahpExtensionListProjects         = "extensions/listProjects"
	ahpExtensionCloneProject         = "extensions/cloneProject"
	ahpExtensionRemoveProject        = "extensions/removeProject"
	ahpExtensionGetPlan              = "extensions/getPlan"
	ahpExtensionSetSessionApproveAll = "extensions/setSessionApproveAll"
)

// ahpBuiltInRuntimes are the runtimes AHP sessions run on, in the order a
// model several of them serve is offered and run.
var ahpBuiltInRuntimes = []struct {
	runtime corev1alpha1.AgentRuntimeType
	name    string
}{
	{corev1alpha1.AgentRuntimeCopilot, "Copilot CLI"},
	{corev1alpha1.AgentRuntimeClaude, "Claude Code"},
	{corev1alpha1.AgentRuntimeCodex, "Codex"},
	{corev1alpha1.AgentRuntimeOpencode, "OpenCode"},
}

// ahpRuntimeName is a built-in runtime's short display name.
func ahpRuntimeName(runtime corev1alpha1.AgentRuntimeType) string {
	for _, r := range ahpBuiltInRuntimes {
		if r.runtime == runtime {
			return r.name
		}
	}
	return string(runtime)
}

// AHPBackendConfig configures the ACP session backend.
type AHPBackendConfig struct {
	// GitCredentialSecret names a Secret in each user's namespace whose "token"
	// key clones and publishes their repositories. When the Secret is absent,
	// sessions clone public repositories read-only.
	GitCredentialSecret string
	// WorkspaceClass selects an ExecutionWorkspaceClass for session-scoped
	// workspaces, so a session's files stay warm between turns. Empty uses
	// the default path: every write turn publishes, and the next starts from it.
	WorkspaceClass string
	// TurnTimeout bounds each turn's Task.
	TurnTimeout time.Duration
	// WorkspaceNamespace and WorkspaceImage enable per-user workspaces:
	// git checkouts of the user's projects and sessions, and terminals. The
	// image must be digest-pinned and provide sh and git.
	WorkspaceNamespace string
	WorkspaceImage     string
	// WorkspaceIdleTimeout is how long a workspace with no shells, no running
	// turn and nothing unpushed stays after its last use. Zero keeps
	// workspaces until their user's last session ends.
	WorkspaceIdleTimeout time.Duration
	// RepositoryCredential returns a person's own github.com token, valid for
	// at least AHPWorkspaceTokenHorizon, from their linked account, for their
	// workspace's git reads. Nil keeps workspaces to public repositories.
	RepositoryCredential func(ctx context.Context, namespace, issuer, subject string) (string, error)
}

// ahpApprovalDecider answers a held permission request as its requester.
type ahpApprovalDecider func(ctx context.Context, ui *UserInfo, namespace, task, approvalID string, approve bool) error

type ahpBackend struct {
	h       *Handlers
	store   store.AHPStore
	live    *liveevents.Broker
	cfg     AHPBackendConfig
	now     func() time.Time
	gitHTTP *http.Client

	// applyApprovalMode asks a Task to hold policy-allowed permission
	// requests for its requester; decideApproval answers one.
	applyApprovalMode func(task *corev1alpha1.Task, manual bool)
	decideApproval    ahpApprovalDecider
	// recheckToken reports whether a principal's connection token is still
	// valid, before a message it sent earlier runs.
	recheckToken func(context.Context, ahphost.Principal) error
	// pendingAuth is who sent each waiting message, by user, session and
	// message id. A message runs with its sender's authority only.
	pendingAuth map[string]ahpPendingSender
	// emitterFor delivers actions for a user with no connection, to settle
	// turns a previous leader left running.
	emitterFor func(ahphost.Principal) ahphost.Emitter
	// updateTerminals republishes the user's terminal list after a shell exits.
	updateTerminals func(context.Context, ahphost.Principal) error
	workspaces      *ahpWorkspaces

	mu     sync.Mutex
	turns  map[string]*ahpTurn
	shells map[string]*ahpTerminal
	// retired holds terminal URIs already used, so input queued for an ended
	// terminal can never reach a new one on the same URI.
	retired map[string]time.Time
	// reserved holds a session's turn slot from the moment a message is
	// admitted until its turn starts, so a second message cannot slip in.
	reserved  map[string]time.Time
	recovered map[string]bool
	// lifecycle serializes, per user, what decides who owns workspace state:
	// session branch claims, session disposal and terminal creation.
	lifecycle map[string]*sync.Mutex
}

// lockUser takes the user's lifecycle lock. It is never taken while mu is held.
func (b *ahpBackend) lockUser(userKey string) func() {
	b.mu.Lock()
	if b.lifecycle == nil {
		b.lifecycle = map[string]*sync.Mutex{}
	}
	lock := b.lifecycle[userKey]
	if lock == nil {
		lock = &sync.Mutex{}
		b.lifecycle[userKey] = lock
	}
	b.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// ahpReservationTTL frees a slot whose message the host never applied.
const ahpReservationTTL = 30 * time.Second

// reserveTurn claims a session's turn slot; false means a turn is running or starting.
func (b *ahpBackend) reserveTurn(userKey, session string) bool {
	key := ahpTurnKey(userKey, session)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turns[key] != nil {
		return false
	}
	if at, ok := b.reserved[key]; ok && b.now().Sub(at) < ahpReservationTTL {
		return false
	}
	b.reserved[key] = b.now()
	return true
}

func (b *ahpBackend) releaseTurn(userKey, session string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.reserved, ahpTurnKey(userKey, session))
}

func newAHPBackend(h *Handlers, s store.AHPStore, live *liveevents.Broker, cfg AHPBackendConfig) *ahpBackend {
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = ahpDefaultTurnTimeout
	}
	return &ahpBackend{
		h: h, store: s, live: live, cfg: cfg, now: time.Now, gitHTTP: ahpGitHTTPClient,
		applyApprovalMode: ahpApplyApprovalMode, decideApproval: h.DecideTaskApprovalAs,
		turns: map[string]*ahpTurn{}, shells: map[string]*ahpTerminal{}, reserved: map[string]time.Time{}, recovered: map[string]bool{},
	}
}

// ahpApplyApprovalMode makes a manual-approval turn hold every permission
// request Orka's policy would allow for the person to decide. Allow all
// leaves the decision to policy.
func ahpApplyApprovalMode(task *corev1alpha1.Task, manual bool) {
	if !manual {
		return
	}
	if task.Spec.AgentRuntime == nil {
		task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
	}
	task.Spec.AgentRuntime.PermissionDecisions = corev1alpha1.PermissionDecisionsRequester
}

func (b *ahpBackend) Name() string { return ahpBackendName }

// ahpProject is a repository a user added to the host.
type ahpProject struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Owner         string    `json:"owner"`
	URL           string    `json:"url"`
	Path          string    `json:"path"`
	DefaultBranch string    `json:"defaultBranch,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// wire is a project as the Copilot dialect describes it.
func (p ahpProject) wire() map[string]any {
	project := map[string]any{
		"id": p.ID, apiFieldName: p.Name, "path": p.Path, "git": true, "origin": "cloned",
		"remoteUrl": p.URL, "status": "ready",
	}
	if p.DefaultBranch != "" {
		project["defaultBranch"] = p.DefaultBranch
	}
	return project
}

// ahpSession is what the backend keeps about one AHP session.
type ahpSession struct {
	Session     string `json:"session"`
	Chat        string `json:"chat"`
	Changeset   string `json:"changeset"`
	Namespace   string `json:"namespace"`
	OrkaSession string `json:"orkaSession"`
	ProjectID   string `json:"projectId"`
	RepoURL     string `json:"repoUrl"`
	BaseBranch  string `json:"baseBranch,omitempty"`
	Runtime     string `json:"runtime"`
	// Branch is where the session's turns publish; WorkDir is the session's
	// working directory on the host.
	Branch       string `json:"branch,omitempty"`
	WorkDir      string `json:"workDir,omitempty"`
	Agent        string `json:"agent,omitempty"`
	Model        string `json:"model,omitempty"`
	ApprovalMode string `json:"approvalMode"`
	Turns        int    `json:"turns"`
	// The running turn, if any. It survives a controller restart, so the
	// turn can still be settled from its Task.
	ActiveTurn      string `json:"activeTurn,omitempty"`
	ActiveTask      string `json:"activeTask,omitempty"`
	ActiveStartedAt string `json:"activeStartedAt,omitempty"`
	// Plan is the agent's latest plan, as a markdown checklist.
	Plan string `json:"plan,omitempty"`
	// Generation counts the Orka Sessions the session has used; a new one
	// starts when the person switches models. SessionTurns counts the
	// current one's turns, and SourceBranch is the branch its turns start
	// from when it is not the base branch.
	Generation   int    `json:"generation,omitempty"`
	SessionTurns int    `json:"sessionTurns,omitempty"`
	SourceBranch string `json:"sourceBranch,omitempty"`
	// Published reports that a turn pushed to Branch, which then belongs to
	// the current Orka Session.
	Published bool `json:"published,omitempty"`
	// History keeps the conversation's recent exchanges, bounded, so a new
	// Orka Session can start with them as context.
	History []ahpExchange `json:"history,omitempty"`
	// Pending are the messages waiting for the running turn to end, steering
	// messages first.
	Pending []ahpPending `json:"pending,omitempty"`
}

// ahpPending is a message sent while a turn runs. Orka's runtimes take no
// input mid-turn, so every waiting message is queued; a steering message is
// queued first.
type ahpPending struct {
	ID      string           `json:"id"`
	Kind    string           `json:"kind"`
	Message ahptypes.Message `json:"message"`
	// Version names this version of the message: a replaced message gets a
	// new one, so its sender can never be paired with another version.
	Version string `json:"version,omitempty"`
}

// ahpPendingSender is who sent one version of a waiting message.
type ahpPendingSender struct {
	principal ahphost.Principal
	version   string
}

// ahpExchange is one message and the reply to it.
type ahpExchange struct {
	User  string `json:"user"`
	Reply string `json:"reply,omitempty"`
}

// ahpPrincipalUserInfo is the person behind a connection: AHP tokens are
// minted only for a person's own OIDC sign-in.
func ahpPrincipalUserInfo(p ahphost.Principal) *UserInfo {
	return &UserInfo{AuthType: AuthTypeOIDC, Issuer: p.Issuer, Subject: p.Subject, Username: p.Username, Namespace: p.Namespace}
}

func ahpHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func ahpRandomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func (b *ahpBackend) getRecord(ctx context.Context, userKey, kind, key string, out any) error {
	record, err := b.store.GetAHPRecord(ctx, userKey, kind, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(record.Value, out)
}

func (b *ahpBackend) putRecord(ctx context.Context, userKey, kind, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return b.store.PutAHPRecord(ctx, store.AHPRecord{UserKey: userKey, Kind: kind, Key: key, Value: raw, UpdatedAt: b.now().UTC()})
}

// ahpAgentChoice is an Orka Agent a user may run as one AHP model.
type ahpAgentChoice struct {
	Agent   string
	Runtime corev1alpha1.AgentRuntimeType
	Model   string
}

// agentChoices lists the built-in-runtime Agents in the user's namespace, one
// per runtime and model. Each turn's Task is authorized when it is created.
func (b *ahpBackend) agentChoices(ctx context.Context, principal ahphost.Principal) ([]ahpAgentChoice, error) {
	var agents corev1alpha1.AgentList
	if err := b.h.client.List(ctx, &agents, client.InNamespace(principal.Namespace)); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	sort.Slice(agents.Items, func(i, j int) bool { return agents.Items[i].Name < agents.Items[j].Name })
	seen := map[string]bool{}
	var choices []ahpAgentChoice
	for i := range agents.Items {
		agent := &agents.Items[i]
		if !agent.DeletionTimestamp.IsZero() || agent.Spec.Runtime == nil || agent.Spec.Runtime.RuntimeRef != nil ||
			agent.Spec.Model == nil || strings.TrimSpace(agent.Spec.Model.Name) == "" {
			continue
		}
		runtime := agent.Spec.Runtime.Type
		if ahpRuntimeName(runtime) == string(runtime) {
			continue
		}
		key := string(runtime) + "\x00" + agent.Spec.Model.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		choices = append(choices, ahpAgentChoice{Agent: agent.Name, Runtime: runtime, Model: agent.Spec.Model.Name})
	}
	return choices, nil
}

// Agents lists one AHP agent offering every model the user has an Agent for,
// each labelled with the runtime that runs it.
func (b *ahpBackend) Agents(ctx context.Context, principal ahphost.Principal) ([]ahptypes.AgentInfo, error) {
	b.recoverTurns(ctx, principal)
	b.workspaces.warm(principal)
	choices, err := b.agentChoices(ctx, principal)
	if err != nil {
		return nil, err
	}
	// Every listed model passed the user's Agent authorization; clients may
	// refuse to send to a model the host does not mark enabled.
	enabled := ahptypes.PolicyStateEnabled
	var models []ahptypes.SessionModelInfo
	listed := map[string]bool{}
	for _, runtime := range ahpBuiltInRuntimes {
		for _, choice := range choices {
			// A model several runtimes serve is offered once, on the first
			// of them, which is where ahpChooseModel runs it.
			if choice.Runtime != runtime.runtime || listed[choice.Model] {
				continue
			}
			listed[choice.Model] = true
			models = append(models, ahptypes.SessionModelInfo{
				Id: choice.Model, Provider: string(ahpAgentProvider), Name: choice.Model + " (" + runtime.name + ")",
				PolicyState: &enabled,
			})
		}
	}
	if len(models) == 0 {
		return nil, nil
	}
	return []ahptypes.AgentInfo{{
		Provider: string(ahpAgentProvider), DisplayName: "Orka", Models: models,
		Description: "Runs each message as an Orka Task in your namespace, with Orka's policy and credentials, on the runtime that serves the chosen model.",
	}}, nil
}

func (b *ahpBackend) projects(ctx context.Context, userKey string) ([]ahpProject, error) {
	records, err := b.store.ListAHPRecords(ctx, userKey, ahpRecordProject)
	if err != nil {
		return nil, err
	}
	projects := make([]ahpProject, 0, len(records))
	for _, record := range records {
		var project ahpProject
		if err := json.Unmarshal(record.Value, &project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].CreatedAt.Before(projects[j].CreatedAt) })
	return projects, nil
}

func ahpProjectsWire(projects []ahpProject) []map[string]any {
	wire := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		wire = append(wire, project.wire())
	}
	return wire
}

// RootExtensions advertises project management and lists the user's projects.
func (b *ahpBackend) RootExtensions(ctx context.Context, principal ahphost.Principal) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	projects, err := b.projects(ctx, principal.UserKey)
	if err != nil {
		return nil, nil, err
	}
	config, err := ahpRootConfig(projects)
	if err != nil {
		return nil, nil, err
	}
	return map[string]json.RawMessage{ahpCopilotProjectManagementMeta: json.RawMessage(`{"available":true}`)}, config, nil
}

func ahpRootConfig(projects []ahpProject) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(map[string]any{"hostName": "orka", "projects": ahpProjectsWire(projects)})
	if err != nil {
		return nil, err
	}
	return map[string]json.RawMessage{ahpCopilotConfigKey: raw}, nil
}

// HandleExtension serves the project commands of the Copilot dialect.
func (b *ahpBackend) HandleExtension(ctx context.Context, principal ahphost.Principal, method string, params json.RawMessage, emit ahphost.Emitter) (any, error) {
	switch method {
	case ahpExtensionListProjects:
		projects, err := b.projects(ctx, principal.UserKey)
		if err != nil {
			return nil, err
		}
		for i := range projects {
			// Fill in a default branch an earlier lookup could not find.
			if projects[i].DefaultBranch == "" {
				if branch := b.defaultBranch(ctx, principal.Namespace, projects[i].URL); branch != "" {
					projects[i].DefaultBranch = branch
					_ = b.putRecord(ctx, principal.UserKey, ahpRecordProject, projects[i].ID, projects[i])
				}
			}
		}
		return map[string]any{"projects": ahpProjectsWire(projects)}, nil
	case ahpExtensionCloneProject:
		var req struct {
			URL string `json:"url"`
		}
		if len(params) == 0 || json.Unmarshal(params, &req) != nil {
			return nil, &ahphost.Rejection{Reason: "a repository url is required"}
		}
		project, err := b.addProject(ctx, principal, req.URL)
		if err != nil {
			return nil, err
		}
		if b.workspaces.enabled() {
			go func() {
				bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				if pod, err := b.workspaces.ensure(bg, principal, 2*time.Minute); err == nil {
					_ = b.workspaces.ensureProject(bg, principal, pod, project)
				}
			}()
		}
		if err := b.publishProjects(ctx, principal, emit); err != nil {
			return nil, err
		}
		return map[string]any{"project": project.wire()}, nil
	case ahpExtensionRemoveProject:
		return b.removeProjectCommand(ctx, principal, params, emit)
	case ahpExtensionSetSessionApproveAll:
		return nil, b.setApproveAllCommand(ctx, principal, params, emit)
	case ahpExtensionGetPlan:
		var req struct {
			Channel string `json:"channel"`
		}
		var content *string
		var record ahpSession
		if len(params) > 0 && json.Unmarshal(params, &req) == nil && req.Channel != "" &&
			b.getRecord(ctx, principal.UserKey, ahpRecordSession, req.Channel, &record) == nil && record.Plan != "" {
			content = &record.Plan
		}
		plan := map[string]any{"exists": content != nil, ahpRecordContent: content}
		return map[string]any{"plan": plan, "todos": []any{}, "dependencies": []any{}}, nil
	default:
		return nil, ahphost.ErrUnknownExtension
	}
}

// removeProjectCommand serves extensions/removeProject. Removing a project
// takes it off the list; its checkout stays.
func (b *ahpBackend) removeProjectCommand(ctx context.Context, principal ahphost.Principal, params json.RawMessage, emit ahphost.Emitter) (any, error) {
	var req struct {
		ID              string `json:"id"`
		DeleteClone     bool   `json:"deleteClone"`
		DeleteWorktrees bool   `json:"deleteWorktrees"`
		Force           bool   `json:"force"`
	}
	if len(params) == 0 || json.Unmarshal(params, &req) != nil || req.ID == "" {
		return nil, &ahphost.Rejection{Reason: "a project id is required, and removal options must be booleans"}
	}
	if req.DeleteClone || req.DeleteWorktrees || req.Force {
		return nil, &ahphost.Rejection{Reason: "this host does not delete a project's files"}
	}
	removed, err := b.removeProject(ctx, principal, req.ID)
	if err != nil {
		return nil, err
	}
	if removed {
		if err := b.publishProjects(ctx, principal, emit); err != nil {
			return nil, err
		}
	}
	return map[string]any{"removed": removed}, nil
}

// setApproveAllCommand serves extensions/setSessionApproveAll, a client's
// approve-all toggle for one session.
func (b *ahpBackend) setApproveAllCommand(ctx context.Context, principal ahphost.Principal, params json.RawMessage, emit ahphost.Emitter) error {
	var req struct {
		Channel string `json:"channel"`
		Enabled *bool  `json:"enabled"`
	}
	if len(params) == 0 || json.Unmarshal(params, &req) != nil || req.Channel == "" || req.Enabled == nil {
		return &ahphost.Rejection{Reason: "a session channel and enabled are required"}
	}
	record, err := b.sessionForChannel(ctx, principal.UserKey, req.Channel)
	if errors.Is(err, store.ErrNotFound) {
		return &ahphost.Rejection{Reason: "unknown session"}
	} else if err != nil {
		return err
	}
	mode := ahpApprovalManual
	if *req.Enabled {
		mode = ahpApprovalAllowAll
	}
	return b.setApprovalMode(ctx, principal.UserKey, record.Session, mode, emit, true)
}

// publishProjects shows the user's clients their current project list.
func (b *ahpBackend) publishProjects(ctx context.Context, principal ahphost.Principal, emit ahphost.Emitter) error {
	projects, err := b.projects(ctx, principal.UserKey)
	if err != nil {
		return err
	}
	config, err := ahpRootConfig(projects)
	if err != nil {
		return err
	}
	return emit.Emit(ctx, ahptypes.RootResourceURI, ahptypes.StateAction{Value: &ahptypes.RootConfigChangedAction{
		Type: ahptypes.ActionTypeRootConfigChanged, Config: config,
	}})
}

// removeProject takes a project off the user's list and reports whether it
// was there. A project a session still uses stays: its turns and worktree
// need it. Sessions are created under the same lock.
func (b *ahpBackend) removeProject(ctx context.Context, principal ahphost.Principal, id string) (bool, error) {
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	var project ahpProject
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordProject, id, &project); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	sessions, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	if err != nil {
		return false, err
	}
	for _, raw := range sessions {
		var record ahpSession
		if json.Unmarshal(raw.Value, &record) == nil && record.ProjectID == id {
			return false, &ahphost.Rejection{Reason: "sessions still use this project; delete them first"}
		}
	}
	return true, b.store.DeleteAHPRecord(ctx, principal.UserKey, ahpRecordProject, id)
}

// addProject records a repository. Orka never clones it here: each turn's
// Task clones it in its own workspace, with Orka's credentials.
func (b *ahpBackend) addProject(ctx context.Context, principal ahphost.Principal, raw string) (ahpProject, error) {
	repo, owner, name, err := ahpCanonicalRepository(raw)
	if err != nil {
		return ahpProject{}, &ahphost.Rejection{Reason: err.Error()}
	}
	id := "project-" + ahpHash(repo)[:16]
	var existing ahpProject
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordProject, id, &existing); err == nil {
		if existing.DefaultBranch == "" {
			existing.DefaultBranch = b.defaultBranch(ctx, principal.Namespace, repo)
			if existing.DefaultBranch != "" {
				return existing, b.putRecord(ctx, principal.UserKey, ahpRecordProject, id, existing)
			}
		}
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return ahpProject{}, err
	}
	projects, err := b.projects(ctx, principal.UserKey)
	if err != nil {
		return ahpProject{}, err
	}
	if len(projects) >= ahpMaxProjects {
		return ahpProject{}, &ahphost.Rejection{Reason: fmt.Sprintf("at most %d projects are allowed", ahpMaxProjects)}
	}
	project := ahpProject{
		ID: id, Name: name, Owner: owner, URL: repo, Path: path.Join(ahpProjectRoot, owner, name), CreatedAt: b.now().UTC(),
		DefaultBranch: b.defaultBranch(ctx, principal.Namespace, repo),
	}
	return project, b.putRecord(ctx, principal.UserKey, ahpRecordProject, id, project)
}

// ahpCanonicalRepository accepts an https repository URL without credentials,
// query or fragment and returns it without a trailing .git.
func ahpCanonicalRepository(raw string) (repo, owner, name string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != connectorSchemeHTTPS || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", "", errors.New("the repository must be an https URL without credentials, query or fragment")
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return "", "", "", errors.New("the repository URL must name an owner and a repository")
	}
	owner, name = segments[0], strings.TrimSuffix(segments[1], ".git")
	for _, part := range []string{owner, name} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\ `) {
			return "", "", "", errors.New("the repository URL is not valid")
		}
	}
	return fmt.Sprintf("https://%s/%s/%s", strings.ToLower(parsed.Host), owner, name), owner, name, nil
}

// ahpDirectoryURI is the file URI of a host path, escaped so that names with
// #, ? or % survive a client's URI parser.
func ahpDirectoryURI(dir string) string { return (&url.URL{Scheme: "file", Path: dir}).String() }

// ahpURIPath is the host path a file URI names, unescaped. Anything that is
// not a plain file URI names no path.
func ahpURIPath(uri string) (string, bool) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" || strings.ContainsAny(parsed.Path, "\x00\n") {
		return "", false
	}
	return parsed.Path, true
}

// projectForDirectory finds the project a session's working directory names.
func (b *ahpBackend) projectForDirectory(ctx context.Context, userKey string, dirs []string) (ahpProject, error) {
	if len(dirs) == 0 {
		return ahpProject{}, &ahphost.Rejection{Reason: "pick a project: sessions on Orka run in a repository"}
	}
	projects, err := b.projects(ctx, userKey)
	if err != nil {
		return ahpProject{}, err
	}
	if dir, ok := ahpURIPath(dirs[0]); ok {
		for _, project := range projects {
			if dir == project.Path {
				return project, nil
			}
		}
	}
	return ahpProject{}, &ahphost.Rejection{Reason: "the working directory is not a project on this host; add the repository first"}
}

func ahpSessionConfig(mode string) *ahptypes.SessionConfigState {
	modes := []json.RawMessage{json.RawMessage(`"manual"`), json.RawMessage(`"allow-all"`)}
	description := "Manual asks you before a tool runs that Orka's policy allows. Allow all lets Orka's policy decide."
	readOnly := true
	mutable := true
	notMutable := false
	values, _ := json.Marshal(mode)
	available, _ := json.Marshal([]string{ahpApprovalManual, ahpApprovalAllowAll})
	return &ahptypes.SessionConfigState{
		Schema: ahptypes.SessionConfigSchema{Type: "object", Properties: map[string]ahptypes.SessionConfigPropertySchema{
			ahpConfigApprovalMode: {Type: ahpSchemaString, Title: "Approvals", Description: &description, Enum: modes,
				EnumLabels: []string{"Manual approvals", "Allow all"}, SessionMutable: &mutable},
			ahpConfigAvailableModes: {Type: "array", Title: "Available approvals", ReadOnly: &readOnly, SessionMutable: &notMutable,
				Items: &ahptypes.ConfigPropertySchema{Type: ahpSchemaString, Title: "Approval mode", Enum: modes}},
			ahpConfigEffectiveMode: {Type: ahpSchemaString, Title: "Approvals in force", ReadOnly: &readOnly, SessionMutable: &notMutable, Enum: modes},
		}},
		Values: map[string]json.RawMessage{ahpConfigApprovalMode: values, ahpConfigAvailableModes: available, ahpConfigEffectiveMode: values},
	}
}

func ahpApprovalModeFrom(config map[string]json.RawMessage) (string, error) {
	raw, ok := config[ahpConfigApprovalMode]
	if !ok {
		return ahpApprovalManual, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		return "", errors.New("approvalMode must be a string")
	}
	switch mode {
	case ahpApprovalManual, ahpApprovalAllowAll:
		return mode, nil
	case "assisted":
		// Orka has no model-reviewed approvals; ask the person instead.
		return ahpApprovalManual, nil
	default:
		return "", fmt.Errorf("approvalMode %q is not supported", mode)
	}
}

// CreateSession starts a session in one of the user's projects. Nothing runs
// until the first message: that turn's Task creates the Orka Session.
func (b *ahpBackend) CreateSession(ctx context.Context, principal ahphost.Principal, req ahphost.CreateSessionRequest, _ ahphost.Emitter) (*ahphost.CreatedSession, error) {
	// One branch of a project belongs to one session: its worktree and its
	// publications are that session's. The project lookup, the claim and the
	// record are made under the user's lifecycle lock, which project removal
	// also holds.
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	project, err := b.projectForDirectory(ctx, principal.UserKey, req.WorkingDirectories)
	if err != nil {
		return nil, err
	}
	mode, err := ahpApprovalModeFrom(req.Config)
	if err != nil {
		return nil, &ahphost.Rejection{Reason: err.Error()}
	}
	baseBranch := ""
	if raw, ok := req.Config[ahpConfigBaseBranch]; ok {
		_ = json.Unmarshal(raw, &baseBranch)
	}
	if baseBranch == "" || !ahpValidBranch(baseBranch) {
		baseBranch = project.DefaultBranch
	}
	// Turns publish to the branch the client named for the session, or to an
	// Orka branch when it named none.
	branch := ""
	if raw, ok := req.Config[ahpConfigBranch]; ok {
		_ = json.Unmarshal(raw, &branch)
	}
	if !ahpValidBranch(branch) || branch == baseBranch {
		branch = "orka/session-" + ahpRandomHex(8)
	}
	existing, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	if err != nil {
		return nil, err
	}
	for _, raw := range existing {
		var other ahpSession
		if json.Unmarshal(raw.Value, &other) == nil && other.ProjectID == project.ID && other.Branch == branch {
			return nil, &ahphost.Rejection{Reason: fmt.Sprintf("another session already uses the branch %s", branch)}
		}
	}
	workDir := project.Path
	if b.workspaces.enabled() {
		workDir = ahpWorktreePath(project, branch)
	}
	chatID := ahpRandomHex(16)
	chat := "ahp-chat:/" + chatID
	changeset := "ahp-changeset:/" + chatID + "/session"
	record := ahpSession{
		Session: req.Session, Chat: chat, Changeset: changeset, Namespace: principal.Namespace,
		OrkaSession: "ahp-" + ahpHash(principal.UserKey, req.Session)[:24], ProjectID: project.ID, RepoURL: project.URL,
		BaseBranch: baseBranch, Runtime: req.Provider, ApprovalMode: mode, Branch: branch, WorkDir: workDir,
	}
	if err := b.putRecord(ctx, principal.UserKey, ahpRecordSession, req.Session, record); err != nil {
		return nil, err
	}
	if b.workspaces.enabled() {
		// Clients run git in the session's working directory right away. A
		// worktree that takes longer is finished before a terminal opens in it.
		if pod, err := b.workspaces.ensure(ctx, principal, ahpWorkspaceStartWait); err == nil {
			setupCtx, cancel := context.WithTimeout(ctx, ahpWorkspaceSetupWait)
			_ = b.workspaces.ensureWorktree(setupCtx, principal, pod, project, record)
			cancel()
		}
	}
	if err := b.putRecord(ctx, principal.UserKey, ahpRecordChat, chat, req.Session); err != nil {
		return nil, err
	}
	now := b.now().UTC().Format(time.RFC3339Nano)
	title := project.Owner + "/" + project.Name
	description := "Cumulative file edits the agent has made in this session."
	user := ahptypes.ChatOrigin{Value: &ahptypes.ChatUserOrigin{Kind: ahptypes.ChatOriginKindUser}}
	return &ahphost.CreatedSession{
		State: ahptypes.SessionState{
			Title: title, Status: ahptypes.SessionStatusIdle,
			Project:            &ahptypes.ProjectInfo{Uri: project.URL, DisplayName: project.Owner + "/" + project.Name},
			WorkingDirectories: []string{ahpDirectoryURI(workDir)},
			ActiveClients:      []ahptypes.SessionActiveClient{},
			Chats:              []ahptypes.ChatSummary{{Resource: chat, Title: title, Status: ahptypes.SessionStatusIdle, ModifiedAt: now, Origin: &user}},
			DefaultChat:        &chat,
			Config:             ahpSessionConfig(mode),
			Changesets: []ahptypes.Changeset{{
				Label: "Session Changes", ChangeKind: ahpChangesetKindSession, UriTemplate: changeset, Description: &description,
			}},
		},
		Chats: map[string]ahptypes.ChatState{chat: {
			Resource: chat, Title: title, Status: ahptypes.SessionStatusIdle, ModifiedAt: now, Origin: &user, Turns: []ahptypes.Turn{},
		}},
		Changesets: map[string]ahptypes.ChangesetState{changeset: {Status: ahptypes.ChangesetStatusReady, Files: []ahptypes.ChangesetFile{}}},
	}, nil
}

// DisposeSession cancels a running turn and forgets the session. The Orka
// Session and its Tasks keep their history.
func (b *ahpBackend) DisposeSession(ctx context.Context, principal ahphost.Principal, session string) error {
	// Nothing can claim the branch or start using the workspace until the
	// worktree, the workspace decision and the records are settled; a session
	// already disposed by another request is left alone.
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	var record ahpSession
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordSession, session, &record); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if turn := b.activeTurn(principal.UserKey, session); turn != nil {
		turn.stop()
	}
	if record.ActiveTask != "" {
		b.cancelTask(ctx, principal, record.Namespace, record.ActiveTask)
	}
	if b.workspaces.enabled() {
		// Remove the worktree while the session still holds its branch, so a
		// new session cannot claim the branch and lose its files to this.
		// Terminals are the user's own and outlive sessions: the workspace
		// itself goes only with the last session and no terminal running.
		sessions, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
		if err == nil && len(sessions) <= 1 && !b.hasTerminals(principal.UserKey) {
			b.workspaces.remove(ctx, principal)
		} else {
			var project ahpProject
			if b.getRecord(ctx, principal.UserKey, ahpRecordProject, record.ProjectID, &project) == nil {
				dropCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				b.workspaces.dropWorktree(dropCtx, principal, project, record.Branch)
				cancel()
			}
		}
	}
	hash := ahpHash(principal.UserKey, session)
	for _, kind := range []string{ahpRecordContent, ahpRecordFile} {
		if err := b.store.DeleteAHPRecords(ctx, principal.UserKey, kind, hash+"/"); err != nil {
			return err
		}
	}
	if err := b.store.DeleteAHPRecord(ctx, principal.UserKey, ahpRecordChat, record.Chat); err != nil {
		return err
	}
	return b.store.DeleteAHPRecord(ctx, principal.UserKey, ahpRecordSession, session)
}

func (b *ahpBackend) cancelTask(ctx context.Context, principal ahphost.Principal, namespace, name string) {
	task := &corev1alpha1.Task{}
	if err := b.h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, task); err != nil {
		return
	}
	_, _ = b.h.requestTaskCancellation(ctx, ahpPrincipalUserInfo(principal), task)
}

func (b *ahpBackend) sessionForChannel(ctx context.Context, userKey, channel string) (ahpSession, error) {
	session := channel
	if strings.HasPrefix(channel, "ahp-chat:") {
		if err := b.getRecord(ctx, userKey, ahpRecordChat, channel, &session); err != nil {
			return ahpSession{}, err
		}
	}
	var record ahpSession
	err := b.getRecord(ctx, userKey, ahpRecordSession, session, &record)
	return record, err
}

// resolveModel maps a turn's model to the Agent that runs it. An Orka Session
// is bound to one runtime profile, so a session keeps its Agent until the
// person picks another model; switching reports true and the turn moves the
// session to a new Orka Session.
func (b *ahpBackend) resolveModel(ctx context.Context, principal ahphost.Principal, record ahpSession, model *ahptypes.ModelSelection) (ahpAgentChoice, bool, error) {
	choices, err := b.agentChoices(ctx, principal)
	if err != nil {
		return ahpAgentChoice{}, false, err
	}
	wanted := record.Model
	if model != nil && model.Id != "" {
		wanted = model.Id
	}
	if record.Agent != "" && wanted == record.Model {
		for _, choice := range choices {
			if choice.Agent == record.Agent {
				return choice, false, nil
			}
		}
		return ahpAgentChoice{}, false, &ahphost.Rejection{Reason: "this session's agent is no longer available to you"}
	}
	choice, ok := ahpChooseModel(choices, record.Runtime, wanted)
	if !ok {
		return ahpAgentChoice{}, false, &ahphost.Rejection{Reason: "the selected model is not available to you"}
	}
	return choice, record.Agent != "", nil
}

// ahpChooseModel picks the Agent for a model on a session's provider. The
// offered agent runs a model on the first built-in runtime that serves it, as
// Agents lists it; a session on another provider stays on that runtime.
func ahpChooseModel(choices []ahpAgentChoice, provider, model string) (ahpAgentChoice, bool) {
	for _, runtime := range ahpBuiltInRuntimes {
		if provider != string(ahpAgentProvider) && string(runtime.runtime) != provider {
			continue
		}
		for _, choice := range choices {
			if choice.Runtime == runtime.runtime && (model == "" || choice.Model == model) {
				return choice, true
			}
		}
	}
	return ahpAgentChoice{}, false
}

// AuthorizeClientAction admits the client actions this backend acts on and
// the harmless view actions; anything else is rejected.
func (b *ahpBackend) AuthorizeClientAction(ctx context.Context, principal ahphost.Principal, channel string, action ahptypes.StateAction) error {
	if strings.HasPrefix(channel, ahpTerminalScheme) {
		return b.authorizeTerminalAction(principal, channel, action)
	}
	record, err := b.sessionForChannel(ctx, principal.UserKey, channel)
	if err != nil {
		return &ahphost.Rejection{Reason: "unknown session"}
	}
	switch a := action.Value.(type) {
	case *ahptypes.ChatTurnStartedAction, *ahptypes.ChatPendingMessageSetAction,
		*ahptypes.ChatPendingMessageRemovedAction, *ahptypes.ChatQueuedMessagesReorderedAction:
		if channel != record.Chat {
			return &ahphost.Rejection{Reason: "messages go to the session's chat"}
		}
		return b.authorizeMessage(ctx, principal, record, action)
	case *ahptypes.ChatTurnCancelledAction:
		if record.ActiveTurn == "" || a.TurnId != record.ActiveTurn {
			return &ahphost.Rejection{Reason: "the turn is not running"}
		}
		return nil
	case *ahptypes.ChatToolCallConfirmedAction:
		turn := b.activeTurn(principal.UserKey, record.Session)
		if turn == nil || a.TurnId != turn.id || !turn.awaitingConfirmation(a.ToolCallId) {
			return &ahphost.Rejection{Reason: "the tool call is not waiting for a decision"}
		}
		if b.decideApproval == nil {
			return &ahphost.Rejection{Reason: "approvals are not available on this host"}
		}
		return nil
	case *ahptypes.SessionConfigChangedAction:
		for key := range a.Config {
			switch key {
			case ahpConfigApprovalMode:
				if _, err := ahpApprovalModeFrom(a.Config); err != nil {
					return &ahphost.Rejection{Reason: err.Error()}
				}
			case ahpConfigAvailableModes, ahpConfigEffectiveMode:
				return &ahphost.Rejection{Reason: key + " is read-only"}
			}
		}
		return nil
	case *ahptypes.SessionTitleChangedAction, *ahptypes.SessionActiveClientSetAction, *ahptypes.SessionActiveClientRemovedAction,
		*ahptypes.SessionIsReadChangedAction, *ahptypes.SessionIsArchivedChangedAction,
		*ahptypes.ChatDraftChangedAction, *ahptypes.ChatIsReadChangedAction, *ahptypes.ChatIsArchivedChangedAction:
		// View state only. Client-provided tools in an active client are never offered to agents.
		return nil
	default:
		return &ahphost.Rejection{Reason: "not supported on this host"}
	}
}

// authorizeMessage admits a message for the session's chat: one that starts a
// turn now, or one that waits for the running turn to end.
func (b *ahpBackend) authorizeMessage(ctx context.Context, principal ahphost.Principal, record ahpSession, action ahptypes.StateAction) error {
	switch a := action.Value.(type) {
	case *ahptypes.ChatTurnStartedAction:
		if strings.TrimSpace(a.Message.Text) == "" || len(a.Message.Text) > ahpMaxPromptBytes || a.TurnId == "" {
			return &ahphost.Rejection{Reason: "a message needs text and a turn id"}
		}
		if _, _, err := b.resolveModel(ctx, principal, record, a.Message.Model); err != nil {
			return err
		}
		if record.ActiveTurn != "" || !b.reserveTurn(principal.UserKey, record.Session) {
			return &ahphost.Rejection{Reason: "a turn is already running"}
		}
		return nil
	case *ahptypes.ChatPendingMessageSetAction:
		if a.Id == "" || len(a.Id) > ahpPendingIDMax || strings.TrimSpace(a.Message.Text) == "" || len(a.Message.Text) > ahpMaxPromptBytes {
			return &ahphost.Rejection{Reason: "a message needs text and an id"}
		}
		if a.Kind != ahptypes.PendingMessageKindSteering && a.Kind != ahptypes.PendingMessageKindQueued {
			return &ahphost.Rejection{Reason: "unknown pending message kind"}
		}
		replacing := slices.ContainsFunc(record.Pending, func(p ahpPending) bool { return p.ID == a.Id })
		if !replacing && len(record.Pending) >= ahpPendingMax {
			return &ahphost.Rejection{Reason: "too many messages are waiting"}
		}
		if _, _, err := b.resolveModel(ctx, principal, record, a.Message.Model); err != nil {
			return err
		}
		return nil
	default:
		return nil
	}
}

// HandleClientAction runs what an admitted client action asks for.
func (b *ahpBackend) HandleClientAction(ctx context.Context, principal ahphost.Principal, channel string, action ahptypes.StateAction, emit ahphost.Emitter) {
	if strings.HasPrefix(channel, ahpTerminalScheme) {
		b.handleTerminalAction(principal, channel, action)
		return
	}
	record, err := b.sessionForChannel(ctx, principal.UserKey, channel)
	if err != nil {
		return
	}
	switch a := action.Value.(type) {
	case *ahptypes.ChatTurnStartedAction:
		b.startTurn(ctx, principal, record, a, emit)
	case *ahptypes.ChatTurnCancelledAction:
		if turn := b.activeTurn(principal.UserKey, record.Session); turn != nil {
			turn.markCancelledByClient()
		}
		if record.ActiveTask != "" {
			b.cancelTask(ctx, principal, record.Namespace, record.ActiveTask)
		}
	case *ahptypes.ChatToolCallConfirmedAction:
		if turn := b.activeTurn(principal.UserKey, record.Session); turn != nil {
			turn.confirm(ctx, principal, a.ToolCallId, a.Approved)
		}
	case *ahptypes.ChatPendingMessageSetAction:
		version := ahpRandomHex(16)
		b.mu.Lock()
		if b.pendingAuth == nil {
			b.pendingAuth = map[string]ahpPendingSender{}
		}
		b.pendingAuth[ahpPendingKey(principal.UserKey, record.Session, a.Id)] = ahpPendingSender{principal: principal, version: version}
		b.mu.Unlock()
		// Orka's runtimes take no input mid-turn, so a steering message is
		// queued first instead, and shown so: it then appears once, when its
		// turn starts, not also as input to the running turn.
		steering := a.Kind == ahptypes.PendingMessageKindSteering
		var order []string
		if err := b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
			s.Pending = slices.DeleteFunc(s.Pending, func(p ahpPending) bool { return p.ID == a.Id })
			entry := ahpPending{ID: a.Id, Kind: string(ahptypes.PendingMessageKindQueued), Message: a.Message, Version: version}
			if steering {
				s.Pending = slices.Insert(s.Pending, 0, entry)
			} else {
				s.Pending = append(s.Pending, entry)
			}
			order = order[:0]
			for _, p := range s.Pending {
				order = append(order, p.ID)
			}
		}); err != nil {
			// The message was not queued: take it out of the client's queue.
			logSessionUpdate(err, record.Session, "queue a message")
			b.mu.Lock()
			delete(b.pendingAuth, ahpPendingKey(principal.UserKey, record.Session, a.Id))
			b.mu.Unlock()
			_ = emit.Emit(ctx, record.Chat, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageRemovedAction{
				Type: ahptypes.ActionTypeChatPendingMessageRemoved, Kind: a.Kind, Id: a.Id,
			}})
			return
		}
		if steering {
			actions := []ahptypes.StateAction{
				{Value: &ahptypes.ChatPendingMessageRemovedAction{Type: ahptypes.ActionTypeChatPendingMessageRemoved, Kind: ahptypes.PendingMessageKindSteering, Id: a.Id}},
				{Value: &ahptypes.ChatPendingMessageSetAction{Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: a.Id, Message: a.Message}},
			}
			if len(order) > 1 {
				actions = append(actions, ahptypes.StateAction{Value: &ahptypes.ChatQueuedMessagesReorderedAction{
					Type: ahptypes.ActionTypeChatQueuedMessagesReordered, Order: order,
				}})
			}
			_ = emit.Emit(ctx, record.Chat, actions...)
		}
		b.runPending(ctx, principal.UserKey, record.Session, emit)
	case *ahptypes.ChatPendingMessageRemovedAction:
		logSessionUpdate(b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
			s.Pending = slices.DeleteFunc(s.Pending, func(p ahpPending) bool { return p.ID == a.Id })
		}), record.Session, "remove a queued message")
		b.mu.Lock()
		delete(b.pendingAuth, ahpPendingKey(principal.UserKey, record.Session, a.Id))
		b.mu.Unlock()
	case *ahptypes.ChatQueuedMessagesReorderedAction:
		logSessionUpdate(b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
			s.Pending = ahpReorderPending(s.Pending, a.Order)
		}), record.Session, "reorder queued messages")
	case *ahptypes.SessionConfigChangedAction:
		if _, ok := a.Config[ahpConfigApprovalMode]; !ok {
			return
		}
		mode, err := ahpApprovalModeFrom(a.Config)
		if err != nil {
			return
		}
		_ = b.setApprovalMode(ctx, principal.UserKey, record.Session, mode, emit, false)
	}
}

// setApprovalMode records a session's approval mode, which its next turn
// uses, and shows it to the session's clients. A client that changed the
// mode itself has already shown the selection.
func (b *ahpBackend) setApprovalMode(ctx context.Context, userKey, session, mode string, emit ahphost.Emitter, showSelection bool) error {
	if err := b.updateSession(ctx, userKey, session, func(s *ahpSession) { s.ApprovalMode = mode }); err != nil {
		logSessionUpdate(err, session, "set the approval mode")
		return err
	}
	value, _ := json.Marshal(mode)
	config := map[string]json.RawMessage{ahpConfigEffectiveMode: value}
	if showSelection {
		config[ahpConfigApprovalMode] = value
	}
	return emit.Emit(ctx, session, ahptypes.StateAction{Value: &ahptypes.SessionConfigChangedAction{
		Type: ahptypes.ActionTypeSessionConfigChanged, Config: config,
	}})
}

// updateSession changes a stored session. A caller whose next step depends on
// the change must check the error: the change happened only if it is nil.
func (b *ahpBackend) updateSession(ctx context.Context, userKey, session string, mutate func(*ahpSession)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var record ahpSession
	if err := b.getRecord(ctx, userKey, ahpRecordSession, session, &record); err != nil {
		return err
	}
	mutate(&record)
	return b.putRecord(ctx, userKey, ahpRecordSession, session, record)
}

// logSessionUpdate records a failed session update whose caller can go on.
func logSessionUpdate(err error, session, what string) {
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Error(err, "update AHP session", "session", session, "change", what)
	}
}

// ahpFileContent is what a changeset serves for one file of a session.
type ahpFileContent struct {
	Path   string `json:"path"`
	After  string `json:"after,omitempty"`
	Before string `json:"before,omitempty"`
	// Deleted marks a file the session removed: it has no after side.
	Deleted bool `json:"deleted,omitempty"`
}

const ahpContentScheme = "ahp-content:/"

// ReadResource serves the before and after contents of the files in a
// session's changeset, as the agent reported them. Nothing is read from a
// workspace or from this server's disk. On the root channel the session is
// found from the resource: a content URI names it, and a file is served from
// the user's session that last changed it.
func (b *ahpBackend) ReadResource(ctx context.Context, principal ahphost.Principal, session, channel, uri string) (ahptypes.ResourceReadResult, error) {
	var sessions []string
	if session == "" {
		records, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
		if err != nil {
			return ahptypes.ResourceReadResult{}, err
		}
		for _, record := range records {
			sessions = append(sessions, record.Key)
		}
	} else {
		var record ahpSession
		if err := b.getRecord(ctx, principal.UserKey, ahpRecordSession, session, &record); err != nil || channel != record.Changeset {
			return ahptypes.ResourceReadResult{}, ahphost.ErrResourceNotFound
		}
		sessions = []string{session}
	}
	var found store.AHPRecord
	ok := false
	for _, candidate := range sessions {
		hash := ahpHash(principal.UserKey, candidate)
		nonce := ""
		var updated time.Time
		switch {
		case strings.HasPrefix(uri, ahpContentScheme+hash[:24]+"/"):
			nonce = strings.TrimPrefix(uri, ahpContentScheme+hash[:24]+"/")
		case strings.HasPrefix(uri, "file:///"):
			filePath, ok := ahpURIPath(uri)
			if !ok {
				continue
			}
			fileRecord, err := b.store.GetAHPRecord(ctx, principal.UserKey, ahpRecordFile, hash+"/"+filePath)
			if err != nil {
				continue
			}
			var file ahpFileContent
			if json.Unmarshal(fileRecord.Value, &file) != nil {
				continue
			}
			nonce, updated = file.After, fileRecord.UpdatedAt
		}
		if nonce == "" {
			continue
		}
		content, err := b.store.GetAHPRecord(ctx, principal.UserKey, ahpRecordContent, hash+"/"+nonce)
		if err != nil {
			continue
		}
		if !updated.IsZero() {
			content.UpdatedAt = updated
		}
		if !ok || content.UpdatedAt.After(found.UpdatedAt) {
			found, ok = content, true
		}
	}
	if !ok {
		if session == "" {
			// Anything else on the root channel is a file in the user's workspace.
			return b.readWorkspaceFile(ctx, principal, uri)
		}
		return ahptypes.ResourceReadResult{}, ahphost.ErrResourceNotFound
	}
	return ahptypes.ResourceReadResult{Data: string(found.Value), Encoding: ahptypes.ContentEncodingUtf8}, nil
}

// ResolveSessionConfig resolves a new session's configuration for the form a
// client shows before creating it: the approval mode and the branches.
func (b *ahpBackend) ResolveSessionConfig(ctx context.Context, principal ahphost.Principal, params ahptypes.ResolveSessionConfigParams) (ahptypes.ResolveSessionConfigResult, error) {
	mode, err := ahpApprovalModeFrom(params.Config)
	if err != nil {
		return ahptypes.ResolveSessionConfigResult{}, &ahphost.Rejection{Reason: err.Error()}
	}
	config := ahpSessionConfig(mode)
	baseBranch, branch := "", ""
	if raw, ok := params.Config[ahpConfigBaseBranch]; ok {
		_ = json.Unmarshal(raw, &baseBranch)
	}
	if raw, ok := params.Config[ahpConfigBranch]; ok {
		_ = json.Unmarshal(raw, &branch)
	}
	if params.WorkingDirectory != nil && (baseBranch == "" || !ahpValidBranch(baseBranch)) {
		if project, err := b.projectForDirectory(ctx, principal.UserKey, []string{*params.WorkingDirectory}); err == nil {
			baseBranch = project.DefaultBranch
		}
	}
	notMutable := false
	baseDescription := "The branch the session starts from."
	branchDescription := "The branch the session's turns publish to. Orka names one when it is empty."
	config.Schema.Properties[ahpConfigBaseBranch] = ahptypes.SessionConfigPropertySchema{
		Type: ahpSchemaString, Title: "Base branch", Description: &baseDescription, SessionMutable: &notMutable,
	}
	config.Schema.Properties[ahpConfigBranch] = ahptypes.SessionConfigPropertySchema{
		Type: ahpSchemaString, Title: "Branch", Description: &branchDescription, SessionMutable: &notMutable,
	}
	if ahpValidBranch(baseBranch) {
		config.Values[ahpConfigBaseBranch], _ = json.Marshal(baseBranch)
	}
	if ahpValidBranch(branch) {
		config.Values[ahpConfigBranch], _ = json.Marshal(branch)
	}
	return ahptypes.ResolveSessionConfigResult{Schema: config.Schema, Values: config.Values}, nil
}

// gitCredential returns the user's repository credential, if configured.
func (b *ahpBackend) gitCredential(ctx context.Context, namespace string) *corev1alpha1.WorkspaceCredentialReference {
	if b.cfg.GitCredentialSecret == "" {
		return nil
	}
	// Read Secrets uncached: the manager's cache must not start watching
	// Secrets for this lookup.
	var reader client.Reader = b.h.client
	if b.h.apiReader != nil {
		reader = b.h.apiReader
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: b.cfg.GitCredentialSecret}, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "read the AHP git credential Secret", "namespace", namespace)
		}
		return nil
	}
	if len(secret.Data[ahpGitCredentialSecretKey]) == 0 {
		return nil
	}
	return &corev1alpha1.WorkspaceCredentialReference{Name: b.cfg.GitCredentialSecret, Key: ahpGitCredentialSecretKey}
}
