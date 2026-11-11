package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
)

// Match the source-preparation worker's Git base85/zlib checks without running
// Git, applying a patch, or materializing source. Expansion shares one budget
// across the entire response, including forward and reverse binary hunks.
func (p *remediationPatchParser) binary() bool {
	p.position++
	blocks := 0
	for blocks < 2 {
		kind, sizeRaw, ok := strings.Cut(p.line(), " ")
		if !ok || (kind != "literal" && kind != "delta") {
			break
		}
		size, err := strconv.ParseUint(sizeRaw, 10, 32)
		if err != nil || size > uint64(p.binaryBudget) {
			return false
		}
		p.position++
		content, ok := p.binaryBlock(int(size))
		if !ok || (kind == "delta" && !remediationBinaryDelta(content)) {
			return false
		}
		p.binaryBudget -= len(content)
		blocks++
		p.skipBlank()
	}
	return blocks != 0
}

func (p *remediationPatchParser) binaryBlock(size int) ([]byte, bool) {
	var compressed bytes.Buffer
	for p.position < len(p.lines) && p.line() != "" {
		decoded, ok := remediationDecodeGit85(p.line())
		if !ok {
			return nil, false
		}
		_, _ = compressed.Write(decoded)
		p.position++
	}
	input := bytes.NewReader(compressed.Bytes())
	stream, err := zlib.NewReader(input)
	if err != nil {
		return nil, false
	}
	content, readErr := io.ReadAll(io.LimitReader(stream, int64(size)+1))
	closeErr := stream.Close()
	return content, readErr == nil && closeErr == nil && len(content) == size && input.Len() == 0
}

func remediationDecodeGit85(line string) ([]byte, bool) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"
	if len(line) == 0 {
		return nil, false
	}
	var size int
	switch {
	case line[0] >= 'A' && line[0] <= 'Z':
		size = int(line[0]-'A') + 1
	case line[0] >= 'a' && line[0] <= 'z':
		size = int(line[0]-'a') + 27
	default:
		return nil, false
	}
	groups := (size + 3) / 4
	if len(line) != 1+groups*5 {
		return nil, false
	}
	decoded := make([]byte, groups*4)
	for group := range groups {
		var value uint64
		for _, character := range []byte(line[1+group*5 : 1+(group+1)*5]) {
			digit := strings.IndexByte(alphabet, character)
			if digit < 0 {
				return nil, false
			}
			value = value*85 + uint64(digit)
		}
		if value > 1<<32-1 {
			return nil, false
		}
		binary.BigEndian.PutUint32(decoded[group*4:], uint32(value))
	}
	return decoded[:size], true
}

func remediationDeltaSize(content []byte) (uint64, []byte, bool) {
	var size uint64
	for shift := uint(0); len(content) != 0 && shift < 32; shift += 7 {
		value := content[0]
		content = content[1:]
		size |= uint64(value&0x7f) << shift
		if size > remediationBinaryBudget {
			return 0, nil, false
		}
		if value&0x80 == 0 {
			return size, content, true
		}
	}
	return 0, nil, false
}

func remediationBinaryDelta(content []byte) bool {
	sourceSize, content, ok := remediationDeltaSize(content)
	if !ok {
		return false
	}
	targetSize, content, ok := remediationDeltaSize(content)
	if !ok {
		return false
	}
	var produced uint64
	for len(content) != 0 {
		command := content[0]
		content = content[1:]
		if command&0x80 == 0 {
			if command == 0 || len(content) < int(command) {
				return false
			}
			content = content[command:]
			produced += uint64(command)
		} else {
			offset, count := uint64(0), uint64(0)
			for bit := range 7 {
				if command&(1<<bit) == 0 {
					continue
				}
				if len(content) == 0 {
					return false
				}
				if bit < 4 {
					offset |= uint64(content[0]) << (bit * 8)
				} else {
					count |= uint64(content[0]) << ((bit - 4) * 8)
				}
				content = content[1:]
			}
			if count == 0 {
				count = 65536
			}
			if offset+count > sourceSize {
				return false
			}
			produced += count
		}
		if produced > targetSize {
			return false
		}
	}
	return produced == targetSize
}
