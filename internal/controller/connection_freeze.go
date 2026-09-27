/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

// freezeRequesterConnections records, for every connection-mode
// OutboundAccessPolicy reachable from the frozen tool policy, which of the
// requester's Connections was Ready at dispatch. A missing or unready
// Connection is simply not frozen, which makes the later call fail closed.
// Read failures are returned so binding retries rather than freezing a
// partial view.
func freezeRequesterConnections(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	mcpConfiguration harnessv2.MCPPolicyConfiguration,
) ([]agentExecutionSnapshotConnection, error) {
	if reader == nil || task == nil {
		return nil, nil
	}
	requester := task.Spec.RequestedBy
	if requester == nil || strings.TrimSpace(requester.Issuer) == "" || strings.TrimSpace(requester.Subject) == "" {
		return nil, nil
	}
	verified, err := requesterProvenanceVerified(ctx, reader, task)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, nil
	}
	var frozen []agentExecutionSnapshotConnection
	seenPolicies := map[string]struct{}{}
	for _, descriptor := range mcpConfiguration.ToolPolicy.Tools {
		if descriptor.Source != harnessv2.MCPToolSourceBrokeredCustom {
			continue
		}
		tool := &corev1alpha1.Tool{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: descriptor.Name}, tool); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("load tool %q: %w", descriptor.Name, err)
		}
		if tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil {
			continue
		}
		policyName := tool.Spec.HTTP.OutboundAccessPolicyRef.Name
		if _, seen := seenPolicies[policyName]; seen {
			continue
		}
		seenPolicies[policyName] = struct{}{}
		policy := &corev1alpha1.OutboundAccessPolicy{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: policyName}, policy); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("load outbound access policy %q: %w", policyName, err)
		}
		if policy.Spec.Connection == nil {
			continue
		}
		provider := policy.Spec.Connection.ProviderRef.Name
		connection := &corev1alpha1.Connection{}
		name := connectors.ConnectionName(provider, requester.Issuer, requester.Subject)
		if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, connection); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("load connection %q: %w", name, err)
		}
		if !connectionReadyFor(connection, requester, provider) {
			continue
		}
		frozen = append(frozen, agentExecutionSnapshotConnection{
			PolicyName:     policyName,
			Provider:       provider,
			ConnectionName: connection.Name,
			UID:            string(connection.UID),
			Generation:     connection.Generation,
			Mode:           connection.Spec.Mode,
		})
	}
	return frozen, nil
}

// maxRequesterProvenanceDepth bounds the coordination ancestry walk.
const maxRequesterProvenanceDepth = 16

// requesterStampKey verifies the stamp the API server seals onto Tasks it
// created for a verified person. Without it no requester is ever trusted
// for connector use.
var requesterStampKey []byte

// requesterStampGrace is how long after creation a Task the API stamped may
// still be waiting for its seal (a second write after the create). Within it
// an unsealed stamp is a transient condition and dispatch retries rather
// than committing a write-once binding with no Connections; after it, an
// unsealed stamp is treated as unverified.
const requesterStampGrace = 2 * time.Minute

// ErrRequesterStampPending reports a freshly created, API-stamped Task whose
// seal has not landed yet; callers retry rather than freeze nothing.
var ErrRequesterStampPending = errors.New("the task's requester stamp is not sealed yet; retry")

// requesterStampPending reports whether task carries the API's source
// annotation but no stamp, and is young enough for the seal to still be
// on its way. Without a configured key no seal can ever arrive (connectors
// are disabled), so nothing is pending and dispatch is never delayed.
func requesterStampPending(task *corev1alpha1.Task, now time.Time) bool {
	return len(requesterStampKey) > 0 && task != nil && task.Annotations[labels.AnnotationRequestedBySource] == labels.RequestedBySourceAPI &&
		task.Annotations[labels.AnnotationRequestedByStamp] == "" &&
		!task.CreationTimestamp.IsZero() && now.Sub(task.CreationTimestamp.Time) < requesterStampGrace
}

// SetRequesterStampKey installs the key requester stamps are verified with.
func SetRequesterStampKey(key []byte) {
	requesterStampKey = append([]byte(nil), key...)
}

// requesterProvenanceVerified reports whether task.spec.requestedBy can be
// trusted for connector use. Trusted workers may set requestedBy on the
// Tasks they create, so the field alone proves nothing, and admission is not
// retroactive, so the controller-only source annotation alone proves nothing
// either: a Task planted while admission was disabled could carry it. A
// requester is trusted when the API server sealed a stamp binding this
// Task's UID to it, or when the Task descends, through controller-owned
// coordination parents that admission verified, from such a Task with the
// same requester at every step.
func requesterProvenanceVerified(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (bool, error) {
	if requesterStampPending(task, time.Now()) {
		return false, ErrRequesterStampPending
	}
	current := task
	for depth := 0; depth <= maxRequesterProvenanceDepth; depth++ {
		if current == nil || current.Spec.RequestedBy == nil {
			return false, nil
		}
		if connectors.RequesterStampValid(requesterStampKey, current) {
			return true, nil
		}
		owner := metav1.GetControllerOf(current)
		if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.Kind != taskResourceKind {
			return false, nil
		}
		parent := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: current.Namespace, Name: owner.Name}, parent); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("load coordination parent %q: %w", owner.Name, err)
		}
		if parent.UID != owner.UID || parent.Spec.RequestedBy == nil ||
			parent.Spec.RequestedBy.Issuer != current.Spec.RequestedBy.Issuer ||
			parent.Spec.RequestedBy.Subject != current.Spec.RequestedBy.Subject {
			return false, nil
		}
		current = parent
	}
	return false, nil
}

