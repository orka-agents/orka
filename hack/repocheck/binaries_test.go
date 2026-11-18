/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package repocheck

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// binaryFormat names the compiled-object format whose header starts head,
// or returns "" when head is not one.
func binaryFormat(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x7fELF")):
		return "ELF"
	case bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xce}),
		bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xcf}),
		bytes.HasPrefix(head, []byte{0xce, 0xfa, 0xed, 0xfe}),
		bytes.HasPrefix(head, []byte{0xcf, 0xfa, 0xed, 0xfe}):
		return "Mach-O"
	case bytes.HasPrefix(head, []byte{0xca, 0xfe, 0xba, 0xbe}):
		return "Mach-O universal or Java class"
	case bytes.HasPrefix(head, []byte("\x00asm")):
		return "WebAssembly"
	case bytes.HasPrefix(head, []byte("!<arch>\n")):
		return "ar archive"
	case bytes.HasPrefix(head, []byte("MZ")) && len(head) >= 0x40:
		// A PE file's DOS header points at the "PE\0\0" signature.
		offset := int(binary.LittleEndian.Uint32(head[0x3c:0x40]))
		if offset >= 0x40 && offset+4 <= len(head) && bytes.Equal(head[offset:offset+4], []byte("PE\x00\x00")) {
			return "Windows PE"
		}
	}
	return ""
}

func TestBinaryFormat(t *testing.T) {
	pe := make([]byte, 0x84)
	copy(pe, "MZ")
	binary.LittleEndian.PutUint32(pe[0x3c:], 0x80)
	copy(pe[0x80:], "PE\x00\x00")
	for _, tc := range []struct {
		name string
		head []byte
		want string
	}{
		{"elf", []byte("\x7fELF\x02\x01\x01"), "ELF"},
		{"mach-o 64", []byte{0xcf, 0xfa, 0xed, 0xfe, 0x07}, "Mach-O"},
		{"pe", pe, "Windows PE"},
		{"wasm", []byte("\x00asm\x01\x00\x00\x00"), "WebAssembly"},
		{"ar", []byte("!<arch>\ndebian-binary"), "ar archive"},
		{"text starting with MZ", []byte("MZ is a postcode prefix\n"), ""},
		{"go source", []byte("package main\n"), ""},
		{"png", []byte("\x89PNG\r\n\x1a\n"), ""},
	} {
		if got := binaryFormat(tc.head); got != tc.want {
			t.Errorf("%s: binaryFormat = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestNoCompiledBinaries enforces the AGENTS.md rule that build artifacts never
// enter the repository.
func TestNoCompiledBinaries(t *testing.T) {
	root := repoRoot(t)
	head := make([]byte, 1024)
	for _, f := range trackedFiles(t, root) {
		// Skip symlinks (120000) and submodules (160000).
		if f.mode != "100644" && f.mode != "100755" {
			continue
		}
		file, err := os.Open(filepath.Join(root, f.path))
		if errors.Is(err, os.ErrNotExist) {
			continue // deleted in the working tree but still staged
		}
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.ReadFull(file, head)
		_ = file.Close()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%s: %v", f.path, err)
		}
		if format := binaryFormat(head[:n]); format != "" {
			t.Errorf("%s is a compiled %s binary. Build artifacts do not belong in the repository: "+
				"write them to bin/ (gitignored) or publish them from a release workflow, "+
				"then remove this one with: git rm --cached %s",
				f.path, format, f.path)
		}
	}
}
