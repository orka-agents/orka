// Package service coordinates one durable report/reproduce/fix/verify run.
// Model replies propose inputs; only sealed runner observations gate progress.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/labels"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
	"github.com/orka-agents/orka/internal/store"
)

const (
	MaxReportBytes = 64 << 10
	MaxPatchBytes  = 256 << 10
	maxPromptBytes = 96 << 10
	defaultTimeout = 45 * time.Minute
)

type SourceReader interface {
	Resolve(context.Context, string, string) (source.Target, error)
	Inventory(context.Context, source.Target) ([]source.Entry, error)
	Packet(context.Context, source.Target, []string) (source.Packet, error)
}

type Config struct {
	Client    client.Client
	Reader    client.Reader
	Namespace string
	AgentName string
	// AIWorkerImage must match the controller's digest-pinned --ai-worker-image.
	// The operator must select an updated worker that implements the memory
	// opt-outs; image identity alone cannot prove an arbitrary image honors them.
	// BoundedExecution additionally requires the durable model-permit protocol
	// and physical HTTP-attempt guard in that qualified worker image.
	AIWorkerImage string
	Results       store.ResultStore
	Backend       contract.ExecutionBackend
	Source        SourceReader
	PollInterval  time.Duration
	RunTimeout    time.Duration
	// BoundedExecution freezes the single-active-run, single-attempt-per-role
	// native worker policy. Existing source-harness runs retain their policy.
	BoundedExecution bool
	// PrepareBackend runs under elected controller ownership before admission.
	PrepareBackend func(context.Context) error
}

type Coordinator struct {
	config    Config
	mu        sync.Mutex
	active    map[string]context.CancelFunc
	noEffects map[noEffectsKey]noEffectsProof
	prepared  atomic.Bool
}

// Service is the controller-manager coordinator used by server composition.
type Service = Coordinator

var _ contract.Service = (*Coordinator)(nil)

func New(config Config) (*Coordinator, error) {
	if config.Client == nil || config.Reader == nil || config.Results == nil || config.Backend == nil ||
		len(kubevalidation.IsDNS1123Label(config.Namespace)) != 0 ||
		len(kubevalidation.IsDNS1123Subdomain(config.AgentName)) != 0 || !pinnedAIWorkerImage(config.AIWorkerImage) {
		return nil, contract.ErrInvalid
	}
	if store.ValidateCanonicalDigest("backend", config.Backend.ConfigurationDigest()) != nil {
		return nil, contract.ErrInvalid
	}
	if backend, ok := config.Backend.(contract.EvidenceScopedBackend); ok &&
		backend.EvidenceScope() != "" && !backend.EvidenceScope().Valid() {
		return nil, contract.ErrInvalid
	}
	if config.PollInterval == 0 {
		config.PollInterval = time.Second
	}
	if config.RunTimeout == 0 {
		config.RunTimeout = defaultTimeout
	}
	if config.PollInterval < time.Millisecond || config.PollInterval > 30*time.Second ||
		config.RunTimeout < time.Second || config.RunTimeout > 2*time.Hour {
		return nil, contract.ErrInvalid
	}
	if config.BoundedExecution && config.RunTimeout > contract.MaxRunDuration {
		return nil, contract.ErrInvalid
	}
	if backend, ok := config.Backend.(contract.EvidenceScopedBackend); ok &&
		backend.EvidenceScope() == contract.ApplicationRuntime {
		if _, complete := config.Backend.(contract.ApplicationPlanningBackend); !complete || !config.BoundedExecution {
			return nil, contract.ErrInvalid
		}
	}
	if config.Source == nil {
		config.Source = source.Client{}
	}
	return &Coordinator{
		config: config, active: make(map[string]context.CancelFunc),
		noEffects: make(map[noEffectsKey]noEffectsProof),
	}, nil
}

// CheckReady must run before exposing submission. It also prevents restart with
// changed authority while accepted work still depends on the old configuration.
func (s *Coordinator) CheckReady(ctx context.Context) error {
	current, err := s.agentAuthority(ctx)
	if err != nil {
		return err
	}
	if err := s.checkRetainedAuthority(ctx, current); err != nil {
		return err
	}
	return s.checkBackendCapability()
}

