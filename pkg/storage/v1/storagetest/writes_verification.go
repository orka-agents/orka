package storagetest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	pv "github.com/orka-agents/orka/pkg/storage/v1/verification"
	"github.com/stretchr/testify/require"
)

type verificationFixture struct {
	manifest   pv.Manifest
	binding    pv.Binding
	provenance map[string][]byte
	evidence   []pv.ExecutionEvidence
	submission pv.KubernetesSubmission
	dispatch   pv.KubernetesDispatch
}

func verificationJSON(value any) []byte {
	content, err := json.Marshal(value)
	if err != nil {
		panic("invalid static verification fixture")
	}
	return content
}

func catalogVerification() verificationFixture {
	digest := storagev1.CanonicalBytesDigest
	manifest := pv.Manifest{
		Version: pv.SchemaVersion, Problem: "reject invalid input", Scope: []string{"input handling"},
		Sources: pv.Sources{
			Repository: "/local/verification-fixture",
			Original: pv.SourceIdentity{Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40),
				ArchiveDigest: digest([]byte("original"))},
			Patched: pv.SourceIdentity{Commit: strings.Repeat("c", 40), Tree: strings.Repeat("d", 40),
				ArchiveDigest: digest([]byte("patched"))},
			DiffDigest: digest([]byte("diff")),
		},
		Environment: pv.Environment{Image: "example/tool@" + digest([]byte("image")),
			ImageID: digest([]byte("image-config")), Platform: "linux/amd64", Profile: pv.Offline},
		Checks: []pv.Check{
			{ID: "problem", Kind: pv.Reproduction, Command: []string{"/checks/run", "problem"},
				Healthy: pv.Expectation{Stdout: "rejected\n"}, Failure: pv.Expectation{Stdout: "accepted\n"}, TimeoutSeconds: 10},
			{ID: "normal", Kind: pv.Normal, Command: []string{"/checks/run", "normal"},
				Healthy: pv.Expectation{Stdout: "accepted\n"}, Failure: pv.Expectation{Stdout: "rejected\n"}, TimeoutSeconds: 10},
		},
	}
	binding := pv.Binding{AttemptID: "verification-attempt", OriginalTaskID: "original-task-uid",
		PatchedTaskID: "patched-task-uid", ManifestDigest: digest(verificationJSON(manifest))}
	binding.RunID = "pv-" + strings.TrimPrefix(digest(verificationJSON(binding)), "sha256:")
	fixture := verificationFixture{manifest: manifest, binding: binding, provenance: map[string][]byte{}}
	for _, value := range []string{"original", "patched", "diff"} {
		fixture.provenance[digest([]byte(value))] = []byte(value)
	}
	for _, side := range []string{pv.Original, pv.Patched} {
		for _, check := range manifest.Checks {
			expected := check.Healthy
			task, tree := binding.PatchedTaskID, manifest.Sources.Patched.Tree
			if side == pv.Original {
				task, tree = binding.OriginalTaskID, manifest.Sources.Original.Tree
				if check.Kind == pv.Reproduction {
					expected = check.Failure
				}
			}
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			observation := pv.Observation{
				RunID: binding.RunID, AttemptID: binding.AttemptID, TaskID: task, ManifestDigest: binding.ManifestDigest,
				Side: side, CheckID: check.ID, SourceTree: tree, ImageID: manifest.Environment.ImageID,
				ContainerID: "container-" + side + "-" + check.ID, JobUID: "verification-job", PodUID: "verification-pod",
				Origin: "runner", StartedAt: at, FinishedAt: at.Add(time.Second), Executed: true,
				ExitCode: new(expected.ExitCode), StdoutDigest: digest([]byte(expected.Stdout)),
				StdoutBytes: len(expected.Stdout), StderrDigest: digest(nil),
			}
			fixture.evidence = append(fixture.evidence, pv.ExecutionEvidence{Observation: observation,
				Blobs: map[string][]byte{observation.StdoutDigest: []byte(expected.Stdout), observation.StderrDigest: {}}})
		}
	}
	fixture.submission = pv.KubernetesSubmission{
		Namespace: "verification-tenant", RequestID: "verification-request", SubmittedBy: "verification-caller",
		AttemptID: binding.AttemptID, OriginalTaskName: "verification-original", PatchedTaskName: "verification-patched",
		Manifest: manifest, Provenance: fixture.provenance, State: pv.SubmissionPreparing,
	}
	fixture.dispatch = pv.KubernetesDispatch{RunID: binding.RunID, Side: pv.Original, CheckID: manifest.Checks[0].ID,
		JobName: "verification-job", SpecDigest: digest([]byte("job-spec")), State: pv.DispatchPlanned}
	return fixture
}

