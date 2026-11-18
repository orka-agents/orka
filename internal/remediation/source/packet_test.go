package source

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var _ func(context.Context, Target, []string) (Packet, error) = Client{}.Packet
var _ func([]byte) (Packet, error) = DecodePacket

type packetNode struct {
	name     string
	mode     string
	content  []byte
	children []packetNode
	opaqueID string
	size     int64
}

type packetFixture struct {
	target  Target
	replies map[string]apiReply
	files   map[string]PacketFile
}

func fixtureObjectID(kind string, content []byte, identityLength int) string {
	object := fmt.Appendf(nil, "%s %d\x00", kind, len(content))
	object = append(object, content...)
	if identityLength == 40 {
		digest := sha1.Sum(object)
		return hex.EncodeToString(digest[:])
	}
	digest := sha256.Sum256(object)
	return hex.EncodeToString(digest[:])
}

func fixtureContentID(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func packetJSON(t *testing.T, value any) string {
	t.Helper()
	content, err := json.Marshal(value)
	require.NoError(t, err)
	return string(content)
}

func fixturePacketTree(t *testing.T, fixture *packetFixture, parent string, nodes []packetNode, identityLength int) string {
	t.Helper()
	type objectEntry struct {
		name, mode, identity string
	}
	rawEntries := make([]objectEntry, 0, len(nodes))
	replies := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		mode, kind := node.mode, "blob"
		if mode == "" {
			mode = "100644"
		}
		name := parent + node.name
		identity, size := node.opaqueID, int64(len(node.content))
		switch mode {
		case "040000":
			kind = "tree"
			identity = fixturePacketTree(t, fixture, name+"/", node.children, identityLength)
		case "160000":
			kind = "commit"
		default:
			if identity == "" {
				identity = fixtureObjectID("blob", node.content, identityLength)
				fixture.replies[fixturePrefix+"/git/blobs/"+identity] = apiReply{http.StatusOK, packetJSON(t, map[string]any{
					"sha": identity, "encoding": "base64", "size": size,
					"content": base64.StdEncoding.EncodeToString(node.content),
				})}
				fixture.files[name] = PacketFile{
					Path: name, Content: string(node.content), SHA256: fixtureContentID(node.content), BlobSHA: identity,
				}
			} else {
				size = node.size
			}
		}
		reply := map[string]any{"path": node.name, "mode": mode, "type": kind, "sha": identity}
		if kind == "blob" {
			reply["size"] = size
		}
		replies = append(replies, reply)
		rawEntries = append(rawEntries, objectEntry{node.name, strings.TrimLeft(mode, "0"), identity})
	}
	slices.SortFunc(rawEntries, func(first, second objectEntry) int {
		firstName, secondName := first.name, second.name
		if first.mode == "40000" {
			firstName += "/"
		}
		if second.mode == "40000" {
			secondName += "/"
		}
		return strings.Compare(firstName, secondName)
	})
	var tree []byte
	for _, entry := range rawEntries {
		tree = fmt.Appendf(tree, "%s %s\x00", entry.mode, entry.name)
		identity, err := hex.DecodeString(entry.identity)
		require.NoError(t, err)
		tree = append(tree, identity...)
	}
	identity := fixtureObjectID("tree", tree, identityLength)
	// Preserve the caller's JSON order, which need not be Git's object order.
	fixture.replies[fixturePrefix+"/git/trees/"+identity] = apiReply{http.StatusOK, packetJSON(t, map[string]any{
		"sha": identity, "tree": replies, "truncated": false,
	})}
	return identity
}

