// Package skill validates and projects Agent Skills delivered from untrusted
// images. Nothing inside a skill is executed or followed: folders are walked
// relative to file descriptors with O_NOFOLLOW, only real folders and regular
// files are accepted, every limit is enforced while reading, and SKILL.md is
// checked as untrusted prompt input before a skill is copied into a session.
package skill

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/orka-agents/orka/internal/acp/toolbox"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/security"
)

// Stable failure reasons. They appear verbatim in Task and RuntimePool status
// through the same FAIL line the toolbox checks use.
const (
	ReasonMissingMount        = "SKILL_MISSING_MOUNT"
	ReasonUnsupportedFileType = "SKILL_UNSUPPORTED_FILE_TYPE"
	ReasonInvalidFileName     = "SKILL_INVALID_FILE_NAME"
	ReasonTooDeep             = "SKILL_TOO_DEEP"
	ReasonTooLarge            = "SKILL_TOO_LARGE"
	ReasonFileTooLarge        = "SKILL_FILE_TOO_LARGE"
	ReasonTooManyEntries      = "SKILL_TOO_MANY_ENTRIES"
	ReasonMissingMarkdown     = "SKILL_MISSING_SKILL_MD"
	ReasonInvalidFrontmatter  = "SKILL_INVALID_FRONTMATTER"
	ReasonUnsafeContent       = "SKILL_UNSAFE_CONTENT"
	ReasonReadFailed          = "SKILL_READ_FAILED"
	ReasonWriteFailed         = "SKILL_WRITE_FAILED"
	ReasonUnsupported         = "SKILL_UNSUPPORTED_PLATFORM"
)

// StartupCheckTimeout bounds the supervisor's skill check, which runs before
// the supervisor serves. With image volumes that check reads the skill images
// themselves, so the runtime startup probe is extended by this much and a slow
// check reports its FAIL line before kubelet restarts the container.
const StartupCheckTimeout = time.Minute

// Limits bounds one skill folder.
type Limits struct {
	MaxEntries       int
	MaxTotalBytes    int64
	MaxFileBytes     int64
	MaxDepth         int
	MaxMarkdownBytes int64
}

// DefaultLimits returns the profile contract limits.
func DefaultLimits() Limits {
	return Limits{
		MaxEntries:       harnessv2.MaxRuntimeSkillEntries,
		MaxTotalBytes:    harnessv2.MaxRuntimeSkillTotalBytes,
		MaxFileBytes:     harnessv2.MaxRuntimeSkillFileBytes,
		MaxDepth:         harnessv2.MaxRuntimeSkillDepth,
		MaxMarkdownBytes: harnessv2.MaxRuntimeSkillMarkdownBytes,
	}
}

// File is one regular file of a loaded skill.
type File struct {
	// Path is slash-separated and relative to the skill root.
	Path       string
	Data       []byte
	Executable bool
}

// Tree is a fully read and validated skill folder.
type Tree struct {
	Name string
	// Dirs are slash-separated folders relative to the skill root, parents
	// before children.
	Dirs  []string
	Files []File
}

// withheldDiagnostic replaces a failure message that would echo
// credential-shaped content from the image.
const withheldDiagnostic = "skill diagnostic withheld because it appears to contain a secret or token"

// failf builds a failure whose message reaches stderr, the termination log,
// and Task and RuntimePool status. Messages can quote image-controlled file
// names, frontmatter keys, and YAML errors, so a message that looks like it
// carries a credential is replaced; the stable reason is always kept.
func failf(reason, format string, args ...any) *toolbox.Failure {
	message := fmt.Sprintf(format, args...)
	if security.LooksLikeSecret(message) {
		message = withheldDiagnostic
	}
	return &toolbox.Failure{Reason: reason, Message: message}
}

// allowedFrontmatterKeys are the Agent Skills specification fields that only
// describe the skill. Keys that change runtime behavior, such as
// allowed-tools (pre-approves tools), hooks (runs commands), model, context,
// or agent (switch models or spawn sub-agents), are rejected so a skill can
// never widen the session's frozen tool policy.
var allowedFrontmatterKeys = []string{"compatibility", "description", "license", "metadata", "name"}

const (
	maxDescriptionBytes   = 1024
	maxCompatibilityBytes = 500
	maxFileNameBytes      = 255
)

// Load reads the skill folder at root, validates its layout and SKILL.md, and
// returns its content. root must be a real folder; nothing inside it is
// followed or executed.
func Load(root, name string, limits Limits) (*Tree, error) {
	if err := harnessv2.ValidateRuntimeSkillName(name); err != nil {
		return nil, failf(ReasonInvalidFrontmatter, "%v", err)
	}
	tree, err := loadTree(root, name, limits)
	if err != nil {
		return nil, err
	}
	if err := validateTree(tree, limits); err != nil {
		return nil, err
	}
	return tree, nil
}

// reservedFiles are paths a runtime would interpret beyond the skill's
// instructions. Codex reads agents/openai.yaml for tool dependencies (which it
// may install) and invocation policy.
var reservedFiles = []string{"agents/openai.yaml", "agents/openai.yml"}

