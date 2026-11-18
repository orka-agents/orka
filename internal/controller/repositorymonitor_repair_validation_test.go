/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	repairGateTestCommand       = "make test"
	repairValidationTestHeadSHA = "0123456789abcdef0123456789abcdef01234567"
)

var repairValidationTestDigest = "sha256:" + strings.Repeat("b", 64)

func repairValidationTestMonitor() *corev1alpha1.RepositoryMonitor {
	monitor := repositoryMonitorReviewIngestTestMonitor("repair-gate")
	monitor.Spec.Validation.Image = repositoryMonitorValidationTestImage
	monitor.Spec.Validation.RepairCommand = repairGateTestCommand
	return monitor
}

func repairValidationTestRepairTask(monitor *corev1alpha1.RepositoryMonitor) *corev1alpha1.Task {
	controller := true
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "repair-gate-pr-7-fix", Namespace: monitor.Namespace, UID: types.UID("repair-task-uid"),
			Labels: map[string]string{
				labels.LabelCreatedBy:         repositoryMonitorTaskCreatedBy,
				labels.LabelRepositoryMonitor: monitor.Name,
				labels.LabelGitHubNumber:      "7",
			},
			Annotations: map[string]string{
				labels.AnnotationRepositoryMonitorName:    monitor.Name,
				labels.AnnotationMonitorRunID:             "run-1",
				labels.AnnotationMonitorItemKind:          repositoryMonitorPullRequestKind,
				labels.AnnotationMonitorItemNumber:        "7",
				labels.AnnotationMonitorHeadSHA:           repairValidationTestHeadSHA,
				labels.AnnotationGitHubRepository:         "orka-agents/orka",
				repositoryMonitorIssueAnnotationCommandID: "cmd-1",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: repositoryMonitorResourceKind,
				Name: monitor.Name, UID: monitor.UID, Controller: &controller,
			}},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &corev1alpha1.AgentReference{Name: "repairer"},
			Prompt:   "repair",
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent:                     corev1alpha1.WorkspaceIntentWrite,
				GitRepo:                    "https://github.com/orka-agents/orka.git",
				Branch:                     "feature",
				Ref:                        repairValidationTestHeadSHA,
				ExpectedRemoteSHA:          repairValidationTestHeadSHA,
				ReadCredentialRef:          &corev1alpha1.WorkspaceCredentialReference{Name: "read-token"},
				PublicationCredentialRef:   &corev1alpha1.WorkspaceCredentialReference{Name: "write-token"},
				PushBranch:                 "feature",
				DenyRepositoryControlPaths: true,
			},
		},
	}
}

func repairValidationTestInput(monitor *corev1alpha1.RepositoryMonitor, repair *corev1alpha1.Task) tools.RepositoryRepairValidationInput {
	return tools.RepositoryRepairValidationInput{
		Monitor: monitor, RepairTask: repair, Image: monitor.Spec.Validation.Image,
		Command: monitor.Spec.Validation.RepairCommand, DeltaDigest: repairValidationTestDigest,
	}
}

type repairValidationFixture struct {
	monitor    *corev1alpha1.RepositoryMonitor
	repair     *corev1alpha1.Task
	validation *corev1alpha1.Task
	command    *corev1.Secret
	input      tools.RepositoryRepairValidationInput
}

func newRepairValidationFixture(t *testing.T) repairValidationFixture {
	t.Helper()
	monitor := repairValidationTestMonitor()
	repair := repairValidationTestRepairTask(monitor)
	input := repairValidationTestInput(monitor, repair)
	validation, err := tools.BuildRepositoryRepairValidationTask(input)
	if err != nil {
		t.Fatalf("BuildRepositoryRepairValidationTask() error = %v", err)
	}
	validation.UID = types.UID("repair-validation-task-uid")
	return repairValidationFixture{
		monitor: monitor, repair: repair, validation: validation, input: input,
		command: tools.BuildRepositoryRepairValidationCommandSecret(repair, validation, repairGateTestCommand),
	}
}