func newPacketFixture(t *testing.T, identityLength int, nodes ...packetNode) *packetFixture {
	t.Helper()
	fixture := &packetFixture{replies: make(map[string]apiReply), files: make(map[string]PacketFile)}
	tree := fixturePacketTree(t, fixture, "", nodes, identityLength)
	commit := fixtureObjectID("commit", []byte("tree "+tree+"\nauthor Source Fixture <fixture@example.invalid> 1 +0000\n"+
		"committer Source Fixture <fixture@example.invalid> 1 +0000\n\nSynthetic fixture\n"), identityLength)
	fixture.target = Target{
		Repository: Repository{URL: fixtureURL, Owner: "source-fixtures", Name: "project", DefaultBranch: "main"},
		Ref:        "synthetic-release", Commit: commit, Tree: tree,
	}
	fixture.replies[fixturePrefix] = apiReply{http.StatusOK, repositoryJSON()}
	fixture.replies[fixturePrefix+"/commits/"+commit] = apiReply{http.StatusOK, commitJSON(commit, tree)}
	return fixture
}

func (fixture *packetFixture) packet(names ...string) Packet {
	packet := Packet{Version: 1, Target: fixture.target, Files: make([]PacketFile, 0, len(names))}
	for _, name := range names {
		packet.Files = append(packet.Files, fixture.files[name])
	}
	return packet
}

func TestPacketPinnedPublicSelectionWithoutMaterializingVendoredSource(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic-only")
	t.Setenv("GITHUB_TOKEN", "synthetic-only")
	t.Setenv("HTTPS_PROXY", (&url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:1",
		User:   url.UserPassword("SYNTHETIC_USER_"+rand.Text(), "SYNTHETIC_PASSWORD_"+rand.Text()),
	}).String())
	for _, identityLength := range []int{40, 64} {
		t.Run(fmt.Sprint(identityLength), func(t *testing.T) {
			fixture := newPacketFixture(t, identityLength,
				packetNode{name: "pkg", mode: "040000", children: []packetNode{
					{name: "check.go", content: []byte("package check\n")},
					{name: "helper.go", content: []byte("package check\n// synthetic helper\n")},
				}},
				packetNode{name: "go.mod", content: []byte("module example.invalid/synthetic\n")},
				packetNode{name: "ignored-link", mode: "120000", content: []byte("/outside\n")},
				packetNode{name: "ignored-submodule", mode: "160000", opaqueID: strings.Repeat("f", identityLength)},
				packetNode{name: "vendor", mode: "040000", children: []packetNode{
					{name: "oversized.bin", opaqueID: strings.Repeat("e", identityLength), size: 2 << 30},
				}},
			)
			api := newAPIFixture(t, fixture.replies)
			jar, err := cookiejar.New(nil)
			require.NoError(t, err)
			endpoint, err := url.Parse(api.server.URL)
			require.NoError(t, err)
			jar.SetCookies(endpoint, []*http.Cookie{{Name: "synthetic-session", Value: "fixture"}})
			api.client.HTTPClient.Jar = jar
			selected := []string{"pkg/check.go", "go.mod", "pkg/helper.go"}
			packet, err := api.client.Packet(t.Context(), fixture.target, selected)
			require.NoError(t, err)
			require.Equal(t, fixture.packet(selected...), packet)
			encoded := []byte(packetJSON(t, packet))
			decoded, err := DecodePacket(encoded)
			require.NoError(t, err)
			require.Equal(t, packet, decoded)
			requests := api.observed()
			require.Len(t, requests, 7, "only metadata, pinned commit, two trees and three selected blobs")
			require.Equal(t, fixturePrefix, requests[0].path)
			require.Equal(t, fixturePrefix+"/commits/"+fixture.target.Commit, requests[1].path)
			require.Equal(t, fixturePrefix+"/git/trees/"+fixture.target.Tree, requests[2].path)
			for _, request := range requests {
				require.Equal(t, http.MethodGet, request.method)
				require.Empty(t, request.query, "no recursive tree or ref-dependent contents query")
				require.Empty(t, request.header.Get("Authorization"))
				require.Empty(t, request.header.Get("Proxy-Authorization"))
				require.Empty(t, request.header.Get("Cookie"))
				require.NotContains(t, request.path, "/contents/")
				require.NotContains(t, request.path, strings.Repeat("e", identityLength))
			}
			_, err = api.client.Packet(t.Context(), fixture.target, selected)
			require.NoError(t, err)
			require.Len(t, api.observed(), 14, "tree/public metadata caches must not survive the request")
		})
	}
}

