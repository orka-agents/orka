/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// stage prepares what the host container takes from Orka's image. It copies
// the connection token from its Secret mount into the memory-backed volume
// only the host mounts, and copies this binary into a volume the host mounts
// read-only, so the host can run "orka-host-agent init". It runs as an init
// container as the host's uid, so host images need no tools.
func stage(args []string) error {
	flags := flag.NewFlagSet("stage", flag.ContinueOnError)
	tokenFrom := flags.String("token-from", "", "the connection token file to copy")
	tokenTo := flags.String("token-to", "", "where to write the connection token")
	agentTo := flags.String("agent-to", "", "where to copy this binary")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *tokenFrom == "" || *tokenTo == "" || *agentTo == "" {
		return errors.New("--token-from, --token-to and --agent-to are required")
	}
	if err := stageToken(*tokenFrom, *tokenTo); err != nil {
		return err
	}
	return stageAgent(*agentTo)
}

func stageToken(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("open %s: %w", from, err)
	}
	defer func() { _ = source.Close() }()
	token, err := io.ReadAll(io.LimitReader(source, maxTokenBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", from, err)
	}
	if len(token) > maxTokenBytes || strings.TrimSpace(string(token)) == "" {
		return fmt.Errorf("%s does not hold a connection token", from)
	}
	// A token the host has not consumed yet is read-only; replace it.
	return replaceFile(to, 0o440, func(target io.Writer) error {
		_, err := target.Write(token)
		return err
	})
}

func stageAgent(to string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find this binary: %w", err)
	}
	source, err := os.Open(self)
	if err != nil {
		return fmt.Errorf("open %s: %w", self, err)
	}
	defer func() { _ = source.Close() }()
	return replaceFile(to, 0o555, func(target io.Writer) error {
		_, err := io.Copy(target, source)
		return err
	})
}

// replaceFile writes a new file at path with mode, replacing one an earlier
// run of the init container left behind.
func replaceFile(path string, mode fs.FileMode, write func(io.Writer) error) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	target, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := write(target); err != nil {
		_ = target.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return target.Close()
}
