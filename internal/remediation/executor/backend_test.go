package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// This store supplies deterministic observer fixtures, not runtime proof. All
// production binding, evidence validation, assessment, and sealing remain real.
type finishingStore struct {
	*sqlite.Store
	finish  bool
	readErr error
}

func (s *finishingStore) GetPatchVerificationRun(ctx context.Context, id string) (*pv.Record, error) {
	record, err := s.Store.GetPatchVerificationRun(ctx, id)
	if s.readErr != nil && record != nil && record.State == pv.RunFinalized {
		return record, s.readErr
	}
	return record, err
}

func (s *finishingStore) BindKubernetesValidationTasks(ctx context.Context, ns, id, original, patched string, binding pv.Binding) error {
	if err := s.Store.BindKubernetesValidationTasks(ctx, ns, id, original, patched, binding); err != nil {
		return err
	}
	if !s.finish {
		return nil
	}
	record, err := s.Store.GetPatchVerificationRun(ctx, binding.RunID)
	if err != nil {
		return err
	}
	for _, side := range pv.ActionSides(record.Manifest.Action) {
		for _, check := range record.Manifest.Checks {
			expectation := check.Healthy
			taskID, tree := binding.PatchedTaskID, record.Manifest.Sources.Patched.Tree
			if side == pv.Original {
				taskID, tree = binding.OriginalTaskID, record.Manifest.Sources.Original.Tree
				if check.Kind == pv.Reproduction {
					expectation = check.Failure
				}
			}
			now := time.Now().UTC()
			obs := pv.Observation{RunID: binding.RunID, AttemptID: binding.AttemptID, TaskID: taskID,
				ManifestDigest: binding.ManifestDigest, Side: side, CheckID: check.ID, SourceTree: tree,
				ImageID: record.Manifest.Environment.ImageID, ContainerID: "container-" + side + "-" + check.ID,
				JobUID: "job-" + side + "-" + check.ID, PodUID: "pod-" + side + "-" + check.ID,
				Origin: "runner", StartedAt: now, FinishedAt: now.Add(time.Millisecond),
				Executed: true, HTTPCompleted: true, ExitCode: new(0),
				StdoutDigest: pv.Digest([]byte(expectation.Stdout)), StdoutBytes: len(expectation.Stdout), StderrDigest: pv.Digest(nil)}
			if err := s.RecordPatchVerificationEvidence(ctx, binding, pv.ExecutionEvidence{Observation: obs,
				Blobs: map[string][]byte{obs.StdoutDigest: []byte(expectation.Stdout), obs.StderrDigest: {}}}); err != nil {
				return err
			}
		}
	}
	_, err = s.FinalizePatchVerificationRun(ctx, binding)
	return err
}

type fixture struct {
	backend    *Backend
	store      *finishingStore
	request    contract.ExecutionRequest
	jobs       []*batchv1.Job
	deleteUIDs []types.UID
}

