/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	distributionref "github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

const (
	// kagentRuntimeRepository is where Orka releases its kagent runtime image,
	// tagged with the release version without the v.
	kagentRuntimeRepository = "ghcr.io/orka-agents/orka/acp-kagent-runtime"
	kagentImageTimeout      = 30 * time.Second
	// kagentImageMaxBytes bounds a manifest or image config read.
	kagentImageMaxBytes = 4 << 20
)

var kagentReleaseVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(beta|rc)\.(0|[1-9][0-9]*))?$`)

// kagentDefaultImage is the runtime image a release of this CLI deploys: the
// one the same release published. A development build names none.
func kagentDefaultImage(cliVersion string) (string, error) {
	if !kagentReleaseVersion.MatchString(cliVersion) {
		return "", fmt.Errorf("this orka build (%s) is not a release, so it has no matching kagent runtime image; pass --image", cliVersion)
	}
	return kagentRuntimeRepository + ":" + strings.TrimPrefix(cliVersion, "v"), nil
}

// resolveKagentRuntimeImage pins image to its digest, and reads the digest of
// the adapter image it was built on from its kagentAdapterDigestLabel label.
// A multi-platform image is pinned by its index; the label is read from its
// linux/amd64 image, or else its linux/arm64 one: every platform's build
// shares it.
func resolveKagentRuntimeImage(ctx context.Context, image string, client *http.Client) (pinned, adapter string, err error) {
	ctx, cancel := context.WithTimeout(ctx, kagentImageTimeout)
	defer cancel()
	ref, err := distributionref.ParseNamed(strings.TrimSpace(image))
	if err != nil {
		return "", "", fmt.Errorf("--image %q is not a full registry/repository reference", image)
	}
	reference := ""
	switch r := ref.(type) {
	case distributionref.Digested:
		if r.Digest().Algorithm() != "sha256" {
			return "", "", errors.New("--image digests must use SHA256")
		}
		reference = r.Digest().String()
	case distributionref.Tagged:
		reference = r.Tag()
	default:
		return "", "", fmt.Errorf("--image %q needs a tag or a SHA256 digest", image)
	}
	name := distributionref.TrimNamed(ref).Name()
	repository, err := remote.NewRepository(name)
	if err != nil {
		return "", "", fmt.Errorf("--image %q names no registry repository", image)
	}
	repository.Client = kagentRegistryClient(client)
	descriptor, err := repository.Resolve(ctx, reference)
	if err != nil {
		return "", "", fmt.Errorf("resolving %s: %w", image, err)
	}
	manifest := descriptor
	if descriptor.MediaType == ocispec.MediaTypeImageIndex || descriptor.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		var index ocispec.Index
		if err := fetchKagentJSON(ctx, repository, descriptor, &index); err != nil {
			return "", "", fmt.Errorf("reading %s: %w", image, err)
		}
		found := false
		for _, architecture := range []string{"amd64", "arm64"} {
			for _, candidate := range index.Manifests {
				if candidate.Platform != nil && candidate.Platform.OS == "linux" && candidate.Platform.Architecture == architecture {
					manifest, found = candidate, true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return "", "", fmt.Errorf("%s has no linux/amd64 or linux/arm64 image", image)
		}
	}
	var imageManifest ocispec.Manifest
	if err := fetchKagentJSON(ctx, repository, manifest, &imageManifest); err != nil {
		return "", "", fmt.Errorf("reading %s: %w", image, err)
	}
	var config ocispec.Image
	if err := fetchKagentJSON(ctx, repository, imageManifest.Config, &config); err != nil {
		return "", "", fmt.Errorf("reading the configuration of %s: %w", image, err)
	}
	adapter = config.Config.Labels[kagentAdapterDigestLabel]
	if !kagentDigest.MatchString(adapter) {
		return "", "", fmt.Errorf("%s is not an Orka kagent runtime image: it has no %s label", image, kagentAdapterDigestLabel)
	}
	return name + "@" + descriptor.Digest.String(), adapter, nil
}

func fetchKagentJSON(ctx context.Context, repository *remote.Repository, descriptor ocispec.Descriptor, out any) error {
	if descriptor.Size > kagentImageMaxBytes {
		return errors.New("the registry object is too large")
	}
	raw, err := content.FetchAll(ctx, repository, descriptor)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// kagentRegistryClient reads public images anonymously, and private ones with
// the credentials docker login stored.
func kagentRegistryClient(client *http.Client) *auth.Client {
	if client == nil {
		client = &http.Client{Timeout: kagentImageTimeout}
	}
	registry := &auth.Client{Client: client, Cache: auth.NewCache()}
	if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		registry.Credential = credentials.Credential(store)
	}
	return registry
}