func TestRepairValidationTaskIsAnExactReadOnlyCheckoutOfTheRepairHead(t *testing.T) {
	f := newRepairValidationFixture(t)
	task := f.validation
	if !isRepositoryMonitorValidationTask(task) || !isRepositoryMonitorRepairValidationTask(task) {
		t.Fatalf("repair validation Task is not classified as a repair validation Task: %#v", task.ObjectMeta)
	}
	if !strings.HasSuffix(task.Name, "-validation") || len(task.Name) > 63-len("-patch") {
		t.Fatalf("validation Task name %q must keep the -validation suffix and leave room for its capability Secret", task.Name)
	}
	workspace := task.Spec.Workspace
	if workspace.Intent != corev1alpha1.WorkspaceIntentRead || workspace.Ref != repairValidationTestHeadSHA ||
		workspace.PublicationCredentialRef != nil || workspace.PushBranch != "" || workspace.ExpectedRemoteSHA != "" ||
		workspace.ReadCredentialRef == nil || workspace.ReadCredentialRef.Name != "read-token" {
		t.Fatalf("validation workspace = %#v, want read-only exact head with only the read credential", workspace)
	}
	if task.Annotations[labels.AnnotationRepositoryValidationPatchDigest] != repairValidationTestDigest ||
		task.Annotations[labels.AnnotationRepositoryValidationDenyControlPaths] != booleanTrueValue ||
		task.Annotations[labels.AnnotationRepositoryValidationCommandDigest] != tools.RepositoryValidationCommandDigest(repairGateTestCommand) {
		t.Fatalf("validation annotations = %#v", task.Annotations)
	}
	other := f.input
	other.DeltaDigest = "sha256:" + strings.Repeat("c", 64)
	if tools.RepositoryRepairValidationTaskName(f.repair, other.DeltaDigest) == task.Name {
		t.Fatal("a different patch must get a different validation Task")
	}
	if tools.RepositoryRepairValidationTaskMatches(task, other) {
		t.Fatal("validation Task matched a different patch digest")
	}
}

func TestRepairValidationJobAppliesPatchAfterCheckoutAndBeforeNetworkGate(t *testing.T) {
	f := newRepairValidationFixture(t)
	builder := setupJobBuilder()
	builder.ControllerURL = "http://orka-api.orka-system.svc:8080"
	job, err := builder.BuildWithOptions(context.Background(), f.validation, nil, nil, JobBuildOptions{RepositoryMonitorValidation: true})
	if err != nil {
		t.Fatalf("BuildWithOptions() error = %v", err)
	}
	names := make([]string, 0, len(job.Spec.Template.Spec.InitContainers))
	var patch *corev1.Container
	for i := range job.Spec.Template.Spec.InitContainers {
		container := &job.Spec.Template.Spec.InitContainers[i]
		names = append(names, container.Name)
		if container.Name == repositoryMonitorRepairValidationPatchContainer {
			patch = container
		}
	}
	want := []string{
		workspacePreparationInitContainerName, repositoryMonitorRepairValidationPatchContainer,
		repositoryMonitorValidationCommandContainer, repositoryMonitorValidationNetworkProbeContainer,
		repositoryMonitorValidationNetworkGateContainer,
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("init containers = %v, want %v", names, want)
	}
	wantArgs := []string{
		repositoryMonitorRepairValidationPatchWorkerMode, builder.ControllerURL,
		repositoryMonitorRepairValidationPatchMount + "/" + tools.RepositoryRepairValidationCapabilitySecretKey,
		workspaceRootPath, repairValidationTestDigest, ".", booleanTrueValue,
	}
	if strings.Join(patch.Args, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("patch args = %v, want %v", patch.Args, wantArgs)
	}
	if len(patch.Env) != 0 {
		t.Fatalf("patch container env = %#v, want none (no credentials)", patch.Env)
	}
	for _, mount := range patch.VolumeMounts {
		if mount.Name == "git-read-credentials" {
			t.Fatal("patch container must not mount the repository read credential")
		}
	}
	for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.Name == repositoryMonitorRepairValidationPatchVolume {
			t.Fatal("validation command container must not see the patch capability")
		}
		if mount.Name == taskWorkspaceVolume && !mount.ReadOnly {
			t.Fatal("validation command must see the patched checkout read-only")
		}
	}
	if patch.SecurityContext == nil || patch.SecurityContext.RunAsUser == nil ||
		*patch.SecurityContext.RunAsUser != *job.Spec.Template.Spec.SecurityContext.RunAsUser {
		t.Fatal("patch container must run as the validation Task's isolated UID")
	}

	builder.ControllerURL = ""
	if _, err := builder.BuildWithOptions(context.Background(), f.validation, nil, nil, JobBuildOptions{RepositoryMonitorValidation: true}); err == nil {
		t.Fatal("repair validation Job without a controller artifact endpoint must fail")
	}
}