func TestPacketSelectionValidationPrecedesAllHTTP(t *testing.T) {
	fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("synthetic\n")})
	api := newAPIFixture(t, nil)
	selections := make([][]string, 0, 40)
	selections = append(selections, [][]string{
		nil, {}, {""}, {"."}, {".."}, {"../file"}, {"/file"}, {"a/../file"}, {"a/./file"},
		{"a//file"}, {"file/"}, {".git/config"}, {".GIT/config"}, {".git./config"}, {".GIT./config"}, {"a\\file"}, {"C:/file"},
		{"file\x00"}, {"a\nb"}, {"file?"}, {"file#"}, {"file%2fother"}, {"https://github.com/a/b"},
		{"AUX.txt"}, {"NUL"}, {"file."}, {"file "}, {" file"}, {"caf\u00e9.go"}, {"cafe\u0301.go"}, {string([]byte{255})},
		{"a", "a"}, {"a", "A"}, {"Dir/a", "dir/b"}, {"a", "a/file"}, {"a/file", "a"},
		{strings.Repeat("a/", maxPathDepth) + "file"}, {strings.Repeat("a", 256)}, {strings.Repeat("x", maxPathBytes+1)},
		{"ghp_" + strings.Repeat("x", 25)},
	}...)
	tooMany := make([]string, packetMaxFiles+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("f%d", index)
	}
	selections = append(selections, tooMany)
	for _, selected := range selections {
		packet, err := api.client.Packet(t.Context(), fixture.target, selected)
		require.Error(t, err)
		require.Empty(t, packet)
	}
	for _, mutate := range []func(*Target){
		func(target *Target) { *target = Target{} },
		func(target *Target) { target.Commit = "main" },
		func(target *Target) { target.Tree = strings.Repeat("A", 40) },
		func(target *Target) { target.Tree = strings.Repeat("0", 40) },
		func(target *Target) { target.Repository.Owner = "other" },
		func(target *Target) { target.Repository.Name = "other" },
		func(target *Target) {
			target.Repository.URL = (&url.URL{
				Scheme: "https",
				Host:   "github.com",
				Path:   "/source-fixtures/project",
				User:   url.User("SYNTHETIC_USER_" + rand.Text()),
			}).String()
		},
		func(target *Target) { target.Repository.URL += ".git" },
		func(target *Target) { target.Repository.URL += "?token=synthetic" },
		func(target *Target) { target.Ref = "" },
		func(target *Target) { target.Ref = strings.Repeat("f", 40) },
		func(target *Target) { target.Ref = "ghp_" + strings.Repeat("x", 25) },
	} {
		target := fixture.target
		mutate(&target)
		packet, err := api.client.Packet(t.Context(), target, []string{"file"})
		require.Error(t, err)
		require.Empty(t, packet)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := api.client.Packet(ctx, fixture.target, []string{"file"})
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, api.observed())
}

