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

type connectorToolInfo struct {
	PolicyName       string
	Provider         string
	Class            corev1alpha1.AgentRuntimeBrokeredToolClass
	SpecDigest       string
	PolicyUID        string
	PolicyGeneration int64
	ToolUID          string
	Delegated        bool
	Builtin          bool
}

var ErrBuiltinToolProviderAmbiguous = errors.New("built-in tool is declared by more than one ConnectorProvider")

var ErrLinkedRepositoryScope = errors.New("the task's workspace reaches beyond the repository scope of the task its requester was inherited from")

var ErrLinkedBuiltinChanged = errors.New("the requester's linked account changed while the task was being bound; retrying")

func permanentLinkedBuiltinError(err error) error {
	if errors.Is(err, ErrBuiltinToolProviderAmbiguous) || errors.Is(err, ErrLinkedRepositoryScope) {
		return permanentACPAgentConfiguration(err)
	}
	return err
}

const linkedInheritanceDepth = 16

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

type connectorScope struct {
	strictPolicies bool
	builtins       bool
}

func NativeConnectorToolDispatchDigest(toolUID, policyUID, specDigest string) string {
	sum := sha256.Sum256([]byte("orka.native-connector-tool\x00" + toolUID + "\x00" + policyUID + "\x00" + specDigest))
	return hex.EncodeToString(sum[:])
}

type nativeBuiltinName string

func (n nativeBuiltinName) Name() string                { return string(n) }
func (n nativeBuiltinName) Description() string         { return "" }
func (n nativeBuiltinName) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n nativeBuiltinName) Execute(context.Context, json.RawMessage) (string, error) {
	return "", errors.New("classification placeholder is not executable")
}

func NativeWorkerToolRegistry(task *corev1alpha1.Task, agent *corev1alpha1.Agent) *tools.Registry {
	return nativeWorkerToolRegistry(tools.DefaultRegistry, task, agent)
}

func nativeWorkerToolRegistry(defaults *tools.Registry, task *corev1alpha1.Task, agent *corev1alpha1.Agent) *tools.Registry {
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

func ConnectorToolDispatchDigest(tool corev1alpha1.ToolSpec, policy corev1alpha1.OutboundAccessPolicySpec) (string, error) {
	return approvals.TargetSpecDigest(struct {
		Tool   corev1alpha1.ToolSpec                 `json:"tool"`
		Policy corev1alpha1.OutboundAccessPolicySpec `json:"policy"`
	}{Tool: tool, Policy: policy})
}

func classificationRegistry(registry *tools.Registry) *tools.Registry {
	if registry == nil {
		return tools.DefaultRegistry
	}
	return registry
}

func connectorToolsFor(ctx context.Context, reader client.Reader, registry *tools.Registry, namespace string, toolNames []string) (map[string]connectorToolInfo, error) {
	return classifyConnectorTools(ctx, reader, registry, namespace, toolNames, connectorScope{})
}

func classifyConnectorTools(ctx context.Context, reader client.Reader, registry *tools.Registry, namespace string, toolNames []string, scope connectorScope) (map[string]connectorToolInfo, error) {
	if reader == nil {
		return nil, nil
	}
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
		provider, connectorBacked := outboundaccess.ConnectionProviderName(policy)
		if !connectorBacked {
			continue
		}
		specDigest, err := ConnectorToolDispatchDigest(tool.Spec, policy.Spec)
		if err != nil {
			return nil, fmt.Errorf("digest tool %q: %w", name, err)
		}
		result[name] = connectorToolInfo{
			PolicyName:       policyName,
			Provider:         provider,
			Class:            tool.Spec.BrokeredToolClass,
			SpecDigest:       specDigest,
			PolicyUID:        string(policy.UID),
			PolicyGeneration: policy.Generation,
			ToolUID:          string(tool.UID),
			Delegated:        policy.Spec.AgentIdentity != nil,
		}
	}
	return result, nil
}

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

var ErrChildSealRefused = errors.New("the child task cannot inherit the parent's requester")

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

const acpChildSealAttempts = 4

