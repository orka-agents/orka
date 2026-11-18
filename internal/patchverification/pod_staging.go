package patchverification

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// StagePodInput materializes only the validated archive and frozen check files.
// Its destinations must be empty, private, runner-owned directories. No child
// process may run until this function succeeds.
func StagePodInput(ctx context.Context, input PodInput, sourceDirectory, checksDirectory string) error {
	if _, err := ValidatePodInput(input); err != nil {
		return errors.New("invalid frozen pod input")
	}
	content, err := json.Marshal(input.Manifest)
	if err != nil || len(content) > MaxManifestBytes || len(input.Manifest.Files) > MaxFrozenFiles ||
		len(input.Manifest.Environment.Services) > MaxServices {
		return errors.New("frozen pod input exceeds staging limits")
	}
	for _, directory := range []string{sourceDirectory, checksDirectory} {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" {
			return errors.New("pod staging directory is unavailable")
		}
		resolved, err := filepath.EvalSymlinks(directory)
		if err != nil || resolved != directory {
			return errors.New("pod staging directory cannot be symlinked")
		}
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			return errors.New("pod staging directories must be empty")
		}
	}
	if sourcePathsOverlap(sourceDirectory, checksDirectory) {
		return errors.New("pod staging directories must be separate")
	}
	for name, value := range input.Manifest.Environment.Variables {
		switch name {
		case "CGO_ENABLED", "GOEXPERIMENT", "GOMAXPROCS", "TZ", "LANG", "LC_ALL":
		default:
			return errors.New("unsupported frozen pod environment variable")
		}
		if len(value) > 1024 || strings.ContainsRune(value, 0) {
			return errors.New("invalid frozen pod environment variable")
		}
	}
	if err := stagePodArchive(ctx, input.Archive, sourceDirectory); err != nil {
		return err
	}
	for _, file := range input.Manifest.Files {
		if ctx.Err() != nil || !sourceValidPath(file.Path) {
			return errors.New("invalid frozen check path or canceled staging")
		}
		mode := os.FileMode(0444)
		if file.Executable {
			mode = 0555
		}
		if sourceWriteSnapshot(checksDirectory, file.Path, file.Content, mode) != nil {
			return errors.New("frozen checks could not be staged")
		}
	}
	if sourceSealDirectories(checksDirectory) != nil {
		return errors.New("frozen checks could not be sealed")
	}
	files, err := validateWorkerCheckFiles(input.Manifest.Files, checksDirectory)
	commands := input.Manifest
	commands.Checks = slices.Clone(commands.Checks)
	for index := range commands.Checks {
		commands.Checks[index].Command = CheckExecutable(commands.Checks[index])
	}
	if err != nil || validateWorkerCommands(commands, files, checksDirectory) != nil {
		return errors.New("frozen pod commands or files are invalid")
	}
	return nil
}

func stagePodArchive(ctx context.Context, archive []byte, sourceDirectory string) error {
	// Extract on the source volume itself, so promotion also works when the
	// source and input EmptyDirs are different filesystems.
	staged := filepath.Join(sourceDirectory, ".orka-source-"+rand.Text())
	if err := sourceExtractArchive(ctx, archive, staged); err != nil {
		return errors.New("source archive could not be safely staged")
	}
	entries, err := os.ReadDir(staged)
	if err != nil {
		return errors.New("source snapshot inventory could not be read")
	}
	if os.Chmod(staged, 0700) != nil {
		return errors.New("source snapshot promotion permissions could not be set")
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return errors.New("pod staging was canceled")
		}
		destination := filepath.Join(sourceDirectory, entry.Name())
		if _, err := os.Lstat(destination); !os.IsNotExist(err) {
			return errors.New("source snapshot has conflicting paths")
		}
		oldPath := filepath.Join(staged, entry.Name())
		// Moving a directory updates its ".." entry. The observer intentionally
		// has no CAP_DAC_OVERRIDE, so temporarily permit its owner to do that.
		if entry.IsDir() && os.Chmod(oldPath, 0700) != nil {
			return errors.New("source snapshot parent permissions could not be set")
		}
		if os.Rename(oldPath, destination) != nil {
			return errors.New("source snapshot could not be promoted")
		}
	}
	if os.Remove(staged) != nil || sourceSealDirectories(sourceDirectory) != nil {
		return errors.New("source snapshot could not be sealed")
	}
	return nil
}
