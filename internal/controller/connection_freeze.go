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
	// Every connection-mode policy the Task can reach is frozen, with or
	// without a usable link: an entry without a Connection keeps the call
	// failing closed even if the policy is later retargeted to a service
	// credential, because the resolver refuses a frozen policy whose adapter
	// changed. Only a verified requester's Ready Connection fills the entry.
	requester := task.Spec.RequestedBy
	linkable := requester != nil && strings.TrimSpace(requester.Issuer) != "" && strings.TrimSpace(requester.Subject) != ""
	if linkable {
		verified, err := requesterProvenanceVerified(ctx, reader, task)
		if err != nil {
			return nil, err
		}
		linkable = verified
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
			// A policy that cannot be read, including one that is missing, is
			// never left out of the snapshot: a name recreated later in
			// direct or gateway mode would then pass the adapter-change
			// guard. Dispatch retries instead of committing a partial view.
			return nil, fmt.Errorf("load outbound access policy %q: %w", policyName, err)
		}
		if policy.Spec.Connection == nil {
			continue
		}
		provider := policy.Spec.Connection.ProviderRef.Name
		entry := agentExecutionSnapshotConnection{
			PolicyName: policyName, Provider: provider, PolicyUID: string(policy.UID), PolicyGeneration: policy.Generation,
		}
		if linkable {
			connection := &corev1alpha1.Connection{}
			name := connectors.ConnectionName(provider, requester.Issuer, requester.Subject)
			err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, connection)
			switch {
			case err == nil && connectionReadyFor(connection, requester, provider):
				entry.ConnectionName = connection.Name
				entry.UID = string(connection.UID)
				entry.Generation = connection.Generation
				entry.GrantSequence = connection.Status.GrantSequence
				entry.Mode = connection.Spec.Mode
			case err != nil && !apierrors.IsNotFound(err):
				return nil, fmt.Errorf("load connection %q: %w", name, err)
			}
		}
		frozen = append(frozen, entry)
	}
	return frozen, nil
}

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

// requesterStampPending reports whether task is still waiting for its seal
// and young enough for it to be on its way: a Task the API stamped (the seal
// is a second write after the create) or a coordination child a worker
// created (its parent's worker asks the controller to seal it right after).
// Without a configured key no seal can ever arrive (connectors are
// disabled), so nothing is pending and dispatch is never delayed.
func requesterStampPending(task *corev1alpha1.Task, now time.Time) bool {
	if len(requesterStampKey) == 0 || task == nil || task.Spec.RequestedBy == nil ||
		task.Annotations[labels.AnnotationRequestedByStamp] != "" ||
		task.CreationTimestamp.IsZero() || now.Sub(task.CreationTimestamp.Time) >= requesterStampGrace {
		return false
	}
	if task.Annotations[labels.AnnotationRequestedBySource] == labels.RequestedBySourceAPI {
		return true
	}
	owner := metav1.GetControllerOf(task)
	return owner != nil && owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Kind == taskResourceKind
}

// SetRequesterStampKey installs the key requester stamps are verified with.
func SetRequesterStampKey(key []byte) {
	requesterStampKey = append([]byte(nil), key...)
}

// requesterProvenanceVerified reports whether task.spec.requestedBy can be
// trusted for connector use. Trusted workers may set requestedBy on the
// Tasks they create, so the field alone proves nothing, and admission is not
// retroactive, so the controller-only source annotation alone proves nothing
// either: a Task planted while admission was disabled could carry it. Only a
// stamp the controller key sealed for this Task's own UID is trusted. The
// API seals Tasks it creates; a coordination child is sealed only through
// its parent's own worker, which the controller authenticates, so an owner
// reference alone never lets a child inherit another person's authority.
func requesterProvenanceVerified(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (bool, error) {
	_ = reader
	_ = ctx
	if requesterStampPending(task, time.Now()) {
		return false, ErrRequesterStampPending
	}
	if task == nil || task.Spec.RequestedBy == nil {
		return false, nil
	}
	return connectors.RequesterStampValid(requesterStampKey, task), nil
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
	// A linked Connection always carries the grant that linked it; without
	// one there is nothing to bind the snapshot's authority to.
	return connection.Status.GrantSequence > 0 && connectors.ConnectionLinked(connection)
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
		frozen[connection.PolicyName] = outboundaccess.FrozenConnection{
			UID: connection.UID, Generation: connection.Generation, GrantSequence: connection.GrantSequence,
			PolicyUID: connection.PolicyUID, PolicyGeneration: connection.PolicyGeneration,
		}
	}
	return frozen
}

