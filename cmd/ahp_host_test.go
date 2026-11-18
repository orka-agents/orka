/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"testing"

	"github.com/orka-agents/orka/internal/api"
)

func TestValidateAHPSignInRequiresOIDC(t *testing.T) {
	for name, oidc := range map[string]api.OIDCConfig{
		"none":        {},
		"no audience": {Issuer: "https://issuer.example", AllowedSubjects: []string{"*"}},
		"no issuer":   {Audience: "orka", AllowedSubjects: []string{"*"}},
		// Every sign-in is refused without a subject allowlist.
		"no allowed subjects":    {Issuer: "https://issuer.example", Audience: "orka"},
		"blank allowed subjects": {Issuer: "https://issuer.example", Audience: "orka", AllowedSubjects: []string{" "}},
	} {
		if validateAHPSignIn(oidc) == nil {
			t.Errorf("%s: an AHP endpoint no one could sign in to started", name)
		}
	}
	signIn := api.OIDCConfig{Issuer: "https://issuer.example", Audience: "orka", AllowedSubjects: []string{"*"}}
	if err := validateAHPSignIn(signIn); err != nil {
		t.Fatalf("OIDC sign-in refused: %v", err)
	}
}

func TestValidateAHPProvenance(t *testing.T) {
	if validateAHPProvenance(false) == nil {
		t.Error("an AHP endpoint whose Task requesters could be forged started")
	}
	if err := validateAHPProvenance(true); err != nil {
		t.Errorf("provenance-protected AHP endpoint refused: %v", err)
	}
}

func TestValidateAHPWorkspaceClass(t *testing.T) {
	for _, tt := range []struct {
		class                 string
		providerAPI, dispatch bool
		ok                    bool
	}{
		{"", false, false, true},
		{"warm", true, true, true},
		{"warm", false, true, false},
		{"warm", true, false, false},
	} {
		if err := validateAHPWorkspaceClass(tt.class, tt.providerAPI, tt.dispatch); (err == nil) != tt.ok {
			t.Errorf("class %q, provider API %v, dispatch %v: err = %v, want ok %v",
				tt.class, tt.providerAPI, tt.dispatch, err, tt.ok)
		}
	}
}

func TestValidateAHPWorkspace(t *testing.T) {
	digest := "ghcr.io/example/git@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name string
		cfg  api.AHPBackendConfig
		ok   bool
	}{
		{"disabled", api.AHPBackendConfig{}, true},
		{"pinned", api.AHPBackendConfig{WorkspaceImage: digest, WorkspaceNamespace: "orka-ahp-workspaces"}, true},
		{"tag", api.AHPBackendConfig{
			WorkspaceImage: "ghcr.io/example/git:latest", WorkspaceNamespace: "orka-ahp-workspaces",
		}, false},
		{"watch namespace", api.AHPBackendConfig{WorkspaceImage: digest, WorkspaceNamespace: "orka-system"}, false},
		{"control namespace", api.AHPBackendConfig{WorkspaceImage: digest, WorkspaceNamespace: "orka-control"}, false},
		{"ACP runtime namespace", api.AHPBackendConfig{WorkspaceImage: digest, WorkspaceNamespace: "orka-runtimes"}, false},
		{"empty namespace", api.AHPBackendConfig{WorkspaceImage: digest}, false},
	} {
		err := validateAHPWorkspace(test.cfg, "orka-system", "orka-control", "orka-runtimes")
		if (err == nil) != test.ok {
			t.Errorf("%s: err = %v, want ok=%v", test.name, err, test.ok)
		}
	}
}
