package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/labels"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/source"
	"github.com/orka-agents/orka/internal/store"
)

type memoryResults struct {
	mu          sync.Mutex
	values      map[string][]byte
	reads       int
	readsByName map[string]int
	onRead      func()
	beforeSave  func(string, string, []byte) error
	afterSave   func(string, string, []byte) error
}

func (r *memoryResults) SaveResult(_ context.Context, namespace, name string, content []byte) error {
	r.mu.Lock()
	before, after := r.beforeSave, r.afterSave
	r.mu.Unlock()
	if before != nil {
		if err := before(namespace, name, content); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.values[namespace+"/"+name] = bytes.Clone(content)
	r.mu.Unlock()
	if after != nil {
		return after(namespace, name, content)
	}
	return nil
}

func (r *memoryResults) GetResult(_ context.Context, namespace, name string) ([]byte, error) {
	r.mu.Lock()
	r.reads++
	if r.readsByName == nil {
		r.readsByName = make(map[string]int)
	}
	r.readsByName[namespace+"/"+name]++
	content, found := r.values[namespace+"/"+name]
	var hook func()
	if found {
		hook, r.onRead = r.onRead, nil
	}
	content = bytes.Clone(content)
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	if !found {
		return nil, store.ErrNotFound
	}
	return content, nil
}

func (r *memoryResults) DeleteResult(_ context.Context, namespace, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, namespace+"/"+name)
	return nil
}

type fixtureSource struct {
	target  source.Target
	fail    bool
	content string
}

func (s fixtureSource) text() string {
	if s.content != "" {
		return s.content
	}
	return "hello old\n"
}

func (s fixtureSource) Resolve(context.Context, string, string) (source.Target, error) {
	if s.fail {
		return source.Target{}, errors.New("sensitive source detail")
	}
	return s.target, nil
}

func (s fixtureSource) Inventory(context.Context, source.Target) ([]source.Entry, error) {
	return []source.Entry{{Path: "README", Mode: "100644", BlobSHA: strings.Repeat("c", 40), Size: int64(len(s.text()))}}, nil
}

func (s fixtureSource) Packet(_ context.Context, target source.Target, paths []string) (source.Packet, error) {
	if len(paths) != 1 || paths[0] != "README" {
		return source.Packet{}, errors.New("unexpected selected path")
	}
	return source.Packet{Version: 1, Target: target, Files: []source.PacketFile{{
		Path: "README", Content: s.text(), SHA256: strings.TrimPrefix(pv.Digest([]byte(s.text())), "sha256:"),
		BlobSHA: strings.Repeat("c", 40),
	}}}, nil
}

type fixtureBackend struct {
	t                    *testing.T
	mu                   sync.Mutex
	requests             []contract.ExecutionRequest
	records              map[string]*pv.Record
	receipts             map[string]contract.ExecutionReceipt
	resumes              int
	cancels              int
	original             string
	candidate            string
	mutate               func(*pv.Record)
	block                atomic.Bool
	configurationVersion atomic.Int64
	lostNoUID            bool
	retryAfterReceipt    bool
	deadlineAfterReceipt bool
	started              chan struct{}
	startedOnce          sync.Once
	startCalls           atomic.Int32
	validateRequest      func(contract.ExecutionRequest) error
	rejectStart          error
	afterReceiptError    error
	resumeError          error
	beforeStart          func(context.Context, contract.ExecutionRequest) error
}

func (b *fixtureBackend) ConfigurationDigest() string {
	return pv.Digest(fmt.Appendf(nil, "test backend configuration %d", b.configurationVersion.Load()))
}

func (b *fixtureBackend) EvidenceScope() contract.EvidenceScope { return contract.SourceHarness }

type fixtureRetryableError struct{}

func (fixtureRetryableError) Error() string   { return "transient execution read failure" }
func (fixtureRetryableError) Retryable() bool { return true }

type fixtureNoEffectsError struct{ error }

func (fixtureNoEffectsError) NoEffects() bool { return true }

func (b *fixtureBackend) RequestDigest(request contract.ExecutionRequest) (string, error) {
	if b.validateRequest != nil {
		if err := b.validateRequest(request); err != nil {
			return "", err
		}
	}
	content, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	return pv.Digest(content), nil
}

