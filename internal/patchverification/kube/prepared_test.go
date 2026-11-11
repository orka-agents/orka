package kube

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func preparedFixture(t *testing.T, s *Service) (*corev1alpha1.Task, pv.PreparationInput, pv.PreparationReport) {
	t.Helper()
	s.config.Profile = pv.LocalServices
	s.config.LocalServices = true
	s.config.PreparationConfiguration = pv.Digest([]byte("fixture authority"))
	manifest, _ := testManifest(t, s)
	manifest.Action, manifest.DeclaredChanges = pv.ValidateReport, nil
	manifest.Sources = pv.Sources{Repository: "https://github.com/example/fixture", Original: pv.SourceIdentity{
		Commit: manifest.Sources.Original.Commit, Tree: manifest.Sources.Original.Tree}}
	manifest.Environment.Profile = pv.LocalServices
	manifest.Environment.Dependencies[pv.PreparationPolicyKey] = pv.PreparationPolicy
	manifest.Environment.Dependencies[pv.PreparationConfigurationKey] = s.config.PreparationConfiguration
	healthy, err := pv.HTTPExpectation(200, "good")
	require.NoError(t, err)
	failure, err := pv.HTTPExpectation(500, "bad")
	require.NoError(t, err)
	file := pv.FrozenFile{Path: "server", Content: []byte("#!/bin/sh\nexec /usr/bin/server /src\n"), Executable: true}
	file.Digest = pv.Digest(file.Content)
	manifest.Files = []pv.FrozenFile{file}
	manifest.Checks = []pv.Check{{ID: "check", Kind: pv.Reproduction, Healthy: healthy, Failure: failure, TimeoutSeconds: 2,
		HTTP: &pv.HTTPCheck{Version: pv.HTTPCheckVersion, ServerCommand: []string{"/checks/server"}, Path: "/"}}}
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: s.config.Namespace}}
	require.NoError(t, s.client.Create(t.Context(), parent))
	input := pv.PreparationInput{Version: pv.PreparationProtocolVersion, ID: "run", Namespace: s.config.Namespace,
		ParentTaskName: parent.Name, ParentTaskUID: string(parent.UID), Manifest: manifest}
	digest, err := pv.PreparationInputDigest(input)
	require.NoError(t, err)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "prepared-task", Namespace: parent.Namespace,
		Annotations: map[string]string{pv.PreparationInputAnnotation: digest},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task",
			Name: parent.Name, UID: parent.UID, Controller: new(true), BlockOwnerDeletion: new(true)}}}}
	require.NoError(t, s.client.Create(t.Context(), task))
	input.TaskUID = string(task.UID)
	report := pv.PreparationReport{Version: pv.PreparationProtocolVersion, TaskUID: string(task.UID), InputDigest: digest, Manifest: manifest,
		Provenance: map[string][]byte{pv.Digest([]byte("archive")): []byte("archive"), file.Digest: file.Content}}
	report.Manifest.Sources.Original.ArchiveDigest = pv.Digest([]byte("archive"))
	require.NoError(t, pv.ValidatePreparationReport(input, report))
	return task, input, report
}

func TestPreparedSubmissionHandoffIsIdempotentAndParentBound(t *testing.T) {
	s, store := testService(t, true)
	task, input, report := preparedFixture(t, s)
	first, err := s.SubmitPrepared(t.Context(), task, input, report)
	require.NoError(t, err)
	second, err := s.SubmitPrepared(t.Context(), task, input, report)
	require.NoError(t, err)
	require.Equal(t, first.RequestID, second.RequestID)
	require.NoError(t, s.Advance(t.Context(), first.Namespace, first.RequestID))
	bound, err := store.GetKubernetesValidationSubmission(t.Context(), first.Namespace, first.RequestID)
	require.NoError(t, err)
	require.NotNil(t, bound.Binding)
	side := &corev1alpha1.Task{}
	require.NoError(t, s.reader.Get(t.Context(), client.ObjectKey{Namespace: bound.Namespace, Name: bound.OriginalTaskName}, side))
	require.True(t, metav1.IsControlledBy(side, task))
	require.Equal(t, string(side.UID), bound.Binding.OriginalTaskID)
	replaced := task.DeepCopy()
	replaced.UID = "other"
	_, err = s.SubmitPrepared(t.Context(), replaced, input, report)
	require.Error(t, err)
}

