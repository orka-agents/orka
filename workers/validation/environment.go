//go:build linux

package main

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

func childEnvironment(variables map[string]string, privateDirectory string, fixture bool) ([]string, error) {
	values := map[string]string{
		"PATH": "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME": privateDirectory, "TMPDIR": privateDirectory,
		"LANG": "C", "LC_ALL": "C",
		"GOCACHE":     filepath.Join(privateDirectory, "go-build"),
		"GOMODCACHE":  filepath.Join(privateDirectory, "go-mod"),
		"GOPATH":      filepath.Join(privateDirectory, "go"),
		"GOMAXPROCS":  "2",
		"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off",
		"PYTHONDONTWRITEBYTECODE": "1", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
	}
	for name, value := range variables {
		switch name {
		case "CGO_ENABLED", "GOEXPERIMENT", "GOMAXPROCS", "TZ", "LANG", "LC_ALL":
		default:
			return nil, errors.New("unsupported frozen environment variable")
		}
		if len(value) > 1024 || strings.ContainsRune(value, 0) {
			return nil, errors.New("invalid frozen environment variable")
		}
		values[name] = value
	}
	if fixture {
		values["ORKA_LISTEN_FD"] = "3"
	}
	keys := make([]string, 0, len(values))
	for name := range values {
		keys = append(keys, name)
	}
	slices.Sort(keys)
	result := make([]string, 0, len(values))
	for _, name := range keys {
		result = append(result, name+"="+values[name])
	}
	return result, nil
}