func initializeVerification(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	require.NoError(t, p.InitializeKubernetesValidationStore(t.Context()))
}

func createVerificationRun(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	initializeVerification(t, p)
	f := catalogVerification()
	require.NoError(t, p.CreatePatchVerificationRun(t.Context(), f.manifest, f.binding, f.provenance))
}

func createVerificationSubmission(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	initializeVerification(t, p)
	require.NoError(t, p.CreateKubernetesValidationSubmission(t.Context(), catalogVerification().submission))
}

func prepareVerificationBinding(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	createVerificationSubmission(t, p)
	f := catalogVerification()
	require.NoError(t, p.RecordKubernetesValidationTaskUID(t.Context(), f.submission.Namespace, f.submission.RequestID,
		pv.Original, f.binding.OriginalTaskID))
	require.NoError(t, p.RecordKubernetesValidationTaskUID(t.Context(), f.submission.Namespace, f.submission.RequestID,
		pv.Patched, f.binding.PatchedTaskID))
	require.NoError(t, p.CreatePatchVerificationRun(t.Context(), f.manifest, f.binding, f.provenance))
}

func bindVerificationSubmission(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	prepareVerificationBinding(t, p)
	f := catalogVerification()
	require.NoError(t, p.BindKubernetesValidationTasks(t.Context(), f.submission.Namespace, f.submission.RequestID,
		f.binding.OriginalTaskID, f.binding.PatchedTaskID, f.binding))
}

func planVerificationDispatch(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	bindVerificationSubmission(t, p)
	require.NoError(t, p.CreateKubernetesValidationDispatch(t.Context(), catalogVerification().dispatch))
}

func createVerificationDispatch(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	planVerificationDispatch(t, p)
	f := catalogVerification()
	require.NoError(t, p.MarkKubernetesValidationDispatchCreated(t.Context(), f.binding.RunID, pv.Original,
		f.dispatch.CheckID, f.evidence[0].Observation.JobUID))
}

func observeVerificationDispatch(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	f := catalogVerification()
	value, err := p.GetKubernetesValidationDispatch(t.Context(), f.binding.RunID, pv.Original, f.dispatch.CheckID)
	return observedVerification(t, value, err)
}

func observedVerification(t *testing.T, value any, err error) any {
	t.Helper()
	if errors.Is(err, pv.ErrRunNotFound) {
		return "not found"
	}
	require.NoError(t, err)
	return value
}

func observeVerificationRun(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	value, err := p.GetPatchVerificationRun(t.Context(), catalogVerification().binding.RunID)
	return observedVerification(t, value, err)
}

func observeVerificationSubmission(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	f := catalogVerification()
	value, err := p.GetKubernetesValidationSubmission(t.Context(), f.submission.Namespace, f.submission.RequestID)
	return observedVerification(t, value, err)
}

