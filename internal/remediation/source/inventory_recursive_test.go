package source

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func recursiveFixtureReply(t *testing.T, fixture *packetFixture, identity string) apiReply {
	t.Helper()
	var flatten func(string, string) []packetTreeEntry
	flatten = func(sha, prefix string) []packetTreeEntry {
		var reply packetTreeReply
		require.NoError(t, json.Unmarshal([]byte(fixture.replies[fixturePrefix+"/git/trees/"+sha].body), &reply))
		var children []packetTreeEntry
		require.NoError(t, json.Unmarshal(reply.Tree, &children))
		var all []packetTreeEntry
		for _, child := range children {
			child.Path = prefix + child.Path
			all = append(all, child)
			if child.Mode == "040000" {
				all = append(all, flatten(child.SHA, child.Path+"/")...)
			}
		}
		return all
	}
	return apiReply{http.StatusOK, packetJSON(t, map[string]any{
		"sha": identity, "tree": flatten(identity, ""), "truncated": false,
	})}
}

func fixtureSourceDirectoryIdentity(t *testing.T, fixture *packetFixture) string {
	t.Helper()
	var root packetTreeReply
	require.NoError(t, json.Unmarshal([]byte(fixture.replies[fixturePrefix+"/git/trees/"+fixture.target.Tree].body), &root))
	var entries []packetTreeEntry
	require.NoError(t, json.Unmarshal(root.Tree, &entries))
	for _, entry := range entries {
		if entry.Path == "src" {
			return entry.SHA
		}
	}
	t.Fatal("fixture source directory is absent")
	return ""
}

func TestInventoryRecursiveSubtreesReduceAnonymousRequests(t *testing.T) {
	for _, identityLength := range []int{40, 64} {
		t.Run(fmt.Sprint(identityLength), func(t *testing.T) {
			children := make([]packetNode, 0, 82)
			for index := range 80 {
				children = append(children, packetNode{name: fmt.Sprintf("component-%02d", index), mode: "040000",
					children: []packetNode{{name: "file.go", content: fmt.Appendf(nil, "package component%d\n", index)}}})
			}
			children = append(children,
				packetNode{name: "vendor", mode: "040000", children: []packetNode{{name: "private.go", content: []byte("not in inventory")}}},
				packetNode{name: "link", mode: "120000", content: []byte("/outside")},
			)
			fixture := newPacketFixture(t, identityLength,
				packetNode{name: "src", mode: "040000", children: children},
				packetNode{name: "vendor", mode: "040000", children: []packetNode{{name: "ignored.go", content: []byte("ignored")}}},
			)
			sha := fixtureSourceDirectoryIdentity(t, fixture)
			key := fixturePrefix + "/git/trees/" + sha + "?recursive=1"
			fixture.replies[key] = recursiveFixtureReply(t, fixture, sha)
			api := newAPIFixture(t, fixture.replies)
			entries, err := api.client.Inventory(t.Context(), fixture.target)
			require.NoError(t, err)
			require.Len(t, entries, 82)
			requests := api.observed()
			require.Len(t, requests, 4, "80 component directories must not consume 80 anonymous API requests")
			require.Equal(t, "recursive=1", requests[3].query)
			for _, request := range requests {
				require.Empty(t, request.header.Get("Authorization"))
				require.Empty(t, request.header.Get("Cookie"))
				require.NotContains(t, request.path, "/git/blobs/")
			}
			for _, entry := range entries {
				if entry.Path == "vendor" || entry.Path == "src/vendor" {
					require.Equal(t, "040000", entry.Mode)
					continue
				}
				require.NotContains(t, entry.Path, "vendor/")
				require.NotEqual(t, "src/link", entry.Path)
				require.Equal(t, fixture.files[entry.Path].BlobSHA, entry.BlobSHA)
			}
		})
	}
}

func TestInventoryRecursiveRejectsTamperedAndIncompleteMetadata(t *testing.T) {
	for _, mutation := range []string{"nested-sha", "missing-child", "orphan-parent", "duplicate", "wrong-root"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := newPacketFixture(t, 40, packetNode{name: "src", mode: "040000", children: []packetNode{
				{name: "nested", mode: "040000", children: []packetNode{{name: "file.go", content: []byte("package fixture\n")}}},
			}})
			sha := fixtureSourceDirectoryIdentity(t, fixture)
			reply := recursiveFixtureReply(t, fixture, sha)
			var wire packetTreeReply
			require.NoError(t, json.Unmarshal([]byte(reply.body), &wire))
			var entries []packetTreeEntry
			require.NoError(t, json.Unmarshal(wire.Tree, &entries))
			switch mutation {
			case "nested-sha":
				entries[1].SHA = strings.Repeat("a", 40)
			case "missing-child":
				entries = entries[:1]
			case "orphan-parent":
				entries[1].Path = "unreported/file.go"
			case "duplicate":
				entries = append(entries, entries[1])
			case "wrong-root":
				wire.SHA = strings.Repeat("b", 40)
			}
			wire.Tree, _ = json.Marshal(entries)
			reply.body = packetJSON(t, wire)
			fixture.replies[fixturePrefix+"/git/trees/"+sha+"?recursive=1"] = reply
			api := newAPIFixture(t, fixture.replies)
			entriesOut, err := api.client.Inventory(t.Context(), fixture.target)
			require.Error(t, err)
			require.Nil(t, entriesOut, "a corrupt recursive subtree must never produce a prefix")
			require.NotContains(t, err.Error(), "file.go")
		})
	}
}

func TestInventoryRecursiveTruncationFallsBackWithoutTrustingPrefix(t *testing.T) {
	fixture := newPacketFixture(t, 40, packetNode{name: "src", mode: "040000", children: []packetNode{
		{name: "nested", mode: "040000", children: []packetNode{{name: "file.go", content: []byte("package fixture\n")}}},
	}})
	sha := fixtureSourceDirectoryIdentity(t, fixture)
	fixture.replies[fixturePrefix+"/git/trees/"+sha+"?recursive=1"] = apiReply{http.StatusOK, packetJSON(t, map[string]any{
		"sha": sha, "tree": []any{map[string]any{"path": "forged"}}, "truncated": true,
	})}
	api := newAPIFixture(t, fixture.replies)
	entries, err := api.client.Inventory(t.Context(), fixture.target)
	require.NoError(t, err)
	require.Equal(t, []Entry{{
		Path: "src/nested/file.go", Mode: "100644", Size: int64(len(fixture.files["src/nested/file.go"].Content)),
		BlobSHA: fixture.files["src/nested/file.go"].BlobSHA,
	}}, entries)
	require.Len(t, api.observed(), 6, "discard the prefix and verify root/src/nested through bounded fallback")
}

func TestRecursiveInventoryRequiresAllExcludedTreeHashes(t *testing.T) {
	fixture := newPacketFixture(t, 40, packetNode{name: "src", mode: "040000", children: []packetNode{
		{name: "vendor", mode: "040000", children: []packetNode{{name: "dependency.go", content: []byte("package dep\n")}}},
	}})
	sha := fixtureSourceDirectoryIdentity(t, fixture)
	reply := recursiveFixtureReply(t, fixture, sha)
	reply.body = strings.Replace(reply.body, `"vendor/dependency.go"`, `"vendor/tampered.go"`, 1)
	fixture.replies[fixturePrefix+"/git/trees/"+sha+"?recursive=1"] = reply
	api := newAPIFixture(t, fixture.replies)
	entries, err := api.client.Inventory(t.Context(), fixture.target)
	require.Error(t, err)
	require.Nil(t, entries)
}