func validateTree(tree *Tree, limits Limits) error {
	for _, file := range tree.Files {
		lower := strings.ToLower(file.Path)
		if lower != strings.ToLower(harnessv2.RuntimeSkillMarkdownName) && path.Base(lower) == strings.ToLower(harnessv2.RuntimeSkillMarkdownName) {
			// Codex and OpenCode scan skill folders recursively, so a nested
			// SKILL.md would become an extra skill that was never validated.
			return failf(ReasonUnsafeContent, "skill %s contains a nested %s at %s; only the root %s is allowed", tree.Name, harnessv2.RuntimeSkillMarkdownName, file.Path, harnessv2.RuntimeSkillMarkdownName)
		}
		if slices.Contains(reservedFiles, lower) {
			return failf(ReasonUnsafeContent, "skill %s contains %s, which configures runtime tool dependencies and invocation policy; it is not allowed", tree.Name, file.Path)
		}
	}
	index := slices.IndexFunc(tree.Files, func(file File) bool { return file.Path == harnessv2.RuntimeSkillMarkdownName })
	if index < 0 {
		return failf(ReasonMissingMarkdown, "skill %s has no %s at its root", tree.Name, harnessv2.RuntimeSkillMarkdownName)
	}
	markdown := tree.Files[index].Data
	if int64(len(markdown)) > limits.MaxMarkdownBytes {
		return failf(ReasonFileTooLarge, "skill %s %s is larger than %d bytes", tree.Name, harnessv2.RuntimeSkillMarkdownName, limits.MaxMarkdownBytes)
	}
	return ValidateMarkdown(tree.Name, markdown)
}

// ValidateMarkdown checks SKILL.md as untrusted prompt input: valid UTF-8 text
// with a frontmatter block whose name matches the declared skill, only
// descriptive frontmatter keys, no shell preprocessing constructs, and no
// credential-shaped content.
func ValidateMarkdown(name string, markdown []byte) error {
	if !utf8.Valid(markdown) || bytes.IndexByte(markdown, 0) >= 0 {
		return failf(ReasonUnsafeContent, "skill %s %s must be UTF-8 text without NUL bytes", name, harnessv2.RuntimeSkillMarkdownName)
	}
	text := strings.ReplaceAll(string(markdown), "\r\n", "\n")
	// Claude Code runs !`command` (and ```! fenced blocks) through the shell
	// when a skill loads, before any tool policy applies. Reject every such
	// marker instead of trying to tell code examples apart from commands.
	if strings.Contains(text, "!`") || strings.Contains(text, "```!") {
		return failf(ReasonUnsafeContent, "skill %s %s contains a shell preprocessing marker (\"!`\"); skills may not run commands when they load", name, harnessv2.RuntimeSkillMarkdownName)
	}
	frontmatter, err := splitFrontmatter(text)
	if err != nil {
		return failf(ReasonInvalidFrontmatter, "skill %s %s: %v", name, harnessv2.RuntimeSkillMarkdownName, err)
	}
	if err := validateFrontmatter(name, frontmatter); err != nil {
		return failf(ReasonInvalidFrontmatter, "skill %s %s: %v", name, harnessv2.RuntimeSkillMarkdownName, err)
	}
	if security.LooksLikeSecret(text) {
		return failf(ReasonUnsafeContent, "skill %s %s appears to contain a secret or token", name, harnessv2.RuntimeSkillMarkdownName)
	}
	return nil
}

func splitFrontmatter(text string) (string, error) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", fmt.Errorf("must start with a --- frontmatter block")
	}
	if strings.HasPrefix(rest, "---\n") || rest == "---" {
		return "", nil
	}
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		if !strings.HasSuffix(rest, "\n---") {
			return "", fmt.Errorf("frontmatter block is not closed with ---")
		}
		end = len(rest) - len("\n---")
	}
	return rest[:end], nil
}

func validateFrontmatter(name, raw string) error {
	var fields map[string]any
	if err := yaml.UnmarshalStrict([]byte(raw), &fields); err != nil {
		return fmt.Errorf("frontmatter is not a valid YAML mapping: %v", err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if !slices.Contains(allowedFrontmatterKeys, key) {
			return fmt.Errorf("frontmatter key %q is not allowed; Orka accepts only %s", key, strings.Join(allowedFrontmatterKeys, ", "))
		}
	}
	declared, ok := fields["name"].(string)
	if !ok || declared != name {
		return fmt.Errorf("frontmatter name must equal the declared skill name %q", name)
	}
	description, ok := fields["description"].(string)
	if !ok || strings.TrimSpace(description) == "" {
		return fmt.Errorf("frontmatter description is required")
	}
	if len(description) > maxDescriptionBytes {
		return fmt.Errorf("frontmatter description exceeds %d bytes", maxDescriptionBytes)
	}
	if value, present := fields["license"]; present {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("frontmatter license must be a string")
		}
	}
	if value, present := fields["compatibility"]; present {
		text, ok := value.(string)
		if !ok || len(text) > maxCompatibilityBytes {
			return fmt.Errorf("frontmatter compatibility must be a string of at most %d bytes", maxCompatibilityBytes)
		}
	}
	if value, present := fields["metadata"]; present {
		metadata, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("frontmatter metadata must be a mapping of strings")
		}
		for key, entry := range metadata {
			if _, ok := entry.(string); !ok {
				return fmt.Errorf("frontmatter metadata %q must be a string", key)
			}
		}
	}
	return nil
}

// validFileName rejects names that cannot be represented safely in every
// runtime's skills folder.
func validFileName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > maxFileNameBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func joinRelative(parent, name string) string {
	if parent == "" {
		return name
	}
	return path.Join(parent, name)
}
