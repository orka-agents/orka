package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/orka-agents/orka/internal/controller"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation"
	"github.com/orka-agents/orka/internal/remediation/applicationexec"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/service"
	"github.com/orka-agents/orka/internal/remediation/source"
	"github.com/orka-agents/orka/internal/storageclient"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type remediationApplicationOptions struct {
	Kubeconfig               string
	Context                  string
	WorkerImage              string
	WorkerImageID            string
	ToolchainImage           string
	ToolchainImageID         string
	GoVersion                string
	RuntimeImage             string
	RuntimeImageID           string
	Platform                 string
	StorageClass             string
	NodeSelector             string
	TargetClusterUID         string
	ControlClusterUID        string
	NetworkCanary            string
	PolicyFile               string
	RuntimeClassName         string
	APIServer                string
	SourceEndpoints          remediationSourceEndpoints
	RuntimeQualificationFile string
}

type remediationSourceEndpoints []applicationexec.SourceEndpoint

func (endpoints *remediationSourceEndpoints) String() string {
	if endpoints == nil {
		return ""
	}
	values := make([]string, 0, len(*endpoints))
	for _, endpoint := range *endpoints {
		values = append(values, endpoint.Host+"="+endpoint.Address)
	}
	return strings.Join(values, ",")
}

func (endpoints *remediationSourceEndpoints) Set(value string) error {
	host, address, ok := strings.Cut(value, "=")
	if !ok || (host != "github.com" && host != "api.github.com") {
		return errors.New("application source endpoint must be github.com=IP or api.github.com=IP")
	}
	ip := net.ParseIP(address)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || len(*endpoints) >= 8 {
		return errors.New(
			"application source endpoints require at most eight literal unicast, " +
				"non-loopback IPs on fixed port 443",
		)
	}
	*endpoints = append(*endpoints, applicationexec.SourceEndpoint{Host: host, Address: ip.String()})
	return nil
}

func (options remediationApplicationOptions) configured() bool {
	if options.Platform == "linux/amd64" {
		options.Platform = ""
	}
	return !reflect.ValueOf(options).IsZero()
}

func bindRemediationApplicationFlags(flags *flag.FlagSet, options *remediationApplicationOptions) {
	flags.StringVar(&options.Kubeconfig, "remediation-application-kubeconfig", "",
		"Separate target-cluster kubeconfig file; never uses the global kubeconfig.")
	flags.StringVar(&options.Context, "remediation-application-context", "",
		"Explicit context in the separate application target kubeconfig.")
	flags.StringVar(&options.WorkerImage, "remediation-application-worker-image", "",
		"Digest-pinned trusted application build/observation worker.")
	flags.StringVar(&options.WorkerImageID, "remediation-application-worker-image-id", "",
		"Expected CRI image ID of the application worker.")
	flags.StringVar(&options.ToolchainImage, "remediation-application-toolchain-image", "",
		"Digest-pinned offline Go toolchain image.")
	flags.StringVar(&options.ToolchainImageID, "remediation-application-toolchain-image-id", "",
		"Expected CRI image ID of the application toolchain.")
	flags.StringVar(&options.GoVersion, "remediation-application-go-version", "",
		"Exact Go compiler version required by application builds, such as go1.27.2.")
	flags.StringVar(&options.RuntimeImage, "remediation-application-runtime-image", "",
		"Operator-approved digest-pinned application runtime image (defaults to the trusted worker image).")
	flags.StringVar(&options.RuntimeImageID, "remediation-application-runtime-image-id", "",
		"Expected CRI ID of the application runtime image.")
	flags.StringVar(&options.Platform, "remediation-application-platform", "linux/amd64",
		"Application target platform (linux/amd64 or linux/arm64).")
	flags.StringVar(&options.StorageClass, "remediation-application-storage-class", "",
		"Target storage class for isolated source/build artifacts.")
	flags.StringVar(&options.NodeSelector, "remediation-application-node-selector", "",
		"Qualified dedicated application node selector key=value.")
	flags.StringVar(&options.TargetClusterUID, "remediation-application-target-cluster-uid", "",
		"Expected target kube-system Namespace UID; must differ from Orka.")
	flags.StringVar(&options.ControlClusterUID, "remediation-application-control-cluster-uid", "",
		"Expected Orka kube-system Namespace UID.")
	flags.StringVar(&options.NetworkCanary, "remediation-application-network-canary", "",
		"Reachable target-isolation qualification canary literal IP:port.")
	flags.StringVar(&options.PolicyFile, "remediation-application-policy-file", "",
		"Private operator JSON profile for admitted namespaced resources, "+
			"actors, synthetic Secrets, and fixture catalogue.")
	flags.StringVar(&options.RuntimeClassName, "remediation-application-runtime-class", "",
		"Optional target RuntimeClass name; its exact live identity and measured isolation must qualify.")
	flags.StringVar(&options.APIServer, "remediation-application-api-server", "",
		"Worker-reachable target HTTPS literal IP:port (defaults to the separate kubeconfig endpoint).")
	flags.Var(&options.SourceEndpoints, "remediation-application-source-endpoint",
		"Repeatable fixed source endpoint github.com=IP or api.github.com=IP; "+
			"HTTPS443 only, github.com required, no DNS fallback.")
	flags.StringVar(&options.RuntimeQualificationFile, "remediation-application-runtime-qualification-file", "",
		"Explicit operator-qualified runtime JSON; omission preserves native-v2, "+
			"never selects a runtime waiver by class name.")
}

