/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package k8scaps

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/discovery"
)

const (
	MinSupportedServerVersion      = "1.33"
	FeatureBaselineServerVersion   = "1.35"
	ImageVolumeServerVersion       = "1.36"
	PodLevelResourcesServerVersion = "1.34"
)

type Capabilities struct {
	ServerVersion            string
	SupplementalGroupsStrict bool
	PodLevelResources        bool
	PodLogsSplitStreams      bool
	InPlacePodResize         bool
	ImageVolumes             bool
	PodUserNamespaces        bool
}

type DiscoveryReader interface {
	ServerVersion() (*version.Version, string, error)
	HasPodResizeSubresource() (bool, error)
}

type clientDiscoveryReader struct {
	client discovery.DiscoveryInterface
}

func (r clientDiscoveryReader) ServerVersion() (*version.Version, string, error) {
	info, err := r.client.ServerVersion()
	if err != nil {
		return nil, "", fmt.Errorf("read API server version: %w", err)
	}
	parsed, err := version.ParseGeneric(info.GitVersion)
	if err != nil {
		return nil, info.GitVersion, fmt.Errorf("parse API server version %q: %w", info.GitVersion, err)
	}
	return parsed, info.GitVersion, nil
}

func (r clientDiscoveryReader) HasPodResizeSubresource() (bool, error) {
	list, err := r.client.ServerResourcesForGroupVersion("v1")
	if err != nil {
		return false, err
	}
	if list == nil {
		return false, nil
	}
	for i := range list.APIResources {
		if list.APIResources[i].Name == "pods/resize" {
			return true, nil
		}
	}
	return false, nil
}

func Detect(client discovery.DiscoveryInterface) (Capabilities, error) {
	if client == nil {
		return Capabilities{}, fmt.Errorf("discovery client is nil")
	}
	return DetectWithReader(clientDiscoveryReader{client: client})
}

func DetectWithReader(reader DiscoveryReader) (Capabilities, error) {
	if reader == nil {
		return Capabilities{}, fmt.Errorf("discovery reader is nil")
	}
	parsed, raw, err := reader.ServerVersion()
	if err != nil {
		return Capabilities{ServerVersion: strings.TrimSpace(raw)}, err
	}
	caps := Capabilities{
		ServerVersion: strings.TrimSpace(raw),
	}
	podLevelMin := version.MustParseGeneric(PodLevelResourcesServerVersion)
	featureBaseline := version.MustParseGeneric(FeatureBaselineServerVersion)
	imageVolumeMin := version.MustParseGeneric(ImageVolumeServerVersion)
	if !parsed.LessThan(podLevelMin) {
		caps.PodLevelResources = true
	}
	if !parsed.LessThan(featureBaseline) {
		caps.PodLogsSplitStreams = true
	}
	if !parsed.LessThan(imageVolumeMin) {
		caps.ImageVolumes = true
	}
	if hasResize, resizeErr := reader.HasPodResizeSubresource(); resizeErr == nil {
		caps.InPlacePodResize = hasResize
	}
	return caps, nil
}

func (c Capabilities) WithSupplementalGroupsStrict(enabled bool) Capabilities {
	c.SupplementalGroupsStrict = enabled
	return c
}

func (c Capabilities) WithPodUserNamespaces(enabled bool) Capabilities {
	c.PodUserNamespaces = enabled
	return c
}

func (c Capabilities) MeetsVersion(minVersion string) bool {
	if strings.TrimSpace(c.ServerVersion) == "" {
		return false
	}
	parsed, err := version.ParseGeneric(c.ServerVersion)
	if err != nil {
		return false
	}
	return !parsed.LessThan(version.MustParseGeneric(minVersion))
}

func IsDisruptedPod(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for i := range pod.Status.Conditions {
		cond := &pod.Status.Conditions[i]
		if cond.Type == corev1.DisruptionTarget && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func SelectJobPod(pods []corev1.Pod) (corev1.Pod, bool) {
	if len(pods) == 0 {
		return corev1.Pod{}, false
	}
	ordered := make([]corev1.Pod, len(pods))
	copy(ordered, pods)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
			return left.CreationTimestamp.Before(&right.CreationTimestamp)
		}
		return left.Name < right.Name
	})
	bestIdx := -1
	bestScore := -1
	for i := range ordered {
		score := jobPodAttemptScore(&ordered[i])
		if score >= bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return ordered[len(ordered)-1], true
	}
	return ordered[bestIdx], true
}

func jobPodAttemptScore(pod *corev1.Pod) int {
	if IsDisruptedPod(pod) {
		return 0
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return 4
	case corev1.PodRunning:
		return 3
	case corev1.PodFailed:
		return 2
	default:
		return 1
	}
}
