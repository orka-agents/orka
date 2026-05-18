/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	orkaadmission "github.com/sozercan/orka/internal/admission"
	"github.com/sozercan/orka/internal/api"
	"github.com/sozercan/orka/internal/controller"
	_ "github.com/sozercan/orka/internal/llm/anthropic"
	_ "github.com/sozercan/orka/internal/llm/openai"
	_ "github.com/sozercan/orka/internal/metrics"
	"github.com/sozercan/orka/internal/store/sqlite"
	"github.com/sozercan/orka/internal/tracing"
	"github.com/sozercan/orka/internal/workerenv"
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
	var taskProvenanceAdmissionEnabled bool
	var taskProvenanceAdmissionTrustedUsers string
	var taskProvenanceAdmissionTrustedServiceAccounts string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var apiPort int
	var watchNamespace string
	var copilotWorkerImage string
	var claudeWorkerImage string
	var codexWorkerImage string
	var codexSandboxMode string
	var generalWorkerImage string
	var aiWorkerClusterRoleName string
	var vendorWorkerClusterRoleName string
	var containerWorkerClusterRoleName string
	var workerClusterRoleBindingNamePrefix string
	var chatEnabled bool
	var chatProvider string
	var chatModel string
	var chatMaxIterations int
	var chatMaxDuration time.Duration
	var chatToolTimeout time.Duration
	var chatMaxConcurrent int
	var chatMaxTasksPerTurn int
	var chatMaxSessionSize int
	var aiWorkerImage string
	var storeBackend string
	var storePath string
	var controllerURL string
	var enforceNamespaceIsolation bool
	var maxTasksPerNamespace int
	var oidcIssuer string
	var oidcAudience string
	var oidcJWKSURL string
	var contextTokenProfile string
	var contextTokenIssuer string
	var contextTokenAudience string
	var contextTokenJWKSURL string
	var contextTokenHeaders string
	var contextTokenAuthzMode string
	var contextTokenTaskCreateScopes string
	var contextTokenTaskReadScopes string
	var contextTokenTaskListScopes string
	var contextTokenTaskDeleteScopes string
	var contextTokenToolReadScopes string
	var contextTokenToolUseScopes string
	var contextTokenProviderUseScopes string
	var contextTokenSecretReadScopes string
	var contextTokenAgentReadScopes string
	var contextTokenAgentWriteScopes string
	var contextTokenMemoryReadScopes string
	var contextTokenMemoryWriteScopes string
	var contextTokenSessionReadScopes string
	var contextTokenSessionWriteScopes string
	var contextTokenSecurityReadScopes string
	var contextTokenSecurityWriteScopes string
	var contextTokenSkillReadScopes string
	var contextTokenSkillWriteScopes string
	var contextTokenTTSURL string
	var contextTokenTTSAudience string
	var contextTokenTTSTimeout string
	var contextTokenTTSTokenSource string
	var contextTokenSubjectTokenType string
	var contextTokenChildScope string
	var contextTokenOutboundScope string
	var contextTokenChildTokenTTL string
	var contextTokenToolTokenTTL string
	var enableTracing bool
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
	flag.BoolVar(&taskProvenanceAdmissionEnabled, "task-provenance-admission-enabled",
		envBool("ORKA_TASK_PROVENANCE_ADMISSION_ENABLED", false),
		"Enable validating admission that rejects untrusted direct Task writes to Orka-managed "+
			"provenance fields.")
	flag.StringVar(&taskProvenanceAdmissionTrustedUsers, "task-provenance-admission-trusted-users",
		os.Getenv("ORKA_TASK_PROVENANCE_ADMISSION_TRUSTED_USERS"),
		"Comma-separated Kubernetes usernames trusted to set Orka-managed Task provenance fields. "+
			"Defaults to the controller ServiceAccount usernames in the controller namespace.")
	flag.StringVar(&taskProvenanceAdmissionTrustedServiceAccounts,
		"task-provenance-admission-trusted-service-accounts",
		os.Getenv("ORKA_TASK_PROVENANCE_ADMISSION_TRUSTED_SERVICE_ACCOUNTS"),
		"Comma-separated ServiceAccount names trusted in the target Task namespace to set "+
			"Orka-managed Task provenance fields. Defaults to orka-worker.")
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
	flag.StringVar(&codexWorkerImage, "codex-worker-image",
		controller.DefaultCodexWorkerImage, "Container image for Codex agent worker.")
	flag.StringVar(&codexSandboxMode, "codex-sandbox-mode", "",
		"Sandbox mode for Codex agent worker. Empty uses worker default.")
	flag.StringVar(&aiWorkerImage, "ai-worker-image",
		controller.DefaultAIWorkerImage, "Container image for AI worker.")
	flag.StringVar(&generalWorkerImage, "general-worker-image",
		controller.DefaultGeneralWorkerImage, "Container image for general worker.")
	flag.StringVar(&aiWorkerClusterRoleName, "ai-worker-cluster-role-name",
		controller.DefaultAIWorkerClusterRoleName, "ClusterRole name for AI worker tasks.")
	flag.StringVar(&vendorWorkerClusterRoleName, "vendor-worker-cluster-role-name",
		controller.DefaultVendorWorkerClusterRoleName, "ClusterRole name for vendor worker tasks.")
	flag.StringVar(&containerWorkerClusterRoleName, "container-worker-cluster-role-name",
		controller.DefaultContainerWorkerClusterRoleName, "ClusterRole name for container worker tasks.")
	flag.StringVar(&workerClusterRoleBindingNamePrefix, "worker-cluster-role-binding-prefix",
		os.Getenv("ORKA_WORKER_CLUSTER_ROLE_BINDING_PREFIX"),
		"Prefix for per-namespace worker ClusterRoleBinding names. Empty uses the legacy 'orka' prefix.")
	flag.BoolVar(&chatEnabled, "chat-enabled", true, "Enable the chat endpoint.")
	flag.StringVar(&chatProvider, "chat-provider", "", "Default Provider CRD name for chat.")
	flag.StringVar(&chatModel, "chat-model", "", "Default model for chat.")
	flag.IntVar(&chatMaxIterations, "chat-max-iterations", 50, "Max tool execution loops per chat request.")
	flag.DurationVar(&chatMaxDuration, "chat-max-duration", 30*time.Minute, "Max wall-clock time per chat request.")
	flag.DurationVar(&chatToolTimeout, "chat-tool-timeout", 60*time.Second, "Max time for a single tool execution.")
	flag.IntVar(&chatMaxConcurrent, "chat-max-concurrent", 10, "Max concurrent chat sessions.")
	flag.IntVar(&chatMaxTasksPerTurn, "chat-max-tasks-per-turn", 5, "Max tasks created per chat turn.")
	flag.IntVar(&chatMaxSessionSize, "chat-max-session-size", 500*1024,
		"Soft limit for session ConfigMap size before truncation (bytes).")
	flag.StringVar(&storeBackend, "store-backend", "sqlite", "Storage backend (sqlite)")
	flag.StringVar(&storePath, "store-path", "/data/orka.db", "Path to SQLite database file")
	flag.StringVar(&controllerURL, "controller-url", "",
		"Base URL for the controller API, used by workers. E.g. http://orka-controller.orka-system.svc:8080")
	flag.BoolVar(&enforceNamespaceIsolation, "enforce-namespace-isolation", false,
		"When true, restrict users to their ServiceAccount's namespace for all operations.")
	flag.IntVar(&maxTasksPerNamespace, "max-tasks-per-namespace", 0,
		"Maximum active tasks per namespace (0 = unlimited).")
	flag.StringVar(&oidcIssuer, "oidc-issuer", os.Getenv("ORKA_OIDC_ISSUER"),
		"OIDC issuer URL for authenticating external API requests. Requires --oidc-audience when set.")
	flag.StringVar(&oidcAudience, "oidc-audience", os.Getenv("ORKA_OIDC_AUDIENCE"),
		"OIDC audience expected in external API bearer tokens. Requires --oidc-issuer when set.")
	flag.StringVar(&oidcJWKSURL, "oidc-jwks-url", os.Getenv("ORKA_OIDC_JWKS_URL"),
		"Optional OIDC JWKS URL. When empty, it is discovered from the issuer metadata.")
	flag.StringVar(&contextTokenProfile, "context-token-profile", os.Getenv("ORKA_CONTEXT_TOKEN_PROFILE"),
		"Context-token profile for external API requests (supported: kontxt).")
	flag.StringVar(&contextTokenIssuer, "context-token-issuer", os.Getenv("ORKA_CONTEXT_TOKEN_ISSUER"),
		"Context-token issuer URL. Requires --context-token-profile and --context-token-audience when set.")
	flag.StringVar(&contextTokenAudience, "context-token-audience", os.Getenv("ORKA_CONTEXT_TOKEN_AUDIENCE"),
		"Context-token audience expected in external API tokens. "+
			"Requires --context-token-profile and --context-token-issuer when set.")
	flag.StringVar(&contextTokenJWKSURL, "context-token-jwks-url", os.Getenv("ORKA_CONTEXT_TOKEN_JWKS_URL"),
		"Optional context-token JWKS URL. For kontxt, defaults to <issuer>/.well-known/jwks.json.")
	flag.StringVar(&contextTokenHeaders, "context-token-headers", os.Getenv("ORKA_CONTEXT_TOKEN_HEADERS"),
		"Comma-separated context-token headers. Use Header for raw tokens or Header:Scheme for scheme-prefixed "+
			"tokens (default for kontxt: Txn-Token; bearer opt-in: Txn-Token,Authorization:Bearer).")
	flag.StringVar(&contextTokenAuthzMode, "context-token-authz-mode", os.Getenv("ORKA_CONTEXT_TOKEN_AUTHZ_MODE"),
		"Context-token authorization mode: off, audit, or enforce. Empty defaults to off.")
	flag.StringVar(&contextTokenTaskCreateScopes, "context-token-task-create-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_TASK_CREATE_SCOPES"),
		"Comma-separated context-token scopes that authorize Task creation. Defaults to orka:tasks:create.")
	flag.StringVar(&contextTokenTaskReadScopes, "context-token-task-read-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_TASK_READ_SCOPES"),
		"Comma-separated context-token scopes that authorize Task reads and related data. Defaults to orka:tasks:get.")
	flag.StringVar(&contextTokenTaskListScopes, "context-token-task-list-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_TASK_LIST_SCOPES"),
		"Comma-separated context-token scopes that authorize Task listing. Defaults to orka:tasks:list.")
	flag.StringVar(&contextTokenTaskDeleteScopes, "context-token-task-delete-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_TASK_DELETE_SCOPES"),
		"Comma-separated context-token scopes that authorize Task deletion. Defaults to orka:tasks:delete.")
	flag.StringVar(&contextTokenToolReadScopes, "context-token-tool-read-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_TOOL_READ_SCOPES"),
		"Comma-separated context-token scopes that authorize Tool reads. Defaults to orka:tools:read.")
	flag.StringVar(&contextTokenToolUseScopes, "context-token-tool-use-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_TOOL_USE_SCOPES"),
		"Comma-separated context-token scopes that authorize Orka-managed tool execution. Defaults to orka:tools:use.")
	flag.StringVar(&contextTokenProviderUseScopes, "context-token-provider-use-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_PROVIDER_USE_SCOPES"),
		"Comma-separated context-token scopes that authorize model provider use and listing. Defaults to orka:providers:use.")
	flag.StringVar(&contextTokenSecretReadScopes, "context-token-secret-read-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_SECRET_READ_SCOPES"),
		"Comma-separated context-token scopes that authorize Secret metadata reads. Defaults to orka:secrets:read.")
	flag.StringVar(&contextTokenAgentReadScopes, "context-token-agent-read-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_AGENT_READ_SCOPES"),
		"Comma-separated context-token scopes that authorize Agent reads. Defaults to orka:agents:read.")
	flag.StringVar(&contextTokenAgentWriteScopes, "context-token-agent-write-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_AGENT_WRITE_SCOPES"),
		"Comma-separated context-token scopes that authorize Agent writes. Defaults to orka:agents:write.")
	flag.StringVar(&contextTokenMemoryReadScopes, "context-token-memory-read-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_MEMORY_READ_SCOPES"),
		"Comma-separated context-token scopes that authorize memory reads. Defaults to orka:memory:read.")
	flag.StringVar(&contextTokenMemoryWriteScopes, "context-token-memory-write-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_MEMORY_WRITE_SCOPES"),
		"Comma-separated context-token scopes that authorize memory writes. Defaults to orka:memory:write.")
	flag.StringVar(&contextTokenSessionReadScopes, "context-token-session-read-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_SESSION_READ_SCOPES"),
		"Comma-separated context-token scopes that authorize session reads. Defaults to orka:sessions:read.")
	flag.StringVar(&contextTokenSessionWriteScopes, "context-token-session-write-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_SESSION_WRITE_SCOPES"),
		"Comma-separated context-token scopes that authorize session writes. Defaults to orka:sessions:write.")
	flag.StringVar(&contextTokenSecurityReadScopes, "context-token-security-read-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_SECURITY_READ_SCOPES"),
		"Comma-separated context-token scopes that authorize security scan reads. Defaults to orka:security:read.")
	flag.StringVar(&contextTokenSecurityWriteScopes, "context-token-security-write-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_SECURITY_WRITE_SCOPES"),
		"Comma-separated context-token scopes that authorize security scan writes. Defaults to orka:security:write.")
	flag.StringVar(&contextTokenSkillReadScopes, "context-token-skill-read-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_SKILL_READ_SCOPES"),
		"Comma-separated context-token scopes that authorize Skill reads. Defaults to orka:skills:read.")
	flag.StringVar(&contextTokenSkillWriteScopes, "context-token-skill-write-scopes",
		os.Getenv("ORKA_CONTEXT_TOKEN_SKILL_WRITE_SCOPES"),
		"Comma-separated context-token scopes that authorize Skill writes. Defaults to orka:skills:write.")
	flag.StringVar(&contextTokenTTSURL, "context-token-tts-url", os.Getenv("ORKA_CONTEXT_TOKEN_TTS_URL"),
		"kontxt TTS base URL for optional token exchange/replacement.")
	flag.StringVar(&contextTokenTTSAudience, "context-token-tts-audience", os.Getenv("ORKA_CONTEXT_TOKEN_TTS_AUDIENCE"),
		"Audience to request from kontxt TTS exchanges.")
	flag.StringVar(&contextTokenTTSTimeout, "context-token-tts-timeout", os.Getenv("ORKA_CONTEXT_TOKEN_TTS_TIMEOUT"),
		"Timeout for kontxt TTS exchanges. Defaults to 5s when TTS is enabled.")
	flag.StringVar(&contextTokenTTSTokenSource, "context-token-tts-token-source",
		os.Getenv("ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE"),
		"Subject token source for kontxt TTS exchanges: serviceAccount, incoming, or none.")
	flag.StringVar(&contextTokenSubjectTokenType, "context-token-subject-token-type",
		os.Getenv("ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE"),
		"Subject token type for worker-side kontxt TTS exchanges. Defaults from token source when empty.")
	flag.StringVar(&contextTokenChildScope, "context-token-child-scope", os.Getenv("ORKA_CONTEXT_TOKEN_CHILD_SCOPE"),
		"Scope workers request for child delegated TxTokens when TTS is configured.")
	flag.StringVar(&contextTokenOutboundScope, "context-token-outbound-scope",
		os.Getenv("ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE"),
		"Scope workers request for outbound HTTP Tool TxTokens when TTS is configured.")
	flag.StringVar(&contextTokenChildTokenTTL, "context-token-child-token-ttl",
		os.Getenv("ORKA_CONTEXT_TOKEN_CHILD_TOKEN_TTL"),
		"Requested TTL for child delegation TxTokens. Defaults to 5m when TTS is enabled.")
	flag.StringVar(&contextTokenToolTokenTTL, "context-token-tool-token-ttl",
		os.Getenv("ORKA_CONTEXT_TOKEN_TOOL_TOKEN_TTL"),
		"Requested TTL for outbound tool TxTokens. Defaults to 2m when TTS is enabled.")
	flag.BoolVar(&enableTracing, "enable-tracing", false,
		"Enable OpenTelemetry tracing. Configure endpoint via OTEL_EXPORTER_OTLP_ENDPOINT env var.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	contextTokenConfig, err := api.NewContextTokenConfig(
		contextTokenProfile,
		contextTokenIssuer,
		contextTokenAudience,
		contextTokenJWKSURL,
		contextTokenHeaders,
	)
	if err != nil {
		setupLog.Error(err, "invalid context token configuration")
		os.Exit(1)
	}
	contextTokenAuthzConfig, err := api.NewContextTokenAuthorizationConfig(api.ContextTokenAuthorizationConfigOptions{
		Mode:                contextTokenAuthzMode,
		TaskCreateScopes:    contextTokenTaskCreateScopes,
		TaskReadScopes:      contextTokenTaskReadScopes,
		TaskListScopes:      contextTokenTaskListScopes,
		TaskDeleteScopes:    contextTokenTaskDeleteScopes,
		ToolReadScopes:      contextTokenToolReadScopes,
		ToolUseScopes:       contextTokenToolUseScopes,
		ProviderUseScopes:   contextTokenProviderUseScopes,
		SecretReadScopes:    contextTokenSecretReadScopes,
		AgentReadScopes:     contextTokenAgentReadScopes,
		AgentWriteScopes:    contextTokenAgentWriteScopes,
		MemoryReadScopes:    contextTokenMemoryReadScopes,
		MemoryWriteScopes:   contextTokenMemoryWriteScopes,
		SessionReadScopes:   contextTokenSessionReadScopes,
		SessionWriteScopes:  contextTokenSessionWriteScopes,
		SecurityReadScopes:  contextTokenSecurityReadScopes,
		SecurityWriteScopes: contextTokenSecurityWriteScopes,
		SkillReadScopes:     contextTokenSkillReadScopes,
		SkillWriteScopes:    contextTokenSkillWriteScopes,
	})
	if err != nil {
		setupLog.Error(err, "invalid context token authorization configuration")
		os.Exit(1)
	}
	contextTokenTTSConfig, err := api.NewContextTokenTTSConfig(
		contextTokenTTSURL,
		contextTokenTTSAudience,
		contextTokenTTSTimeout,
		contextTokenTTSTokenSource,
		contextTokenChildTokenTTL,
		contextTokenToolTokenTTL,
	)
	if err != nil {
		setupLog.Error(err, "invalid context token TTS configuration")
		os.Exit(1)
	}

	// Initialize OpenTelemetry tracing (noop when disabled)
	tracingShutdown, err := tracing.Init("orka-controller", enableTracing)
	if err != nil {
		setupLog.Error(err, "failed to initialize tracing")
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			setupLog.Error(err, "failed to shutdown tracing")
		}
	}()

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
		LeaderElectionID:       "03b49a10.orka.ai",
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

	if taskProvenanceAdmissionEnabled {
		admissionConfig := orkaadmission.NewTaskProvenanceConfig(
			true,
			taskProvenanceAdmissionTrustedUsers,
			taskProvenanceAdmissionTrustedServiceAccounts,
			currentPodNamespace(),
		)
		orkaadmission.RegisterTaskProvenanceWebhook(mgr.GetWebhookServer(), mgr.GetScheme(), admissionConfig)
		setupLog.Info("enabled Task provenance validating admission",
			"trustedUsers", strings.Join(admissionConfig.TrustedUsernames, ","),
			"trustedServiceAccounts", strings.Join(admissionConfig.TrustedServiceAccountNames, ","),
		)
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
	webhookNotifier.SetKubeClient(mgr.GetClient())
	jobBuilder := controller.NewJobBuilder(mgr.GetClient())
	jobBuilder.CopilotWorkerImage = copilotWorkerImage
	jobBuilder.ClaudeWorkerImage = claudeWorkerImage
	jobBuilder.CodexWorkerImage = codexWorkerImage
	jobBuilder.CodexSandboxMode = codexSandboxMode
	jobBuilder.AIWorkerImage = aiWorkerImage
	jobBuilder.GeneralWorkerImage = generalWorkerImage
	if contextTokenTTSConfig.Enabled() {
		jobBuilder.ContextTokenTTSURL = contextTokenTTSConfig.URL
		jobBuilder.ContextTokenTTSAudience = contextTokenTTSConfig.Audience
		jobBuilder.ContextTokenTTSTokenSource = contextTokenTTSConfig.TokenSource
		if contextTokenTTSConfig.Timeout > 0 {
			jobBuilder.ContextTokenTTSTimeout = contextTokenTTSConfig.Timeout.String()
		}
		if contextTokenTTSConfig.ChildTokenTTL > 0 {
			jobBuilder.ContextTokenChildTokenTTL = contextTokenTTSConfig.ChildTokenTTL.String()
		}
		if contextTokenTTSConfig.ToolTokenTTL > 0 {
			jobBuilder.ContextTokenToolTokenTTL = contextTokenTTSConfig.ToolTokenTTL.String()
		}
		jobBuilder.ContextTokenSubjectTokenType = contextTokenSubjectTokenType
		jobBuilder.ContextTokenChildScope = contextTokenChildScope
		jobBuilder.ContextTokenOutboundScope = contextTokenOutboundScope
	}
	setupLog.Info("worker images configured",
		"ai", aiWorkerImage,
		"copilot", copilotWorkerImage,
		"claude", claudeWorkerImage,
		"codex", codexWorkerImage,
		"codexSandboxMode", codexSandboxMode,
		"general", generalWorkerImage,
	)
	jobBuilder.ControllerURL = controllerURL
	// Auto-discover controller URL from in-cluster service if not explicitly set
	if jobBuilder.ControllerURL == "" {
		ns := os.Getenv(workerenv.PodNamespace)
		if ns == "" {
			if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
				ns = strings.TrimSpace(string(data))
			}
		}
		if ns != "" {
			jobBuilder.ControllerURL = fmt.Sprintf("http://orka.%s.svc:%d", ns, apiPort)
			setupLog.Info("auto-discovered controller URL", "url", jobBuilder.ControllerURL)
		}
	}
	// Setup Task controller with helper components
	maxTasksPerNamespaceValue := int32(maxTasksPerNamespace) //nolint:gosec // flag default is non-negative
	if err := (&controller.TaskReconciler{
		Client:                             mgr.GetClient(),
		Scheme:                             mgr.GetScheme(),
		JobBuilder:                         jobBuilder,
		SessionManager:                     sessionManager,
		WebhookNotifier:                    webhookNotifier,
		KubeClient:                         kubeClient,
		ResultStore:                        sqliteStore,
		PlanStore:                          sqliteStore,
		MessageStore:                       sqliteStore,
		ArtifactStore:                      sqliteStore,
		EnforceNamespaceIsolation:          enforceNamespaceIsolation,
		MaxTasksPerNamespace:               maxTasksPerNamespaceValue,
		AIWorkerClusterRoleName:            aiWorkerClusterRoleName,
		VendorWorkerClusterRoleName:        vendorWorkerClusterRoleName,
		ContainerWorkerClusterRoleName:     containerWorkerClusterRoleName,
		WorkerClusterRoleBindingNamePrefix: workerClusterRoleBindingNamePrefix,
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

	if err := (&controller.SkillReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Skill")
		os.Exit(1)
	}

	if err := (&controller.RepositoryScanReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		SecurityStore: sqliteStore,
		ArtifactStore: sqliteStore,
		ResultStore:   sqliteStore,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RepositoryScan")
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
		Port:                      apiPort,
		WatchNamespace:            watchNamespace,
		EnforceNamespaceIsolation: enforceNamespaceIsolation,
		OIDC: api.OIDCConfig{
			Issuer:   oidcIssuer,
			Audience: oidcAudience,
			JWKSURL:  oidcJWKSURL,
		},
		ContextTokens:             contextTokenConfig,
		ContextTokenAuthorization: contextTokenAuthzConfig,
		ResultStore:               sqliteStore,
		SessionStore:              sqliteStore,
		PlanStore:                 sqliteStore,
		MessageStore:              sqliteStore,
		ArtifactStore:             sqliteStore,
		MemoryStore:               sqliteStore,
		MemoryProposalStore:       sqliteStore,
		SecurityStore:             sqliteStore,
		HealthChecker:             sqliteStore,
		Clientset:                 kubeClient,
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

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid boolean %s=%q: %v\n", name, value, err)
		os.Exit(1)
	}
	return parsed
}

func currentPodNamespace() string {
	if namespace := strings.TrimSpace(os.Getenv(workerenv.PodNamespace)); namespace != "" {
		return namespace
	}
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
