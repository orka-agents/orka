package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/gateway/workerclient"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const nativeGatewayReplyBootstrapTimeout = 10 * time.Second

func newNativeGatewayReplySender(
	ctx context.Context, newReader func() (client.Reader, error), env workerenv.AIWorkerEnv,
	tokenFile string, timeout time.Duration,
) (tools.GatewayReplySender, error) {
	if !env.GatewayReplyEnabled || !slices.Contains(env.Tools, aitools.GatewayReplyToolName) {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unavailable := errors.New("gateway reply requires an authenticated native gateway Task")
	if newReader == nil || env.TaskUID == "" || env.TaskNamespace == "" || env.TaskName == "" {
		return nil, unavailable
	}
	bootstrapCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	omit := func(reason string) (tools.GatewayReplySender, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if bootstrapCtx.Err() != nil {
			reason = "bootstrap_timeout"
		}
		// Local only: neither upstream diagnostics nor a remote event request
		// may turn this optional dependency into a leak or startup dependency.
		fmt.Fprintf(os.Stderr, "warning: reply_in_conversation omitted reason=%s\n", reason)
		return nil, nil
	}
	reader, err := newReader()
	if err != nil {
		return omit("task_read_unavailable")
	}
	if reader == nil {
		return nil, unavailable
	}
	task := &corev1alpha1.Task{}
	if err := reader.Get(
		bootstrapCtx, client.ObjectKey{Namespace: env.TaskNamespace, Name: env.TaskName}, task,
	); err != nil {
		if parentErr := ctx.Err(); parentErr != nil {
			return nil, parentErr
		}
		if apierrors.IsNotFound(err) || apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
			return nil, unavailable
		}
		// Failure to read optional bootstrap context is not an identity grant,
		// nor a reason to fail otherwise valid model work.
		return omit("task_read_unavailable")
	}
	if string(task.UID) != env.TaskUID || task.Spec.Type != corev1alpha1.TaskTypeAI ||
		labels.ParentTaskName(task.Labels, task.Annotations) != "" {
		return nil, unavailable
	}
	sender, err := workerclient.New(workerclient.Config{
		ControllerURL: env.ControllerURL, Namespace: env.TaskNamespace,
		TaskName: env.TaskName, TaskUID: env.TaskUID, TokenFile: tokenFile,
	})
	if err != nil {
		return nil, unavailable
	}
	// Authenticate durable origin, not current message admission. Readiness or
	// capability withdrawal must not hide the tool permanently after startup.
	// Optional service failures deny only this tool; explicit identity rejection
	// still fails startup. Never infer identity from a budget/capability/503 error.
	if err = sender.AuthenticateOrigin(bootstrapCtx); err != nil {
		if parentErr := ctx.Err(); parentErr != nil {
			return nil, parentErr
		}
		if errors.Is(err, workerclient.ErrUnavailable) {
			return omit("origin_unavailable")
		}
		return nil, unavailable
	}
	if bootstrapCtx.Err() != nil {
		return omit("bootstrap_timeout")
	}
	return sender, nil
}

func inClusterNativeGatewayReplyTaskReader() (client.Reader, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return newNativeGatewayReplyTaskReader(config)
}

func newNativeGatewayReplyTaskReader(config *rest.Config) (client.Reader, error) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	// The shared client's lazy discovery uses context.TODO(), not Get's context.
	// This private reader needs only the known Task CRD, so avoid discovery rather
	// than changing timeouts or mappings for every other worker tool.
	gv := corev1alpha1.GroupVersion
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	mapper.AddSpecific(gv.WithKind("Task"), gv.WithResource("tasks"), gv.WithResource("task"), meta.RESTScopeNamespace)
	config = rest.CopyConfig(config)
	config.WarningHandlerWithContext = rest.NoWarnings{}
	return client.New(config, client.Options{Scheme: scheme, Mapper: mapper})
}