func newFixture(t *testing.T, finish bool) *fixture {
	t.Helper()
	f := &fixture{}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	f.store = &finishingStore{Store: sqlite.NewStore(db, ":memory:"), finish: finish}
	healthy, err := pv.HTTPExpectation(200, "hello")
	require.NoError(t, err)
	failure, err := pv.HTTPExpectation(500, "bad")
	require.NoError(t, err)
	file := pv.FrozenFile{Path: "server.sh", Content: []byte("#!/bin/sh\nexec /usr/bin/server /src/README\n"), Executable: true}
	file.Digest = pv.Digest(file.Content)
	f.request = contract.ExecutionRequest{ID: "synthetic-run", Namespace: "default",
		ParentTaskName: "parent", ParentTaskUID: "parent-uid", Action: pv.ValidateReport, Problem: "synthetic greeting behavior",
		Source: contract.Source{Repository: "https://github.com/example/fixture", Commit: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40)},
		Checks: contract.Checks{Scope: []string{"greeting"}, Files: []pv.FrozenFile{file},
			Checks: []pv.Check{{ID: "greeting", Kind: pv.Reproduction,
				HTTP:    &pv.HTTPCheck{Version: pv.HTTPCheckVersion, ServerCommand: []string{"/checks/server.sh"}, Path: "/"},
				Healthy: healthy, Failure: failure, TimeoutSeconds: 2}}}}
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"}}
	var sequence atomic.Int64
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1.Pod{}, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
				if object.GetUID() == "" {
					object.SetUID(types.UID(fmt.Sprintf("uid-%d", sequence.Add(1))))
				}
				object.SetCreationTimestamp(metav1.Now())
				if err := c.Create(ctx, object, options...); err != nil {
					return err
				}
				if job, ok := object.(*batchv1.Job); ok {
					f.jobs = append(f.jobs, job.DeepCopy())
					if job.Spec.Template.Spec.Containers[0].Name == "prepare" {
						pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace,
							UID: types.UID("pod-" + string(job.UID)), Labels: map[string]string{"job-name": job.Name},
							OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}},
							Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodSucceeded,
								ContainerStatuses: []corev1.ContainerStatus{{Name: "prepare", ImageID: f.backend.config.GeneralWorkerImageID,
									ContainerID: "container-" + string(job.UID), State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}}}
						return c.Create(ctx, pod)
					}
				}
				return nil
			},
			Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
				opts := (&client.DeleteOptions{}).ApplyOptions(options)
				if _, ok := object.(*batchv1.Job); ok {
					require.NotNil(t, opts.Preconditions)
					require.NotNil(t, opts.Preconditions.UID)
					require.Equal(t, object.GetUID(), *opts.Preconditions.UID)
					f.deleteUIDs = append(f.deleteUIDs, *opts.Preconditions.UID)
					pods := &corev1.PodList{}
					require.NoError(t, c.List(ctx, pods, client.InNamespace(object.GetNamespace())))
					for _, pod := range pods.Items {
						for _, owner := range pod.OwnerReferences {
							if owner.UID == object.GetUID() {
								require.NoError(t, c.Delete(ctx, &pod))
							}
						}
					}
				}
				return c.Delete(ctx, object, options...)
			},
		}).Build()
	f.backend, err = New(c, c, kubernetesfake.NewClientset(), f.store, scheme, Config{
		Enabled: true, Namespace: "default", GeneralWorkerImage: "example/general@" + pv.Digest([]byte("general")),
		GeneralWorkerImageID: pv.Digest([]byte("general-config")),
		HelperImage:          "example/helper@" + pv.Digest([]byte("helper")), ToolImage: "example/tool@" + pv.Digest([]byte("tool")),
		HelperImageID: pv.Digest([]byte("helper-config")),
		ToolImageID:   "example/tool@" + pv.Digest([]byte("tool")),
		Platform:      "linux/amd64", Profile: pv.LocalServices, LocalServices: true,
		NodeSelectorKey: "orka.ai/validation", NodeSelectorValue: "dedicated",
		CanaryHost: "10.0.0.2", CanaryPort: 80, ReconcileInterval: time.Millisecond, RunTimeout: 5 * time.Second, CleanupTimeout: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	f.backend.readReport = func(ctx context.Context, namespace, name string) (pv.PreparationReport, error) {
		pod := &corev1.Pod{}
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod))
		cm := &corev1.ConfigMap{}
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pod.Labels["job-name"] + "-input"}, cm))
		input, err := pv.DecodePreparationInput(bytes.NewReader(cm.BinaryData["request.json"]))
		require.NoError(t, err)
		report := fixtureReport(t, input)
		report.PodUID = string(pod.UID)
		return report, nil
	}
	return f
}

func fixtureReport(t *testing.T, input pv.PreparationInput) pv.PreparationReport {
	t.Helper()
	digest, err := pv.PreparationInputDigest(input)
	require.NoError(t, err)
	report := pv.PreparationReport{Version: pv.PreparationProtocolVersion, TaskUID: input.TaskUID, InputDigest: digest,
		Manifest: input.Manifest, Provenance: map[string][]byte{pv.Digest([]byte("original archive")): []byte("original archive")}}
	report.Manifest.Sources.Original.ArchiveDigest = pv.Digest([]byte("original archive"))
	for _, file := range input.Manifest.Files {
		report.Provenance[file.Digest] = file.Content
	}
	if input.Manifest.Action == pv.VerifyPatch {
		report.Manifest.Sources.Patched = pv.SourceIdentity{Tree: strings.Repeat("d", 40), ArchiveDigest: pv.Digest([]byte("patched archive"))}
		report.Manifest.Sources.PatchDigest = pv.Digest(input.Patch)
		report.Manifest.Sources.DiffDigest = pv.Digest([]byte("diff"))
		report.Provenance[report.Manifest.Sources.Patched.ArchiveDigest] = []byte("patched archive")
		report.Provenance[report.Manifest.Sources.PatchDigest] = input.Patch
		report.Provenance[report.Manifest.Sources.DiffDigest] = []byte("diff")
	}
	require.NoError(t, pv.ValidatePreparationReport(input, report))
	return report
}

