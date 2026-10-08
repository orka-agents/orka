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
	"slices"
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
	// Builtin marks an Orka built-in tool a ConnectorProvider declares. It
	// has no policy: PolicyName is its BuiltinConnectionKey and the
	// controller resolves the person's credential for it directly.
	Builtin bool
}

// ErrBuiltinToolProviderAmbiguous reports a built-in tool that more than one
// accepted ConnectorProvider declares; a linked account can serve it from
// only one, so the configuration is refused rather than a provider chosen.
var ErrBuiltinToolProviderAmbiguous = errors.New("built-in tool is declared by more than one ConnectorProvider")

// ErrLinkedRepositoryScope reports a coordination child whose workspace
// names a repository its parent chain never held: the requester it
// inherited must not reach further than the Task the person created.
var ErrLinkedRepositoryScope = errors.New("the task's workspace reaches beyond the repository scope of the task its requester was inherited from")

// ErrLinkedBuiltinChanged reports a requester link that disappeared or was
// narrowed between planning, which exposed a linked built-in, and the
// freeze; binding retries and plans again.
var ErrLinkedBuiltinChanged = errors.New("the requester's linked account changed while the task was being bound; retrying")

// permanentLinkedBuiltinError turns the configuration refusals of the
// linked built-in path into permanent ACP configuration errors and leaves
// every other error retryable.
func permanentLinkedBuiltinError(err error) error {
	if errors.Is(err, ErrBuiltinToolProviderAmbiguous) || errors.Is(err, ErrLinkedRepositoryScope) {
		return permanentACPAgentConfiguration(err)
	}
	return err
}

// linkedInheritanceDepth bounds the parent chain walked for repository scope.
const linkedInheritanceDepth = 16

// taskRepositories returns the GitHub repositories a Task's workspace
// names, as canonical owner/repo pairs (or the raw value when not a GitHub
// URL), so scopes compare the way the GitHub tools compare them.
func taskRepositories(task *corev1alpha1.Task) map[string]struct{} {
	repositories := map[string]struct{}{}
	if task == nil || task.Spec.Workspace == nil {
		return repositories
	}
	for _, raw := range []string{task.Spec.Workspace.GitRepo, task.Spec.Workspace.PublicationGitRepo} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if owner, repo, err := tools.ParseGitHubRepository(raw); err == nil {
			repositories[strings.ToLower(owner+"/"+repo)] = struct{}{}
			continue
		}
		repositories[strings.ToLower(raw)] = struct{}{}
	}
	return repositories
}

// linkedRepositoryScopeInherited checks that a coordination child's
// workspace stays within its parent's repositories, all the way up the
// controller-owner chain to the Task the person created. A child inherits
// its parent's verified requester, and delegate_task lets a coordinator
// name any repository, so without this the person's linked account could
// be pointed anywhere by delegating instead of calling directly. A Task
// with no controlling Task is its own root and passes, unless the
// controller-managed parent annotation shows it was delegated: a child
// orphaned from its parent can no longer be checked and is refused. Read
// failures are returned as they are, so the caller retries.
func linkedRepositoryScopeInherited(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) error {
	current := task
	for range linkedInheritanceDepth {
		owner := metav1.GetControllerOf(current)
		recordedParent := strings.TrimSpace(current.Annotations[labels.AnnotationParentTaskUID])
		if owner == nil || owner.Kind != taskResourceKind || owner.APIVersion != corev1alpha1.GroupVersion.String() {
			if recordedParent != "" {
				return fmt.Errorf("%w: task %q was delegated by task %s, which no longer controls it", ErrLinkedRepositoryScope, current.Name, recordedParent)
			}
			return nil
		}
		if recordedParent != "" && recordedParent != string(owner.UID) {
			return fmt.Errorf("%w: task %q records parent %s but is controlled by task %q", ErrLinkedRepositoryScope, current.Name, recordedParent, owner.Name)
		}
		parent := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: current.Namespace, Name: owner.Name}, parent); err != nil {
			return fmt.Errorf("load parent task %q for repository scope: %w", owner.Name, err)
		}
		if parent.UID != owner.UID {
			return fmt.Errorf("%w: parent task %q was replaced", ErrLinkedRepositoryScope, owner.Name)
		}
		allowed := taskRepositories(parent)
		for repository := range taskRepositories(current) {
			if _, ok := allowed[repository]; !ok {
				return fmt.Errorf("%w: task %q names %s, which task %q does not hold", ErrLinkedRepositoryScope, current.Name, repository, parent.Name)
			}
		}
		current = parent
	}
	return fmt.Errorf("%w: the parent chain of task %q is deeper than %d", ErrLinkedRepositoryScope, task.Name, linkedInheritanceDepth)
}