// connectionReadyFor reports whether connection is the requester's live,
// linked Connection to provider for its current generation.
func connectionReadyFor(connection *corev1alpha1.Connection, requester *corev1alpha1.RequestedBy, provider string) bool {
	if connection == nil || requester == nil || !connection.DeletionTimestamp.IsZero() {
		return false
	}
	if connection.Spec.Subject.Issuer != requester.Issuer || connection.Spec.Subject.Subject != requester.Subject ||
		connection.Spec.ProviderRef.Name != provider {
		return false
	}
	return connectors.ConnectionLinked(connection)
}

// bindFrozenConnections loads the Task's execution snapshot and hands the
// executor the requester identity and frozen Connection bindings. Without a
// snapshot store or binding the executor keeps none, so connection-mode
// policies fail closed.
func bindFrozenConnections(
	ctx context.Context,
	snapshots store.AgentExecutionSnapshotStore,
	task *corev1alpha1.Task,
	executor *workerexecutor.ToolExecutor,
) error {
	if task == nil || executor == nil {
		return errors.New("frozen connection binding requires a Task and executor")
	}
	executor.SetRequester(task.Spec.RequestedBy)
	executor.SetFrozenConnections(nil)
	if snapshots == nil || task.Status.AgentExecutionBinding == nil {
		return nil
	}
	binding := task.Status.AgentExecutionBinding
	snapshot, err := snapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{
		TaskUID: string(task.UID),
		Digest:  binding.Snapshot.Digest,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("load execution snapshot for frozen connections: %w", err)
	}
	var body agentExecutionSnapshotBody
	if err := json.Unmarshal(snapshot.Body, &body); err != nil {
		return fmt.Errorf("decode execution snapshot for frozen connections: %w", err)
	}
	executor.SetFrozenConnections(frozenConnectionsFromSnapshot(body))
	return nil
}

func frozenConnectionsFromSnapshot(body agentExecutionSnapshotBody) map[string]outboundaccess.FrozenConnection {
	if len(body.Connections) == 0 {
		return nil
	}
	frozen := make(map[string]outboundaccess.FrozenConnection, len(body.Connections))
	for _, connection := range body.Connections {
		frozen[connection.PolicyName] = outboundaccess.FrozenConnection{UID: connection.UID, Generation: connection.Generation}
	}
	return frozen
}

// connectorToolInfo describes one Tool whose OutboundAccessPolicy is in
// connection mode.
type connectorToolInfo struct {
	PolicyName string
	Provider   string
	Class      corev1alpha1.AgentRuntimeBrokeredToolClass
}

// connectorToolsFor returns, for every named Tool backed by a connection-mode
// policy, its policy, provider, and class. Unknown tools and tools without
// such a policy are skipped; read failures are returned so callers retry.
func connectorToolsFor(ctx context.Context, reader client.Reader, namespace string, toolNames []string) (map[string]connectorToolInfo, error) {
	if reader == nil {
		return nil, nil
	}
	result := map[string]connectorToolInfo{}
	policies := map[string]*corev1alpha1.OutboundAccessPolicy{}
	seen := map[string]struct{}{}
	for _, name := range toolNames {
		if _, done := seen[name]; done {
			continue
		}
		seen[name] = struct{}{}
		tool := &corev1alpha1.Tool{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, tool); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("load tool %q: %w", name, err)
		}
		if tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil {
			continue
		}
		policyName := tool.Spec.HTTP.OutboundAccessPolicyRef.Name
		policy, cached := policies[policyName]
		if !cached {
			policy = &corev1alpha1.OutboundAccessPolicy{}
			if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: policyName}, policy); err != nil {
				if apierrors.IsNotFound(err) {
					policies[policyName] = nil
					continue
				}
				return nil, fmt.Errorf("load outbound access policy %q: %w", policyName, err)
			}
			policies[policyName] = policy
		}
		if policy == nil || policy.Spec.Connection == nil {
			continue
		}
		result[name] = connectorToolInfo{
			PolicyName: policyName,
			Provider:   policy.Spec.Connection.ProviderRef.Name,
			Class:      tool.Spec.BrokeredToolClass,
		}
	}
	return result, nil
}
