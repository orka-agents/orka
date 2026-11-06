// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
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

// externalRuntimeEgress compiles Core's runtime egress intent into the
// host-name allowlist: the in-cluster provider authentication proxy and the
// controller artifact/MCP broker API, the destinations Core's NetworkPolicy
// shape admits besides DNS (the egress gateway resolves allowlisted names).
// With --runtime-telemetry-collector-egress and telemetry enabled, the OTLP
// collector endpoints projected into the supervisor are admitted too.
// Cleartext origins become HTTP rules; TLS origins become TLSPassthrough so the
// runtime still verifies the in-cluster origin's own certificate. Any origin
// the allowlist cannot express fails admission.
func (r *RuntimePoolReconciler) externalRuntimeEgress(cfg runtimePoolConfig) (*workspacev1alpha1.RuntimeEgressPolicy, error) {
	return r.externalRuntimeEgressWithEnv(cfg, os.Getenv)
}

func (r *RuntimePoolReconciler) externalRuntimeEgressWithEnv(cfg runtimePoolConfig, getenv func(string) string) (*workspacev1alpha1.RuntimeEgressPolicy, error) {
	type destination struct {
		host string
		port int32
	}
	type origin struct{ name, raw string }
	origins := []origin{
		{name: "provider proxy", raw: cfg.providerProxy.baseURL},
		{name: "controller API", raw: r.ControllerAPIURL},
	}
	if r.EnableTelemetry && r.TelemetryCollectorEgress {
		for _, endpoint := range runtimePoolTelemetryCollectorEndpoints(true, getenv) {
			if err := validateRuntimeTelemetryCollectorOrigin(endpoint); err != nil {
				return nil, fmt.Errorf("telemetry collector origin cannot be admitted to the runtime egress allowlist: %w", err)
			}
			origins = append(origins, origin{name: "telemetry collector", raw: endpoint})
		}
	}
	granted := map[destination]workspacev1alpha1.RuntimeEgressProtocol{}
	for _, origin := range origins {
		host, port, protocol, err := externalEgressOrigin(origin.raw)
		if err != nil {
			return nil, fmt.Errorf("%s origin cannot be admitted to the runtime egress allowlist: %w", origin.name, err)
		}
		key := destination{host: host, port: port}
		if existing, ok := granted[key]; ok && existing != protocol {
			return nil, fmt.Errorf("runtime egress origins grant %s port %d with conflicting protocols", host, port)
		}
		granted[key] = protocol
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
		policy.Rules = append(policy.Rules, workspacev1alpha1.RuntimeEgressRule{Protocol: key.protocol, Host: key.host, Ports: rulePorts})
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