// connectorToolInfo describes one Tool whose OutboundAccessPolicy is in
// connection mode.
type connectorToolInfo struct {
	PolicyName string
	Provider   string
	Class      corev1alpha1.AgentRuntimeBrokeredToolClass
	// PolicyUID and PolicyGeneration pin the policy object this
	// classification was read from, so execution can refuse another.
	PolicyUID        string
	PolicyGeneration int64
}

// connectorToolsFor returns, for every named Tool backed by a connection-mode
// policy, its policy, provider, and class. Unknown tools and tools without
// a policy are skipped. A Tool whose referenced policy does not exist cannot
// be classified, so it returns a retryable error rather than counting as a
// plain Tool: a binding made then could later run under a policy recreated
// in another mode. Read failures are returned so callers retry.
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
					return nil, fmt.Errorf("tool %q references outbound access policy %q, which does not exist; binding waits for it", name, policyName)
				}
				return nil, fmt.Errorf("load outbound access policy %q: %w", policyName, err)
			}
			policies[policyName] = policy
		}
		if policy.Spec.Connection == nil {
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

// ErrChildSealRefused reports a child Task that must not inherit its
// parent's connector authority: the parent is unverified, the child is not
// controller-owned by it, or the child names a different requester.
var ErrChildSealRefused = errors.New("the child task cannot inherit the parent's requester")

// SealChildRequesterStamp seals the requester stamp onto a child Task the
// parent created for its own requester. The parent must itself carry a valid
// stamp, the child must be controller-owned by the parent, and the child's
// requester must equal the parent's. The stamp is computed from the parent's
// identity and written together with the source annotation in one patch
// fenced on the child's resource version, so a requester swapped in between
// validation and sealing makes the write conflict instead of being signed.
func SealChildRequesterStamp(ctx context.Context, c client.Client, key []byte, parent, child *corev1alpha1.Task) error {
	if c == nil || parent == nil || child == nil {
		return errors.New("sealing a child task requires a client, the parent, and the child")
	}
	if len(key) < connectors.MinRequesterStampKeyBytes {
		return errors.New("requester stamps are not enabled on this controller")
	}
	if !connectors.RequesterStampValid(key, parent) {
		return fmt.Errorf("%w: the parent task carries no verified requester", ErrChildSealRefused)
	}
	owner := metav1.GetControllerOf(child)
	if owner == nil || owner.UID != parent.UID || owner.Kind != taskResourceKind || owner.APIVersion != corev1alpha1.GroupVersion.String() {
		return fmt.Errorf("%w: the child task is not controller-owned by the parent", ErrChildSealRefused)
	}
	if child.Spec.RequestedBy == nil || parent.Spec.RequestedBy == nil ||
		child.Spec.RequestedBy.Issuer != parent.Spec.RequestedBy.Issuer || child.Spec.RequestedBy.Subject != parent.Spec.RequestedBy.Subject {
		return fmt.Errorf("%w: the child task names a different requester than its parent", ErrChildSealRefused)
	}
	stamp := connectors.RequesterStamp(key, child.UID, parent.Spec.RequestedBy.Issuer, parent.Spec.RequestedBy.Subject)
	if stamp == "" {
		return errors.New("the child task has no UID to bind the requester stamp to")
	}
	original := child.DeepCopy()
	if child.Annotations == nil {
		child.Annotations = map[string]string{}
	}
	child.Annotations[labels.AnnotationRequestedBySource] = labels.RequestedBySourceAPI
	child.Annotations[labels.AnnotationRequestedByStamp] = stamp
	return c.Patch(ctx, child, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

// acpChildSealAttempts bounds how often a seal fenced out by a concurrent
// write to the fresh child is retried.
const acpChildSealAttempts = 4

// ACPChildTaskSealer returns the seal hook for coordination tools the broker
// executes on behalf of an authenticated ACP Task. The controller created
// the child itself, so it seals the child directly against that Task as the
// parent, re-reading both when a concurrent write fences the patch.
func ACPChildTaskSealer(reader client.Reader, parentNamespace, parentName, parentUID string) func(context.Context, client.Client, *corev1alpha1.Task) error {
	sealOnce := acpChildTaskSealOnce(reader, parentNamespace, parentName, parentUID)
	// Nothing repairs a seal later, and an unsealed child fails closed for
	// connector tools, so a transient read or patch failure is retried a
	// few times; a refusal is final.
	return func(ctx context.Context, c client.Client, child *corev1alpha1.Task) error {
		var err error
		backoff := acpChildSealRetryBackoff
		for attempt := range acpChildSealTransientAttempts {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return err
				case <-time.After(backoff):
				}
				backoff *= 2
			}
			if err = sealOnce(ctx, c, child); err == nil || errors.Is(err, ErrChildSealRefused) {
				return err
			}
		}
		return err
	}
}

// acpChildSealTransientAttempts bounds the retries of a brokered child seal
// that failed for a transient reason; acpChildSealRetryBackoff is the first
// wait, doubled each time.
const acpChildSealTransientAttempts = 3

var acpChildSealRetryBackoff = 200 * time.Millisecond

func acpChildTaskSealOnce(reader client.Reader, parentNamespace, parentName, parentUID string) func(context.Context, client.Client, *corev1alpha1.Task) error {
	return func(ctx context.Context, c client.Client, child *corev1alpha1.Task) error {
		if reader == nil || child == nil || strings.TrimSpace(parentUID) == "" {
			return fmt.Errorf("%w: sealing a brokered child task requires the authenticated parent", ErrChildSealRefused)
		}
		parent := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: parentNamespace, Name: parentName}, parent); err != nil {
			return fmt.Errorf("load the parent task to seal its child: %w", err)
		}
		if string(parent.UID) != parentUID {
			return fmt.Errorf("%w: the authenticated parent task identity changed", ErrChildSealRefused)
		}
		var err error
		for attempt := range acpChildSealAttempts {
			if attempt > 0 {
				latest := &corev1alpha1.Task{}
				if err = reader.Get(ctx, client.ObjectKeyFromObject(child), latest); err != nil {
					return fmt.Errorf("re-read the child task to seal it: %w", err)
				}
				if latest.UID != child.UID {
					return fmt.Errorf("%w: the child task was replaced while it was being sealed", ErrChildSealRefused)
				}
				child = latest
			}
			if err = SealChildRequesterStamp(ctx, c, requesterStampKey, parent, child); !apierrors.IsConflict(err) {
				return err
			}
		}
		return fmt.Errorf("seal the child task: %w", err)
	}
}

// connectorCandidateTools is the allowed tool list with every name the
// effective MCP policy denies removed (the Task's disallowed tools, plus any
// extra denied set such as an external runtime's registered policy), so a
// refusal never triggers on a connector tool the policy would not expose.
func connectorCandidateTools(task *corev1alpha1.Task, agent *corev1alpha1.Agent, extraDisallowed []string) []string {
	allowed := effectiveACPAllowedTools(task, agent)
	denied := map[string]struct{}{}
	if task != nil && task.Spec.AgentRuntime != nil {
		for _, name := range task.Spec.AgentRuntime.DisallowedTools {
			denied[name] = struct{}{}
		}
	}
	for _, name := range extraDisallowed {
		denied[name] = struct{}{}
	}
	if len(denied) == 0 {
		return allowed
	}
	kept := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if _, gone := denied[name]; gone {
			continue
		}
		kept = append(kept, name)
	}
	return kept
}
