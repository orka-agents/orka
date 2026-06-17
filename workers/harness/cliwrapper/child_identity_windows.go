//go:build windows

package cliwrapper

import "os"

const (
	EnvChildUID = "ORKA_HARNESS_WRAPPER_CHILD_UID"
	EnvChildGID = "ORKA_HARNESS_WRAPPER_CHILD_GID"
)

func childCredentialIDs() (int, int, bool) { return 0, 0, false }

func chownTreeForChild(string) error { return nil }

func prepareArtifactsForChild(string) error { return nil }

func prepareHomeForChild(string) error { return nil }

func prepareControlFileForChild(string, uint32) error { return nil }

func prepareOpenControlFileForChild(*os.File, uint32) error { return nil }

func removeAllForChild(string) error { return nil }

func suspendChildIdentity() func() { return func() {} }
