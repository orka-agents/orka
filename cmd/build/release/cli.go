package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	windowsOS = "windows"
	amd64Arch = "amd64"
)

type cliTarget struct {
	os, arch string
}

// Keep archive and checksum ordering stable across matrix job completion order.
var cliTargets = []cliTarget{
	{"darwin", amd64Arch}, {"darwin", "arm64"},
	{"linux", amd64Arch}, {"linux", "arm64"},
	{windowsOS, amd64Arch},
}

func (target cliTarget) archiveName(version string) string {
	extension := ".tar.gz"
	if target.os == windowsOS {
		extension = ".zip"
	}
	return fmt.Sprintf("orka_%s_%s_%s%s", version, target.os, target.arch, extension)
}

func cliChecksumFile(version string) string {
	return "orka_" + version + "_checksums.txt"
}

type cliArchiveEntry struct {
	name string
	data []byte
	mode fs.FileMode
}

func packageCLI(root, directory, version, goos, goarch string) error {
	if _, err := branchFor(version); err != nil {
		return err
	}
	target := cliTarget{goos, goarch}
	supported := slices.Contains(cliTargets, target)
	if !supported {
		return fmt.Errorf("unsupported CLI target %s/%s", goos, goarch)
	}
	binaryName := "orka"
	if goos == windowsOS {
		binaryName += ".exe"
	}
	binary, err := os.ReadFile(filepath.Join(directory, binaryName))
	if err != nil {
		return err
	}
	if len(binary) == 0 {
		return errors.New("CLI binary is empty")
	}
	license, err := os.ReadFile(filepath.Join(root, "LICENSE"))
	if err != nil {
		return err
	}
	readme := fmt.Sprintf("Orka CLI %s\n\n"+
		"Put %s on your PATH, then run `orka version` and `orka --help`.\n"+
		"Use the CLI version that matches your Orka controller installation.\n\n"+
		"Documentation: https://orka-agents.github.io/orka/docs/cli-reference\n"+
		"Source and releases: https://github.com/%s\n", version, binaryName, repository)
	entries := []cliArchiveEntry{
		{name: binaryName, data: binary, mode: 0o755},
		{name: "LICENSE", data: license, mode: 0o644},
		{name: "README", data: []byte(readme), mode: 0o644},
	}
	archive, err := os.Create(filepath.Join(directory, target.archiveName(version)))
	if err != nil {
		return err
	}
	if goos == windowsOS {
		err = writeCLIZip(archive, entries)
	} else {
		err = writeCLITar(archive, entries)
	}
	return errors.Join(err, archive.Close())
}

func writeCLITar(output io.Writer, entries []cliArchiveEntry) error {
	compressed := gzip.NewWriter(output)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		err := archive.WriteHeader(&tar.Header{Name: entry.name, Mode: int64(entry.mode), Size: int64(len(entry.data))})
		if err == nil {
			_, err = archive.Write(entry.data)
		}
		if err != nil {
			return errors.Join(err, archive.Close(), compressed.Close())
		}
	}
	return errors.Join(archive.Close(), compressed.Close())
}

func writeCLIZip(output io.Writer, entries []cliArchiveEntry) error {
	archive := zip.NewWriter(output)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(entry.mode)
		file, err := archive.CreateHeader(header)
		if err == nil {
			_, err = file.Write(entry.data)
		}
		if err != nil {
			return errors.Join(err, archive.Close())
		}
	}
	return archive.Close()
}

func cliArchives(directory, version string) ([]releaseArtifact, error) {
	if _, err := branchFor(version); err != nil {
		return nil, err
	}
	artifacts := make([]releaseArtifact, 0, len(cliTargets))
	for _, target := range cliTargets {
		name := target.archiveName(version)
		digest, err := fileHash(filepath.Join(directory, name))
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, releaseArtifact{File: name, SHA256: digest})
	}
	return artifacts, nil
}

func cliChecksums(archives []releaseArtifact) string {
	var checksums strings.Builder
	for _, archive := range archives {
		fmt.Fprintf(&checksums, "%s  %s\n", archive.SHA256, archive.File)
	}
	return checksums.String()
}

func writeCLIChecksums(directory, version string) error {
	archives, err := cliArchives(directory, version)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, cliChecksumFile(version)), []byte(cliChecksums(archives)), 0o644)
}

func cliArtifacts(directory, version string) ([]releaseArtifact, error) {
	archives, err := cliArchives(directory, version)
	if err != nil {
		return nil, err
	}
	checksumFile := cliChecksumFile(version)
	checksums, err := os.ReadFile(filepath.Join(directory, checksumFile))
	if err != nil {
		return nil, err
	}
	if string(checksums) != cliChecksums(archives) {
		return nil, errors.New("CLI checksums do not match the release archives")
	}
	for _, name := range []string{checksumFile, checksumFile + ".bundle"} {
		digest, err := fileHash(filepath.Join(directory, name))
		if err != nil {
			return nil, err
		}
		archives = append(archives, releaseArtifact{File: name, SHA256: digest})
	}
	return archives, nil
}

func (w *workflow) verifyCLISignature(data candidateBundle, directory string) error {
	checksums := filepath.Join(directory, cliChecksumFile(data.Version))
	_, err := w.command("cosign", "verify-blob", "--bundle", checksums+".bundle",
		"--certificate-identity", "https://github.com/"+repository+"/.github/workflows/"+releaseWorkflow+
			"@refs/heads/"+data.Branch,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
		"--certificate-github-workflow-sha", data.CandidateSHA, checksums)
	if err != nil {
		return errors.New("CLI checksum signature verification failed")
	}
	return nil
}