func TestPacketRevalidatesPublicCommitTreeBeforeReadingSource(t *testing.T) {
	for _, failure := range []string{"private", "missing-public", "unknown-commit", "wrong-commit", "wrong-tree", "forged-target-tree"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("synthetic\n")})
			commitPath := fixturePrefix + "/commits/" + fixture.target.Commit
			switch failure {
			case "private":
				fixture.replies[fixturePrefix] = apiReply{http.StatusOK, strings.Replace(repositoryJSON(), `"private":false`, `"private":true`, 1)}
			case "missing-public":
				fixture.replies[fixturePrefix] = apiReply{http.StatusOK, strings.Replace(repositoryJSON(), `"visibility":"public",`, "", 1)}
			case "unknown-commit":
				fixture.replies[commitPath] = apiReply{http.StatusNotFound, "synthetic must not escape"}
			case "wrong-commit":
				fixture.replies[commitPath] = apiReply{http.StatusOK, commitJSON(fixtureCommit, fixture.target.Tree)}
			case "wrong-tree":
				fixture.replies[commitPath] = apiReply{http.StatusOK, commitJSON(fixture.target.Commit, fixtureTree)}
			case "forged-target-tree":
				fixture.target.Tree = fixtureTree
			}
			api := newAPIFixture(t, fixture.replies)
			packet, err := api.client.Packet(t.Context(), fixture.target, []string{"file"})
			require.Error(t, err)
			require.Empty(t, packet)
			require.NotContains(t, err.Error(), "synthetic must not escape")
			for _, request := range api.observed() {
				require.NotContains(t, request.path, "/git/")
			}
		})
	}
}

func TestPacketTreeModesAndExactPathSelection(t *testing.T) {
	fixture := newPacketFixture(t, 40,
		packetNode{name: "regular", content: []byte("regular\n")},
		packetNode{name: "executable", mode: "100755", content: []byte("#!/bin/sh\nexit 0\n")},
		packetNode{name: "link", mode: "120000", content: []byte("regular")},
		packetNode{name: "submodule", mode: "160000", opaqueID: fixtureCommit},
		packetNode{name: "directory", mode: "040000", children: []packetNode{{name: "file", content: []byte("nested\n")}}},
	)
	for _, name := range []string{"missing", "Regular", "link", "link/file", "submodule", "submodule/file", "directory", "regular/file"} {
		api := newAPIFixture(t, fixture.replies)
		packet, err := api.client.Packet(t.Context(), fixture.target, []string{name})
		require.Error(t, err)
		require.Empty(t, packet)
		for _, request := range api.observed() {
			require.NotContains(t, request.path, "/git/blobs/", "non-regular or missing path must be rejected before blob retrieval")
		}
	}
	api := newAPIFixture(t, fixture.replies)
	packet, err := api.client.Packet(t.Context(), fixture.target, []string{"executable"})
	require.NoError(t, err)
	require.Equal(t, fixture.packet("executable"), packet)
}

func TestPacketRejectsHostileTreeReplies(t *testing.T) {
	for _, failure := range []string{
		"wrong-sha", "missing-sha", "truncated", "missing-truncated", "null-truncated", "missing-tree", "null-tree",
		"wrong-entry-sha", "wrong-mode", "wrong-type", "path-traversal", "non-immediate-path", "duplicate-path", "duplicate-key",
		"oversized", "error-response",
	} {
		t.Run(failure, func(t *testing.T) {
			fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("synthetic\n")})
			treePath := fixturePrefix + "/git/trees/" + fixture.target.Tree
			var reply map[string]any
			require.NoError(t, json.Unmarshal([]byte(fixture.replies[treePath].body), &reply))
			entries := reply["tree"].([]any)
			entry := entries[0].(map[string]any)
			switch failure {
			case "wrong-sha":
				reply["sha"] = fixtureTree
			case "missing-sha":
				delete(reply, "sha")
			case "truncated":
				reply["truncated"] = true
			case "missing-truncated":
				delete(reply, "truncated")
			case "null-truncated":
				reply["truncated"] = nil
			case "missing-tree":
				delete(reply, "tree")
			case "null-tree":
				reply["tree"] = nil
			case "wrong-entry-sha":
				entry["sha"] = fixtureTree
			case "wrong-mode":
				entry["mode"] = "100755"
			case "wrong-type":
				entry["type"] = "commit"
			case "path-traversal":
				entry["path"] = ".."
			case "non-immediate-path":
				entry["path"] = "a/file"
			case "duplicate-path":
				reply["tree"] = append(entries, entry)
			}
			body, status := packetJSON(t, reply), http.StatusOK
			switch failure {
			case "duplicate-key":
				body = strings.Replace(body, `"truncated":false`, `"truncated":true,"truncated":false`, 1)
			case "oversized":
				body = strings.Repeat(" ", apiMaxReplyBytes+1) + body
			case "error-response":
				status, body = http.StatusTooManyRequests, "synthetic reply must not escape"
			}
			fixture.replies[treePath] = apiReply{status, body}
			api := newAPIFixture(t, fixture.replies)
			packet, err := api.client.Packet(t.Context(), fixture.target, []string{"file"})
			require.Error(t, err)
			require.Empty(t, packet)
			require.NotContains(t, err.Error(), "synthetic reply")
			require.Len(t, api.observed(), 3)
		})
	}
}

