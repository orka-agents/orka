package applicationexec

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRuntimeScopeRequiresMeasuredQualificationAndSettlement(t *testing.T) {
	f := newFixture(t)
	require.Empty(t, f.backend.EvidenceScope())
	_, err := f.backend.RequestDigest(f.request)
	require.ErrorIs(t, err, contract.ErrEvidenceScopeUnsupported)
	require.NoError(t, f.backend.CheckReadiness(t.Context()))
	require.Equal(t, contract.ApplicationRuntime, f.backend.EvidenceScope())
	require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
	require.NoError(t, EnsureNoActive(t.Context(), f.control, ""))
	var probes int
	for _, object := range f.created {
		if job, ok := object.(*batchv1.Job); ok && job.Spec.Template.Spec.Containers[0].Name == "boundary" {
			probes++
			require.Empty(t, job.OwnerReferences, "a control Task cannot own a target Job")
			require.NotEmpty(t, job.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution)
			require.Empty(t, job.Spec.Template.Spec.NodeName, "WaitForFirstConsumer must retain scheduler binding")
		}
	}
	require.Equal(t, 6, probes)
	require.True(t, apierrors.IsNotFound(f.target.Get(t.Context(), client.ObjectKey{Namespace: "kube-system", Name: "orka-application-execution"}, &coordinationv1.Lease{})))
}

func TestQualificationRejectsUnprovenControls(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*pv.ApplicationBoundaryReport)
	}{
		{"dead-positive-control", func(r *pv.ApplicationBoundaryReport) { r.CanaryReachable = false }},
		{"network-policy-not-enforced", func(r *pv.ApplicationBoundaryReport) { r.CanaryReachable = true }},
		{"foreign-serviceaccount", func(r *pv.ApplicationBoundaryReport) { r.APIUserUID = "other-sa" }},
		{"unauthenticated-api", func(r *pv.ApplicationBoundaryReport) { r.APIStatuses["self-identity"] = 401 }},
		{"secret-authority", func(r *pv.ApplicationBoundaryReport) { r.APIStatuses["secrets"] = 200 }},
		{"unbounded-pids", func(r *pv.ApplicationBoundaryReport) { r.PIDsMax = 0 }},
		{"privileged-capability", func(r *pv.ApplicationBoundaryReport) { r.Capabilities["CapEff"] = "0000000000000001" }},
		{"writable-root", func(r *pv.ApplicationBoundaryReport) { r.ReadOnlyRoot = false }},
		{"unbound-pod", func(r *pv.ApplicationBoundaryReport) { r.PodUID = "replacement" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.reportHook = func(mode string, report any) {
				if mode == "boundary" {
					test.change(report.(*pv.ApplicationBoundaryReport))
				}
			}
			require.Error(t, f.backend.CheckReadiness(t.Context()))
			require.Empty(t, f.backend.EvidenceScope())
			require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
		})
	}
}

func TestStartResumeAndComparisonUseRealTaskUIDsAndSealedStore(t *testing.T) {
	for _, action := range []pv.Action{pv.ValidateReport, pv.VerifyPatch} {
		t.Run(string(action), func(t *testing.T) {
			f := newFixture(t)
			require.NoError(t, f.backend.CheckReadiness(t.Context()))
			f.request.Action = action
			if action == pv.VerifyPatch {
				f.request.Patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"
				f.request.DeclaredChanges = []pv.DeclaredChange{{Kind: "source", Description: "generic reconciliation correction", Paths: []string{"main.go"}}}
			}
			var receipt contract.ExecutionReceipt
			record, err := f.backend.Start(t.Context(), f.request, func(value contract.ExecutionReceipt) error {
				receipt = value
				return nil
			})
			require.NoError(t, err)
			require.NotNil(t, record)
			require.Equal(t, pv.RunFinalized, record.State)
			require.NotNil(t, record.Seal)
			if action == pv.ValidateReport {
				require.Equal(t, pv.Reproduced, record.Assessment.Conclusion)
			} else {
				require.Equal(t, pv.Verified, record.Assessment.Conclusion)
				require.NotEqual(t, record.Binding.OriginalTaskID, record.Binding.PatchedTaskID)
			}
			require.Len(t, record.Evidence, len(pv.ActionSides(action))*2)
			r, err := f.backend.boundRun(t.Context(), receipt)
			require.NoError(t, err)
			require.True(t, r.state.Settled)
			require.True(t, r.state.Finished)
			require.Equal(t, string(r.state.ArmTasks[pv.Original].UID), record.Binding.OriginalTaskID)
			require.Equal(t, f.request.Deadline.UTC(), r.state.Deadline.UTC())
			for _, evidence := range record.Evidence {
				require.NoError(t, pv.ValidateObservation(record.Manifest, record.Binding, evidence.Observation))
				require.NotEqual(t, evidence.Observation.PodUID, evidence.Observation.Application.Subject.PodUID)
				require.NotEqual(t, evidence.Observation.ContainerID, evidence.Observation.Application.Subject.ContainerID)
				require.NotEmpty(t, evidence.Observation.Application.Target.NamespaceUID)
				require.NotEmpty(t, evidence.Observation.Application.Artifact.Build.JobUID)
				require.NotEmpty(t, evidence.Observation.Application.Subject.ServiceAccountUID)
			}
			before := len(f.created)
			resumed, err := f.backend.Resume(t.Context(), f.request, receipt)
			require.NoError(t, err)
			require.Equal(t, record, resumed)
			require.Len(t, f.created, before, "Resume must not create another attempt")
			for _, entry := range r.state.Effects {
				require.True(t, entry.Deleted, entry.Key)
			}
			for _, volume := range r.state.Volumes {
				require.True(t, volume.Released)
			}
			require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
		})
	}
}

