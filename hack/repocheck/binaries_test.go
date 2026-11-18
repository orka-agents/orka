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

// binaryFormat names the compiled-object format of the file r reads, or
// returns "" when it is not one.
func binaryFormat(r io.ReaderAt) (string, error) {
	head := make([]byte, 0x40)
	n, err := r.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("\x7fELF")):
		return "ELF", nil
	case bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xce}),
		bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xcf}),
		bytes.HasPrefix(head, []byte{0xce, 0xfa, 0xed, 0xfe}),
		bytes.HasPrefix(head, []byte{0xcf, 0xfa, 0xed, 0xfe}):
		return "Mach-O", nil
	case bytes.HasPrefix(head, []byte{0xca, 0xfe, 0xba, 0xbe}):
		return "Mach-O universal or Java class", nil
	case bytes.HasPrefix(head, []byte("\x00asm")):
		return "WebAssembly", nil
	case bytes.HasPrefix(head, []byte("!<arch>\n")):
		return "ar archive", nil
	case bytes.HasPrefix(head, []byte("MZ")) && len(head) == 0x40:
		// A PE file's DOS header points at the "PE\0\0" signature, which
		// may sit anywhere after the DOS stub.
		offset := int64(binary.LittleEndian.Uint32(head[0x3c:0x40]))
		if offset < 0x40 {
			return "", nil
		}
		signature := make([]byte, 4)
		if _, err := r.ReadAt(signature, offset); err != nil {
			if errors.Is(err, io.EOF) {
				return "", nil
			}
			return "", err
		}
		if bytes.Equal(signature, []byte("PE\x00\x00")) {
			return "Windows PE", nil
		}
	}
	return "", nil
}

func TestBinaryFormat(t *testing.T) {
	pe := func(offset int) []byte {
		b := make([]byte, offset+4)
		copy(b, "MZ")
		binary.LittleEndian.PutUint32(b[0x3c:], uint32(offset))
		copy(b[offset:], "PE\x00\x00")
		return b
	}
	truncatedPE := pe(0x80)[:0x80]
	for _, tc := range []struct {
		name string
		head []byte
		want string
	}{
		{"elf", []byte("\x7fELF\x02\x01\x01"), "ELF"},
		{"mach-o 64", []byte{0xcf, 0xfa, 0xed, 0xfe, 0x07}, "Mach-O"},
		{"pe", pe(0x80), "Windows PE"},
		{"pe with a large DOS stub", pe(0x2000), "Windows PE"},
		{"pe signature past the end", truncatedPE, ""},
		{"wasm", []byte("\x00asm\x01\x00\x00\x00"), "WebAssembly"},
		{"ar", []byte("!<arch>\ndebian-binary"), "ar archive"},
		{"text starting with MZ", []byte("MZ is a postcode prefix\n"), ""},
		{"go source", []byte("package main\n"), ""},
		{"png", []byte("\x89PNG\r\n\x1a\n"), ""},
	} {
		got, err := binaryFormat(bytes.NewReader(tc.head))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: binaryFormat = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestNoCompiledBinaries enforces the AGENTS.md rule that build artifacts never
// enter the repository.
func TestNoCompiledBinaries(t *testing.T) {
	root := repoRoot(t)
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
		format, err := binaryFormat(file)
		_ = file.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.path, err)
		}
		if format != "" {
			t.Errorf("%s is a compiled %s binary. Build artifacts do not belong in the repository: "+
				"write them to bin/ (gitignored) or publish them from a release workflow, "+
				"then remove this one with: git rm --cached %s",
				f.path, format, f.path)
		}
	}
}