func (b *fixtureBackend) Start(ctx context.Context, request contract.ExecutionRequest, save func(contract.ExecutionReceipt) error) (*pv.Record, error) {
	b.startCalls.Add(1)
	if _, err := b.RequestDigest(request); err != nil {
		return nil, fixtureNoEffectsError{err}
	}
	if b.beforeStart != nil {
		if err := b.beforeStart(ctx, request); err != nil {
			return nil, err
		}
	}
	if b.rejectStart != nil {
		return nil, b.rejectStart
	}
	b.mu.Lock()
	b.requests = append(b.requests, request)
	if b.lostNoUID {
		b.mu.Unlock()
		return nil, errors.New("lost acknowledgement")
	}
	content, err := json.Marshal(request)
	require.NoError(b.t, err)
	receipt := contract.ExecutionReceipt{
		Namespace: request.Namespace, TaskName: request.ID + "-prepare",
		TaskUID: request.ID + "-prepare-uid", InputDigest: pv.Digest(content),
	}
	record := executionRecord(b.t, request, b.original, b.candidate)
	if b.mutate != nil {
		b.mutate(record)
	}
	b.records[request.ID], b.receipts[request.ID] = record, receipt
	b.mu.Unlock()
	if err := save(receipt); err != nil {
		return nil, err
	}
	b.startedOnce.Do(func() { close(b.started) })
	if b.afterReceiptError != nil {
		return nil, b.afterReceiptError
	}
	if b.retryAfterReceipt {
		return nil, fixtureRetryableError{}
	}
	if b.deadlineAfterReceipt {
		return nil, fmt.Errorf("%w: %w", contract.ErrUnavailable, context.DeadlineExceeded)
	}
	if b.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return record, nil
}

func (b *fixtureBackend) Recover(_ context.Context, request contract.ExecutionRequest) (contract.ExecutionRecovery, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	receipt, ok := b.receipts[request.ID]
	if !ok {
		return contract.ExecutionRecovery{}, contract.ErrNotFound
	}
	return contract.ExecutionRecovery{Receipt: &receipt}, nil
}

func (b *fixtureBackend) Resume(_ context.Context, request contract.ExecutionRequest, receipt contract.ExecutionReceipt) (*pv.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resumes++
	if b.resumeError != nil {
		return nil, b.resumeError
	}
	if receipt != b.receipts[request.ID] {
		return nil, contract.ErrConflict
	}
	return b.records[request.ID], nil
}

func (b *fixtureBackend) Cancel(_ context.Context, receipt contract.ExecutionReceipt) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancels++
	for _, saved := range b.receipts {
		if saved == receipt {
			return nil
		}
	}
	return contract.ErrConflict
}

func executionRecord(t *testing.T, request contract.ExecutionRequest, original, candidate string) *pv.Record {
	t.Helper()
	manifest := pv.Manifest{
		Version: pv.SchemaVersion, Action: request.Action, Problem: request.Problem,
		Scope: request.Checks.Scope, Gaps: request.Checks.Gaps,
		Sources: pv.Sources{Repository: request.Source.Repository, Original: pv.SourceIdentity{
			Commit: request.Source.Commit, Tree: request.Source.Tree, ArchiveDigest: pv.Digest([]byte("original archive")),
		}, Paths: slices.Clone(request.Source.Paths)},
		Environment: pv.Environment{
			Image:   "registry.example/tool@sha256:" + strings.Repeat("1", 64),
			ImageID: "sha256:" + strings.Repeat("2", 64), Platform: "linux/amd64", Profile: pv.LocalServices,
			Dependencies: map[string]string{"orka.kubernetes.policy": pv.KubernetesPolicyVersion},
			Requirements: request.Checks.Requirements, Services: request.Checks.Services,
		},
		Files: request.Checks.Files, Checks: request.Checks.Checks, DeclaredChanges: request.DeclaredChanges,
	}
	if request.EvidenceScope != "" {
		manifest.Environment.Dependencies[contract.EvidenceScopeKey] = string(request.EvidenceScope)
	}
	var err error
	manifest.ReportDigest, err = pv.StableReportDigest(manifest.Problem, manifest.Scope)
	require.NoError(t, err)
	patchedTask := ""
	if request.Action == pv.VerifyPatch {
		manifest.Sources.Patched = pv.SourceIdentity{Tree: strings.Repeat("d", 40), ArchiveDigest: pv.Digest([]byte("candidate archive"))}
		manifest.Sources.PatchDigest = pv.Digest([]byte(request.Patch))
		manifest.Sources.DiffDigest = pv.Digest([]byte("source difference"))
		patchedTask = "candidate-task-uid"
	}
	binding, err := pv.NewRunBinding(manifest, request.ID, "original-task-uid", patchedTask)
	require.NoError(t, err)
	record := &pv.Record{Manifest: manifest, Binding: binding, State: pv.RunFinalized}
	observations := make([]pv.Observation, 0, len(pv.ActionSides(request.Action))*len(manifest.Checks))
	for _, side := range pv.ActionSides(request.Action) {
		for _, check := range manifest.Checks {
			expected := check.Healthy
			if side == pv.Original && check.Kind == pv.Reproduction && original != "not-reproduced" {
				expected = check.Failure
			}
			if side == pv.Patched && ((check.Kind == pv.Reproduction && candidate == "not-fixed") ||
				(check.Kind == pv.Normal && candidate == "regression")) {
				expected = check.Failure
			}
			taskUID, tree := binding.OriginalTaskID, manifest.Sources.Original.Tree
			if side == pv.Patched {
				taskUID, tree = binding.PatchedTaskID, manifest.Sources.Patched.Tree
			}
			exit := 0
			observation := pv.Observation{
				RunID: binding.RunID, AttemptID: binding.AttemptID, TaskID: taskUID, ManifestDigest: binding.ManifestDigest,
				Side: side, CheckID: check.ID, SourceTree: tree, ImageID: manifest.Environment.ImageID,
				ContainerID: "container-" + side + "-" + check.ID, JobUID: "job-" + side + "-" + check.ID,
				PodUID: "pod-" + side + "-" + check.ID, Origin: "runner",
				StartedAt: time.Unix(1000, 0).UTC(), FinishedAt: time.Unix(1001, 0).UTC(),
				Executed: true, HTTPCompleted: true, ExitCode: &exit,
				StdoutDigest: pv.Digest([]byte(expected.Stdout)), StdoutBytes: len(expected.Stdout),
				StderrDigest: pv.Digest(nil),
			}
			if side == pv.Original && original == "setup-failed" {
				observation.Executed, observation.HTTPCompleted, observation.SetupError = false, false, "setup failed"
			}
			encoded, err := json.Marshal(observation)
			require.NoError(t, err)
			record.Evidence = append(record.Evidence, pv.EvidenceRecord{Observation: observation, Digest: pv.Digest(encoded)})
			observations = append(observations, observation)
		}
	}
	record.Assessment = pv.Evaluate(manifest, binding, observations)
	content, err := json.Marshal(record.Assessment)
	require.NoError(t, err)
	record.Seal = &pv.Seal{Content: content, Digest: pv.Digest(content), Assessment: record.Assessment}
	return record
}

