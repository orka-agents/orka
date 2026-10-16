package nativesession

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// bundleManifestEntry names the manifest every bundle carries first.
const bundleManifestEntry = "manifest.json"

// spilledFilePattern matches the names agents give the files they spill tool
// output to.
var spilledFilePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// bundleFile is one file of a bundle.
type bundleFile struct {
	name    string
	content []byte
}

// writeBundle archives the manifest and files, in order, as one gzip member
// holding a tar. It fails with ErrUnsupported when the archive exceeds the
// expanded budget readBundle enforces.
func writeBundle(limit int, manifest any, files []bundleFile) ([]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	var tarred bytes.Buffer
	archive := tar.NewWriter(&tarred)
	for _, file := range append([]bundleFile{{bundleManifestEntry, encoded}}, files...) {
		header := &tar.Header{
			Name: file.name, Mode: 0o600, Size: int64(len(file.content)), Typeflag: tar.TypeReg,
			Format: tar.FormatPAX, ModTime: time.Unix(0, 0),
		}
		if err := archive.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := archive.Write(file.content); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if tarred.Len() > expandedLimit(limit) {
		return nil, fmt.Errorf("%w: conversation exceeds the expanded budget", ErrUnsupported)
	}
	var out bytes.Buffer
	compressed := gzip.NewWriter(&out)
	if _, err := compressed.Write(tarred.Bytes()); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// readBundle reads a bundle strictly: one gzip member holding a tar of regular
// files, each at most once, within the limits. accept reports whether a file
// other than the manifest belongs in the bundle. It returns the manifest and
// the other files.
func readBundle(data []byte, maxBytes int, accept func(name string) bool) ([]byte, map[string][]byte, error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return nil, nil, err
	}
	if len(data) == 0 || len(data) > limit {
		return nil, nil, errors.New("native session bundle exceeds its size bounds")
	}
	source := bytes.NewReader(data)
	compressed, err := gzip.NewReader(source)
	if err != nil {
		return nil, nil, errors.New("native session bundle is not gzip data")
	}
	compressed.Multistream(false)
	files, err := readBundleArchive(compressed, int64(expandedLimit(limit)), accept)
	if err != nil {
		return nil, nil, err
	}
	if trailing, err := io.Copy(io.Discard, io.LimitReader(compressed, 1)); err != nil || trailing != 0 || source.Len() != 0 {
		return nil, nil, errors.New("native session bundle has trailing data")
	}
	manifest, ok := files[bundleManifestEntry]
	if !ok {
		return nil, nil, errors.New("native session bundle has no manifest")
	}
	delete(files, bundleManifestEntry)
	return manifest, files, nil
}

// readBundleArchive reads tar entries until the archive ends, within budget
// bytes of content.
func readBundleArchive(compressed io.Reader, budget int64, accept func(name string) bool) (map[string][]byte, error) {
	archive := tar.NewReader(io.LimitReader(compressed, budget+1))
	files := map[string][]byte{}
	var read int64
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, errors.New("native session bundle archive is invalid")
		}
		if _, seen := files[header.Name]; seen || header.Typeflag != tar.TypeReg || header.Size < 0 || read+header.Size > budget {
			return nil, errors.New("native session bundle entry is invalid")
		}
		if header.Name != bundleManifestEntry && !accept(header.Name) {
			return nil, errors.New("native session bundle has an unknown or excess entry")
		}
		read += header.Size
		content, err := io.ReadAll(io.LimitReader(archive, header.Size))
		if err != nil || int64(len(content)) != header.Size {
			return nil, errors.New("native session bundle entry is truncated")
		}
		files[header.Name] = content
	}
}

// decodeManifest decodes one JSON object with no unknown fields.
func decodeManifest(content []byte, manifest any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decodeOnly(decoder, manifest); err != nil {
		return errors.New("native session bundle manifest is invalid")
	}
	return nil
}

// decodeOnly decodes the one JSON value decoder holds into value; anything
// after it, even a stray closing bracket, is an error.
func decodeOnly(decoder *json.Decoder, value any) error {
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// exactFields decodes line, one JSON object, by exact key. The agents are
// JavaScript and read keys exactly, while Go's struct decoding also matches a
// key that differs in case, so a differently cased duplicate could otherwise
// hide the value the agent reads.
func exactFields(line []byte) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if !bytes.HasPrefix(line, []byte("{")) || decodeOnly(json.NewDecoder(bytes.NewReader(line)), &fields) != nil {
		return nil, false
	}
	return fields, true
}

// exactField decodes fields[name] into value and leaves value unchanged when
// the field is absent.
func exactField(fields map[string]json.RawMessage, name string, value any) error {
	raw, ok := fields[name]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, value)
}
