package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	distributionref "github.com/distribution/reference"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/orka-agents/orka/internal/api"
	"github.com/orka-agents/orka/internal/controller"
)

const (
	acpRuntimeImageResolveTimeout = 30 * time.Second
	acpRuntimeImageRequestTimeout = 15 * time.Second
)

// Advertise coding runtimes only when their image and model connection are configured.
func configuredACPRuntimeAvailability(
	images controller.ACPRuntimeImages,
	proxy controller.RuntimePoolProviderProxyConfig,
) api.ACPRuntimeAvailability {
	if proxy.Validate() != nil {
		return api.ACPRuntimeAvailability{}
	}
	return api.ACPRuntimeAvailability{
		Codex:    controller.ACPRuntimeImageAvailable(images.Codex),
		Claude:   controller.ACPRuntimeImageAvailable(images.Claude),
		Copilot:  controller.ACPRuntimeImageAvailable(images.Copilot),
		OpenCode: controller.ACPRuntimeImageAvailable(images.Opencode),
	}
}

// resolveACPRuntimeImagesOrDisable pins configured tags once per controller
// process. When any resolution fails, every coding-agent runtime is disabled
// for this process and the error is returned for logging: agent Tasks then
// fail closed with an unavailable runtime, while AI and container Tasks, which
// never needed these images, keep running. A partially resolved set is never
// used, so a tag can never silently fall back to a mutable reference.
func resolveACPRuntimeImagesOrDisable(
	ctx context.Context,
	configured controller.ACPRuntimeImages,
	httpClient *http.Client,
) (controller.ACPRuntimeImages, error) {
	resolved, err := resolveACPRuntimeImages(ctx, configured, httpClient)
	if err != nil {
		return controller.ACPRuntimeImages{}, err
	}
	return resolved, nil
}

// resolveACPRuntimeImages pins configured tags once per controller process.
// Consumers still receive only digest references or disabled providers, and a
// failed resolution returns no partial configuration.
func resolveACPRuntimeImages(
	ctx context.Context,
	configured controller.ACPRuntimeImages,
	httpClient *http.Client,
) (controller.ACPRuntimeImages, error) {
	ctx, cancel := context.WithTimeout(ctx, acpRuntimeImageResolveTimeout)
	defer cancel()
	client := runtimeImageRegistryClient(httpClient)
	resolved := controller.ACPRuntimeImages{}
	for _, provider := range []struct {
		name  string
		image string
		out   *string
	}{
		{"codex", configured.Codex, &resolved.Codex},
		{"claude", configured.Claude, &resolved.Claude},
		{"copilot", configured.Copilot, &resolved.Copilot},
		{"opencode", configured.Opencode, &resolved.Opencode},
	} {
		image, err := resolveACPRuntimeImage(ctx, provider.image, client)
		if err != nil {
			return controller.ACPRuntimeImages{}, fmt.Errorf("%s ACP runtime image: %w", provider.name, err)
		}
		*provider.out = image
	}
	return resolved, nil
}

func runtimeImageRegistryClient(client *http.Client) *auth.Client {
	if client == nil {
		client = &http.Client{}
	}
	bounded := *client
	if bounded.Timeout <= 0 || bounded.Timeout > acpRuntimeImageRequestTimeout {
		bounded.Timeout = acpRuntimeImageRequestTimeout
	}
	bounded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("registry redirects must use HTTPS")
		}
		if len(via) >= 10 {
			return errors.New("too many registry redirects")
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(req, via)
		}
		return nil
	}
	// No local credentials are loaded. Public registries may still issue
	// anonymous bearer tokens, which ORAS handles without exposing them.
	return &auth.Client{Client: &bounded, Cache: auth.NewCache()}
}

func resolveACPRuntimeImage(ctx context.Context, image string, client *auth.Client) (string, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", nil
	}
	ref, err := distributionref.ParseNamed(image)
	if err != nil {
		return "", errors.New("expected a full registry/repository reference with an explicit tag or SHA256 digest")
	}
	if digested, ok := ref.(distributionref.Digested); ok {
		if digested.Digest().Algorithm() != "sha256" {
			return "", errors.New("digest references must use SHA256")
		}
		// A tag alongside the digest is informational; pools and admission
		// accept only the canonical repository@digest form. Explicit digests
		// are trusted as given and never contact a registry.
		return distributionref.TrimNamed(ref).Name() + "@" + digested.Digest().String(), nil
	}
	tagged, ok := ref.(distributionref.Tagged)
	if !ok {
		return "", errors.New("an explicit tag or SHA256 digest is required")
	}
	repository := distributionref.TrimNamed(ref).Name()
	remoteRepository, err := remote.NewRepository(repository)
	if err != nil {
		return "", errors.New("invalid registry repository")
	}
	remoteRepository.Client = client
	// Resolve returns the manifest or multi-platform index digest as published;
	// it does not select a platform based on the controller's architecture.
	descriptor, err := remoteRepository.Resolve(ctx, tagged.Tag())
	if err != nil {
		return "", runtimeImageRegistryError(err)
	}
	resolved := repository + "@" + descriptor.Digest.String()
	if !controller.ACPRuntimeImageAvailable(resolved) {
		return "", errors.New("registry did not return a usable SHA256 image digest")
	}
	return resolved, nil
}

func runtimeImageRegistryError(err error) error {
	// Registry responses and token endpoint URLs can contain sensitive values.
	// Return only known errors or HTTP status codes to the startup logger.
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, errdef.ErrNotFound} {
		if errors.Is(err, known) {
			return fmt.Errorf("resolve tag: %w", known)
		}
	}
	if response, ok := errors.AsType[*errcode.ErrorResponse](err); ok {
		return fmt.Errorf("registry request failed with HTTP %d", response.StatusCode)
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return errors.New("registry TLS certificate verification failed")
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return errors.New("registry DNS lookup failed")
	}
	if operation, ok := errors.AsType[*net.OpError](err); ok {
		return fmt.Errorf("registry %s operation failed", operation.Op)
	}
	return errors.New("could not resolve tag; check registry access or configure a SHA256 digest")
}
