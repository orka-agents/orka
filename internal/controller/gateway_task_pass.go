package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *TaskReconciler) freezeGatewayTaskGrant(ctx context.Context, task *corev1alpha1.Task, configuration harnessv2.MCPPolicyConfiguration) (*gatewayruntime.TaskGrant, error) {
	var grant *gatewayruntime.TaskGrant
	var err error
	if r.GatewayService != nil {
		grant, err = r.GatewayService.ResolveTaskGrant(ctx, task)
		if err != nil {
			return nil, err
		}
	}
	var names []string
	var passTools []*corev1alpha1.Tool
	for _, descriptor := range configuration.ToolPolicy.Tools {
		if descriptor.Source != harnessv2.MCPToolSourceBrokeredCustom {
			continue
		}
		tool := &corev1alpha1.Tool{}
		if err := uncachedReader(r.APIReader, r.Client).Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: customACPMCPToolResource(descriptor)}, tool); err != nil {
			return nil, err
		}
		if tool.Spec.HTTP == nil || !tool.Spec.HTTP.GatewayTaskPass {
			continue
		}
		if err := workerexecutor.ValidateGatewayTaskPassTool(tool); err != nil {
			return nil, permanentACPAgentConfiguration(err)
		}
		passTools = append(passTools, tool)
		names = append(names, tool.Name)
	}
	if len(names) > 0 && namesDelegatingTask(task) {
		grant, err = r.inheritGatewayTaskGrant(ctx, task)
		if err != nil {
			return nil, err
		}
		names = slices.DeleteFunc(names, func(name string) bool { return !slices.Contains(grant.Tools, name) })
	}
	for _, tool := range passTools {
		if grant == nil || grant.GrantRef == "" || tool.Spec.HTTP.URL != protocol.TaskToolURL(grant.Endpoint, tool.Name) {
			return nil, permanentACPAgentConfiguration(errors.New("gateway task pass requires a bound root and matching gateway endpoint"))
		}
	}
	if grant == nil {
		return nil, nil
	}
	slices.Sort(names)
	grant.Tools = names
	timeout := defaultACPTaskTimeout
	if task.Spec.Timeout != nil && task.Spec.Timeout.Duration > 0 {
		timeout = task.Spec.Timeout.Duration
	}
	deadline := taskDeadlineFromTimeout(task, time.Now(), timeout)
	if deadline.Before(grant.Deadline) {
		grant.Deadline = deadline
	}
	return grant, nil
}

// inheritGatewayTaskGrant returns the grant a delegated child inherits: its
// parent's frozen grant, bounded by every waiting ancestor's deadline and
// linked to that parent.
func (r *TaskReconciler) inheritGatewayTaskGrant(ctx context.Context, task *corev1alpha1.Task) (*gatewayruntime.TaskGrant, error) {
	if !r.TaskProvenanceProtected {
		return nil, permanentACPAgentConfiguration(errors.New("child gateway task passes require Task provenance admission"))
	}
	reader := uncachedReader(r.APIReader, r.Client)
	parent, err := gatewayPassParent(ctx, reader, task)
	if err != nil {
		if errors.Is(err, errDelegationChainBroken) {
			return nil, permanentACPAgentConfiguration(err)
		}
		return nil, err
	}
	// A parent binds before it runs, so one that could delegate but has no
	// binding, such as a native AI-worker Task, never will.
	if parent.Status.AgentExecutionBinding == nil {
		return nil, permanentACPAgentConfiguration(errors.New("parent has no frozen gateway grant"))
	}
	inherited, err := loadGatewayPassSnapshot(ctx, reader, r.AgentExecutionSnapshots, parent)
	if err != nil {
		return nil, err
	}
	if inherited == nil {
		return nil, permanentACPAgentConfiguration(errors.New("parent has no frozen gateway grant"))
	}
	chain, err := gatewayPassChain(ctx, reader, r.AgentExecutionSnapshots, r.GatewayService, parent, inherited, "", task)
	if err != nil {
		return nil, err
	}
	copy := *inherited
	for _, ancestor := range chain {
		if wait := ancestor.task.Status.Wait; ancestor.task.Status.Phase == corev1alpha1.TaskPhaseSucceeded && wait != nil {
			for _, deadline := range []time.Time{wait.Deadline.Time, taskWaitChainDeadline(ancestor.task)} {
				if deadline.Before(copy.Deadline) {
					copy.Deadline = deadline
				}
			}
		}
	}
	copy.Parent = &gatewayruntime.TaskGrantParent{Namespace: parent.Namespace, Name: parent.Name, UID: string(parent.UID), SnapshotDigest: parent.Status.AgentExecutionBinding.Snapshot.Digest}
	copy.Tools = slices.Clone(inherited.Tools)
	return &copy, nil
}

func (e RegistryACPMCPToolExecutor) bindGatewayTaskGrant(ctx context.Context, request harnessv2.MCPBrokerCallRequest, tool *corev1alpha1.Tool, executor *workerexecutor.ToolExecutor) error {
	if e.GatewayService == nil || e.AgentExecutionSnapshots == nil {
		return errors.New("gateway task pass controller is unavailable")
	}
	authenticated, ok := ACPMCPAuthenticatedTaskFromContext(ctx)
	if !ok || authenticated.Namespace != request.Namespace || authenticated.UID != string(request.Metadata.TaskUID) {
		return errors.New("gateway task pass task is unauthenticated")
	}
	task := &corev1alpha1.Task{}
	if err := e.Reader.Get(ctx, client.ObjectKey{Namespace: authenticated.Namespace, Name: authenticated.Name}, task); err != nil {
		return err
	}
	binding := task.Status.AgentExecutionBinding
	if string(task.UID) != authenticated.UID || binding == nil {
		return errors.New("gateway task pass task is unbound")
	}
	snapshot, err := e.AgentExecutionSnapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: string(task.UID), Digest: binding.Snapshot.Digest})
	if err != nil {
		return err
	}
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	if err != nil {
		return err
	}
	if _, _, _, err := validateAgentExecutionSnapshot(binding, snapshot, body); err != nil {
		return err
	}
	grant := body.GatewayGrant
	if grant == nil || tool.Spec.HTTP.URL != protocol.TaskToolURL(grant.Endpoint, tool.Name) {
		return errors.New("gateway task pass endpoint is not bound")
	}
	httpClient, err := gatewayruntime.NewAdapterHTTPClient(e.HTTPClient, 30*time.Second, grant.ServiceRef, e.GatewayService.Config.AllowInsecureLoopback)
	if err != nil {
		return err
	}
	validate := func(callCtx context.Context) error {
		if grant.Parent != nil && !e.TaskProvenanceProtected {
			return errors.New("child gateway task passes require Task provenance admission")
		}
		_, err := gatewayPassChain(callCtx, e.Reader, e.AgentExecutionSnapshots, e.GatewayService, task, grant, tool.Name, nil)
		return err
	}
	executor.SetGatewayTaskPass(func(callCtx context.Context, _ *corev1alpha1.Tool) (string, error) {
		if err := validate(callCtx); err != nil {
			return "", err
		}
		chain, err := gatewayPassChain(callCtx, e.Reader, e.AgentExecutionSnapshots, e.GatewayService, task, grant, tool.Name, nil)
		if err != nil {
			return "", err
		}
		var pass string
		for _, c := range slices.Backward(chain) {
			pass, err = e.GatewayService.TaskPass(callCtx, c.task, c.grant, validate)
			if err != nil {
				return "", err
			}
		}
		return pass, nil
	}, httpClient)
	executor.SetSendGate(validate)
	return nil
}
