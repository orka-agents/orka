package kube

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func testService(t *testing.T, assignUID bool) (*Service, *sqlite.Store) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{})
	if assignUID {
		counter := 0
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			counter++
			object.SetUID(types.UID("api-uid-" + string(rune('0'+counter))))
			object.SetCreationTimestamp(metav1.Now())
			return c.Create(ctx, object, options...)
		}})
	}
	controllerClient := builder.Build()
	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := sqlite.NewStore(db, ":memory:")
	digest := strings.Repeat("a", 64)
	service, err := New(controllerClient, controllerClient, kubernetesfake.NewSimpleClientset(), store, scheme, Config{
		Enabled: true, Namespace: "default", HelperImage: "example/helper@sha256:" + digest,
		HelperImageID: pv.Digest([]byte("helper-config")),
		ToolImage:     "example/tool@sha256:" + digest, ToolImageID: "containerd://example/tool@sha256:" + digest,
		Platform: "linux/amd64", Profile: pv.Offline,
		NodeSelectorKey: "orka.ai/validation-sandbox", NodeSelectorValue: pv.Offline,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.SetJobRenderer(service.BuildValidationJob)
	return service, store
}

func testManifest(t *testing.T, service *Service) (pv.Manifest, map[string][]byte) {
	t.Helper()
	original, patched, diff := []byte("original"), []byte("patched"), []byte("diff")
	manifest := pv.Manifest{Version: pv.SchemaVersion, Action: pv.VerifyPatch, Problem: "unsafe input", Scope: []string{"input"},
		DeclaredChanges: []pv.DeclaredChange{{Kind: "source", Description: "repair unsafe input"}},
		Sources: pv.Sources{Repository: "/input/repository", Original: pv.SourceIdentity{Commit: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40), ArchiveDigest: pv.Digest(original)},
			Patched: pv.SourceIdentity{Commit: strings.Repeat("d", 40), Tree: strings.Repeat("e", 40), ArchiveDigest: pv.Digest(patched)}, DiffDigest: pv.Digest(diff)},
		Environment: pv.Environment{Image: service.config.ToolImage, ImageID: service.config.ToolImageID,
			Platform: service.config.Platform, Profile: pv.Offline, Dependencies: map[string]string{"orka.kubernetes.policy": pv.KubernetesPolicyVersion, helperImageKey: service.config.HelperImage, helperImageIDKey: service.config.HelperImageID}},
		Checks: []pv.Check{{ID: "check", Kind: pv.Reproduction, Command: []string{"/checks/check"},
			Healthy: pv.Expectation{Stdout: "safe\n"}, Failure: pv.Expectation{Stdout: "unsafe\n"}, TimeoutSeconds: 10},
			{ID: "normal", Kind: pv.Normal, Command: []string{"/checks/normal"}, Healthy: pv.Expectation{Stdout: "normal\n"}, Failure: pv.Expectation{Stdout: "broken\n"}, TimeoutSeconds: 10}}}
	manifest.ReportDigest, _ = pv.StableReportDigest(manifest.Problem, manifest.Scope)
	if err := pv.ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}
	return manifest, map[string][]byte{pv.Digest(original): original, pv.Digest(patched): patched, pv.Digest(diff): diff}
}