func TestRepairValidationJobSurvivesAPIDefaultingComparison(t *testing.T) {
	f := newRepairValidationFixture(t)
	scheme := repositoryMonitorValidationRuntimeScheme(t)
	builder := setupJobBuilder()
	builder.ControllerURL = "http://orka-api.orka-system.svc:8080"
	expected, err := builder.BuildWithOptions(context.Background(), f.validation, nil, nil, JobBuildOptions{RepositoryMonitorValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	actual := repositoryMonitorValidationRuntimeJob(t, f.validation, scheme, builder)
	actual.Spec.Template.Spec.DeprecatedServiceAccount = actual.Spec.Template.Spec.ServiceAccountName
	// Simulate API-server defaulting of the patch container independently of
	// Orka's normalizer.
	for i := range actual.Spec.Template.Spec.InitContainers {
		container := &actual.Spec.Template.Spec.InitContainers[i]
		container.TerminationMessagePath = corev1.TerminationMessagePathDefault
		container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	if err := setControllerRefForTest(f.validation, expected, scheme); err != nil {
		t.Fatal(err)
	}
	if err := validateRepositoryMonitorValidationJobAgainstExpected(f.validation, actual, expected); err != nil {
		t.Fatalf("API-defaulted repair validation Job rejected: %v", err)
	}
	mutated := actual.DeepCopy()
	for i := range mutated.Spec.Template.Spec.InitContainers {
		if mutated.Spec.Template.Spec.InitContainers[i].Name == repositoryMonitorRepairValidationPatchContainer {
			mutated.Spec.Template.Spec.InitContainers[i].Args[4] = "sha256:" + strings.Repeat("d", 64)
		}
	}
	if err := validateRepositoryMonitorValidationJobAgainstExpected(f.validation, mutated, expected); !errors.Is(err, errRepositoryMonitorValidationConfinement) {
		t.Fatalf("patch digest drift error = %v, want confinement failure", err)
	}
}

func TestRepairValidationConfinementRequiresAppliedPatchBeforeNetworkPolicy(t *testing.T) {
	ctx := context.Background()
	f := newRepairValidationFixture(t)
	bindingStore := setupControllerSQLiteStore(t)
	binding, err := tools.RepositoryRepairValidationBindingEvent(f.input, f.validation)
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.EnsureRepositoryRepairValidationBinding(ctx, bindingStore, binding); err != nil {
		t.Fatal(err)
	}
	scheme := repositoryMonitorValidationRuntimeScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(f.monitor, f.repair, f.validation, f.command).Build()
	builder := setupJobBuilder()
	builder.Client = k8sClient
	builder.ControllerURL = "http://orka-api.orka-system.svc:8080"
	reconciler := &TaskReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, JobBuilder: builder, RepositoryValidationBindings: bindingStore}

	validationTask, err := reconciler.repositoryMonitorValidationTask(ctx, f.validation)
	if err != nil || !validationTask {
		t.Fatalf("repositoryMonitorValidationTask() = (%v, %v), want verified repair validation", validationTask, err)
	}
	for range 2 {
		if _, err := reconciler.ensureRepositoryMonitorValidationNetworkGate(ctx, f.validation); err != nil {
			t.Fatal(err)
		}
	}
	job := repositoryMonitorValidationRuntimeJob(t, f.validation, scheme, builder)
	f.validation.Status.Attempts = 1
	pod := repositoryMonitorValidationRuntimePod(job)
	done := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: workspacePreparationInitContainerName, State: done},
		{Name: repositoryMonitorValidationCommandContainer, State: done},
		{Name: repositoryMonitorValidationNetworkProbeContainer, State: done},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.reconcileRepositoryMonitorValidationConfinement(ctx, f.validation, job); err != nil {
		t.Fatalf("confinement before patch: %v", err)
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(f.validation), policy); !apierrors.IsNotFound(err) {
		t.Fatalf("NetworkPolicy before the patch was applied: err = %v, want not found", err)
	}
	pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{Name: repositoryMonitorRepairValidationPatchContainer, State: done})
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.reconcileRepositoryMonitorValidationConfinement(ctx, f.validation, job); err != nil {
		t.Fatalf("confinement after patch: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(f.validation), policy); err != nil {
		t.Fatalf("NetworkPolicy after the patch was applied: %v", err)
	}
}

func TestRepairValidationProvenanceFailsClosed(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*repairValidationFixture){
		"patch digest annotation drift": func(f *repairValidationFixture) {
			f.validation.Annotations[labels.AnnotationRepositoryValidationPatchDigest] = "sha256:" + strings.Repeat("e", 64)
		},
		"repair task replaced": func(f *repairValidationFixture) { f.repair.UID = "replacement-uid" },
		"monitor image changed": func(f *repairValidationFixture) {
			f.monitor.Spec.Validation.Image = "ghcr.io/example/other@sha256:" + strings.Repeat("f", 64)
		},
		"monitor command changed": func(f *repairValidationFixture) {
			f.monitor.Spec.Validation.RepairCommand = "make test-all"
		},
		"command secret tampered": func(f *repairValidationFixture) {
			f.command.Data[tools.RepositoryValidationCommandSecretKey] = []byte("true")
		},
		"workspace widened": func(f *repairValidationFixture) {
			f.validation.Spec.Workspace.PublicationCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: "write-token"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRepairValidationFixture(t)
			bindingStore := setupControllerSQLiteStore(t)
			binding, err := tools.RepositoryRepairValidationBindingEvent(f.input, f.validation)
			if err != nil {
				t.Fatal(err)
			}
			if err := tools.EnsureRepositoryRepairValidationBinding(ctx, bindingStore, binding); err != nil {
				t.Fatal(err)
			}
			mutate(&f)
			scheme := repositoryMonitorValidationRuntimeScheme(t)
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(f.monitor, f.repair, f.validation, f.command).Build()
			reconciler := &TaskReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, RepositoryValidationBindings: bindingStore}
			validationTask, err := reconciler.repositoryMonitorValidationTask(ctx, f.validation)
			if !validationTask || !errors.Is(err, errRepositoryMonitorValidationConfinement) {
				t.Fatalf("repositoryMonitorValidationTask() = (%v, %v), want confinement failure", validationTask, err)
			}
		})
	}
}