func init() {
	f := catalogVerification()
	stateWriteCases = append(stateWriteCases,
		writeCase[storagev1.StateProvider]{
			method: "CreatePatchVerificationRun", setup: initializeVerification,
			args: func(storagev1.ControllerEpochFence) []any {
				f := catalogVerification()
				return []any{f.manifest, f.binding, f.provenance}
			}, observe: observeVerificationRun,
		},
		writeCase[storagev1.StateProvider]{
			method: "GetPatchVerificationRun", setup: createVerificationRun,
			args: fixed(f.binding.RunID), observe: observeVerificationRun,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				f := catalogVerification()
				require.NoError(t, p.RecordPatchVerificationEvidence(t.Context(), f.binding, f.evidence[0]))
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "GetPatchVerificationBlob", setup: createVerificationRun,
			args: fixed(f.binding.RunID, f.manifest.Sources.Original.ArchiveDigest), observe: observeVerificationRun,
		},
		writeCase[storagev1.StateProvider]{
			method: "RecordPatchVerificationEvidence", setup: createVerificationRun,
			args: func(storagev1.ControllerEpochFence) []any {
				f := catalogVerification()
				return []any{f.binding, f.evidence[0]}
			}, observe: observeVerificationRun,
		},
		writeCase[storagev1.StateProvider]{
			method: "FinalizePatchVerificationRun",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				createVerificationRun(t, p)
				f := catalogVerification()
				for _, evidence := range f.evidence {
					require.NoError(t, p.RecordPatchVerificationEvidence(t.Context(), f.binding, evidence))
				}
			}, args: fixed(f.binding), observe: observeVerificationRun,
		},
		writeCase[storagev1.StateProvider]{
			method: "CancelPatchVerificationRun", setup: createVerificationRun, args: fixed(f.binding), observe: observeVerificationRun,
		},
		writeCase[storagev1.StateProvider]{
			method: "RecoverInterruptedPatchVerificationRun", setup: createVerificationRun, args: fixed(f.binding), observe: observeVerificationRun,
		},
		writeCase[storagev1.StateProvider]{
			method: "CreateKubernetesValidationSubmission", setup: initializeVerification,
			args:    func(storagev1.ControllerEpochFence) []any { return []any{catalogVerification().submission} },
			observe: observeVerificationSubmission,
		},
		writeCase[storagev1.StateProvider]{
			method: "CompleteKubernetesValidationSubmission", setup: createVerificationSubmission,
			args: fixed(f.submission.Namespace, f.submission.RequestID, "setup-unavailable"), observe: observeVerificationSubmission,
		},
		writeCase[storagev1.StateProvider]{
			method: "RecordKubernetesValidationTaskUID", setup: createVerificationSubmission,
			args: fixed(f.submission.Namespace, f.submission.RequestID, pv.Original, f.binding.OriginalTaskID), observe: observeVerificationSubmission,
		},
		writeCase[storagev1.StateProvider]{
			method: "BindKubernetesValidationTasks", setup: prepareVerificationBinding,
			args:    fixed(f.submission.Namespace, f.submission.RequestID, f.binding.OriginalTaskID, f.binding.PatchedTaskID, f.binding),
			observe: observeVerificationSubmission,
		},
		writeCase[storagev1.StateProvider]{
			method: "AttachCancelledKubernetesValidationRun",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				prepareVerificationBinding(t, p)
				f := catalogVerification()
				require.NoError(t, p.RequestKubernetesValidationCancellation(t.Context(), f.submission.Namespace, f.submission.RequestID))
				_, err := p.CancelPatchVerificationRun(t.Context(), f.binding)
				require.NoError(t, err)
			}, args: fixed(f.submission.Namespace, f.submission.RequestID, f.binding), observe: observeVerificationSubmission,
		},
		writeCase[storagev1.StateProvider]{
			method: "RequestKubernetesValidationCancellation", setup: createVerificationSubmission,
			args: fixed(f.submission.Namespace, f.submission.RequestID), observe: observeVerificationSubmission,
		},
		writeCase[storagev1.StateProvider]{
			method: "CreateKubernetesValidationDispatch", setup: bindVerificationSubmission,
			args: func(storagev1.ControllerEpochFence) []any { return []any{catalogVerification().dispatch} }, observe: observeVerificationDispatch,
		},
		writeCase[storagev1.StateProvider]{
			method: "MarkKubernetesValidationDispatchCreated", setup: planVerificationDispatch,
			args: fixed(f.binding.RunID, pv.Original, f.dispatch.CheckID, f.evidence[0].Observation.JobUID), observe: observeVerificationDispatch,
		},
		writeCase[storagev1.StateProvider]{
			method: "MarkKubernetesValidationDispatchObserved", setup: createVerificationDispatch,
			args: fixed(f.binding.RunID, pv.Original, f.dispatch.CheckID, f.evidence[0].Observation.JobUID,
				f.evidence[0].Observation.PodUID, f.evidence[0].Observation.ContainerID), observe: observeVerificationDispatch,
		},
		writeCase[storagev1.StateProvider]{
			method: "MarkKubernetesValidationDispatchRecorded",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				createVerificationDispatch(t, p)
				f := catalogVerification()
				obs := f.evidence[0].Observation
				require.NoError(t, p.MarkKubernetesValidationDispatchObserved(t.Context(), f.binding.RunID,
					pv.Original, f.dispatch.CheckID, obs.JobUID, obs.PodUID, obs.ContainerID))
				require.NoError(t, p.RecordPatchVerificationEvidence(t.Context(), f.binding, f.evidence[0]))
			}, args: fixed(f.binding.RunID, pv.Original, f.dispatch.CheckID, f.evidence[0].Observation.JobUID,
				f.evidence[0].Observation.PodUID), observe: observeVerificationDispatch,
		},
		writeCase[storagev1.StateProvider]{
			method: "MarkKubernetesValidationDispatchFailed", setup: planVerificationDispatch,
			args: fixed(f.binding.RunID, pv.Original, f.dispatch.CheckID, "worker-unavailable"), observe: observeVerificationDispatch,
		},
	)
}
