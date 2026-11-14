// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// externalProviderRequiresEgressAllowlist reports whether a provider accepts
// only the host-name egress allowlist (RuntimeWorkload.Egress) and rejects a
// Kubernetes NetworkPolicy, as gateway-enforced backends such as Agent
// Substrate v1 do. Providers declare it with the shared
// runtime.egress-allowlist feature.
func externalProviderRequiresEgressAllowlist(provider *workspacev1alpha1.ExecutionWorkspaceProvider) bool {
	return provider != nil && slices.Contains(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureEgressAllowlist)
}

// externalRuntimeUsesEgressAllowlist selects the admitted network shape for a
// new workload request. Only native processes on a gateway-enforcing backend
// receive the egress allowlist; Pod-backed runtimes keep their NetworkPolicy.
// The shape is frozen for a workspace: a successor whose shape differs from
// its admitted predecessor is refused rather than silently dropping policy.
func externalRuntimeUsesEgressAllowlist(provider *workspacev1alpha1.ExecutionWorkspaceProvider, nativeProcess bool, previous *workspacev1alpha1.WorkloadRequest) (bool, error) {
	egress := nativeProcess && externalProviderRequiresEgressAllowlist(provider)
	if previous == nil {
		return egress, nil
	}
	if previous.Runtime == nil || (previous.Runtime.Egress != nil) == (previous.Runtime.NetworkPolicy != nil) {
		return false, fmt.Errorf("predecessor workload has no single admitted network shape")
	}
	if (previous.Runtime.Egress != nil) != egress {
		return false, fmt.Errorf("provider network enforcement differs from the predecessor's admitted shape; retire the workspace under its original provider release")
	}
	return egress, nil
}

// externalProviderSupportsEgressCredentialInjection reports whether the
// provider's backend gateway enforces RuntimeEgressRule.ReplaceHeaders.
func externalProviderSupportsEgressCredentialInjection(provider *workspacev1alpha1.ExecutionWorkspaceProvider) bool {
	return provider != nil && slices.Contains(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureEgressCredentialInjection)
}

// externalProviderTokenInjection names the Secret key the egress gateway
// injects as the provider-proxy bearer. A nil value means Core delivers the
// token through the runtime's sealed bootstrap instead.
type externalProviderTokenInjection struct {
	namespace, name string
}

// externalProviderTokenEgressPlaceholder is the public value a runtime whose
// provider-proxy bearer is gateway-injected sends in Authorization. The
// gateway discards it and the proxy never accepts it.
const externalProviderTokenEgressPlaceholder = harnessv2.ProviderTokenEgressInjectionPlaceholder

// bearerAuthorizationPrefix is the scheme prefix of a bearer Authorization value.
const bearerAuthorizationPrefix = "Bearer "

// externalRuntimeEgress compiles Core's runtime egress intent into the
// host-name allowlist: the in-cluster provider authentication proxy and the
// controller artifact/MCP broker API, the destinations Core's NetworkPolicy
// shape admits besides DNS (the egress gateway resolves allowlisted names).
// Cleartext origins become HTTP rules; TLS origins become TLSPassthrough so the
// runtime still verifies the in-cluster origin's own certificate. Any origin
// the allowlist cannot express fails admission.
//
// With injection, the provider-proxy origin gets its own HTTP rule that
// replaces Authorization with "Bearer " plus the token from Core's private
// provider Secret, so the token never enters the runtime. Injection needs a
// cleartext proxy origin (the gateway re-originates TLS trusting only public
// roots) that no other admitted origin shares.
func (r *RuntimePoolReconciler) externalRuntimeEgress(cfg runtimePoolConfig, injection *externalProviderTokenInjection) (*workspacev1alpha1.RuntimeEgressPolicy, error) {
	type destination struct {
		host string
		port int32
	}
	proxyHost, proxyPort, proxyProtocol, err := externalEgressOrigin(cfg.providerProxy.baseURL)
	if err == nil {
		err = externalEgressHostResolvable("--acp-provider-proxy-base-url", proxyHost)
	}
	if err != nil {
		return nil, fmt.Errorf("provider proxy origin cannot be admitted to the runtime egress allowlist: %w", err)
	}
	controllerHost, controllerPort, controllerProtocol, err := externalEgressOrigin(r.ControllerAPIURL)
	if err == nil {
		err = externalEgressHostResolvable("--controller-url", controllerHost)
	}
	if err != nil {
		return nil, fmt.Errorf("controller API origin cannot be admitted to the runtime egress allowlist: %w", err)
	}
	if injection != nil {
		if proxyProtocol != workspacev1alpha1.RuntimeEgressHTTP {
			return nil, fmt.Errorf("provider proxy token injection requires an http provider proxy origin")
		}
		if proxyHost == controllerHost {
			return nil, fmt.Errorf("provider proxy token injection requires a provider proxy host that the controller API does not share")
		}
	}
	granted := map[destination]workspacev1alpha1.RuntimeEgressProtocol{}
	for _, origin := range []struct {
		host     string
		port     int32
		protocol workspacev1alpha1.RuntimeEgressProtocol
	}{{proxyHost, proxyPort, proxyProtocol}, {controllerHost, controllerPort, controllerProtocol}} {
		key := destination{host: origin.host, port: origin.port}
		if existing, ok := granted[key]; ok && existing != origin.protocol {
			return nil, fmt.Errorf("runtime egress origins grant %s port %d with conflicting protocols", origin.host, origin.port)
		}
		granted[key] = origin.protocol
	}
	type ruleKey struct {
		host     string
		protocol workspacev1alpha1.RuntimeEgressProtocol
	}
	ports := map[ruleKey][]int32{}
	for key, protocol := range granted {
		rule := ruleKey{host: key.host, protocol: protocol}
		ports[rule] = append(ports[rule], key.port)
	}
	policy := &workspacev1alpha1.RuntimeEgressPolicy{}
	for key, rulePorts := range ports {
		slices.Sort(rulePorts)
		rule := workspacev1alpha1.RuntimeEgressRule{Protocol: key.protocol, Host: key.host, Ports: rulePorts}
		if injection != nil && key.host == proxyHost {
			rule.ReplaceHeaders = []workspacev1alpha1.RuntimeEgressCredentialHeader{{
				Header: "Authorization", Prefix: bearerAuthorizationPrefix,
				SecretKeyRef: workspacev1alpha1.RuntimeEgressSecretKeyReference{Namespace: injection.namespace, Name: injection.name, Key: runtimePoolProviderTokenKey},
			}}
		}
		policy.Rules = append(policy.Rules, rule)
	}
	// Revision hashes the request, so the order must be stable.
	slices.SortFunc(policy.Rules, func(a, b workspacev1alpha1.RuntimeEgressRule) int {
		if c := strings.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		return strings.Compare(string(a.Protocol), string(b.Protocol))
	})
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return policy, nil
}

