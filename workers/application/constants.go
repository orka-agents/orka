//go:build linux

package main

const (
	modePrepare  = "prepare"
	modeBuild    = "build"
	modeSubject  = "subject"
	modeObserve  = "observe"
	modeBoundary = "boundary"
	modeMarker   = "marker"
	modeFixture  = "fixture"
	modeInstall  = "install"
	modeGit      = "git"

	targetTokenPath   = "/target/token"
	workerHomeEnv     = "HOME=/work"
	workerLanguageEnv = "LANG=C"
	workerLocaleEnv   = "LC_ALL=C"
)
