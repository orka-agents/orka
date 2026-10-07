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
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/connectors"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
)

// connectorToolInfo describes one Tool whose OutboundAccessPolicy is in
// connection mode.
type connectorToolInfo struct {
	PolicyName string
	Provider   string
	Class      corev1alpha1.AgentRuntimeBrokeredToolClass
	// SpecDigest is the dispatch digest of the Tool spec and its policy spec
	// as read; see ConnectorToolDispatchDigest.
	SpecDigest string
	// PolicyUID and PolicyGeneration pin the policy object this
	// classification was read from, so execution can refuse another.
	PolicyUID        string
	PolicyGeneration int64
	// ToolUID is the Tool object read, so a Tool deleted and recreated with
	// the same spec is still a different object to a running native Job.
	ToolUID string
}

// NativeConnectorToolDispatchDigest is the dispatch identity a native Job
// freezes for one connector-backed Tool: the spec digest of the Tool and its
// policy (ConnectorToolDispatchDigest) bound to both objects' UIDs, so a
// Tool or policy deleted and recreated with the same spec is refused like a
// changed one.
func NativeConnectorToolDispatchDigest(toolUID, policyUID, specDigest string) string {
	sum := sha256.Sum256([]byte("orka.native-connector-tool\x00" + toolUID + "\x00" + policyUID + "\x00" + specDigest))
	return hex.EncodeToString(sum[:])
}

// nativeBuiltinName stands in for a built-in the native worker registers
// itself, so classification skips a Tool resource of the same name; it is
// never executed.
type nativeBuiltinName string

func (n nativeBuiltinName) Name() string                { return string(n) }
func (n nativeBuiltinName) Description() string         { return "" }
func (n nativeBuiltinName) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n nativeBuiltinName) Execute(context.Context, json.RawMessage) (string, error) {
	return "", errors.New("classification placeholder is not executable")
}

// NativeWorkerToolRegistry is the built-in registry a native type: ai worker
// resolves the Task's tools against: the default registry plus the memory
// tools every worker registers and, when the worker registers them for this
// Task, the coordination tools. A Tool resource shadowed by one of these is
// never what the worker runs, so it is never classified as connector-backed.
func NativeWorkerToolRegistry(task *corev1alpha1.Task, agent *corev1alpha1.Agent) *tools.Registry {
	registry := tools.NewRegistry()
	for _, name := range tools.DefaultRegistry.Names() {
		if tool, ok := tools.DefaultRegistry.Get(name); ok {
			registry.Register(tool)
		}
	}
	extra := aitools.MemoryToolNames()
	coordination := agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled
	if coordination || aitools.RegistersCoordinationTools(task, agent) {
		extra = append(extra, aitools.CoordinationToolNames()...)
	}
	for _, name := range extra {
		if _, ok := registry.Get(name); !ok {
			registry.Register(nativeBuiltinName(name))
		}
	}
	return registry
}

// ConnectorToolDispatchDigest digests everything that shapes a connector-backed
// call besides the person's Connection: the Tool spec (URL, method, headers,
// schema) and the connection-mode policy that injects the credential (output
// header and prefix). The Job builder freezes it at dispatch and the
// controller executes only a definition that still matches.
func ConnectorToolDispatchDigest(tool corev1alpha1.ToolSpec, policy corev1alpha1.OutboundAccessPolicySpec) (string, error) {
	return approvals.TargetSpecDigest(struct {
		Tool   corev1alpha1.ToolSpec                 `json:"tool"`
		Policy corev1alpha1.OutboundAccessPolicySpec `json:"policy"`
	}{Tool: tool, Policy: policy})
}

// classificationRegistry is the built-in tool registry a runtime's policy
// was built against: the one handed in, or the default. Classification
// must use the same registry as the policy, or a Tool resource shadowed by
// a built-in in one and not the other would be treated differently.
func classificationRegistry(registry *tools.Registry) *tools.Registry {
	if registry == nil {
		return tools.DefaultRegistry
	}
	return registry
}

// connectorToolsFor returns, for every named Tool backed by a connection-mode
// policy, its policy, provider, and class. Unknown tools and tools without
// such a policy are skipped; read failures are returned so callers retry.
func connectorToolsFor(ctx context.Context, reader client.Reader, registry *tools.Registry, namespace string, toolNames []string) (map[string]connectorToolInfo, error) {
	return classifyConnectorTools(ctx, reader, registry, namespace, toolNames, false)
}