func TestRepairValidationBindingNeverMatchesAReview(t *testing.T) {
	ctx := context.Background()
	f := newRepairValidationFixture(t)
	bindingStore := setupControllerSQLiteStore(t)
	event, err := tools.RepositoryRepairValidationBindingEvent(f.input, f.validation)
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.EnsureRepositoryRepairValidationBinding(ctx, bindingStore, event); err != nil {
		t.Fatal(err)
	}
	binding, err := tools.FindRepositoryValidationCommandBinding(ctx, bindingStore, f.validation.Namespace, f.validation.Name)
	if err != nil || binding == nil {
		t.Fatalf("FindRepositoryValidationCommandBinding() = (%v, %v)", binding, err)
	}
	if binding.Kind != tools.RepositoryValidationKindRepair || binding.PatchDigest != repairValidationTestDigest {
		t.Fatalf("binding = %#v, want repair kind and patch digest", binding)
	}
	if binding.MatchesReview(f.repair, f.monitor, f.monitor.Spec.Validation.Image, repairValidationTestHeadSHA) {
		t.Fatal("a repair binding must never authorize review validation")
	}
	if !binding.MatchesRepair(f.repair, f.monitor, f.monitor.Spec.Validation.Image) {
		t.Fatal("repair binding does not match its repair")
	}
}

// repairValidationGateHarness drives the dispatcher gate against a fake API
// server whose validation Task is completed by the test.
type repairValidationGateHarness struct {
	dispatcher *ACPDispatcher
	client     client.Client
	f          repairValidationFixture
	delta      harnessv2.WorkspaceDeltaDescriptor
	reserved   []artifactcap.OperationRequest
}

type repairValidationReservations struct{ h *repairValidationGateHarness }

func (r repairValidationReservations) Reserve(_ context.Context, request artifactcap.OperationRequest, _ time.Time) error {
	r.h.reserved = append(r.h.reserved, request)
	return nil
}