func TestFirstRunPersistsAcceptanceBeforeJobAndResumesSameSeal(t *testing.T) {
	f := newFixture(t, true)
	var receipt contract.ExecutionReceipt
	record, err := f.backend.Start(t.Context(), f.request, func(value contract.ExecutionReceipt) error {
		require.Empty(t, f.jobs)
		require.NotEmpty(t, value.TaskUID)
		receipt = value
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, pv.Reproduced, record.Assessment.Conclusion)
	require.NotEqual(t, imageDigest(f.backend.config.GeneralWorkerImage), f.backend.config.GeneralWorkerImageID)
	require.Equal(t, f.backend.config.GeneralWorkerImage, record.Manifest.Environment.Dependencies["orka.remediation.general-image"])
	require.Equal(t, f.backend.config.GeneralWorkerImageID, record.Manifest.Environment.Dependencies["orka.remediation.general-image-id"])
	require.Len(t, f.jobs, 1)
	require.Len(t, f.deleteUIDs, 1)
	require.Equal(t, f.jobs[0].UID, f.deleteUIDs[0])
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task))
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, task.Status.Phase)
	require.Empty(t, task.Finalizers)
	again, err := f.backend.Resume(t.Context(), f.request, receipt)
	require.NoError(t, err)
	require.Equal(t, record.Seal.Digest, again.Seal.Digest)
	require.Len(t, f.jobs, 1, "resume must not create replacement work")
	require.NoError(t, pv.ValidateRunBinding(record.Manifest, record.Binding))
	require.NotEqual(t, receipt.TaskUID, record.Binding.OriginalTaskID)
}

func TestPreparationAndCheckJobWireIsolation(t *testing.T) {
	f := newFixture(t, true)
	record, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
	require.NoError(t, err)
	prep := f.jobs[0]
	require.Equal(t, int32(0), *prep.Spec.BackoffLimit)
	require.Equal(t, min(int64(120), int64(f.backend.config.RunTimeout/time.Second)), *prep.Spec.ActiveDeadlineSeconds)
	require.Equal(t, batchv1.Failed, *prep.Spec.PodReplacementPolicy)
	pod := prep.Spec.Template.Spec
	require.False(t, *pod.AutomountServiceAccountToken)
	require.False(t, *pod.EnableServiceLinks)
	require.Empty(t, pod.InitContainers)
	require.Equal(t, corev1.StorageMediumMemory, pod.Volumes[1].EmptyDir.Medium)
	require.Equal(t, "256Mi", pod.Volumes[1].EmptyDir.SizeLimit.String())
	container := pod.Containers[0]
	require.Equal(t, []string{"/worker", "--prepare-validation-source", "/input/request.json"}, container.Command)
	require.True(t, *container.SecurityContext.ReadOnlyRootFilesystem)
	require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	require.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)
	require.Equal(t, "512Mi", container.Resources.Limits.Memory().String())
	require.Equal(t, "1", container.Resources.Limits.Cpu().String())
	require.Empty(t, container.EnvFrom)
	require.Empty(t, container.SecurityContext.Capabilities.Add)
	submission, err := f.store.GetKubernetesValidationSubmissionByRun(t.Context(), "default", record.Binding.RunID)
	require.NoError(t, err)
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: submission.OriginalTaskName}, task))
	job, err := f.backend.BuildValidationJob(t.Context(), task)
	require.NoError(t, err)
	require.Equal(t, task.UID, job.OwnerReferences[0].UID)
	require.Equal(t, []string{"/runner/orka-validation-worker", "--input", "/input/bundle.gz"}, job.Spec.Template.Spec.Containers[0].Command)
	require.False(t, *job.Spec.Template.Spec.AutomountServiceAccountToken)
	require.Equal(t, int32(0), *job.Spec.BackoffLimit)
	require.Equal(t, batchv1.Failed, *job.Spec.PodReplacementPolicy)
	require.Len(t, job.Spec.Template.Spec.InitContainers, 1)
	require.Equal(t, f.backend.config.HelperImage, job.Spec.Template.Spec.InitContainers[0].Image)
	require.Equal(t, f.backend.config.ToolImage, job.Spec.Template.Spec.Containers[0].Image)
}