// classifyConnectorTools is connectorToolsFor with a choice about a policy
// that is missing: a classification treats the Tool as not connector-backed
// (its execution fails on its own), while a snapshot freeze or a binding that
// refuses connector Tools (strictPolicies) retries instead, so a policy of
// the same name recreated later in another mode can never pass the
// adapter-change guard through an omitted entry.
func classifyConnectorTools(ctx context.Context, reader client.Reader, registry *tools.Registry, namespace string, toolNames []string, strictPolicies bool) (map[string]connectorToolInfo, error) {
	if reader == nil {
		return nil, nil
	}
	registry = classificationRegistry(registry)
	result := map[string]connectorToolInfo{}
	policies := map[string]*corev1alpha1.OutboundAccessPolicy{}
	seen := map[string]struct{}{}
	for _, name := range toolNames {
		if _, done := seen[name]; done {
			continue
		}
		seen[name] = struct{}{}
		// A built-in tool wins over a Tool resource of the same name in every
		// runtime, so such a resource is never the implementation here.
		if _, builtin := registry.Get(name); builtin {
			continue
		}
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
				if apierrors.IsNotFound(err) && !strictPolicies {
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
		specDigest, err := ConnectorToolDispatchDigest(tool.Spec, policy.Spec)
		if err != nil {
			return nil, fmt.Errorf("digest tool %q: %w", name, err)
		}
		result[name] = connectorToolInfo{
			PolicyName:       policyName,
			Provider:         policy.Spec.Connection.ProviderRef.Name,
			Class:            tool.Spec.BrokeredToolClass,
			SpecDigest:       specDigest,
			PolicyUID:        string(policy.UID),
			PolicyGeneration: policy.Generation,
			ToolUID:          string(tool.UID),
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
// executes on behalf of an authenticated ACP Task, also used for the runs a
// scheduled Task creates. The controller created the child itself, so it
// seals the child directly against that Task as the parent, re-reading both
// when a concurrent write fences the patch.
func ACPChildTaskSealer(reader client.Reader, parentNamespace, parentName, parentUID string) func(context.Context, client.Client, *corev1alpha1.Task) error {
	sealOnce := acpChildTaskSealOnce(reader, parentNamespace, parentName, parentUID)
	// Nothing repairs a seal later, and an unsealed child fails closed for
	// connector tools, so a transient read or patch failure is retried a
	// few times; a refusal is final.
	return func(ctx context.Context, c client.Client, child *corev1alpha1.Task) error {
		// Without stamps, or without a requester to vouch for, there is
		// nothing to seal, as the API and worker sealers also conclude.
		if len(requesterStampKey) < connectors.MinRequesterStampKeyBytes || child == nil || child.Spec.RequestedBy == nil {
			return nil
		}
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

// FrozenConnectorToolDigests returns, for every named Tool backed by a
// connection-mode policy, the digest of its spec as read now. The Job builder
// freezes the result into the worker's environment so the controller executes
// only the definition the worker was dispatched with.
func FrozenConnectorToolDigests(ctx context.Context, reader client.Reader, namespace string, toolNames []string) (map[string]string, error) {
	infos, err := connectorToolsFor(ctx, reader, tools.DefaultRegistry, namespace, toolNames)
	if err != nil {
		return nil, err
	}
	return nativeConnectorToolDigests(infos), nil
}

// nativeConnectorToolDigests is the native dispatch identity of each
// classified connector tool.
func nativeConnectorToolDigests(infos map[string]connectorToolInfo) map[string]string {
	if len(infos) == 0 {
		return nil
	}
	digests := make(map[string]string, len(infos))
	for name, info := range infos {
		digests[name] = NativeConnectorToolDispatchDigest(info.ToolUID, info.PolicyUID, info.SpecDigest)
	}
	return digests
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
	registry *tools.Registry,
	task *corev1alpha1.Task,
	toolNames []string,
) (visible []string, connectorWrite []string, err error) {
	visible, connectorWrite, _, err = filterConnectorToolsForRequester(ctx, reader, registry, task, toolNames)
	return visible, connectorWrite, err
}

// filterConnectorToolsForRequester is FilterConnectorToolsForRequester that
// also returns the classification the result was derived from, so a caller
// that freezes Connections later can reject a policy that changed between.
func filterConnectorToolsForRequester(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	toolNames []string,
) (visible []string, connectorWrite []string, infos map[string]connectorToolInfo, err error) {
	if reader == nil || task == nil || len(toolNames) == 0 {
		return toolNames, nil, nil, nil
	}
	infos, err = connectorToolsFor(ctx, reader, registry, task.Namespace, toolNames)
	if err != nil {
		return nil, nil, nil, err
	}
	modes := map[string]string{}
	visible, connectorWrite, err = filterClassifiedConnectorTools(toolNames, infos, func(info connectorToolInfo) (string, error) {
		mode, cached := modes[info.Provider]
		if !cached {
			connection, err := requesterConnection(ctx, reader, task, info.Provider)
			if err != nil {
				return "", err
			}
			if connection != nil {
				mode = connection.Spec.Mode
				if mode == "" {
					mode = corev1alpha1.ConnectionModeReadOnly
				}
			}
			modes[info.Provider] = mode
		}
		return mode, nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return visible, connectorWrite, infos, nil
}

// frozenConnectionsMatchClassification reports dispatch drift when a policy
// the freeze bound was not classified as the same connection-mode policy
// object (name, UID, generation, provider) when visibility and approvals
// were decided: a policy that entered connection mode between the two reads
// would otherwise reach the person's credential with no hiding and no
// approval default.
func frozenConnectionsMatchClassification(frozen []agentExecutionSnapshotConnection, infos map[string]connectorToolInfo) error {
	for _, entry := range frozen {
		matched := false
		for _, info := range infos {
			if info.PolicyName == entry.PolicyName && info.PolicyUID == entry.PolicyUID &&
				info.PolicyGeneration == entry.PolicyGeneration && info.Provider == entry.Provider {
				matched = true
				break
			}
		}
		if !matched {
			return errConnectorDispatchDrift
		}
	}
	return nil
}

// filterClassifiedConnectorTools applies the readOnly rule to one
// classification: modeFor returns the requester's link mode behind a
// connector tool ("" when there is no link).
func filterClassifiedConnectorTools(
	toolNames []string,
	infos map[string]connectorToolInfo,
	modeFor func(connectorToolInfo) (string, error),
) (visible []string, connectorWrite []string, err error) {
	if len(infos) == 0 {
		return toolNames, nil, nil
	}
	for _, name := range toolNames {
		info, ok := infos[name]
		if !ok {
			visible = append(visible, name)
			continue
		}
		mode, err := modeFor(info)
		if err != nil {
			return nil, nil, err
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

// errConnectorDispatchDrift reports a Tool or policy that changed between
// the dispatch's Connection freeze and its Job build; the dispatch retries.
var errConnectorDispatchDrift = errors.New("a connector tool or its policy changed while the task was being dispatched; retrying")

// nativeConnectorDispatch classifies a native Job's tools once and derives
// visibility, the approval set, and the dispatch digests from that single
// classification. With frozen bindings, link modes come from the bindings
// (not a second Connection read), and a connector tool whose policy has no
// binding of the same policy object is drift since the freeze: the build
// fails so the dispatch retries instead of mixing revisions in one Job.
func nativeConnectorDispatch(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	toolNames []string,
	bindings []corev1alpha1.ConnectionBinding,
	frozen bool,
) (visible, connectorWrite []string, digests map[string]string, err error) {
	if reader == nil || task == nil || len(toolNames) == 0 {
		return toolNames, nil, nil, nil
	}
	infos, err := classifyConnectorTools(ctx, reader, registry, task.Namespace, toolNames, true)
	if err != nil {
		return nil, nil, nil, err
	}
	if !frozen {
		modes := map[string]string{}
		visible, connectorWrite, err = filterClassifiedConnectorTools(toolNames, infos, func(info connectorToolInfo) (string, error) {
			mode, cached := modes[info.Provider]
			if !cached {
				connection, err := requesterConnection(ctx, reader, task, info.Provider)
				if err != nil {
					return "", err
				}
				if connection != nil {
					mode = connection.Spec.Mode
					if mode == "" {
						mode = corev1alpha1.ConnectionModeReadOnly
					}
				}
				modes[info.Provider] = mode
			}
			return mode, nil
		})
		if err != nil {
			return nil, nil, nil, err
		}
		return visible, connectorWrite, nativeConnectorToolDigests(infos), nil
	}
	byPolicy := make(map[string]corev1alpha1.ConnectionBinding, len(bindings))
	for _, binding := range bindings {
		byPolicy[binding.PolicyName] = binding
	}
	// A policy frozen as connection-backed must still classify as one: a
	// policy that left connection mode (or a Tool retargeted off it) since
	// the freeze would otherwise turn its tools into plain local tools, with
	// no controller route and no connector approval default.
	classified := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		classified[info.PolicyName] = struct{}{}
	}
	for policyName := range byPolicy {
		if _, ok := classified[policyName]; !ok {
			return nil, nil, nil, errConnectorDispatchDrift
		}
	}
	visible, connectorWrite, err = filterClassifiedConnectorTools(toolNames, infos, func(info connectorToolInfo) (string, error) {
		binding, ok := byPolicy[info.PolicyName]
		if !ok || binding.PolicyUID != info.PolicyUID || binding.PolicyGeneration != info.PolicyGeneration || binding.Provider != info.Provider {
			return "", errConnectorDispatchDrift
		}
		if binding.UID == "" {
			return "", nil
		}
		if binding.Mode == "" {
			return corev1alpha1.ConnectionModeReadOnly, nil
		}
		return binding.Mode, nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return visible, connectorWrite, nativeConnectorToolDigests(infos), nil
}

// freezeRequesterConnectionsForTools records, for every connection-mode
// policy reachable from the named tools, which of the requester's Connections
// was Ready now. A missing or unready Connection is simply not frozen, which
// makes the later call fail closed. Read failures are returned so the caller
// retries rather than freezing a partial view.
func freezeRequesterConnectionsForTools(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	toolNames []string,
) ([]agentExecutionSnapshotConnection, error) {
	if reader == nil || task == nil {
		return nil, nil
	}
	infos, err := classifyConnectorTools(ctx, reader, registry, task.Namespace, toolNames, true)
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
		// Every connection-mode policy the Task can reach is frozen, with or
		// without a usable link: an entry without a Connection keeps the
		// call failing closed even if the policy is later retargeted to a
		// service credential, because the resolver refuses a frozen policy
		// whose adapter changed. The policy the Connection is bound under is
		// part of what the Task is dispatched with.
		entry := agentExecutionSnapshotConnection{
			PolicyName: info.PolicyName, Provider: info.Provider, PolicyUID: info.PolicyUID, PolicyGeneration: info.PolicyGeneration,
		}
		if connection != nil {
			entry.ConnectionName = connection.Name
			entry.UID = string(connection.UID)
			entry.Generation = connection.Generation
			entry.GrantSequence = connection.Status.GrantSequence
			entry.Mode = connection.Spec.Mode
		}
		frozen = append(frozen, entry)
	}
	return frozen, nil
}

// freezeRequesterConnections is the ACP entry point: it freezes the
// Connections behind the brokered custom tools of the frozen tool policy.
func freezeRequesterConnections(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	mcpConfiguration harnessv2.MCPPolicyConfiguration,
) ([]agentExecutionSnapshotConnection, error) {
	var names []string
	for _, descriptor := range mcpConfiguration.ToolPolicy.Tools {
		if descriptor.Source == harnessv2.MCPToolSourceBrokeredCustom {
			names = append(names, descriptor.Name)
		}
	}
	// Every descriptor was just built from an existing Tool: one removed
	// mid-binding makes binding retry, or a Tool recreated under a policy in
	// another mode could run without a frozen-policy entry.
	if reader != nil && task != nil {
		for _, name := range names {
			if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, &corev1alpha1.Tool{}); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("tool %q was removed while the task was being bound; binding retries", name)
				}
				return nil, fmt.Errorf("load tool %q: %w", name, err)
			}
		}
	}
	return freezeRequesterConnectionsForTools(ctx, reader, registry, task, names)
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
			UID: connection.UID, Generation: connection.Generation, GrantSequence: connection.GrantSequence, Mode: connection.Mode,
			PolicyUID: connection.PolicyUID, PolicyGeneration: connection.PolicyGeneration,
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
		frozen[binding.PolicyName] = outboundaccess.FrozenConnection{
			UID: binding.UID, Generation: binding.Generation, GrantSequence: binding.GrantSequence,
			PolicyUID: binding.PolicyUID, PolicyGeneration: binding.PolicyGeneration,
		}
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
	sum := sha256.Sum256([]byte(policyName + "\x00" + binding.UID + "\x00" + strconv.FormatInt(binding.Generation, 10) +
		"\x00" + strconv.FormatInt(binding.GrantSequence, 10)))
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
