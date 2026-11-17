package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Like stoppedAncestor, cross-namespace delegation uses a protected UID and
// namespace annotation; a controller owner always identifies a local parent.
func gatewayPassParent(ctx context.Context, reader client.Reader, child *corev1alpha1.Task) (*corev1alpha1.Task, error) {
	name := labels.ParentTaskName(child.Labels, child.Annotations)
	uid := strings.TrimSpace(child.Annotations[labels.AnnotationParentTaskUID])
	namespace := strings.TrimSpace(child.Annotations[labels.AnnotationParentTaskNamespace])
	if namespace == "" {
		namespace = child.Namespace
	}
	if owner := taskControllerOwner(child); owner != nil {
		if name == "" {
			name = owner.Name
		}
		if owner.Name != name || (uid != "" && uid != string(owner.UID)) || namespace != child.Namespace {
			return nil, errDelegationChainBroken
		}
		uid = string(owner.UID)
	}
	if name == "" || uid == "" {
		return nil, errDelegationChainBroken
	}
	parent := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errDelegationChainBroken
		}
		return nil, err
	}
	if string(parent.UID) != uid || !sameRequester(parent.Spec.RequestedBy, child.Spec.RequestedBy) {
		return nil, errDelegationChainBroken
	}
	return parent, nil
}

func loadGatewayPassSnapshot(ctx context.Context, reader client.Reader, snapshots store.AgentExecutionSnapshotStore, task *corev1alpha1.Task) (*gatewayruntime.TaskGrant, error) {
	binding := task.Status.AgentExecutionBinding
	if binding == nil || snapshots == nil {
		return nil, store.ErrNotReady
	}
	digest, err := canonicalAgentExecutionBindingDigest(*binding)
	if err != nil || digest != binding.BindingDigest || binding.Task.UID != task.UID || binding.Task.BoundSpecGeneration != task.Generation {
		return nil, errors.New("gateway task binding changed")
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: task.Namespace}, namespace); err != nil {
		return nil, err
	}
	if namespace.UID != binding.Task.NamespaceUID {
		return nil, errors.New("gateway task namespace changed")
	}
	snapshot, err := snapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: string(task.UID), Digest: binding.Snapshot.Digest})
	if err != nil {
		return nil, err
	}
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := validateAgentExecutionSnapshot(binding, snapshot, body); err != nil {
		return nil, err
	}
	return body.GatewayGrant, nil
}

type gatewayPassAncestor struct {
	task  *corev1alpha1.Task
	grant *gatewayruntime.TaskGrant
}

// child is nil for the caller itself. Only ancestors may retain authority
// after a successful turn, and only while waiting for that exact child UID.
func gatewayPassActive(task, child *corev1alpha1.Task) bool {
	if _, cancelled := task.Annotations[labels.AnnotationCancelRequested]; cancelled || !task.DeletionTimestamp.IsZero() {
		return false
	}
	if task.Status.Phase == corev1alpha1.TaskPhaseRunning && task.Status.ExecutionOutcome == nil {
		return true
	}
	if _, cancelled := taskWaitCancellation(task); cancelled {
		return false
	}
	wait, execution := task.Status.Wait, task.Status.Execution
	if child == nil || child.Namespace != task.Namespace || task.Status.Phase != corev1alpha1.TaskPhaseSucceeded || wait == nil || execution == nil ||
		wait.State != corev1alpha1.TaskWaitStateWaiting || wait.Kind != corev1alpha1.TaskWaitKindTasks || wait.Attempt != execution.Attempt || wait.PromptID != execution.PromptID ||
		execution.State != corev1alpha1.TaskExecutionStateSucceeded || execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded ||
		!wait.Deadline.After(time.Now()) || !taskWaitChainDeadline(task).After(time.Now()) {
		return false
	}
	for i, name := range wait.References {
		if name == child.Name && i < len(wait.ReferenceUIDs) && wait.ReferenceUIDs[i] == string(child.UID) {
			return true
		}
	}
	return false
}

// Walk only frozen parent references and compare each against live protected
// provenance. Live Agent policies never replace a parent's frozen authority.
func gatewayPassChain(ctx context.Context, reader client.Reader, snapshots store.AgentExecutionSnapshotStore, service *gatewayruntime.Service, task *corev1alpha1.Task, grant *gatewayruntime.TaskGrant, tool string, child *corev1alpha1.Task) ([]gatewayPassAncestor, error) {
	fail := errors.New("gateway task pass is unavailable")
	var chain []gatewayPassAncestor
	seen := map[string]bool{}
	for range maxDelegationChainDepth {
		live := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(task), live); err != nil {
			return nil, err
		}
		if live.UID != task.UID || seen[string(live.UID)] || grant == nil || !grant.Deadline.After(time.Now()) || (tool != "" && !slices.Contains(grant.Tools, tool)) || !gatewayPassActive(live, child) {
			return nil, fail
		}
		seen[string(live.UID)] = true
		// Continuations have their own UID but no gateway event or child grant.
		if live.Annotations[labels.AnnotationContinuationOfUID] != "" {
			return nil, fail
		}
		verified, err := loadGatewayPassSnapshot(ctx, reader, snapshots, live)
		if err != nil {
			return nil, err
		}
		if verified == nil || !sameGatewayPassGrant(verified, grant) {
			return nil, fail
		}
		chain = append(chain, gatewayPassAncestor{live, verified})
		if grant.Parent == nil {
			if namesDelegatingTask(live) || service == nil {
				return nil, fail
			}
			// At freeze there is no tool call yet. A parked parent retains
			// delegation origin only; root calls keep their existing liveness.
			if tool == "" || live.Status.Phase != corev1alpha1.TaskPhaseRunning {
				err = service.ValidateParentTaskGrantOrigin(ctx, live, grant)
			} else {
				err = service.ValidateTaskGrant(ctx, live, grant, tool)
			}
			if err != nil {
				return nil, err
			}
			return chain, nil
		}
		parent, err := gatewayPassParent(ctx, reader, live)
		if err != nil {
			return nil, err
		}
		ref := grant.Parent
		if ref.UID != string(parent.UID) || ref.Name != parent.Name || ref.Namespace != parent.Namespace || parent.Status.AgentExecutionBinding == nil || ref.SnapshotDigest != parent.Status.AgentExecutionBinding.Snapshot.Digest {
			return nil, fail
		}
		inherited, err := loadGatewayPassSnapshot(ctx, reader, snapshots, parent)
		if err != nil {
			return nil, err
		}
		if inherited == nil || !gatewayPassAttenuated(grant, inherited) {
			return nil, fail
		}
		child, task, grant = live, parent, inherited
	}
	return nil, fmt.Errorf("%w: delegation depth exceeded", fail)
}

func gatewayPassAttenuated(child, parent *gatewayruntime.TaskGrant) bool {
	if child.GrantRef != parent.GrantRef || child.NamespaceUID != parent.NamespaceUID || child.GatewayUID != parent.GatewayUID || child.Endpoint != parent.Endpoint || child.ServiceRef != parent.ServiceRef || child.Deadline.After(parent.Deadline) {
		return false
	}
	for _, tool := range child.Tools {
		if !slices.Contains(parent.Tools, tool) {
			return false
		}
	}
	return true
}

func sameGatewayPassGrant(a, b *gatewayruntime.TaskGrant) bool {
	if !gatewayPassAttenuated(a, b) || !a.Deadline.Equal(b.Deadline) || !slices.Equal(a.Tools, b.Tools) {
		return false
	}
	if a.Parent == nil || b.Parent == nil {
		return a.Parent == b.Parent
	}
	return *a.Parent == *b.Parent
}
