/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// orka-admission serves the stateless, fail-closed admission boundary shared
// by isolated harness-v1 and harness-v2 installations. It owns no controllers,
// dispatch state, runtime credentials, SQLite database, or leader-election
// lease.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	healthProbeBindAddress     string
	preStopDelay               time.Duration
	webhookCertPath            string
	webhookCertName            string
	webhookCertKey             string
	webhookServiceDNSName      string
	webhookPort                int
	enableHTTP2                bool
	controllerUsernames        string
	taskProvenanceTrustedUsers string
	taskProvenanceTrustedSAs   string
}

func (o *options) bind(fs *flag.FlagSet) {
	fs.StringVar(&o.healthProbeBindAddress, "health-probe-bind-address", ":8081",
		"The address on which to serve liveness and readiness probes.")
	fs.DurationVar(&o.preStopDelay, "pre-stop-delay", 0,
		"If nonzero, wait for endpoint removal and exit without starting the admission service.")
	fs.StringVar(&o.webhookCertPath, "webhook-cert-path", "",
		"The directory containing the webhook serving certificate and private key.")
	fs.StringVar(&o.webhookCertName, "webhook-cert-name", "tls.crt",
		"The webhook serving certificate filename.")
	fs.StringVar(&o.webhookCertKey, "webhook-cert-key", "tls.key",
		"The webhook serving private-key filename.")
	fs.StringVar(&o.webhookServiceDNSName, "webhook-service-dns-name", "",
		"The Kubernetes Service DNS name that the webhook serving certificate must cover.")
	fs.IntVar(&o.webhookPort, "webhook-port", 9443, "The HTTPS webhook listener port.")
	fs.BoolVar(&o.enableHTTP2, "enable-http2", false,
		"Enable HTTP/2 on the webhook listener. HTTP/2 is disabled by default.")
	fs.StringVar(&o.controllerUsernames, "controller-usernames", os.Getenv("ORKA_ADMISSION_CONTROLLER_USERNAMES"),
		"Comma-separated exact Kubernetes usernames authorized for controller-owned Task execution writes.")
	fs.StringVar(&o.taskProvenanceTrustedUsers, "task-provenance-trusted-users",
		os.Getenv("ORKA_ADMISSION_TASK_PROVENANCE_TRUSTED_USERS"),
		"Comma-separated exact Kubernetes usernames authorized to write controller-managed Task provenance; "+
			"defaults to controller-usernames.")
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
	if strings.TrimSpace(o.webhookServiceDNSName) == "" {
		return errors.New("--webhook-service-dns-name is required")
	}
	if o.webhookPort < 1 || o.webhookPort > 65535 {
		return errors.New("--webhook-port must be between 1 and 65535")
	}
	if len(splitCommaList(o.controllerUsernames)) == 0 {
		return errors.New("--controller-usernames must contain at least one exact username")
	}
	return nil
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
	if opts.preStopDelay != 0 {
		if err := runPreStopDelay(opts.preStopDelay, time.Sleep); err != nil {
			setupLog.Error(err, "invalid admission pre-stop delay")
			os.Exit(2)
		}
		return
	}
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
	// GetWebhookServer lazily adds the configured server to the manager's
	// runnable set. Registering only on the pre-manager value would not start it.
	webhookServer = mgr.GetWebhookServer()

	controllerUsernames := splitCommaList(opts.controllerUsernames)
	provenanceUsers := strings.TrimSpace(opts.taskProvenanceTrustedUsers)
	if provenanceUsers == "" {
		provenanceUsers = strings.Join(controllerUsernames, ",")
	}
	orkaadmission.RegisterTaskProvenanceWebhook(
		webhookServer,
		admissionScheme,
		orkaadmission.NewTaskProvenanceConfig(
			true,
			strings.Join(controllerUsernames, ","),
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
	orkaadmission.RegisterExecutionModeWebhooks(
		webhookServer,
		admissionScheme,
		mgr.GetAPIReader(),
		orkaadmission.ExecutionModeConfig{ControllerUsernames: controllerUsernames},
	)

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to register liveness check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook-certificates", servingCertificateFilesChecker(
		opts.webhookCertPath, opts.webhookCertName, opts.webhookCertKey, opts.webhookServiceDNSName,
	)); err != nil {
		setupLog.Error(err, "unable to register webhook certificate readiness check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook", webhookServer.StartedChecker()); err != nil {
		setupLog.Error(err, "unable to register readiness check")
		os.Exit(1)
	}

	setupLog.Info("starting stateless execution-mode admission service",
		"webhookPort", opts.webhookPort,
		"controllerUsernames", strings.Join(controllerUsernames, ","),
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "admission service stopped with an error")
		os.Exit(1)
	}
}

func runPreStopDelay(delay time.Duration, sleep func(time.Duration)) error {
	const maxDelay = 20 * time.Second
	if delay <= 0 || delay > maxDelay {
		return fmt.Errorf("--pre-stop-delay must be greater than zero and no more than %s", maxDelay)
	}
	if sleep == nil {
		return errors.New("pre-stop delay requires a sleep function")
	}
	sleep(delay)
	return nil
}

func servingCertificateFilesChecker(directory, certificateName, keyName, serviceDNSName string) healthz.Checker {
	return func(_ *http.Request) error {
		for _, name := range []string{certificateName, keyName} {
			path := filepath.Join(directory, name)
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("webhook serving certificate file %s is unavailable: %w", name, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("webhook serving certificate file %s is not a nonempty regular file", name)
			}
		}

		pair, err := tls.LoadX509KeyPair(
			filepath.Join(directory, certificateName),
			filepath.Join(directory, keyName),
		)
		if err != nil {
			return fmt.Errorf("load webhook serving certificate and private key: %w", err)
		}
		if len(pair.Certificate) == 0 {
			return errors.New("webhook serving certificate chain is empty")
		}

		now := time.Now()
		for index, rawCertificate := range pair.Certificate {
			certificate, err := x509.ParseCertificate(rawCertificate)
			if err != nil {
				return fmt.Errorf("parse webhook serving certificate chain entry %d: %w", index, err)
			}
			if now.Before(certificate.NotBefore) {
				return fmt.Errorf("webhook serving certificate chain entry %d is not valid before %s",
					index, certificate.NotBefore.UTC().Format(time.RFC3339))
			}
			if !now.Before(certificate.NotAfter) {
				return fmt.Errorf("webhook serving certificate chain entry %d expired at %s",
					index, certificate.NotAfter.UTC().Format(time.RFC3339))
			}
			if index == 0 {
				if err := certificate.VerifyHostname(strings.TrimSpace(serviceDNSName)); err != nil {
					return fmt.Errorf("webhook serving certificate is not valid for %s: %w", serviceDNSName, err)
				}
			}
		}
		return nil
	}
}
