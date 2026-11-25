/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"path"
	"slices"
	"strings"
)

// Feature kinds in an action signature.
const (
	FeatureTool   = "tool"
	FeatureEffect = "effect"
	FeatureBinary = "bin"
	FeaturePath   = "path"
	FeatureDest   = "dest"
	// FeatureDestDomain is a destination host's last two DNS labels.
	FeatureDestDomain = "dest_domain"
	FeatureConcern    = "concern"
	FeatureTaint      = "taint"
)

// Feature is one normalized behavioural feature of an action.
type Feature struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// String returns "kind:value".
func (f Feature) String() string { return f.Kind + ":" + f.Value }

// Signature returns the action's normalized behavioural features, sorted and
// unique: the tool, each effect kind seen, binary basenames, path prefixes,
// destinations with IP addresses widened to their /24, triggered concerns and
// taint. Two actions that did the same kind of thing share a signature even
// when file names, addresses or Task names differ.
func (d *OversightDecision) Signature() []Feature {
	set := map[Feature]bool{}
	add := func(kind, value string) {
		if value = strings.TrimSpace(value); value != "" {
			set[Feature{Kind: kind, Value: value}] = true
		}
	}
	add(FeatureTool, d.Action.ToolName)
	for kind, count := range d.Action.Effects.Counts {
		if count > 0 {
			add(FeatureEffect, kind)
		}
	}
	for _, binary := range d.Action.Effects.Binaries {
		add(FeatureBinary, BinaryName(binary.Value))
	}
	for _, p := range d.Action.Effects.Paths {
		add(FeaturePath, PathPrefix(p.Value))
	}
	for _, dest := range d.Action.Effects.Destinations {
		add(FeatureDest, DestinationClass(dest.Value))
	}
	for _, concern := range d.TriggeredConcerns() {
		add(FeatureConcern, concern)
	}
	for _, taint := range append(slices.Clone(d.Action.TaintFlags), d.Action.InheritedTaint...) {
		add(FeatureTaint, taint)
	}
	features := make([]Feature, 0, len(set))
	for feature := range set {
		features = append(features, feature)
	}
	slices.SortFunc(features, func(a, b Feature) int { return strings.Compare(a.String(), b.String()) })
	return features
}

// SignatureHash returns a short stable hash of a signature.
func SignatureHash(features []Feature) string {
	h := sha256.New()
	for _, feature := range features {
		h.Write([]byte(feature.String()))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// BinaryName returns a binary path's base name.
func BinaryName(binary string) string {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return ""
	}
	return path.Base(binary)
}

// PathPrefix returns the directory of an absolute path, at most three levels
// deep, followed by "/*": "/var/run/secrets/x/token" becomes
// "/var/run/secrets/*". A relative path is returned cleaned.
func PathPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	dir := path.Dir(path.Clean(p))
	if dir == "/" {
		return path.Clean(p)
	}
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return "/" + strings.Join(parts, "/") + "/*"
}

// DestinationClass widens an IPv4 destination to its /24 ("10.0.3.*:443")
// and keeps a host name as given.
func DestinationClass(dest string) string {
	dest = strings.TrimSpace(dest)
	host, port, err := net.SplitHostPort(dest)
	if err != nil {
		host, port = dest, ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		octets := strings.Split(ip.To4().String(), ".")
		host = strings.Join(octets[:3], ".") + ".*"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// Jaccard returns |a ∩ b| / |a ∪ b| for two sorted, unique feature lists,
// and the shared features.
func Jaccard(a, b []Feature) (float64, []Feature) {
	if len(a) == 0 && len(b) == 0 {
		return 0, nil
	}
	inB := make(map[Feature]bool, len(b))
	for _, feature := range b {
		inB[feature] = true
	}
	var shared []Feature
	for _, feature := range a {
		if inB[feature] {
			shared = append(shared, feature)
		}
	}
	union := len(a) + len(b) - len(shared)
	return float64(len(shared)) / float64(union), shared
}

// DestinationDomain returns a destination host's last two DNS labels, such
// as "example.com" for "cdn.example.com:443", or "" for an IP address.
func DestinationDomain(dest string) string {
	dest = strings.TrimSpace(dest)
	host, _, err := net.SplitHostPort(dest)
	if err != nil {
		host = dest
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || net.ParseIP(host) != nil {
		return ""
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	return strings.Join(labels[len(labels)-2:], ".")
}