func TestAcceptanceAndStatusErrorsRemainErrors(t *testing.T) {
	t.Run("callback", func(t *testing.T) {
		f := newFixture(t, true)
		callbackErr := errors.New("private callback details")
		record, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return callbackErr })
		require.Nil(t, record)
		require.ErrorIs(t, err, callbackErr)
		require.Equal(t, contract.ErrUnavailable.Error(), err.Error())
		require.Empty(t, f.jobs)
	})
	t.Run("status write", func(t *testing.T) {
		f := newFixture(t, true)
		writeErr := errors.New("private storage details")
		original := f.backend.client
		f.backend.client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{
			SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
				return writeErr
			},
		})
		record, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
		require.Nil(t, record)
		require.ErrorIs(t, err, writeErr)
		require.Equal(t, contract.ErrUnavailable.Error(), err.Error())
	})
}

func TestInvalidRequestsHaveNoEffects(t *testing.T) {
	cases := map[string]func(*contract.ExecutionRequest){
		"namespace":  func(r *contract.ExecutionRequest) { r.Namespace = "other" },
		"parent UID": func(r *contract.ExecutionRequest) { r.ParentTaskUID = "replaced" },
		"tree":       func(r *contract.ExecutionRequest) { r.Source.Tree = "HEAD" },
		"repository": func(r *contract.ExecutionRequest) { r.Source.Repository = "https://127.0.0.1/repository" },
		"raw verdict": func(r *contract.ExecutionRequest) {
			r.Checks.Checks[0].HTTP = nil
			r.Checks.Checks[0].Command = []string{"/checks/server.sh"}
		},
		"candidate in validation": func(r *contract.ExecutionRequest) { r.Patch = "unexpected patch" },
		"mutable checks":          func(r *contract.ExecutionRequest) { r.Checks.Files[0].Content = []byte("changed") },
		"cluster requirement": func(r *contract.ExecutionRequest) {
			r.Checks.Requirements = []pv.EnvironmentRequirement{{Kind: "cluster", Name: "required"}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, true)
			change(&f.request)
			called := false
			record, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { called = true; return nil })
			require.Error(t, err)
			require.Nil(t, record)
			require.False(t, called)
			require.Empty(t, f.jobs)
			cms := &corev1.ConfigMapList{}
			require.NoError(t, f.backend.reader.List(t.Context(), cms))
			require.Empty(t, cms.Items)
		})
	}
}

func TestResumeRejectsChangedBindingsWithoutEffects(t *testing.T) {
	for _, kind := range []string{"receipt UID", "input digest", "parent UID", "task UID", "patch", "check", "image", "image ID", "helper image ID"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, true)
			var receipt contract.ExecutionReceipt
			_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
			require.NoError(t, err)
			switch kind {
			case "receipt UID":
				receipt.TaskUID = "other"
			case "input digest":
				receipt.InputDigest = pv.Digest([]byte("other"))
			case "parent UID":
				parent := &corev1alpha1.Task{}
				require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "parent"}, parent))
				parent.UID = "other"
				require.NoError(t, f.backend.client.Update(t.Context(), parent))
			case "task UID":
				task := &corev1alpha1.Task{}
				require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: receipt.TaskName}, task))
				task.UID = "other"
				require.NoError(t, f.backend.client.Update(t.Context(), task))
			case "patch":
				f.request.Patch = "changed"
			case "check":
				f.request.Checks.Checks[0].HTTP.Path = "/different"
			case "image":
				f.backend.config.GeneralWorkerImage = "example/general@" + pv.Digest([]byte("different"))
			case "image ID":
				f.backend.config.GeneralWorkerImageID = pv.Digest([]byte("different config"))
			case "helper image ID":
				f.backend.config.HelperImageID = pv.Digest([]byte("different helper config"))
			}
			record, err := f.backend.Resume(t.Context(), f.request, receipt)
			require.Error(t, err)
			require.Nil(t, record)
			require.Len(t, f.jobs, 1)
		})
	}
}

