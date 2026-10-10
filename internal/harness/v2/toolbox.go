package v2

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// Toolbox limits are part of the immutable runtime profile contract: every
// profile builder (controller planning, RuntimePool projection, the deployed
// template, the dispatcher, and the supervisor) enforces the same shape.
const (
	MaxRuntimeToolboxes          = 4
	MaxRuntimeToolboxPathEntries = 8
	MaxRuntimeToolboxPathBytes   = 256
	MaxRuntimeToolboxImageBytes  = 512

	// RuntimeToolboxHomebrewMountPath is the only non-/opt mount path, so
	// dalec-homebrew prefixes land where Homebrew expects them.
	RuntimeToolboxHomebrewMountPath = "/home/linuxbrew/.linuxbrew"
	runtimeToolboxOptPrefix         = "/opt/"
)

// RuntimeToolboxReservedOptNames are the /opt/<name> folders the built-in
// runtime images already populate. A toolbox may never shadow them.
var RuntimeToolboxReservedOptNames = []string{
	"codex", "codex-acp", "claude", "claude-agent-acp", "copilot", "opencode", "ripgrep", "yarn-v1.22.22",
}

var runtimeToolboxOptNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// RuntimeToolbox is one read-only tool image bound into a runtime Pod. The
// declared order is significant: it decides PATH precedence between toolboxes
// and must never be sorted.
type RuntimeToolbox struct {
	Image       string   `json:"image"`
	MountPath   string   `json:"mountPath"`
	PathEntries []string `json:"pathEntries,omitempty"`
}

// PathDirs returns the absolute PATH folders this toolbox contributes, in
// declared order.
func (t RuntimeToolbox) PathDirs() []string {
	dirs := make([]string, 0, len(t.PathEntries))
	for _, entry := range t.PathEntries {
		dirs = append(dirs, path.Join(t.MountPath, entry))
	}
	return dirs
}

// Validate checks one toolbox's shape. Registry allowlisting and feature gating
// are controller policy and live outside the profile contract.
func (t RuntimeToolbox) Validate() error {
	if err := ValidateRuntimeToolboxImage(t.Image); err != nil {
		return err
	}
	if err := ValidateRuntimeToolboxMountPath(t.MountPath); err != nil {
		return err
	}
	if len(t.PathEntries) > MaxRuntimeToolboxPathEntries {
		return fmt.Errorf("toolbox %s declares %d pathEntries; at most %d are allowed", t.MountPath, len(t.PathEntries), MaxRuntimeToolboxPathEntries)
	}
	seen := make(map[string]struct{}, len(t.PathEntries))
	for _, entry := range t.PathEntries {
		if err := validateRuntimeToolboxPathEntry(t.MountPath, entry); err != nil {
			return err
		}
		if _, duplicate := seen[entry]; duplicate {
			return fmt.Errorf("toolbox %s repeats pathEntry %q", t.MountPath, entry)
		}
		seen[entry] = struct{}{}
	}
	return nil
}

// ValidateRuntimeToolboxes validates each toolbox and rejects overlapping
// mount paths. A nil or empty list is valid and means no toolboxes.
func ValidateRuntimeToolboxes(toolboxes []RuntimeToolbox) error {
	if len(toolboxes) > MaxRuntimeToolboxes {
		return fmt.Errorf("%d toolboxes declared; at most %d are allowed", len(toolboxes), MaxRuntimeToolboxes)
	}
	for i := range toolboxes {
		if err := toolboxes[i].Validate(); err != nil {
			return fmt.Errorf("toolboxes[%d]: %w", i, err)
		}
		for j := range i {
			if runtimeToolboxMountPathsOverlap(toolboxes[i].MountPath, toolboxes[j].MountPath) {
				return fmt.Errorf("toolboxes[%d] mountPath %s overlaps toolboxes[%d] mountPath %s", i, toolboxes[i].MountPath, j, toolboxes[j].MountPath)
			}
		}
	}
	return nil
}

// ValidateRuntimeToolboxImage requires a fully qualified digest-pinned image
// in the canonical form Kubernetes' container-image parser accepts: an
// explicit registry host, a lowercase repository, no tag, and a sha256 digest.
func ValidateRuntimeToolboxImage(image string) error {
	if image == "" {
		return fmt.Errorf("toolbox image is required")
	}
	if len(image) > MaxRuntimeToolboxImageBytes {
		return fmt.Errorf("toolbox image exceeds %d bytes", MaxRuntimeToolboxImageBytes)
	}
	shape := fmt.Errorf("toolbox image %q must be a fully qualified registry reference pinned by digest (<registry>/<repository>@sha256:<64 hex>) without a tag", image)
	// ParseNamed accepts only the canonical form, so docker.io shorthand such
	// as "yq@sha256:..." or "library/yq@sha256:..." is rejected.
	named, err := reference.ParseNamed(image)
	if err != nil {
		return shape
	}
	if _, tagged := named.(reference.NamedTagged); tagged {
		return shape
	}
	canonical, ok := named.(reference.Canonical)
	if !ok || canonical.Digest().Algorithm() != digest.SHA256 || canonical.String() != image {
		return shape
	}
	domain := reference.Domain(named)
	if domain != "localhost" && !strings.ContainsAny(domain, ".:") {
		return shape
	}
	return nil
}

