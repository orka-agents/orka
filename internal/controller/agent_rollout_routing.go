/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

// AgentConditionRolloutInvalid is set on an Agent whose canary failed a
// check, so routing sends its Tasks to the stable Agent until a promoter
// aborts it or starts a new one.
const AgentConditionRolloutInvalid = "RolloutInvalid"

// Rollout arms a routed Task records.
const (
	rolloutArmStable    = "stable"
	rolloutArmCandidate = "candidate"
)

// AgentRolloutVerifier checks, before a Task is routed to a canary's
// candidate, that the canary still carries out an approval in force. A nil
// candidate with no error means the canary was aborted or replaced since
// the Agent was read: the Task runs on the stable Agent.
type AgentRolloutVerifier interface {
	VerifyAgentRollout(ctx context.Context, agent *corev1alpha1.Agent) (*corev1alpha1.Agent, error)
}

// VerifyAgentRollout returns the canary's candidate Agent when its decision
// is still the latest for its experiment and approves, the bound tuple
// matches, the Agent is the unchanged baseline, and the candidate is the
// one the canary names and the experiment ran. Any failed check is a
// *evals.RolloutRefusal.
func (r *EvalRunReconciler) VerifyAgentRollout(ctx context.Context, agent *corev1alpha1.Agent) (*corev1alpha1.Agent, error) {
	rollout := agent.Status.Rollout
	if rollout == nil || rollout.Candidate == nil {
		return nil, refuseRollout("Agent %q has no canary", agent.Name)
	}
	if r.ExperimentStore == nil {
		return nil, refuseRollout("canaries need the eval experiment store")
	}
	approved, err := r.approvedCandidate(ctx, agent.Namespace, agent.Name, rollout.DecisionID)
	if err != nil {
		return nil, err
	}
	// The routing decision used an Agent read that may predate an abort or a
	// new canary. Only the live rollout, read uncached by the check above,
	// may send a Task to a candidate.
	if live := approved.stable.Status.Rollout; live == nil || live.Candidate == nil || live.RecordID != rollout.RecordID ||
		live.DecisionID != rollout.DecisionID || live.Candidate.UID != rollout.Candidate.UID {
		return nil, nil
	}
	if approved.candidate.UID != rollout.Candidate.UID || approved.candidate.Generation != rollout.Candidate.Generation {
		return nil, refuseRollout("candidate Agent %q is not the one the canary started with", approved.candidate.Name)
	}
	return approved.candidate, nil
}

// rolloutBucket maps a routing key to 0..99.
func rolloutBucket(key string) int32 {
	sum := sha256.Sum256([]byte(key))
	return int32(binary.BigEndian.Uint64(sum[:8]) % 100)
}

// rolloutSessionLabel names a Session in a label value.
func rolloutSessionLabel(namespace, session string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + session))
	return hex.EncodeToString(sum[:])[:40]
}

// routeAgentRollout picks the Agent a Task that has not bound yet runs on
// while the Agent it names has a canary. A Task owned by an EvalRun is
// never routed: its trial must run on the Agent its run froze. A Task that
// continues a Session takes the arm the Session already has, and a Session
// that started before the canary stays on the stable Agent. A new Session,
// or a standalone Task, goes to the candidate when a hash of its name falls
// below the canary's percent, and only after the canary is verified;
// otherwise the Agent is marked RolloutInvalid and the Task runs on it.
func (r *TaskReconciler) routeAgentRollout(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent) (*corev1alpha1.Agent, error) {
	if agent == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Status.AgentExecutionBinding != nil {
		return agent, nil
	}
	rollout := agent.Status.Rollout
	if rollout == nil || rollout.Candidate == nil || rollout.Percent < 1 {
		return agent, nil
	}
	if owner := metav1.GetControllerOf(task); owner != nil && owner.Kind == "EvalRun" {
		return agent, nil
	}
	arm, err := r.rolloutArm(ctx, task, agent, rollout)
	if err != nil {
		return nil, err
	}
	routed := agent
	if arm == rolloutArmCandidate {
		candidate, err := r.verifyRollout(ctx, agent)
		if refusal, ok := errors.AsType[*evals.RolloutRefusal](err); ok {
			r.markRolloutInvalid(ctx, agent, refusal.Reason)
			arm = rolloutArmStable
		} else if err != nil {
			return nil, err
		} else if candidate == nil {
			arm = rolloutArmStable
		} else {
			routed = candidate
		}
	}
	if err := r.recordRolloutArm(ctx, task, arm, routed.Name); err != nil {
		return nil, err
	}
	if arm == rolloutArmCandidate {
		logf.FromContext(ctx).Info("Routed a Task to its Agent's canary", "agent", agent.Name, "candidate", routed.Name)
	}
	return routed, nil
}