func TestPacketRejectsHostileBlobReplies(t *testing.T) {
	for _, failure := range []string{
		"wrong-sha", "missing-sha", "wrong-content", "wrong-encoding", "invalid-base64", "missing-content", "null-content",
		"short-size", "long-size", "missing-size", "null-size", "oversized-size", "wrong-tree-size", "oversized-body", "noncanonical-padding",
	} {
		t.Run(failure, func(t *testing.T) {
			fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("synthetic\n")})
			blobPath := fixturePrefix + "/git/blobs/" + fixture.files["file"].BlobSHA
			var reply map[string]any
			require.NoError(t, json.Unmarshal([]byte(fixture.replies[blobPath].body), &reply))
			switch failure {
			case "wrong-sha":
				reply["sha"] = fixtureTree
			case "missing-sha":
				delete(reply, "sha")
			case "wrong-content":
				reply["content"] = base64.StdEncoding.EncodeToString([]byte("different\n"))
			case "wrong-encoding":
				reply["encoding"] = "utf-8"
			case "invalid-base64":
				reply["content"] = "!!!"
			case "noncanonical-padding":
				reply["content"] = "c3ludGhldGljCh=="
			case "missing-content":
				delete(reply, "content")
			case "null-content":
				reply["content"] = nil
			case "short-size":
				reply["size"] = 1
			case "long-size":
				reply["size"] = 100
			case "missing-size":
				delete(reply, "size")
			case "null-size":
				reply["size"] = nil
			case "oversized-size":
				reply["size"] = packetMaxTextBytes + 1
			case "wrong-tree-size":
				treePath := fixturePrefix + "/git/trees/" + fixture.target.Tree
				fixture.replies[treePath] = apiReply{http.StatusOK, strings.Replace(fixture.replies[treePath].body, `"size":10`, `"size":11`, 1)}
			}
			body := packetJSON(t, reply)
			if failure == "oversized-body" {
				body += strings.Repeat(" ", apiMaxReplyBytes+1)
			}
			fixture.replies[blobPath] = apiReply{http.StatusOK, body}
			api := newAPIFixture(t, fixture.replies)
			packet, err := api.client.Packet(t.Context(), fixture.target, []string{"file"})
			require.Error(t, err)
			require.Empty(t, packet)
		})
	}
}

