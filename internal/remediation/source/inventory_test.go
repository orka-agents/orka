package source

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var _ func(context.Context, Target) ([]Entry, error) = Client{}.Inventory

func TestInventoryExactPublicCommitWithoutContents(t *testing.T) {
	for _, identityLength := range []int{40, 64} {
		t.Run(fmt.Sprint(identityLength), func(t *testing.T) {
			fixture := newPacketFixture(t, identityLength,
				packetNode{name: "go.mod", content: []byte("module example.invalid/synthetic\n")},
				packetNode{name: "src", mode: "040000", children: []packetNode{
					{name: "check.go", content: []byte("package check\n")},
				}},
				packetNode{name: "vendor", mode: "040000", children: []packetNode{
					{name: "dependency.go", content: []byte("package dependency\n")},
				}},
				packetNode{name: ".git", mode: "040000", children: []packetNode{
					{name: "config", content: []byte("synthetic ignored metadata\n")},
				}},
				packetNode{name: "symlink", mode: "120000", content: []byte("/outside\n")},
				packetNode{name: "gitlink", mode: "160000", opaqueID: strings.Repeat("f", identityLength)},
			)
			api := newAPIFixture(t, fixture.replies)
			entries, err := api.client.Inventory(t.Context(), fixture.target)
			require.NoError(t, err)
			require.Len(t, entries, 3)
			require.Equal(t, Entry{
				Path: "go.mod", Mode: "100644", Size: int64(len(fixture.files["go.mod"].Content)),
				BlobSHA: fixture.files["go.mod"].BlobSHA,
			}, entries[0])
			require.Equal(t, fixture.files["src/check.go"].BlobSHA, entries[1].BlobSHA)
			require.Equal(t, "src/check.go", entries[1].Path)
			require.Equal(t, "vendor", entries[2].Path)
			require.Equal(t, "040000", entries[2].Mode)
			requests := api.observed()
			require.Len(t, requests, 4, "public metadata, exact commit, root and src trees only")
			require.Equal(t, fixturePrefix+"/commits/"+fixture.target.Commit, requests[1].path)
			for index, request := range requests {
				require.Equal(t, http.MethodGet, request.method)
				if index == 3 {
					require.Equal(t, "recursive=1", request.query)
				} else {
					require.Empty(t, request.query)
				}
				require.Empty(t, request.header.Get("Authorization"))
				require.Empty(t, request.header.Get("Cookie"))
				require.NotContains(t, request.path, "/git/blobs/")
			}
			encoded := packetJSON(t, entries)
			require.NotContains(t, encoded, "package")
			require.NotContains(t, encoded, "synthetic ignored metadata")
			again, err := api.client.Inventory(t.Context(), fixture.target)
			require.NoError(t, err)
			require.Equal(t, entries, again)
			require.Len(t, api.observed(), 8, "authorization and trees are never cached across calls")

			expanded, err := api.client.InventoryPaths(t.Context(), fixture.target, []string{"vendor"})
			require.NoError(t, err)
			require.Equal(t, []Entry{{
				Path: "vendor/dependency.go", Mode: "100644", Size: int64(len(fixture.files["vendor/dependency.go"].Content)),
				BlobSHA: fixture.files["vendor/dependency.go"].BlobSHA,
			}}, expanded)
			require.Len(t, api.observed(), 12, "expansion revalidates metadata, exact commit and both ancestor trees")
		})
	}
}

