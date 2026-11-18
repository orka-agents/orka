package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

const (
	packetVersion        = 1
	packetMaxFiles       = 32
	packetMaxTextBytes   = 256 << 10
	packetMaxJSONBytes   = 2 << 20
	packetMaxTrees       = 128
	packetMaxTreeEntries = 32768
	packetMaxTreeBytes   = 8 << 20
	maxPathBytes         = 1024
	maxPathDepth         = 32
)

var (
	errPacketInput     = errors.New("invalid source packet target, selection, or document shape")
	errPacketLimit     = errors.New("source packet exceeds file, text, or tree limits")
	errPacketPath      = errors.New("source packet requires distinct, unaliased, safe relative file paths")
	errPacketIntegrity = errors.New("source packet object identity or content integrity mismatch")
	errPacketText      = errors.New("source packet files must be nonempty UTF-8 newline-terminated text without binary control characters")
	errPacketSecret    = errors.New("source packet cannot contain credentials")
)

// Packet is a version-1, ordered selection of source files, not a complete
// checkout or a proof of repository provenance. Content remains untrusted input.
type Packet struct {
	Version int          `json:"version"`
	Target  Target       `json:"target"`
	Files   []PacketFile `json:"files"`
}

// PacketFile preserves exact bytes as UTF-8 text. SHA256 is the lowercase,
// unprefixed SHA-256 of Content. BlobSHA includes Git's blob header when hashed.
type PacketFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
	BlobSHA string `json:"blobSHA"`
}

// Packet anonymously revalidates the public Target's commit-to-tree linkage and
// selects regular files through nonrecursive Git trees, never the contents API.
// Both tree identities and blob identities are verified against their bytes.
//
// Select 1-32 portable ASCII relative paths, without traversal, case aliases, or
// file/directory conflicts. Content must be nonempty UTF-8 text ending in LF, at
// most 256 KiB per file and in total. Source is screened with the repository's
// CredentialMatcher, which rejects known credential patterns but is not an
// exhaustive secret detector. Unknown credentials are not made safe by hashing.
//
// The whole request has a 20-second deadline and the usual 1-MiB API reply
// limit. Tree lookups are cached only within this request, bounded to 128 trees
// and 32,768 entries / 8 MiB of raw tree data in total. Unselected symlinks,
// gitlinks, and large files do not require materialization. No Git subprocess or
// filesystem staging is used.
func (c Client) Packet(ctx context.Context, target Target, selected []string) (Packet, error) {
	if err := ctx.Err(); err != nil {
		return Packet{}, err
	}
	if err := validatePacketTarget(target); err != nil {
		return Packet{}, err
	}
	if err := validatePacketPaths(selected); err != nil {
		return Packet{}, err
	}
	operation, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	if err := revalidatePublic(operation, c, target); err != nil {
		return Packet{}, err
	}
	base, err := c.apiBase()
	if err != nil {
		return Packet{}, err
	}
	client, transport, err := c.anonymousClient()
	if err != nil {
		return Packet{}, err
	}
	defer transport.CloseIdleConnections()
	reader := packetReader{
		client: client, prefix: base + "/repos/" + target.Repository.Owner + "/" + target.Repository.Name,
		identityLength: len(target.Commit), trees: make(map[string]map[string]packetTreeEntry),
	}
	entries := make([]packetTreeEntry, 0, len(selected))
	total := int64(0)
	for _, name := range selected {
		entry, err := reader.file(operation, target.Tree, name)
		if err != nil {
			return Packet{}, err
		}
		if entry.Size == nil || *entry.Size <= 0 || *entry.Size > packetMaxTextBytes-total {
			return Packet{}, errPacketLimit
		}
		total += *entry.Size
		entries = append(entries, entry)
	}
	packet := Packet{Version: packetVersion, Target: target, Files: make([]PacketFile, 0, len(selected))}
	for index, entry := range entries {
		content, err := reader.blob(operation, entry)
		if err != nil {
			return Packet{}, err
		}
		packet.Files = append(packet.Files, PacketFile{
			Path: selected[index], Content: string(content), SHA256: packetContentSHA(content), BlobSHA: entry.SHA,
		})
	}
	if err := validatePacket(packet); err != nil {
		return Packet{}, err
	}
	if err := operation.Err(); err != nil {
		return Packet{}, err
	}
	return packet, nil
}

// DecodePacket checks a bounded version-1 document's exact required JSON fields,
// paths, text, credential screening, and file self-digests. It makes no network
// requests. Valid self-digests DO NOT establish a file's inclusion in Target's
// tree or its public GitHub origin; obtain remote binding through Client.Packet.
// The JSON envelope is limited to 2 MiB to accommodate escaped bounded text.
func DecodePacket(data []byte) (Packet, error) {
	if len(data) > packetMaxJSONBytes {
		return Packet{}, errPacketLimit
	}
	if !unambiguousJSON(data) || !packetJSONShape(data) {
		return Packet{}, errPacketInput
	}
	var packet Packet
	if json.Unmarshal(data, &packet) != nil {
		return Packet{}, errPacketInput
	}
	if err := validatePacket(packet); err != nil {
		return Packet{}, err
	}
	return packet, nil
}

