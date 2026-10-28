package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	maxEnforcementChartBytes = 8 << 20
	enforcementChartName     = "guardd"
)

func bundleEnforcementChart(destination, archivePath, expected string) error {
	if archivePath == "" && expected == "" {
		return nil
	}
	if archivePath == "" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(expected) {
		return errors.New("enforcement chart requires a local archive and its complete lowercase SHA256")
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open local enforcement chart: %w", err)
	}
	blob, err := io.ReadAll(io.LimitReader(file, maxEnforcementChartBytes+1))
	if err := errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("read local enforcement chart: %w", err)
	}
	if len(blob) > maxEnforcementChartBytes {
		return errors.New("local enforcement chart exceeds archive bound")
	}
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != expected {
		return errors.New("local enforcement chart SHA256 does not match")
	}
	version, err := enforcementChartVersion(blob)
	if err != nil {
		return err
	}
	chartPath := filepath.Join(destination, "Chart.yaml")
	contents, err := os.ReadFile(chartPath)
	if err != nil {
		return fmt.Errorf("read generated chart metadata: %w", err)
	}
	var chart map[string]any
	if err := yaml.Unmarshal(contents, &chart); err != nil {
		return fmt.Errorf("decode generated chart metadata: %w", err)
	}
	if chart == nil {
		return errors.New("generated chart metadata must be an object")
	}
	if dependencies, exists := chart["dependencies"]; exists && dependencies != nil {
		return errors.New("enforcement composition requires a dependency-free Orka chart input")
	}
	chart["dependencies"] = []map[string]string{
		{"name": enforcementChartName, "alias": "enforcement", "version": version},
	}
	contents, err = yaml.Marshal(chart)
	if err != nil {
		return fmt.Errorf("encode composed chart metadata: %w", err)
	}
	directory := filepath.Join(destination, "charts")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create vendored chart directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "guardd.tgz"), blob, 0o600); err != nil {
		return fmt.Errorf("write verified enforcement chart: %w", err)
	}
	if err := os.WriteFile(chartPath, contents, 0o600); err != nil {
		return fmt.Errorf("write composed chart metadata: %w", err)
	}
	return nil
}

func enforcementChartVersion(blob []byte) (string, error) {
	compressed, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return "", fmt.Errorf("open enforcement chart gzip: %w", err)
	}
	expanded, err := io.ReadAll(io.LimitReader(compressed, (16<<20)+1))
	if err := errors.Join(err, compressed.Close()); err != nil {
		return "", fmt.Errorf("expand enforcement chart: %w", err)
	}
	if len(expanded) > 16<<20 {
		return "", errors.New("expanded enforcement chart exceeds bound")
	}
	reader := tar.NewReader(bytes.NewReader(expanded))
	seen := make(map[string]bool)
	version := ""
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("decode enforcement chart archive: %w", err)
		}
		name := strings.TrimSuffix(header.Name, "/")
		if (name != enforcementChartName && !strings.HasPrefix(name, "guardd/")) ||
			path.Clean(name) != name || seen[name] ||
			name == "guardd/charts" || strings.HasPrefix(name, "guardd/charts/") ||
			(header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir) {
			return "", errors.New("enforcement archive must contain one regular Guardd chart tree")
		}
		seen[name] = true
		if name != "guardd/Chart.yaml" {
			continue
		}
		if header.Size > 16384 {
			return "", errors.New("enforcement chart metadata exceeds bound")
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return "", fmt.Errorf("read enforcement chart metadata: %w", err)
		}
		var metadata struct {
			APIVersion   string `yaml:"apiVersion"`
			Name         string `yaml:"name"`
			Version      string `yaml:"version"`
			Type         string `yaml:"type"`
			Dependencies []any  `yaml:"dependencies"`
		}
		if err := yaml.Unmarshal(body, &metadata); err != nil {
			return "", fmt.Errorf("decode enforcement chart metadata: %w", err)
		}
		if metadata.APIVersion != "v2" || metadata.Name != enforcementChartName ||
			metadata.Type != "application" || len(metadata.Dependencies) != 0 ||
			len(metadata.Version) > 128 ||
			!regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?(\+[A-Za-z0-9.-]+)?$`).MatchString(metadata.Version) {
			return "", errors.New("expected a standalone versioned Guardd v2 application chart")
		}
		version = metadata.Version
	}
	if version == "" {
		return "", errors.New("enforcement archive has no Guardd chart metadata")
	}
	return version, nil
}
