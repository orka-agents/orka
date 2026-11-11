package intake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const SchemaVersion = 1

type Section struct {
	Kind          string `json:"kind"`
	Text          string `json:"text"`
	SourcePointer string `json:"sourcePointer"`
}

type Report struct {
	Version      int       `json:"version"`
	SourceKind   string    `json:"sourceKind"`
	SourceDigest string    `json:"sourceDigest"`
	Restricted   bool      `json:"restricted"`
	Title        string    `json:"title"`
	Sections     []Section `json:"sections"`
	Repositories []string  `json:"repositories,omitempty"`
	Versions     []string  `json:"versions,omitempty"`
	Warnings     []string  `json:"warnings,omitempty"`
}

const (
	maxDerivedBytes   = 256 << 10
	maxRawFieldBytes  = 256 << 10
	maxSectionBytes   = 64 << 10
	maxTotalTextBytes = maxDerivedBytes
	maxTitleBytes     = 4 << 10
	maxSections       = 128
	maxRepositories   = 32
	maxVersions       = 64
	maxMetadataBytes  = 256
	titleField        = "title"
)

var versionPattern = regexp.MustCompile(`^[vV]?[0-9]+\.[0-9]+(?:\.[0-9]+){0,2}(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?(?:\+[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)

type normalizer struct {
	report        Report
	textBytes     int
	technicalBody bool
}

// Parse selects technical content from a supplied report. Input is untrusted;
// normalization neither authorizes model disclosure nor confirms the report.
func Parse(data []byte) (Report, error) {
	value, err := decodeJSON(data, &jsonBudget{})
	if err != nil {
		return Report{}, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Report{}, errors.New("report must be a JSON object")
	}
	digest := sha256.Sum256(data)
	n := normalizer{report: Report{
		Version: SchemaVersion, SourceKind: "supplied-report",
		SourceDigest: "sha256:" + hex.EncodeToString(digest[:]),
		Restricted:   true, Sections: make([]Section, 0),
	}}
	n.warn("report_content_is_untrusted_data")
	n.warn("normalization_does_not_authorize_disclosure")
	if value, exists := object["restricted"]; exists {
		restricted, ok := value.(bool)
		if !ok {
			return Report{}, errors.New("restricted must be a boolean")
		}
		n.report.Restricted = restricted
	} else {
		n.warn("restriction_unspecified_assumed_restricted")
	}
	for _, field := range []string{titleField, "problem", "description", "summary"} {
		if value, exists := object[field]; exists {
			text, ok := value.(string)
			if !ok {
				return Report{}, errors.New("selected technical fields must be strings")
			}
			if err := n.technical(field, text, pointer("", field)); err != nil {
				return Report{}, err
			}
		}
	}
	if value, exists := object["repository"]; exists {
		repository, ok := value.(string)
		if !ok || len(repository) > maxMetadataBytes {
			return Report{}, errors.New("repository must be a string of at most 256 bytes")
		}
		if !canonicalRepository(repository) {
			n.warn("noncanonical_repository_omitted")
		} else if err := n.repository(repository, "/repository"); err != nil {
			return Report{}, err
		}
	}
	if value, exists := object["versions"]; exists {
		if err := n.versions(value); err != nil {
			return Report{}, err
		}
	}
	for field := range object {
		if !slices.Contains([]string{titleField, "problem", "description", "summary", "repository", "versions", "restricted"}, field) {
			n.warn("nontechnical_fields_omitted")
			break
		}
	}
	if n.report.Title == "" {
		n.warn("missing_title")
	}
	if !n.technicalBody {
		n.warn("missing_technical_details")
	}
	encoded, err := json.Marshal(n.report)
	if err != nil || len(encoded) > maxDerivedBytes {
		return Report{}, errors.New("normalized report exceeds the 256 KiB derived JSON limit")
	}
	return n.report, nil
}

func (n *normalizer) technical(kind, raw, source string) error {
	if len(raw) > maxRawFieldBytes {
		return errors.New("selected technical field exceeds the 256 KiB limit")
	}
	if kind == titleField && len(raw) > maxTitleBytes {
		return errors.New("title exceeds the 4 KiB limit")
	}
	text, prose, flags, err := normalizeText(raw)
	if err != nil {
		return err
	}
	for _, flag := range flags {
		n.warn(flag)
	}
	if text == "" {
		n.warn("empty_technical_field_omitted")
		return nil
	}
	if kind == titleField {
		if len(text) > maxTitleBytes {
			return errors.New("normalized title exceeds the 4 KiB limit")
		}
		n.report.Title = text
	} else if strings.TrimSpace(strings.ReplaceAll(text, redactionMarker, "")) != "" {
		n.technicalBody = true
	}
	if err := n.section(kind, text, source); err != nil {
		return err
	}
	if kind != titleField {
		for _, candidate := range urlPattern.FindAllString(proseWithoutCode(prose), -1) {
			candidate = trimURLPunctuation(candidate)
			if canonicalRepository(candidate) {
				if err := n.repository(candidate, source); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (n *normalizer) repository(repository, source string) error {
	if !slices.ContainsFunc(n.report.Repositories, func(existing string) bool {
		return strings.EqualFold(existing, repository)
	}) {
		if len(n.report.Repositories) >= maxRepositories {
			return errors.New("report exceeds the 32 repository limit")
		}
		n.report.Repositories = append(n.report.Repositories, repository)
	}
	n.warn("repository_candidates_are_unverified")
	return n.section("repository-candidate", repository, source)
}

func (n *normalizer) versions(value any) error {
	var values []any
	array := false
	switch value := value.(type) {
	case string:
		values = []any{value}
	case []any:
		values, array = value, true
	default:
		return errors.New("versions must be a string or array of strings")
	}
	if len(values) > maxVersions {
		return errors.New("report exceeds the 64 version limit")
	}
	for i, value := range values {
		version, ok := value.(string)
		if !ok || len(version) > maxMetadataBytes || !versionPattern.MatchString(version) {
			return errors.New("versions entries must be explicit version literals of at most 256 bytes")
		}
		if scrubText(version) != version {
			return errors.New("version literals must not contain sensitive text")
		}
		if !slices.Contains(n.report.Versions, version) {
			n.report.Versions = append(n.report.Versions, version)
		}
		source := "/versions"
		if array {
			source = pointer(source, strconv.Itoa(i))
		}
		if err := n.section("reported-version", version, source); err != nil {
			return err
		}
		n.warn("reported_versions_are_unverified")
	}
	return nil
}

func (n *normalizer) section(kind, text, source string) error {
	section := Section{Kind: kind, Text: text, SourcePointer: source}
	if slices.Contains(n.report.Sections, section) {
		return nil
	}
	if len(text) > maxSectionBytes || len(n.report.Sections) >= maxSections ||
		n.textBytes+len(text) > maxTotalTextBytes {
		return errors.New("normalized report exceeds section or text limits")
	}
	n.textBytes += len(text)
	n.report.Sections = append(n.report.Sections, section)
	return nil
}

func (n *normalizer) warn(warning string) {
	if !slices.Contains(n.report.Warnings, warning) {
		n.report.Warnings = append(n.report.Warnings, warning)
	}
}

func pointer(base, token string) string {
	token = strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
	return base + "/" + token
}