func newRepairValidationGateHarness(t *testing.T, monitorMutation func(*corev1alpha1.RepositoryMonitor)) *repairValidationGateHarness {
	t.Helper()
	f := newRepairValidationFixture(t)
	if monitorMutation != nil {
		monitorMutation(f.monitor)
	}
	scheme := repositoryMonitorValidationRuntimeScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(f.monitor, f.repair).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			// The API server assigns UIDs; the patch capability is bound to one.
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("uid-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	h := &repairValidationGateHarness{client: k8sClient, f: f}
	h.dispatcher = &ACPDispatcher{
		Client: k8sClient, APIReader: k8sClient,
		ArtifactCapabilitySecret:     []byte(strings.Repeat("s", artifactcap.MinSecretBytes)),
		ArtifactReservations:         repairValidationReservations{h: h},
		RepositoryValidationBindings: setupControllerSQLiteStore(t),
		RepairValidationPollInterval: 10 * time.Millisecond,
		RepairValidationWaitTimeout:  5 * time.Second,
	}
	h.delta = harnessv2.WorkspaceDeltaDescriptor{
		State: harnessv2.WorkspaceDeltaPrepared, PublicationSafe: true, NoFollowVerified: true,
		Artifact: &harnessv2.ArtifactReference{
			ArtifactID: harnessv2.ArtifactID("sha256-" + strings.Repeat("b", 64)), Digest: repairValidationTestDigest,
			SizeBytes: 512, MediaType: artifactcap.MediaTypeWorkspaceDelta,
		},
	}
	return h
}

// completeValidation finishes the validation Task once the gate creates it.
func (h *repairValidationGateHarness) completeValidation(t *testing.T, phase corev1alpha1.TaskPhase, commandRan bool) {
	t.Helper()
	go func() {
		key := types.NamespacedName{Namespace: h.f.validation.Namespace, Name: h.f.validation.Name}
		for range 400 {
			task := &corev1alpha1.Task{}
			if err := h.client.Get(context.Background(), key, task); err == nil {
				secret := &corev1.Secret{}
				if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: key.Namespace, Name: tools.RepositoryRepairValidationCapabilitySecretName(key.Name)}, secret); err != nil {
					time.Sleep(5 * time.Millisecond)
					continue
				}
				task.Status.Phase = phase
				task.Status.Message = "validation finished"
				if commandRan {
					task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseFailed}
				}
				_ = h.client.Status().Update(context.Background(), task)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func (h *repairValidationGateHarness) run(t *testing.T) repairValidationGateOutcome {
	t.Helper()
	repair := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(h.f.repair), repair); err != nil {
		t.Fatal(err)
	}
	monitor, required, unavailable := h.dispatcher.repositoryMonitorRepairGateMonitor(context.Background(), repair)
	if !required || unavailable != "" {
		t.Fatalf("gate required = %v (%q), want required", required, unavailable)
	}
	return h.dispatcher.runRepositoryMonitorRepairValidation(context.Background(), repair, monitor, h.delta)
}

func TestRepairValidationGatePublishesOnlyARecordedPassForTheExactPatch(t *testing.T) {
	h := newRepairValidationGateHarness(t, nil)
	h.completeValidation(t, corev1alpha1.TaskPhaseSucceeded, false)
	outcome := h.run(t)
	if outcome.status != tools.RepositoryRepairValidationPassed {
		t.Fatalf("outcome = %#v, want passed", outcome)
	}
	recorded, err := tools.FindRepositoryRepairValidationResult(context.Background(), h.dispatcher.RepositoryValidationBindings, h.f.validation.Namespace, h.f.validation.Name)
	if err != nil || recorded == nil || recorded.Status != tools.RepositoryRepairValidationPassed || recorded.PatchDigest != repairValidationTestDigest {
		t.Fatalf("recorded result = (%#v, %v), want pass for the exact patch digest", recorded, err)
	}
	secret := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: h.f.validation.Namespace, Name: tools.RepositoryRepairValidationCapabilitySecretName(h.f.validation.Name)}, secret); err != nil {
		t.Fatal(err)
	}
	var document tools.RepositoryRepairValidationCapability
	if err := json.Unmarshal(secret.Data[tools.RepositoryRepairValidationCapabilitySecretKey], &document); err != nil {
		t.Fatal(err)
	}
	validation := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: h.f.validation.Namespace, Name: h.f.validation.Name}, validation); err != nil {
		t.Fatal(err)
	}
	if document.Request.Operation != string(artifactcap.OperationDownload) || document.Request.ObjectDigest != repairValidationTestDigest ||
		document.Request.TaskID != string(validation.UID) || len(h.reserved) == 0 {
		t.Fatalf("capability request = %#v, reserved = %d; want a reserved download bound to the digest and validation Task UID", document.Request, len(h.reserved))
	}
	claims, err := artifactcap.Verify(h.dispatcher.ArtifactCapabilitySecret, document.Capability, artifactcap.PresentedRequest{
		Method: "GET", Path: mustArtifactObjectPath(t, repairValidationTestDigest), ObjectDigest: repairValidationTestDigest,
		ContentLength: 512, MediaType: artifactcap.MediaTypeWorkspaceDelta, RequestDigest: document.RequestDigest,
	}, time.Now())
	if err != nil || claims.ExpiresAt.Sub(claims.IssuedAt) > artifactcap.MaxCapabilityTTL {
		t.Fatalf("capability verification = (%#v, %v)", claims, err)
	}
	if _, err := artifactcap.Verify(h.dispatcher.ArtifactCapabilitySecret, document.Capability, artifactcap.PresentedRequest{
		Method: "PUT", Path: mustArtifactObjectPath(t, repairValidationTestDigest), ObjectDigest: repairValidationTestDigest,
		ContentLength: 512, MediaType: artifactcap.MediaTypeWorkspaceDelta, RequestDigest: document.RequestDigest,
	}, time.Now()); err == nil {
		t.Fatal("patch capability must not authorize upload")
	}
	if _, err := artifactcap.Verify(h.dispatcher.ArtifactCapabilitySecret, document.Capability, artifactcap.PresentedRequest{
		Method: "GET", Path: mustArtifactObjectPath(t, repairValidationTestDigest), ObjectDigest: repairValidationTestDigest,
		ContentLength: 512, MediaType: artifactcap.MediaTypeWorkspaceDelta, RequestDigest: document.RequestDigest,
	}, time.Now().Add(artifactcap.MaxCapabilityTTL+time.Second)); !errors.Is(err, artifactcap.ErrExpired) {
		t.Fatalf("patch capability after its lifetime = %v, want expired", err)
	}

	// A different patch from the same repair is validated again.
	h.delta.Artifact.Digest = "sha256:" + strings.Repeat("c", 64)
	h.f.validation.Name = tools.RepositoryRepairValidationTaskName(h.f.repair, h.delta.Artifact.Digest)
	h.completeValidation(t, corev1alpha1.TaskPhaseFailed, true)
	if outcome := h.run(t); outcome.status != tools.RepositoryRepairValidationFailed {
		t.Fatalf("changed patch outcome = %#v, want a fresh failing validation", outcome)
	}
}

