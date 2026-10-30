package main

import (
	"strings"
	"testing"
)

func TestNormalizeSessionLoaderImage(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name, image, want string
	}{
		{name: "mirror", image: "registry.example.com/loader@" + digest, want: "registry.example.com/loader@" + digest},
		{name: "tag and digest", image: "registry.example.com/loader:v1@" + digest,
			want: "registry.example.com/loader@" + digest},
		{name: "empty"},
		{name: "tag", image: "registry.example.com/loader:v1"},
		{name: "implicit latest", image: "registry.example.com/loader"},
		{name: "malformed digest", image: "registry.example.com/loader@sha256:abc"},
		{name: "other algorithm", image: "registry.example.com/loader@sha512:" + strings.Repeat("a", 128)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeSessionLoaderImage(tc.image)
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "SHA256") {
					t.Fatalf("error = %v, want SHA256-pinned image requirement", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("normalized image = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
