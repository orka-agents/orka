/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"cmp"
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

	"github.com/gofiber/fiber/v3"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
)

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

	ahpProjectRoot = "/workspace/src"

	ahpDefaultTurnTimeout = 2 * time.Hour
	ahpMaxPromptBytes     = 256 << 10
	ahpMaxProjects        = 200
	ahpPendingMax         = 10
	ahpPendingIDMax       = 128

	ahpAgentProvider = corev1alpha1.AgentRuntimeCopilot
)

const (
	ahpCopilotProjectManagementMeta  = "copilot.projectManagement"
	ahpCopilotConfigKey              = "copilot"
	ahpExtensionListProjects         = "extensions/listProjects"
	ahpExtensionCloneProject         = "extensions/cloneProject"
	ahpExtensionRemoveProject        = "extensions/removeProject"
	ahpExtensionGetPlan              = "extensions/getPlan"
	ahpExtensionSetSessionApproveAll = "extensions/setSessionApproveAll"
)

var ahpBuiltInRuntimes = []struct {
	runtime corev1alpha1.AgentRuntimeType
	name    string
}{
	{corev1alpha1.AgentRuntimeCopilot, "Copilot CLI"},
	{corev1alpha1.AgentRuntimeClaude, "Claude Code"},
	{corev1alpha1.AgentRuntimeCodex, "Codex"},
	{corev1alpha1.AgentRuntimeOpencode, "OpenCode"},
}

func ahpRuntimeName(runtime corev1alpha1.AgentRuntimeType) string {
	for _, r := range ahpBuiltInRuntimes {
		if r.runtime == runtime {
			return r.name
		}
	}
	return string(runtime)
}

type AHPBackendConfig struct {
	GitCredentialSecret  string
	WorkspaceClass       string
	TurnTimeout          time.Duration
	WorkspaceNamespace   string
	WorkspaceImage       string
	WorkspaceIdleTimeout time.Duration
	RepositoryCredential func(ctx context.Context, namespace, issuer, subject string) (string, error)
}

type ahpApprovalDecider func(ctx context.Context, ui *UserInfo, namespace, task, approvalID string, approve bool) error

type ahpBackend struct {
	h       *Handlers
	store   store.AHPStore
	live    *liveevents.Broker
	cfg     AHPBackendConfig
	now     func() time.Time
	gitHTTP *http.Client

	applyApprovalMode          func(task *corev1alpha1.Task, manual bool)
	decideApproval             ahpApprovalDecider
	recheckToken               func(context.Context, ahphost.Principal) error
	pendingAuth                map[string]ahpPendingSender
	emitterFor                 func(ahphost.Principal) ahphost.Emitter
	updateTerminals            func(context.Context, ahphost.Principal) error
	cancelBackoff, modeBackoff wait.Backoff
	readWorkspace              func(ctx context.Context, principal ahphost.Principal, uri string) (result ahptypes.ResourceReadResult, running bool, err error)
	workspaces                 *ahpWorkspaces

	mu           sync.Mutex
	turns        map[string]*ahpTurn
	shells       map[string]*ahpTerminal
	retired      map[string]time.Time
	reserved     map[string]time.Time
	recovered    map[string]bool
	lifecycle    map[string]*sync.Mutex
	changes      map[string]*sync.Mutex
	releasing    map[string]map[string]bool
	unshown      map[string]map[string]bool
	sessionRetry time.Duration
}

func (b *ahpBackend) lockUser(userKey string) func() {
	return b.lockIn(&b.lifecycle, userKey)
}

func (b *ahpBackend) lockChanges(userKey string) func() {
	return b.lockIn(&b.changes, userKey)
}

func (b *ahpBackend) lockIn(locks *map[string]*sync.Mutex, userKey string) func() {
	b.mu.Lock()
	if *locks == nil {
		*locks = map[string]*sync.Mutex{}
	}
	lock := (*locks)[userKey]
	if lock == nil {
		lock = &sync.Mutex{}
		(*locks)[userKey] = lock
	}
	b.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

const ahpReservationTTL = 30 * time.Second

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
	b := &ahpBackend{
		h: h, store: s, live: live, cfg: cfg, now: time.Now, gitHTTP: ahpGitHTTPClient,
		applyApprovalMode: ahpApplyApprovalMode, decideApproval: h.DecideTaskApprovalAs,
		turns: map[string]*ahpTurn{}, shells: map[string]*ahpTerminal{}, reserved: map[string]time.Time{}, recovered: map[string]bool{},
	}
	b.readWorkspace = b.readWorkspaceFile
	b.cancelBackoff = wait.Backoff{Duration: time.Second, Factor: 2, Steps: 7, Cap: 30 * time.Second}
	b.modeBackoff = wait.Backoff{Duration: 50 * time.Millisecond, Factor: 2, Steps: 6, Cap: time.Second}
	return b
}

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