type serviceFixture struct {
	t               *testing.T
	service         *Coordinator
	client          client.Client
	results         *memoryResults
	backend         *fixtureBackend
	mu              sync.Mutex
	roles           []string
	prompts         map[string]string
	responses       map[string]string
	createHook      func(client.Object) error
	afterCreateHook func(client.Object) error
	jobImage        string
	skipJobs        bool
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	var sequence atomic.Int64
	fixture := &serviceFixture{
		t: t, results: &memoryResults{values: make(map[string][]byte)}, prompts: make(map[string]string),
		responses: make(map[string]string),
		backend: &fixtureBackend{t: t, records: make(map[string]*pv.Record),
			receipts: make(map[string]contract.ExecutionReceipt), started: make(chan struct{})},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture-agent", Namespace: "private", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{}, Model: &corev1alpha1.ModelConfig{
			Provider: "openai", Name: "fixture",
		}},
	}
	fixture.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).
		WithGlobalResourceVersionCounter().
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1.Pod{}, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
				if object.GetUID() == "" {
					object.SetUID(types.UID(fmt.Sprintf("uid-%d", sequence.Add(1))))
				}
				if object.GetCreationTimestamp().Time.IsZero() {
					object.SetCreationTimestamp(metav1.Now())
				}
				if fixture.createHook != nil {
					if err := fixture.createHook(object); err != nil {
						return err
					}
				}
				if err := c.Create(ctx, object, opts...); err != nil {
					return err
				}
				if fixture.afterCreateHook != nil {
					return fixture.afterCreateHook(object)
				}
				return nil
			},
		}).Build()
	var err error
	fixture.service, err = New(Config{
		Client: fixture.client, Reader: fixture.client, Namespace: "private", AgentName: "fixture-agent",
		AIWorkerImage: "registry.example/ai-worker@sha256:" + strings.Repeat("3", 64),
		Results:       fixture.results, Backend: fixture.backend, PollInterval: time.Millisecond, RunTimeout: 30 * time.Second,
		Source: fixtureSource{target: source.Target{
			Repository: source.Repository{URL: "https://github.com/example/project", Owner: "example", Name: "project", DefaultBranch: "main"},
			Ref:        "main", Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40),
		}},
	})
	require.NoError(t, err)
	require.NoError(t, fixture.service.CheckReady(t.Context()))
	fixture.responses["select"] = `{"files":["README"]}`
	fixture.responses["checks"] = marshalTest(t, proposedChecks(t))
	fixture.responses["reproduce"] = `{"reproduced":true,"summary":"original observation matches the report","limitations":[]}`
	fixture.responses["generate"] = marshalTest(t, remediation.PatchProposal{
		Summary: "change the greeting", Edits: []remediation.SourceEdit{{Path: "README", Old: "hello old\n", New: "hello new\n"}},
		DeclaredChanges: []pv.DeclaredChange{{Kind: "source", Paths: []string{"README"}, Description: "change greeting"}},
	})
	fixture.responses["verify-0"] = `{"approved":true,"summary":"original and candidate observations compared","limitations":[]}`
	fixture.responses["verify-1"] = fixture.responses["verify-0"]
	return fixture
}

func marshalTest(t *testing.T, value any) string {
	t.Helper()
	content, err := json.Marshal(value)
	require.NoError(t, err)
	return string(content)
}