// externalRuntimeProviderTokenInjection returns the provider-proxy token
// injection an admitted request carries, or nil. A request may inject only
// Core's provider Secret token into Authorization.
func externalRuntimeProviderTokenInjection(request *workspacev1alpha1.WorkloadRequest) (*externalProviderTokenInjection, error) {
	if request == nil || request.Runtime == nil || !request.Runtime.Egress.HasCredentialInjection() {
		return nil, nil
	}
	var found *externalProviderTokenInjection
	for _, rule := range request.Runtime.Egress.Rules {
		for _, header := range rule.ReplaceHeaders {
			ref := header.SecretKeyRef
			if found != nil || !strings.EqualFold(header.Header, "Authorization") || header.Prefix != "Bearer " || ref.Key != runtimePoolProviderTokenKey || ref.Namespace == "" {
				return nil, fmt.Errorf("runtime egress carries a credential replacement Core did not admit")
			}
			found = &externalProviderTokenInjection{namespace: ref.Namespace, name: ref.Name}
		}
	}
	return found, nil
}

func externalEgressOrigin(raw string) (string, int32, workspacev1alpha1.RuntimeEgressProtocol, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || strings.HasSuffix(parsed.Host, ":") {
		return "", 0, "", fmt.Errorf("origin must be an absolute URL without credentials")
	}
	var protocol workspacev1alpha1.RuntimeEgressProtocol
	port := int32(80)
	switch parsed.Scheme {
	case urlSchemeHTTP:
		protocol = workspacev1alpha1.RuntimeEgressHTTP
	case urlSchemeHTTPS:
		protocol, port = workspacev1alpha1.RuntimeEgressTLSPassthrough, 443
	default:
		return "", 0, "", fmt.Errorf("origin must use http or https")
	}
	if explicit := parsed.Port(); explicit != "" {
		number, err := strconv.ParseUint(explicit, 10, 16)
		if err != nil || number == 0 {
			return "", 0, "", fmt.Errorf("origin port is invalid")
		}
		port = int32(number)
	}
	// DNS names are case-insensitive; the allowlist admits only lowercase.
	host := strings.ToLower(parsed.Hostname())
	if err := workspacev1alpha1.ValidateRuntimeEgressHost(host); err != nil {
		return "", 0, "", err
	}
	return host, port, protocol, nil
}

// externalEgressHostResolvable refuses a short in-cluster Service name. A
// gateway-enforced runtime resolves allowlisted names through the backend: a
// Substrate Actor's resolver tries search suffixes its DNS relay refuses, and
// cluster DNS answers only the fully qualified "<service>.<namespace>.svc.<cluster
// domain>" form, so a "<service>.<namespace>.svc" origin is admitted but never
// connects. Core does not know the cluster domain, so it names the form to use
// rather than rewriting the host. Pod-backed runtimes, which resolve through
// their Pod's search path, never reach this check.
func externalEgressHostResolvable(flag, host string) error {
	if !strings.HasSuffix(host, ".svc") {
		return nil
	}
	return fmt.Errorf("host %q is a short Kubernetes Service name, which native workspace runtimes cannot resolve; set %s to "+
		"the fully qualified name %q (use your cluster domain if it is not cluster.local)", host, flag, host+".cluster.local")
}
