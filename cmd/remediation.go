package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/applicationexec"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/executor"
	"github.com/orka-agents/orka/internal/remediation/service"
	"github.com/orka-agents/orka/internal/remediation/source"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const defaultRemediationNodeSelector = "remediation.orka.ai/qualified=true"

type remediationOptions struct {
	Enabled              bool
	Backend              string
	Application          remediationApplicationOptions
	Namespace            string
	AgentName            string
	AIWorkerImage        string
	GeneralWorkerImage   string
	GeneralWorkerImageID string
	HelperImage          string
	HelperImageID        string
	ToolImage            string
	ToolImageID          string
	Platform             string
	NodeSelector         string
	NetworkCanary        string
}

func setupRemediation(
	ctx context.Context, mgr manager.Manager, kubeClient kubernetes.Interface,
	storage *sqlite.Store, results store.ResultStore, builder *controller.JobBuilder, options remediationOptions,
) (*service.Service, controller.ValidationTaskReconciler, error) {
	if options.Backend == "" {
		options.Backend = string(contract.SourceHarness)
	}
	if options.Backend != string(contract.SourceHarness) && options.Backend != string(contract.ApplicationRuntime) {
		return nil, nil, errors.New("unknown remediation backend")
	}
	if options.Backend != string(contract.ApplicationRuntime) && options.Application.configured() {
		return nil, nil, errors.New("application settings require explicit --remediation-backend=application-runtime")
	}
	if !options.Enabled {
		if options.Backend != string(contract.SourceHarness) {
			return nil, nil, errors.New("application-runtime remediation requires --remediation-enabled")
		}
		if err := applicationexec.EnsureNoActive(ctx, mgr.GetAPIReader(), options.Namespace); err != nil {
			return nil, nil, err
		}
		return nil, nil, service.EnsureNoActiveRuns(ctx, mgr.GetAPIReader(), options.Namespace)
	}
	if strings.TrimSpace(options.AgentName) == "" {
		return nil, nil, errors.New("remediation requires an operator-configured native Agent")
	}
	if options.Backend == string(contract.ApplicationRuntime) {
		return setupApplicationRemediation(ctx, mgr, storage, results, builder, options)
	}
	if err := applicationexec.EnsureNoActive(ctx, mgr.GetAPIReader(), options.Namespace); err != nil {
		return nil, nil, err
	}
	key, value, ok := strings.Cut(options.NodeSelector, "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return nil, nil, errors.New("remediation validation node selector must be key=value")
	}
	host, port := "", 0
	if options.NetworkCanary != "" {
		var portText string
		var err error
		host, portText, err = net.SplitHostPort(options.NetworkCanary)
		if err != nil {
			return nil, nil, errors.New("remediation network canary must be IP:port")
		}
		port, err = strconv.Atoi(portText)
		if err != nil || net.ParseIP(host) == nil || port < 1 || port > 65535 {
			return nil, nil, errors.New("remediation network canary must have a literal IP and valid port")
		}
	}
	backend, err := executor.New(
		mgr.GetClient(), mgr.GetAPIReader(), kubeClient, storage, mgr.GetScheme(), executor.Config{
			Enabled: true, Namespace: options.Namespace,
			GeneralWorkerImage: options.GeneralWorkerImage, GeneralWorkerImageID: options.GeneralWorkerImageID,
			HelperImage: options.HelperImage, HelperImageID: options.HelperImageID,
			ToolImage: options.ToolImage, ToolImageID: options.ToolImageID,
			Platform: options.Platform, Profile: pv.LocalServices, NodeSelectorKey: key, NodeSelectorValue: value,
			LocalServices: true, CanaryHost: host, CanaryPort: port,
		})
	if err != nil {
		return nil, nil, fmt.Errorf("configure remediation execution: %w", err)
	}
	coordinator, err := service.New(service.Config{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: options.Namespace,
		AgentName: options.AgentName, AIWorkerImage: options.AIWorkerImage,
		Results: results, Backend: backend, Source: source.Client{},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("configure remediation coordinator: %w", err)
	}
	if err := coordinator.CheckReady(ctx); err != nil {
		return nil, nil, fmt.Errorf("qualify remediation authority: %w", err)
	}
	builder.ValidationJobBuilder = backend.BuildValidationJob
	backend.ValidationService().SetJobRenderer(func(ctx context.Context, task *corev1alpha1.Task) (*batchv1.Job, error) {
		return builder.Build(ctx, task, nil, nil)
	})
	if err := mgr.Add(backend.ValidationService()); err != nil {
		return nil, nil, err
	}
	if err := mgr.Add(coordinator); err != nil {
		return nil, nil, err
	}
	return coordinator, backend, nil
}
