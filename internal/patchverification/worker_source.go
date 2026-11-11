package patchverification

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

const (
	PreparationProtocolVersion  = 1
	MaxPreparationInputBytes    = 512 << 10
	MaxPreparationPatchBytes    = 256 << 10
	MaxPreparationReportBytes   = 2 << 20
	MaxPublicSourceArchiveBytes = 512 << 10
	PreparationTaskKey          = "orka.remediation.preparation-task"
	PreparationUIDKey           = "orka.remediation.preparation-uid"
	PreparationDigestKey        = "orka.remediation.preparation-digest"
	PreparationParentTaskKey    = "orka.remediation.parent-task"
	PreparationParentUIDKey     = "orka.remediation.parent-uid"
	PreparationPolicyKey        = "orka.remediation.preparation-policy"
	PreparationConfigurationKey = "orka.remediation.configuration"
	PreparationPolicy           = "public-git-v2-cgroup-pids"
	PreparationInputAnnotation  = ValidationTaskPrefix + "preparation-input"
	PreparationCancelAnnotation = ValidationTaskPrefix + "preparation-cancelled"
)

var publicRepositoryPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}$`)

func PreparationCancellationFenceName(taskName string) string {
	return taskName + "-cancelled"
}

// PreparationInput binds the public-source operation to the coordinator and its
// accepted Task. The manifest's archive identities are filled only by the worker.
type PreparationInput struct {
	Version        int      `json:"version"`
	ID             string   `json:"id"`
	Namespace      string   `json:"namespace"`
	ParentTaskName string   `json:"parentTaskName"`
	ParentTaskUID  string   `json:"parentTaskUID"`
	TaskUID        string   `json:"taskUID,omitempty"`
	Manifest       Manifest `json:"manifest"`
	Patch          []byte   `json:"patch,omitempty"`
}

type PreparationReport struct {
	Version     int               `json:"version"`
	TaskUID     string            `json:"taskUID"`
	PodUID      string            `json:"podUID"`
	InputDigest string            `json:"inputDigest"`
	Manifest    Manifest          `json:"manifest"`
	Provenance  map[string][]byte `json:"provenance"`
}

func PreparationInputDigest(input PreparationInput) (string, error) {
	input.TaskUID = ""
	content, err := json.Marshal(input)
	if err != nil || len(content) > MaxPreparationInputBytes {
		return "", ErrLimit
	}
	return Digest(content), nil
}

func ValidatePreparationInput(input PreparationInput) error {
	manifest := input.Manifest
	if !validPreparationIdentity(input) || !validPreparationContract(input) || !validPreparationText(reflect.ValueOf(input)) {
		return ErrEvidence
	}
	manifest.Sources.Original.ArchiveDigest = Digest(nil)
	if manifest.Action == VerifyPatch {
		manifest.Sources.Patched = SourceIdentity{Tree: manifest.Sources.Original.Tree, ArchiveDigest: Digest(nil)}
		manifest.Sources.PatchDigest, manifest.Sources.DiffDigest = Digest(input.Patch), Digest(nil)
	}
	if ValidateManifest(manifest) != nil {
		return ErrEvidence
	}
	if err := validatePreparationFiles(manifest); err != nil {
		return err
	}
	content, err := json.Marshal(input)
	if err != nil || len(content) > MaxPreparationInputBytes || ContainsCredentialMaterial(content) ||
		ContainsCredentialMaterial(input.Patch) {
		return ErrLimit
	}
	sizing := input
	sizing.TaskUID = strings.Repeat("u", 128)
	content, err = json.Marshal(sizing)
	if err != nil || len(content) > MaxPreparationInputBytes {
		return ErrLimit
	}
	return nil
}

func validPreparationText(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		return value.IsNil() || validPreparationText(value.Elem())
	case reflect.String:
		return sourceCleanText(value.String())
	case reflect.Struct:
		for _, field := range value.Fields() {
			if !validPreparationText(field) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return true
		}
		for i := 0; i < value.Len(); i++ {
			if !validPreparationText(value.Index(i)) {
				return false
			}
		}
	case reflect.Map:
		entries := value.MapRange()
		for entries.Next() {
			if !validPreparationText(entries.Key()) || !validPreparationText(entries.Value()) {
				return false
			}
		}
	}
	return true
}

func validPreparationIdentity(input PreparationInput) bool {
	manifest := input.Manifest
	source := manifest.Sources
	name := source.Repository[strings.LastIndex(source.Repository, "/")+1:]
	if input.Version != PreparationProtocolVersion || !identifierPattern.MatchString(input.ID) ||
		!identifierPattern.MatchString(input.Namespace) || !identifierPattern.MatchString(input.ParentTaskName) ||
		!identifierPattern.MatchString(input.ParentTaskUID) ||
		(input.TaskUID != "" && !identifierPattern.MatchString(input.TaskUID)) ||
		!publicRepositoryPattern.MatchString(source.Repository) || name == "." || name == ".." ||
		strings.HasSuffix(name, ".git") || !sourceValidCommit(source.Original.Commit) ||
		!sourceValidCommit(source.Original.Tree) || len(source.Original.Commit) != len(source.Original.Tree) ||
		source.Original.ArchiveDigest != "" || source.Patched != (SourceIdentity{}) ||
		source.DiffDigest != "" || source.PatchDigest != "" {
		return false
	}
	return true
}

func validPreparationContract(input PreparationInput) bool {
	manifest := input.Manifest
	if manifest.Application != nil {
		return validApplicationPreparationContract(input)
	}
	if (manifest.Action != ValidateReport && manifest.Action != VerifyPatch) ||
		(manifest.Action == ValidateReport && len(input.Patch) != 0) ||
		(manifest.Action == VerifyPatch && len(input.Patch) == 0) ||
		len(input.Patch) > MaxPreparationPatchBytes || !sourceCleanText(string(input.Patch)) ||
		len(manifest.Files) == 0 || len(manifest.Files) > MaxFrozenFiles ||
		manifest.EarlierValidation != nil || manifest.Environment.Profile != LocalServices ||
		len(manifest.Environment.Services) != 0 || len(manifest.Environment.Variables) != 0 ||
		!sha256Pattern.MatchString(manifest.Environment.Dependencies[PreparationConfigurationKey]) ||
		manifest.Environment.Dependencies[PreparationPolicyKey] != PreparationPolicy {
		return false
	}
	return len(MissingRequirements(manifest.Environment)) == 0
}

func validatePreparationFiles(manifest Manifest) error {
	if manifest.Application != nil {
		if len(manifest.Files) != 0 {
			return ErrEvidence
		}
		return nil
	}
	files := make(map[string]FrozenFile)
	for _, file := range manifest.Files {
		if _, found := files[file.Path]; found {
			return ErrEvidence
		}
		if !sourceValidPath(file.Path) || !sourceCleanText(string(file.Content)) || ContainsCredentialMaterial(file.Content) {
			return ErrEvidence
		}
		files[file.Path] = file
	}
	for _, check := range manifest.Checks {
		if ValidateHTTPCheck(check) != nil {
			return ErrEvidence
		}
		if file, declared := files[strings.TrimPrefix(CheckExecutable(check)[0], "/checks/")]; !declared || !file.Executable {
			return ErrEvidence
		}
	}
	return nil
}

func ValidatePreparationReport(input PreparationInput, report PreparationReport) error {
	if input.Manifest.Application != nil {
		return ErrEvidence
	}
	digest, err := PreparationInputDigest(input)
	if err != nil || ValidatePreparationInput(input) != nil || report.Version != PreparationProtocolVersion ||
		report.TaskUID != input.TaskUID || report.InputDigest != digest || ValidateManifest(report.Manifest) != nil {
		return ErrEvidence
	}
	expected := input.Manifest
	expected.Sources = report.Manifest.Sources
	if !reflect.DeepEqual(expected, report.Manifest) ||
		report.Manifest.Sources.Repository != input.Manifest.Sources.Repository ||
		report.Manifest.Sources.Original.Commit != input.Manifest.Sources.Original.Commit ||
		report.Manifest.Sources.Original.Tree != input.Manifest.Sources.Original.Tree ||
		!slices.Equal(report.Manifest.Sources.Paths, input.Manifest.Sources.Paths) {
		return ErrBinding
	}
	required := map[string]bool{report.Manifest.Sources.Original.ArchiveDigest: true}
	for _, file := range report.Manifest.Files {
		required[file.Digest] = true
	}
	if input.Manifest.Action == VerifyPatch {
		if report.Manifest.Sources.Patched.Commit != "" || report.Manifest.Sources.PatchDigest != Digest(input.Patch) {
			return ErrBinding
		}
		required[report.Manifest.Sources.Patched.ArchiveDigest] = true
		required[report.Manifest.Sources.PatchDigest], required[report.Manifest.Sources.DiffDigest] = true, true
	}
	if len(report.Provenance) != len(required) {
		return ErrEvidence
	}
	archiveLimit, reportLimit := MaxPublicSourceArchiveBytes, MaxPreparationReportBytes
	for id := range required {
		blob, ok := report.Provenance[id]
		if !ok || Digest(blob) != id || len(blob) > archiveLimit || ContainsCredentialMaterial(blob) {
			return ErrEvidence
		}
	}
	content, err := json.Marshal(report)
	if err != nil || len(content)+1 > reportLimit || ContainsCredentialMaterial(content) {
		return ErrLimit
	}

	return nil
}

func validApplicationPreparationContract(input PreparationInput) bool {
	manifest := input.Manifest
	return (manifest.Action == ValidateReport || manifest.Action == VerifyPatch) &&
		(manifest.Action != ValidateReport || len(input.Patch) == 0) &&
		(manifest.Action != VerifyPatch || len(input.Patch) != 0) &&
		len(input.Patch) <= MaxPreparationPatchBytes && sourceCleanText(string(input.Patch)) &&
		len(manifest.Files) == 0 && len(manifest.Sources.Paths) == 0 && manifest.EarlierValidation == nil &&
		manifest.ApplicationSourceMetadata == nil &&
		manifest.Environment.Profile == ApplicationRuntimeProfile &&
		len(manifest.Environment.Services) == 0 && len(manifest.Environment.Variables) == 0 &&
		sha256Pattern.MatchString(manifest.Environment.Dependencies[PreparationConfigurationKey]) &&
		manifest.Environment.Dependencies[PreparationPolicyKey] == PreparationPolicy &&
		len(MissingRequirements(manifest.Environment)) == 0
}

func DecodePreparationInput(reader io.Reader) (PreparationInput, error) {
	var input PreparationInput
	if err := decodePreparationJSON(reader, &input, MaxPreparationInputBytes); err != nil {
		return input, err
	}
	return input, ValidatePreparationInput(input)
}

func DecodePreparationReport(reader io.Reader) (PreparationReport, error) {
	var report PreparationReport
	err := decodePreparationJSON(reader, &report, MaxPreparationReportBytes)
	return report, err
}

func decodePreparationJSON(reader io.Reader, value any, limit int) error {
	content, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return err
	}
	if len(content) > limit {
		return ErrLimit
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return io.ErrUnexpectedEOF
		}
		return errors.New("invalid bounded preparation document")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid bounded preparation document")
	}
	return nil
}