// connectorScope chooses what classifyConnectorTools treats as
// connector-backed beyond Tools behind connection-mode policies.
type connectorScope struct {
	// strictPolicies retries on a missing policy instead of skipping the
	// Tool; see classifyConnectorTools.
	strictPolicies bool
	// builtins classifies catalog built-ins a ConnectorProvider declares.
	// Only paths that execute built-ins in the controller (the ACP broker)
	// set it: a native worker runs built-ins itself and never holds a
	// linked account, so for it they stay on the Task's own credentials.
	builtins bool
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
	return nativeWorkerToolRegistry(tools.DefaultRegistry, task, agent)
}

func nativeWorkerToolRegistry(defaults *tools.Registry, task *corev1alpha1.Task, agent *corev1alpha1.Agent) *tools.Registry {
	// The controller's default registry also holds the pull request tools
	// it serves the compatibility proxies with; a worker has those only as
	// coordination tools, added below when coordination applies.
	proxyOnly := map[string]bool{}
	for _, name := range tools.ProxyPRToolNames() {
		proxyOnly[name] = true
	}
	registry := tools.NewRegistry()
	for _, name := range defaults.Names() {
		if proxyOnly[name] {
			continue
		}
		if tool, ok := defaults.Get(name); ok {
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
	return classifyConnectorTools(ctx, reader, registry, namespace, toolNames, connectorScope{})
}

// classifyConnectorTools is connectorToolsFor with a choice about a policy
// that is missing: a classification treats the Tool as not connector-backed
// (its execution fails on its own), while a snapshot freeze or a binding that
// refuses connector Tools (strictPolicies) retries instead, so a policy of
// the same name recreated later in another mode can never pass the
// adapter-change guard through an omitted entry.
func classifyConnectorTools(ctx context.Context, reader client.Reader, registry *tools.Registry, namespace string, toolNames []string, scope connectorScope) (map[string]connectorToolInfo, error) {
	if reader == nil {
		return nil, nil
	}
	// Only a configured broker registry executes linked built-ins; the
	// fallback below decides only that a built-in shadows a Tool resource.
	brokerRegistry := registry
	registry = classificationRegistry(registry)
	result := map[string]connectorToolInfo{}
	policies := map[string]*corev1alpha1.OutboundAccessPolicy{}
	var providers *corev1alpha1.ConnectorProviderList
	seen := map[string]struct{}{}
	for _, name := range toolNames {
		if _, done := seen[name]; done {
			continue
		}
		seen[name] = struct{}{}
		// A built-in tool wins over a Tool resource of the same name in every
		// runtime, so such a resource is never the implementation here.
		if _, builtin := registry.Get(name); builtin {
			if !scope.builtins || brokerRegistry == nil {
				continue
			}
			if _, brokered := brokerRegistry.Get(name); !brokered {
				continue
			}
			class, linked := connectors.BuiltinConnectorToolClass(name)
			if !linked {
				continue
			}
			if providers == nil {
				providers = &corev1alpha1.ConnectorProviderList{}
				if err := reader.List(ctx, providers, client.InNamespace(namespace)); err != nil {
					return nil, fmt.Errorf("list connector providers: %w", err)
				}
			}
			provider, err := builtinToolProvider(providers, name)
			if err != nil {
				return nil, err
			}
			if provider == nil {
				continue
			}
			result[name] = connectorToolInfo{
				PolicyName: outboundaccess.BuiltinConnectionKey(name), Provider: provider.Name,
				Class: corev1alpha1.AgentRuntimeBrokeredToolClass(class), Builtin: true,
			}
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
				if apierrors.IsNotFound(err) && !scope.strictPolicies {
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

// builtinToolProvider returns the one accepted provider declaring name as a
// built-in, nil when none does, or ErrBuiltinToolProviderAmbiguous when
// several do.
func builtinToolProvider(providers *corev1alpha1.ConnectorProviderList, name string) (*corev1alpha1.ConnectorProvider, error) {
	var found *corev1alpha1.ConnectorProvider
	var names []string
	for i := range providers.Items {
		provider := &providers.Items[i]
		if !connectors.ProviderAccepted(provider) {
			continue
		}
		if _, declared := connectors.DeclaresBuiltinTool(provider, name); !declared {
			continue
		}
		names = append(names, provider.Name)
		found = provider
	}
	if len(names) > 1 {
		return nil, fmt.Errorf("%w: %q is declared by %s", ErrBuiltinToolProviderAmbiguous, name, strings.Join(names, ", "))
	}
	return found, nil
}

// brokeredLinkedBuiltins returns the names that are catalog built-ins the
// given broker registry can execute: tools that reach an agent only through
// a linked account frozen at dispatch. Without a broker registry (the ACP
// broker is not configured) nothing runs under a linked account, so the
// native registry's GitHub tools keep their Task-credential path.
func brokeredLinkedBuiltins(registry *tools.Registry, toolNames []string) []string {
	if registry == nil {
		return nil
	}
	var linked []string
	for _, name := range toolNames {
		if _, catalog := connectors.BuiltinConnectorToolClass(name); !catalog {
			continue
		}
		if _, registered := registry.Get(name); registered {
			linked = append(linked, name)
		}
	}
	return linked
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
	// The person's links to provider under any name: the API creates the
	// canonical name, but the reconciler adopts Connections created outside
	// it. Exactly one link binds; several are ambiguous and bind nothing,
	// as the live resolver and list_connections also refuse them.
	owned, err := connectors.ListSubjectConnectionsAuthoritative(ctx, reader, task.Namespace, requester)
	if err != nil {
		return nil, fmt.Errorf("list the requester's connections: %w", err)
	}
	var matching []corev1alpha1.Connection
	for i := range owned {
		if owned[i].Spec.ProviderRef.Name == provider {
			matching = append(matching, owned[i])
		}
	}
	// A cached listing can trail a Connection the API just created, always
	// under the canonical name: read that name fresh so informer lag never
	// hides it. An object there that is not the person's link is ignored
	// rather than allowed to hide the link they do hold.
	name := connectors.ConnectionName(provider, requester.Issuer, requester.Subject)
	fresh := &corev1alpha1.Connection{}
	switch err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, fresh); {
	case err == nil:
		if fresh.Spec.Subject.Issuer == requester.Issuer && fresh.Spec.Subject.Subject == requester.Subject && fresh.Spec.ProviderRef.Name == provider {
			listed := slices.IndexFunc(matching, func(c corev1alpha1.Connection) bool { return c.Name == name })
			if listed < 0 {
				matching = append(matching, *fresh)
			} else {
				matching[listed] = *fresh
			}
		}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("load connection %q: %w", name, err)
	}
	if len(matching) != 1 {
		return nil, nil
	}
	connection := &matching[0]
	if !connectionReadyFor(connection, requester, provider) {
		return nil, nil
	}
	// Ready is the Connection controller's last projection. A provider
	// update it has not reconciled yet (new authority, wider scopes) would
	// make every call fail at the credential source while the snapshot,
	// which re-consent cannot repair, keeps the stale grant; the link is
	// judged against the provider as it stands now.
	current := &corev1alpha1.ConnectorProvider{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: provider}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load connector provider %q: %w", provider, err)
	}
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	if !connectors.ProviderAccepted(current) || !connectors.ConsentMatchesProvider(connection, current) ||
		!connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(current, mode)) {
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
	visible, connectorWrite, _, err = filterConnectorToolsForRequester(ctx, reader, registry, task, toolNames, connectorScope{})
	return visible, connectorWrite, err
}

// FilterBrokeredConnectorToolsForRequester is FilterConnectorToolsForRequester
// for tools the controller's broker executes itself: catalog built-ins a
// ConnectorProvider declares follow the same readOnly and approval rules as
// Tools behind connection-mode policies whenever the requester holds a
// Ready link to that provider.
func FilterBrokeredConnectorToolsForRequester(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	toolNames []string,
) (visible []string, connectorWrite []string, err error) {
	visible, connectorWrite, _, err = filterConnectorToolsForRequester(ctx, reader, registry, task, toolNames, connectorScope{builtins: true})
	return visible, connectorWrite, err
}

// filterConnectorToolsForRequester also returns the classification the
// result was derived from, so a caller that freezes Connections later can
// reject a policy that changed between.
func filterConnectorToolsForRequester(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	toolNames []string,
	scope connectorScope,
) (visible []string, connectorWrite []string, infos map[string]connectorToolInfo, err error) {
	if reader == nil || task == nil || len(toolNames) == 0 {
		return toolNames, nil, nil, nil
	}
	infos, err = classifyConnectorTools(ctx, reader, registry, task.Namespace, toolNames, scope)
	if err != nil {
		return nil, nil, nil, err
	}
	if scope.builtins && anyBuiltinInfo(infos) {
		if err := linkedRepositoryScopeInherited(ctx, reader, task); err != nil {
			return nil, nil, nil, err
		}
	}
	// brokeredBuiltin reports a catalog built-in the broker could execute:
	// the requester reaches it only through a linked account, so without
	// one it is hidden rather than run on other credentials. Without a
	// broker registry nothing is brokered.
	brokeredBuiltin := func(name string) bool {
		if !scope.builtins || registry == nil {
			return false
		}
		_, linked := connectors.BuiltinConnectorToolClass(name)
		_, registered := registry.Get(name)
		return linked && registered
	}
	modes := map[string]string{}
	visible, connectorWrite, err = filterClassifiedConnectorTools(toolNames, infos, brokeredBuiltin, func(info connectorToolInfo) (string, error) {
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

// connectorClassificationUnchanged reports dispatch drift when any tool
// the freeze covered classified differently from the read that decided
// visibility and approvals. Tools are compared one by one, not by policy:
// a Tool retargeted onto a connection-mode policy another Tool already
// uses would otherwise pass on that Tool's entry, although its own
// visibility and approval default were decided as a plain tool.
func connectorClassificationUnchanged(planned, frozen map[string]connectorToolInfo, toolNames []string) error {
	for _, name := range toolNames {
		before, wasConnector := planned[name]
		after, isConnector := frozen[name]
		if wasConnector != isConnector || before != after {
			return errConnectorDispatchDrift
		}
	}
	return nil
}

// filterClassifiedConnectorTools applies the readOnly rule to one
// classification: modeFor returns the requester's link mode behind a
// connector tool ("" when there is no link), and hidden, when set, drops an
// unclassified name (a brokered built-in with no linked account).
func filterClassifiedConnectorTools(
	toolNames []string,
	infos map[string]connectorToolInfo,
	hidden func(string) bool,
	modeFor func(connectorToolInfo) (string, error),
) (visible []string, connectorWrite []string, err error) {
	if len(infos) == 0 && hidden == nil {
		return toolNames, nil, nil
	}
	for _, name := range toolNames {
		info, ok := infos[name]
		if !ok {
			if hidden != nil && hidden(name) {
				continue
			}
			visible = append(visible, name)
			continue
		}
		mode, err := modeFor(info)
		if err != nil {
			return nil, nil, err
		}
		// A brokered built-in has no credential path but the link: with
		// no Ready link it is hidden. A Tool behind a connection-mode
		// policy stays visible and fails closed at call time instead, so
		// a policy retargeted later still cannot serve it.
		if info.Builtin && mode == "" {
			continue
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
	frozenDigests map[string]string,
	frozen bool,
) (visible, connectorWrite []string, digests map[string]string, err error) {
	if reader == nil || task == nil || len(toolNames) == 0 {
		return toolNames, nil, nil, nil
	}
	infos, err := classifyConnectorTools(ctx, reader, registry, task.Namespace, toolNames, connectorScope{strictPolicies: true})
	if err != nil {
		return nil, nil, nil, err
	}
	if !frozen {
		modes := map[string]string{}
		visible, connectorWrite, err = filterClassifiedConnectorTools(toolNames, infos, nil, func(info connectorToolInfo) (string, error) {
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
	// Tools are also compared one by one against the freeze: a Tool
	// retargeted off a policy another Tool still uses keeps that policy
	// classified, yet it would run as a plain local tool with no connector
	// route and no approval default; one retargeted onto a frozen policy
	// would reach the person's credential undecided.
	current := nativeConnectorToolDigests(infos)
	for _, name := range toolNames {
		before, wasConnector := frozenDigests[name]
		after, isConnector := current[name]
		if wasConnector != isConnector || before != after {
			return nil, nil, nil, errConnectorDispatchDrift
		}
	}
	visible, connectorWrite, err = filterClassifiedConnectorTools(toolNames, infos, nil, func(info connectorToolInfo) (string, error) {
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
	scope connectorScope,
) ([]agentExecutionSnapshotConnection, error) {
	frozen, _, err := freezeClassifiedRequesterConnections(ctx, reader, registry, task, toolNames, scope)
	return frozen, err
}

// freezeClassifiedRequesterConnections is freezeRequesterConnectionsForTools
// that also returns the classification the freeze was made from.
func freezeClassifiedRequesterConnections(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	toolNames []string,
	scope connectorScope,
) ([]agentExecutionSnapshotConnection, map[string]connectorToolInfo, error) {
	if reader == nil || task == nil {
		return nil, nil, nil
	}
	scope.strictPolicies = true
	infos, err := classifyConnectorTools(ctx, reader, registry, task.Namespace, toolNames, scope)
	if err != nil {
		return nil, nil, err
	}
	if scope.builtins && anyBuiltinInfo(infos) {
		// The link is frozen for a child only within its parents' repositories.
		if err := linkedRepositoryScopeInherited(ctx, reader, task); err != nil {
			return nil, nil, err
		}
	}
	var frozen []agentExecutionSnapshotConnection
	seenPolicies := map[string]struct{}{}
	connections := map[string]*corev1alpha1.Connection{}
	for _, name := range toolNames {
		info, ok := infos[name]
		if !ok {
			continue
		}
		if _, seen := seenPolicies[info.PolicyName]; seen {
			continue
		}
		seenPolicies[info.PolicyName] = struct{}{}
		connection, cached := connections[info.Provider]
		if !cached {
			connection, err = requesterConnection(ctx, reader, task, info.Provider)
			if err != nil {
				return nil, nil, err
			}
			connections[info.Provider] = connection
		}
		if info.Builtin {
			// A built-in is frozen only with a usable link: without one
			// the broker never offers it, and a link made after dispatch
			// is never picked up, so the approval set computed at dispatch
			// stays true for the whole run. The names here are the ones
			// planning exposed, and planning hides a built-in without a
			// usable link (and a write built-in without readWrite), so a
			// link missing or narrowed now changed between the two reads:
			// binding retries rather than freeze a policy no call can use.
			if connection == nil {
				if scope.builtins {
					return nil, nil, fmt.Errorf("%w: %s", ErrLinkedBuiltinChanged, name)
				}
				continue
			}
			if class, _ := connectors.BuiltinConnectorToolClass(name); scope.builtins && class == corev1alpha1.ConnectorToolClassWrite &&
				connection.Spec.Mode != corev1alpha1.ConnectionModeReadWrite {
				return nil, nil, fmt.Errorf("%w: %s", ErrLinkedBuiltinChanged, name)
			}
			frozen = append(frozen, agentExecutionSnapshotConnection{
				PolicyName: info.PolicyName, Tool: name, Provider: info.Provider, ConnectionName: connection.Name,
				UID: string(connection.UID), Generation: connection.Generation,
				GrantSequence: connection.Status.GrantSequence, Mode: connection.Spec.Mode,
			})
			continue
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
	return frozen, infos, nil
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
	frozen, _, _, err := freezeRequesterConnectionsClassified(ctx, reader, registry, task, mcpConfiguration)
	return frozen, err
}

// freezeRequesterConnectionsClassified is freezeRequesterConnections that
// also returns the classification the freeze was made from and the tools
// it covered, so the caller can hold it to the planning classification.
func freezeRequesterConnectionsClassified(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	mcpConfiguration harnessv2.MCPPolicyConfiguration,
) ([]agentExecutionSnapshotConnection, map[string]connectorToolInfo, []string, error) {
	var names, custom []string
	for _, descriptor := range mcpConfiguration.ToolPolicy.Tools {
		if descriptor.Source == harnessv2.MCPToolSourceBrokeredCustom || descriptor.Source == harnessv2.MCPToolSourceBrokeredBuiltin {
			names = append(names, descriptor.Name)
		}
		if descriptor.Source == harnessv2.MCPToolSourceBrokeredCustom {
			custom = append(custom, descriptor.Name)
		}
	}
	// Every custom descriptor was just built from an existing Tool: one
	// removed mid-binding makes binding retry, or a Tool recreated under a
	// policy in another mode could run without a frozen-policy entry.
	if reader != nil && task != nil {
		for _, name := range custom {
			if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, &corev1alpha1.Tool{}); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, nil, nil, fmt.Errorf("tool %q was removed while the task was being bound; binding retries", name)
				}
				return nil, nil, nil, fmt.Errorf("load tool %q: %w", name, err)
			}
		}
	}
	frozen, infos, err := freezeClassifiedRequesterConnections(ctx, reader, registry, task, names, connectorScope{builtins: true})
	return frozen, infos, names, permanentLinkedBuiltinError(err)
}

// anyBuiltinInfo reports whether any classified tool is a linked built-in.
func anyBuiltinInfo(infos map[string]connectorToolInfo) bool {
	for _, info := range infos {
		if info.Builtin {
			return true
		}
	}
	return false
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
		// The provider travels with the binding, so a policy retargeted to
		// another provider after dispatch is refused on this path too.
		frozen[binding.PolicyName] = outboundaccess.FrozenConnection{
			Name: binding.ConnectionName, UID: binding.UID, Generation: binding.Generation, GrantSequence: binding.GrantSequence, Provider: binding.Provider,
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
	// A policy frozen with no Connection held for it is unbound: there is
	// no link for a digest to name.
	if !ok || binding.UID == "" || binding.GrantSequence <= 0 {
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
			Name: connection.ConnectionName, UID: connection.UID, Generation: connection.Generation, GrantSequence: connection.GrantSequence,
			PolicyUID: connection.PolicyUID, PolicyGeneration: connection.PolicyGeneration, Provider: connection.Provider,
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
