/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	webhookCertRotationCAName       = "orka-webhook-ca"
	webhookCertRotationOrganization = "orka"
	webhookCertRotationFieldOwner   = "orka-controller"
)

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
	for _, name := range []string{o.CertName, o.KeyName} {
		if filepath.Base(name) != name || name == "." || strings.HasPrefix(name, "..") {
			return errors.New("webhook certificate and key names must be plain file names")
		}
	}
	if o.CertName == o.KeyName {
		return errors.New("webhook certificate and key names must differ")
	}
	if strings.Contains(o.DNSName, "/") || strings.Contains(o.DNSName, ":") {
		return fmt.Errorf("--webhook-cert-rotation-dns-name must be a bare DNS name, got %q", o.DNSName)
	}
	return nil
}

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
		FieldOwner:            webhookCertRotationFieldOwner,
		RequireLeaderElection: true,
	})
	if err != nil {
		return nil, fmt.Errorf("register webhook certificate rotation: %w", err)
	}
	return ready, nil
}

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
