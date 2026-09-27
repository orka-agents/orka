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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
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
		if requester == nil || strings.TrimSpace(requester.Issuer) == "" || strings.TrimSpace(requester.Subject) == "" {
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

// connectionReadyFor reports whether connection is the requester's live,
// Ready link to provider for its current generation.
func connectionReadyFor(connection *corev1alpha1.Connection, requester *corev1alpha1.RequestedBy, provider string) bool {
	if connection == nil || requester == nil || !connection.DeletionTimestamp.IsZero() {
		return false
	}
	if connection.Spec.Subject.Issuer != requester.Issuer || connection.Spec.Subject.Subject != requester.Subject ||
		connection.Spec.ProviderRef.Name != provider {
		return false
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == connection.Generation
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