type ahpProject struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Owner         string    `json:"owner"`
	URL           string    `json:"url"`
	Path          string    `json:"path"`
	DefaultBranch string    `json:"defaultBranch,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (p ahpProject) wire() map[string]any {
	project := map[string]any{
		"id": p.ID, apiFieldName: p.Name, "path": p.Path, "git": true, "origin": "cloned",
		"remoteUrl": p.URL, apiFieldStatus: "ready",
	}
	if p.DefaultBranch != "" {
		project["defaultBranch"] = p.DefaultBranch
	}
	return project
}

type ahpSession struct {
	Session         string        `json:"session"`
	Chat            string        `json:"chat"`
	Changeset       string        `json:"changeset"`
	Namespace       string        `json:"namespace"`
	OrkaSession     string        `json:"orkaSession"`
	ProjectID       string        `json:"projectId"`
	RepoURL         string        `json:"repoUrl"`
	BaseBranch      string        `json:"baseBranch,omitempty"`
	Runtime         string        `json:"runtime"`
	Branch          string        `json:"branch,omitempty"`
	WorkDir         string        `json:"workDir,omitempty"`
	Agent           string        `json:"agent,omitempty"`
	Model           string        `json:"model,omitempty"`
	ApprovalMode    string        `json:"approvalMode"`
	Turns           int           `json:"turns"`
	ActiveTurn      string        `json:"activeTurn,omitempty"`
	ActiveTask      string        `json:"activeTask,omitempty"`
	ActiveTaskUID   string        `json:"activeTaskUID,omitempty"`
	ActiveStartedAt string        `json:"activeStartedAt,omitempty"`
	Plan            string        `json:"plan,omitempty"`
	Generation      int           `json:"generation,omitempty"`
	SessionTurns    int           `json:"sessionTurns,omitempty"`
	SourceBranch    string        `json:"sourceBranch,omitempty"`
	Published       bool          `json:"published,omitempty"`
	History         []ahpExchange `json:"history,omitempty"`
	Pending         []ahpPending  `json:"pending,omitempty"`
}

type ahpPending struct {
	ID      string           `json:"id"`
	Kind    string           `json:"kind"`
	Message ahptypes.Message `json:"message"`
	Version string           `json:"version,omitempty"`
}

type ahpPendingSender struct {
	principal ahphost.Principal
	version   string
}

type ahpExchange struct {
	User  string `json:"user"`
	Reply string `json:"reply,omitempty"`
}

func ahpPrincipalUserInfo(p ahphost.Principal) *UserInfo {
	return &UserInfo{AuthType: AuthTypeOIDC, Issuer: p.Issuer, Subject: p.Subject, Username: p.Username, Namespace: p.Namespace}
}

func ahpHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func ahpOrkaSessionName(userKey string, record ahpSession, generation int) string {
	name := "ahp-" + ahpHash(userKey, record.Session, record.Chat)[:24]
	if generation > 0 {
		name += fmt.Sprintf("-g%d", generation)
	}
	return name
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

type ahpAgentChoice struct {
	Agent   string
	Runtime corev1alpha1.AgentRuntimeType
	Model   string
}

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
		if meta.IsStatusConditionFalse(agent.Status.Conditions, "Ready") {
			continue
		}
		if err := authorizeAgentUse(ctx, b.h.clientset, ahpPrincipalUserInfo(principal), agent.Namespace, agent.Name,
			"AHPSession", ""); err != nil {
			continue
		}
		seen[key] = true
		choices = append(choices, ahpAgentChoice{Agent: agent.Name, Runtime: runtime, Model: agent.Spec.Model.Name})
	}
	return choices, nil
}

func (b *ahpBackend) hasSession(ctx context.Context, principal ahphost.Principal) bool {
	sessions, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	return err == nil && len(sessions) > 0
}

var _ ahphost.ConnectionPreparer = (*ahpBackend)(nil)

func (b *ahpBackend) PrepareConnection(ctx context.Context, principal ahphost.Principal) {
	b.recoverTurns(ctx, principal)
}

func (b *ahpBackend) Agents(ctx context.Context, principal ahphost.Principal) ([]ahptypes.AgentInfo, error) {
	b.mu.Lock()
	recovered := b.recovered[principal.UserKey]
	b.mu.Unlock()
	if !recovered {
		go b.recoverTurns(context.WithoutCancel(ctx), principal)
	}
	if b.cfg.WorkspaceIdleTimeout > 0 || b.hasSession(ctx, principal) {
		b.workspaces.warm(principal)
	}
	choices, err := b.agentChoices(ctx, principal)
	if err != nil {
		return nil, err
	}
	enabled := ahptypes.PolicyStateEnabled
	var models []ahptypes.SessionModelInfo
	listed := map[string]bool{}
	for _, runtime := range ahpBuiltInRuntimes {
		for _, choice := range choices {
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

func (b *ahpBackend) backfillDefaultBranch(ctx context.Context, userKey, id, branch string) {
	unlock := b.lockUser(userKey)
	defer unlock()
	var current ahpProject
	if b.getRecord(ctx, userKey, ahpRecordProject, id, &current) != nil || current.DefaultBranch != "" {
		return
	}
	current.DefaultBranch = branch
	_ = b.putRecord(ctx, userKey, ahpRecordProject, id, current)
}

func ahpRootConfig(projects []ahpProject) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(map[string]any{"hostName": "orka", "projects": ahpProjectsWire(projects)})
	if err != nil {
		return nil, err
	}
	return map[string]json.RawMessage{ahpCopilotConfigKey: raw}, nil
}

func (b *ahpBackend) HandleExtension(ctx context.Context, principal ahphost.Principal, method string, params json.RawMessage, emit ahphost.Emitter) (any, error) {
	switch method {
	case ahpExtensionListProjects:
		projects, err := b.projects(ctx, principal.UserKey)
		if err != nil {
			return nil, err
		}
		for i := range projects {
			if projects[i].DefaultBranch == "" {
				if branch := b.defaultBranch(ctx, principal.Namespace, projects[i].URL); branch != "" {
					projects[i].DefaultBranch = branch
					b.backfillDefaultBranch(ctx, principal.UserKey, projects[i].ID, branch)
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

func (b *ahpBackend) addProject(ctx context.Context, principal ahphost.Principal, raw string) (ahpProject, error) {
	repo, owner, name, err := ahpCanonicalRepository(raw)
	if err != nil {
		return ahpProject{}, &ahphost.Rejection{Reason: err.Error()}
	}
	id := "project-" + ahpHash(repo)[:16]
	var existing ahpProject
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordProject, id, &existing); err == nil {
		if existing.DefaultBranch == "" {
			if branch := b.defaultBranch(ctx, principal.Namespace, repo); branch != "" {
				b.backfillDefaultBranch(ctx, principal.UserKey, id, branch)
				existing.DefaultBranch = branch
			}
		}
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return ahpProject{}, err
	}
	project := ahpProject{
		ID: id, Name: name, Owner: owner, URL: repo, Path: path.Join(ahpProjectRoot, owner, name), CreatedAt: b.now().UTC(),
		DefaultBranch: b.defaultBranch(ctx, principal.Namespace, repo),
	}
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	projects, err := b.projects(ctx, principal.UserKey)
	if err != nil {
		return ahpProject{}, err
	}
	if len(projects) >= ahpMaxProjects {
		return ahpProject{}, &ahphost.Rejection{Reason: fmt.Sprintf("at most %d projects are allowed", ahpMaxProjects)}
	}
	for _, other := range projects {
		if other.ID == id {
			return other, nil
		}
		if other.Path == project.Path {
			return ahpProject{}, &ahphost.Rejection{Reason: fmt.Sprintf("the project %s already uses %s", other.URL, project.Path)}
		}
	}
	return project, b.putRecord(ctx, principal.UserKey, ahpRecordProject, id, project)
}

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
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\ ?#`) {
			return "", "", "", errors.New("the repository URL is not valid")
		}
	}
	return fmt.Sprintf("https://%s/%s/%s", strings.ToLower(parsed.Host), owner, name), owner, name, nil
}

