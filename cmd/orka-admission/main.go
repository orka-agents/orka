/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// orka-admission serves the stateless, fail-closed admission boundary used
// during harness v1/v2 coexistence. It intentionally owns no controllers,
// dispatch state, runtime credentials, SQLite database, or leader-election
// lease.
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	orkaadmission "github.com/orka-agents/orka/internal/admission"
	"github.com/orka-agents/orka/internal/controller"
)

var admissionScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(admissionScheme))
	utilruntime.Must(corev1alpha1.AddToScheme(admissionScheme))
}

type options struct {
	healthProbeBindAddress         string
	webhookCertPath                string
	webhookCertName                string
	webhookCertKey                 string
	webhookPort                    int
	enableHTTP2                    bool
	controllerUsername             string
	adjudicationControllerUsername string
	classificationUsernames        string
	adminGroups                    string
	taskProvenanceTrustedUsers     string
	taskProvenanceTrustedSAs       string
}

func (o *options) bind(fs *flag.FlagSet) {
	fs.StringVar(&o.healthProbeBindAddress, "health-probe-bind-address", ":8081",
		"The address on which to serve liveness and readiness probes.")
	fs.StringVar(&o.webhookCertPath, "webhook-cert-path", "",
		"The directory containing the webhook serving certificate and private key.")
	fs.StringVar(&o.webhookCertName, "webhook-cert-name", "tls.crt",
		"The webhook serving certificate filename.")
	fs.StringVar(&o.webhookCertKey, "webhook-cert-key", "tls.key",
		"The webhook serving private-key filename.")
	fs.IntVar(&o.webhookPort, "webhook-port", 9443, "The HTTPS webhook listener port.")
	fs.BoolVar(&o.enableHTTP2, "enable-http2", false,
		"Enable HTTP/2 on the webhook listener. HTTP/2 is disabled by default.")
	fs.StringVar(&o.controllerUsername, "controller-username", os.Getenv("ORKA_ADMISSION_CONTROLLER_USERNAME"),
		"Exact Kubernetes username authorized for controller-owned execution binding writes.")
	fs.StringVar(&o.adjudicationControllerUsername, "adjudication-controller-username",
		os.Getenv("ORKA_ADMISSION_ADJUDICATION_CONTROLLER_USERNAME"),
		"Exact Kubernetes username authorized for adjudication status and "+
			"resolution-reference writes; defaults to controller-username.")
	fs.StringVar(&o.classificationUsernames, "classification-usernames",
		os.Getenv("ORKA_ADMISSION_CLASSIFICATION_USERNAMES"),
		"Comma-separated exact Kubernetes usernames authorized for one-time bridge classification writes.")
	fs.StringVar(&o.adminGroups, "admin-groups",
		defaultString(os.Getenv("ORKA_ADMISSION_ADMIN_GROUPS"), "system:masters"),
		"Comma-separated Kubernetes groups authorized to create adjudications and author execution control or policy specs.")
	fs.StringVar(&o.taskProvenanceTrustedUsers, "task-provenance-trusted-users",
		os.Getenv("ORKA_ADMISSION_TASK_PROVENANCE_TRUSTED_USERS"),
		"Comma-separated exact Kubernetes usernames authorized to write "+
			"controller-managed Task provenance; defaults to controller-username.")
	fs.StringVar(&o.taskProvenanceTrustedSAs, "task-provenance-trusted-service-accounts",
		os.Getenv("ORKA_ADMISSION_TASK_PROVENANCE_TRUSTED_SERVICE_ACCOUNTS"),
		"Comma-separated ServiceAccount names trusted in each Task namespace to write managed provenance.")
}

