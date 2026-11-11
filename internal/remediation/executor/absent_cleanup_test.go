package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func absentFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, false)
	config := f.backend.config
	config.CleanupTimeout = time.Second
	read := f.backend.readReport
	backend, err := New(f.backend.client, f.backend.reader, f.backend.kubeClient, f.store, f.backend.scheme, config)
	require.NoError(t, err)
	backend.readReport = read
	f.backend = backend
	return f
}

func pauseAcceptedPreparation(t *testing.T, f *fixture, withJob bool) contract.ExecutionReceipt {
	t.Helper()
	var receipt contract.ExecutionReceipt
	f.backend.readReport = func(context.Context, string, string) (pv.PreparationReport, error) {
		return pv.PreparationReport{}, errors.New("pause after accepted work")
	}
	_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error {
		receipt = r
		if !withJob {
			return errors.New("pause before Job")
		}
		return nil
	})
	require.Error(t, err)
	require.NotEmpty(t, receipt.TaskUID)
	return receipt
}

func deleteFixtureTask(t *testing.T, f *fixture, name string) {
	t.Helper()
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.client.Get(t.Context(), client.ObjectKey{Namespace: f.request.Namespace, Name: name}, task))
	task.Finalizers = nil
	require.NoError(t, f.backend.client.Update(t.Context(), task))
	require.NoError(t, f.backend.client.Delete(t.Context(), task, client.Preconditions{UID: new(task.UID)}))
}

func TestAbsentPreparationAndParentDeletionRequireCleanupProof(t *testing.T) {
	for _, withJob := range []bool{false, true} {
		for _, withParent := range []bool{false, true} {
			f := absentFixture(t)
			receipt := pauseAcceptedPreparation(t, f, withJob)
			deleteFixtureTask(t, f, receipt.TaskName)
			if withParent {
				deleteFixtureTask(t, f, f.request.ParentTaskName)
			}
			require.NoError(t, f.backend.Cancel(t.Context(), receipt))
			require.NoError(t, f.backend.Cancel(t.Context(), receipt))
			fence := &corev1.ConfigMap{}
			require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKey{
				Namespace: receipt.Namespace, Name: pv.PreparationCancellationFenceName(receipt.TaskName)}, fence))
			require.Equal(t, receipt.TaskUID, fence.Data["taskUID"])
			require.Equal(t, receipt.InputDigest, fence.Data["inputDigest"])
			require.True(t, *fence.Immutable)
			require.Empty(t, fence.OwnerReferences, "parent GC must not remove the cancellation fence")
			require.True(t, apierrors.IsNotFound(f.backend.reader.Get(t.Context(), client.ObjectKey{
				Namespace: receipt.Namespace, Name: receipt.TaskName}, &batchv1.Job{})))
			require.True(t, apierrors.IsNotFound(f.backend.reader.Get(t.Context(), client.ObjectKey{
				Namespace: receipt.Namespace, Name: receipt.TaskName + "-input"}, &corev1.ConfigMap{})))
			before := len(f.jobs)
			_, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
			require.Error(t, err)
			require.Len(t, f.jobs, before)
			if withJob {
				require.Len(t, f.deleteUIDs, 1)
			}
		}
	}
}

func TestAbsentPreparationCleansTrackedValidationChildrenAfterParentGC(t *testing.T) {
	f := absentFixture(t)
	receipt := pauseAcceptedPreparation(t, f, false)
	input, _, err := f.backend.requestInput(f.request)
	require.NoError(t, err)
	input.TaskUID = receipt.TaskUID
	task := &corev1alpha1.Task{}
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task))
	submission, err := f.backend.validation.SubmitPrepared(t.Context(), task, input, fixtureReport(t, input))
	require.NoError(t, err)
	require.NoError(t, f.backend.validation.Advance(t.Context(), receipt.Namespace, submission.RequestID))
	submission, err = f.store.GetKubernetesValidationSubmission(t.Context(), receipt.Namespace, submission.RequestID)
	require.NoError(t, err)
	sideTask := &corev1alpha1.Task{}
	require.NoError(t, f.backend.reader.Get(t.Context(), client.ObjectKey{
		Namespace: receipt.Namespace, Name: submission.OriginalTaskName}, sideTask))
	job, err := f.backend.BuildValidationJob(t.Context(), sideTask)
	require.NoError(t, err)
	spec, err := json.Marshal(job.Spec)
	require.NoError(t, err)
	check := submission.Manifest.Checks[0]
	dispatch := pv.KubernetesDispatch{RunID: submission.RunID, Side: pv.Original, CheckID: check.ID,
		JobName: job.Name, SpecDigest: pv.Digest(spec), State: pv.DispatchPlanned}
	require.NoError(t, f.store.CreateKubernetesValidationDispatch(t.Context(), dispatch))
	require.NoError(t, f.backend.client.Create(t.Context(), job))
	require.NoError(t, f.store.MarkKubernetesValidationDispatchCreated(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, string(job.UID)))
	require.NoError(t, f.backend.client.Create(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: receipt.Namespace, Name: "unlabeled-validation-child",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}}}))
	deleteFixtureTask(t, f, submission.OriginalTaskName)
	deleteFixtureTask(t, f, receipt.TaskName)
	deleteFixtureTask(t, f, f.request.ParentTaskName)
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	require.Contains(t, f.deleteUIDs, job.UID)
	require.True(t, apierrors.IsNotFound(f.backend.reader.Get(t.Context(), client.ObjectKey{
		Namespace: receipt.Namespace, Name: "unlabeled-validation-child"}, &corev1.Pod{})))
	current, err := f.store.GetKubernetesValidationSubmission(t.Context(), receipt.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.Equal(t, pv.SubmissionTerminal, current.State)
	record, err := f.store.GetPatchVerificationRun(t.Context(), submission.RunID)
	require.NoError(t, err)
	require.Equal(t, pv.RunCancelled, record.State)
}