func TestPacketTextAndByteLimits(t *testing.T) {
	for _, size := range []int{packetMaxTextBytes - 1, packetMaxTextBytes, packetMaxTextBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			content := append([]byte(strings.Repeat("x", size-1)), '\n')
			fixture := newPacketFixture(t, 40, packetNode{name: "file", content: content})
			api := newAPIFixture(t, fixture.replies)
			packet, err := api.client.Packet(t.Context(), fixture.target, []string{"file"})
			if size > packetMaxTextBytes {
				require.ErrorIs(t, err, errPacketLimit)
				require.Empty(t, packet)
				require.Len(t, api.observed(), 3, "reject declared oversized blobs before downloading")
			} else {
				require.NoError(t, err)
				require.Len(t, packet.Files[0].Content, size)
			}
		})
	}
	for _, content := range [][]byte{nil, []byte("no newline"), []byte("binary\x00\n"), {0xff, '\n'}, []byte("\x1b[1mtext\n")} {
		fixture := newPacketFixture(t, 40, packetNode{name: "file", content: content})
		api := newAPIFixture(t, fixture.replies)
		packet, err := api.client.Packet(t.Context(), fixture.target, []string{"file"})
		require.Error(t, err)
		require.Empty(t, packet)
	}
	content := []byte("// caf\u00e9 \u4fee\u590d\r\n\tunicode text\n")
	fixture := newPacketFixture(t, 40, packetNode{name: "file", content: content})
	blobPath := fixturePrefix + "/git/blobs/" + fixture.files["file"].BlobSHA
	wrapped := base64.StdEncoding.EncodeToString(content)
	wrapped = wrapped[:8] + "\r\n" + wrapped[8:] + "\n"
	fixture.replies[blobPath] = apiReply{http.StatusOK, packetJSON(t, map[string]any{
		"sha": fixture.files["file"].BlobSHA, "size": len(content), "encoding": "base64", "content": wrapped,
	})}
	api := newAPIFixture(t, fixture.replies)
	packet, err := api.client.Packet(t.Context(), fixture.target, []string{"file"})
	require.NoError(t, err)
	require.Equal(t, content, []byte(packet.Files[0].Content))
}

func TestPacketAggregateLimitsCountRepeatedContentAndUTF8Bytes(t *testing.T) {
	for _, extra := range []int{0, 1} {
		nodes := make([]packetNode, 0, packetMaxFiles)
		names := make([]string, 0, packetMaxFiles)
		for index := range packetMaxFiles {
			content := []byte(strings.Repeat("\u00e9", packetMaxTextBytes/packetMaxFiles/2-1) + "x\n")
			if index == 0 && extra != 0 {
				content = append([]byte("x"), content...)
			}
			name := fmt.Sprintf("f%d", index)
			nodes, names = append(nodes, packetNode{name: name, content: content}), append(names, name)
		}
		fixture := newPacketFixture(t, 40, nodes...)
		api := newAPIFixture(t, fixture.replies)
		packet, err := api.client.Packet(t.Context(), fixture.target, names)
		if extra != 0 {
			require.ErrorIs(t, err, errPacketLimit)
			require.Empty(t, packet)
			require.Len(t, api.observed(), 3)
		} else {
			require.NoError(t, err)
			require.Len(t, packet.Files, packetMaxFiles)
			total := 0
			for _, file := range packet.Files {
				total += len(file.Content)
			}
			require.Equal(t, packetMaxTextBytes, total)
		}
	}
}

func TestPacketCredentialScreeningUsesSharedSourceRules(t *testing.T) {
	for _, content := range []string{
		"password = \"synthetic-only\"\n", "{\"api_key\":\"synthetic-only\"}\n",
		"Authorization: Bearer synthetic-only\n", "-----BEGIN " + "PRIVATE KEY-----\nsynthetic\n",
		"ghp_" + strings.Repeat("x", 30) + "\n",
	} {
		fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte(content)})
		api := newAPIFixture(t, fixture.replies)
		packet, err := api.client.Packet(t.Context(), fixture.target, []string{"file"})
		require.ErrorIs(t, err, errPacketSecret)
		require.Empty(t, packet)
		require.NotContains(t, err.Error(), "synthetic-only")
		_, err = DecodePacket([]byte(packetJSON(t, fixture.packet("file"))))
		require.ErrorIs(t, err, errPacketSecret)
	}
	for _, content := range []string{
		"password := os.Getenv(\"PASSWORD\")\n", "api_key = ${API_KEY}\n",
		"{\"properties\":{\"password\":{\"type\":\"string\"}}}\n", "password = \"\"\n",
	} {
		fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte(content)})
		api := newAPIFixture(t, fixture.replies)
		packet, err := api.client.Packet(t.Context(), fixture.target, []string{"file"})
		require.NoError(t, err)
		require.Equal(t, fixture.packet("file"), packet)
	}
}

