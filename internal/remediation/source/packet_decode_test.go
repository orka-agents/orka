package source

import (
	"crypto/rand"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodePacketExactShape(t *testing.T) {
	fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("source\n")})
	packet := fixture.packet("file")
	encoded := packetJSON(t, packet)
	require.JSONEq(t, `{"version":1,"target":{"repository":{"url":"`+fixtureURL+
		`","owner":"source-fixtures","name":"project","defaultBranch":"main","archived":false},"ref":"synthetic-release","commit":"`+
		fixture.target.Commit+`","tree":"`+fixture.target.Tree+`"},"files":[{"path":"file","content":"source\n","sha256":"`+
		packet.Files[0].SHA256+`","blobSHA":"`+packet.Files[0].BlobSHA+`"}]}`, encoded)
	indented, err := json.MarshalIndent(packet, "", "  ")
	require.NoError(t, err)
	decoded, err := DecodePacket(indented)
	require.NoError(t, err)
	require.Equal(t, packet, decoded)
	for _, fields := range []struct {
		level string
		names []string
	}{
		{"document", []string{"version", "target", "files"}},
		{"target", []string{"repository", "ref", "commit", "tree"}},
		{"repository", []string{"url", "owner", "name", "defaultBranch", "archived"}},
		{"file", []string{"path", "content", "sha256", "blobSHA"}},
	} {
		for _, name := range fields.names {
			for _, mutation := range []string{"missing", "null", "uppercase", "unknown"} {
				t.Run(fields.level+"/"+name+"/"+mutation, func(t *testing.T) {
					var document map[string]any
					require.NoError(t, json.Unmarshal([]byte(encoded), &document))
					target := document["target"].(map[string]any)
					object := document
					switch fields.level {
					case "target":
						object = target
					case "repository":
						object = target["repository"].(map[string]any)
					case "file":
						object = document["files"].([]any)[0].(map[string]any)
					}
					switch mutation {
					case "missing":
						delete(object, name)
					case "null":
						object[name] = nil
					case "uppercase":
						object[strings.ToUpper(name)] = object[name]
						delete(object, name)
					case "unknown":
						object["unknown"] = true
					}
					decoded, err := DecodePacket([]byte(packetJSON(t, document)))
					require.Error(t, err)
					require.Empty(t, decoded)
				})
			}
		}
	}
}

func TestDecodePacketRejectsAmbiguousMalformedAndInexactDocuments(t *testing.T) {
	fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("source\n")})
	encoded := packetJSON(t, fixture.packet("file"))
	for _, data := range []string{
		"", "null", "[]", "{}", encoded + "{}", encoded + "unexpected",
		strings.Replace(encoded, `"version":1`, `"version":"1"`, 1),
		strings.Replace(encoded, `"version":1`, `"version":2`, 1),
		strings.Replace(encoded, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(encoded, `"version":1`, `"version":1,"VERSION":1`, 1),
		strings.Replace(encoded, `"version":1`, `"version":1,"\u0076ersion":1`, 1),
		strings.Replace(encoded, `"archived":false`, `"archived":"false"`, 1),
		strings.Replace(encoded, `"archived":false`, `"archived":false,"archived":true`, 1),
		strings.Replace(encoded, `"path":"file"`, `"path":"file","PATH":"file"`, 1),
		strings.Replace(encoded, `"content":"source\n"`, `"content":42`, 1),
		strings.Replace(encoded, `"content":"source\n"`, "\"content\":\"source\xff\\n\"", 1),
		strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40),
	} {
		packet, err := DecodePacket([]byte(data))
		require.Error(t, err)
		require.Empty(t, packet)
	}
	for _, mutate := range []func(*Packet){
		func(packet *Packet) { packet.Target.Commit = "HEAD" },
		func(packet *Packet) {
			packet.Target.Repository.URL = (&url.URL{
				Scheme: "https",
				Host:   "github.com",
				Path:   "/owner/repo",
				User:   url.UserPassword("SYNTHETIC_USER_"+rand.Text(), "SYNTHETIC_PASSWORD_"+rand.Text()),
			}).String()
		},
		func(packet *Packet) { packet.Files = nil },
		func(packet *Packet) { packet.Files = append(packet.Files, packet.Files[0]) },
		func(packet *Packet) { packet.Files[0].Path = "../file" },
		func(packet *Packet) { packet.Files[0].Content = "changed\n" },
		func(packet *Packet) { packet.Files[0].SHA256 = strings.Repeat("f", 64) },
		func(packet *Packet) { packet.Files[0].SHA256 = "sha256:" + packet.Files[0].SHA256 },
		func(packet *Packet) { packet.Files[0].SHA256 = strings.ToUpper(packet.Files[0].SHA256) },
		func(packet *Packet) { packet.Files[0].BlobSHA = fixtureCommit },
		func(packet *Packet) { packet.Files[0].BlobSHA = strings.Repeat("f", 64) },
	} {
		packet := fixture.packet("file")
		mutate(&packet)
		decoded, err := DecodePacket([]byte(packetJSON(t, packet)))
		require.Error(t, err)
		require.Empty(t, decoded)
	}
}