func TestInventoryRejectsUnboundOrIncompleteTrees(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*packetFixture)
	}{
		{"private", func(fixture *packetFixture) {
			fixture.replies[fixturePrefix] = apiReply{http.StatusOK, strings.Replace(repositoryJSON(), `"private":false`, `"private":true`, 1)}
		}},
		{"wrong-commit", func(fixture *packetFixture) {
			fixture.replies[fixturePrefix+"/commits/"+fixture.target.Commit] = apiReply{
				http.StatusOK, commitJSON(strings.Repeat("e", 40), fixture.target.Tree),
			}
		}},
		{"wrong-tree", func(fixture *packetFixture) {
			fixture.replies[fixturePrefix+"/commits/"+fixture.target.Commit] = apiReply{
				http.StatusOK, commitJSON(fixture.target.Commit, strings.Repeat("f", 40)),
			}
		}},
		{"truncated", func(fixture *packetFixture) {
			key := fixturePrefix + "/git/trees/" + fixture.target.Tree
			reply := fixture.replies[key]
			reply.body = strings.Replace(reply.body, `"truncated":false`, `"truncated":true`, 1)
			fixture.replies[key] = reply
		}},
		{"forged-entry", func(fixture *packetFixture) {
			key := fixturePrefix + "/git/trees/" + fixture.target.Tree
			reply := fixture.replies[key]
			reply.body = strings.Replace(reply.body, `"path":"safe.go"`, `"path":"forged.go"`, 1)
			fixture.replies[key] = reply
		}},
		{"oversized-response", func(fixture *packetFixture) {
			key := fixturePrefix + "/git/trees/" + fixture.target.Tree
			reply := fixture.replies[key]
			reply.body = strings.Repeat(" ", apiMaxReplyBytes) + reply.body
			fixture.replies[key] = reply
		}},
		{"duplicate-sha", func(fixture *packetFixture) {
			key := fixturePrefix + "/git/trees/" + fixture.target.Tree
			reply := fixture.replies[key]
			reply.body = `{"SHA":"` + fixture.target.Tree + `",` + reply.body[1:]
			fixture.replies[key] = reply
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPacketFixture(t, 40, packetNode{name: "safe.go", content: []byte("package synthetic\n")})
			test.mutate(fixture)
			api := newAPIFixture(t, fixture.replies)
			entries, err := api.client.Inventory(t.Context(), fixture.target)
			require.Error(t, err)
			require.Nil(t, entries, "a rejected inventory must never return a partial prefix")
			require.NotContains(t, err.Error(), "safe.go")
			require.NotContains(t, err.Error(), "forged.go")
			require.NotContains(t, err.Error(), "package synthetic")
		})
	}
}

func TestInventoryBoundedTreeVisitsEvenWithIdenticalSubtrees(t *testing.T) {
	for _, directories := range []int{127, 128} {
		t.Run(fmt.Sprint(directories), func(t *testing.T) {
			nodes := make([]packetNode, 0, directories+1)
			nodes = append(nodes, packetNode{name: "file.go", content: []byte("package synthetic\n")})
			for index := range directories {
				nodes = append(nodes, packetNode{name: fmt.Sprintf("dir-%03d", index), mode: "040000"})
			}
			fixture := newPacketFixture(t, 40, nodes...)
			api := newAPIFixture(t, fixture.replies)
			entries, err := api.client.Inventory(t.Context(), fixture.target)
			if directories == 127 {
				require.NoError(t, err)
				require.Len(t, entries, 1)
			} else {
				require.ErrorIs(t, err, errPacketLimit)
				require.Nil(t, entries)
			}
			require.LessOrEqual(t, len(api.observed()), 4, "identical trees are cached only within this bounded traversal")
		})
	}
}

func TestInventoryOutputLimitsMeasuredOnEncodedMetadata(t *testing.T) {
	entry := Entry{Path: "safe.go", Mode: "100644", Size: 1, BlobSHA: strings.Repeat("a", 40)}
	encoded, err := json.Marshal(entry)
	require.NoError(t, err)
	for _, excess := range []int{0, 1} {
		reader := inventoryReader{bytes: MaxInventoryBytes - len(encoded) - 1 + excess}
		err := reader.append(entry)
		if excess == 0 {
			require.NoError(t, err)
			require.Equal(t, MaxInventoryBytes, reader.bytes)
		} else {
			require.ErrorIs(t, err, errPacketLimit)
			require.Empty(t, reader.entries)
		}
	}
	reader := inventoryReader{entries: make([]Entry, MaxInventoryEntries)}
	require.ErrorIs(t, reader.append(entry), errPacketLimit)
}

func TestInventoryCancellationAndUnsafeExpansionDoNotIssueRequests(t *testing.T) {
	fixture := newPacketFixture(t, 40, packetNode{name: "file.go", content: []byte("package synthetic\n")})
	api := newAPIFixture(t, fixture.replies)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	entries, err := api.client.Inventory(ctx, fixture.target)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, entries)
	for _, directories := range [][]string{nil, {".git"}, {"vendor/../src"}, {"/vendor"}, {"src"}, {"vendor", "vendor"}} {
		entries, err := api.client.InventoryPaths(t.Context(), fixture.target, directories)
		require.Error(t, err)
		require.Nil(t, entries)
	}
	require.Empty(t, api.observed())
}
