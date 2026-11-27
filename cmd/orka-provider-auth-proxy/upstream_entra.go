/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/orka-agents/orka/internal/entraclient"
	"github.com/orka-agents/orka/internal/provideridentity"
)

// errUpstreamToken reports that no Entra token could be obtained for the
// upstream. The proxy answers 503 and forwards nothing.
var errUpstreamToken = errors.New("provider upstream token is unavailable")

// errUpstreamOrigin reports a request whose destination is not the pinned
// upstream origin. It cannot happen through ServeHTTP, which joins every path
// onto the upstream base; the check keeps the token pinned if that changes.
var errUpstreamOrigin = errors.New("refusing to send the upstream token outside the upstream origin")

// upstreamEntraConfig selects keyless upstream authentication: the proxy
// exchanges its own projected ServiceAccount token for an Entra token of the
// named workload identity, and sends that token only to the upstream origin.
type upstreamEntraConfig struct {
	// Identities is provideridentity JSON, the same format the controller's
	// --provider-identities takes.
	Identities string
	// Identity names the identity whose tokens authenticate the upstream.
	Identity string
	// TokenFile is the proxy's projected ServiceAccount token, the federated
	// credential for that identity.
	TokenFile string
}

func (c upstreamEntraConfig) enabled() bool {
	return strings.TrimSpace(c.Identities) != "" || strings.TrimSpace(c.Identity) != ""
}

// upstreamEntra attaches a workload-identity token to requests for one
// pinned upstream origin.
type upstreamEntra struct {
	source   *provideridentity.Source
	identity string
	baseURL  string
	origin   string
}

func newUpstreamEntra(cfg upstreamEntraConfig, upstream *url.URL) (*upstreamEntra, error) {
	identity := strings.TrimSpace(cfg.Identity)
	tokenFile := strings.TrimSpace(cfg.TokenFile)
	if strings.TrimSpace(cfg.Identities) == "" || identity == "" || tokenFile == "" {
		return nil, errors.New("upstream Entra authentication needs the identities, the identity name and the token file")
	}
	config, err := provideridentity.Parse(cfg.Identities)
	if err != nil {
		return nil, err
	}
	source, err := provideridentity.NewSource(config, entraclient.FileCredential(tokenFile))
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("upstream Entra authentication needs at least one identity")
	}
	baseURL := upstream.String()
	// Check requires an https upstream whose origin the identity allows.
	if err := source.Check(identity, baseURL); err != nil {
		return nil, fmt.Errorf("upstream Entra identity %q cannot be used for the upstream: %w", identity, err)
	}
	origin, err := provideridentity.Origin(baseURL)
	if err != nil {
		return nil, err
	}
	return &upstreamEntra{source: source, identity: identity, baseURL: baseURL, origin: origin}, nil
}

// authenticate sets the upstream bearer on request, which must be for the
// pinned origin. It never logs or returns the token.
func (u *upstreamEntra) authenticate(ctx context.Context, request *http.Request) error {
	origin, err := provideridentity.Origin(request.URL.String())
	if err != nil || origin != u.origin {
		return errUpstreamOrigin
	}
	token, err := u.source.Token(ctx, u.identity, u.baseURL)
	if err != nil || token == "" {
		return errUpstreamToken
	}
	request.Header.Set(authorizationHeader, "Bearer "+token)
	return nil
}
