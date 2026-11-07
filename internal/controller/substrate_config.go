/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
)

const (
	EnvSubstrateAPIEndpoint           = "ORKA_SUBSTRATE_API_ENDPOINT"
	EnvSubstrateAPICAFile             = "ORKA_SUBSTRATE_API_CA_FILE"
	EnvSubstrateAPICertFile           = "ORKA_SUBSTRATE_API_CERT_FILE"
	EnvSubstrateAPIKeyFile            = "ORKA_SUBSTRATE_API_KEY_FILE"
	EnvSubstrateAPIBearerTokenFile    = "ORKA_SUBSTRATE_API_BEARER_TOKEN_FILE"
	EnvSubstrateAPIInsecureSkipVerify = "ORKA_SUBSTRATE_API_INSECURE_SKIP_VERIFY"
	EnvSubstrateRouterURL             = "ORKA_SUBSTRATE_ROUTER_URL"
	EnvSubstrateDefaultTemplate       = "ORKA_SUBSTRATE_DEFAULT_TEMPLATE"
	EnvSubstrateDefaultTemplateNS     = "ORKA_SUBSTRATE_DEFAULT_TEMPLATE_NAMESPACE"
	EnvSubstrateBootstrapSecretName   = "ORKA_SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME"
	EnvSubstrateBootstrapSecretKey    = "ORKA_SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY"
	EnvSubstrateClaimTimeout          = "ORKA_SUBSTRATE_CLAIM_TIMEOUT"
	EnvSubstrateCommandTimeout        = "ORKA_SUBSTRATE_COMMAND_TIMEOUT"
	EnvSubstrateCleanupPolicy         = "ORKA_SUBSTRATE_CLEANUP_POLICY"
)

const (
	defaultSubstrateAPIEndpoint    = "api.ate-system.svc:443"
	defaultSubstrateRouterURL      = "https://atenet-router.ate-system.svc:443"
	defaultSubstrateBootstrapKey   = "token"
	defaultSubstrateClaimTimeout   = 2 * time.Minute
	defaultSubstrateCommandTimeout = 30 * time.Minute
)

// SubstrateConfig holds disabled-by-default alpha configuration for the
// in-tree Substrate MCP Tool support.
type SubstrateConfig struct {
	APIEndpoint        string
	APICAFile          string
	APICertFile        string
	APIKeyFile         string
	APIBearerTokenFile string
	// Atespace is selected from the immutable workspace binding by the controller.
	Atespace              string
	APIInsecureSkipVerify bool
	RouterURL             string
	DefaultTemplate       string
	DefaultTemplateNS     string
	BootstrapSecretName   string
	BootstrapSecretKey    string
	ClaimTimeout          time.Duration
	CommandTimeout        time.Duration
	CleanupPolicy         corev1alpha1.WorkspaceCleanupPolicy
	// EnableTelemetry mirrors --enable-telemetry: Substrate control and router
	// clients then propagate Task trace context and record client spans.
	EnableTelemetry bool
}

// DefaultSubstrateConfig returns safe alpha defaults. Substrate is still
// disabled unless the controller feature gate is explicitly enabled.
func DefaultSubstrateConfig() SubstrateConfig {
	return SubstrateConfig{
		APIEndpoint:    defaultSubstrateAPIEndpoint,
		RouterURL:      defaultSubstrateRouterURL,
		ClaimTimeout:   defaultSubstrateClaimTimeout,
		CommandTimeout: defaultSubstrateCommandTimeout,
		CleanupPolicy:  corev1alpha1.WorkspaceCleanupPolicyDelete,
	}
}

// ErrRemovedSubstrateSetting marks a configured Substrate v0.1.0 setting.
// Startup rejects it unconditionally, independently of MCP Tool admission and
// recovery state, so an obsolete operator setting is never silently ignored.
var ErrRemovedSubstrateSetting = errors.New("removed Substrate setting")

