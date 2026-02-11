/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"github.com/sozercan/mercan/internal/api"
	"github.com/sozercan/mercan/internal/controller"
	_ "github.com/sozercan/mercan/internal/llm/anthropic"
	_ "github.com/sozercan/mercan/internal/llm/openai"
	_ "github.com/sozercan/mercan/internal/metrics"
	"github.com/sozercan/mercan/internal/store/sqlite"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(corev1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var apiPort int
	var watchNamespace string
	var copilotWorkerImage string
	var claudeWorkerImage string
	var chatEnabled bool
	var chatProvider string
	var chatModel string
	var chatMaxIterations int
	var chatMaxDuration time.Duration
	var chatToolTimeout time.Duration
	var chatMaxConcurrent int
	var chatMaxTasksPerTurn int
	var chatMaxSessionSize int
	var storeBackend string
	var storePath string
	var controllerURL string
	var tlsOpts []func(*tls.Config)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.IntVar(&apiPort, "api-port", 8080, "The port the REST API server binds to.")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "Namespace to watch for resources. Empty for all namespaces.")
	flag.StringVar(&copilotWorkerImage, "copilot-worker-image",
		controller.DefaultCopilotWorkerImage, "Container image for Copilot agent worker.")
	flag.StringVar(&claudeWorkerImage, "claude-worker-image",
		controller.DefaultClaudeWorkerImage, "Container image for Claude agent worker.")
	flag.BoolVar(&chatEnabled, "chat-enabled", true, "Enable the chat endpoint.")
	flag.StringVar(&chatProvider, "chat-provider", "", "Default Provider CRD name for chat.")
	flag.StringVar(&chatModel, "chat-model", "", "Default model for chat.")
	flag.IntVar(&chatMaxIterations, "chat-max-iterations", 20, "Max tool execution loops per chat request.")
	flag.DurationVar(&chatMaxDuration, "chat-max-duration", 5*time.Minute, "Max wall-clock time per chat request.")
	flag.DurationVar(&chatToolTimeout, "chat-tool-timeout", 60*time.Second, "Max time for a single tool execution.")
	flag.IntVar(&chatMaxConcurrent, "chat-max-concurrent", 10, "Max concurrent chat sessions.")
	flag.IntVar(&chatMaxTasksPerTurn, "chat-max-tasks-per-turn", 5, "Max tasks created per chat turn.")
	flag.IntVar(&chatMaxSessionSize, "chat-max-session-size", 500*1024,
		"Soft limit for session ConfigMap size before truncation (bytes).")
	flag.StringVar(&storeBackend, "store-backend", "sqlite", "Storage backend (sqlite)")
	flag.StringVar(&storePath, "store-path", "/data/mercan.db", "Path to SQLite database file")
	flag.StringVar(&controllerURL, "controller-url", "",
		"Base URL for the controller API, used by workers. E.g. http://mercan-controller.mercan-system.svc:8080")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "03b49a10.mercan.ai",
	}

	// Set namespace scope if specified
	if watchNamespace != "" {
		mgrOptions.Cache.DefaultNamespaces = map[string]cache.Config{
			watchNamespace: {},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Create Kubernetes clientset for pod log reading
	kubeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}

	// Create SQLite store
	if storeBackend != "sqlite" {
		setupLog.Error(fmt.Errorf("unsupported store backend: %s", storeBackend), "unknown store backend")
		os.Exit(1)
	}

	db, err := sqlite.NewDB(storePath)
	if err != nil {
		setupLog.Error(err, "unable to create SQLite database", "path", storePath)
		os.Exit(1)
	}

	sqliteStore := sqlite.NewStore(db, storePath)
	if err := mgr.Add(sqliteStore); err != nil {
		setupLog.Error(err, "unable to add SQLite store as runnable")
		os.Exit(1)
	}

	// Create helper components
	sessionManager := controller.NewSessionManager(sqliteStore)
	webhookNotifier := controller.NewWebhookNotifier()
	jobBuilder := controller.NewJobBuilder(mgr.GetClient())
	jobBuilder.CopilotWorkerImage = copilotWorkerImage
	jobBuilder.ClaudeWorkerImage = claudeWorkerImage
	jobBuilder.ControllerURL = controllerURL
	priorityQueue := controller.NewPriorityQueue()

	// Setup Task controller with helper components
	if err := (&controller.TaskReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		JobBuilder:      jobBuilder,
		SessionManager:  sessionManager,
		WebhookNotifier: webhookNotifier,
		PriorityQueue:   priorityQueue,
		KubeClient:      kubeClient,
		ResultStore:     sqliteStore,
		SessionStore:    sqliteStore,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Task")
		os.Exit(1)
	}

	if err := (&controller.ToolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tool")
		os.Exit(1)
	}

	if err := (&controller.AgentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Agent")
		os.Exit(1)
	}

	if err := (&controller.ProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Provider")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Start REST API server
	apiServer := api.NewServer(mgr.GetClient(), sessionManager, api.ServerConfig{
		Port:           apiPort,
		WatchNamespace: watchNamespace,
		ResultStore:    sqliteStore,
		SessionStore:   sqliteStore,
		Chat: api.ChatConfig{
			Enabled:         chatEnabled,
			Provider:        chatProvider,
			Model:           chatModel,
			MaxIterations:   chatMaxIterations,
			MaxDuration:     chatMaxDuration,
			ToolTimeout:     chatToolTimeout,
			MaxConcurrent:   chatMaxConcurrent,
			MaxTasksPerTurn: chatMaxTasksPerTurn,
			MaxSessionSize:  chatMaxSessionSize,
		},
	})

	// Add API server as a runnable
	if err := mgr.Add(apiServer); err != nil {
		setupLog.Error(err, "unable to add API server")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
