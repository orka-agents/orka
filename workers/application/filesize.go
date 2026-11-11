//go:build linux

package main

import (
	"errors"
	"os"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func installFileSizeLimit(mode string) error {
	role := "execute"
	switch mode {
	case modePrepare, modeGit:
		role = modePrepare
	case modeBuild:
		role = modeBuild
	case modeInstall:
		role = "initialize"
	case modeBoundary:
		role = os.Getenv("ORKA_APPLICATION_FILESIZE_ROLE")
	case modeSubject, modeObserve, modeMarker, modeFixture:
	default:
		return pv.ErrEvidence
	}
	limit, err := pv.ApplicationFileSizeLimit(role)
	if err != nil {
		return err
	}
	var inherited unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_FSIZE, &inherited) != nil || inherited.Max < limit {
		return errors.New("inherited file-size ceiling cannot support this trusted worker role")
	}
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: limit, Max: limit}); err != nil {
		return errors.New("application file-size hard limit could not be established")
	}
	var actual unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_FSIZE, &actual) != nil || actual.Cur != limit || actual.Max != limit {
		return errors.New("application file-size limit differs from its fixed role")
	}
	return nil
}