// removedSubstrateEnv names settings from the Substrate v0.1.0 integration.
// Setting any of them fails startup instead of silently changing behavior.
var removedSubstrateEnv = []string{
	"ORKA_SUBSTRATE_ACTOR_DNS_SUFFIX",
	"ORKA_SUBSTRATE_SESSION_IDENTITY_TOKEN_SECRET_NAME",
	"ORKA_SUBSTRATE_SESSION_IDENTITY_TOKEN_SECRET_KEY",
	"ORKA_SUBSTRATE_SESSION_IDENTITY_REQUIRED",
	"ORKA_SUBSTRATE_SESSION_IDENTITY_AUDIENCE",
	"ORKA_SUBSTRATE_SESSION_IDENTITY_APP_ID",
	"ORKA_SUBSTRATE_SESSION_IDENTITY_USER_ID",
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
	if value := strings.TrimSpace(getenv(EnvSubstrateAPICertFile)); value != "" {
		cfg.APICertFile = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateAPIKeyFile)); value != "" {
		cfg.APIKeyFile = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateAPIBearerTokenFile)); value != "" {
		cfg.APIBearerTokenFile = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateAPIInsecureSkipVerify)); value != "" {
		cfg.APIInsecureSkipVerify = strings.EqualFold(value, "true")
	}

	if value := strings.TrimSpace(getenv(EnvSubstrateRouterURL)); value != "" {
		cfg.RouterURL = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateDefaultTemplate)); value != "" {
		cfg.DefaultTemplate = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateDefaultTemplateNS)); value != "" {
		cfg.DefaultTemplateNS = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateBootstrapSecretName)); value != "" {
		cfg.BootstrapSecretName = value
	}
	if value := strings.TrimSpace(getenv(EnvSubstrateBootstrapSecretKey)); value != "" {
		cfg.BootstrapSecretKey = value
	}
	for _, name := range removedSubstrateEnv {
		if strings.TrimSpace(getenv(name)) != "" {
			return cfg, fmt.Errorf("%w: %s is no longer supported: Substrate v1 routes Actors by the ate-target-actor header and Orka issues per-Actor credentials itself", ErrRemovedSubstrateSetting, name)
		}
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
	if c.ClaimTimeout == 0 {
		c.ClaimTimeout = defaultSubstrateClaimTimeout
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = defaultSubstrateCommandTimeout
	}
	if c.CleanupPolicy == "" {
		c.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicyDelete
	}
	if strings.TrimSpace(c.BootstrapSecretName) != "" && strings.TrimSpace(c.BootstrapSecretKey) == "" {
		c.BootstrapSecretKey = defaultSubstrateBootstrapKey
	}
	return c
}

// ValidateControl validates only the authenticated control API. It is enough
// for control-only work such as SubstrateActorPool finalization, which may use
// either a client certificate/key pair or a bearer token.
func (c SubstrateConfig) ValidateControl() error {
	cfg := c.WithDefaults()

	if strings.TrimSpace(cfg.APIEndpoint) == "" {
		return fmt.Errorf("substrate API endpoint is required")
	}
	if strings.TrimSpace(cfg.APICAFile) == "" && !cfg.APIInsecureSkipVerify {
		return fmt.Errorf(
			"substrate API trust requires --substrate-api-ca-file or --substrate-api-insecure-skip-verify=true",
		)
	}
	if (cfg.APICertFile == "") != (cfg.APIKeyFile == "") {
		return fmt.Errorf("substrate control authentication requires both --substrate-api-cert-file and --substrate-api-key-file")
	}
	if (cfg.APICertFile != "") == (cfg.APIBearerTokenFile != "") {
		return fmt.Errorf("substrate control authentication requires either a client certificate/key pair or --substrate-api-bearer-token-file")
	}
	return nil
}

// ValidateMCPTools validates the control API and routing used by MCP actors.
func (c SubstrateConfig) ValidateMCPTools() error {
	cfg := c.WithDefaults()

	if err := cfg.ValidateControl(); err != nil {
		return err
	}
	// The router serves only mTLS from SPIFFE IDs on its ingress allowlist,
	// so Actor traffic always needs the pod-certificate identity.
	if cfg.APICertFile == "" {
		return fmt.Errorf("substrate router access requires the pod-certificate identity in --substrate-api-cert-file and --substrate-api-key-file")
	}
	if strings.TrimSpace(cfg.RouterURL) == "" {
		return fmt.Errorf("substrate router URL is required")
	}
	if cfg.ClaimTimeout <= 0 {
		return fmt.Errorf("substrate claim timeout must be greater than zero")
	}
	return validateSubstrateRouting(cfg.RouterURL)
}

// validateSubstrateRouting checks the router URL that MCP actor requests are
// sent to. Upstream v1 serves Actor traffic only over mTLS.
func validateSubstrateRouting(routerURL string) error {
	trimmed := strings.TrimSpace(routerURL)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || parsed.Scheme != urlSchemeHTTPS {
		return fmt.Errorf("substrate router URL must be an https URL")
	}
	// Tool.status.endpoint republishes the router URL to Tool readers, and the
	// MCP path is appended to it as text.
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		strings.Contains(trimmed, "#") {
		return fmt.Errorf("substrate router URL must not contain credentials, a query, or a fragment")
	}
	return nil
}

// Validate rejects unsupported legacy Substrate workspace-agent config values.
func (c SubstrateConfig) Validate() error {
	cfg := c.WithDefaults()
	if err := cfg.ValidateMCPTools(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.BootstrapSecretName) == "" {
		return fmt.Errorf("substrate workspace bootstrap token secret name is required")
	}
	if strings.TrimSpace(cfg.BootstrapSecretKey) == "" {
		return fmt.Errorf("substrate workspace bootstrap token secret key is required")
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

// WorkspaceClientConfig keeps authentication and routing consistent across ACP,
// direct workspaces, MCP actor pools and native resource operations.
func (c SubstrateConfig) WorkspaceClientConfig() workspace.SubstrateConfig {
	c = c.WithDefaults()
	return workspace.SubstrateConfig{
		APIEndpoint: c.APIEndpoint, APICAFile: c.APICAFile,
		APICertFile: c.APICertFile, APIKeyFile: c.APIKeyFile,
		APIBearerTokenFile: c.APIBearerTokenFile, APIInsecureSkipVerify: c.APIInsecureSkipVerify,
		Atespace: c.Atespace, RouterURL: c.RouterURL,
		EnableTelemetry: c.EnableTelemetry,
	}
}
