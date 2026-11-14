package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
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
			return nil, err
		}
		if grant == nil || grant.GrantRef == "" || tool.Spec.HTTP.URL != grant.Endpoint+"/v1/task-tools/"+tool.Name {
			return nil, errors.New("gateway task pass requires a bound root and matching gateway endpoint")
		}
		names = append(names, tool.Name)
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
	if grant == nil || tool.Spec.HTTP.URL != grant.Endpoint+"/v1/task-tools/"+tool.Name {
		return errors.New("gateway task pass endpoint is not bound")
	}
	httpClient, err := gatewayruntime.NewAdapterHTTPClient(e.HTTPClient, 30*time.Second, grant.ServiceRef, e.GatewayService.Config.AllowInsecureLoopback)
	if err != nil {
		return err
	}
	executor.SetGatewayTaskPass(func(callCtx context.Context, _ *corev1alpha1.Tool) (string, error) {
		return e.GatewayService.TaskPass(callCtx, task, grant, tool.Name)
	}, httpClient)
	executor.SetSendGate(func(callCtx context.Context) error {
		return e.GatewayService.ValidateTaskGrant(callCtx, task, grant, tool.Name)
	})
	return nil
}