// CheckConfiguredAuthority compares retained native/backend authority before
// an opt-in backend performs startup qualification or recovery. The explicit
// scope is used only for this comparison, never to grant admission or evidence.
func CheckConfiguredAuthority(ctx context.Context, config Config, expectedScope contract.EvidenceScope) error {
	if !expectedScope.Valid() {
		return contract.ErrInvalid
	}
	if expectedScope == contract.ApplicationRuntime {
		if _, ok := config.Backend.(contract.ApplicationPlanningBackend); !ok || !config.BoundedExecution {
			return contract.ErrInvalid
		}
	}
	coordinator, err := New(config)
	if err != nil {
		return err
	}
	current, err := coordinator.agentAuthority(ctx)
	if err != nil {
		return err
	}
	current.EvidenceScope = expectedScope
	return coordinator.checkRetainedAuthority(ctx, current)
}

func (s *Coordinator) checkRetainedAuthority(ctx context.Context, current authority) error {
	items, err := listPendingStates(ctx, s.config.Reader, s.config.Namespace)
	if err != nil {
		return err
	}
	for index := range items {
		r, err := s.prepareListedRun(ctx, &items[index])
		if err != nil {
			return err
		}
		if (!contract.TerminalPhase(r.state.Status.Phase) || !r.state.Settled) && !authorityMatches(r.state.Authority, current) {
			return contract.ErrConflict
		}
	}
	return nil
}

func (s *Coordinator) checkBackendCapability() error {
	if !s.backendEvidenceScope().Valid() {
		return contract.ErrEvidenceScopeUnsupported
	}
	if s.backendEvidenceScope() == contract.ApplicationRuntime {
		if _, ok := s.config.Backend.(contract.ApplicationPlanningBackend); !ok || !s.config.BoundedExecution {
			return contract.ErrInvalid
		}
	}
	return nil
}

func (s *Coordinator) agentAuthority(ctx context.Context) (authority, error) {
	agent := new(corev1alpha1.Agent)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: s.config.AgentName}, agent); err != nil {
		return authority{}, contract.ErrUnavailable
	}
	spec := &agent.Spec
	if agent.UID == "" || agent.DeletionTimestamp != nil || spec.Runtime != nil || spec.Soul != nil || spec.Tools == nil ||
		len(spec.Tools) != 0 || len(spec.Skills) != 0 || spec.Session != nil || spec.Coordination != nil ||
		(spec.Execution != nil && spec.Execution.Workspace != nil) ||
		(spec.SystemPrompt != nil && spec.SystemPrompt.ConfigMapRef != nil) ||
		(spec.Model != nil && len(spec.Model.Fallbacks) != 0) {
		return authority{}, contract.ErrInvalid
	}
	probe := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{labels.AnnotationDisableCoordinationToolInject: boolTrue}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI, AI: &corev1alpha1.AISpec{Tools: []string{}},
			Env: privateAgentEnvironment(),
		},
	}
	if len(aitools.Resolve(probe, agent)) != 0 {
		return authority{}, contract.ErrInvalid
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return authority{}, contract.ErrInvalid
	}
	result := authority{
		BackendDigest:    s.config.Backend.ConfigurationDigest(),
		EvidenceScope:    s.backendEvidenceScope(),
		AIWorkerImage:    s.config.AIWorkerImage,
		BoundedExecution: s.config.BoundedExecution,
		AgentName:        agent.Name, AgentUID: string(agent.UID), AgentGeneration: agent.Generation, AgentDigest: pv.Digest(data),
	}
	if spec.ProviderRef != nil {
		if spec.ProviderRef.Namespace != "" && spec.ProviderRef.Namespace != s.config.Namespace {
			return authority{}, contract.ErrInvalid
		}
		provider := new(corev1alpha1.Provider)
		if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: spec.ProviderRef.Name}, provider); err != nil {
			return authority{}, contract.ErrUnavailable
		}
		if provider.UID == "" || provider.DeletionTimestamp != nil {
			return authority{}, contract.ErrInvalid
		}
		data, err := json.Marshal(provider.Spec)
		if err != nil {
			return authority{}, contract.ErrInvalid
		}
		result.ProviderName, result.ProviderUID = provider.Name, string(provider.UID)
		result.ProviderGeneration, result.ProviderDigest = provider.Generation, pv.Digest(data)
	} else if spec.Model == nil || spec.Model.Provider == "" || spec.Model.Name == "" {
		return authority{}, contract.ErrInvalid
	}
	return result, nil
}

func (s *Coordinator) backendEvidenceScope() contract.EvidenceScope {
	if backend, ok := s.config.Backend.(contract.EvidenceScopedBackend); ok {
		return backend.EvidenceScope()
	}
	return ""
}

func authorityMatches(saved, current authority) bool {
	// Legacy executions keep their original bytes and digests, and remain
	// unlabelled. They may resume only under the existing source-harness backend.
	if saved.EvidenceScope == "" && current.EvidenceScope == contract.SourceHarness {
		current.EvidenceScope = ""
	}
	return saved == current
}

