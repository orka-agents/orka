package source

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInventoryFallsBackForOversizedRecursiveReplies(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			fixture := newPacketFixture(t, 40,
				packetNode{name: "src", mode: "040000", children: []packetNode{
					{name: "file.go", content: []byte("package fixture\n")},
					{name: "node_modules", mode: "040000", children: []packetNode{{name: "module.js", content: []byte("dependency")}}},
				}},
				packetNode{name: "tests", mode: "040000", children: manyInventoryDirectories()},
			)
			sha := fixtureSourceDirectoryIdentity(t, fixture)
			for _, key := range slices.Sorted(maps.Keys(fixture.replies)) {
				if strings.Contains(key, "/git/trees/") {
					id := strings.TrimPrefix(key, fixturePrefix+"/git/trees/")
					fixture.replies[key+"?recursive=1"] = recursiveFixtureReply(t, fixture, id)
				}
			}
			entries := make([]map[string]any, 25000)
			for index := range entries {
				entries[index] = map[string]any{
					"path": fmt.Sprintf("node_modules/dependency-%05d/index.js", index),
					"type": "blob", "mode": "100644", "size": 1, "sha": strings.Repeat("a", 40),
					"url": "https://api.github.com/repos/source-fixtures/project/git/blobs/" + strings.Repeat("a", 40),
				}
			}
			body := packetJSON(t, map[string]any{"sha": sha, "tree": entries, "truncated": truncated})
			require.Greater(t, len(body), MaxInventoryBytes)
			fixture.replies[fixturePrefix+"/git/trees/"+sha+"?recursive=1"] = apiReply{http.StatusOK, body}
			api := newAPIFixture(t, fixture.replies)
			inventory, err := api.client.Inventory(t.Context(), fixture.target)
			require.NoError(t, err)
			require.Len(t, inventory, 130)
			require.Equal(t, "src/file.go", inventory[0].Path)
			require.Equal(t, "src/node_modules", inventory[1].Path)
			require.Equal(t, "040000", inventory[1].Mode)
			require.Equal(t, "tests/part-000/check.go", inventory[2].Path)
			requests := api.observed()
			require.Len(t, requests, 6)
			recursive := 0
			for _, request := range requests {
				if request.query != "" {
					require.Equal(t, "recursive=1", request.query)
					recursive++
				}
				require.Empty(t, request.header.Get("Authorization"))
			}
			require.Equal(t, 2, recursive, "fallback descendants must not prevent an unrelated sibling from being batched")
		})
	}
}

func TestInventoryDeferredDependenciesDoNotExhaustSiblingBudget(t *testing.T) {
	dependency := make([]packetNode, 17000)
	for index := range dependency {
		dependency[index] = packetNode{name: fmt.Sprintf("file-%05d.go", index), content: []byte("package dependency\n")}
	}
	fixture := newPacketFixture(t, 40,
		packetNode{name: "src", mode: "040000", children: []packetNode{
			{name: "file.go", content: []byte("package first\n")},
			{name: "vendor", mode: "040000", children: dependency},
		}},
		packetNode{name: "tests", mode: "040000", children: []packetNode{
			{name: "file.go", content: []byte("package second\n")},
			{name: "vendor", mode: "040000", children: dependency},
		}},
	)
	rootReply := fixture.replies[fixturePrefix+"/git/trees/"+fixture.target.Tree]
	require.NotEmpty(t, rootReply.body)
	for _, key := range slices.Sorted(maps.Keys(fixture.replies)) {
		if strings.Contains(key, "/git/trees/") {
			sha := strings.TrimPrefix(key, fixturePrefix+"/git/trees/")
			fixture.replies[key+"?recursive=1"] = recursiveFixtureReply(t, fixture, sha)
		}
	}
	api := newAPIFixture(t, fixture.replies)
	inventory, err := api.client.Inventory(t.Context(), fixture.target)
	require.NoError(t, err)
	require.Len(t, inventory, 4, "nested vendor metadata cannot exclude otherwise eligible public source")
	require.Equal(t, "src/file.go", inventory[0].Path)
	require.Equal(t, "src/vendor", inventory[1].Path)
	require.Equal(t, "tests/file.go", inventory[2].Path)
	require.Equal(t, "tests/vendor", inventory[3].Path)
	require.Len(t, api.observed(), 5, "verified but deferred vendor contents must not consume the traversable tree budget")
}

func manyInventoryDirectories() []packetNode {
	nodes := make([]packetNode, 128)
	for index := range nodes {
		nodes[index] = packetNode{name: fmt.Sprintf("part-%03d", index), mode: "040000", children: []packetNode{
			{name: "check.go", content: fmt.Appendf(nil, "package test%d\n", index)},
		}}
	}
	return nodes
}

func TestInventoryFallsBackForRecursiveOnlyDepthLimit(t *testing.T) {
	child := packetNode{name: "file.go", content: []byte("package dependency")}
	for range 65 {
		child = packetNode{name: "nested", mode: "040000", children: []packetNode{child}}
	}
	fixture := newPacketFixture(t, 40, packetNode{name: "src", mode: "040000", children: []packetNode{
		{name: "vendor", mode: "040000", children: []packetNode{child}},
	}})
	sha := fixtureSourceDirectoryIdentity(t, fixture)
	fixture.replies[fixturePrefix+"/git/trees/"+sha+"?recursive=1"] = recursiveFixtureReply(t, fixture, sha)
	api := newAPIFixture(t, fixture.replies)
	inventory, err := api.client.Inventory(t.Context(), fixture.target)
	require.NoError(t, err)
	require.Len(t, inventory, 1)
	require.Equal(t, "src/vendor", inventory[0].Path)
	require.Equal(t, "040000", inventory[0].Mode)
	require.Len(t, api.observed(), 5)
}
