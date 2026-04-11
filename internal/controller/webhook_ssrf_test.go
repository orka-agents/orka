/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"testing"
)

func TestIsAllowedWebhookURL_ValidURLs(t *testing.T) {
	validURLs := []string{
		"https://example.com/webhook",
		"http://public.example.org/notify",
		"https://api.example.com:8080/hook",
		"http://receiver.default.svc.cluster.local/webhook",
		"http://receiver.default.svc:8080/webhook",
	}

	for _, url := range validURLs {
		t.Run(url, func(t *testing.T) {
			err := isAllowedWebhookURL(url)
			if err != nil {
				t.Errorf("isAllowedWebhookURL(%q) returned error: %v", url, err)
			}
		})
	}
}

func TestIsAllowedWebhookURL_BlockedURLs(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"metadata endpoint", "http://169.254.169.254/latest/meta-data"},
		{"google metadata", "http://metadata.google.internal/computeMetadata/v1/"},
		{"kubernetes default", "https://kubernetes.default/api"},
		{"kubernetes default svc", "https://kubernetes.default.svc/api"},
		{"kubernetes default svc cluster local", "https://kubernetes.default.svc.cluster.local/api"},
		{"localhost", "http://localhost:8080/webhook"},
		{"127.0.0.1", "http://127.0.0.1/webhook"},
		{"loopback IPv6", "http://[::1]/webhook"},
		{"private IP 10.x", "http://10.0.0.1/webhook"},
		{"private IP 172.16.x", "http://172.16.0.1/webhook"},
		{"private IP 192.168.x", "http://192.168.1.1/webhook"},
		{"file scheme", "file:///etc/passwd"},
		{"ftp scheme", "ftp://internal.example.com/data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := isAllowedWebhookURL(tt.url)
			if err == nil {
				t.Errorf("isAllowedWebhookURL(%q) should have returned error but didn't", tt.url)
			}
		})
	}
}

func TestIsAllowedWebhookURL_InvalidURLs(t *testing.T) {
	invalidURLs := []string{
		"not-a-url",
		"://missing-scheme",
		"",
	}

	for _, url := range invalidURLs {
		t.Run(url, func(t *testing.T) {
			err := isAllowedWebhookURL(url)
			if err == nil {
				t.Errorf("isAllowedWebhookURL(%q) should have returned error for invalid URL", url)
			}
		})
	}
}