func readApplicationRuntimeQualification(name string) (*pv.ApplicationRuntimeQualification, error) {
	if name == "" {
		return nil, nil
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, errors.New("unable to read the application runtime qualification")
	}
	defer func() { _ = file.Close() }()
	const maxQualificationBytes = 16 << 10
	content, err := io.ReadAll(io.LimitReader(file, maxQualificationBytes+1))
	if err != nil || len(content) > maxQualificationBytes {
		return nil, errors.New("application runtime qualification exceeds its metadata bounds")
	}
	return remediation.DecodeApplicationRuntimeQualification(content)
}

func readApplicationPolicy(name string) (*pv.ApplicationPolicy, error) {
	if name == "" {
		return nil, nil
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, errors.New("unable to read the application operator policy")
	}
	defer func() { _ = file.Close() }()
	const maxPolicyBytes = 32 << 10
	content, err := io.ReadAll(io.LimitReader(file, maxPolicyBytes+1))
	if err != nil || len(content) > maxPolicyBytes {
		return nil, errors.New("application operator policy exceeds its bounded planning context")
	}
	return remediation.DecodeApplicationPolicy(content)
}

func applicationRESTConfig(options remediationApplicationOptions) (*rest.Config, error) {
	if options.Kubeconfig == "" || options.Context == "" {
		return nil, errors.New("application-runtime requires a separate kubeconfig and explicit context")
	}
	raw, err := clientcmd.LoadFromFile(options.Kubeconfig)
	if err != nil {
		return nil, errors.New("unable to read the application target kubeconfig")
	}
	selected := raw.Contexts[options.Context]
	if selected == nil || raw.Clusters[selected.Cluster] == nil || raw.AuthInfos[selected.AuthInfo] == nil {
		return nil, errors.New("application target context must identify an explicit cluster and credential")
	}
	credentials := raw.AuthInfos[selected.AuthInfo]
	if credentials.Exec != nil || credentials.AuthProvider != nil ||
		credentials.Impersonate != "" || credentials.ImpersonateUID != "" ||
		len(credentials.ImpersonateGroups) != 0 || len(credentials.ImpersonateUserExtra) != 0 {
		return nil, errors.New("application target credentials cannot use plugins or impersonation")
	}
	cluster := raw.Clusters[selected.Cluster]
	if cluster.InsecureSkipTLSVerify || cluster.ProxyURL != "" {
		return nil, errors.New("application target requires verified TLS and a direct API endpoint")
	}
	if err := clientcmd.ResolveLocalPaths(raw); err != nil {
		return nil, errors.New("unable to resolve application target credential paths")
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(
		*raw, options.Context, &clientcmd.ConfigOverrides{}, nil,
	).ClientConfig()
	if err != nil {
		return nil, errors.New("invalid application target client configuration")
	}
	config.Timeout = 30 * time.Second
	config.UserAgent = "orka-application-remediation"
	config.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	if err := rest.LoadTLSFiles(config); err != nil || len(config.CAData) == 0 {
		return nil, errors.New("application target requires an explicit readable certificate authority")
	}
	return config, nil
}

func applicationBackendConfig(options remediationOptions, target *rest.Config) (applicationexec.Config, error) {
	app := options.Application
	key, value, ok := strings.Cut(app.NodeSelector, "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return applicationexec.Config{}, errors.New("application node selector must be key=value")
	}
	host, portText, err := net.SplitHostPort(app.NetworkCanary)
	if err != nil || net.ParseIP(host) == nil {
		return applicationexec.Config{}, errors.New("application network canary must be a literal IP:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return applicationexec.Config{}, errors.New("application network canary port is invalid")
	}
	policy, err := readApplicationPolicy(app.PolicyFile)
	if err != nil {
		return applicationexec.Config{}, err
	}
	qualification, err := readApplicationRuntimeQualification(app.RuntimeQualificationFile)
	if err != nil {
		return applicationexec.Config{}, err
	}
	workerAPIServer := app.APIServer
	if workerAPIServer == "" {
		workerAPIServer = target.Host
	}
	return applicationexec.Config{
		Enabled: true, Namespace: options.Namespace,
		WorkerImage: app.WorkerImage, WorkerImageID: app.WorkerImageID,
		ToolchainImage: app.ToolchainImage, ToolchainImageID: app.ToolchainImageID, GoVersion: app.GoVersion,
		RuntimeImage: app.RuntimeImage, RuntimeImageID: app.RuntimeImageID,
		Platform: app.Platform, StorageClass: app.StorageClass, NodeSelectorKey: key, NodeSelectorValue: value,
		TargetClusterUID: app.TargetClusterUID, ControlClusterUID: app.ControlClusterUID,
		TargetAPIServer: workerAPIServer, TargetCAData: target.CAData,
		CanaryHost: host, CanaryPort: port,
		RunTimeout: contract.MaxRunDuration, CleanupTimeout: 2 * time.Minute,
		ResourcePolicy:       policy,
		RuntimeClassName:     app.RuntimeClassName,
		SourceEndpoints:      append([]applicationexec.SourceEndpoint(nil), app.SourceEndpoints...),
		RuntimeQualification: qualification,
	}, nil
}

func setupApplicationRemediation(
	ctx context.Context, mgr manager.Manager, storage *storageclient.Client,
	builder *controller.JobBuilder, options remediationOptions,
) (*service.Service, controller.ValidationTaskReconciler, error) {
	targetConfig, err := applicationRESTConfig(options.Application)
	if err != nil {
		return nil, nil, err
	}
	config, err := applicationBackendConfig(options, targetConfig)
	if err != nil {
		return nil, nil, err
	}
	target, err := client.New(targetConfig, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, nil, errors.New("unable to construct the application target client")
	}
	targetKube, err := kubernetes.NewForConfig(targetConfig)
	if err != nil {
		return nil, nil, errors.New("unable to construct the application target Kubernetes client")
	}
	backend, err := applicationexec.New(
		mgr.GetClient(), mgr.GetAPIReader(), target, target, targetKube, storage, mgr.GetScheme(), config,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("configure application remediation: %w", err)
	}
	coordinatorConfig := service.Config{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: options.Namespace,
		AgentName: options.AgentName, AIWorkerImage: options.AIWorkerImage,
		Results: storage, Backend: backend, Source: source.Client{},
		RunTimeout: contract.MaxRunDuration, BoundedExecution: true,
	}
	if options.StorageFences == nil {
		return nil, nil, errors.New("application remediation requires controller-owned storage fencing")
	}
	coordinatorConfig.PrepareBackend = func(ctx context.Context) error {
		qualification, cancel := context.WithTimeout(ctx, config.RunTimeout+config.CleanupTimeout)
		defer cancel()
		if _, _, err := options.StorageFences.WriteFence(qualification, storagev1.StateContract); err != nil {
			return fmt.Errorf("await application remediation storage fence: %w", err)
		}
		if err := backend.CheckReadiness(qualification); err != nil {
			return fmt.Errorf("qualify application remediation boundary: %w", err)
		}
		return nil
	}
	if err := service.CheckConfiguredAuthority(ctx, coordinatorConfig, contract.ApplicationRuntime); err != nil {
		return nil, nil, fmt.Errorf("check retained application remediation authority: %w", err)
	}
	coordinator, err := service.New(coordinatorConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("configure application remediation coordinator: %w", err)
	}
	builder.ValidationJobBuilder = backend.BuildValidationJob
	if err := mgr.Add(backend.CleanupRunnable()); err != nil {
		return nil, nil, err
	}
	if err := mgr.Add(coordinator); err != nil {
		return nil, nil, err
	}
	return coordinator, backend, nil
}