func TestPreparedTaskAdmissionFenceForbidsReplacement(t *testing.T) {
	s, store := testService(t, true)
	task, input, report := preparedFixture(t, s)
	submission, err := s.SubmitPrepared(t.Context(), task, input, report)
	require.NoError(t, err)
	require.NoError(t, s.reservePreparedTask(t.Context(), submission, submission.OriginalTaskName))
	require.NoError(t, s.Advance(t.Context(), submission.Namespace, submission.RequestID))
	stored, err := store.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.Equal(t, pv.SubmissionTerminal, stored.State)
	require.Empty(t, stored.OriginalTaskUID)
	err = s.reader.Get(t.Context(), client.ObjectKey{Namespace: submission.Namespace, Name: submission.OriginalTaskName}, &corev1alpha1.Task{})
	require.True(t, apierrors.IsNotFound(err))
}

func TestPreparedJobAdmissionFenceForbidsReplacement(t *testing.T) {
	s, store := testService(t, true)
	prep, input, report := preparedFixture(t, s)
	submission, err := s.SubmitPrepared(t.Context(), prep, input, report)
	require.NoError(t, err)
	require.NoError(t, s.Advance(t.Context(), submission.Namespace, submission.RequestID))
	submission, err = store.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	task := &corev1alpha1.Task{}
	require.NoError(t, s.reader.Get(t.Context(), client.ObjectKey{Namespace: submission.Namespace, Name: submission.OriginalTaskName}, task))
	check := submission.Manifest.Checks[0]
	job, err := s.buildCheckJob(submission, task, check)
	require.NoError(t, err)
	spec, err := json.Marshal(job.Spec)
	require.NoError(t, err)
	dispatch := pv.KubernetesDispatch{RunID: submission.RunID, Side: pv.Original, CheckID: check.ID,
		JobName: job.Name, SpecDigest: pv.Digest(spec), State: pv.DispatchPlanned}
	require.NoError(t, store.CreateKubernetesValidationDispatch(t.Context(), dispatch))
	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-input", Namespace: job.Namespace}, Immutable: new(true)}
	require.NoError(t, s.OwnObject(task, marker))
	require.NoError(t, s.client.Create(t.Context(), marker))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	s.config.CanaryHost = host
	s.config.CanaryPort, err = strconv.Atoi(port)
	require.NoError(t, err)
	require.NoError(t, s.resumePlannedJob(t.Context(), submission, task, check, &dispatch))
	stored, err := store.GetKubernetesValidationDispatch(t.Context(), submission.RunID, pv.Original, check.ID)
	require.NoError(t, err)
	require.Equal(t, pv.DispatchFailed, stored.State)
	require.Empty(t, stored.JobUID)
	err = s.reader.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})
	require.True(t, apierrors.IsNotFound(err))
}

func TestPreparedParentReplacementPreventsDispatch(t *testing.T) {
	s, store := testService(t, true)
	task, input, report := preparedFixture(t, s)
	submission, err := s.SubmitPrepared(t.Context(), task, input, report)
	require.NoError(t, err)
	parent := &corev1alpha1.Task{}
	require.NoError(t, s.client.Get(t.Context(), client.ObjectKey{Namespace: input.Namespace, Name: input.ParentTaskName}, parent))
	parent.UID = "replacement"
	require.NoError(t, s.client.Update(t.Context(), parent))
	require.NoError(t, s.Advance(t.Context(), submission.Namespace, submission.RequestID))
	stored, err := store.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.Equal(t, pv.SubmissionTerminal, stored.State)
	require.Empty(t, stored.OriginalTaskUID)
}

func TestAdvanceLockWaitHonorsCancellation(t *testing.T) {
	s, _ := testService(t, true)
	s.reconcileGate <- struct{}{}
	defer func() { <-s.reconcileGate }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.ErrorIs(t, s.Advance(ctx, "default", "request"), context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)
}