// ValidateRuntimeToolboxMountPath accepts exactly /home/linuxbrew/.linuxbrew
// or /opt/<name> where <name> is a single clean component that no built-in
// runtime image already uses.
func ValidateRuntimeToolboxMountPath(mountPath string) error {
	if err := validateRuntimeToolboxPathText("mountPath", mountPath); err != nil {
		return err
	}
	if mountPath == RuntimeToolboxHomebrewMountPath {
		return nil
	}
	if !strings.HasPrefix(mountPath, runtimeToolboxOptPrefix) {
		return fmt.Errorf("toolbox mountPath %q must be %s or /opt/<name>", mountPath, RuntimeToolboxHomebrewMountPath)
	}
	name := strings.TrimPrefix(mountPath, runtimeToolboxOptPrefix)
	if name == "" || strings.Contains(name, "/") || !runtimeToolboxOptNamePattern.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("toolbox mountPath %q must be /opt/<name> with a single clean folder name", mountPath)
	}
	if path.Clean(mountPath) != mountPath {
		return fmt.Errorf("toolbox mountPath %q must be a clean absolute path", mountPath)
	}
	if slices.Contains(RuntimeToolboxReservedOptNames, name) {
		return fmt.Errorf("toolbox mountPath %q is reserved by the built-in runtime images", mountPath)
	}
	return nil
}

func validateRuntimeToolboxPathEntry(mountPath, entry string) error {
	if err := validateRuntimeToolboxPathText("pathEntry", entry); err != nil {
		return err
	}
	if strings.HasPrefix(entry, "/") {
		return fmt.Errorf("toolbox pathEntry %q must be relative to %s", entry, mountPath)
	}
	if path.Clean(entry) != entry || entry == "." {
		return fmt.Errorf("toolbox pathEntry %q must be a clean relative path", entry)
	}
	if slices.Contains(strings.Split(entry, "/"), "..") {
		return fmt.Errorf("toolbox pathEntry %q must stay inside %s", entry, mountPath)
	}
	return nil
}

func validateRuntimeToolboxPathText(field, value string) error {
	if value == "" {
		return fmt.Errorf("toolbox %s is required", field)
	}
	if len(value) > MaxRuntimeToolboxPathBytes {
		return fmt.Errorf("toolbox %s exceeds %d bytes", field, MaxRuntimeToolboxPathBytes)
	}
	if strings.Contains(value, "//") || (len(value) > 1 && strings.HasSuffix(value, "/")) {
		return fmt.Errorf("toolbox %s %q must be a clean path without empty components or a trailing slash", field, value)
	}
	// ':' would split PATH and ',' would split the copier's --path-entries
	// list, so neither may appear anywhere in a toolbox path.
	for _, r := range value {
		if r == ':' || r == ',' || r == 0 || r < 0x20 || r == 0x7f {
			return fmt.Errorf("toolbox %s %q contains a character that is not allowed in PATH", field, value)
		}
	}
	return nil
}

func runtimeToolboxMountPathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// RuntimeToolboxChildPath appends every toolbox PATH folder, in declared order,
// after the base PATH so toolbox tools never shadow system commands.
func RuntimeToolboxChildPath(base string, toolboxes []RuntimeToolbox) string {
	parts := []string{base}
	for i := range toolboxes {
		parts = append(parts, toolboxes[i].PathDirs()...)
	}
	return strings.Join(parts, ":")
}

// CloneRuntimeToolboxes deep-copies a toolbox list, preserving nil.
func CloneRuntimeToolboxes(toolboxes []RuntimeToolbox) []RuntimeToolbox {
	if toolboxes == nil {
		return nil
	}
	cloned := make([]RuntimeToolbox, len(toolboxes))
	for i := range toolboxes {
		cloned[i] = toolboxes[i]
		if toolboxes[i].PathEntries != nil {
			cloned[i].PathEntries = append([]string(nil), toolboxes[i].PathEntries...)
		}
	}
	return cloned
}
