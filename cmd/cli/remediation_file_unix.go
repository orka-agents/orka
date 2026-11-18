//go:build !windows

package main

import (
	"errors"
	"os"
)

func checkRemediationPrivateFile(_ *os.File, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("private file must have owner-only permissions")
	}
	return nil
}
