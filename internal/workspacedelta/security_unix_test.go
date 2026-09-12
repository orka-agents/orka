//go:build darwin || linux

package workspacedelta

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCaptureRejectsFIFOAndSocket(t *testing.T) {
	t.Run("fifo", func(t *testing.T) {
		root := t.TempDir()
		if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		_, err := Capture(root, Options{})
		if !errors.Is(err, ErrUnsafeFileType) {
			t.Fatalf("Capture error = %v, want ErrUnsafeFileType", err)
		}
	})
	t.Run("socket", func(t *testing.T) {
		root, err := os.MkdirTemp("/tmp", "orka-wd-")
		if err != nil {
			t.Fatalf("create short socket directory: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		socketPath := filepath.Join(root, "agent.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			t.Fatalf("listen unix socket: %v", err)
		}
		defer func() { _ = listener.Close() }()
		_, err = Capture(root, Options{})
		if !errors.Is(err, ErrUnsafeFileType) {
			t.Fatalf("Capture error = %v, want ErrUnsafeFileType", err)
		}
	})
}

func TestCaptureRejectsHardlinksInsideOrOutsideWorkspace(t *testing.T) {
	for _, test := range []struct {
		name  string
		alias func(t *testing.T, root, original string)
	}{
		{name: "inside", alias: func(t *testing.T, root, original string) {
			if err := os.Link(original, filepath.Join(root, "alias")); err != nil {
				t.Fatalf("create internal hardlink: %v", err)
			}
		}},
		{name: "outside", alias: func(t *testing.T, root, original string) {
			outside := filepath.Join(t.TempDir(), "alias")
			if err := os.Link(original, outside); err != nil {
				t.Fatalf("create external hardlink: %v", err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			original := filepath.Join(root, "original")
			if err := os.WriteFile(original, []byte("data"), 0o600); err != nil {
				t.Fatalf("write original: %v", err)
			}
			test.alias(t, root, original)
			_, err := Capture(root, Options{})
			if !errors.Is(err, ErrHardlinkAmbiguous) {
				t.Fatalf("Capture error = %v, want ErrHardlinkAmbiguous", err)
			}
		})
	}
}
