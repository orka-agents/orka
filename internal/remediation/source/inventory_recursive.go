package source

import (
	"context"
	"errors"
	"maps"
	"path"
	"slices"
	"strings"
)

func (reader *inventoryReader) appendRecursive(ctx context.Context, directory inventoryDirectory, expanded bool) (bool, error) {
	reader.visits++
	if reader.visits > packetMaxTrees {
		return false, errPacketLimit
	}
	trees, found := reader.recursive[directory.identity]
	if !found {
		var reply packetTreeReply
		if err := getJSONWithinLimit(ctx, reader.client, reader.prefix+"/git/trees/"+directory.identity+"?recursive=1",
			&reply, "recursive inventory lookup", MaxInventoryBytes); err != nil {
			if errors.Is(err, errSourceReplyLimit) {
				return reader.useNonrecursiveInventory()
			}
			return false, err
		}
		if reply.SHA != directory.identity || reply.Truncated == nil || len(reply.Tree) == 0 {
			return false, errPacketIntegrity
		}
		if *reply.Truncated {
			return reader.useNonrecursiveInventory()
		}
		entries, err := decodePacketArray[packetTreeEntry](reply.Tree, packetMaxTreeEntries)
		if err != nil {
			if errors.Is(err, errPacketLimit) {
				return reader.useNonrecursiveInventory()
			}
			return false, err
		}
		trees, _, err = verifiedRecursiveTrees(directory.identity, entries, reader.identityLength)
		if err != nil {
			if errors.Is(err, errPacketLimit) {
				return reader.useNonrecursiveInventory()
			}
			return false, err
		}
		// Dependency contents were verified with the recursive root but are not
		// retained or charged to the traversable inventory. Otherwise a nested
		// vendor tree could exhaust the budget for unrelated eligible siblings.
		var count, size int
		trees, count, size = retainInventoryTrees(trees, expanded, reader.identityLength)
		if size > MaxInventoryBytes-reader.treeBytes || count > packetMaxTreeEntries-reader.treeEntries {
			return reader.useNonrecursiveInventory()
		}
		reader.treeBytes += size
		reader.treeEntries += count
		if reader.recursive == nil {
			reader.recursive = make(map[string]map[string]map[string]packetTreeEntry)
		}
		reader.recursive[directory.identity] = trees
	}
	queue := []string{""}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		prefix := queue[0]
		queue = queue[1:]
		for _, name := range slices.Sorted(maps.Keys(trees[prefix])) {
			entry := trees[prefix][name]
			relative := path.Join(prefix, name)
			full := directory.path + relative
			if !validPacketPath(full) {
				continue
			}
			switch entry.Mode {
			case "040000":
				if !expanded && inventoryDependencyPath(name) {
					if err := reader.append(Entry{Path: full, Mode: entry.Mode, BlobSHA: entry.SHA}); err != nil {
						return false, err
					}
				} else {
					queue = append(queue, relative)
				}
			case "100644", "100755":
				if err := reader.append(Entry{Path: full, Mode: entry.Mode, BlobSHA: entry.SHA, Size: *entry.Size}); err != nil {
					return false, err
				}
			}
		}
	}
	return true, nil
}

func (reader *inventoryReader) useNonrecursiveInventory() (bool, error) {
	// Descendants of this subtree fall back together. After four discarded
	// downloads, stop recursion for the call to bound wasted network traffic.
	reader.recursiveDiscards++
	reader.recursiveDisabled = reader.recursiveDiscards >= 4
	return false, nil
}

func retainInventoryTrees(trees map[string]map[string]packetTreeEntry, expanded bool, identityLength int) (map[string]map[string]packetTreeEntry, int, int) {
	retained := make(map[string]map[string]packetTreeEntry)
	count, size := 0, 0
	queue := []string{""}
	for len(queue) > 0 {
		directory := queue[0]
		queue = queue[1:]
		tree := trees[directory]
		retained[directory] = tree
		count += len(tree)
		for name, entry := range tree {
			size += len(strings.TrimLeft(entry.Mode, "0")) + 1 + len(name) + 1 + identityLength/2
			relative := path.Join(directory, name)
			if entry.Mode == "040000" && validPacketPath(relative) && (expanded || !inventoryDependencyPath(name)) {
				queue = append(queue, relative)
			}
		}
	}
	return retained, count, size
}

// A recursive API response is not itself a Git object. Reconstruct and verify
// every immediate-child tree, including excluded subtrees, against the parent
// entries and the independently frozen root. An incomplete prefix cannot pass.
func verifiedRecursiveTrees(root string, entries []packetTreeEntry, identityLength int) (map[string]map[string]packetTreeEntry, int, error) {
	groups := map[string][]packetTreeEntry{"": nil}
	identities := map[string]string{"": root}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.Path == "" || path.IsAbs(entry.Path) || path.Clean(entry.Path) != entry.Path ||
			entry.Path == "." || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") || seen[entry.Path] {
			return nil, 0, errPacketIntegrity
		}
		if strings.Count(entry.Path, "/") > 64 {
			return nil, 0, errPacketLimit
		}
		seen[entry.Path] = true
		parent, name := path.Split(entry.Path)
		parent = strings.TrimSuffix(parent, "/")
		if entry.Mode == "040000" {
			identities[entry.Path] = entry.SHA
			if _, found := groups[entry.Path]; !found {
				groups[entry.Path] = nil
			}
		}
		entry.Path = name
		groups[parent] = append(groups[parent], entry)
	}
	trees := make(map[string]map[string]packetTreeEntry, len(groups))
	size := 0
	for directory, children := range groups {
		expected, found := identities[directory]
		if !found || !validObjectID(expected) || len(expected) != identityLength {
			return nil, 0, errPacketIntegrity
		}
		tree, content, err := packetTreeContent(children, identityLength)
		if err != nil {
			return nil, 0, err
		}
		actual, err := packetGitID(packetTreeType, content, identityLength)
		if err != nil || actual != expected {
			return nil, 0, errPacketIntegrity
		}
		size += len(content)
		if size > MaxInventoryBytes {
			return nil, 0, errPacketLimit
		}
		trees[directory] = tree
	}
	return trees, size, nil
}