func validatePacketTarget(target Target) error {
	if err := validateTarget(target); err != nil {
		return errPacketInput
	}
	content, err := json.Marshal(target)
	if err != nil {
		return errPacketInput
	}
	if (pv.CredentialMatcher{}).Match(content) {
		return errPacketSecret
	}
	return nil
}

func validatePacket(packet Packet) error {
	if packet.Version != packetVersion {
		return errPacketInput
	}
	if err := validatePacketTarget(packet.Target); err != nil {
		return err
	}
	paths := make([]string, 0, len(packet.Files))
	for _, file := range packet.Files {
		paths = append(paths, file.Path)
	}
	if err := validatePacketPaths(paths); err != nil {
		return err
	}
	total := 0
	for _, file := range packet.Files {
		if len(file.Content) > packetMaxTextBytes-total {
			return errPacketLimit
		}
		total += len(file.Content)
		if err := validatePacketText(file.Content); err != nil {
			return err
		}
		if (pv.CredentialMatcher{}).MatchString(file.Content) {
			return errPacketSecret
		}
		content := []byte(file.Content)
		if file.SHA256 != packetContentSHA(content) || !validObjectID(file.BlobSHA) ||
			len(file.BlobSHA) != len(packet.Target.Commit) {
			return errPacketIntegrity
		}
		identity, err := packetGitID(packetBlobType, content, len(file.BlobSHA))
		if err != nil || identity != file.BlobSHA {
			return errPacketIntegrity
		}
	}
	return nil
}

func packetContentSHA(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func validatePacketText(content string) error {
	if content == "" || !utf8.ValidString(content) || !strings.HasSuffix(content, "\n") {
		return errPacketText
	}
	for _, char := range content {
		if unicode.IsControl(char) && char != '\t' && char != '\n' && char != '\r' {
			return errPacketText
		}
	}
	return nil
}

func validatePacketPaths(selected []string) error {
	if len(selected) == 0 {
		return errPacketInput
	}
	if len(selected) > packetMaxFiles {
		return errPacketLimit
	}
	// Track prefixes as well as full names: A/one and a/two alias a directory,
	// and file "a" conflicts with file "a/two" even though their strings differ.
	seen := make(map[string]struct {
		path string
		file bool
	})
	for _, name := range selected {
		if !validPacketPath(name) {
			return errPacketPath
		}
		if (pv.CredentialMatcher{}).MatchString(name) {
			return errPacketSecret
		}
		parts := strings.Split(name, "/")
		for index := range parts {
			prefix := strings.Join(parts[:index+1], "/")
			key := strings.ToLower(prefix)
			isFile := index == len(parts)-1
			previous, found := seen[key]
			if found && (previous.path != prefix || previous.file || isFile) {
				return errPacketPath
			}
			seen[key] = struct {
				path string
				file bool
			}{path: prefix, file: isFile}
		}
	}
	return nil
}

func validPacketPath(name string) bool {
	if !validSourcePath(name) || strings.ContainsAny(name, `%?#*<>"|`) {
		return false
	}
	for _, char := range name {
		if char > 126 {
			return false
		}
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") {
			return false
		}
		base, _, _ := strings.Cut(strings.ToUpper(part), ".")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
			"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return false
		}
	}
	return true
}

func validSourcePath(name string) bool {
	if name == "" || len(name) > maxPathBytes || !utf8.ValidString(name) ||
		strings.ContainsAny(name, `\:`) || strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) > maxPathDepth {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 255 ||
			strings.EqualFold(strings.TrimRight(part, ". "), ".git") {
			return false
		}
	}
	return true
}

func packetJSONShape(data []byte) bool {
	document, ok := packetJSONFields(data, "version", "target", "files")
	if !ok {
		return false
	}
	target, ok := packetJSONFields(document["target"], "repository", "ref", "commit", "tree")
	if !ok {
		return false
	}
	if _, ok := packetJSONFields(target["repository"], "url", "owner", "name", "defaultBranch", "archived"); !ok {
		return false
	}
	files, err := decodePacketArray[json.RawMessage](document["files"], packetMaxFiles)
	if err != nil || len(files) == 0 {
		return false
	}
	for _, file := range files {
		if _, ok := packetJSONFields(file, "path", "content", "sha256", "blobSHA"); !ok {
			return false
		}
	}
	return true
}

func packetJSONFields(data []byte, names ...string) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != len(names) {
		return nil, false
	}
	for _, name := range names {
		value, found := fields[name]
		if !found || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, false
		}
	}
	return fields, true
}

func decodePacketArray[T any](data []byte, limit int) ([]T, error) {
	if limit < 0 {
		return nil, errPacketLimit
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errPacketInput
	}
	values := make([]T, 0, min(64, limit))
	for decoder.More() {
		if len(values) == limit {
			return nil, errPacketLimit
		}
		var value T
		if decoder.Decode(&value) != nil {
			return nil, errPacketInput
		}
		values = append(values, value)
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim(']') {
		return nil, errPacketInput
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errPacketInput
	}
	return values, nil
}
