//go:build linux

package main

import (
	"context"
	"errors"
	"os"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func runCatalogueFixture(ctx context.Context, input pv.ApplicationWorkerInput) error {
	if _, err := measureProcess(); err != nil {
		return err
	}
	if err := noTargetCredentials(); err != nil {
		return err
	}
	if input.Manifest.ApplicationPolicy == nil || input.FixtureID == "" || input.MarkerRef != nil {
		return pv.ErrEvidence
	}
	if _, err := os.Stat(pv.ApplicationMarkerPath); !os.IsNotExist(err) {
		return pv.ErrEvidence
	}
	if err := verifyFixtureSecrets(input); err != nil {
		return err
	}
	selected, replica := selectCatalogueFixture(input)
	if selected == nil || len(selected.Args) == 0 {
		return pv.ErrBinding
	}
	if err := validateFixtureExecutable(selected.Args[0]); err != nil {
		return err
	}
	if err := os.Chdir(pv.ApplicationWorkPath); err != nil {
		return err
	}
	environment := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", workerHomeEnv, "TMPDIR=/work", workerLanguageEnv, workerLocaleEnv,
	}
	if replica {
		if os.Getenv("ORKA_APPLICATION_DEADLINE") == "" || selected.ReplicaTarget == nil ||
			len(selected.ConfigKeys) != 0 || len(selected.SecretFiles) != 0 {
			return pv.ErrEvidence
		}
		return runReplicaProcess(ctx, selected.Args, environment)
	}
	return execCatalogueFixture(ctx, selected.Args, environment)
}

func selectCatalogueFixture(input pv.ApplicationWorkerInput) (*pv.ApplicationFixtureProfile, bool) {
	var selected *pv.ApplicationFixtureProfile
	replica := false
	for _, fixture := range input.Manifest.Application.Fixtures {
		if fixture.ID != input.FixtureID ||
			(fixture.Kind != pv.ApplicationFixtureImage && fixture.Kind != pv.ApplicationFixtureReplica) {
			continue
		}
		for i := range input.Manifest.ApplicationPolicy.Fixtures {
			profile := &input.Manifest.ApplicationPolicy.Fixtures[i]
			if profile.ID == fixture.Profile {
				selected = profile
				replica = fixture.Kind == pv.ApplicationFixtureReplica
			}
		}
	}
	return selected, replica
}

func validateFixtureExecutable(executable string) error {
	var filesystem unix.Statfs_t
	if unix.Statfs(executable, &filesystem) != nil || filesystem.Flags&unix.ST_RDONLY == 0 {
		return errors.New("application fixture executable must come from the read-only operator image")
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return pv.ErrEvidence
	}
	return nil
}

func execCatalogueFixture(ctx context.Context, argv, environment []string) error {
	null, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return pv.ErrEvidence
	}
	defer func() { _ = unix.Close(null) }()
	for _, fd := range []int{0, 1, 2} {
		if unix.Dup2(null, fd) != nil {
			return pv.ErrEvidence
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return unix.Exec(argv[0], argv, environment)
}
