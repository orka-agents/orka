//go:build !darwin && !linux

package supervisor

import (
	"fmt"
	"io"
	"runtime"
)

func lockSessionIdentityState(string) (io.Closer, error) {
	return nil, fmt.Errorf("cross-process session identity locking is unsupported on %s", runtime.GOOS)
}
