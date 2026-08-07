/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const adjudicatedSessionCleanupDigestDomain = "agent-execution-adjudicated-session-cleanup/v1"

// adjudicatedBlockedSessionDeletionReady consumes only the exact Applied
// resolution referenced by a blocked RuntimeSessionControl. The blocked
// evidence is never cleared: after route cleanup reaches its durable barriers,
// the exact blocked control is captured in a cleanup intent and deleted.
func (r *TaskReconciler) adjudicatedBlockedSessionDeletionReady(
	ctx context.Context,
	task *corev1alpha1.Task,
) (bool, error) {
	if task == nil || task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) == "" {
		return true, nil
	}
	if r.SessionManager == nil {
		return false, errors.New("session manager is required for adjudicated blocked Session cleanup")
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if reader == nil {
		return false, errors.New("kubernetes reader is required for adjudicated blocked Session cleanup")
	}
	sessionName := strings.TrimSpace(task.Spec.SessionRef.Name)
	control := &corev1alpha1.RuntimeSessionControl{}
	err := reader.Get(ctx, client.ObjectKey{
		Namespace: task.Namespace,
		Name:      storekube.RuntimeSessionControlObjectName(sessionName),
	}, control)
	if apierrors.IsNotFound(err) {
		return r.SessionManager.resumeSessionCleanupForTaskFinalizer(ctx, task.Namespace, sessionName)
	}
	if err != nil {
		return false, fmt.Errorf("read Task Session control for adjudicated cleanup: %w", err)
	}
	if control.Spec.SessionName != sessionName {
		return false, errors.New("task Session reference does not match the authoritative control")
	}
	if control.Status.Availability != corev1alpha1.RuntimeSessionControlAvailability(store.SessionReconciliationBlocked) {
		return true, nil
	}
	ref := control.Status.AgentExecutionResolutionRef
	if ref == nil {
		return false, nil
	}
	adjudication, err := r.appliedBlockedSessionAdjudication(ctx, reader, task, control, ref)
	if err != nil {
		return false, err
	}
	ready, err := r.adjudicatedSessionRouteCleanupReady(ctx, task, adjudication.Spec.Action)
	if err != nil || !ready {
		return ready, err
	}
	if err := r.SessionManager.reclaimAdjudicatedSession(ctx, control, ref); err != nil {
		return false, err
	}
	return true, nil
}

func (r *TaskReconciler) appliedBlockedSessionAdjudication(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	control *corev1alpha1.RuntimeSessionControl,
	ref *corev1alpha1.AgentExecutionResolutionRef,
) (*corev1alpha1.AgentExecutionAdjudication, error) {
	if err := validateAgentExecutionResolutionRefDigest(task.Namespace, ref); err != nil {
		return nil, fmt.Errorf("validate Session adjudication resolution reference: %w", err)
	}
	adjudication := &corev1alpha1.AgentExecutionAdjudication{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: ref.AdjudicationName}, adjudication); err != nil {
		return nil, fmt.Errorf("read referenced Session adjudication: %w", err)
	}
	if adjudication.Status.State != corev1alpha1.AgentExecutionAdjudicationApplied ||
		!resolutionRefMatchesAdjudication(ref, adjudication) ||
		adjudication.Status.ResolutionRefDigest != ref.ResolutionDigest {
		return nil, errors.New("session resolution does not match the exact Applied adjudication")
	}
	if adjudication.Status.OperationID != agentExecutionAdjudicationOperationID(adjudication) ||
		adjudication.Status.ResultingSubjectResourceVersion != control.ResourceVersion ||
		adjudication.Status.ObservedAt == nil ||
		!adjudication.Status.ObservedAt.Time.Equal(ref.AppliedAt.Time) {
		return nil, errors.New("applied Session adjudication is missing its exact subject-write receipt")
	}
	if err := store.ValidateCanonicalDigest(
		"applied Session adjudication operation digest", adjudication.Status.OperationDigest,
	); err != nil {
		return nil, err
	}
	if adjudication.Spec.TaskRef.Name != task.Name || adjudication.Spec.TaskRef.UID != task.UID ||
		adjudication.Spec.SessionRef == nil || adjudication.Spec.SessionRef.Name != control.Spec.SessionName ||
		adjudication.Spec.SessionRef.UID != control.UID {
		return nil, errors.New("applied adjudication does not name the exact Task and blocked Session")
	}
	blockedDigest, err := canonicalAgentExecutionBlockedStateDigest(control)
	if err != nil {
		return nil, err
	}
	if adjudication.Spec.BlockedStateDigest != blockedDigest {
		return nil, errors.New("applied adjudication does not match the immutable blocked Session evidence")
	}
	subject := &agentExecutionAdjudicationSubject{
		kind: agentExecutionAdjudicationSubjectSession, task: task, session: control,
	}
	if reason := validateAgentExecutionCleanupAction(adjudication, subject); reason != "" {
		return nil, errors.New(reason)
	}
	return adjudication, nil
}