func TestAbsentPreparationRejectsForeignOrUnboundResources(t *testing.T) {
	for _, kind := range []string{"fence", "Job", "side Task", "Pod", "unlabeled Pod"} {
		t.Run(kind, func(t *testing.T) {
			f := absentFixture(t)
			f.backend.config.CleanupTimeout = 40 * time.Millisecond
			receipt := pauseAcceptedPreparation(t, f, false)
			deleteFixtureTask(t, f, receipt.TaskName)
			switch kind {
			case "fence":
				require.NoError(t, f.backend.client.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
					Namespace: receipt.Namespace, Name: pv.PreparationCancellationFenceName(receipt.TaskName)}, Immutable: new(true),
					Data: map[string]string{"taskUID": "foreign", "inputDigest": receipt.InputDigest}}))
			case "Job":
				job, err := f.backend.buildPreparationJob(&corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
					Namespace: receipt.Namespace, Name: receipt.TaskName, UID: "foreign"}})
				require.NoError(t, err)
				require.NoError(t, f.backend.client.Create(t.Context(), job))
			case "side Task":
				original, _ := pvkube.PreparedTaskNames(receipt.TaskUID, receipt.InputDigest)
				require.NoError(t, f.backend.client.Create(t.Context(), &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
					Namespace: receipt.Namespace, Name: original}}))
			case "Pod":
				require.NoError(t, f.backend.client.Create(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Namespace: receipt.Namespace, Name: "orphan-preparation", Labels: map[string]string{
						pv.ValidationTaskPrefix + "preparation": receipt.TaskUID}}}))
			case "unlabeled Pod":
				require.NoError(t, f.backend.client.Create(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Namespace: receipt.Namespace, Name: "unlabeled-orphan",
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: receipt.TaskName, UID: "orphan-job", Controller: new(true)}}}}))
			}
			require.Error(t, f.backend.Cancel(t.Context(), receipt))
			require.Empty(t, f.deleteUIDs)
		})
	}
}

func TestAbsentPreparationRejectsLateJobAndTaskReplacement(t *testing.T) {
	for _, replacement := range []string{"Job", "Task"} {
		t.Run(replacement, func(t *testing.T) {
			f := absentFixture(t)
			receipt := pauseAcceptedPreparation(t, f, false)
			deleteFixtureTask(t, f, receipt.TaskName)
			if replacement == "Task" {
				require.NoError(t, f.backend.client.Create(t.Context(), &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
					Namespace: receipt.Namespace, Name: receipt.TaskName, UID: "replacement"}}))
			} else {
				reads := 0
				f.backend.reader = interceptor.NewClient(f.backend.client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*batchv1.Job); ok && key.Name == receipt.TaskName {
							reads++
							if reads == 2 {
								job, err := f.backend.buildPreparationJob(&corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
									Namespace: receipt.Namespace, Name: receipt.TaskName, UID: types.UID(receipt.TaskUID)}})
								require.NoError(t, err)
								require.NoError(t, c.Create(ctx, job))
							}
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			}
			require.ErrorIs(t, f.backend.Cancel(t.Context(), receipt), contract.ErrConflict)
			require.Empty(t, f.deleteUIDs)
		})
	}
}
