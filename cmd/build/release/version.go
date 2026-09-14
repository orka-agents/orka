package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var versionedImages = []string{controllerImage, publisherImage, "agent-harness-wrapper", "ai-worker", "general-worker"}

func updateVersion(root, tag string) error {
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+(?:-(?:beta|rc)\.\d+)?$`).MatchString(tag) {
		return errors.New("usage: release update-version vX.Y.Z[-beta.N|-rc.N]")
	}
	version := strings.TrimPrefix(tag, "v")
	edits := map[string][]replacement{
		makefilePath: {{pattern: `^VERSION := .*$`, value: "VERSION := " + tag, count: 1}},
		chartInputPath: {
			{pattern: `^version: .*$`, value: "version: " + version, count: 1},
			{pattern: `^appVersion: .*$`, value: `appVersion: "` + tag + `"`, count: 1},
		},
		"config/manager/manager.yaml": {
			{pattern: `(ghcr\.io/orka-agents/orka/ai-worker:)[^\s]+`, value: "${1}" + version, count: 1},
			{pattern: `(ghcr\.io/orka-agents/orka/general-worker:)[^\s]+`, value: "${1}" + version, count: 1},
		},
		"config/manager/kustomization.yaml": {{pattern: `^(\s*newTag:)\s*.*$`, value: "${1} " + version, count: 2}},
	}
	for _, name := range versionedImages {
		edits[valuesInputPath] = append(edits[valuesInputPath], replacement{
			pattern: `^([ \t]+repository:[ \t]*` + regexp.QuoteMeta(imageRepository(name)) + `[ \t]*\n` +
				`(?:[ \t]*(?:#.*)?\n)*[ \t]+tag:)[ \t]*.*$`,
			value: `${1} "` + version + `"`, count: 1,
		})
	}
	// Check every expected field before writing any file. Preserve surrounding
	// bytes instead of reserializing YAML and changing comments or formatting.
	updates := make(map[string]string, len(edits))
	paths := make([]string, 0, len(edits))
	for name, replacements := range edits {
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		for _, edit := range replacements {
			pattern := regexp.MustCompile("(?m)" + edit.pattern)
			if count := len(pattern.FindAllStringIndex(content, -1)); count != edit.count {
				return fmt.Errorf("expected %d replacements in %s, found %d", edit.count, name, count)
			}
			content = pattern.ReplaceAllString(content, edit.value)
		}
		updates[path] = content
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := replaceFile(path, updates[path]); err != nil {
			return err
		}
	}
	return nil
}

func replaceFile(path, content string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".release-version-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := file.Chmod(info.Mode().Perm()); err != nil {
		_ = file.Close()
		return err
	}
	_, writeErr := file.WriteString(content)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