func ACPChildTaskSealer(reader client.Reader, parentNamespace, parentName, parentUID string) func(context.Context, client.Client, *corev1alpha1.Task) error {
	sealOnce := acpChildTaskSealOnce(reader, parentNamespace, parentName, parentUID)
	return func(ctx context.Context, c client.Client, child *corev1alpha1.Task) error {
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

var requesterStampKey []byte

func SetRequesterStampKey(key []byte) {
	requesterStampKey = append([]byte(nil), key...)
}

const requesterStampGrace = 2 * time.Minute

var ErrRequesterStampPending = errors.New("the task's requester stamp is not sealed yet; retry")

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

func verifiedTaskRequester(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) *corev1alpha1.RequestedBy {
	if verified, err := requesterProvenanceVerified(ctx, reader, task); err != nil || !verified {
		return nil
	}
	return task.Spec.RequestedBy.DeepCopy()
}

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

var errConnectorDispatchDrift = errors.New("a connector tool or its policy changed while the task was being dispatched; retrying")

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
	classified := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		classified[info.PolicyName] = struct{}{}
	}
	for policyName := range byPolicy {
		if _, ok := classified[policyName]; !ok {
			return nil, nil, nil, errConnectorDispatchDrift
		}
	}
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

func freezeRequesterConnectionsClassified(
	ctx context.Context,
	reader client.Reader,
	registry *tools.Registry,
	task *corev1alpha1.Task,
	mcpConfiguration harnessv2.MCPPolicyConfiguration,
) ([]agentExecutionSnapshotConnection, map[string]connectorToolInfo, []string, error) {
	var names, custom []string
	for _, descriptor := range mcpConfiguration.ToolPolicy.Tools {
		if descriptor.Forward != nil && descriptor.Forward.Write {
			continue
		}
		if descriptor.Source == harnessv2.MCPToolSourceBrokeredCustom || descriptor.Source == harnessv2.MCPToolSourceBrokeredBuiltin {
			names = append(names, descriptor.Name)
		}
		if descriptor.Source == harnessv2.MCPToolSourceBrokeredCustom {
			custom = append(custom, descriptor.Name)
		}
	}
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

func anyBuiltinInfo(infos map[string]connectorToolInfo) bool {
	for _, info := range infos {
		if info.Builtin {
			return true
		}
	}
	return false
}

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

func FrozenConnectionsFromTaskStatus(task *corev1alpha1.Task) map[string]outboundaccess.FrozenConnection {
	if task == nil || len(task.Status.ConnectionBindings) == 0 {
		return nil
	}
	frozen := make(map[string]outboundaccess.FrozenConnection, len(task.Status.ConnectionBindings))
	for _, binding := range task.Status.ConnectionBindings {
		frozen[binding.PolicyName] = outboundaccess.FrozenConnection{
			Name: binding.ConnectionName, UID: binding.UID, Generation: binding.Generation, GrantSequence: binding.GrantSequence, Provider: binding.Provider,
			PolicyUID: binding.PolicyUID, PolicyGeneration: binding.PolicyGeneration,
		}
	}
	return frozen
}

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
	executor.SetRequester(verifiedTaskRequester(ctx, reader, task))
	executor.SetTaskUID(string(task.UID))
	executor.SetFrozenConnections(FrozenConnectionsFromTaskStatus(task))
	return bindVerifiedTaskTransactionAuthority(ctx, reader, task, readScopes, enforceCredentialAuth, executor)
}

func frozenConnectionDigest(frozen map[string]outboundaccess.FrozenConnection, policyName string) string {
	binding, ok := frozen[policyName]
	if !ok || binding.UID == "" || binding.GrantSequence <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(policyName + "\x00" + binding.UID + "\x00" + strconv.FormatInt(binding.Generation, 10) +
		"\x00" + strconv.FormatInt(binding.GrantSequence, 10)))
	return hex.EncodeToString(sum[:])
}

func connectionReadyFor(connection *corev1alpha1.Connection, requester *corev1alpha1.RequestedBy, provider string) bool {
	if connection == nil || requester == nil || !connection.DeletionTimestamp.IsZero() {
		return false
	}
	if connection.Spec.Subject.Issuer != requester.Issuer || connection.Spec.Subject.Subject != requester.Subject ||
		connection.Spec.ProviderRef.Name != provider {
		return false
	}
	return connection.Status.GrantSequence > 0 && connectors.ConnectionLinked(connection)
}

func bindFrozenConnections(
	ctx context.Context,
	reader client.Reader,
	snapshots store.AgentExecutionSnapshotStore,
	task *corev1alpha1.Task,
	executor *workerexecutor.ToolExecutor,
) error {
	if task == nil || executor == nil {
		return errors.New("frozen connection binding requires a Task and executor")
	}
	executor.SetRequester(verifiedTaskRequester(ctx, reader, task))
	executor.SetTaskUID(string(task.UID))
	executor.SetFrozenConnections(nil)
	identity, err := agentIdentityBindingFromTask(ctx, reader, task)
	if err != nil {
		return err
	}
	executor.SetAgentIdentity(identity)
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