func TestAcceptancePrecedesEveryTargetEffectAndInputIsFrozen(t *testing.T) {
	f := newFixture(t)
	f.backend.qualified.Store(true)
	var receipt contract.ExecutionReceipt
	rejected := errors.New("receipt persistence failed")
	_, err := f.backend.Start(t.Context(), f.request, func(value contract.ExecutionReceipt) error {
		receipt = value
		return rejected
	})
	require.ErrorIs(t, err, rejected)
	require.Empty(t, f.created)
	require.ErrorIs(t, EnsureNoActive(t.Context(), f.control, "control"), ErrActive)
	request := f.request
	record, err := f.backend.Resume(t.Context(), request, receipt)
	require.NoError(t, err)
	require.NotNil(t, record)

	second := newFixture(t)
	second.backend.qualified.Store(true)
	originalArgs := append([]string(nil), second.request.Checks.Application.Runtime.Args...)
	record, err = second.backend.Start(t.Context(), second.request, func(value contract.ExecutionReceipt) error {
		second.request.Checks.Application.Runtime.Args = []string{"unaccepted caller mutation"}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, originalArgs, record.Manifest.Application.Runtime.Args)
}

func TestCancellationIsDurableBeforeAndAfterParentDeletion(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, false))
	parent := &corev1alpha1.Task{}
	require.NoError(t, f.control.Get(t.Context(), client.ObjectKey{Namespace: "control", Name: "parent"}, parent))
	require.NoError(t, f.control.Delete(t.Context(), parent))
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
	require.NoError(t, f.backend.Cancel(t.Context(), r.receipt()))
	retained, err := f.backend.boundRun(t.Context(), r.receipt())
	require.NoError(t, err)
	require.True(t, retained.state.Cancelled)
	require.True(t, retained.state.Settled)
	_, err = f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
	require.ErrorIs(t, err, contract.ErrConflict)
	require.NotEmpty(t, retained.state.Effects, "failed/cancelled attempts retain their receipts")
}

func TestParentRevocationPreventsNewEffects(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	parent := &corev1alpha1.Task{}
	require.NoError(t, f.control.Get(t.Context(), client.ObjectKey{Namespace: "control", Name: "parent"}, parent))
	parent.Annotations = map[string]string{labels.AnnotationCancelRequested: ""}
	require.NoError(t, f.control.Update(t.Context(), parent))
	require.ErrorIs(t, f.backend.ensureClaim(t.Context(), r), contract.ErrConflict)
	require.Empty(t, f.created)
	require.NoError(t, f.backend.ReconcileCleanup(t.Context()))
	retained, err := f.backend.boundRun(t.Context(), r.receipt())
	require.NoError(t, err)
	require.True(t, retained.state.Cancelled)
	require.True(t, retained.state.Settled)
}

