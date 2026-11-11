package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

func readRemediationInput(input string, mode contract.Mode, requestID string) (contract.Request, error) {
	request := contract.Request{RequestID: requestID, Mode: mode}
	if requestID != "" && !validRemediationID(requestID) {
		return request, errors.New("--request-id must be a canonical DNS subdomain of at most 63 characters")
	}
	switch mode {
	case contract.Generate, contract.Validate:
	default:
		return request, errors.New("--mode must be generate or validate")
	}
	raw, err := readRemediationFile(input, remediationMaxReportBytes, false)
	if err != nil {
		return request, err
	}
	fields, err := remediationJSONObject(raw)
	if err != nil {
		return request, errors.New("report must contain one JSON object with unambiguous fields")
	}
	text := func(name string) string {
		var value string
		_ = json.Unmarshal(fields[name], &value)
		return value
	}
	if strings.TrimSpace(text("title")) == "" ||
		(strings.TrimSpace(text("problem")) == "" && strings.TrimSpace(text("description")) == "") {
		return request, errors.New("report requires a title and a problem or description")
	}
	request.Repository, request.Ref = text("repository"), text("ref")
	repository, err := url.Parse(request.Repository)
	if err != nil || repository.Scheme != "https" || repository.Hostname() == "" || repository.User != nil ||
		repository.RawQuery != "" || repository.Fragment != "" || len(request.Repository) > 2048 {
		return request, errors.New("report requires an HTTPS repository URL without credentials, query parameters, or a fragment")
	}
	if request.Ref == "" || len(request.Ref) > 256 || strings.ContainsRune(request.Ref, utf8.RuneError) ||
		strings.IndexFunc(request.Ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) != -1 {
		return request, errors.New("report requires an explicit ref; version aliases and default branches are not inferred")
	}
	request.Report = raw
	return request, nil
}

func remediationJSONObject(raw []byte) (map[string]json.RawMessage, error) {
	invalid := errors.New("invalid JSON object")
	if !utf8.Valid(raw) {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, invalid
	}
	fields := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		key := strings.ToLower(name)
		if err != nil || !ok || seen[key] {
			return nil, invalid
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, invalid
		}
		fields[name] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, invalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid
	}
	return fields, nil
}

func readRemediationFile(filename string, limit int64, private bool) ([]byte, error) {
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("input file is unavailable, not regular/private, or exceeds its size limit")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, errors.New("input file could not be opened")
	}
	defer func() { _ = file.Close() }()
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.New("input file changed while opening")
	}
	if private {
		if err := checkRemediationPrivateFile(file, current); err != nil {
			return nil, err
		}
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("input could not be read within its size limit")
	}
	return raw, nil
}

func remediationOutputAvailable(path string) error {
	root, _, err := openRemediationOutput(path)
	if err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		return errors.New("patch output directory is unavailable")
	}
	return nil
}

func openRemediationOutput(path string) (*os.Root, string, error) {
	if path == "" || path == "-" || os.IsPathSeparator(path[len(path)-1]) {
		return nil, "", errors.New("--output must name a new private patch file")
	}
	rawParts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	if slices.Contains(rawParts, "..") {
		return nil, "", errors.New("patch output must not contain '..' path components")
	}
	if len(rawParts) == 0 || rawParts[len(rawParts)-1] == "." {
		return nil, "", errors.New("--output must name a new private patch file")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", errors.New("patch output directory is unavailable")
	}
	volumeRoot := filepath.VolumeName(absolute) + string(os.PathSeparator)
	parts := strings.Split(strings.TrimPrefix(absolute, volumeRoot), string(os.PathSeparator))
	name := parts[len(parts)-1]
	if name == "" {
		return nil, "", errors.New("--output must name a new private patch file")
	}
	root, err := os.OpenRoot(volumeRoot)
	if err != nil {
		return nil, "", errors.New("patch output directory is unavailable")
	}
	opened := false
	defer func() {
		if !opened {
			_ = root.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		info, err := root.Lstat(part)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, "", errors.New("patch output directory must exist and must not traverse symlinks")
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			return nil, "", errors.New("patch output directory is unavailable")
		}
		current, err := next.Stat(".")
		// Bind each opened directory to the entry checked without following
		// symlinks, then keep the final parent handle through the write.
		if err != nil || !os.SameFile(info, current) {
			_ = next.Close()
			return nil, "", errors.New("patch output directory changed while opening")
		}
		_ = root.Close()
		root = next
	}
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return nil, "", errors.New("patch output already exists or is unavailable; refusing overwrite")
	}
	opened = true
	return root, name, nil
}

func writeRemediationPatch(path string, patch []byte) error {
	root, name, err := openRemediationOutput(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("could not create a new private patch file; refusing overwrite")
	}
	complete := false
	info, statErr := file.Stat()
	defer func() {
		_ = file.Close()
		if !complete && statErr == nil {
			if current, err := root.Lstat(name); err == nil && os.SameFile(info, current) {
				_ = root.Remove(name)
			}
		}
	}()
	if statErr != nil {
		return errors.New("could not inspect the new patch file")
	}
	if err := checkRemediationPrivateFile(file, info); err != nil {
		return err
	}
	if _, err := file.Write(patch); err != nil {
		return errors.New("could not write the candidate patch")
	}
	if err := file.Sync(); err != nil {
		return errors.New("could not persist the candidate patch")
	}
	if err := file.Close(); err != nil {
		return errors.New("could not close the candidate patch")
	}
	complete = true
	return nil
}
