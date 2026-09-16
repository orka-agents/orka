/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// The rotator watches ValidatingWebhookConfigurations cluster-wide and rewrites
// its own caBundle whenever the serving CA changes. The RBAC for that is
// deliberately not a kubebuilder marker: only the Helm chart's generated
// certificate mode grants it, name-scoped for writes, so raw-manifest and
// Kustomize installs that mount an operator certificate never receive it.

const (
	webhookCertRotationCAName       = "orka-webhook-ca"
	webhookCertRotationOrganization = "orka"
	webhookCertRotationFieldOwner   = "orka-controller"
)

// webhookCertRotationOptions configures controller-managed webhook serving
// certificates. When SecretName is empty the operator supplies the certificate
// and the controller only reads the mounted files.
type webhookCertRotationOptions struct {
	SecretName  string
	WebhookName string
	DNSName     string
	CertDir     string
	CertName    string
	KeyName     string
}

func (o webhookCertRotationOptions) enabled() bool {
	return strings.TrimSpace(o.SecretName) != ""
}

// validateWebhookCertRotationOptions rejects a partially configured rotation so
// a misconfigured install fails at startup instead of serving without a
// trusted certificate.
func validateWebhookCertRotationOptions(o webhookCertRotationOptions) error {
	if !o.enabled() {
		return nil
	}
	missing := make([]string, 0, 4)
	if strings.TrimSpace(o.WebhookName) == "" {
		missing = append(missing, "--webhook-cert-rotation-webhook")
	}
	if strings.TrimSpace(o.DNSName) == "" {
		missing = append(missing, "--webhook-cert-rotation-dns-name")
	}
	if strings.TrimSpace(o.CertDir) == "" {
		missing = append(missing, "--webhook-cert-path")
	}
	if strings.TrimSpace(o.CertName) == "" || strings.TrimSpace(o.KeyName) == "" {
		missing = append(missing, "--webhook-cert-name and --webhook-cert-key")
	}
	if len(missing) > 0 {
		return fmt.Errorf("--webhook-cert-rotation-secret requires %s", strings.Join(missing, ", "))
	}
	if strings.Contains(o.DNSName, "/") || strings.Contains(o.DNSName, ":") {
		return fmt.Errorf("--webhook-cert-rotation-dns-name must be a bare DNS name, got %q", o.DNSName)
	}
	return nil
}

// setupWebhookCertRotation registers the certificate rotator with the manager
// and returns a channel that closes once the serving certificate has been
// written to the Secret, projected by the kubelet into CertDir, and its CA
// injected into the webhook configuration. The rotator never writes files
// itself, so CertDir must be a mount of SecretName. Without rotation the
// channel is already closed so callers can wait on it unconditionally.
func setupWebhookCertRotation(
	mgr manager.Manager,
	namespace string,
	o webhookCertRotationOptions,
) (<-chan struct{}, error) {
	ready := make(chan struct{})
	if !o.enabled() {
		close(ready)
		return ready, nil
	}
	if err := validateWebhookCertRotationOptions(o); err != nil {
		return nil, err
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, errors.New("webhook certificate rotation requires the controller Pod namespace")
	}
	dnsName := strings.TrimSpace(o.DNSName)
	extraDNSNames := []string{}
	if strings.HasSuffix(dnsName, ".svc") {
		extraDNSNames = append(extraDNSNames, dnsName+".cluster.local")
	}
	err := rotator.AddRotator(mgr, &rotator.CertRotator{
		SecretKey: types.NamespacedName{
			Namespace: namespace,
			Name:      strings.TrimSpace(o.SecretName),
		},
		CertDir:        o.CertDir,
		CertName:       o.CertName,
		KeyName:        o.KeyName,
		CAName:         webhookCertRotationCAName,
		CAOrganization: webhookCertRotationOrganization,
		DNSName:        dnsName,
		ExtraDNSNames:  extraDNSNames,
		IsReady:        ready,
		Webhooks: []rotator.WebhookInfo{{
			Name: strings.TrimSpace(o.WebhookName),
			Type: rotator.Validating,
		}},
		FieldOwner: webhookCertRotationFieldOwner,
		// The controller is a single-writer singleton behind leader election
		// with a Recreate rollout, so only the leader mints or rotates the CA.
		RequireLeaderElection: true,
	})
	if err != nil {
		return nil, fmt.Errorf("register webhook certificate rotation: %w", err)
	}
	return ready, nil
}

// webhookReadyChecker reports the webhook server ready only after its serving
// certificate exists and the server has started. Before that the Pod stays
// unready so a fail-closed admission configuration is never pointed at a
// listener without a trusted certificate.
func webhookReadyChecker(certsReady <-chan struct{}, started healthz.Checker) healthz.Checker {
	return func(req *http.Request) error {
		select {
		case <-certsReady:
			return started(req)
		default:
			return errors.New("webhook serving certificate is not ready")
		}
	}
}