func proposedChecks(t *testing.T) remediation.CheckProposal {
	t.Helper()
	healthy, err := pv.HTTPExpectation(200, "healthy")
	require.NoError(t, err)
	failure, err := pv.HTTPExpectation(200, "reported behavior")
	require.NoError(t, err)
	proposal := remediation.CheckProposal{
		Scope: []string{"greeting and ordinary behavior"}, Gaps: []string{},
		Files: []remediation.ProposedFile{{Path: "server.py", Content: "#!/usr/bin/env python3\nprint('synthetic test source')\n", Executable: true}},
	}
	for _, kind := range []string{pv.Reproduction, pv.Normal} {
		proposal.Checks = append(proposal.Checks, pv.Check{
			ID: kind, Kind: kind, HTTP: &pv.HTTPCheck{Version: 1, ServerCommand: []string{"/checks/server.py"}, Path: "/" + kind},
			Healthy: healthy, Failure: failure, TimeoutSeconds: 1,
		})
	}
	return proposal
}

func (f *serviceFixture) request(mode contract.Mode) contract.Request {
	request := contract.Request{
		RequestID: "request-one", Mode: mode, Repository: "https://github.com/example/project", Ref: "main",
		Report: json.RawMessage(`{"title":"Greeting behavior","problem":"The greeting should use the new text."}`),
	}
	return request
}

func (f *serviceFixture) start() func() {
	f.t.Helper()
	ctx, cancel := context.WithCancel(f.t.Context())
	done, pumpDone := make(chan error, 1), make(chan struct{})
	go func() { done <- f.service.Start(ctx) }()
	go func() {
		defer close(pumpDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				f.completeTasks(ctx)
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			require.NoError(f.t, <-done)
			<-pumpDone
		})
	}
	f.t.Cleanup(stop)
	return stop
}

func (f *serviceFixture) completeTasks(ctx context.Context) {
	var tasks corev1alpha1.TaskList
	if f.client.List(ctx, &tasks, client.InNamespace("private")) != nil {
		return
	}
	for index := range tasks.Items {
		task := &tasks.Items[index]
		if task.Spec.Type != corev1alpha1.TaskTypeAI || task.Labels[runLabel] == "" {
			continue
		}
		_, handled, err := f.service.ReconcileRemediationTask(ctx, task)
		if handled || err != nil || task.Status.Phase == corev1alpha1.TaskPhaseSucceeded ||
			task.Status.Phase == corev1alpha1.TaskPhaseFailed || task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			continue
		}
		if task.Annotations[labels.AnnotationCancelRequested] != "" {
			task.Status.Phase = corev1alpha1.TaskPhaseCancelled
		} else {
			if !f.skipJobs {
				job := f.proposalJob(task)
				err := f.client.Create(ctx, job)
				if apierrors.IsAlreadyExists(err) {
					err = f.client.Get(ctx, client.ObjectKeyFromObject(job), job)
				}
				if err != nil {
					continue
				}
				task.Status.JobName, task.Status.JobUID = job.Name, string(job.UID)
				if f.service.config.BoundedExecution {
					task.Status.Phase = corev1alpha1.TaskPhaseRunning
					if f.client.Status().Update(ctx, task) != nil {
						continue
					}
					_, resultErr := f.results.GetResult(ctx, task.Namespace, task.Name)
					if errors.Is(resultErr, store.ErrNotFound) {
						if f.service.ClaimModelCall(ctx, contract.ModelCallClaim{
							Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID),
							JobUID: string(job.UID), PodUID: "fixture-pod-" + string(job.UID),
						}) != nil {
							continue
						}
					} else if resultErr != nil {
						continue
					}
				}
			}
			role := task.Labels[roleLabel]
			f.mu.Lock()
			if _, exists := f.prompts[role]; !exists {
				f.roles = append(f.roles, role)
			}
			f.prompts[role] = task.Spec.Prompt
			output := f.responses[role]
			f.mu.Unlock()
			if err := f.results.SaveResult(ctx, task.Namespace, task.Name, []byte(output)); err != nil {
				continue
			}
			task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
		}
		_ = f.client.Status().Update(ctx, task)
	}
}

func (f *serviceFixture) proposalJob(task *corev1alpha1.Task) *batchv1.Job {
	image := f.jobImage
	if image == "" {
		image = f.service.config.AIWorkerImage
	}
	controller := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: task.Name + "-job", Namespace: task.Namespace,
			Labels: map[string]string{labels.LabelTask: labels.SelectorValue(task.Name)},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: task.Name,
				UID: task.UID, Controller: &controller,
			}},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: image}}},
		}},
	}
	if f.service.config.BoundedExecution {
		job.Spec.BackoffLimit = new(int32)
		job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
		job.Spec.Template.Spec.Containers[0].Env = slices.Clone(task.Spec.Env)
	}
	return job
}

func (f *serviceFixture) awaitStatus(id string, predicate func(*contract.Status) bool) *contract.Status {
	f.t.Helper()
	var status *contract.Status
	require.EventuallyWithT(f.t, func(c *assert.CollectT) {
		var err error
		status, err = f.service.Get(f.t.Context(), "private", id)
		require.NoError(c, err)
		require.True(c, predicate(status), "status: %+v", status)
	}, 20*time.Second, 5*time.Millisecond)
	return status
}

