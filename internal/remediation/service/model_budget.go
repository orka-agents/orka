package service

import (
	"context"
	"encoding/json"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

var _ contract.ModelCallBudgetService = (*Coordinator)(nil)

// ClaimModelCall permanently consumes one outbound-attempt permit. Replays,
// including the same Pod after an uncertain acknowledgement, are denied. A
// failed send may waste a permit but can never recreate it.
func (s *Coordinator) ClaimModelCall(ctx context.Context, claim contract.ModelCallClaim) error {
	if claim.Namespace != s.config.Namespace || claim.TaskName == "" || claim.TaskUID == "" ||
		claim.JobUID == "" || claim.PodUID == "" {
		return contract.ErrForbidden
	}
	r, task, err := s.loadModelCallRun(ctx, claim)
	if err != nil {
		return err
	}
	ctx = bindRunIdentity(ctx, runIdentity{namespace: claim.Namespace, id: r.state.ID, uid: r.object.UID})
	_, err = s.update(ctx, r.state.ID, func(current *run) error {
		if err := s.requireActiveModelCallRun(ctx, current); err != nil {
			return err
		}
		role := task.Labels[roleLabel]
		op, err := modelCallReservation(current, role, claim)
		if err != nil {
			return err
		}
		if err := s.requireModelCallWorker(ctx, current, role, claim, task, op); err != nil {
			return err
		}
		op.ModelCall = &modelCallClaim{JobUID: claim.JobUID, PodUID: claim.PodUID}
		return nil
	})
	return err
}

func (s *Coordinator) loadModelCallRun(
	ctx context.Context, claim contract.ModelCallClaim,
) (*run, *corev1alpha1.Task, error) {
	task := new(corev1alpha1.Task)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.TaskName}, task); err != nil {
		return nil, nil, contract.ErrUnavailable
	}
	if string(task.UID) != claim.TaskUID || task.Status.JobUID != claim.JobUID || task.Labels[runLabel] == "" {
		return nil, nil, contract.ErrForbidden
	}
	object := new(corev1.ConfigMap)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: task.Labels[runLabel]}, object); err != nil {
		return nil, nil, contract.ErrUnavailable
	}
	var saved state
	if len(object.Data["state"]) > maxStateBytes || json.Unmarshal([]byte(object.Data["state"]), &saved) != nil {
		return nil, nil, contract.ErrConflict
	}
	r, err := decodeRun(object, saved.ID, claim.Namespace)
	if err != nil {
		return nil, nil, err
	}
	return r, task, nil
}

func (s *Coordinator) requireActiveModelCallRun(ctx context.Context, r *run) error {
	if !r.state.Authority.BoundedExecution || r.state.CancelRequested ||
		r.state.Final != nil || contract.TerminalPhase(r.state.Status.Phase) ||
		!time.Now().Before(r.state.Deadline) {
		return contract.ErrForbidden
	}
	if err := s.requireRunSlot(ctx, r); err != nil {
		return err
	}
	parent := new(corev1alpha1.Task)
	if r.state.Parent.UID == "" || s.config.Reader.Get(ctx, client.ObjectKey{
		Namespace: r.state.Namespace, Name: r.state.Parent.Name,
	}, parent) != nil {
		return contract.ErrUnavailable
	}
	if string(parent.UID) != r.state.Parent.UID || parent.DeletionTimestamp != nil ||
		parent.Annotations[labels.AnnotationCancelRequested] != "" || parent.Status.Phase != corev1alpha1.TaskPhaseRunning {
		return contract.ErrForbidden
	}
	return nil
}

func modelCallReservation(r *run, role string, claim contract.ModelCallClaim) (*taskOperation, error) {
	op := r.state.Agents[role]
	if role == parentRole || op == nil || !op.Attempted || op.UID != claim.TaskUID ||
		op.Name != claim.TaskName || op.ModelCall != nil {
		return nil, contract.ErrConflict
	}
	used := 0
	for _, other := range r.state.Agents {
		if other.ModelCall != nil {
			used++
		}
	}
	if used >= contract.MaxModelCalls {
		return nil, contract.ErrConflict
	}
	return op, nil
}

