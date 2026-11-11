package source

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

const (
	MaxInventoryEntries = 32768
	MaxInventoryBytes   = 4 << 20
)

// Entry contains no source content. Regular files have mode 100644 or 100755.
// A 040000 entry identifies an excluded dependency directory: its BlobSHA is
// the tree identity, not a blob. InventoryPaths can explicitly expand it.
// Sizes are API metadata, not part of Git tree hashes; Packet verifies actual
// blob sizes and bytes before admitting any selected content.
type Entry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Size    int64  `json:"size"`
	BlobSHA string `json:"blobSHA"`
}

// Inventory revalidates public visibility and the exact commit/tree association
// before walking cryptographically verified Git trees. It exposes
// only portable regular-file metadata, omits Git metadata, symlinks and gitlinks,
// and defers dependency directories until explicitly requested. No blobs,
// subprocesses, credentials, checkout, or filesystem staging are involved.
//
// The root is nonrecursive so top-level vendor directories remain deferred.
// Other subtrees use one bounded recursive request and reconstruct every tree
// hash before exposing metadata. Truncated subtrees fall back to verified
// nonrecursive traversal, never accept a prefix. Each operation has a 20-second
// deadline, at most 128 traversal batches, 32,768 tree/output entries, and 4 MiB
// of tree/output metadata. Ordinary replies are capped at 1 MiB; recursive
// subtree replies are capped at 4 MiB.
func (c Client) Inventory(ctx context.Context, target Target) ([]Entry, error) {
	return c.inventory(ctx, target, nil)
}

// InventoryPaths explicitly expands 1-32 dependency directories omitted by
// Inventory. The caller must select these from that exact target's inventory.
// The public target and every ancestor tree are independently revalidated here.
// Git metadata is never expandable.
func (c Client) InventoryPaths(ctx context.Context, target Target, directories []string) ([]Entry, error) {
	if err := validatePacketPaths(directories); err != nil {
		return nil, err
	}
	for _, directory := range directories {
		if !inventoryDependencyPath(directory) {
			return nil, errors.New("inventory expansion requires an explicitly selected dependency directory")
		}
	}
	return c.inventory(ctx, target, directories)
}

type inventoryDirectory struct {
	path, identity string
	nonrecursive   bool
}

type inventoryReader struct {
	packetReader
	entries           []Entry
	bytes             int
	visits            int
	recursive         map[string]map[string]map[string]packetTreeEntry
	recursiveDisabled bool
	recursiveDiscards int
}

func (c Client) inventory(ctx context.Context, target Target, directories []string) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePacketTarget(target); err != nil {
		return nil, err
	}
	operation, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	if err := revalidatePublic(operation, c, target); err != nil {
		return nil, err
	}
	base, err := c.apiBase()
	if err != nil {
		return nil, err
	}
	client, transport, err := c.anonymousClient()
	if err != nil {
		return nil, err
	}
	defer transport.CloseIdleConnections()
	reader := inventoryReader{
		packetReader: packetReader{
			client: client, prefix: base + "/repos/" + target.Repository.Owner + "/" + target.Repository.Name,
			identityLength: len(target.Commit), trees: make(map[string]map[string]packetTreeEntry),
		},
		entries: make([]Entry, 0), bytes: 2,
	}
	queue := []inventoryDirectory{{identity: target.Tree}}
	if len(directories) != 0 {
		queue = make([]inventoryDirectory, 0, len(directories))
		for _, directory := range directories {
			identity, err := reader.directory(operation, target.Tree, directory)
			if err != nil {
				return nil, err
			}
			queue = append(queue, inventoryDirectory{path: directory + "/", identity: identity})
		}
	}
	if err := reader.walk(operation, queue, len(directories) != 0); err != nil {
		return nil, err
	}
	slices.SortFunc(reader.entries, func(first, second Entry) int { return strings.Compare(first.Path, second.Path) })
	if err := operation.Err(); err != nil {
		return nil, err
	}
	return reader.entries, nil
}

func (reader *inventoryReader) directory(ctx context.Context, root, name string) (string, error) {
	identity := root
	for part := range strings.SplitSeq(name, "/") {
		tree, err := reader.inventoryTree(ctx, identity)
		if err != nil {
			return "", err
		}
		entry, found := tree[part]
		if !found || entry.Mode != "040000" {
			return "", errPacketPath
		}
		identity = entry.SHA
	}
	return identity, nil
}

func (reader *inventoryReader) walk(ctx context.Context, queue []inventoryDirectory, expanded bool) error {
	for len(queue) != 0 {
		directory := queue[0]
		queue = queue[1:]
		nonrecursive := directory.nonrecursive || reader.recursiveDisabled
		if directory.path != "" && !nonrecursive {
			handled, err := reader.appendRecursive(ctx, directory, expanded)
			if err != nil {
				return err
			}
			if handled {
				continue
			}
			nonrecursive = true
		}
		tree, err := reader.inventoryTree(ctx, directory.identity)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(tree))
		for name := range tree {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			entry := tree[name]
			fullPath := directory.path + name
			if !validPacketPath(fullPath) {
				continue
			}
			switch entry.Mode {
			case "040000":
				if !expanded && inventoryDependencyPath(name) {
					if err := reader.append(Entry{Path: fullPath, Mode: entry.Mode, BlobSHA: entry.SHA}); err != nil {
						return err
					}
				} else {
					if len(queue) >= packetMaxTrees {
						return errPacketLimit
					}
					queue = append(queue, inventoryDirectory{
						path: fullPath + "/", identity: entry.SHA, nonrecursive: nonrecursive,
					})
				}
			case "100644", "100755":
				if err := reader.append(Entry{Path: fullPath, Mode: entry.Mode, BlobSHA: entry.SHA, Size: *entry.Size}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (reader *inventoryReader) inventoryTree(ctx context.Context, identity string) (map[string]packetTreeEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader.visits++
	if reader.visits > packetMaxTrees {
		return nil, errPacketLimit
	}
	tree, err := reader.tree(ctx, identity)
	if err != nil {
		return nil, err
	}
	if reader.treeBytes > MaxInventoryBytes {
		return nil, errPacketLimit
	}
	return tree, nil
}

func (reader *inventoryReader) append(entry Entry) error {
	if (pv.CredentialMatcher{}).MatchString(entry.Path) {
		return errPacketSecret
	}
	encoded, err := json.Marshal(entry)
	if err != nil || len(reader.entries) >= MaxInventoryEntries ||
		len(encoded)+1 > MaxInventoryBytes-reader.bytes {
		return errPacketLimit
	}
	reader.bytes += len(encoded) + 1
	reader.entries = append(reader.entries, entry)
	return nil
}

func inventoryDependencyPath(name string) bool {
	for part := range strings.SplitSeq(name, "/") {
		switch strings.ToLower(part) {
		case "vendor", "node_modules", "third_party", "third-party", "thirdparty":
			return true
		}
	}
	return false
}