func ahpDirectoryURI(dir string) string { return (&url.URL{Scheme: "file", Path: dir}).String() }

func ahpURIPath(uri string) (string, bool) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" || strings.ContainsAny(parsed.Path, "\x00\n") {
		return "", false
	}
	return parsed.Path, true
}

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
		Schema: ahptypes.SessionConfigSchema{Type: mcpSchemaTypeObject, Properties: map[string]ahptypes.SessionConfigPropertySchema{
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
		return ahpApprovalManual, nil
	default:
		return "", fmt.Errorf("approvalMode %q is not supported", mode)
	}
}

func (b *ahpBackend) CreateSession(ctx context.Context, principal ahphost.Principal, req ahphost.CreateSessionRequest, _ ahphost.Emitter) (*ahphost.CreatedSession, error) {
	for _, reserved := range []string{"ahp-chat:", "ahp-changeset:", ahpContentScheme, ahpTerminalScheme} {
		if strings.HasPrefix(req.Session, reserved) {
			return nil, &ahphost.Rejection{Reason: "the session URI uses a scheme reserved for another kind of channel"}
		}
	}
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
	branch := ""
	if raw, ok := req.Config[ahpConfigBranch]; ok {
		_ = json.Unmarshal(raw, &branch)
	}
	if !ahpValidBranch(branch) || ahpBranchesClash(branch, baseBranch) ||
		(b.workspaces.enabled() && ahpBranchesClash(branch, project.DefaultBranch)) {
		branch = "orka/session-" + ahpRandomHex(8)
	}
	existing, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	if err != nil {
		return nil, err
	}
	for _, raw := range existing {
		var other ahpSession
		if json.Unmarshal(raw.Value, &other) == nil && other.ProjectID == project.ID && ahpBranchesClash(other.Branch, branch) {
			if other.Branch != branch {
				return nil, &ahphost.Rejection{Reason: fmt.Sprintf("the branch %s clashes with the branch %s another session uses", branch, other.Branch)}
			}
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
		ProjectID: project.ID, RepoURL: project.URL,
		BaseBranch: baseBranch, Runtime: req.Provider, ApprovalMode: mode, Branch: branch, WorkDir: workDir,
	}
	record.OrkaSession = ahpOrkaSessionName(principal.UserKey, record, 0)
	if err := b.putRecord(ctx, principal.UserKey, ahpRecordChat, chat, req.Session); err != nil {
		return nil, err
	}
	if err := b.putRecord(ctx, principal.UserKey, ahpRecordSession, req.Session, record); err != nil {
		_ = b.store.DeleteAHPRecord(context.WithoutCancel(ctx), principal.UserKey, ahpRecordChat, chat)
		return nil, err
	}
	if b.workspaces.enabled() {
		if pod, err := b.workspaces.ensure(ctx, principal, ahpWorkspaceStartWait); err == nil {
			setupCtx, cancel := context.WithTimeout(ctx, ahpWorkspaceSetupWait)
			_ = b.workspaces.ensureWorktree(setupCtx, principal, pod, project, record)
			cancel()
		}
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

func (b *ahpBackend) DisposeSession(ctx context.Context, principal ahphost.Principal, session string) error {
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	var record ahpSession
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordSession, session, &record); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if record.ActiveTask != "" {
		if err := b.cancelTask(ctx, principal, record.Namespace, record.ActiveTask, record.ActiveTaskUID); err != nil {
			return fmt.Errorf("cancel the session's running Task: %w", err)
		}
	}
	if turn := b.activeTurn(principal.UserKey, session); turn != nil {
		turn.stop()
	}
	if b.workspaces.enabled() {
		sessions, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
		terminals := b.hasTerminals(principal.UserKey)
		removed := err == nil && len(sessions) <= 1 && !terminals && b.workspaces.removeUnlessHoldsWork(ctx, principal)
		if !removed && !terminals {
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
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.releasing, hash)
	delete(b.unshown, hash)
	return b.store.DeleteAHPRecord(ctx, principal.UserKey, ahpRecordSession, session)
}

func (b *ahpBackend) cancelTask(ctx context.Context, principal ahphost.Principal, namespace, name, uid string) error {
	task := &corev1alpha1.Task{}
	if err := b.h.uncachedReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if uid != "" && string(task.UID) != uid {
		return nil
	}
	ui := ahpPrincipalUserInfo(principal)
	if !ahpOwns(ui, task) {
		return fiber.NewError(fiber.StatusForbidden, "only the Task's requester may cancel it")
	}
	_, err := b.h.recordTaskCancellation(ctx, task, string(task.UID), cancelMessage(ui, ""))
	if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
		switch fiberErr.Code {
		case fiber.StatusNotFound, fiber.StatusConflict:
			return nil
		}
	}
	return err
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
		choice, ok := ahpChooseModel(choices, record.Runtime, wanted)
		if !ok {
			return ahpAgentChoice{}, false, &ahphost.Rejection{Reason: "this session's agent is no longer available to you"}
		}
		return choice, true, nil
	}
	choice, ok := ahpChooseModel(choices, record.Runtime, wanted)
	if !ok {
		return ahpAgentChoice{}, false, &ahphost.Rejection{Reason: "the selected model is not available to you"}
	}
	return choice, record.Agent != "", nil
}

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
		return nil
	default:
		return &ahphost.Rejection{Reason: "not supported on this host"}
	}
}

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