func TestRepairValidationGateFailureAndUnavailableAreNotPublished(t *testing.T) {
	for name, tc := range map[string]struct {
		phase      corev1alpha1.TaskPhase
		commandRan bool
		want       tools.RepositoryRepairValidationStatus
	}{
		"command failed":      {phase: corev1alpha1.TaskPhaseFailed, commandRan: true, want: tools.RepositoryRepairValidationFailed},
		"check could not run": {phase: corev1alpha1.TaskPhaseFailed, want: tools.RepositoryRepairValidationUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRepairValidationGateHarness(t, nil)
			h.completeValidation(t, tc.phase, tc.commandRan)
			outcome := h.run(t)
			if outcome.status != tc.want || !strings.Contains(outcome.evidence, "Nothing was pushed") {
				t.Fatalf("outcome = %#v, want %s with evidence", outcome, tc.want)
			}
			// A redispatch of the same patch reuses the recorded verdict.
			if again := h.run(t); again.status != tc.want {
				t.Fatalf("replayed outcome = %#v, want %s", again, tc.want)
			}
		})
	}
}

func TestRepairValidationGateTimesOutClosed(t *testing.T) {
	h := newRepairValidationGateHarness(t, nil)
	h.dispatcher.RepairValidationWaitTimeout = 50 * time.Millisecond
	if outcome := h.run(t); outcome.status != tools.RepositoryRepairValidationUnavailable {
		t.Fatalf("outcome = %#v, want unavailable", outcome)
	}
}