func TestServiceStageOrderingAndIndependentValidator(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.start()
	request := fixture.request(contract.Generate)
	status, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	require.Equal(t, contract.StageReproduce, status.Stage)
	status = fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseSucceeded, status.Phase)
	require.Equal(t, "Verified", status.Outcome)
	require.True(t, status.PatchReady)
	patch, err := fixture.service.Patch(t.Context(), "private", request.RequestID)
	require.NoError(t, err)
	require.Contains(t, string(patch), "+hello new")
	fixture.mu.Lock()
	require.Equal(t, []string{"select", "checks", "reproduce", "generate", "verify-0"}, fixture.roles)
	require.Contains(t, fixture.prompts["reproduce"], `"executed":true`)
	require.Contains(t, fixture.prompts["verify-0"], `"side":"original"`)
	require.Contains(t, fixture.prompts["verify-0"], `"side":"patched"`)
	require.NotContains(t, fixture.prompts["verify-0"], "change the greeting")
	fixture.mu.Unlock()
	fixture.backend.mu.Lock()
	require.Len(t, fixture.backend.requests, 2)
	original, candidate := fixture.backend.requests[0], fixture.backend.requests[1]
	require.Equal(t, pv.ValidateReport, original.Action)
	require.Empty(t, original.Patch)
	require.Equal(t, pv.VerifyPatch, candidate.Action)
	require.Equal(t, original.Source, candidate.Source)
	require.Equal(t, original.Checks, candidate.Checks)
	require.Equal(t, original.ParentTaskUID, candidate.ParentTaskUID)
	fixture.backend.mu.Unlock()
	var tasks corev1alpha1.TaskList
	require.NoError(t, fixture.client.List(t.Context(), &tasks))
	for _, task := range tasks.Items {
		if IsParentTask(&task) {
			continue
		}
		require.Empty(t, aitools.Resolve(&task, nil))
		require.Nil(t, task.Spec.SessionRef)
		require.Nil(t, task.Spec.Workspace)
		require.Nil(t, task.Spec.SecretRef)
		require.Zero(t, task.Spec.RetryPolicy.MaxRetries)
		require.Equal(t, privateAgentEnvironment(), task.Spec.Env)
	}
}

func TestServiceReproductionGateAndOneCandidate(t *testing.T) {
	for _, scenario := range []struct {
		name, original, candidate, wantOutcome string
		wantFix                                bool
	}{
		{name: "false report", original: "not-reproduced", wantOutcome: "NotReproduced"},
		{name: "setup failure", original: "setup-failed", wantOutcome: "UnableToValidate"},
		{name: "bad candidate", candidate: "not-fixed", wantOutcome: "Unverified", wantFix: true},
		{name: "normal regression", candidate: "regression", wantOutcome: "Unverified", wantFix: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			fixture.backend.original, fixture.backend.candidate = scenario.original, scenario.candidate
			fixture.start()
			request := fixture.request(contract.Generate)
			_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
			require.Equal(t, scenario.wantOutcome, status.Outcome)
			require.Equal(t, scenario.wantFix, status.PatchReady)
			fixture.mu.Lock()
			require.Equal(t, scenario.wantFix, slices.Contains(fixture.roles, "generate"))
			generations := 0
			for _, role := range fixture.roles {
				if role == "generate" {
					generations++
				}
			}
			require.LessOrEqual(t, generations, 1)
			fixture.mu.Unlock()
			fixture.backend.mu.Lock()
			if scenario.wantFix {
				require.Len(t, fixture.backend.requests, 2)
			} else {
				require.Len(t, fixture.backend.requests, 1)
			}
			fixture.backend.mu.Unlock()
		})
	}
}

func TestServiceValidateDoesNotGenerateCandidate(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.start()
	request := fixture.request(contract.Validate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseSucceeded, status.Phase)
	require.Equal(t, contract.OutcomeReproduced, status.Outcome)
	require.False(t, status.PatchReady)
	fixture.mu.Lock()
	require.NotContains(t, fixture.roles, "generate")
	fixture.mu.Unlock()
}

func TestServiceChangedChecksRerunBothSidesWithoutAnotherCandidate(t *testing.T) {
	fixture := newServiceFixture(t)
	revised := proposedChecks(t)
	revised.Checks[1].HTTP.Path = "/additional-normal-case"
	approved := false
	fixture.responses["verify-0"] = marshalTest(t, remediation.CandidateReview{
		Approved: &approved, Summary: "exercise another ordinary behavior", RevisedChecks: &revised,
	})
	fixture.start()
	request := fixture.request(contract.Generate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, "Verified", status.Outcome)
	fixture.backend.mu.Lock()
	require.Len(t, fixture.backend.requests, 3)
	first, second := fixture.backend.requests[1], fixture.backend.requests[2]
	require.Equal(t, pv.VerifyPatch, first.Action)
	require.Equal(t, pv.VerifyPatch, second.Action)
	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, first.Source, second.Source)
	require.Equal(t, first.Patch, second.Patch)
	require.NotEqual(t, first.Checks, second.Checks)
	record := fixture.backend.records[second.ID]
	require.Len(t, record.Evidence, 4)
	fixture.backend.mu.Unlock()
	fixture.mu.Lock()
	require.Equal(t, []string{"select", "checks", "reproduce", "generate", "verify-0", "verify-1"}, fixture.roles)
	fixture.mu.Unlock()
}

