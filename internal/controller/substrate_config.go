/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"fmt"
	"strings"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const (
	EnvSubstrateAPIEndpoint           = "ORKA_SUBSTRATE_API_ENDPOINT"
	EnvSubstrateAPICAFile             = "ORKA_SUBSTRATE_API_CA_FILE"
	EnvSubstrateAPIInsecureSkipVerify = "ORKA_SUBSTRATE_API_INSECURE_SKIP_VERIFY"
	EnvSubstrateRouterURL             = "ORKA_SUBSTRATE_ROUTER_URL"
	EnvSubstrateActorDNSSuffix        = "ORKA_SUBSTRATE_ACTOR_DNS_SUFFIX"
	EnvSubstrateDefaultTemplate       = "ORKA_SUBSTRATE_DEFAULT_TEMPLATE"
	EnvSubstrateDefaultTemplateNS     = "ORKA_SUBSTRATE_DEFAULT_TEMPLATE_NAMESPACE"
	EnvSubstrateClaimTimeout          = "ORKA_SUBSTRATE_CLAIM_TIMEOUT"
	EnvSubstrateCommandTimeout        = "ORKA_SUBSTRATE_COMMAND_TIMEOUT"
	EnvSubstrateCleanupPolicy         = "ORKA_SUBSTRATE_CLEANUP_POLICY"
)

const (
	defaultSubstrateAPIEndpoint    = "api.ate-system.svc:443"
	defaultSubstrateRouterURL      = "http://atenet-router.ate-system.svc"
	defaultSubstrateActorDNSSuffix = "actors.resources.substrate.ate.dev"
	defaultSubstrateClaimTimeout   = 2 * time.Minute
	defaultSubstrateCommandTimeout = 30 * time.Minute
)

// SubstrateConfig holds disabled-by-default alpha configuration for the
// Agent Substrate execution workspace provider.
type SubstrateConfig struct {
	APIEndpoint           string
	APICAFile             string
	APIInsecureSkipVerify bool
	RouterURL             string
	ActorDNSSuffix        string
	DefaultTemplate       string
	DefaultTemplateNS     string
	ClaimTimeout          time.Duration
	CommandTimeout        time.Duration
	CleanupPolicy         corev1alpha1.WorkspaceCleanupPolicy
}

// DefaultSubstrateConfig returns safe alpha defaults. Substrate is still
// disabled unless the controller feature gate is explicitly enabled.
func DefaultSubstrateConfig() SubstrateConfig {
	return SubstrateConfig{
		APIEndpoint:    defaultSubstrateAPIEndpoint,
		RouterURL:      defaultSubstrateRouterURL,
		ActorDNSSuffix: defaultSubstrateActorDNSSuffix,
		ClaimTimeout:   defaultSubstrateClaimTimeout,
		CommandTimeout: defaultSubstrateCommandTimeout,
		CleanupPolicy:  corev1alpha1.WorkspaceCleanupPolicyDelete,
	}
}

// SubstrateConfigFromEnv loads Substrate config defaults from process env.
func SubstrateConfigFromEnv(getenv func(string) string) (SubstrateConfig, error) {
	cfg := DefaultSubstrateConfig()

	if value := strings.TrimSpace(getenv(EnvSubstrateAPIEndpoint)); value != "" {
		cfg.APIEndpoint = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateAPICAFile)); value != "" {
		cfg.APICAFile = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateAPIInsecureSkipVerify)); value != "" {
		cfg.APIInsecureSkipVerify = strings.EqualFold(value, "true")
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateRouterURL)); value != "" {
		cfg.RouterURL = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateActorDNSSuffix)); value != "" {
		cfg.ActorDNSSuffix = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateDefaultTemplate)); value != "" {
		cfg.DefaultTemplate = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateDefaultTemplateNS)); value != "" {
		cfg.DefaultTemplateNS = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateClaimTimeout)); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return cfg, fmt.Errorf("parse %s: %w", EnvSubstrateClaimTimeout, err)
		}
		cfg.ClaimTimeout = duration
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateCommandTimeout)); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil {
			return cfg, fmt.Errorf("parse %s: %w", EnvSubstrateCommandTimeout, err)
		}
		cfg.CommandTimeout = duration
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateCleanupPolicy)); value != "" {
		cfg.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicy(value)
	}

	return cfg.WithDefaults(), nil
}

// WithDefaults fills unset optional fields with safe defaults.
func (c SubstrateConfig) WithDefaults() SubstrateConfig {
	if strings.TrimSpace(c.APIEndpoint) == "" {
		c.APIEndpoint = defaultSubstrateAPIEndpoint
	}
	if strings.TrimSpace(c.RouterURL) == "" {
		c.RouterURL = defaultSubstrateRouterURL
	}
	if strings.TrimSpace(c.ActorDNSSuffix) == "" {
		c.ActorDNSSuffix = defaultSubstrateActorDNSSuffix
	}
	if c.ClaimTimeout == 0 {
		c.ClaimTimeout = defaultSubstrateClaimTimeout
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = defaultSubstrateCommandTimeout
	}
	if c.CleanupPolicy == "" {
		c.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicyDelete
	}
	return c
}

// Validate rejects unsupported Substrate config values.
func (c SubstrateConfig) Validate() error {
	cfg := c.WithDefaults()

	if strings.TrimSpace(cfg.APIEndpoint) == "" {
		return fmt.Errorf("substrate API endpoint is required")
	}
	if strings.TrimSpace(cfg.RouterURL) == "" {
		return fmt.Errorf("substrate router URL is required")
	}
	if strings.TrimSpace(cfg.ActorDNSSuffix) == "" {
		return fmt.Errorf("substrate actor DNS suffix is required")
	}
	if cfg.ClaimTimeout <= 0 {
		return fmt.Errorf("substrate claim timeout must be greater than zero")
	}
	if cfg.CommandTimeout <= 0 {
		return fmt.Errorf("substrate command timeout must be greater than zero")
	}

	switch cfg.CleanupPolicy {
	case corev1alpha1.WorkspaceCleanupPolicyDelete, corev1alpha1.WorkspaceCleanupPolicyRetain:
	default:
		return fmt.Errorf("unsupported substrate cleanup policy %q", cfg.CleanupPolicy)
	}

	return nil
}
