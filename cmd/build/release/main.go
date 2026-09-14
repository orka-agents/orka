// Command release prepares, qualifies, and publishes one immutable Orka release candidate.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	root, err := os.Getwd()
	if err == nil {
		err = newWorkflow(root).execute(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Release stopped:", err)
		os.Exit(1)
	}
}

func (w *workflow) execute(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: release " +
			"<prepare|update-version|candidate|bundle|download|check-bundle|check-environment|qualify|publish> [args]")
	}
	name, values := args[0], args[1:]
	counts := map[string]int{
		"prepare": 1, "update-version": 1, "candidate": 2, "bundle": 4, "download": 5,
		"check-bundle": 1, "check-environment": 2, "qualify": 1, "publish": 1,
	}
	count, found := counts[name]
	if !found || len(values) != count {
		return fmt.Errorf("invalid arguments for release command %q", name)
	}
	// Resolve artifact paths before preparation can change the checkout.
	path := func(value string) string {
		if filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(w.root, value)
	}
	switch name {
	case "prepare":
		return w.prepare(values[0])
	case "update-version":
		return updateVersion(w.root, values[0])
	case "candidate":
		return w.validateCandidate(values[0], values[1])
	case "bundle":
		return w.bundle(path(values[0]), values[1], values[2], values[3])
	case "download":
		return w.downloadBundle(values[0], values[1], values[2], values[3], path(values[4]))
	case "check-bundle":
		_, err := loadBundle(path(values[0]))
		return err
	case "check-environment":
		return w.checkEnvironment(values[0], values[1])
	case "qualify":
		return w.qualify(path(values[0]))
	case "publish":
		return w.publish(path(values[0]))
	default:
		return fmt.Errorf("unknown release command %q", name)
	}
}