func TestServiceModelApprovalCannotOverrideTamperedEvidence(t *testing.T) {
	for _, scenario := range []string{"digest", "missing", "source", "summary", "normal-skipped"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newServiceFixture(t)
			fixture.backend.mutate = func(record *pv.Record) {
				switch scenario {
				case "digest":
					record.Evidence[0].Digest = pv.Digest([]byte("different"))
				case "missing":
					record.Evidence = record.Evidence[1:]
				case "source":
					record.Manifest.Sources.Original.Commit = strings.Repeat("e", 40)
				case "summary":
					record.Assessment.Reason = "claimed success without matching observations"
				case "normal-skipped":
					record.Evidence[1].Observation.Skipped = true
				}
			}
			fixture.start()
			request := fixture.request(contract.Generate)
			_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
			require.Equal(t, contract.PhaseFailed, status.Phase)
			require.False(t, status.PatchReady)
			require.Equal(t, "original-evidence-invalid", status.Reason)
			fixture.mu.Lock()
			require.NotContains(t, fixture.roles, "generate")
			fixture.mu.Unlock()
		})
	}
}

func TestServiceInvalidModelOutputAndIndependentRejection(t *testing.T) {
	for _, scenario := range []struct {
		role, output, reason string
		patch                bool
	}{
		{role: "checks", output: "I ran the tests and they passed.", reason: "checks-invalid"},
		{role: "reproduce", output: `{"reproduced":true,"Reproduced":false,"summary":"ambiguous","limitations":[]}`,
			reason: "agent-output-invalid"},
		{role: "generate", output: `{"summary":"no actual candidate","patch":"","declaredChanges":[],"limitations":[]}`,
			reason: "candidate-invalid"},
		{role: "verify-0", output: `{"approved":false,"summary":"checks do not cover the claimed behavior","limitations":[]}`,
			reason: "validation-rejected", patch: true},
		{role: "verify-0", output: `{"summary":"looks good","limitations":[]}`,
			reason: "validation-invalid", patch: true},
	} {
		t.Run(scenario.role+"-"+scenario.reason, func(t *testing.T) {
			fixture := newServiceFixture(t)
			fixture.responses[scenario.role] = scenario.output
			fixture.start()
			request := fixture.request(contract.Generate)
			_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
			require.Equal(t, contract.PhaseFailed, status.Phase)
			require.Equal(t, scenario.reason, status.Reason)
			require.Equal(t, scenario.patch, status.PatchReady)
			require.NotContains(t, status.Reason, "behavior")
		})
	}
}

func TestServiceResumeRetainsExecutionAndTaskIdentity(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.backend.block.Store(true)
	stop := fixture.start()
	request := fixture.request(contract.Generate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	select {
	case <-fixture.backend.started:
	case <-time.After(5 * time.Second):
		t.Fatal("execution did not persist its receipt")
	}
	stop()
	before, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	receipt := *before.state.Executions["original"].Receipt
	parentUID := before.state.Parent.UID
	require.Empty(t, before.state.Executions["original"].Record.Digest)
	fixture.backend.block.Store(false)
	fixture.service, err = New(fixture.service.config)
	require.NoError(t, err)
	fixture.start()
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, "Verified", status.Outcome)
	after, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Equal(t, receipt, *after.state.Executions["original"].Receipt)
	require.Equal(t, parentUID, after.state.Parent.UID)
	fixture.backend.mu.Lock()
	require.Len(t, fixture.backend.requests, 2)
	require.Equal(t, 1, fixture.backend.resumes)
	require.Zero(t, fixture.backend.cancels)
	fixture.backend.mu.Unlock()
}

func TestServiceLostAcknowledgementNeverCreatesReplacement(t *testing.T) {
	t.Run("observed exact Task recovers", func(t *testing.T) {
		fixture := newServiceFixture(t)
		var creates atomic.Int32
		fixture.afterCreateHook = func(object client.Object) error {
			if object.GetLabels()[roleLabel] == "checks" {
				creates.Add(1)
				return errors.New("acknowledgement lost")
			}
			return nil
		}
		fixture.start()
		request := fixture.request(contract.Validate)
		_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
		require.NoError(t, err)
		status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
		require.Equal(t, "Reproduced", status.Outcome)
		require.EqualValues(t, 1, creates.Load())
	})
	t.Run("unobserved Task is not recreated", func(t *testing.T) {
		fixture := newServiceFixture(t)
		var creates atomic.Int32
		fixture.createHook = func(object client.Object) error {
			if object.GetLabels()[roleLabel] == "checks" {
				creates.Add(1)
				return errors.New("acknowledgement lost")
			}
			return nil
		}
		fixture.start()
		request := fixture.request(contract.Generate)
		_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
		require.NoError(t, err)
		status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
		require.Equal(t, contract.PhaseNeedsInput, status.Phase)
		require.False(t, status.PatchReady)
		require.EqualValues(t, 1, creates.Load())
		require.ErrorIs(t, EnsureNoActiveRuns(t.Context(), fixture.client, "private"), contract.ErrConflict)
	})
	t.Run("unobserved execution is not restarted", func(t *testing.T) {
		fixture := newServiceFixture(t)
		fixture.backend.lostNoUID = true
		fixture.start()
		request := fixture.request(contract.Generate)
		_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
		require.NoError(t, err)
		status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
		require.Equal(t, contract.PhaseNeedsInput, status.Phase)
		require.ErrorIs(t, EnsureNoActiveRuns(t.Context(), fixture.client, "private"), contract.ErrConflict)
		fixture.backend.mu.Lock()
		require.Len(t, fixture.backend.requests, 1)
		require.Zero(t, fixture.backend.resumes)
		fixture.backend.mu.Unlock()
	})
}