func validateRequest(request contract.Request) error {
	if !validID(request.RequestID) || strings.TrimSpace(request.Repository) == "" ||
		strings.TrimSpace(request.Ref) == "" || len(request.Repository) > 512 || len(request.Ref) > 1024 ||
		len(request.Report) == 0 || len(request.Report) > MaxReportBytes || !utf8.Valid(request.Report) ||
		(pv.CredentialMatcher{}).Match(request.Report) ||
		(request.RequiredEvidenceScope != "" && !request.RequiredEvidenceScope.Valid()) {
		return contract.ErrInvalid
	}
	switch request.Mode {
	case contract.Generate, contract.Validate:
	default:
		return contract.ErrInvalid
	}
	report, err := intake.Parse(request.Report)
	if err != nil || report.Title == "" || !slices.ContainsFunc(report.Sections, func(section intake.Section) bool {
		return (section.Kind == "problem" || section.Kind == "description" || section.Kind == "summary") &&
			strings.TrimSpace(section.Text) != ""
	}) {
		return contract.ErrInvalid
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(request.Report, &metadata) != nil {
		return contract.ErrInvalid
	}
	for field, expected := range map[string]string{"repository": request.Repository, "ref": request.Ref} {
		if raw, exists := metadata[field]; exists {
			var value string
			if json.Unmarshal(raw, &value) != nil || value != expected {
				return contract.ErrInvalid
			}
		}
	}
	return nil
}

func validID(id string) bool {
	return len(id) <= 63 && len(kubevalidation.IsDNS1123Subdomain(id)) == 0
}

func (s *Coordinator) Submit(ctx context.Context, namespace, subject string, request contract.Request) (*contract.Status, error) {
	if namespace != s.config.Namespace {
		return nil, contract.ErrForbidden
	}
	if strings.TrimSpace(subject) == "" || len(subject) > 2048 || validateRequest(request) != nil {
		return nil, contract.ErrInvalid
	}
	in := input{
		ID: request.RequestID, Mode: request.Mode, Report: bytes.Clone(request.Report), Repository: request.Repository,
		Ref: request.Ref, SubjectDigest: pv.Digest([]byte(subject)), RequiredEvidenceScope: request.RequiredEvidenceScope,
	}
	inputData, err := json.Marshal(in)
	if err != nil || len(inputData) > maxInputBytes {
		return nil, contract.ErrInvalid
	}
	existing, err := s.load(ctx, request.RequestID)
	if err == nil {
		if existing.state.InputDigest != pv.Digest(inputData) {
			return nil, contract.ErrConflict
		}
		status := existing.state.Status
		return &status, nil
	}
	if !errors.Is(err, contract.ErrNotFound) {
		return nil, err
	}
	if s.config.PrepareBackend != nil && !s.prepared.Load() {
		return nil, contract.ErrUnavailable
	}
	if err := contract.RequireEvidenceScope(request.RequiredEvidenceScope, s.backendEvidenceScope()); err != nil {
		return nil, err
	}
	if err := s.checkBackendCapability(); err != nil {
		return nil, err
	}
	identity, err := s.agentAuthority(ctx)
	if err != nil {
		return nil, err
	}
	if err := contract.RequireEvidenceScope(request.RequiredEvidenceScope, identity.EvidenceScope); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	st := state{
		Version: stateVersion, ID: request.RequestID, Namespace: namespace, InputDigest: pv.Digest(inputData),
		Authority: identity, CreatedAt: now, Deadline: now.Add(s.config.RunTimeout), Step: stepSource,
		Status: contract.Status{ID: request.RequestID, Namespace: namespace, Phase: contract.PhasePending,
			Stage: contract.StageReproduce, Reason: "accepted", EvidenceScope: identity.EvidenceScope},
		Agents: make(map[string]*taskOperation), Executions: make(map[string]*executionOperation),
	}
	stateData, err := json.Marshal(st)
	if err != nil {
		return nil, contract.ErrInvalid
	}
	object := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: stateName(request.RequestID), Namespace: namespace,
			Labels: map[string]string{stateLabel: "v1", activeLabel: boolTrue}},
		Data: map[string]string{"input": string(inputData), "state": string(stateData)},
	}
	if err := s.config.Client.Create(ctx, object); err != nil {
		existing, readErr := s.load(ctx, request.RequestID)
		if readErr != nil {
			return nil, contract.ErrUnavailable
		}
		if existing.state.InputDigest != pv.Digest(inputData) {
			return nil, contract.ErrConflict
		}
		status := existing.state.Status
		return &status, nil
	}
	status := st.Status
	return &status, nil
}

