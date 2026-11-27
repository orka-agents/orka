/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/api"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/store"
)

func newAHPHost(s store.AHPStore, maxTTL time.Duration, admit func(ahphost.Principal) error) (*ahphost.Host, error) {
	backends, err := ahphost.NewRegistry()
	if err != nil {
		return nil, err
	}
	tokens := &ahphost.TokenService{Store: s, MaxTTL: maxTTL, Admit: admit}
	host, err := ahphost.New(ahphost.Config{
		Store:    s,
		Tokens:   tokens,
		Backends: backends,
		Logger:   ctrl.Log.WithName("ahp"),
	})
	if err != nil {
		return nil, err
	}
	tokens.OnRevoke = host.CloseToken
	return host, nil
}

func ahpBackendFor(host *ahphost.Host, cfg api.AHPBackendConfig) *api.AHPBackendConfig {
	if host == nil {
		return nil
	}
	return &cfg
}

func validateAHPSignIn(oidc api.OIDCConfig) error {
	if !oidc.Enabled() {
		return errors.New("--enable-ahp-api requires --oidc-issuer and --oidc-audience: AHP tokens belong to OIDC sign-ins")
	}
	if !slices.ContainsFunc(oidc.AllowedSubjects, func(subject string) bool { return strings.TrimSpace(subject) != "" }) {
		return errors.New("--enable-ahp-api requires --oidc-allowed-subjects: without it every OIDC sign-in is refused, " +
			"so no one could mint or use an AHP token")
	}
	return nil
}

func validateAHPProvenance(provenanceProtected bool) error {
	if !provenanceProtected {
		return errors.New("--enable-ahp-api requires Task provenance admission (--task-provenance-admission-enabled or " +
			"--task-provenance-admission-external): whose AHP sessions a person may read is decided from Task requesters, " +
			"which direct Task writers could otherwise forge")
	}
	return nil
}

func validateAHPWorkspaceClass(class string, providerAPI, dispatch bool) error {
	if strings.TrimSpace(class) != "" && (!providerAPI || !dispatch) {
		return errors.New("--ahp-workspace-class requires --enable-workspace-provider-api and " +
			"--acp-workspace-dispatch-enabled: AHP turns run in the class's workspace, " +
			"which the controller dispatches only with both")
	}
	return nil
}

var ahpWorkspaceImageDigest = regexp.MustCompile(`^[^@\s]+@sha256:[a-f0-9]{64}$`)

func validateAHPWorkspace(cfg api.AHPBackendConfig, watchNamespace, controlNamespace, runtimeNamespace string) error {
	if cfg.WorkspaceImage == "" {
		return nil
	}
	if !ahpWorkspaceImageDigest.MatchString(cfg.WorkspaceImage) {
		return fmt.Errorf("--ahp-workspace-image must be pinned by digest")
	}
	switch strings.TrimSpace(cfg.WorkspaceNamespace) {
	case "", watchNamespace, controlNamespace, runtimeNamespace:
		return fmt.Errorf("--ahp-workspace-namespace must name a namespace of its own")
	}
	return nil
}
