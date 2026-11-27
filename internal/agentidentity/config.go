// Copyright (c) 2026. MIT License - see LICENSE file for details.

package agentidentity

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/entraclient"
)

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type EntraConfig struct {
	TenantID          string
	BlueprintClientID string
	Authority         string
}

func (c EntraConfig) authority() string {
	return entraclient.CanonicalAuthority(c.Authority)
}

type Config struct {
	Entra                 *EntraConfig
	AgentUseAuthorization bool
	EnforcedSince         time.Time
	Enforcement           *EnforcementPeriod
}

func (c Config) EnforcementStart() (since time.Time, started bool) {
	if c.Enforcement != nil {
		return c.Enforcement.Since()
	}
	return c.EnforcedSince, true
}

func (c Config) Validate() error {
	if c.Entra == nil {
		return nil
	}
	tenant, blueprint := c.Entra.TenantID, c.Entra.BlueprintClientID
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(blueprint) == "" {
		return fmt.Errorf("the Entra agent identity issuer needs both a tenant ID and a blueprint client ID")
	}
	if !guidPattern.MatchString(tenant) || !guidPattern.MatchString(blueprint) {
		return fmt.Errorf("the Entra tenant ID and blueprint client ID must be GUIDs without surrounding space")
	}
	return entraclient.ValidateAuthority(c.Entra.Authority)
}

func (c Config) Instance(issuer string) (string, bool) {
	switch issuer {
	case corev1alpha1.AgentIdentityIssuerEntra:
		if c.Entra == nil {
			return "", false
		}
		instance := "entra:" + strings.ToLower(strings.TrimSpace(c.Entra.TenantID)) + "/" +
			strings.ToLower(strings.TrimSpace(c.Entra.BlueprintClientID))
		if authority := c.Entra.authority(); authority != DefaultEntraAuthority {
			instance += "@" + authority
		}
		return instance, true
	default:
		return "", false
	}
}

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
