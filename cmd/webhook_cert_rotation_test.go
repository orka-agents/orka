/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestValidateWebhookCertRotationOptions(t *testing.T) {
	complete := webhookCertRotationOptions{
		SecretName:  "orka-webhook-tls",
		WebhookName: "orka-controller",
		DNSName:     "orka-webhook.orka-system.svc",
		CertDir:     "/var/run/orka/webhook/tls",
		CertName:    "tls.crt",
		KeyName:     "tls.key",
	}
	tests := []struct {
		name    string
		mutate  func(*webhookCertRotationOptions)
		wantErr string
	}{
		{name: "disabled", mutate: func(o *webhookCertRotationOptions) { *o = webhookCertRotationOptions{} }},
		{name: "complete", mutate: func(*webhookCertRotationOptions) {}},
		{
			name:    "missing webhook name",
			mutate:  func(o *webhookCertRotationOptions) { o.WebhookName = " " },
			wantErr: "--webhook-cert-rotation-webhook",
		},
		{
			name:    "missing dns name",
			mutate:  func(o *webhookCertRotationOptions) { o.DNSName = "" },
			wantErr: "--webhook-cert-rotation-dns-name",
		},
		{
			name:    "missing cert dir",
			mutate:  func(o *webhookCertRotationOptions) { o.CertDir = "" },
			wantErr: "--webhook-cert-path",
		},
		{
			name:    "missing key file name",
			mutate:  func(o *webhookCertRotationOptions) { o.KeyName = "" },
			wantErr: "--webhook-cert-name and --webhook-cert-key",
		},
		{
			name:    "dns name with scheme or port",
			mutate:  func(o *webhookCertRotationOptions) { o.DNSName = "orka-webhook.orka-system.svc:443" },
			wantErr: "bare DNS name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := complete
			tt.mutate(&opts)
			err := validateWebhookCertRotationOptions(opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSetupWebhookCertRotationDisabledIsImmediatelyReady(t *testing.T) {
	ready, err := setupWebhookCertRotation(nil, "", webhookCertRotationOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case <-ready:
	default:
		t.Fatal("disabled rotation must return a closed ready channel")
	}
}

func TestSetupWebhookCertRotationRejectsIncompleteOptions(t *testing.T) {
	_, err := setupWebhookCertRotation(nil, "orka-system", webhookCertRotationOptions{SecretName: "orka-webhook-tls"})
	if err == nil {
		t.Fatal("incomplete rotation options were accepted")
	}
}

func TestWebhookReadyCheckerWaitsForCertificate(t *testing.T) {
	ready := make(chan struct{})
	startedErr := errors.New("server not started")
	started := func(*http.Request) error { return startedErr }
	check := webhookReadyChecker(ready, started)

	if err := check(nil); err == nil || strings.Contains(err.Error(), startedErr.Error()) {
		t.Fatalf("checker must fail on the certificate before consulting the server, got %v", err)
	}
	close(ready)
	if err := check(nil); !errors.Is(err, startedErr) {
		t.Fatalf("checker must delegate to the server once the certificate is ready, got %v", err)
	}
}