func (b *ahpBackend) unqueue(ctx context.Context, record ahpSession, a *ahptypes.ChatPendingMessageSetAction, key, version string,
	prior ahpPendingSender, emit ahphost.Emitter) {
	b.mu.Lock()
	if b.pendingAuth[key].version == version {
		if prior.version != "" {
			b.pendingAuth[key] = prior
		} else {
			delete(b.pendingAuth, key)
		}
	}
	b.mu.Unlock()
	i := slices.IndexFunc(record.Pending, func(p ahpPending) bool { return p.ID == a.Id })
	if i < 0 {
		_ = emit.Emit(ctx, record.Chat, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageRemovedAction{
			Type: ahptypes.ActionTypeChatPendingMessageRemoved, Kind: a.Kind, Id: a.Id,
		}})
		return
	}
	var actions []ahptypes.StateAction
	if a.Kind == ahptypes.PendingMessageKindSteering {
		actions = append(actions, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageRemovedAction{
			Type: ahptypes.ActionTypeChatPendingMessageRemoved, Kind: ahptypes.PendingMessageKindSteering, Id: a.Id,
		}})
	}
	actions = append(actions, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageSetAction{
		Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: a.Id, Message: record.Pending[i].Message,
	}})
	_ = emit.Emit(ctx, record.Chat, actions...)
}