func (s *Coordinator) requireModelCallWorker(
	ctx context.Context, r *run, role string, claim contract.ModelCallClaim,
	task *corev1alpha1.Task, op *taskOperation,
) error {
	expected := new(corev1alpha1.Task)
	if err := s.readJSON(ctx, r, op.Spec, expected); err != nil {
		return err
	}
	if err := s.config.Reader.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		return contract.ErrUnavailable
	}
	if !taskMatches(task, expected, claim.TaskUID) || task.Status.JobUID != claim.JobUID ||
		(task.Status.Phase != corev1alpha1.TaskPhaseRunning && task.Status.Phase != corev1alpha1.TaskPhasePending) {
		return contract.ErrForbidden
	}
	if task.Status.Phase == corev1alpha1.TaskPhasePending {
		return contract.ErrUnavailable
	}
	identity, err := s.agentAuthority(ctx)
	if err != nil || !authorityMatches(r.state.Authority, identity) {
		return contract.ErrConflict
	}
	job := new(batchv1.Job)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: task.Status.JobName}, job); err != nil {
		return contract.ErrUnavailable
	}
	if !boundedNativeJob(job, task, r, role) {
		return contract.ErrForbidden
	}
	return nil
}

func boundedNativeJob(job *batchv1.Job, task *corev1alpha1.Task, r *run, role string) bool {
	owner := metav1.GetControllerOf(job)
	if job.UID == "" || string(job.UID) != task.Status.JobUID || job.DeletionTimestamp != nil ||
		owner == nil || owner.UID != task.UID || owner.Name != task.Name ||
		owner.Kind != taskKind || owner.APIVersion != corev1alpha1.GroupVersion.String() ||
		job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 ||
		job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever ||
		(job.Spec.Completions != nil && *job.Spec.Completions != 1) ||
		(job.Spec.Parallelism != nil && *job.Spec.Parallelism != 1) {
		return false
	}
	workers := 0
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name != "worker" {
			continue
		}
		workers++
		if container.Image != r.state.Authority.AIWorkerImage {
			return false
		}
		for name, value := range map[string]string{
			contract.BoundedModelRequestsEnv: "1",
			contract.ModelBudgetRunEnv:       string(r.object.UID),
			contract.ModelBudgetRoleEnv:      role,
			contract.ModelBudgetDeadlineEnv:  r.state.Deadline.Format(time.RFC3339Nano),
			"ORKA_MEMORY_TOOLS_AUTO_ENABLE":  boolFalse,
			"ORKA_MEMORY_CONTEXT_ENABLED":    boolFalse,
		} {
			found := 0
			for _, variable := range container.Env {
				if variable.Name == name {
					found++
					if variable.ValueFrom != nil || variable.Value != value {
						return false
					}
				}
			}
			if found != 1 {
				return false
			}
		}
	}
	return workers == 1
}

func validModelCallClaim(r *run, op *taskOperation) bool {
	return !r.state.Authority.BoundedExecution ||
		(op != nil && op.ModelCall != nil && op.ModelCall.PodUID != "" && op.Job != nil && op.ModelCall.JobUID == op.Job.UID)
}

func (s *Coordinator) completedModelCall(ctx context.Context, r *run, role string, task *corev1alpha1.Task) error {
	if validModelCallClaim(r, r.state.Agents[role]) {
		return nil
	}
	// A worker can consume its permit after the driver loads the run, then
	// finish before the Task read. Re-read before declaring that claim absent.
	current, err := s.load(ctx, r.state.ID)
	if err != nil {
		return err
	}
	op := current.state.Agents[role]
	if current.object.UID != r.object.UID || !authorityMatches(r.state.Authority, current.state.Authority) ||
		op == nil || op.Job == nil || op.UID != string(task.UID) || op.Job.UID != task.Status.JobUID ||
		!validModelCallClaim(current, op) {
		return uncertain()
	}
	return contract.ErrUnavailable
}