func TestRepairValidationGateDoesNotApplyWithoutRepairCommand(t *testing.T) {
	h := newRepairValidationGateHarness(t, func(monitor *corev1alpha1.RepositoryMonitor) {
		monitor.Spec.Validation.RepairCommand = ""
	})
	repair := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(h.f.repair), repair); err != nil {
		t.Fatal(err)
	}
	if _, required, _ := h.dispatcher.repositoryMonitorRepairGateMonitor(context.Background(), repair); required {
		t.Fatal("a monitor without repairCommand must not gate publication")
	}
	status, err := h.dispatcher.gateRepositoryMonitorRepairPublication(context.Background(), repair, "attempt", fenceForRepairValidationTest(), h.delta)
	if err != nil || status.State != "" {
		t.Fatalf("gate = (%#v, %v), want no gate", status, err)
	}
	var tasks corev1alpha1.TaskList
	if err := h.client.List(context.Background(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 1 {
		t.Fatalf("tasks = %d, want only the repair Task (no validation Task)", len(tasks.Items))
	}
}

func TestRepairValidationGateFailsClosedWhenMonitorIsMissing(t *testing.T) {
	h := newRepairValidationGateHarness(t, nil)
	if err := h.client.Delete(context.Background(), h.f.monitor); err != nil {
		t.Fatal(err)
	}
	repair := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(h.f.repair), repair); err != nil {
		t.Fatal(err)
	}
	_, err := h.dispatcher.gateRepositoryMonitorRepairPublication(context.Background(), repair, "attempt", fenceForRepairValidationTest(), h.delta)
	var deliveryErr *acpDeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.reason != repositoryMonitorRepairValidationUnavailableDeliveryReason {
		t.Fatalf("gate error = %v, want unavailable delivery error", err)
	}
}

func setControllerRefForTest(owner *corev1alpha1.Task, job client.Object, scheme *runtime.Scheme) error {
	return controllerutil.SetControllerReference(owner, job, scheme)
}

func mustArtifactObjectPath(t *testing.T, digest string) string {
	t.Helper()
	value, err := artifactcap.ObjectPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fenceForRepairValidationTest() store.ControllerEpochFence { return store.ControllerEpochFence{} }

func TestRepairValidationDeliveryMessageFitsTheTaskStatusSchema(t *testing.T) {
	message := repairValidationDeliveryMessage(strings.Repeat("é", 9000))
	if len([]rune(message)) > 1024 {
		t.Fatalf("delivery message has %d characters, want at most 1024", len([]rune(message)))
	}
	if repairValidationDeliveryMessage("") == "" {
		t.Fatal("empty evidence must still explain that nothing was pushed")
	}
}

// A command that started and then ran past the validation timeout is recorded
// as an executed failure, like review validation, so the repair agent gets
// the feedback. A timeout before the command started could not validate.
func TestClassifyRepairValidationTaskTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		executed bool
		message  string
		want     tools.RepositoryRepairValidationStatus
	}{
		{name: "command timed out", executed: true, message: "task timed out", want: tools.RepositoryRepairValidationFailed},
		{name: "never started", message: "task timed out; " + podWaitingMessagePrefix + "ImagePullBackOff", want: tools.RepositoryRepairValidationUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "repair-validation"}}
			task.Status.Phase = corev1alpha1.TaskPhaseFailed
			task.Status.Message = tc.message
			if tc.executed {
				task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseFailed, Message: tc.message}
			}
			got := classifyRepairValidationTask(task, "make test")
			if got.status != tc.want || !strings.Contains(got.evidence, tc.message) {
				t.Fatalf("classifyRepairValidationTask() = %#v, want status %s with the controller message", got, tc.want)
			}
		})
	}
}

// Labels and annotations are mutable by Task editors, so removing them must
// not disable the gate; only the immutable spec and the admission-held
// RepositoryMonitor controller reference identify a repair. A monitor-owned
// write Task without an exact pull request head, such as an issue
// implementation, is not a repair.
func TestRepairValidationGateIgnoresMutableRepairMarkers(t *testing.T) {
	h := newRepairValidationGateHarness(t, nil)
	repair := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(h.f.repair), repair); err != nil {
		t.Fatal(err)
	}
	stripped := repair.DeepCopy()
	delete(stripped.Labels, labels.LabelCreatedBy)
	delete(stripped.Annotations, repositoryMonitorIssueAnnotationCommandID)
	delete(stripped.Annotations, labels.AnnotationMonitorItemKind)
	if _, required, unavailable := h.dispatcher.repositoryMonitorRepairGateMonitor(context.Background(), stripped); !required || unavailable != "" {
		t.Fatalf("gate required = %v (%q) after stripping metadata, want required", required, unavailable)
	}
	implementation := repair.DeepCopy()
	implementation.Spec.Workspace.ExpectedRemoteSHA = ""
	if _, required, _ := h.dispatcher.repositoryMonitorRepairGateMonitor(context.Background(), implementation); required {
		t.Fatal("a monitor write Task without an exact pull request head must not be gated")
	}
}