func TestServiceCancellationTimeoutAndDisablement(t *testing.T) {
	for _, timedOut := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout-%t", timedOut), func(t *testing.T) {
			fixture := newServiceFixture(t)
			fixture.backend.block.Store(true)
			if timedOut {
				fixture.service.config.RunTimeout = 3 * time.Second
			}
			require.NoError(t, EnsureNoActiveRuns(t.Context(), fixture.client, "private"))
			fixture.start()
			request := fixture.request(contract.Generate)
			_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			select {
			case <-fixture.backend.started:
			case <-time.After(5 * time.Second):
				t.Fatal("execution did not start")
			}
			require.ErrorIs(t, EnsureNoActiveRuns(t.Context(), fixture.client, "private"), contract.ErrConflict)
			if !timedOut {
				_, err := fixture.service.Cancel(t.Context(), "private", request.RequestID)
				require.NoError(t, err)
			}
			status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
			expected := contract.PhaseCancelled
			if timedOut {
				expected = contract.PhaseTimedOut
			}
			require.Equal(t, expected, status.Phase)
			require.False(t, status.PatchReady)
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				require.NoError(c, EnsureNoActiveRuns(t.Context(), fixture.client, "private"))
			}, 5*time.Second, 5*time.Millisecond)
			fixture.backend.mu.Lock()
			require.Positive(t, fixture.backend.cancels)
			require.Len(t, fixture.backend.requests, 1)
			fixture.backend.mu.Unlock()
		})
	}
}

func TestServicePrivateAccessExactInputAndConfigurationDrift(t *testing.T) {
	fixture := newServiceFixture(t)
	request := fixture.request(contract.Generate)
	status, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	require.Equal(t, request.RequestID, status.ID)
	_, err = fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	changed := request
	changed.Report = append([]byte(" "), changed.Report...)
	_, err = fixture.service.Submit(t.Context(), "private", "subject", changed)
	require.ErrorIs(t, err, contract.ErrConflict)
	_, err = fixture.service.Submit(t.Context(), "private", "other-subject", request)
	require.ErrorIs(t, err, contract.ErrConflict)
	_, err = fixture.service.Get(t.Context(), "other-namespace", request.RequestID)
	require.ErrorIs(t, err, contract.ErrForbidden)
	_, err = fixture.service.Cancel(t.Context(), "other-namespace", request.RequestID)
	require.ErrorIs(t, err, contract.ErrForbidden)
	_, err = fixture.service.Patch(t.Context(), "other-namespace", request.RequestID)
	require.ErrorIs(t, err, contract.ErrForbidden)
	fixture.results.mu.Lock()
	require.Zero(t, fixture.results.reads)
	fixture.results.mu.Unlock()
	fixture.backend.configurationVersion.Store(1)
	require.ErrorIs(t, fixture.service.CheckReady(t.Context()), contract.ErrConflict)
	fixture.backend.configurationVersion.Store(0)
	require.NoError(t, fixture.service.CheckReady(t.Context()))
	agent := new(corev1alpha1.Agent)
	require.NoError(t, fixture.client.Get(t.Context(), client.ObjectKey{Namespace: "private", Name: "fixture-agent"}, agent))
	agent.Spec.Model.Name = "changed"
	require.NoError(t, fixture.client.Update(t.Context(), agent))
	require.ErrorIs(t, fixture.service.CheckReady(t.Context()), contract.ErrConflict)
	content := marshalTest(t, status)
	require.NotContains(t, content, "Greeting")
	require.NotContains(t, content, "subject")
	require.NotContains(t, content, "report")
}

func TestServiceResultReadIsTaskUIDFenced(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.results.onRead = func() {
		var tasks corev1alpha1.TaskList
		require.NoError(t, fixture.client.List(t.Context(), &tasks, client.MatchingLabels{roleLabel: "select"}))
		require.Len(t, tasks.Items, 1)
		task := tasks.Items[0].DeepCopy()
		require.NoError(t, fixture.client.Delete(t.Context(), task))
		task.UID, task.ResourceVersion = "", ""
		require.NoError(t, fixture.client.Create(t.Context(), task))
	}
	fixture.start()
	request := fixture.request(contract.Generate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseNeedsInput, status.Phase)
	require.Equal(t, "identity-conflict", status.Reason)
	require.False(t, status.PatchReady)
	require.ErrorIs(t, EnsureNoActiveRuns(t.Context(), fixture.client, "private"), contract.ErrConflict)
}

