/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/orka-agents/orka/hack/commentlint/scan"
)

const usage = `usage: go run ./hack/commentlint <command> [flags] [paths...]

commands:
  list [-context N] [paths...]   print deletable comments as JSON lines
  stats [paths...]               summarize deletable comment lines
  delete [-from file] [file:line ...]
                                 delete the comments starting at file:line;
                                 -from reads JSON lines with "file" and "line"
                                 and skips "category":"functional"
  check                          compare per-file counts with the baseline
  baseline [-init]               rewrite the baseline; refuses any increase
                                 unless -init
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	root, err := repoRoot()
	if err == nil {
		err = run(root, os.Args[1], os.Args[2:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func run(root, command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	contextLines := fs.Int("context", 3, "code lines of context after each comment")
	from := fs.String("from", "", "JSON lines file of deletion targets")
	initBaseline := fs.Bool("init", false, "write the baseline even if counts increase")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch command {
	case "list":
		comments, err := scan.Inventory(root, fs.Args())
		if err != nil {
			return err
		}
		scan.WithContext(root, comments, *contextLines)
		enc := json.NewEncoder(os.Stdout)
		for _, c := range comments {
			if err := enc.Encode(c); err != nil {
				return err
			}
		}
		return nil
	case "stats":
		return stats(root, fs.Args())
	case "delete":
		targets, err := parseTargets(*from, fs.Args())
		if err != nil {
			return err
		}
		if err := scan.Delete(root, targets); err != nil {
			return err
		}
		fmt.Printf("deleted %d comments; run make comment-baseline to lock in the reduction\n", len(targets))
		return nil
	case "check":
		return check(root)
	case "baseline":
		return baseline(root, *initBaseline)
	}
	return fmt.Errorf("unknown command %q\n%s", command, usage)
}

func parseTargets(from string, args []string) ([]scan.Target, error) {
	var targets []scan.Target
	if from != "" {
		f, err := os.Open(from)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var t scan.Target
			if err := json.Unmarshal([]byte(line), &t); err != nil {
				return nil, fmt.Errorf("%s: %w", from, err)
			}
			if t.Category == "functional" {
				continue
			}
			targets = append(targets, t)
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	for _, arg := range args {
		file, line, ok := strings.Cut(arg, ":")
		n, err := strconv.Atoi(line)
		if !ok || err != nil {
			return nil, fmt.Errorf("target %q is not file:line", arg)
		}
		targets = append(targets, scan.Target{File: file, Line: n})
	}
	if len(targets) == 0 {
		return nil, errors.New("no deletion targets")
	}
	return targets, nil
}

func stats(root string, prefixes []string) error {
	comments, err := scan.Inventory(root, prefixes)
	if err != nil {
		return err
	}
	type bucket struct{ groups, lines int }
	byLang := map[string]*bucket{}
	byKind := map[string]*bucket{}
	files := map[string]bool{}
	total := bucket{}
	for _, c := range comments {
		lang := strings.TrimPrefix(filepath.Ext(c.File), ".")
		if lang == "" {
			lang = "script"
		}
		for key, m := range map[string]map[string]*bucket{lang: byLang, c.Kind: byKind} {
			if m[key] == nil {
				m[key] = &bucket{}
			}
			m[key].groups++
			m[key].lines += c.Lines
		}
		files[c.File] = true
		total.groups++
		total.lines += c.Lines
	}
	fmt.Printf("files with comments: %d\ncomment groups: %d\ncomment lines: %d\n", len(files), total.groups, total.lines)
	for name, m := range map[string]map[string]*bucket{"by extension": byLang, "by kind": byKind} {
		fmt.Println(name + ":")
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-9s groups=%-6d lines=%d\n", k, m[k].groups, m[k].lines)
		}
	}
	return nil
}

func check(root string) error {
	comments, err := scan.Inventory(root, nil)
	if err != nil {
		return err
	}
	base, err := scan.LoadBaseline(root)
	if err != nil {
		return err
	}
	violations := scan.Compare(scan.Counts(comments), base)
	if len(violations) == 0 {
		return nil
	}
	for _, v := range violations {
		fmt.Fprintln(os.Stderr, v)
	}
	return fmt.Errorf("%d files differ from %s", len(violations), scan.BaselinePath)
}

func baseline(root string, init bool) error {
	comments, err := scan.Inventory(root, nil)
	if err != nil {
		return err
	}
	counts := scan.Counts(comments)
	if !init {
		base, err := scan.LoadBaseline(root)
		if err != nil {
			return err
		}
		var increases []string
		for _, v := range scan.Compare(counts, base) {
			if v.Increase {
				increases = append(increases, v.String())
			}
		}
		if len(increases) > 0 {
			return fmt.Errorf("the baseline only ratchets down:\n%s", strings.Join(increases, "\n"))
		}
	}
	return scan.WriteBaseline(root, counts)
}