func TestPacketTreeBudgetsAndRequestLocalCache(t *testing.T) {
	fixture := newPacketFixture(t, 40,
		packetNode{name: "one", content: []byte("one\n")}, packetNode{name: "two", content: []byte("two\n")})
	for _, used := range []int{packetMaxTreeEntries - 3, packetMaxTreeEntries - 2, packetMaxTreeEntries - 1} {
		api := newAPIFixture(t, fixture.replies)
		reader := packetReader{
			client: api.server.Client(), prefix: api.server.URL + fixturePrefix, identityLength: 40,
			trees: make(map[string]map[string]packetTreeEntry), treeEntries: used,
		}
		tree, err := reader.tree(t.Context(), fixture.target.Tree)
		if used+2 > packetMaxTreeEntries {
			require.ErrorIs(t, err, errPacketLimit)
			require.Empty(t, tree)
		} else {
			require.NoError(t, err)
			require.Len(t, tree, 2)
			_, err = reader.tree(t.Context(), fixture.target.Tree)
			require.NoError(t, err)
			require.Len(t, api.observed(), 1)
		}
	}
	for _, used := range []int{packetMaxTrees - 1, packetMaxTrees} {
		api := newAPIFixture(t, fixture.replies)
		reader := packetReader{
			client: api.server.Client(), prefix: api.server.URL + fixturePrefix, identityLength: 40,
			trees: make(map[string]map[string]packetTreeEntry),
		}
		for index := range used {
			reader.trees[fmt.Sprint(index)] = nil
		}
		_, err := reader.tree(t.Context(), fixture.target.Tree)
		if used == packetMaxTrees {
			require.ErrorIs(t, err, errPacketLimit)
			require.Empty(t, api.observed())
		} else {
			require.NoError(t, err)
			require.Len(t, api.observed(), 1)
		}
	}
	rawBytes := len("100644 one\x00") + 20 + len("100644 two\x00") + 20
	for _, used := range []int{packetMaxTreeBytes - rawBytes - 1, packetMaxTreeBytes - rawBytes, packetMaxTreeBytes - rawBytes + 1} {
		api := newAPIFixture(t, fixture.replies)
		reader := packetReader{
			client: api.server.Client(), prefix: api.server.URL + fixturePrefix, identityLength: 40,
			trees: make(map[string]map[string]packetTreeEntry), treeBytes: used,
		}
		_, err := reader.tree(t.Context(), fixture.target.Tree)
		if used+rawBytes > packetMaxTreeBytes {
			require.ErrorIs(t, err, errPacketLimit)
		} else {
			require.NoError(t, err)
			require.Equal(t, used+rawBytes, reader.treeBytes)
		}
	}
}

func TestPacketRedirectsAndCancellationStayBounded(t *testing.T) {
	fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("source\n")})
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	t.Cleanup(destination.Close)
	for _, blocked := range []string{"tree", "blob", "slow-tree"} {
		t.Run(blocked, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if (strings.Contains(request.URL.Path, "/git/trees/") && blocked != "blob") ||
					(strings.Contains(request.URL.Path, "/git/blobs/") && blocked == "blob") {
					if blocked == "slow-tree" {
						<-request.Context().Done()
					} else {
						http.Redirect(writer, request, destination.URL, http.StatusFound)
					}
					return
				}
				reply, found := fixture.replies[request.URL.Path]
				if !found {
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				writer.WriteHeader(reply.status)
				_, _ = writer.Write([]byte(reply.body))
			}))
			t.Cleanup(server.Close)
			client := Client{APIBaseURL: server.URL, HTTPClient: server.Client()}
			timeout := 2 * time.Second
			if blocked == "slow-tree" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			packet, err := client.Packet(ctx, fixture.target, []string{"file"})
			if blocked == "slow-tree" {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.ErrorContains(t, err, "HTTP 302")
			}
			require.Empty(t, packet)
		})
	}
	require.Zero(t, redirected.Load())
}
