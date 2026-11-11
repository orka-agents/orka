//go:build linux

package main

import (
	"errors"
	"io"
	"path/filepath"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func readReadonlyMaterial(name string, maximum int) ([]byte, error) {
	var filesystem unix.Statfs_t
	if unix.Statfs(name, &filesystem) != nil || filesystem.Flags&unix.ST_RDONLY == 0 {
		return nil, errors.New("application startup material must be read-only")
	}
	file, err := openProjected(name)
	if err != nil {
		return nil, err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(content) == 0 || len(content) > maximum {
		return nil, pv.ErrEvidence
	}
	return content, nil
}

func verifySubjectTLS(input pv.ApplicationWorkerInput) error {
	err := pv.ValidateApplicationSubjectTLSIdentities(input.Manifest.ApplicationPolicy, input.Target, input.SubjectTLS)
	if err != nil {
		return err
	}
	for _, identity := range input.SubjectTLS {
		var profile pv.ApplicationSubjectTLSBundle
		for _, configured := range input.Manifest.ApplicationPolicy.SubjectTLS {
			if configured.ID == identity.ID {
				profile = configured
			}
		}
		content := map[string][]byte{}
		for _, name := range []string{"ca.crt", "tls.crt", "tls.key"} {
			value, err := readReadonlyMaterial(filepath.Join(pv.ApplicationSubjectConfigPath, identity.ID, name), 16<<10)
			if err != nil {
				return err
			}
			content[name] = value
		}
		digest, err := pv.ValidateApplicationTLSBundle(
			profile, input.Target.Namespace, content["ca.crt"], content["tls.crt"], content["tls.key"],
		)
		if err != nil || digest != identity.Digest {
			return errors.New("application startup TLS differs from its bound synthetic Secret")
		}
	}
	return nil
}

func verifyFixtureSecrets(input pv.ApplicationWorkerInput) error {
	if err := pv.ValidateApplicationFixtureSecretIdentities(input); err != nil {
		return err
	}
	for _, identity := range input.FixtureSecrets {
		content, err := readReadonlyMaterial(filepath.Join(pv.ApplicationFixtureSecretsPath, identity.ID), 128)
		if err != nil || len(content) < 32 || pv.Digest(content) != identity.Digest {
			return errors.New("application fixture secret file differs from its exact synthetic binding")
		}
	}
	return nil
}