func (s *Coordinator) Get(ctx context.Context, namespace, id string) (*contract.Status, error) {
	if namespace != s.config.Namespace {
		return nil, contract.ErrForbidden
	}
	if !validID(id) {
		return nil, contract.ErrInvalid
	}
	r, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	status := r.state.Status
	if r.state.Parent.UID != "" {
		parent := new(corev1alpha1.Task)
		if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: r.state.Parent.Name}, parent); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, contract.ErrNotFound
			}
			return nil, contract.ErrUnavailable
		}
		if string(parent.UID) != r.state.Parent.UID || parent.DeletionTimestamp != nil {
			return nil, contract.ErrConflict
		}
		if contract.TerminalPhase(status.Phase) && r.state.Final != nil {
			if !r.state.ResultPublished || parent.Status.ResultRef == nil || !parent.Status.ResultRef.Available {
				return nil, contract.ErrUnavailable
			}
			result, err := s.config.Results.GetResult(ctx, namespace, parent.Name)
			if err != nil {
				return nil, contract.ErrUnavailable
			}
			if pv.Digest(result) != r.state.Result.Digest {
				return nil, contract.ErrConflict
			}
		}
		status.TaskName, status.TaskUID = parent.Name, string(parent.UID)
	}
	return &status, nil
}

func (s *Coordinator) Cancel(ctx context.Context, namespace, id string) (*contract.Status, error) {
	if namespace != s.config.Namespace {
		return nil, contract.ErrForbidden
	}
	if !validID(id) {
		return nil, contract.ErrInvalid
	}
	if s.config.BoundedExecution {
		before, err := s.load(ctx, id)
		if err != nil {
			return nil, err
		}
		if before.state.Authority.BoundedExecution && !contract.TerminalPhase(before.state.Status.Phase) && before.state.Final == nil {
			ctx = bindRunIdentity(ctx, runIdentity{namespace: before.state.Namespace, id: before.state.ID, uid: before.object.UID})
			if err := s.markParentCancelled(ctx, before); err != nil {
				return nil, err
			}
		}
	}
	r, err := s.update(ctx, id, func(r *run) error {
		if !contract.TerminalPhase(r.state.Status.Phase) && r.state.Final == nil {
			r.state.CancelRequested, r.state.StopReason = true, reasonCancelled
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	cancel := s.active[id]
	s.mu.Unlock()
	if cancel != nil && r.state.CancelRequested {
		cancel()
	}
	status := r.state.Status
	return &status, nil
}

func (s *Coordinator) Patch(ctx context.Context, namespace, id string) ([]byte, error) {
	if namespace != s.config.Namespace {
		return nil, contract.ErrForbidden
	}
	if !validID(id) {
		return nil, contract.ErrInvalid
	}
	r, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !r.state.Status.PatchReady || r.state.Candidate.empty() {
		return nil, contract.ErrNotFound
	}
	return s.readBlob(ctx, r, r.state.Candidate)
}

func (s *Coordinator) NeedLeaderElection() bool { return true }

func (s *Coordinator) Start(ctx context.Context) error {
	defer s.prepared.Store(false)
	if s.config.PrepareBackend != nil {
		if err := s.config.PrepareBackend(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	if err := s.CheckReady(ctx); err != nil {
		return err
	}
	s.prepared.Store(true)
	var workers sync.WaitGroup
	defer workers.Wait()
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		items, err := listPendingStates(ctx, s.config.Reader, s.config.Namespace)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
		} else {
			for index := range items {
				r, err := s.prepareListedRun(ctx, &items[index])
				if err != nil {
					continue
				}
				st := r.state
				if contract.TerminalPhase(st.Status.Phase) && st.Settled {
					continue
				}
				s.mu.Lock()
				limit := 4
				if st.Authority.BoundedExecution {
					limit = 1
				}
				if _, active := s.active[st.ID]; active || len(s.active) >= limit {
					s.mu.Unlock()
					continue
				}
				if err := s.acquireRunSlot(ctx, r); err != nil {
					s.mu.Unlock()
					continue
				}
				operation, cancel := context.WithCancel(ctx)
				owner := runIdentity{namespace: r.state.Namespace, id: st.ID, uid: r.object.UID}
				s.active[st.ID] = cancel
				s.mu.Unlock()
				workers.Go(func() {
					defer cancel()
					defer func() { s.mu.Lock(); delete(s.active, st.ID); s.mu.Unlock() }()
					s.drive(operation, ctx, owner)
				})
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func pending(err error) bool {
	return errors.Is(err, contract.ErrUnavailable) || errors.Is(err, contract.ErrConflict)
}