func TestResumeNeverAdmitsMissingPreparationJob(t *testing.T) {
	f := newFixture(t, false)
	callbackErr := errors.New("stop before admission")
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return callbackErr })
	require.ErrorIs(t, err, callbackErr)
	_, err = f.backend.Resume(t.Context(), f.request, receipt)
	require.ErrorIs(t, err, contract.ErrUnavailable)
	require.Empty(t, f.jobs)
}

func TestCancellationUsesExactUIDAndIsFinite(t *testing.T) {
	f := newFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	f.backend.readReport = func(context.Context, string, string) (pv.PreparationReport, error) {
		cancel()
		return pv.PreparationReport{}, context.Canceled
	}
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(ctx, f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
	require.Error(t, err)
	wrong := receipt
	wrong.TaskUID = "wrong"
	require.ErrorIs(t, f.backend.Cancel(t.Context(), wrong), contract.ErrConflict)
	require.Empty(t, f.deleteUIDs)
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	require.Equal(t, []types.UID{f.jobs[0].UID}, f.deleteUIDs)
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task))
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, task.Status.Phase)
	require.Empty(t, task.Finalizers)
}

func TestErrorBearingFinalEvidenceCannotBecomeSuccess(t *testing.T) {
	f := newFixture(t, true)
	var receipt contract.ExecutionReceipt
	record, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
	require.NoError(t, err)
	require.Equal(t, pv.Reproduced, record.Assessment.Conclusion)
	f.store.readErr = pv.ErrIntegrity
	record, err = f.backend.Resume(t.Context(), f.request, receipt)
	require.Error(t, err)
	require.Nil(t, record)
}

func TestCandidateHasFreshOriginalAndPatchedBindings(t *testing.T) {
	f := newFixture(t, true)
	f.request.Action = pv.VerifyPatch
	f.request.Patch = "synthetic unit fixture patch"
	f.request.DeclaredChanges = []pv.DeclaredChange{{Kind: "source", Paths: []string{"README"}, Description: "synthetic greeting change"}}
	normal := f.request.Checks.Checks[0]
	normal.ID, normal.Kind = "normal", pv.Normal
	f.request.Checks.Checks = append(f.request.Checks.Checks, normal)
	record, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
	require.NoError(t, err)
	require.Equal(t, pv.Verified, record.Assessment.Conclusion)
	require.NotEqual(t, record.Binding.OriginalTaskID, record.Binding.PatchedTaskID)
	require.Len(t, record.Evidence, 4)
	require.Equal(t, pv.Digest([]byte(f.request.Patch)), record.Manifest.Sources.PatchDigest)
}

func TestCancelledPreparationCannotBeReplaced(t *testing.T) {
	f := newFixture(t, false)
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return errors.New("stop") })
	require.Error(t, err)
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: receipt.TaskName}, task))
	require.NoError(t, f.backend.client.Delete(t.Context(), task))
	_, err = f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
	require.ErrorIs(t, err, contract.ErrConflict)
	require.Empty(t, f.jobs)
}

func TestPreparedReportCannotChangeFrozenChecksOrCandidate(t *testing.T) {
	for _, change := range []func(*pv.PreparationReport){
		func(r *pv.PreparationReport) { r.Manifest.Checks[0].HTTP.Path = "/forged" },
		func(r *pv.PreparationReport) { r.Manifest.Sources.Original.Tree = strings.Repeat("f", 40) },
		func(r *pv.PreparationReport) { r.Provenance[pv.Digest([]byte("unbound"))] = []byte("unbound") },
		func(r *pv.PreparationReport) { r.TaskUID = "forged" },
	} {
		f := newFixture(t, false)
		read := f.backend.readReport
		f.backend.readReport = func(ctx context.Context, ns, name string) (pv.PreparationReport, error) {
			report, err := read(ctx, ns, name)
			encoded, marshalErr := json.Marshal(report)
			require.NoError(t, marshalErr)
			var copied pv.PreparationReport
			require.NoError(t, json.Unmarshal(encoded, &copied))
			change(&copied)
			return copied, err
		}
		record, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
		require.Error(t, err)
		require.Nil(t, record)
		submissions, listErr := f.store.ListActiveKubernetesValidationSubmissions(t.Context(), "default")
		require.NoError(t, listErr)
		require.Empty(t, submissions)
	}
}

