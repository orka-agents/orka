package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCLIReleaseArchiveNames(t *testing.T) {
	names := make([]string, 0, len(cliTargets))
	for _, target := range cliTargets {
		names = append(names, target.archiveName("v0.3.0"))
	}
	want := []string{
		"orka_v0.3.0_darwin_amd64.tar.gz", "orka_v0.3.0_darwin_arm64.tar.gz",
		"orka_v0.3.0_linux_amd64.tar.gz", "orka_v0.3.0_linux_arm64.tar.gz", "orka_v0.3.0_windows_amd64.zip",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("CLI archives = %v, want %v", names, want)
	}
}

func TestPackageCLIContainsOnlyBinaryLicenseAndReadme(t *testing.T) {
	for _, version := range []string{"v0.3.0", "v0.3.0-rc.1"} {
		for _, target := range cliTargets {
			t.Run(version+"/"+target.os+"/"+target.arch, func(t *testing.T) {
				root := t.TempDir()
				directory := filepath.Join(root, "dist")
				binaryName := "orka"
				if target.os == windowsOS {
					binaryName += ".exe"
				}
				writeTestFile(t, filepath.Join(root, "LICENSE"), "license fixture")
				binary := filepath.Join(directory, binaryName)
				writeTestFile(t, binary, "built CLI bytes "+version)
				writeTestFile(t, filepath.Join(directory, "unrelated-file"), "must not be packaged")
				w := newWorkflow(root)
				args := []string{"package-cli", "dist", version, target.os, target.arch}
				must(t, w.execute(args))
				path := filepath.Join(directory, target.archiveName(version))
				entries := readCLIArchive(t, path)
				if len(entries) != 3 {
					t.Fatalf("archive contains %d entries, want 3", len(entries))
				}
				for name, want := range map[string]string{binaryName: "built CLI bytes " + version, "LICENSE": "license fixture"} {
					if string(entries[name].data) != want {
						t.Fatalf("archive entry %s has wrong bytes", name)
					}
				}
				for _, text := range []string{
					version, binaryName, "orka version", "https://orka-agents.github.io/orka/docs/cli-reference",
				} {
					if !strings.Contains(string(entries["README"].data), text) {
						t.Fatalf("README does not contain %q", text)
					}
				}
				if entries[binaryName].mode.Perm() != 0o755 || entries["LICENSE"].mode.Perm() != 0o644 ||
					entries["README"].mode.Perm() != 0o644 {
					t.Fatal("archive has incorrect file permissions")
				}
				digest := hashTestFile(t, path)
				must(t, os.Chtimes(binary, time.Unix(123, 0), time.Unix(123, 0)))
				must(t, w.execute(args))
				if hashTestFile(t, path) != digest {
					t.Fatal("packaging the same bytes changed the archive")
				}
			})
		}
	}
}

func readCLIArchive(t *testing.T, path string) map[string]cliArchiveEntry {
	t.Helper()
	entries := map[string]cliArchiveEntry{}
	add := func(name string, mode fs.FileMode, reader io.Reader) {
		t.Helper()
		if _, duplicate := entries[name]; duplicate || filepath.Base(name) != name || !mode.IsRegular() {
			t.Fatalf("unexpected archive entry %q", name)
		}
		data, err := io.ReadAll(reader)
		must(t, err)
		entries[name] = cliArchiveEntry{name: name, mode: mode, data: data}
	}
	if strings.HasSuffix(path, ".zip") {
		archive, err := zip.OpenReader(path)
		must(t, err)
		defer func() { must(t, archive.Close()) }()
		for _, file := range archive.File {
			reader, err := file.Open()
			must(t, err)
			add(file.Name, file.Mode(), reader)
			must(t, reader.Close())
		}
	} else {
		file, err := os.Open(path)
		must(t, err)
		defer func() { must(t, file.Close()) }()
		compressed, err := gzip.NewReader(file)
		must(t, err)
		defer func() { must(t, compressed.Close()) }()
		archive := tar.NewReader(compressed)
		for {
			header, err := archive.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			must(t, err)
			add(header.Name, header.FileInfo().Mode(), archive)
		}
	}
	return entries
}

func TestPackageCLIRejectsUnsupportedOrMissingInputs(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "dist")
	writeTestFile(t, filepath.Join(root, "LICENSE"), "license fixture")
	writeTestFile(t, filepath.Join(directory, "orka"), "binary")
	for _, args := range [][]string{
		{"../v0.3.0", "linux", "amd64"}, {"dev", "linux", "amd64"},
		{testVersion, "linux", "386"}, {testVersion, "windows", "arm64"},
		{testVersion, "../linux", "amd64"},
	} {
		wantError(t, packageCLI(root, directory, args[0], args[1], args[2]), "")
	}
	wantError(t, packageCLI(root, directory, testVersion, windowsOS, "amd64"), "orka.exe")
	writeTestFile(t, filepath.Join(directory, "orka"), "")
	wantError(t, packageCLI(root, directory, testVersion, "linux", "amd64"), "empty")
	writeTestFile(t, filepath.Join(directory, "orka"), "binary")
	must(t, os.Remove(filepath.Join(root, "LICENSE")))
	wantError(t, packageCLI(root, directory, testVersion, "linux", "amd64"), "LICENSE")
}