func (r *TaskReconciler) verifyRollout(ctx context.Context, agent *corev1alpha1.Agent) (*corev1alpha1.Agent, error) {
	if r.RolloutVerifier == nil {
		return nil, refuseRollout("canaries need --enable-evals on the controller")
	}
	return r.RolloutVerifier.VerifyAgentRollout(ctx, agent)
}

// rolloutArm decides a Task's arm before verification.
func (r *TaskReconciler) rolloutArm(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent,
	rollout *corev1alpha1.AgentRolloutStatus) (string, error) {
	if task.Spec.SessionRef == nil {
		return bucketArm("task/"+string(task.UID), rollout.Percent), nil
	}
	session := task.Spec.SessionRef.Name
	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks, client.InNamespace(task.Namespace),
		client.MatchingLabels{labels.LabelRolloutSession: rolloutSessionLabel(task.Namespace, session)}); err != nil {
		return "", err
	}
	for i := range tasks.Items {
		earlier := &tasks.Items[i]
		if earlier.UID == task.UID || earlier.Spec.AgentRef == nil || earlier.Spec.AgentRef.Name != agent.Name {
			continue
		}
		// The Session keeps its arm; a candidate arm holds only while the
		// canary still routes to that candidate.
		if earlier.Annotations[labels.AnnotationRolloutArm] == rolloutArmCandidate &&
			earlier.Annotations[labels.AnnotationRolloutCandidate] == rollout.Candidate.Name {
			return rolloutArmCandidate, nil
		}
		return rolloutArmStable, nil
	}
	if r.SessionManager == nil {
		return rolloutArmStable, nil
	}
	if _, err := r.SessionManager.GetSession(ctx, task.Namespace, session); err == nil {
		return rolloutArmStable, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return bucketArm("session/"+task.Namespace+"/"+session, rollout.Percent), nil
}

func bucketArm(key string, percent int32) string {
	if rolloutBucket(key) < percent {
		return rolloutArmCandidate
	}
	return rolloutArmStable
}

// recordRolloutArm marks the Task with the arm it took, and its Session,
// so later Tasks of the Session take the same arm.
func (r *TaskReconciler) recordRolloutArm(ctx context.Context, task *corev1alpha1.Task, arm, agentName string) error {
	session := ""
	if task.Spec.SessionRef != nil {
		session = rolloutSessionLabel(task.Namespace, task.Spec.SessionRef.Name)
	}
	if task.Annotations[labels.AnnotationRolloutArm] == arm && task.Annotations[labels.AnnotationRolloutCandidate] == agentName &&
		task.Labels[labels.LabelRolloutSession] == session {
		return nil
	}
	patch := client.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationRolloutArm] = arm
	task.Annotations[labels.AnnotationRolloutCandidate] = agentName
	if session != "" {
		if task.Labels == nil {
			task.Labels = map[string]string{}
		}
		task.Labels[labels.LabelRolloutSession] = session
	}
	if err := r.Patch(ctx, task, patch); err != nil {
		return fmt.Errorf("record the Task's rollout arm: %w", err)
	}
	return nil
}

// markRolloutInvalid sets RolloutInvalid on the Agent. It is best effort:
// routing checks the canary again for every Task.
func (r *TaskReconciler) markRolloutInvalid(ctx context.Context, agent *corev1alpha1.Agent, reason string) {
	current := &corev1alpha1.Agent{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Name}, current); err != nil {
		return
	}
	changed := meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: AgentConditionRolloutInvalid, Status: metav1.ConditionTrue, Reason: "CanaryCheckFailed",
		Message: evalRunConditionMessage(reason), ObservedGeneration: current.Generation,
	})
	if !changed {
		return
	}
	if err := r.Status().Update(ctx, current); err != nil && !apierrors.IsConflict(err) {
		logf.FromContext(ctx).Error(err, "Failed to mark an Agent's canary invalid", "agent", agent.Name)
	}
}
