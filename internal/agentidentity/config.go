// Copyright (c) 2026. MIT License - see LICENSE file for details.

// Package agentidentity holds the controller's configuration for external
// Agent identities (ADR 0039) and the checks every binding path shares.
package agentidentity

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// EntraConfig is the one Entra issuer instance an installation uses.
type EntraConfig struct {
	// TenantID is the Entra tenant that holds the blueprint.
	TenantID string
	// BlueprintClientID is the agent identity blueprint every bound identity
	// must be a child of.
	BlueprintClientID string
	// Authority is the Entra authority URL tokens are requested from;
	// DefaultEntraAuthority when empty.
	Authority string
}

// authority returns the canonical authority: no surrounding space or
// trailing slash, scheme and host in lower case, the path as given, and
// DefaultEntraAuthority when unset. Paths are case-sensitive, so they are
// never folded.
func (c EntraConfig) authority() string {
	authority := strings.TrimSuffix(strings.TrimSpace(c.Authority), "/")
	if authority == "" {
		return DefaultEntraAuthority
	}
	parsed, err := url.Parse(authority)
	if err != nil || parsed.Host == "" {
		// Validate refuses it; keep the value as given.
		return authority
	}
	parsed.Scheme, parsed.Host = strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Host)
	return parsed.String()
}

// Config is the controller-wide identity configuration.
type Config struct {
	// Entra is nil when no Entra issuer is configured.
	Entra *EntraConfig
	// AgentUseAuthorization mirrors --agent-use-authorization-enabled, which
	// identity-bound Agents require. Setting it asserts that the agent-use
	// ValidatingAdmissionPolicies are installed, which the Helm value does
	// together with the flag; the controller does not detect them.
	AgentUseAuthorization bool
	// EnforcedSince is when this controller's current, unbroken period of
	// Agent use enforcement began, by the API server's clock; see
	// StartEnforcementPeriod. Objects created before it may have been
	// created while the check was off, so no claim covers them. Zero leaves
	// claims judged by their grant time alone.
	EnforcedSince time.Time
	// Enforcement, when set, replaces EnforcedSince with a period that starts
	// once this controller leads; until then no identity binding is decided.
	Enforcement *EnforcementPeriod
}

// EnforcementStart returns when the current period of Agent use enforcement
// began, or started=false while it is not known yet.
func (c Config) EnforcementStart() (since time.Time, started bool) {
	if c.Enforcement != nil {
		return c.Enforcement.Since()
	}
	return c.EnforcedSince, true
}

// Validate rejects a partly or wrongly configured issuer at startup.
func (c Config) Validate() error {
	if c.Entra == nil {
		return nil
	}
	tenant, blueprint := c.Entra.TenantID, c.Entra.BlueprintClientID
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(blueprint) == "" {
		return fmt.Errorf("the Entra agent identity issuer needs both a tenant ID and a blueprint client ID")
	}
	// The issuer sends these values as they are, so surrounding space would
	// reach Entra; they must be bare GUIDs.
	if !guidPattern.MatchString(tenant) || !guidPattern.MatchString(blueprint) {
		return fmt.Errorf("the Entra tenant ID and blueprint client ID must be GUIDs without surrounding space")
	}
	authority, err := url.Parse(c.Entra.authority())
	// A bare "?" or "#" parses to an empty query or fragment but still changes
	// the token endpoint built from the authority, so the delimiters are
	// refused too.
	if err != nil || authority.Scheme != "https" || authority.Host == "" || authority.User != nil ||
		authority.RawQuery != "" || authority.ForceQuery || authority.Fragment != "" || strings.ContainsAny(c.Entra.Authority, "?#") {
		return fmt.Errorf("the Entra authority must be an https URL without credentials, query, or fragment")
	}
	return nil
}

// Instance returns the configured instance of an issuer type, which a Task
// binding freezes next to the identity.
func (c Config) Instance(issuer string) (string, bool) {
	switch issuer {
	case corev1alpha1.AgentIdentityIssuerEntra:
		if c.Entra == nil {
			return "", false
		}
		instance := "entra:" + strings.ToLower(strings.TrimSpace(c.Entra.TenantID)) + "/" +
			strings.ToLower(strings.TrimSpace(c.Entra.BlueprintClientID))
		// Tokens come from the authority too, so restarting against another
		// one must fail bound Tasks closed. The public cloud stays implicit.
		if authority := c.Entra.authority(); authority != DefaultEntraAuthority {
			instance += "@" + authority
		}
		return instance, true
	default:
		return "", false
	}
}

// ValidateAgent reports why an identity-bound Agent cannot be used on this
// controller. Agents without an identity always pass.
func (c Config) ValidateAgent(agent *corev1alpha1.Agent) error {
	issuer, _, ok := agent.IdentityRef()
	if !ok {
		return nil
	}
	if _, configured := c.Instance(issuer); !configured {
		return fmt.Errorf("identity issuer %q is not configured on this controller", issuer)
	}
	if !c.AgentUseAuthorization {
		return fmt.Errorf("an Agent with an identity requires --agent-use-authorization-enabled on the controller")
	}
	if agent.Spec.Runtime == nil {
		return fmt.Errorf("an Agent identity is supported only on runtime Agents")
	}
	if agent.Spec.Runtime.Ceiling == nil || agent.Spec.Runtime.Ceiling.AllowedTools == nil {
		return fmt.Errorf("an Agent with an identity must set runtime.ceiling.allowedTools")
	}
	return nil
}

// BindingRef returns the identity a Task binding freezes for the Agent, or
// nil when the Agent has none.
func (c Config) BindingRef(agent *corev1alpha1.Agent) (*corev1alpha1.AgentExecutionIdentityRef, error) {
	issuer, id, ok := agent.IdentityRef()
	if !ok {
		return nil, nil
	}
	if err := c.ValidateAgent(agent); err != nil {
		return nil, err
	}
	instance, _ := c.Instance(issuer)
	return &corev1alpha1.AgentExecutionIdentityRef{Issuer: issuer, ID: id, Instance: instance}, nil
}
