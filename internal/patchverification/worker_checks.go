package patchverification

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func validateWorkerCheckFiles(frozenFiles []FrozenFile, checksDir string) (map[string]FrozenFile, error) {
	files := make(map[string]FrozenFile)
	directories := map[string]bool{".": true}
	for _, file := range frozenFiles {
		_, duplicate := files[file.Path]
		if file.Path == "" || file.Path == "." || path.Clean(file.Path) != file.Path || strings.HasPrefix(file.Path, "../") || path.IsAbs(file.Path) || strings.ContainsRune(file.Path, '\\') || duplicate || file.Digest != Digest(file.Content) {
			return nil, errors.New("invalid frozen check file path")
		}
		files[file.Path] = file
		for directory := path.Dir(file.Path); directory != "."; directory = path.Dir(directory) {
			directories[directory] = true
		}
	}
	seen := 0
	if err := filepath.WalkDir(checksDir, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("frozen check files are unavailable")
		}
		relative, err := filepath.Rel(checksDir, filename)
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("frozen check files cannot be symlinked")
		}
		if entry.IsDir() && directories[relative] {
			return nil
		}
		frozen, declared := files[relative]
		info, err := entry.Info()
		if err != nil || !declared || !info.Mode().IsRegular() || info.Size() != int64(len(frozen.Content)) || (info.Mode().Perm()&0111 != 0) != frozen.Executable {
			return errors.New("staged checks differ from the frozen file inventory")
		}
		opened, err := os.Open(filename)
		if err != nil {
			return errors.New("frozen check file could not be opened")
		}
		content, readErr := io.ReadAll(io.LimitReader(opened, int64(len(frozen.Content))+1))
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil || len(content) != len(frozen.Content) || Digest(content) != frozen.Digest {
			return errors.New("staged check content does not match its frozen digest")
		}
		seen++
		return nil
	}); err != nil {
		return nil, err
	}
	if seen != len(files) {
		return nil, errors.New("a frozen check file is missing")
	}
	return files, nil
}

func validateWorkerCommands(manifest Manifest, files map[string]FrozenFile, checksDir string) error {
	commands := make([][]string, 0, len(manifest.Checks)+len(manifest.Environment.Services))
	for _, check := range manifest.Checks {
		if len(check.Stdin) > MaxOutputBytes {
			return errors.New("check input exceeds the worker limit")
		}
		commands = append(commands, check.Command)
	}
	for _, service := range manifest.Environment.Services {
		commands = append(commands, service.Command)
	}
	for _, command := range commands {
		if len(command) == 0 {
			return errors.New("commands must execute declared frozen files under /checks")
		}
		_, declared := files[strings.TrimPrefix(command[0], "/checks/")]
		if !strings.HasPrefix(command[0], "/checks/") || !declared {
			return errors.New("commands must execute declared frozen files under /checks")
		}
		for _, argument := range command {
			if len(argument) > 8192 || strings.ContainsRune(argument, 0) {
				return errors.New("invalid frozen command argument")
			}
		}
		filename := filepath.Join(checksDir, strings.TrimPrefix(command[0], "/checks/"))
		resolved, err := filepath.EvalSymlinks(filename)
		if err != nil || resolved != filename {
			return errors.New("frozen executable is unavailable or symlinked")
		}
		info, err := os.Stat(filename)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return errors.New("frozen check file is not executable")
		}
	}
	return nil
}
