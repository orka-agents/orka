package artifactcap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytesReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(buffer)
	if err == nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
	}
	return n, err
}

func ensurePrivateDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create artifact storage directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect artifact storage directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafePath
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure artifact storage directory: %w", err)
		}
	}
	return nil
}

func syncDirectory(dir string) error {
	file, err := openFileNoFollow(dir, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck
	return file.Sync()
}

func replaceRegularFile(source, destination string) error {
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return ErrUnsafePath
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(source, destination)
}
