package workspacedelta

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidRoot           = errors.New("invalid workspace root")
	ErrInvalidBaseline       = errors.New("invalid trusted baseline")
	ErrPathTraversal         = errors.New("workspace path traversal")
	ErrReservedPath          = errors.New("reserved workspace path")
	ErrExcludedPathModified  = errors.New("excluded workspace path modified")
	ErrUnsafeFileType        = errors.New("unsafe workspace file type")
	ErrHardlinkAmbiguous     = errors.New("workspace hardlink ambiguity")
	ErrUnsafeSymlink         = errors.New("unsafe workspace symlink")
	ErrLimitExceeded         = errors.New("workspace delta limit exceeded")
	ErrUnsupportedFilesystem = errors.New("unsupported filesystem safety primitive")
)

// PathError associates a safety failure with a slash-separated workspace path.
type PathError struct {
	Op   string
	Path string
	Err  error
}

func (e *PathError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Path == "" {
		return fmt.Sprintf("%s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("%s %q: %v", e.Op, e.Path, e.Err)
}

func (e *PathError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func pathError(op, path string, err error) error {
	return &PathError{Op: op, Path: path, Err: err}
}