// A pass counts only while the live monitor still requires the check that
// produced it; a changed or removed check fails closed.
func TestRepairValidationPassIsRecheckedAgainstTheLiveMonitor(t *testing.T) {
	passed := repairValidationGateOutcome{required: true, status: tools.RepositoryRepairValidationPassed}
	for name, tc := range map[string]struct {
		mutate func(*corev1alpha1.RepositoryMonitor)
		want   tools.RepositoryRepairValidationStatus
	}{
		"unchanged":       {mutate: func(*corev1alpha1.RepositoryMonitor) {}, want: tools.RepositoryRepairValidationPassed},
		"command changed": {mutate: func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Validation.RepairCommand = "make test-all" }, want: tools.RepositoryRepairValidationUnavailable},
		"command removed": {mutate: func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Validation.RepairCommand = "" }, want: tools.RepositoryRepairValidationUnavailable},
		"image changed": {mutate: func(m *corev1alpha1.RepositoryMonitor) {
			m.Spec.Validation.Image = "ghcr.io/example/other@sha256:" + strings.Repeat("f", 64)
		}, want: tools.RepositoryRepairValidationUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRepairValidationGateHarness(t, nil)
			repair := &corev1alpha1.Task{}
			if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(h.f.repair), repair); err != nil {
				t.Fatal(err)
			}
			validated, required, _ := h.dispatcher.repositoryMonitorRepairGateMonitor(context.Background(), repair)
			if !required || validated == nil {
				t.Fatal("fixture repair is not gated")
			}
			live := validated.DeepCopy()
			tc.mutate(live)
			if err := h.client.Update(context.Background(), live); err != nil {
				t.Fatal(err)
			}
			if got := h.dispatcher.recheckRepairValidationConfiguration(context.Background(), repair, validated, passed); got.status != tc.want {
				t.Fatalf("recheck = %#v, want %s", got, tc.want)
			}
		})
	}
}

// Orphan deletion of a RepositoryMonitor removes the owner reference but
// leaves the admission-held monitor markers; such a repair fails closed.
func TestRepairValidationGateFailsClosedForOrphanedRepair(t *testing.T) {
	h := newRepairValidationGateHarness(t, nil)
	repair := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(h.f.repair), repair); err != nil {
		t.Fatal(err)
	}
	for name, marker := range map[string]func(*corev1alpha1.Task){
		"monitor label": func(task *corev1alpha1.Task) {
			task.Labels = map[string]string{labels.LabelRepositoryMonitor: h.f.monitor.Name}
		},
		"monitor annotation": func(task *corev1alpha1.Task) {
			task.Annotations = map[string]string{labels.AnnotationRepositoryMonitorName: h.f.monitor.Name}
		},
	} {
		t.Run(name, func(t *testing.T) {
			orphaned := repair.DeepCopy()
			orphaned.OwnerReferences = nil
			orphaned.Labels, orphaned.Annotations = nil, nil
			marker(orphaned)
			if _, required, unavailable := h.dispatcher.repositoryMonitorRepairGateMonitor(context.Background(), orphaned); !required || unavailable == "" {
				t.Fatalf("gate = (required %v, %q), want required and unavailable", required, unavailable)
			}
		})
	}
	unmarked := repair.DeepCopy()
	unmarked.OwnerReferences = nil
	unmarked.Labels, unmarked.Annotations = nil, nil
	if _, required, _ := h.dispatcher.repositoryMonitorRepairGateMonitor(context.Background(), unmarked); required {
		t.Fatal("a write Task with no RepositoryMonitor owner or marker must not be gated")
	}
}

// stallingTaskReader blocks reads of one Task until the caller's context
// ends, like an unresponsive API server.
type stallingTaskReader struct {
	client.Reader
	name string
}

func (r stallingTaskReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1alpha1.Task); ok && key.Name == r.name {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// A stalled API call cannot hold the gate, and with it the runtime session,
// past the validation wait.
func TestRepairValidationGateBoundsStalledAPICalls(t *testing.T) {
	h := newRepairValidationGateHarness(t, nil)
	h.dispatcher.RepairValidationWaitTimeout = 200 * time.Millisecond
	h.dispatcher.APIReader = stallingTaskReader{Reader: h.client, name: h.f.validation.Name}
	done := make(chan repairValidationGateOutcome, 1)
	go func() { done <- h.run(t) }()
	select {
	case outcome := <-done:
		if outcome.status != tools.RepositoryRepairValidationUnavailable {
			t.Fatalf("outcome = %#v, want unavailable", outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gate did not return after its wait deadline while the API server stalled")
	}
}