func (b *ahpBackend) cancelTurn(ctx context.Context, principal ahphost.Principal, record ahpSession, turnID string) {
	if record.ActiveTurn != turnID {
		return
	}
	if turn := b.activeTurn(principal.UserKey, record.Session); turn != nil && turn.id == turnID {
		turn.markCancelledByClient()
	}
	if record.ActiveTask == "" {
		return
	}
	b.keepCancelling(ctx, principal, record.Session, record.Namespace, record.ActiveTask, record.ActiveTaskUID)
}

func (b *ahpBackend) keepCancelling(ctx context.Context, principal ahphost.Principal, session, namespace, task, uid string) {
	go func() {
		err := retry.OnError(b.cancelBackoff, func(error) bool { return true }, func() error {
			return b.cancelTask(ctx, principal, namespace, task, uid)
		})
		if err != nil {
			log.Error(err, "cancel AHP turn Task", "session", session, "task", task)
		}
	}()
}

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
		b.cancelTurn(ctx, principal, record, a.TurnId)
	case *ahptypes.ChatToolCallConfirmedAction:
		if turn := b.activeTurn(principal.UserKey, record.Session); turn != nil && turn.id == a.TurnId {
			turn.confirm(ctx, principal, a.ToolCallId, a.Approved)
		}
	case *ahptypes.ChatPendingMessageSetAction:
		version := ahpRandomHex(16)
		key := ahpPendingKey(principal.UserKey, record.Session, a.Id)
		b.mu.Lock()
		if b.pendingAuth == nil {
			b.pendingAuth = map[string]ahpPendingSender{}
		}
		prior, replacing := b.pendingAuth[key]
		b.pendingAuth[key] = ahpPendingSender{principal: principal, version: version}
		b.mu.Unlock()
		steering := a.Kind == ahptypes.PendingMessageKindSteering
		var order []string
		full := false
		err := b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
			if !slices.ContainsFunc(s.Pending, func(p ahpPending) bool { return p.ID == a.Id }) && len(s.Pending) >= ahpPendingMax {
				full = true
				return
			}
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
		})
		if err != nil || full {
			logSessionUpdate(err, record.Session, "queue a message")
			if !replacing {
				prior = ahpPendingSender{}
			}
			b.unqueue(ctx, record, a, key, version, prior, emit)
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
		err = retry.OnError(b.modeBackoff, func(error) bool { return true }, func() error {
			return b.setApprovalMode(ctx, principal.UserKey, record.Session, mode, emit, false)
		})
		if err == nil {
			return
		}
		log.Error(err, "store the AHP session's approval mode", "session", record.Session)
		var current ahpSession
		if b.getRecord(ctx, principal.UserKey, ahpRecordSession, record.Session, &current) != nil {
			return
		}
		value, _ := json.Marshal(current.ApprovalMode)
		_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionConfigChangedAction{
			Type:   ahptypes.ActionTypeSessionConfigChanged,
			Config: map[string]json.RawMessage{ahpConfigApprovalMode: value, ahpConfigEffectiveMode: value},
		}})
	}
}

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