func (r *TaskReconciler) adjudicatedSessionRouteCleanupReady(
	ctx context.Context,
	task *corev1alpha1.Task,
	action corev1alpha1.AgentExecutionAdjudicationAction,
) (bool, error) {
	cleanupV1 := action == corev1alpha1.AgentExecutionAdjudicationCleanupV1 ||
		action == corev1alpha1.AgentExecutionAdjudicationCleanupBoth
	cleanupV2 := action == corev1alpha1.AgentExecutionAdjudicationCleanupV2 ||
		action == corev1alpha1.AgentExecutionAdjudicationCleanupBoth
	if cleanupV1 {
		ready, err := r.adjudicatedHarnessV1TaskDeletionReady(ctx, task)
		if err != nil || !ready {
			return ready, err
		}
	}
	if cleanupV2 {
		if r.DurableControlStore == nil {
			return false, errors.New("durable ACP control store is required for adjudicated Session cleanup")
		}
		ready, err := r.acpTaskDeletionReady(ctx, task)
		if err != nil || !ready {
			return ready, err
		}
		reclaimed, err := r.reclaimACPTaskPublicationBundles(ctx, task)
		if err != nil || !reclaimed {
			return reclaimed, err
		}
		retired, err := r.retireACPArtifactIdentities(ctx, task)
		if err != nil || !retired {
			return retired, err
		}
	}
	return true, nil
}

func (m *SessionManager) reclaimAdjudicatedSession(
	ctx context.Context,
	control *corev1alpha1.RuntimeSessionControl,
	ref *corev1alpha1.AgentExecutionResolutionRef,
) error {
	if m == nil || m.cleanupStore == nil || m.epochs == nil {
		return errors.New("durable Session cleanup and controller epoch source are required")
	}
	fence, err := m.epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	operationID := store.CanonicalControlID(
		"adjudicated-session-cleanup", control.Namespace, control.Spec.SessionName, ref.ResolutionDigest,
	)
	operationDigest, err := acpDomainDigest(adjudicatedSessionCleanupDigestDomain, map[string]any{
		"namespace": control.Namespace, "sessionName": control.Spec.SessionName,
		"sessionUID": control.Spec.SessionUID, "controlObjectUID": control.UID,
		"controlResourceVersion": control.ResourceVersion, "resolutionDigest": ref.ResolutionDigest,
		"operationID": operationID,
	})
	if err != nil {
		return err
	}
	return m.cleanupStore.ReclaimSession(ctx, store.ReclaimSessionRequest{
		Namespace: control.Namespace, SessionName: control.Spec.SessionName, Fence: fence,
		OperationID: operationID, OperationDigest: operationDigest, RequestedAt: ref.AppliedAt.Time,
		Adjudication: &store.SessionCleanupAdjudicationFence{
			ControlObjectUID: string(control.UID), ControlResourceVersion: control.ResourceVersion,
			ResolutionDigest: ref.ResolutionDigest,
		},
	})
}

func (m *SessionManager) resumeSessionCleanupForTaskFinalizer(
	ctx context.Context,
	namespace string,
	sessionName string,
) (bool, error) {
	if m == nil || m.cleanupStore == nil || m.epochs == nil {
		return true, nil
	}
	persistence, ok := m.store.(store.SessionCleanupPersistenceStore)
	if !ok {
		return false, errors.New("session cleanup persistence is required to verify missing control state")
	}
	if _, err := persistence.GetSessionCleanupCompletion(ctx, namespace, sessionName); err == nil {
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	intent, err := persistence.GetSessionCleanupIntent(ctx, namespace, sessionName)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	fence, err := m.epochs.CurrentFence(ctx)
	if err != nil {
		return false, err
	}
	if err := m.cleanupStore.ReclaimSession(ctx, store.ReclaimSessionRequest{
		Namespace: intent.Namespace, SessionName: intent.SessionName, Fence: fence,
		OperationID: intent.OperationID, OperationDigest: intent.OperationDigest, RequestedAt: intent.PreparedAt,
	}); err != nil {
		return false, err
	}
	return true, nil
}
