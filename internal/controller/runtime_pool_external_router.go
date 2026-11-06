// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspace/daemonprotocol"
)

// RuntimePoolSubstrateRouter is the controller's configured Agent Substrate
// router. Routed native runtimes are reachable only through it: the router
// selects the Actor from the ate-target-actor header, ignores Host, and admits
// only allowlisted pod-certificate mTLS clients. Provider evidence can never
// choose another router authority.
type RuntimePoolSubstrateRouter struct {
	// URL is core's configured router URL (--substrate-router-url).
	URL string
	// HTTPClient carries the controller's pod-certificate mTLS identity, as
	// built by workspace.NewSubstrateRouterHTTPClient. It must not use an
	// environment proxy.
	HTTPClient *http.Client
}

var errExternalRouterUnconfigured = errors.New("routed native runtime requires the controller's Substrate router configuration")

// externalStartupRouterTarget returns the router target of routed native
// startup evidence, or "" for direct (legacy or Pod-backed) evidence.
func externalStartupRouterTarget(evidence *workspacev1alpha1.StartupEvidence) string {
	if evidence == nil || evidence.Process == nil {
		return ""
	}
	return evidence.Process.RouterTarget
}

func normalizedExternalRouterOrigin(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != urlSchemeHTTPS || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.HasSuffix(parsed.Host, ":") {
		return nil, fmt.Errorf("substrate router URL must be an https URL without credentials, query or fragment")
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	return &url.URL{Scheme: urlSchemeHTTPS, Host: net.JoinHostPort(strings.ToLower(parsed.Hostname()), port), Path: strings.TrimRight(parsed.Path, "/")}, nil
}

// pinnedEndpoint verifies that a provider-reported routed endpoint is exactly
// core's configured router URL (default HTTPS port and trailing slash
// normalized) and returns the configured origin.
func (router *RuntimePoolSubstrateRouter) pinnedEndpoint(endpoint string) (*url.URL, error) {
	if router == nil || router.HTTPClient == nil || strings.TrimSpace(router.URL) == "" {
		return nil, errExternalRouterUnconfigured
	}
	configured, err := normalizedExternalRouterOrigin(router.URL)
	if err != nil {
		return nil, err
	}
	reported, err := normalizedExternalRouterOrigin(endpoint)
	if err != nil || *reported != *configured {
		return nil, fmt.Errorf("routed startup endpoint is not the configured Substrate router: %w", workspaceprovider.ErrStaleIdentity)
	}
	return configured, nil
}

// routedHTTPClient returns a client that reaches exactly one routed native
// process: every request must address the configured router origin and
// carries the attested target. A zero timeout leaves deadlines to the caller
// (long-lived ACP streams); redirects are never followed.
func (router *RuntimePoolSubstrateRouter) routedHTTPClient(endpoint, target string, timeout time.Duration) (*http.Client, error) {
	origin, err := router.pinnedEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	namespace, name, ok := strings.Cut(target, "/")
	if !ok || namespace == "" || name == "" || strings.Contains(name, "/") || strings.TrimSpace(target) != target {
		return nil, fmt.Errorf("routed native runtime target is invalid: %w", workspaceprovider.ErrStaleIdentity)
	}
	base := router.HTTPClient.Transport
	if base == nil {
		return nil, fmt.Errorf("substrate router client requires an explicit mTLS transport")
	}
	routed := *router.HTTPClient
	routed.Transport = &externalRouterTargetTransport{base: base, scheme: origin.Scheme, host: origin.Host, target: target}
	routed.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	routed.Jar = nil
	routed.Timeout = timeout
	return &routed, nil
}

// externalRouterTargetTransport adds the router target to requests for the
// configured router origin and refuses any other destination, so a routed
// client can never carry controller credentials elsewhere.
type externalRouterTargetTransport struct {
	base   http.RoundTripper
	scheme string
	host   string
	target string
}

func (t *externalRouterTargetTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	port := request.URL.Port()
	if port == "" && request.URL.Scheme == urlSchemeHTTPS {
		port = "443"
	}
	if request.URL.Scheme != t.scheme || request.URL.User != nil ||
		net.JoinHostPort(strings.ToLower(request.URL.Hostname()), port) != t.host {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, fmt.Errorf("routed native runtime request does not address the configured Substrate router")
	}
	routed := request.Clone(request.Context())
	routed.Header.Set(daemonprotocol.TargetActorHeader, t.target)
	return t.base.RoundTrip(routed)
}

// DelegatesHTTPTrace reports that request contexts, and so httptrace hooks,
// reach an *http.Transport unchanged; harness v2 keeps its exact
// written/unwritten request evidence for routed calls.
func (t *externalRouterTargetTransport) DelegatesHTTPTrace() bool {
	return harnessv2.TransportSupportsHTTPTrace(t.base)
}

// externalRoutedPoolHTTPClient returns the routed client for an external pool
// whose bound native evidence names a router target, nil for every other pool,
// and an error when routed evidence cannot be reached through the configured
// router.
func externalRoutedPoolHTTPClient(router *RuntimePoolSubstrateRouter, pool *corev1alpha1.RuntimePool, timeout time.Duration) (*http.Client, error) {
	if !runtimePoolHasExternalWorkspace(pool) {
		return nil, nil
	}
	evidence, err := externalRuntimeEvidence(pool)
	if err != nil {
		return nil, err
	}
	if evidence == nil || evidence.RouterTarget == "" {
		return nil, nil
	}
	return router.routedHTTPClient(evidence.Endpoint, evidence.RouterTarget, timeout)
}

// externalStartupHTTPClient returns the routed client for routed startup
// evidence and nil for direct evidence.
func externalStartupHTTPClient(router *RuntimePoolSubstrateRouter, evidence *workspacev1alpha1.StartupEvidence, timeout time.Duration) (*http.Client, error) {
	target := externalStartupRouterTarget(evidence)
	if target == "" {
		return nil, nil
	}
	return router.routedHTTPClient(evidence.Endpoint, target, timeout)
}

// unavailableSupervisorClient fails every call closed when a routed runtime's
// transport cannot be established.
type unavailableSupervisorClient struct{ err error }

func (c unavailableSupervisorClient) Probe(context.Context, string, string, []byte) (RuntimePoolProbeResult, error) {
	return RuntimePoolProbeResult{}, c.err
}

func (c unavailableSupervisorClient) RequestDrain(context.Context, string, string, []byte, harnessv2.StatusResponse, string) error {
	return c.err
}

// externalStartupMatchesBoundEvidence fails closed unless provider startup
// evidence names exactly the instance, endpoint, router target and worker or
// Pod that Core independently bound before credential delivery. Provider
// status can change after binding; it must never redirect controller
// credentials to another Actor that shares the bound instance identity.
func externalStartupMatchesBoundEvidence(pool *corev1alpha1.RuntimePool, evidence *workspacev1alpha1.StartupEvidence) error {
	if pool == nil || evidence == nil {
		return nil
	}
	binding, err := externalRuntimeEvidence(pool)
	if err != nil || binding == nil {
		return err
	}
	matches := binding.Identity == evidence.Identity && binding.Endpoint == evidence.Endpoint &&
		binding.RouterTarget == externalStartupRouterTarget(evidence)
	if evidence.Process != nil {
		matches = matches && binding.NativeProcess && binding.Pod == evidence.Process.Worker
	} else {
		matches = matches && !binding.NativeProcess && evidence.Pod != nil && binding.Pod == *evidence.Pod
	}
	if !matches {
		return fmt.Errorf("startup evidence differs from the bound runtime instance: %w", workspaceprovider.ErrStaleIdentity)
	}
	return nil
}

// externalStartupSupervisorClient selects the supervisor transport for exact
// provider startup evidence: routed evidence uses only the configured router;
// direct evidence keeps the pool's existing transport. Evidence for an
// already-bound instance must match its saved binding first.
func (r *RuntimePoolReconciler) externalStartupSupervisorClient(pool *corev1alpha1.RuntimePool, evidence *workspacev1alpha1.StartupEvidence) RuntimePoolSupervisorClient {
	if err := externalStartupMatchesBoundEvidence(pool, evidence); err != nil {
		return unavailableSupervisorClient{err: err}
	}
	routed, err := externalStartupHTTPClient(r.SubstrateRouter, evidence, runtimePoolProbeTimeout)
	if err != nil {
		return unavailableSupervisorClient{err: err}
	}
	if routed == nil {
		return r.supervisorClientForPool(pool)
	}
	if r.SupervisorClient != nil {
		return r.SupervisorClient
	}
	return &runtimePoolHTTPSupervisorClient{client: routed, now: r.now}
}