func TestServiceRejectsStaleNameOnlyResultBeforeTaskCreation(t *testing.T) {
	fixture := newServiceFixture(t)
	request := fixture.request(contract.Validate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	require.NoError(t, fixture.results.SaveResult(t.Context(), "private", stateName(request.RequestID)+"-select",
		[]byte(`{"files":["README"]}`)))
	fixture.start()
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseNeedsInput, status.Phase)
	var tasks corev1alpha1.TaskList
	require.NoError(t, fixture.client.List(t.Context(), &tasks, client.MatchingLabels{roleLabel: "select"}))
	require.Empty(t, tasks.Items)
}

func TestServiceTransientExecutionFailureResumesExactReceipt(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.backend.retryAfterReceipt = true
	fixture.start()
	request := fixture.request(contract.Validate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, "Reproduced", status.Outcome)
	fixture.backend.mu.Lock()
	require.Len(t, fixture.backend.requests, 1)
	require.Equal(t, 1, fixture.backend.resumes)
	fixture.backend.mu.Unlock()
}

func TestServiceCorruptedPrivateStatusDoesNotLeak(t *testing.T) {
	fixture := newServiceFixture(t)
	request := fixture.request(contract.Validate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	r.state.Status.Reason = "private-report-content"
	require.NoError(t, fixture.service.save(t.Context(), r))
	_, err = fixture.service.Get(t.Context(), "private", request.RequestID)
	require.ErrorIs(t, err, contract.ErrConflict)
	require.NotContains(t, err.Error(), "private-report-content")
	require.ErrorIs(t, EnsureNoActiveRuns(t.Context(), fixture.client, "private"), contract.ErrConflict)
}

func TestServiceLateTaskAcknowledgementCannotStartModelWork(t *testing.T) {
	fixture := newServiceFixture(t)
	late := make(chan *corev1alpha1.Task, 1)
	var creates atomic.Int32
	fixture.createHook = func(object client.Object) error {
		if task, ok := object.(*corev1alpha1.Task); ok && task.Labels[roleLabel] == "checks" && creates.Add(1) == 1 {
			late <- task.DeepCopy()
			return errors.New("create outcome not yet observable")
		}
		return nil
	}
	fixture.start()
	request := fixture.request(contract.Generate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseNeedsInput, status.Phase)
	require.ErrorIs(t, EnsureNoActiveRuns(t.Context(), fixture.client, "private"), contract.ErrConflict)
	task := <-late
	require.NoError(t, fixture.client.Create(t.Context(), task))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, EnsureNoActiveRuns(t.Context(), fixture.client, "private"))
	}, 10*time.Second, 5*time.Millisecond)
	fixture.mu.Lock()
	require.NotContains(t, fixture.roles, "checks")
	require.NotContains(t, fixture.roles, "generate")
	fixture.mu.Unlock()
	current := new(corev1alpha1.Task)
	require.NoError(t, fixture.client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, task.UID, current.UID)
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
}

func TestServiceBackendDeadlineSettlesWithoutResubmission(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.backend.deadlineAfterReceipt = true
	fixture.start()
	request := fixture.request(contract.Generate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseTimedOut, status.Phase)
	require.Equal(t, contract.OutcomeTimedOut, status.Outcome)
	require.False(t, status.PatchReady)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, EnsureNoActiveRuns(t.Context(), fixture.client, "private"))
	}, 10*time.Second, 5*time.Millisecond)
	fixture.backend.mu.Lock()
	require.Len(t, fixture.backend.requests, 1)
	require.Zero(t, fixture.backend.resumes)
	require.Positive(t, fixture.backend.cancels)
	fixture.backend.mu.Unlock()
}

func TestServiceEncodedReportInputBudget(t *testing.T) {
	fixture := newServiceFixture(t)
	request := fixture.request(contract.Generate)
	prefix, suffix := `{"title":"Greeting behavior","problem":"`, `"}`
	request.Report = json.RawMessage(prefix + strings.Repeat("x", MaxReportBytes-len(prefix)-len(suffix)) + suffix)
	require.Len(t, request.Report, MaxReportBytes)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Less(t, len(r.object.Data["input"])+len(r.object.Data["state"]), 1<<20)
	request.RequestID = "request-too-large"
	request.Report = json.RawMessage(prefix + strings.Repeat("x", MaxReportBytes+1-len(prefix)-len(suffix)) + suffix)
	_, err = fixture.service.Submit(t.Context(), "private", "subject", request)
	require.ErrorIs(t, err, contract.ErrInvalid)
	_, err = fixture.service.Get(t.Context(), "private", request.RequestID)
	require.ErrorIs(t, err, contract.ErrNotFound)
}