func (b *ahpBackend) updateSessionUntilDone(ctx context.Context, userKey, session, what string, mutate func(*ahpSession)) <-chan struct{} {
	done := make(chan struct{})
	err := b.updateSession(ctx, userKey, session, mutate)
	logSessionUpdate(err, session, what)
	if err == nil || errors.Is(err, store.ErrNotFound) {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ctx := context.WithoutCancel(ctx)
		for delay := b.retryWait(); err != nil && !errors.Is(err, store.ErrNotFound); delay = min(2*delay, 32*b.retryWait()) {
			time.Sleep(delay)
			err = b.updateSession(ctx, userKey, session, mutate)
			logSessionUpdate(err, session, what)
		}
	}()
	return done
}

func (b *ahpBackend) retryWait() time.Duration { return cmp.Or(b.sessionRetry, time.Second) }

func logSessionUpdate(err error, session, what string) {
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Error(err, "update AHP session", "session", session, "change", what)
	}
}

type ahpFileContent struct {
	Path    string `json:"path"`
	After   string `json:"after,omitempty"`
	Before  string `json:"before,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

const ahpContentScheme = "ahp-content:/"

func (b *ahpBackend) ReadResource(ctx context.Context, principal ahphost.Principal, session, channel, uri string) (ahptypes.ResourceReadResult, error) {
	if session == "" {
		if result, running, err := b.readWorkspace(ctx, principal, uri); running {
			return result, err
		}
	}
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
	ok, deleted := false, false
	for _, candidate := range sessions {
		hash := ahpHash(principal.UserKey, candidate)
		nonce := ""
		var updated time.Time
		switch {
		case strings.HasPrefix(uri, ahpContentScheme+hash[:24]+"/"):
			nonce = strings.TrimPrefix(uri, ahpContentScheme+hash[:24]+"/")
		case strings.HasPrefix(uri, "file:///"):
			filePath, valid := ahpURIPath(uri)
			if !valid {
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
			if file.Deleted {
				if !ok || updated.After(found.UpdatedAt) {
					found, ok, deleted = store.AHPRecord{UpdatedAt: updated}, true, true
				}
				continue
			}
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
			found, ok, deleted = content, true, false
		}
	}
	if !ok || deleted {
		return ahptypes.ResourceReadResult{}, ahphost.ErrResourceNotFound
	}
	return ahptypes.ResourceReadResult{Data: string(found.Value), Encoding: ahptypes.ContentEncodingUtf8}, nil
}

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

func (b *ahpBackend) gitCredential(ctx context.Context, namespace string) *corev1alpha1.WorkspaceCredentialReference {
	if len(b.gitSecretToken(ctx, namespace)) == 0 {
		return nil
	}
	return &corev1alpha1.WorkspaceCredentialReference{Name: b.cfg.GitCredentialSecret, Key: ahpGitCredentialSecretKey}
}

func (b *ahpBackend) gitSecretToken(ctx context.Context, namespace string) []byte {
	if b.cfg.GitCredentialSecret == "" {
		return nil
	}
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
	return secret.Data[ahpGitCredentialSecretKey]
}