func TestExpiredRequestNeverCreatesAndJobDeadlineDoesNotReset(t *testing.T) {
	f := newFixture(t)
	f.backend.qualified.Store(true)
	f.request.Deadline = new(time.Now().Add(-time.Second))
	_, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error { return nil })
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, f.created)
	var noEffects interface{ NoEffects() bool }
	require.ErrorAs(t, err, &noEffects)
	require.True(t, noEffects.NoEffects())

	f.request.Deadline = new(time.Now().UTC().Add(20 * time.Second))
	r := f.newRun()
	f.autoPods = false
	require.NoError(t, f.backend.ensureNamespace(t.Context(), r, pv.Original))
	options := jobOptions{role: "build", mode: "build", image: f.backend.config.ToolchainImage, taskUID: string(r.state.Task.UID), serviceAccount: "worker", deadline: 600}
	first, err := f.backend.ensureJob(t.Context(), r, pv.Original, "deadline", struct{}{}, options)
	require.NoError(t, err)
	require.LessOrEqual(t, *first.Spec.ActiveDeadlineSeconds, int64(20))
	second, err := f.backend.ensureJob(t.Context(), r, pv.Original, "deadline", struct{}{}, options)
	require.NoError(t, err)
	require.Equal(t, first.UID, second.UID)
	require.Equal(t, *first.Spec.ActiveDeadlineSeconds, *second.Spec.ActiveDeadlineSeconds)
	require.Equal(t, *first.Spec.ActiveDeadlineSeconds, r.effect(resourceKey(pv.Original, "deadline")).DeadlineSeconds)
}

func TestCompletedReceiptRejectsReplacedControlIdentity(t *testing.T) {
	for _, which := range []string{"parent", "execution-task"} {
		t.Run(which, func(t *testing.T) {
			f := newFixture(t)
			f.backend.qualified.Store(true)
			var receipt contract.ExecutionReceipt
			_, err := f.backend.Start(t.Context(), f.request, func(value contract.ExecutionReceipt) error { receipt = value; return nil })
			require.NoError(t, err)
			name := receipt.TaskName
			if which == "parent" {
				name = "parent"
			}
			task := &corev1alpha1.Task{}
			require.NoError(t, f.control.Get(t.Context(), client.ObjectKey{Namespace: "control", Name: name}, task))
			require.NoError(t, f.control.Delete(t.Context(), task))
			task.UID, task.ResourceVersion = types.UID("replaced-"+which), ""
			task.CreationTimestamp = metav1.Time{}
			require.NoError(t, f.control.Create(t.Context(), task))
			_, err = f.backend.Resume(t.Context(), f.request, receipt)
			require.ErrorIs(t, err, contract.ErrConflict)
		})
	}
}

func TestChangedEvidenceCannotBecomeRuntimeProof(t *testing.T) {
	tests := []struct {
		name   string
		change func(*pv.ApplicationReport)
	}{
		{"source", func(r *pv.ApplicationReport) { r.Evidence.Observation.Application.Artifact.Source.Tree = "wrong" }},
		{"artifact", func(r *pv.ApplicationReport) {
			r.Evidence.Observation.Application.Artifact.BinaryDigest = pv.Digest([]byte("other"))
		}},
		{"subject-pod", func(r *pv.ApplicationReport) { r.Evidence.Observation.Application.Subject.PodUID = "replacement" }},
		{"subject-container", func(r *pv.ApplicationReport) {
			r.Evidence.Observation.Application.Subject.ContainerID = "containerd://other"
		}},
		{"target-namespace", func(r *pv.ApplicationReport) { r.Evidence.Observation.Application.Target.NamespaceUID = "replacement" }},
		{"target-cluster", func(r *pv.ApplicationReport) {
			r.Evidence.Observation.Application.Target.ClusterUID = "control-cluster"
		}},
		{"observer-pod", func(r *pv.ApplicationReport) { r.PodUID = "subject" }},
		{"subject-attestation", func(r *pv.ApplicationReport) { r.Evidence.Observation.Origin = "runner" }},
		{"resource-replacement", func(r *pv.ApplicationReport) { r.Evidence.Observation.Application.Kubernetes.UID = "replacement" }},
		{"incomplete", func(r *pv.ApplicationReport) { r.Evidence.Observation.Application.Completed = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			// Cleanup safety here is not a one-second benchmark under race instrumentation.
			f.backend.config.CleanupTimeout = 10 * time.Second
			f.backend.qualified.Store(true)
			f.reportHook = func(mode string, report any) {
				if mode == "observe" {
					test.change(report.(*pv.ApplicationReport))
				}
			}
			var receipt contract.ExecutionReceipt
			record, err := f.backend.Start(t.Context(), f.request, func(value contract.ExecutionReceipt) error { receipt = value; return nil })
			require.Error(t, err)
			require.Nil(t, record)
			retained, err := f.backend.boundRun(t.Context(), receipt)
			require.NoError(t, err)
			require.NotEmpty(t, retained.state.Failed)
			require.NotEmpty(t, retained.state.Effects)
			require.False(t, reflect.DeepEqual(retained.state.Artifacts[pv.Original], pv.ApplicationArtifact{}))
			require.NoError(t, f.backend.Cancel(t.Context(), receipt))
		})
	}
}
