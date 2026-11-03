// Package evals holds the data model of capability evals: the
// orka.eval.case.v1 case schema, the trial outcome graders read, and the
// grader registry. It has no Kubernetes or forge dependencies.
package evals

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// Case limits. A case file lives in one ConfigMap key, so it is at most 1 MiB.
const (
	MaxCaseFileBytes = 1 << 20
	MaxCaseLineBytes = 512 << 10
	MaxCases         = corev1alpha1.EvalRunMaxCaseTrials
	MaxCaseIDLength  = 63
	MaxCaseTags      = 16
	MaxCaseTagLength = 63
	// MaxCaseSetTags caps the distinct tags of one case file so status can
	// hold a result for every grader overall and per tag.
	MaxCaseSetTags       = corev1alpha1.EvalRunMaxResults/corev1alpha1.EvalGraderCount - 1
	MaxReviewFiles       = 100
	MaxReviewTitleBytes  = 1024
	MaxReviewBodyBytes   = 64 << 10
	MaxReviewPatchBytes  = 64 << 10
	MaxReviewPathBytes   = 512
	MaxReviewFileChanges = 1_000_000
	MaxReferenceLocation = 32
	MaxReferenceLine     = 10_000_000
)

// Reference verdicts. A case expects the review either to pass the change or
// to hold it back with any other verdict.
const (
	ReferenceVerdictPassed    = "passed"
	ReferenceVerdictNotPassed = "notPassed"
)

// Case is one orka.eval.case.v1 case. Cases are data only: they never name
// images, commands, credentials or endpoints other than the public fixture
// repository the trial clones anonymously.
type Case struct {
	SchemaVersion string        `json:"schemaVersion"`
	ID            string        `json:"id"`
	Tags          []string      `json:"tags,omitempty"`
	Input         CaseInput     `json:"input"`
	Reference     CaseReference `json:"reference"`
}

// CaseInput is a union with one member per subject type.
type CaseInput struct {
	RepositoryMonitorReview *ReviewCaseInput `json:"repositoryMonitorReview,omitempty"`
}

// ReviewCaseInput is the pull request a repositoryMonitorReview trial reviews.
// Files mirrors the forge pull request files response and is generated from
// the fixture repository diff.
type ReviewCaseInput struct {
	RepoURL string           `json:"repoURL"`
	BaseSHA string           `json:"baseSHA"`
	HeadSHA string           `json:"headSHA"`
	Title   string           `json:"title"`
	Body    string           `json:"body,omitempty"`
	Files   []ReviewCaseFile `json:"files"`
}

// ReviewCaseFile is one changed file of a review case.
type ReviewCaseFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previousFilename,omitempty"`
	Status           string `json:"status"`
	Patch            string `json:"patch,omitempty"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
}

// CaseReference is what graders compare a trial against.
type CaseReference struct {
	// Verdict is passed or notPassed.
	Verdict string `json:"verdict"`
	// Locations are the defect locations a finding should point at.
	Locations []ReferenceLocation `json:"locations,omitempty"`
}

// ReferenceLocation is an inclusive line range in one file of the head.
type ReferenceLocation struct {
	File      string `json:"file"`
	StartLine int64  `json:"startLine"`
	EndLine   int64  `json:"endLine"`
}

var (
	caseIDPattern  = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	gitSHAPattern  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	repoSegPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	// Credential shapes a fixture must never carry. This guards against
	// pasting a real secret into a case; it is not a general secret scanner,
	// so ordinary code that mentions tokens or passwords stays valid.
	credentialPatterns = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{30,}|xox[baprs]-[A-Za-z0-9-]{20,}|AKIA[0-9A-Z]{16})\b`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
		regexp.MustCompile(`(?i)\bauthorization\s*:\s*(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
		regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/@:]+:[^\s/@]+@`),
	}
	reviewFileStatuses = map[string]struct{}{
		"added": {}, "removed": {}, "modified": {}, "renamed": {}, "copied": {}, "changed": {}, "unchanged": {},
	}
)