func (o options) validate() error {
	if strings.TrimSpace(o.webhookCertPath) == "" {
		return errors.New("--webhook-cert-path is required")
	}
	if err := validateFilename("--webhook-cert-name", o.webhookCertName); err != nil {
		return err
	}
	if err := validateFilename("--webhook-cert-key", o.webhookCertKey); err != nil {
		return err
	}
	if o.webhookPort < 1 || o.webhookPort > 65535 {
		return fmt.Errorf("--webhook-port must be between 1 and 65535")
	}
	if strings.TrimSpace(o.controllerUsername) == "" {
		return errors.New("--controller-username is required")
	}
	if len(splitCommaList(o.adminGroups)) == 0 {
		return errors.New("--admin-groups must contain at least one group")
	}
	return nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func validateFilename(flagName, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "." || trimmed != filepath.Base(trimmed) {
		return fmt.Errorf("%s must be a nonempty filename without path components", flagName)
	}
	return nil
}

func splitCommaList(raw string) []string {
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func main() {
	var opts options
	opts.bind(flag.CommandLine)
	zapOptions := zap.Options{Development: false}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOptions)))
	setupLog := ctrl.Log.WithName("setup")
	if err := opts.validate(); err != nil {
		setupLog.Error(err, "invalid admission configuration")
		os.Exit(2)
	}

	tlsOptions := make([]func(*tls.Config), 0, 1)
	if !opts.enableHTTP2 {
		tlsOptions = append(tlsOptions, func(config *tls.Config) {
			config.NextProtos = []string{"http/1.1"}
		})
	}
	webhookServer := webhook.NewServer(webhook.Options{
		Port:     opts.webhookPort,
		CertDir:  opts.webhookCertPath,
		CertName: opts.webhookCertName,
		KeyName:  opts.webhookCertKey,
		TLSOpts:  tlsOptions,
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 admissionScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: opts.healthProbeBindAddress,
		LeaderElection:         false,
		WebhookServer:          webhookServer,
	})
	if err != nil {
		setupLog.Error(err, "unable to create admission manager")
		os.Exit(1)
	}

	adjudicationUsername := strings.TrimSpace(opts.adjudicationControllerUsername)
	if adjudicationUsername == "" {
		adjudicationUsername = strings.TrimSpace(opts.controllerUsername)
	}
	provenanceUsers := strings.TrimSpace(opts.taskProvenanceTrustedUsers)
	if provenanceUsers == "" {
		provenanceUsers = strings.TrimSpace(opts.controllerUsername)
	}
	orkaadmission.RegisterTaskProvenanceWebhook(
		webhookServer,
		admissionScheme,
		orkaadmission.NewTaskProvenanceConfig(
			true,
			provenanceUsers,
			opts.taskProvenanceTrustedSAs,
			"",
		),
	)
	orkaadmission.RegisterWorkspaceClassUseWebhooks(
		webhookServer,
		admissionScheme,
		controller.WorkspaceClassAuthorizer{Client: mgr.GetClient()},
	)
	orkaadmission.RegisterCoexistenceWebhooks(
		webhookServer,
		admissionScheme,
		mgr.GetAPIReader(),
		orkaadmission.CoexistenceConfig{
			ControllerUsername:             opts.controllerUsername,
			AdjudicationControllerUsername: adjudicationUsername,
			ClassificationUsernames:        splitCommaList(opts.classificationUsernames),
			AdminGroups:                    splitCommaList(opts.adminGroups),
		},
	)
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to register liveness check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook-certificates", servingCertificateFilesChecker(
		opts.webhookCertPath, opts.webhookCertName, opts.webhookCertKey,
	)); err != nil {
		setupLog.Error(err, "unable to register webhook certificate readiness check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook", webhookServer.StartedChecker()); err != nil {
		setupLog.Error(err, "unable to register readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting stateless coexistence admission service",
		"webhookPort", opts.webhookPort,
		"controllerUsername", strings.TrimSpace(opts.controllerUsername),
		"adjudicationControllerUsername", adjudicationUsername,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "admission service stopped with an error")
		os.Exit(1)
	}
}

func servingCertificateFilesChecker(directory string, names ...string) healthz.Checker {
	return func(_ *http.Request) error {
		for _, name := range names {
			path := filepath.Join(directory, name)
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("webhook serving certificate file %s is unavailable: %w", name, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("webhook serving certificate file %s is not a nonempty regular file", name)
			}
		}
		return nil
	}
}
