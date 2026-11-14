/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package scout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
)

const (
	// MaxDigestBytes bounds one encoded digest.
	MaxDigestBytes = 256 << 10
	// MaxProposalBytes bounds one encoded proposal.
	MaxProposalBytes = 32 << 10
	// DigestFenceInfo is the info string of the fenced block that carries the
	// digest inside a scout Task's final answer.
	DigestFenceInfo = "orka-scout-digest"

	maxJSONDepth = 8
)

// ParseDigest strictly decodes and validates one digest.
func ParseDigest(data []byte, limits Limits) (*Digest, error) {
	digest := &Digest{}
	if err := decodeStrict(data, MaxDigestBytes, digest); err != nil {
		return nil, err
	}
	if err := digest.Validate(limits); err != nil {
		return nil, err
	}
	return digest, nil
}

// ParseProposal strictly decodes and validates one proposal.
func ParseProposal(data []byte) (*Proposal, error) {
	proposal := &Proposal{}
	if err := decodeStrict(data, MaxProposalBytes, proposal); err != nil {
		return nil, err
	}
	if err := proposal.Validate(); err != nil {
		return nil, err
	}
	return proposal, nil
}

// ExtractDigest returns the JSON body of the single fenced block tagged
// DigestFenceInfo in a scout's final answer. Zero or several such blocks are
// errors: a fetched page that smuggles in its own block must not be able to
// replace or shadow the scout's digest. Any longer backtick fence or tilde
// fence is an error too, and so is an indented backtick fence, which Markdown
// may still open or close a block with. Other three-backtick blocks are
// skipped whole, so a tagged line quoted inside one is never read as the
// digest.
func ExtractDigest(answer string) ([]byte, error) {
	if len(answer) > 4*MaxDigestBytes {
		return nil, fmt.Errorf("answer exceeds %d bytes", 4*MaxDigestBytes)
	}
	// Markdown ends a line at CRLF, LF or a lone CR, so split on all three:
	// a fence after a lone CR is still a fence.
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(answer, "\r\n", "\n"), "\r", "\n"), "\n")
	// Check every line before any block is skipped: a longer backtick fence
	// closes a three-backtick block in Markdown, so one inside a skipped
	// block would otherwise hide the next digest block from the count.
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed != line && strings.HasPrefix(trimmed, "```") {
			return nil, errors.New("answer must not contain indented backtick code fences")
		}
		if strings.HasPrefix(trimmed, "````") || strings.HasPrefix(trimmed, "~~~") {
			return nil, errors.New("answer must not contain longer backtick or tilde code fences")
		}
	}
	var body []string
	found := 0
	for index := 0; index < len(lines); index++ {
		line := strings.TrimRight(lines[index], " \t")
		if !strings.HasPrefix(line, "```") {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(line, "```")) != DigestFenceInfo {
			// Any other fenced block is quoted text: skip to its closing
			// fence so a tagged line inside it is never read as a block.
			for index++; index < len(lines) && strings.TrimRight(lines[index], " \t") != "```"; index++ {
			}
			if index >= len(lines) {
				return nil, errors.New("code block is not closed")
			}
			continue
		}
		found++
		closed := false
		var block []string
		for index++; index < len(lines); index++ {
			if strings.TrimRight(lines[index], " \t") == "```" {
				closed = true
				break
			}
			block = append(block, lines[index])
		}
		if !closed {
			return nil, errors.New("digest block is not closed")
		}
		body = block
	}
	switch found {
	case 0:
		return nil, fmt.Errorf("no %q block found", DigestFenceInfo)
	case 1:
		return []byte(strings.Join(body, "\n")), nil
	default:
		return nil, fmt.Errorf("found %d %q blocks, want exactly one", found, DigestFenceInfo)
	}
}

// decodeStrict decodes exactly one JSON value into target. On top of
// DisallowUnknownFields it rejects duplicate keys (encoding/json silently
// keeps the last one), keys that only match a field case-insensitively, and
// excessive nesting, so what a reviewer sees is exactly what was decoded.
func decodeStrict(data []byte, maxBytes int, target any) error {
	if len(data) == 0 {
		return errors.New("input is empty")
	}
	if len(data) > maxBytes {
		return fmt.Errorf("input exceeds %d bytes", maxBytes)
	}
	if err := checkJSONKeys(data, allowedKeys(reflect.TypeOf(target))); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("input must contain exactly one JSON value")
	}
	return nil
}

func checkJSONKeys(data []byte, allowed map[string]struct{}) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var stack []*jsonFrame
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		var top *jsonFrame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.expectKey {
			if delim, ok := token.(json.Delim); ok && delim == '}' {
				stack = stack[:len(stack)-1]
				markValueDone(stack)
				continue
			}
			key, _ := token.(string)
			if _, ok := allowed[key]; !ok {
				return fmt.Errorf("unknown field %q", key)
			}
			if _, duplicate := top.keys[key]; duplicate {
				return fmt.Errorf("duplicate field %q", key)
			}
			top.keys[key] = struct{}{}
			top.expectKey = false
			continue
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			if len(stack) >= maxJSONDepth {
				return fmt.Errorf("input nests deeper than %d levels", maxJSONDepth)
			}
			object := token == json.Delim('{')
			stack = append(stack, &jsonFrame{object: object, expectKey: object, keys: map[string]struct{}{}})
		case json.Delim(']'):
			stack = stack[:len(stack)-1]
			markValueDone(stack)
		default:
			markValueDone(stack)
		}
	}
}

type jsonFrame struct {
	object    bool
	expectKey bool
	keys      map[string]struct{}
}

// markValueDone records that the enclosing object, if any, now expects a key.
func markValueDone(stack []*jsonFrame) {
	if len(stack) > 0 && stack[len(stack)-1].object {
		stack[len(stack)-1].expectKey = true
	}
}

var allowedKeysCache sync.Map

// allowedKeys returns the exact JSON field names reachable from typ.
func allowedKeys(typ reflect.Type) map[string]struct{} {
	if cached, ok := allowedKeysCache.Load(typ); ok {
		return cached.(map[string]struct{})
	}
	keys := map[string]struct{}{}
	collectKeys(typ, keys, map[reflect.Type]bool{})
	allowedKeysCache.Store(typ, keys)
	return keys
}

func collectKeys(typ reflect.Type, keys map[string]struct{}, visited map[reflect.Type]bool) {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || visited[typ] {
		return
	}
	visited[typ] = true
	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		keys[name] = struct{}{}
		collectKeys(field.Type, keys, visited)
	}
}