// ParseCases parses and validates an orka.eval.case.v1 JSONL document: one
// case object per line, blank lines ignored. Unknown, missing or
// case-mismatched fields, duplicate keys, trailing data, oversize input,
// duplicate ids and more than MaxCaseSetTags distinct tags are rejected.
// Every case must carry input for subject.
func ParseCases(data []byte, subject corev1alpha1.EvalSubjectType) ([]Case, error) {
	if len(data) > MaxCaseFileBytes {
		return nil, fmt.Errorf("case file exceeds %d bytes", MaxCaseFileBytes)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("case file is not valid UTF-8")
	}
	reader := bufio.NewReaderSize(bytes.NewReader(data), 64<<10)
	var cases []Case
	seen := map[string]struct{}{}
	tags := map[string]struct{}{}
	for lineNumber := 1; ; lineNumber++ {
		line, err := reader.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(line) > MaxCaseLineBytes {
			return nil, fmt.Errorf("line %d exceeds %d bytes", lineNumber, MaxCaseLineBytes)
		}
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			if len(cases) == MaxCases {
				return nil, fmt.Errorf("case file has more than %d cases", MaxCases)
			}
			c, parseErr := ParseCase(trimmed, subject)
			if parseErr != nil {
				return nil, fmt.Errorf("line %d: %w", lineNumber, parseErr)
			}
			if _, dup := seen[c.ID]; dup {
				return nil, fmt.Errorf("line %d: duplicate case id %q", lineNumber, c.ID)
			}
			seen[c.ID] = struct{}{}
			for _, tag := range c.Tags {
				tags[tag] = struct{}{}
			}
			if len(tags) > MaxCaseSetTags {
				return nil, fmt.Errorf("line %d: case file has more than %d distinct tags", lineNumber, MaxCaseSetTags)
			}
			cases = append(cases, c)
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if len(cases) == 0 {
		return nil, errors.New("case file has no cases")
	}
	return cases, nil
}

// SelectCases returns the cases named by ids in file order, or every case
// when ids is empty. An unknown id is an error.
func SelectCases(cases []Case, ids []string) ([]Case, error) {
	if len(ids) == 0 {
		return cases, nil
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	selected := make([]Case, 0, len(want))
	for _, c := range cases {
		if _, ok := want[c.ID]; ok {
			selected = append(selected, c)
			delete(want, c.ID)
		}
	}
	for _, id := range ids {
		if _, missing := want[id]; missing {
			return nil, fmt.Errorf("case %q is not in the case file", id)
		}
	}
	return selected, nil
}

// ParseCase parses and validates one case object. Every field without
// omitempty is required.
func ParseCase(raw []byte, subject corev1alpha1.EvalSubjectType) (Case, error) {
	var c Case
	if len(raw) > MaxCaseLineBytes {
		return c, fmt.Errorf("case exceeds %d bytes", MaxCaseLineBytes)
	}
	// encoding/json would replace invalid bytes with U+FFFD.
	if !utf8.Valid(raw) {
		return c, errors.New("case is not valid UTF-8")
	}
	if err := checkExactKeys(raw, reflect.TypeFor[Case](), "case"); err != nil {
		return c, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("decode case: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return c, errors.New("case has trailing data")
	}
	if err := c.Validate(subject); err != nil {
		return c, err
	}
	return c, nil
}

// Validate checks a decoded case against the schema and its limits.
func (c Case) Validate(subject corev1alpha1.EvalSubjectType) error {
	if c.SchemaVersion != corev1alpha1.EvalCaseSchemaVersion {
		return fmt.Errorf("schemaVersion must be %q", corev1alpha1.EvalCaseSchemaVersion)
	}
	if err := validateCaseID(c.ID); err != nil {
		return err
	}
	if len(c.Tags) > MaxCaseTags {
		return fmt.Errorf("case %q has more than %d tags", c.ID, MaxCaseTags)
	}
	tags := map[string]struct{}{}
	for _, tag := range c.Tags {
		if len(tag) > MaxCaseTagLength || !caseIDPattern.MatchString(tag) {
			return fmt.Errorf("case %q tag %q must be 1-%d letters, digits, '-', '_' or '.'", c.ID, tag, MaxCaseTagLength)
		}
		if _, dup := tags[tag]; dup {
			return fmt.Errorf("case %q repeats tag %q", c.ID, tag)
		}
		tags[tag] = struct{}{}
	}
	switch subject {
	case corev1alpha1.EvalSubjectRepositoryMonitorReview:
		if c.Input.RepositoryMonitorReview == nil {
			return fmt.Errorf("case %q needs input.repositoryMonitorReview", c.ID)
		}
		if err := c.Input.RepositoryMonitorReview.validate(); err != nil {
			return fmt.Errorf("case %q input: %w", c.ID, err)
		}
	default:
		return fmt.Errorf("subject type %q is not supported", subject)
	}
	if err := c.Reference.validate(); err != nil {
		return fmt.Errorf("case %q reference: %w", c.ID, err)
	}
	return nil
}

func validateCaseID(id string) error {
	if id == "" || len(id) > MaxCaseIDLength || !caseIDPattern.MatchString(id) {
		return fmt.Errorf("case id %q must be 1-%d letters, digits, '-', '_' or '.', starting and ending with a letter or digit", id, MaxCaseIDLength)
	}
	return nil
}

func (in *ReviewCaseInput) validate() error {
	if err := validateFixtureRepoURL(in.RepoURL); err != nil {
		return err
	}
	if !gitSHAPattern.MatchString(in.BaseSHA) {
		return errors.New("baseSHA must be a full lowercase commit SHA")
	}
	if !gitSHAPattern.MatchString(in.HeadSHA) {
		return errors.New("headSHA must be a full lowercase commit SHA")
	}
	if in.BaseSHA == in.HeadSHA {
		return errors.New("baseSHA and headSHA must differ")
	}
	if strings.TrimSpace(in.Title) == "" || len(in.Title) > MaxReviewTitleBytes || hasControl(in.Title, false) {
		return fmt.Errorf("title must be one non-empty line of at most %d bytes", MaxReviewTitleBytes)
	}
	if len(in.Body) > MaxReviewBodyBytes || hasControl(in.Body, true) {
		return fmt.Errorf("body must be at most %d bytes without control characters", MaxReviewBodyBytes)
	}
	if len(in.Files) == 0 || len(in.Files) > MaxReviewFiles {
		return fmt.Errorf("files must list 1-%d files", MaxReviewFiles)
	}
	names := map[string]struct{}{}
	for i, file := range in.Files {
		if err := file.validate(); err != nil {
			return fmt.Errorf("files[%d]: %w", i, err)
		}
		if _, dup := names[file.Filename]; dup {
			return fmt.Errorf("files[%d]: duplicate filename %q", i, file.Filename)
		}
		names[file.Filename] = struct{}{}
	}
	for _, field := range []struct{ name, value string }{{"title", in.Title}, {"body", in.Body}} {
		if containsCredential(field.value) {
			return fmt.Errorf("%s contains a credential", field.name)
		}
	}
	return nil
}

func (f ReviewCaseFile) validate() error {
	if !safeRepoPath(f.Filename) {
		return fmt.Errorf("filename %q must be a clean relative path of at most %d bytes", f.Filename, MaxReviewPathBytes)
	}
	if _, ok := reviewFileStatuses[f.Status]; !ok {
		return fmt.Errorf("status %q is not a pull request file status", f.Status)
	}
	if f.PreviousFilename != "" {
		if !safeRepoPath(f.PreviousFilename) {
			return fmt.Errorf("previousFilename %q must be a clean relative path of at most %d bytes", f.PreviousFilename, MaxReviewPathBytes)
		}
		if f.Status != "renamed" && f.Status != "copied" {
			return errors.New("previousFilename is only allowed for renamed or copied files")
		}
	}
	if f.Additions < 0 || f.Additions > MaxReviewFileChanges || f.Deletions < 0 || f.Deletions > MaxReviewFileChanges {
		return fmt.Errorf("additions and deletions must be from 0 to %d", MaxReviewFileChanges)
	}
	if len(f.Patch) > MaxReviewPatchBytes || hasControl(f.Patch, true) {
		return fmt.Errorf("patch must be at most %d bytes without control characters", MaxReviewPatchBytes)
	}
	if containsCredential(f.Patch) {
		return errors.New("patch contains a credential")
	}
	return nil
}

func (r CaseReference) validate() error {
	switch r.Verdict {
	case ReferenceVerdictPassed:
		if len(r.Locations) > 0 {
			return errors.New("a passed reference has no defect locations")
		}
	case ReferenceVerdictNotPassed:
	default:
		return fmt.Errorf("verdict must be %q or %q", ReferenceVerdictPassed, ReferenceVerdictNotPassed)
	}
	if len(r.Locations) > MaxReferenceLocation {
		return fmt.Errorf("at most %d locations are allowed", MaxReferenceLocation)
	}
	for i, location := range r.Locations {
		if !safeRepoPath(location.File) {
			return fmt.Errorf("locations[%d].file %q must be a clean relative path", i, location.File)
		}
		if location.StartLine < 1 || location.EndLine < location.StartLine || location.EndLine > MaxReferenceLine {
			return fmt.Errorf("locations[%d] needs 1 <= startLine <= endLine <= %d", i, MaxReferenceLine)
		}
	}
	return nil
}

// validateFixtureRepoURL accepts only an anonymous https URL of a GitHub
// repository: no credentials, port, query, fragment or extra path.
func validateFixtureRepoURL(raw string) error {
	// Never echo raw: a rejected URL may carry a credential.
	invalid := errors.New("repoURL must be an anonymous https://github.com/<owner>/<repo> URL")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return invalid
	}
	segments := strings.Split(strings.TrimPrefix(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(segments) != 2 || !repoSegPattern.MatchString(segments[0]) || !repoSegPattern.MatchString(segments[1]) {
		return invalid
	}
	return nil
}

func safeRepoPath(p string) bool {
	if p == "" || len(p) > MaxReviewPathBytes || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || hasControl(p, false) {
		return false
	}
	cleaned := path.Clean(p)
	if cleaned != p || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	return true
}

// hasControl reports control or format characters (such as bidirectional
// overrides). multiline allows newlines and tabs.
func hasControl(s string, multiline bool) bool {
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t' || r == '\r') {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

func containsCredential(s string) bool {
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(s) {
			return true
		}
	}
	return false
}

// checkExactKeys rejects duplicate keys, keys that do not exactly match a
// JSON field of t, and missing fields that lack omitempty. encoding/json would
// otherwise accept a case-folded key ("Verdict"), let a later duplicate
// silently win, and read a missing count as zero.
func checkExactKeys(raw []byte, t reflect.Type, at string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			// Let the typed decode report type errors and null.
			return nil
		}
		fields := jsonFields(t)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		if _, err := decoder.Token(); err != nil {
			return err
		}
		seen := make(map[string]struct{}, len(object))
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, _ := token.(string)
			if _, dup := seen[key]; dup {
				return fmt.Errorf("%s has duplicate field %q", at, key)
			}
			seen[key] = struct{}{}
			field, ok := fields[key]
			if !ok {
				return fmt.Errorf("%s has unknown field %q", at, key)
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				return err
			}
			if err := checkExactKeys(value, field.typ, at+"."+key); err != nil {
				return err
			}
		}
		for name, field := range fields {
			if _, present := seen[name]; field.required && !present {
				return fmt.Errorf("%s is missing field %q", at, name)
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil
		}
		for i, item := range items {
			if err := checkExactKeys(item, t.Elem(), fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

type jsonField struct {
	typ      reflect.Type
	required bool
}

func jsonFields(t reflect.Type) map[string]jsonField {
	fields := make(map[string]jsonField, t.NumField())
	for field := range t.Fields() {
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" || !field.IsExported() {
			continue
		}
		fields[name] = jsonField{typ: field.Type, required: !strings.Contains(","+options+",", ",omitempty,")}
	}
	return fields
}