func TestFixtureJSONDoesNotHideChangedPreparationIdentity(t *testing.T) {
	f := newFixture(t, true)
	input, digest, err := f.backend.requestInput(f.request)
	require.NoError(t, err)
	input.TaskUID = "uid-test"
	require.Equal(t, digest, func() string { d, e := pv.PreparationInputDigest(input); require.NoError(t, e); return d }())
	report := fixtureReport(t, input)
	require.True(t, reflect.DeepEqual(input.Manifest.Checks, report.Manifest.Checks))
	report.InputDigest = pv.Digest([]byte("tampered"))
	require.Error(t, pv.ValidatePreparationReport(input, report))
}

func TestCleanupRefusesReplacedJob(t *testing.T) {
	f := newFixture(t, false)
	var receipt contract.ExecutionReceipt
	f.backend.readReport = func(context.Context, string, string) (pv.PreparationReport, error) {
		return pv.PreparationReport{}, errors.New("stop")
	}
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
	require.Error(t, err)
	job := &batchv1.Job{}
	require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: receipt.TaskName}, job))
	job.UID = "replacement"
	require.NoError(t, f.backend.client.Update(t.Context(), job))
	require.ErrorIs(t, f.backend.Cancel(t.Context(), receipt), contract.ErrConflict)
	require.Empty(t, f.deleteUIDs)
	require.False(t, apierrors.IsNotFound(f.backend.client.Get(t.Context(), client.ObjectKeyFromObject(job), job)))
}

func TestRuntimeAndInputTamperingCannotProduceEvidence(t *testing.T) {
	for _, mutation := range []string{"pod UID", "pod credentials", "job retry", "input bytes", "image identity"} {
		t.Run(mutation, func(t *testing.T) {
			f := newFixture(t, true)
			var receipt contract.ExecutionReceipt
			_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error {
				receipt = r
				return errors.New("persisted receipt; pause before admission")
			})
			require.Error(t, err)
			input, _, err := f.backend.requestInput(f.request)
			require.NoError(t, err)
			input.TaskUID = receipt.TaskUID
			require.NoError(t, f.backend.createPreparation(t.Context(), input, receipt))
			switch mutation {
			case "pod UID", "pod credentials", "image identity":
				pod := &corev1.Pod{}
				require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName + "-pod"}, pod))
				switch mutation {
				case "pod UID":
					pod.UID = "replacement"
					read := f.backend.readReport
					f.backend.readReport = func(ctx context.Context, ns, name string) (pv.PreparationReport, error) {
						report, err := read(ctx, ns, name)
						report.PodUID = "old-pod"
						return report, err
					}
				case "pod credentials":
					pod.Spec.AutomountServiceAccountToken = new(true)
				case "image identity":
					pod.Status.ContainerStatuses[0].ImageID = "example/untrusted@" + pv.Digest([]byte("untrusted image"))
					require.NoError(t, f.backend.client.Status().Update(t.Context(), pod))
				}
				require.NoError(t, f.backend.client.Update(t.Context(), pod))
			case "job retry":
				job := &batchv1.Job{}
				require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, job))
				job.Spec.BackoffLimit = new(int32(1))
				require.NoError(t, f.backend.client.Update(t.Context(), job))
			case "input bytes":
				cm := &corev1.ConfigMap{}
				require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName + "-input"}, cm))
				cm.BinaryData["request.json"] = []byte("{}")
				require.NoError(t, f.backend.client.Update(t.Context(), cm))
			}
			record, err := f.backend.Resume(t.Context(), f.request, receipt)
			require.Error(t, err)
			require.Nil(t, record)
			require.Len(t, f.jobs, 1)
		})
	}
}

