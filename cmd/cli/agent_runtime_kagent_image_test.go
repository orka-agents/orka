/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// kagentTestRegistry serves one image index, tagged 0.4.0, holding an
// attestation and a linux image of architecture, which carries labels.
func kagentTestRegistry(t *testing.T, architecture string, labels map[string]string) (*httptest.Server, string) {
	t.Helper()
	blobs := map[string][]byte{}
	put := func(value any) ocispec.Descriptor {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		id := "sha256:" + hex.EncodeToString(sum[:])
		blobs[id] = raw
		return ocispec.Descriptor{Digest: digest.Digest(id), Size: int64(len(raw))}
	}
	config := put(ocispec.Image{Config: ocispec.ImageConfig{Labels: labels}})
	config.MediaType = ocispec.MediaTypeImageConfig
	manifest := put(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: config, Layers: []ocispec.Descriptor{}})
	manifest.MediaType = ocispec.MediaTypeImageManifest
	manifest.Platform = &ocispec.Platform{OS: "linux", Architecture: architecture}
	// Buildx lists provenance attestations as unknown/unknown images.
	attestation := manifest
	attestation.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	index := put(ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{attestation, manifest}})
	mediaTypes := map[string]string{index.Digest.String(): ocispec.MediaTypeImageIndex, manifest.Digest.String(): ocispec.MediaTypeImageManifest}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/orka/acp-kagent-runtime/")
		var id string
		switch {
		case r.URL.Path == "/v2/":
			return
		case path == "manifests/0.4.0":
			id = index.Digest.String()
		case strings.HasPrefix(path, "manifests/"):
			id = strings.TrimPrefix(path, "manifests/")
		case strings.HasPrefix(path, "blobs/"):
			id = strings.TrimPrefix(path, "blobs/")
		}
		raw, ok := blobs[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mediaTypes[id])
		w.Header().Set("Docker-Content-Digest", id)
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}
	}))
	t.Cleanup(server.Close)
	return server, index.Digest.String()
}

func TestResolveKagentRuntimeImagePinsTheIndexAndReadsTheAdapter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	server, index := kagentTestRegistry(t, "amd64", map[string]string{kagentAdapterDigestLabel: testAdapterDigest})
	repository := strings.TrimPrefix(server.URL, "https://") + "/orka/acp-kagent-runtime"
	pinned, adapter, err := resolveKagentRuntimeImage(context.Background(), repository+":0.4.0", server.Client())
	if err != nil || pinned != repository+"@"+index || adapter != testAdapterDigest {
		t.Fatalf("resolve = %q, %q, %v", pinned, adapter, err)
	}
	// A digest pins the image as given.
	if again, _, err := resolveKagentRuntimeImage(context.Background(), pinned, server.Client()); err != nil || again != pinned {
		t.Fatalf("resolve by digest = %q, %v", again, err)
	}
	if _, _, err := resolveKagentRuntimeImage(context.Background(), repository, server.Client()); err == nil {
		t.Fatal("an image without a tag or digest resolved")
	}
}

// An image built for arm64 alone carries the same label.
func TestResolveKagentRuntimeImageReadsAnArm64OnlyImage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	server, index := kagentTestRegistry(t, "arm64", map[string]string{kagentAdapterDigestLabel: testAdapterDigest})
	repository := strings.TrimPrefix(server.URL, "https://") + "/orka/acp-kagent-runtime"
	if pinned, adapter, err := resolveKagentRuntimeImage(context.Background(), repository+":0.4.0", server.Client()); err != nil ||
		pinned != repository+"@"+index || adapter != testAdapterDigest {
		t.Fatalf("resolve = %q, %q, %v", pinned, adapter, err)
	}
}

func TestResolveKagentRuntimeImageRefusesAnImageWithoutTheAdapterLabel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	server, _ := kagentTestRegistry(t, "amd64", map[string]string{"org.opencontainers.image.title": "something else"})
	image := strings.TrimPrefix(server.URL, "https://") + "/orka/acp-kagent-runtime:0.4.0"
	if _, _, err := resolveKagentRuntimeImage(context.Background(), image, server.Client()); err == nil || !strings.Contains(err.Error(), "is not an Orka kagent runtime image") {
		t.Fatalf("resolve = %v", err)
	}
}
