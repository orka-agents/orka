/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
)

// connectorToolInfo describes one Tool whose OutboundAccessPolicy is in
// connection mode.
type connectorToolInfo struct {
	PolicyName string
	Provider   string
	Class      corev1alpha1.AgentRuntimeBrokeredToolClass
	// SpecDigest is the approval-target digest of the Tool spec as read.
	SpecDigest string
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
		specDigest, err := approvals.TargetSpecDigest(tool.Spec)
		if err != nil {
			return nil, fmt.Errorf("digest tool %q: %w", name, err)
		}
		result[name] = connectorToolInfo{
			PolicyName: policyName,
			Provider:   policy.Spec.Connection.ProviderRef.Name,
			Class:      tool.Spec.BrokeredToolClass,
			SpecDigest: specDigest,
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
	return func(ctx context.Context, c client.Client, child *corev1alpha1.Task) error {
		if reader == nil || child == nil || strings.TrimSpace(parentUID) == "" {
			return errors.New("sealing a brokered child task requires the authenticated parent")
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

// FrozenConnectorToolDigests returns, for every named Tool backed by a
// connection-mode policy, the digest of its spec as read now. The Job builder
// freezes the result into the worker's environment so the controller executes
// only the definition the worker was dispatched with.
func FrozenConnectorToolDigests(ctx context.Context, reader client.Reader, namespace string, toolNames []string) (map[string]string, error) {
	infos, err := connectorToolsFor(ctx, reader, namespace, toolNames)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, nil
	}
	digests := make(map[string]string, len(infos))
	for name, info := range infos {
		digests[name] = info.SpecDigest
	}
	return digests, nil
}

// FrozenConnectionBindingsFromJob decodes the Connection bindings the Job
// builder froze into the worker's environment. It reports false when the Job
// carries none. A Job whose value cannot be decoded fails closed with an error
// rather than yielding an empty binding set.
func FrozenConnectionBindingsFromJob(job *batchv1.Job) ([]corev1alpha1.ConnectionBinding, bool, error) {
	if job == nil {
		return nil, false, nil
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name != workerContainerName {
			continue
		}
		for _, env := range container.Env {
			if env.Name != workerenv.ConnectionBindings {
				continue
			}
			if strings.TrimSpace(env.Value) == "" {
				return nil, false, nil
			}
			var bindings []corev1alpha1.ConnectionBinding
			if err := json.Unmarshal([]byte(env.Value), &bindings); err != nil {
				return nil, true, fmt.Errorf("decode frozen connection bindings on job %q: %w", job.Name, err)
			}
			return bindings, true, nil
		}
	}
	return nil, false, nil
}

// ConnectionBindingsEqual reports whether two binding lists pin the same
// Connections, regardless of order.
func ConnectionBindingsEqual(a, b []corev1alpha1.ConnectionBinding) bool {
	if len(a) != len(b) {
		return false
	}
	byPolicy := make(map[string]corev1alpha1.ConnectionBinding, len(a))
	for _, binding := range a {
		byPolicy[binding.PolicyName] = binding
	}
	for _, binding := range b {
		if byPolicy[binding.PolicyName] != binding {
			return false
		}
	}
	return true
}

// requesterStampKey verifies the stamp the API server seals onto Tasks it
// created for a verified person. Without it no requester is ever trusted
// for connector use.
var requesterStampKey []byte

// SetRequesterStampKey installs the key requester stamps are verified with.
func SetRequesterStampKey(key []byte) {
	requesterStampKey = append([]byte(nil), key...)
}

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

// requesterConnection returns the requester's linked Connection for provider,
// or nil when none is usable or the requester is not provably the person.
func requesterConnection(ctx context.Context, reader client.Reader, task *corev1alpha1.Task, provider string) (*corev1alpha1.Connection, error) {
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
	connection := &corev1alpha1.Connection{}
	name := connectors.ConnectionName(provider, requester.Issuer, requester.Subject)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load connection %q: %w", name, err)
	}
	if !connectionReadyFor(connection, requester, provider) {
		return nil, nil
	}
	return connection, nil
}

// FilterConnectorToolsForRequester applies the readOnly rule: write-class
// tools behind a connection-mode policy are hidden from the agent when the
// requester's Connection to that provider is readOnly. It returns the visible
// tool names in their original order and the connector write tools that
// remain visible, which default into the approval-required set.
func FilterConnectorToolsForRequester(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	toolNames []string,
) (visible []string, connectorWrite []string, err error) {
	if reader == nil || task == nil || len(toolNames) == 0 {
		return toolNames, nil, nil
	}
	infos, err := connectorToolsFor(ctx, reader, task.Namespace, toolNames)
	if err != nil {
		return nil, nil, err
	}
	if len(infos) == 0 {
		return toolNames, nil, nil
	}
	modes := map[string]string{}
	for _, name := range toolNames {
		info, ok := infos[name]
		if !ok {
			visible = append(visible, name)
			continue
		}
		mode, cached := modes[info.Provider]
		if !cached {
			connection, err := requesterConnection(ctx, reader, task, info.Provider)
			if err != nil {
				return nil, nil, err
			}
			if connection != nil {
				mode = connection.Spec.Mode
				if mode == "" {
					mode = corev1alpha1.ConnectionModeReadOnly
				}
			}
			modes[info.Provider] = mode
		}
		if info.Class == corev1alpha1.AgentRuntimeBrokeredToolClassWrite && mode == corev1alpha1.ConnectionModeReadOnly {
			continue
		}
		visible = append(visible, name)
		if info.Class == corev1alpha1.AgentRuntimeBrokeredToolClassWrite {
			connectorWrite = append(connectorWrite, name)
		}
	}
	return visible, connectorWrite, nil
}

// freezeRequesterConnectionsForTools records, for every connection-mode
// policy reachable from the named tools, which of the requester's Connections
// was Ready now. A missing or unready Connection is simply not frozen, which
// makes the later call fail closed. Read failures are returned so the caller
// retries rather than freezing a partial view.
func freezeRequesterConnectionsForTools(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	toolNames []string,
) ([]agentExecutionSnapshotConnection, error) {
	if reader == nil || task == nil {
		return nil, nil
	}
	infos, err := connectorToolsFor(ctx, reader, task.Namespace, toolNames)
	if err != nil {
		return nil, err
	}
	var frozen []agentExecutionSnapshotConnection
	seenPolicies := map[string]struct{}{}
	for _, name := range toolNames {
		info, ok := infos[name]
		if !ok {
			continue
		}
		if _, seen := seenPolicies[info.PolicyName]; seen {
			continue
		}
		seenPolicies[info.PolicyName] = struct{}{}
		connection, err := requesterConnection(ctx, reader, task, info.Provider)
		if err != nil {
			return nil, err
		}
		if connection == nil {
			continue
		}
		frozen = append(frozen, agentExecutionSnapshotConnection{
			PolicyName:     info.PolicyName,
			Provider:       info.Provider,
			ConnectionName: connection.Name,
			UID:            string(connection.UID),
			Generation:     connection.Generation,
			Mode:           connection.Spec.Mode,
		})
	}
	return frozen, nil
}

// freezeRequesterConnections is the ACP entry point: it freezes the
// Connections behind the brokered custom tools of the frozen tool policy.
func freezeRequesterConnections(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	mcpConfiguration harnessv2.MCPPolicyConfiguration,
) ([]agentExecutionSnapshotConnection, error) {
	var names []string
	for _, descriptor := range mcpConfiguration.ToolPolicy.Tools {
		if descriptor.Source == harnessv2.MCPToolSourceBrokeredCustom {
			names = append(names, descriptor.Name)
		}
	}
	return freezeRequesterConnectionsForTools(ctx, reader, task, names)
}

// taskConnectionBindings converts frozen links to the Task status form used
// by native workers.
func taskConnectionBindings(frozen []agentExecutionSnapshotConnection) []corev1alpha1.ConnectionBinding {
	if len(frozen) == 0 {
		return nil
	}
	bindings := make([]corev1alpha1.ConnectionBinding, 0, len(frozen))
	for _, connection := range frozen {
		bindings = append(bindings, corev1alpha1.ConnectionBinding{
			PolicyName: connection.PolicyName, Provider: connection.Provider, ConnectionName: connection.ConnectionName,
			UID: connection.UID, Generation: connection.Generation, Mode: connection.Mode,
		})
	}
	return bindings
}

// FrozenConnectionsFromTaskStatus converts native-Task status bindings into
// the executor's frozen map.
func FrozenConnectionsFromTaskStatus(task *corev1alpha1.Task) map[string]outboundaccess.FrozenConnection {
	if task == nil || len(task.Status.ConnectionBindings) == 0 {
		return nil
	}
	frozen := make(map[string]outboundaccess.FrozenConnection, len(task.Status.ConnectionBindings))
	for _, binding := range task.Status.ConnectionBindings {
		frozen[binding.PolicyName] = outboundaccess.FrozenConnection{UID: binding.UID, Generation: binding.Generation}
	}
	return frozen
}

// BindNativeTaskConnectorAuthority prepares an executor for a connector-backed
// tool call made on behalf of a native type: ai Task through the internal
// controller endpoint: transaction authority plus the requester identity and
// the Connection bindings frozen into Task status at Job creation.
func BindNativeTaskConnectorAuthority(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	readScopes []string,
	enforceCredentialAuth bool,
	executor *workerexecutor.ToolExecutor,
) error {
	if task == nil || executor == nil {
		return errors.New("native connector authority binding requires a Task and executor")
	}
	executor.SetRequester(task.Spec.RequestedBy)
	executor.SetFrozenConnections(FrozenConnectionsFromTaskStatus(task))
	return bindVerifiedTaskTransactionAuthority(ctx, reader, task, readScopes, enforceCredentialAuth, executor)
}

// frozenConnectionDigest returns a stable, non-secret digest of the frozen
// Connection identity behind policyName, for external-effect audit records.
func frozenConnectionDigest(frozen map[string]outboundaccess.FrozenConnection, policyName string) string {
	binding, ok := frozen[policyName]
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(policyName + "\x00" + binding.UID + "\x00" + strconv.FormatInt(binding.Generation, 10)))
	return hex.EncodeToString(sum[:])
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
