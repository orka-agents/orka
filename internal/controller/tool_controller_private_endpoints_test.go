/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"strings"
	"testing"
)

func TestValidateToolHTTPURLPrivateConnectorEndpoints(t *testing.T) {
	r := &ToolReconciler{}
	if err := r.validateToolHTTPURL("https://10.96.0.10:8443/api/items", false); err == nil || !strings.Contains(err.Error(), "private/loopback") {
		t.Fatalf("without the allowance a private endpoint must be refused: %v", err)
	}
	if err := r.validateToolHTTPURL("https://10.96.0.10:8443/api/items", true); err != nil {
		t.Fatalf("with the allowance a private endpoint is accepted: %v", err)
	}
	if err := r.validateToolHTTPURL("https://127.0.0.1:8443/api/items", true); err != nil {
		t.Fatalf("loopback under the allowance: %v", err)
	}
	// The fixed metadata and API server hosts stay blocked under the allowance.
	for _, url := range []string{
		"https://169.254.169.254/latest", "https://kubernetes.default.svc/api", "https://metadata.google.internal/",
		"https://KUBERNETES.DEFAULT.SVC/api", "https://kubernetes.default.svc./api", "https://Metadata.Google.Internal./",
	} {
		if err := r.validateToolHTTPURL(url, true); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("%s under the allowance: %v", url, err)
		}
	}
	// The rest of the URL rules are untouched by the allowance.
	if err := r.validateToolHTTPURL("https://user:pw@10.96.0.10/api", true); err == nil || !strings.Contains(err.Error(), "embedded credentials") {
		t.Fatalf("embedded credentials under the allowance: %v", err)
	}
}
