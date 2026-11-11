package source

import (
	"bytes"
	"context"
	"crypto/sha1" // Git SHA-1 object identities; packet content digests use SHA-256.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	packetTreeType = "tree"
	packetBlobType = "blob"
)

type packetReader struct {
	client         *http.Client
	prefix         string
	identityLength int
	trees          map[string]map[string]packetTreeEntry
	treeEntries    int
	treeBytes      int
}

type packetTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size *int64 `json:"size"`
}

type packetTreeReply struct {
	SHA       string          `json:"sha"`
	Tree      json.RawMessage `json:"tree"`
	Truncated *bool           `json:"truncated"`
}

type packetBlobReply struct {
	SHA      string  `json:"sha"`
	Size     *int64  `json:"size"`
	Encoding string  `json:"encoding"`
	Content  *string `json:"content"`
}

func (reader *packetReader) file(ctx context.Context, root, name string) (packetTreeEntry, error) {
	parts := strings.Split(name, "/")
	identity := root
	for index, part := range parts {
		tree, err := reader.tree(ctx, identity)
		if err != nil {
			return packetTreeEntry{}, err
		}
		entry, found := tree[part]
		if !found {
			return packetTreeEntry{}, errPacketPath
		}
		if index == len(parts)-1 {
			if entry.Type != packetBlobType || (entry.Mode != "100644" && entry.Mode != "100755") {
				return packetTreeEntry{}, errPacketPath
			}
			return entry, nil
		}
		if entry.Type != packetTreeType || entry.Mode != "040000" {
			return packetTreeEntry{}, errPacketPath
		}
		identity = entry.SHA
	}
	return packetTreeEntry{}, errPacketPath
}

func (reader *packetReader) tree(ctx context.Context, identity string) (map[string]packetTreeEntry, error) {
	if previous, found := reader.trees[identity]; found {
		return previous, nil
	}
	if !validObjectID(identity) || len(identity) != reader.identityLength {
		return nil, errPacketIntegrity
	}
	if len(reader.trees) >= packetMaxTrees {
		return nil, errPacketLimit
	}
	var reply packetTreeReply
	if err := getJSON(ctx, reader.client, reader.prefix+"/git/trees/"+identity, &reply, "packet tree lookup"); err != nil {
		return nil, err
	}
	if reply.SHA != identity || len(reply.Tree) == 0 || reply.Truncated == nil || *reply.Truncated {
		return nil, errPacketIntegrity
	}
	entries, err := decodePacketArray[packetTreeEntry](reply.Tree, packetMaxTreeEntries-reader.treeEntries)
	if err != nil {
		return nil, err
	}
	tree, content, err := packetTreeContent(entries, reader.identityLength)
	if err != nil {
		return nil, err
	}
	if len(content) > packetMaxTreeBytes-reader.treeBytes {
		return nil, errPacketLimit
	}
	actual, err := packetGitID(packetTreeType, content, reader.identityLength)
	if err != nil || actual != identity {
		return nil, errPacketIntegrity
	}
	reader.treeEntries += len(entries)
	reader.treeBytes += len(content)
	reader.trees[identity] = tree
	return tree, nil
}

func packetTreeContent(entries []packetTreeEntry, identityLength int) (map[string]packetTreeEntry, []byte, error) {
	tree := make(map[string]packetTreeEntry, len(entries))
	for _, entry := range entries {
		if entry.Path == "" || entry.Path == "." || entry.Path == ".." || !utf8.ValidString(entry.Path) ||
			strings.ContainsAny(entry.Path, "/\x00") || !validObjectID(entry.SHA) || len(entry.SHA) != identityLength {
			return nil, nil, errPacketIntegrity
		}
		if _, found := tree[entry.Path]; found {
			return nil, nil, errPacketIntegrity
		}
		switch entry.Mode {
		case "040000":
			if entry.Type != packetTreeType {
				return nil, nil, errPacketIntegrity
			}
		case "100644", "100755", "120000":
			if entry.Type != packetBlobType || entry.Size == nil || *entry.Size < 0 {
				return nil, nil, errPacketIntegrity
			}
		case "160000":
			if entry.Type != "commit" {
				return nil, nil, errPacketIntegrity
			}
		default:
			return nil, nil, errPacketIntegrity
		}
		tree[entry.Path] = entry
	}
	// Git sorts trees as if their names ended in '/', not as JSON object keys.
	slices.SortFunc(entries, func(first, second packetTreeEntry) int {
		return strings.Compare(packetTreeOrder(first), packetTreeOrder(second))
	})
	var content bytes.Buffer
	for _, entry := range entries {
		mode := entry.Mode
		if mode == "040000" {
			mode = "40000"
		}
		identity, err := hex.DecodeString(entry.SHA)
		if err != nil {
			return nil, nil, errPacketIntegrity
		}
		content.WriteString(mode)
		content.WriteByte(' ')
		content.WriteString(entry.Path)
		content.WriteByte(0)
		content.Write(identity)
	}
	return tree, content.Bytes(), nil
}

func packetTreeOrder(entry packetTreeEntry) string {
	if entry.Mode == "040000" {
		return entry.Path + "/"
	}
	return entry.Path
}

func (reader *packetReader) blob(ctx context.Context, entry packetTreeEntry) ([]byte, error) {
	var reply packetBlobReply
	if err := getJSON(ctx, reader.client, reader.prefix+"/git/blobs/"+entry.SHA, &reply, "packet blob lookup"); err != nil {
		return nil, err
	}
	if reply.SHA != entry.SHA || reply.Size == nil || entry.Size == nil || *reply.Size != *entry.Size ||
		*reply.Size <= 0 || *reply.Size > packetMaxTextBytes || reply.Encoding != "base64" || reply.Content == nil {
		return nil, errPacketIntegrity
	}
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(*reply.Content))
	content, err := io.ReadAll(io.LimitReader(decoder, *reply.Size+1))
	if err != nil || int64(len(content)) != *reply.Size {
		return nil, errPacketIntegrity
	}
	actual, err := packetGitID(packetBlobType, content, reader.identityLength)
	if err != nil || actual != entry.SHA {
		return nil, errPacketIntegrity
	}
	return content, nil
}

func packetGitID(kind string, content []byte, identityLength int) (string, error) {
	var digest hash.Hash
	switch identityLength {
	case 40:
		digest = sha1.New()
	case 64:
		digest = sha256.New()
	default:
		return "", errPacketIntegrity
	}
	_, _ = fmt.Fprintf(digest, "%s %d\x00", kind, len(content))
	_, _ = digest.Write(content)
	return hex.EncodeToString(digest.Sum(nil)), nil
}