func TestCLIChecksumsCoverExactlyTheFiveArchives(t *testing.T) {
	f := newReleaseFixture(t)
	writeTestFile(t, filepath.Join(f.directory, "unrelated.tar.gz"), "not a release asset")
	must(t, f.w.execute([]string{"cli-checksums", "bundle", testVersion}))
	var expected strings.Builder
	for _, target := range cliTargets {
		digest := sha256.Sum256([]byte("CLI archive " + target.os + "/" + target.arch))
		fmt.Fprintf(&expected, "%x  %s\n", digest, target.archiveName(testVersion))
	}
	if got := readTestFile(t, filepath.Join(f.directory, cliChecksumFile(testVersion))); got != expected.String() {
		t.Fatalf("unexpected checksums:\n%s", got)
	}
	if len(f.data.CLI) != 7 {
		t.Fatalf("candidate contains %d CLI assets, want 7", len(f.data.CLI))
	}
}

func TestBundleRejectsMissingOrChangedCLIAssets(t *testing.T) {
	f := newReleaseFixture(t)
	for _, asset := range f.data.CLI {
		t.Run(asset.File, func(t *testing.T) {
			path := filepath.Join(f.directory, asset.File)
			original := readTestFile(t, path)
			must(t, os.Remove(path))
			_, err := loadBundle(f.directory)
			wantError(t, err, asset.File)
			writeTestFile(t, path, "changed bytes")
			_, err = loadBundle(f.directory)
			wantError(t, err, "CLI")
			writeTestFile(t, path, original)
		})
	}
	for _, change := range []func(*candidateBundle){
		func(data *candidateBundle) { data.CLI = nil },
		func(data *candidateBundle) { data.CLI = data.CLI[:6] },
		func(data *candidateBundle) { data.CLI[0] = data.CLI[1] },
		func(data *candidateBundle) { data.CLI[0].File = "../outside" },
		func(data *candidateBundle) { data.CLI[0].SHA256 = strings.Repeat("c", 64) },
		func(data *candidateBundle) { data.CLI = append(data.CLI, data.CLI[0]) },
	} {
		data := f.data
		data.CLI = slices.Clone(f.data.CLI)
		change(&data)
		must(t, writeJSON(filepath.Join(f.directory, candidateFile), data))
		_, err := loadBundle(f.directory)
		wantError(t, err, "CLI assets changed or are incomplete")
	}
}

func TestRecomputedCLIChecksumsCannotReplaceQualifiedArchives(t *testing.T) {
	f := publicationFixture(t)
	writeTestFile(t, filepath.Join(f.directory, f.data.CLI[0].File), "different CLI archive")
	must(t, writeCLIChecksums(f.directory, testVersion))
	_, err := loadBundle(f.directory)
	wantError(t, err, "CLI assets changed")
	must(t, f.w.bundle(f.directory, testVersion, testSHA, "1"))
	_, err = f.w.verifyPublication(f.directory)
	wantError(t, err, "approved qualification artifacts changed")
}

func TestPublicationChecksCLIWorkflowIdentityAndRejectsInvalidSignature(t *testing.T) {
	f := publicationFixture(t)
	previous := f.command
	checked := false
	f.command = func(spec commandSpec) (commandResult, bool) {
		if spec.args[0] != "cosign" {
			return previous(spec)
		}
		checked = true
		checksums := filepath.Join(f.directory, cliChecksumFile(testVersion))
		want := []string{"cosign", "verify-blob", "--bundle", checksums + ".bundle",
			"--certificate-identity", "https://github.com/orka-agents/orka/.github/workflows/release.yml@refs/heads/release-0.2",
			"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
			"--certificate-github-workflow-sha", testSHA, checksums}
		if !slices.Equal(spec.args, want) {
			t.Fatalf("wrong checksum signature verification: %v", spec.args)
		}
		return commandResult{err: errors.New("signature verification failed")}, true
	}
	wantError(t, f.w.publish(f.directory), "CLI checksum signature verification failed")
	if !checked {
		t.Fatal("publication did not verify CLI provenance")
	}
	for _, request := range f.requests {
		if request.method != http.MethodGet {
			t.Fatalf("invalid CLI signature caused a publication mutation: %s", request.path)
		}
	}
}