func TestSubmitUsesKubernetesAssignedTaskUIDsAndRendersHardenedJob(t *testing.T) {
	service, _ := testService(t, true)
	manifest, provenance := testManifest(t, service)
	submission, err := service.Submit(t.Context(), "default", "user", manifest, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.prepareSubmission(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	submission, err = service.GetSubmission(t.Context(), "default", submission.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if submission.Binding.OriginalTaskID != "api-uid-1" || submission.Binding.PatchedTaskID != "api-uid-2" {
		t.Fatalf("binding used non-API UIDs: %#v", submission.Binding)
	}
	task := &corev1alpha1.Task{}
	if err := service.reader.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: submission.OriginalTaskName}, task); err != nil {
		t.Fatal(err)
	}
	job, err := service.BuildValidationJob(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ServiceAccountName != "" {
		t.Fatalf("service account token exposure: %#v", pod)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("validation must use runtime-default, not require modified nodes: %#v", pod.SecurityContext)
	}
	container := pod.Containers[0]
	if container.Image != service.config.ToolImage || container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("unhardened validation container: %#v", container)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != task.UID {
		t.Fatalf("Job owner identity = %#v", job.OwnerReferences)
	}
}

func TestPreparationFailsWhenAPIDoesNotAssignTaskUID(t *testing.T) {
	service, store := testService(t, false)
	manifest, provenance := testManifest(t, service)
	submission, err := service.Submit(t.Context(), "default", "user", manifest, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.prepareSubmission(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetKubernetesValidationSubmission(t.Context(), "default", submission.RequestID)
	if err != nil || current.State != pv.SubmissionTerminal || current.Binding != nil || current.Failure == "" {
		t.Fatalf("missing Task UID must fail preparation: submission=%+v error=%v", current, err)
	}
}

func boundSubmission(t *testing.T, service *Service, action pv.Action) *pv.KubernetesSubmission {
	t.Helper()
	manifest, provenance := testManifest(t, service)
	if action == pv.ValidateReport {
		manifest.Action = action
		manifest.DeclaredChanges = nil
		manifest.Sources.Patched = pv.SourceIdentity{}
		manifest.Sources.DiffDigest = ""
		provenance = map[string][]byte{manifest.Sources.Original.ArchiveDigest: []byte("original")}
	}
	submission, err := service.Submit(t.Context(), "default", "user", manifest, provenance)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.prepareSubmission(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	submission, err = service.GetSubmission(t.Context(), "default", submission.RequestID)
	if err != nil || submission.Binding == nil {
		t.Fatalf("bind submission: %v", err)
	}
	return submission
}

func recordSide(t *testing.T, store *sqlite.Store, submission *pv.KubernetesSubmission, side string, reproduce bool) {
	t.Helper()
	identity, sourceTree := submission.OriginalTaskUID, submission.Manifest.Sources.Original.Tree
	if side == pv.Patched {
		identity, sourceTree = submission.PatchedTaskUID, submission.Manifest.Sources.Patched.Tree
	}
	for _, check := range submission.Manifest.Checks {
		dispatch, err := store.GetKubernetesValidationDispatch(t.Context(), submission.RunID, side, check.ID)
		if errors.Is(err, pv.ErrRunNotFound) {
			dispatch = &pv.KubernetesDispatch{RunID: submission.RunID, Side: side, CheckID: check.ID,
				JobName: validationJobName(submission.RunID, side, check.ID), SpecDigest: pv.Digest([]byte("fixture")), State: pv.DispatchPlanned}
			if err := store.CreateKubernetesValidationDispatch(t.Context(), *dispatch); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if dispatch.JobUID == "" {
			dispatch.JobUID = "job-" + side + "-" + check.ID
			if err := store.MarkKubernetesValidationDispatchCreated(t.Context(), submission.RunID, side, check.ID, dispatch.JobUID); err != nil {
				t.Fatal(err)
			}
		}
		podUID, containerID := "pod-"+side+"-"+check.ID, "containerd://"+side+"-"+check.ID
		if err := store.MarkKubernetesValidationDispatchObserved(t.Context(), submission.RunID, side, check.ID, dispatch.JobUID, podUID, containerID); err != nil {
			t.Fatal(err)
		}
		output := []byte(check.Healthy.Stdout)
		if reproduce && check.Kind == pv.Reproduction {
			output = []byte(check.Failure.Stdout)
		}
		start := time.Now().UTC()
		exitCode := 0
		evidence := pv.ExecutionEvidence{Observation: pv.Observation{
			RunID: submission.RunID, AttemptID: submission.AttemptID, TaskID: identity,
			ManifestDigest: submission.Binding.ManifestDigest, Side: side, CheckID: check.ID, SourceTree: sourceTree,
			ImageID: submission.Manifest.Environment.ImageID, ContainerID: containerID, JobUID: dispatch.JobUID, PodUID: podUID,
			Origin: "runner", StartedAt: start, FinishedAt: start.Add(time.Second), Executed: true, ExitCode: &exitCode,
			StdoutDigest: pv.Digest(output), StdoutBytes: len(output), StderrDigest: pv.Digest(nil),
		}, Blobs: map[string][]byte{pv.Digest(output): output, pv.Digest(nil): {}}}
		if err := store.RecordPatchVerificationEvidence(t.Context(), *submission.Binding, evidence); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkKubernetesValidationDispatchRecorded(t.Context(), submission.RunID, side, check.ID, dispatch.JobUID, podUID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerificationWaitsForBothArms(t *testing.T) {
	service, store := testService(t, true)
	submission := boundSubmission(t, service, pv.VerifyPatch)
	recordSide(t, store, submission, pv.Original, true)
	if err := service.reconcileSubmission(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetPatchVerificationRun(t.Context(), submission.RunID)
	if err != nil || record.Seal != nil || record.State != pv.RunRunning {
		t.Fatalf("original arm prematurely finalized the run: record=%+v error=%v", record, err)
	}
	recordSide(t, store, submission, pv.Patched, false)
	if err := service.reconcileSubmission(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	record, err = store.GetPatchVerificationRun(t.Context(), submission.RunID)
	if err != nil || record.Seal == nil || record.Assessment.Conclusion != pv.Verified || len(record.Evidence) != 4 {
		t.Fatalf("complete comparison was not verified: record=%+v error=%v", record, err)
	}
}

func TestBaselineFailureDoesNotCreatePatchedJob(t *testing.T) {
	service, store := testService(t, true)
	submission := boundSubmission(t, service, pv.VerifyPatch)
	recordSide(t, store, submission, pv.Original, false)
	if err := service.reconcileSubmission(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetPatchVerificationRun(t.Context(), submission.RunID)
	if err != nil || record.Assessment.Conclusion != pv.UnableToVerify || record.Seal == nil {
		t.Fatalf("unsuitable baseline must seal Unable: %+v %v", record, err)
	}
	jobs := &batchv1.JobList{}
	if err := service.reader.List(t.Context(), jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("baseline failure dispatched Jobs: %v %v", jobs, err)
	}
}

func TestReportRepeatCancelAndEvidenceSurviveCleanup(t *testing.T) {
	service, store := testService(t, true)
	first := boundSubmission(t, service, pv.ValidateReport)
	second := boundSubmission(t, service, pv.ValidateReport)
	if first.RequestID == second.RequestID || first.RunID == second.RunID {
		t.Fatal("independent reports reused identities")
	}
	recordSide(t, store, first, pv.Original, true)
	if err := service.reconcileSubmission(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileSubmission(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	before, err := service.Evidence(t.Context(), "default", first.RequestID)
	if err != nil || before.Assessment.Conclusion != pv.Reproduced {
		t.Fatalf("report evidence: %+v %v", before, err)
	}
	if err := service.Cancel(t.Context(), "default", first.RequestID); err != nil {
		t.Fatal(err)
	}
	after, err := service.Evidence(t.Context(), "default", first.RequestID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("cancelling a completed report changed evidence")
	}
	if err := service.Cancel(t.Context(), "default", second.RequestID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := service.GetSubmission(t.Context(), "default", second.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileSubmission(t.Context(), cancelled); err != nil {
		t.Fatal(err)
	}
	record, err := service.Evidence(t.Context(), "default", second.RequestID)
	if err != nil || record.State != pv.RunCancelled || record.Assessment.Conclusion != pv.UnableToValidate {
		t.Fatalf("cancelled report: %+v %v", record, err)
	}
	task := &corev1alpha1.Task{}
	if err := service.reader.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: second.OriginalTaskName}, task); err != nil ||
		len(task.Finalizers) != 0 || task.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		t.Fatalf("cancel cleanup did not settle Task: %+v %v", task, err)
	}
}

func TestPreparingSubmissionResumesAfterTaskCreation(t *testing.T) {
	service, _ := testService(t, true)
	manifest, provenance := testManifest(t, service)
	submission, err := service.Submit(t.Context(), "default", "user", manifest, provenance)
	if err != nil {
		t.Fatal(err)
	}
	task := service.validationTask(submission, pv.Original, submission.OriginalTaskName)
	if err := service.client.Create(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := service.prepareSubmission(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	resumed, err := service.GetSubmission(t.Context(), "default", submission.RequestID)
	if err != nil || resumed.OriginalTaskUID != string(task.UID) || resumed.State != pv.SubmissionRunning {
		t.Fatalf("restart did not recover exact created Task: %+v %v", resumed, err)
	}
}

func TestPlannedDispatchResumesExactJobWithoutReplay(t *testing.T) {
	service, store := testService(t, true)
	submission := boundSubmission(t, service, pv.VerifyPatch)
	task := &corev1alpha1.Task{}
	if err := service.reader.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: submission.OriginalTaskName}, task); err != nil {
		t.Fatal(err)
	}
	check := submission.Manifest.Checks[0]
	job, err := service.buildCheckJob(submission, task, check)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(job.Spec)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := pv.KubernetesDispatch{RunID: submission.RunID, Side: pv.Original, CheckID: check.ID,
		JobName: job.Name, SpecDigest: pv.Digest(spec), State: pv.DispatchPlanned}
	if err := store.CreateKubernetesValidationDispatch(t.Context(), dispatch); err != nil {
		t.Fatal(err)
	}
	if err := service.client.Create(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if err := service.resumePlannedJob(t.Context(), submission, task, check, &dispatch); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetKubernetesValidationDispatch(t.Context(), submission.RunID, pv.Original, check.ID)
	if err != nil || current.JobUID != string(job.UID) || current.State != pv.DispatchCreated {
		t.Fatalf("restart lost Job identity: %+v %v", current, err)
	}
}

func TestDisabledServiceDoesNotInterceptOrdinaryTasks(t *testing.T) {
	var disabled *Service
	if _, handled, err := disabled.ReconcileValidationTask(t.Context(), &corev1alpha1.Task{}); handled || err != nil {
		t.Fatalf("disabled service changed an ordinary Task: handled=%t error=%v", handled, err)
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{pv.ValidationBackendKey: pv.KubernetesBackend}}}
	if _, handled, err := disabled.ReconcileValidationTask(t.Context(), task); !handled || err == nil {
		t.Fatalf("disabled service allowed standalone validation: handled=%t error=%v", handled, err)
	}
}

func TestBoundTaskCannotEscapeByRemovingAnnotations(t *testing.T) {
	service, _ := testService(t, true)
	submission := boundSubmission(t, service, pv.ValidateReport)
	task := &corev1alpha1.Task{}
	if err := service.reader.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: submission.OriginalTaskName}, task); err != nil {
		t.Fatal(err)
	}
	task.Annotations = nil
	if _, handled, err := service.ReconcileValidationTask(t.Context(), task); err != nil || !handled {
		t.Fatalf("bound Task escaped to generic execution: handled=%t error=%v", handled, err)
	}
}

func TestCancellationCleansExactJobAfterLabelRemoval(t *testing.T) {
	service, _ := testService(t, true)
	submission := boundSubmission(t, service, pv.ValidateReport)
	if err := service.reconcileSubmission(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := service.reader.List(t.Context(), jobs); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("expected one dispatched Job: %v", err)
	}
	job := &jobs.Items[0]
	job.Labels = nil
	if err := service.client.Update(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if err := service.Cancel(t.Context(), "default", submission.RequestID); err != nil {
		t.Fatal(err)
	}
	current, err := service.GetSubmission(t.Context(), "default", submission.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileSubmission(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err := service.reader.List(t.Context(), jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("cleanup relied on mutable Job labels: remaining=%d error=%v", len(jobs.Items), err)
	}
}