func TestCancellationFinishesDeletingTaskAndBoundsLockWait(t *testing.T) {
	f := newFixture(t, false)
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error {
		receipt = r
		return errors.New("pause")
	})
	require.Error(t, err)
	f.backend.effectGate <- struct{}{}
	start := time.Now()
	err = f.backend.Cancel(t.Context(), receipt)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)
	<-f.backend.effectGate
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task))
	require.NoError(t, f.backend.client.Delete(t.Context(), task))
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	require.True(t, apierrors.IsNotFound(f.backend.client.Get(t.Context(), client.ObjectKeyFromObject(task), task)))
}

func TestConfigurationDigestAndRetryableErrorsAreExplicit(t *testing.T) {
	f := newFixture(t, true)
	before := f.backend.ConfigurationDigest()
	require.True(t, digestPattern.MatchString(before))
	digest, err := f.backend.RequestDigest(f.request)
	require.NoError(t, err)
	f.backend.config.CanaryPort++
	require.NotEqual(t, before, f.backend.ConfigurationDigest())
	after, err := f.backend.RequestDigest(f.request)
	require.NoError(t, err)
	require.NotEqual(t, digest, after)
	disabled, err := New(nil, nil, nil, nil, nil, Config{})
	require.NoError(t, err)
	require.Empty(t, disabled.ConfigurationDigest())
	for _, test := range []struct {
		cause error
		retry bool
	}{
		{apierrors.NewServiceUnavailable("private details"), true},
		{apierrors.NewTimeoutError("private details", 1), true},
		{pv.ErrIntegrity, false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
	} {
		err := safeError(contract.ErrUnavailable, test.cause)
		var marker interface{ Retryable() bool }
		require.ErrorAs(t, err, &marker)
		require.Equal(t, test.retry, marker.Retryable())
		require.Equal(t, contract.ErrUnavailable.Error(), err.Error())
	}
}

func TestCancellationDoesNotClaimFinalizedInputCleanup(t *testing.T) {
	f := newFixture(t, false)
	f.backend.readReport = func(context.Context, string, string) (pv.PreparationReport, error) {
		return pv.PreparationReport{}, errors.New("pause")
	}
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
	require.Error(t, err)
	cm := &corev1.ConfigMap{}
	require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName + "-input"}, cm))
	cm.Finalizers = []string{"test.example/hold"}
	require.NoError(t, f.backend.client.Update(t.Context(), cm))
	start := time.Now()
	err = f.backend.Cancel(t.Context(), receipt)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task))
	require.Contains(t, task.Finalizers, preparationFinalizer)
	require.NotEqual(t, corev1alpha1.TaskPhaseSucceeded, task.Status.Phase)
}

func TestStandardTaskCancellationInterceptsPreparation(t *testing.T) {
	f := newFixture(t, false)
	f.backend.readReport = func(context.Context, string, string) (pv.PreparationReport, error) {
		return pv.PreparationReport{}, errors.New("pause")
	}
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
	require.Error(t, err)
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	require.NoError(t, f.backend.client.Status().Update(t.Context(), task))
	_, err = f.backend.Resume(t.Context(), f.request, receipt)
	require.ErrorIs(t, err, contract.ErrConflict)
	_, handled, err := f.backend.ReconcileValidationTask(t.Context(), task)
	require.True(t, handled)
	require.NoError(t, err)
	require.Equal(t, []types.UID{f.jobs[0].UID}, f.deleteUIDs)
}

func TestCancellationStopsExactJobDespiteConfigurationDrift(t *testing.T) {
	f := newFixture(t, false)
	f.backend.readReport = func(context.Context, string, string) (pv.PreparationReport, error) {
		return pv.PreparationReport{}, errors.New("pause")
	}
	var receipt contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
	require.Error(t, err)
	f.backend.config.GeneralWorkerImage = "example/general@" + pv.Digest([]byte("new policy"))
	job := &batchv1.Job{}
	require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, job))
	job.Spec.Template.Spec.HostNetwork = true
	require.NoError(t, f.backend.client.Update(t.Context(), job))
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	require.Equal(t, []types.UID{f.jobs[0].UID}, f.deleteUIDs)
}