func TestDecodePacketByteBudgetsIncludeEscapedAndUTF8Text(t *testing.T) {
	for _, size := range []int{packetMaxTextBytes - 1, packetMaxTextBytes, packetMaxTextBytes + 1} {
		content := []byte(strings.Repeat("<", size-1) + "\n")
		fixture := newPacketFixture(t, 40, packetNode{name: "file", content: content})
		encoded := []byte(packetJSON(t, fixture.packet("file")))
		require.Greater(t, len(encoded), size)
		packet, err := DecodePacket(encoded)
		if size > packetMaxTextBytes {
			require.ErrorIs(t, err, errPacketLimit)
			require.Empty(t, packet)
		} else {
			require.NoError(t, err)
			require.Len(t, packet.Files[0].Content, size)
		}
	}
	fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("source\n")})
	encoded := packetJSON(t, fixture.packet("file"))
	for _, size := range []int{packetMaxJSONBytes - 1, packetMaxJSONBytes, packetMaxJSONBytes + 1} {
		data := []byte(encoded + strings.Repeat(" ", size-len(encoded)))
		packet, err := DecodePacket(data)
		if size > packetMaxJSONBytes {
			require.ErrorIs(t, err, errPacketLimit)
			require.Empty(t, packet)
		} else {
			require.NoError(t, err)
			require.Equal(t, fixture.packet("file"), packet)
		}
	}
	one := strings.Repeat("\u00e9", packetMaxTextBytes/4) + "\n"
	fixture = newPacketFixture(t, 40,
		packetNode{name: "one", content: []byte(one)}, packetNode{name: "two", content: []byte(one)})
	_, err := DecodePacket([]byte(packetJSON(t, fixture.packet("one", "two"))))
	require.ErrorIs(t, err, errPacketLimit)
}

func TestDecodePacketDoesNotAttestRemoteBinding(t *testing.T) {
	fixture := newPacketFixture(t, 40, packetNode{name: "file", content: []byte("source\n")})
	packet := fixture.packet("file")
	// Local integrity is deliberately not a membership proof. A caller must
	// obtain remote binding from Client.Packet, not from this decoder's success.
	packet.Target.Tree = fixtureTree
	decoded, err := DecodePacket([]byte(packetJSON(t, packet)))
	require.NoError(t, err)
	require.Equal(t, packet, decoded)
	api := newAPIFixture(t, fixture.replies)
	_, err = api.client.Packet(t.Context(), packet.Target, []string{"file"})
	require.Error(t, err)
	require.Len(t, api.observed(), 2, "remote commit linkage rejects the locally well-formed forged target")
}

func TestPacketArrayDecodingIsBoundedAndUnambiguous(t *testing.T) {
	for _, count := range []int{31, 32, 33} {
		data := []byte("[" + strings.Repeat("0,", count-1) + "0]")
		values, err := decodePacketArray[int](data, packetMaxFiles)
		if count > packetMaxFiles {
			require.ErrorIs(t, err, errPacketLimit)
			require.Empty(t, values)
		} else {
			require.NoError(t, err)
			require.Len(t, values, count)
		}
	}
	for _, data := range []string{"", "null", "{}", "[", "[0,]", "[0] true", `["not an integer"]`} {
		_, err := decodePacketArray[int]([]byte(data), 32)
		require.Error(t, err)
	}
	values, err := decodePacketArray[int]([]byte("[]"), 0)
	require.NoError(t, err)
	require.Empty(t, values)
	_, err = decodePacketArray[int]([]byte("[0]"), 0)
	require.ErrorIs(t, err, errPacketLimit)
	_, err = decodePacketArray[int]([]byte("[]"), -1)
	require.ErrorIs(t, err, errPacketLimit)
}
