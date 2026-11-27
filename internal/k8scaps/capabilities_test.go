/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package k8scaps

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/util/version"
)

type stubDiscoveryReader struct {
	raw       string
	verErr    error
	hasResize bool
	resizeErr error
}

func (s stubDiscoveryReader) ServerVersion() (*version.Version, string, error) {
	if s.verErr != nil {
		return nil, s.raw, s.verErr
	}
	parsed, err := version.ParseGeneric(s.raw)
	return parsed, s.raw, err
}

func (s stubDiscoveryReader) HasPodResizeSubresource() (bool, error) {
	return s.hasResize, s.resizeErr
}

func TestDetectWithReaderVersions(t *testing.T) {
	tests := []struct {
		name            string
		reader          stubDiscoveryReader
		wantSuppStrict  bool
		wantPodLevel    bool
		wantSplitLogs   bool
		wantResize      bool
		wantImageVolume bool
	}{
		{
			name:            "k8s 1.33 backwards compat omits 1.35+ features",
			reader:          stubDiscoveryReader{raw: "v1.33.4", hasResize: false},
			wantSuppStrict:  false,
			wantPodLevel:    false,
			wantSplitLogs:   false,
			wantResize:      false,
			wantImageVolume: false,
		},
		{
			name:            "k8s 1.33 with pods/resize discovered",
			reader:          stubDiscoveryReader{raw: "v1.33.4", hasResize: true},
			wantSuppStrict:  false,
			wantPodLevel:    false,
			wantSplitLogs:   false,
			wantResize:      true,
			wantImageVolume: false,
		},
		{
			name:            "k8s 1.34 omits 1.35+ features",
			reader:          stubDiscoveryReader{raw: "v1.34.1", hasResize: true},
			wantSuppStrict:  false,
			wantPodLevel:    false,
			wantSplitLogs:   false,
			wantResize:      true,
			wantImageVolume: false,
		},
		{
			name:            "k8s 1.35 enables 1.35 feature baseline",
			reader:          stubDiscoveryReader{raw: "v1.35.0", hasResize: true},
			wantSuppStrict:  false,
			wantPodLevel:    true,
			wantSplitLogs:   true,
			wantResize:      true,
			wantImageVolume: false,
		},
		{
			name:            "k8s 1.36 enables image volumes too",
			reader:          stubDiscoveryReader{raw: "v1.36.2", hasResize: true},
			wantSuppStrict:  false,
			wantPodLevel:    true,
			wantSplitLogs:   true,
			wantResize:      true,
			wantImageVolume: true,
		},
		{
			name:            "k8s 1.37 tolerates resize discovery error",
			reader:          stubDiscoveryReader{raw: "v1.37.2", resizeErr: errors.New("forbidden")},
			wantSuppStrict:  false,
			wantPodLevel:    true,
			wantSplitLogs:   true,
			wantResize:      false,
			wantImageVolume: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectWithReader(tc.reader)
			if err != nil {
				t.Fatalf("DetectWithReader() error = %v", err)
			}
			if got.SupplementalGroupsStrict != tc.wantSuppStrict ||
				got.PodLevelResources != tc.wantPodLevel ||
				got.PodLogsSplitStreams != tc.wantSplitLogs ||
				got.InPlacePodResize != tc.wantResize ||
				got.ImageVolumes != tc.wantImageVolume ||
				got.PodUserNamespaces {
				t.Fatalf("DetectWithReader() = %+v, want strict=%v podLevel=%v splitLogs=%v resize=%v imageVol=%v userns=false",
					got, tc.wantSuppStrict, tc.wantPodLevel, tc.wantSplitLogs, tc.wantResize, tc.wantImageVolume)
			}
		})
	}
}

func TestDetectWithReaderErrors(t *testing.T) {
	if _, err := DetectWithReader(nil); err == nil {
		t.Fatal("expected error for nil reader")
	}
	if _, err := DetectWithReader(stubDiscoveryReader{verErr: errors.New("unreachable")}); err == nil {
		t.Fatal("expected error when ServerVersion fails")
	}
}

func TestCapabilitiesWithPodUserNamespacesAndMeetsVersion(t *testing.T) {
	caps := (Capabilities{ServerVersion: "v1.35.1"}).WithPodUserNamespaces(true).WithSupplementalGroupsStrict(true)
	if !caps.PodUserNamespaces || !caps.SupplementalGroupsStrict {
		t.Fatalf("expected PodUserNamespaces and SupplementalGroupsStrict true, got %+v", caps)
	}
	if !caps.MeetsVersion("1.33") || !caps.MeetsVersion("1.35") || caps.MeetsVersion("1.36") {
		t.Fatalf("unexpected MeetsVersion results for %q", caps.ServerVersion)
	}
	if (Capabilities{}).MeetsVersion("1.33") {
		t.Fatal("empty ServerVersion must not satisfy MeetsVersion")
	}
}
