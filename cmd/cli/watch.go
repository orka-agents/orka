/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type watchFrame struct {
	Key  string
	Text string
	Done bool
}

type watchRender func(ctx context.Context) (watchFrame, error)

func watchSeparator(format string) func() string {
	switch format {
	case outputJSON:
		return func() string { return "" }
	case outputYAML:
		return func() string { return "---\n" }
	default:
		return func() string { return "\n--- " + time.Now().Format(time.RFC3339) + "\n" }
	}
}

func watchLoop(ctx context.Context, out io.Writer, interval time.Duration, format string, render watchRender) (bool, error) {
	separator := watchSeparator(format)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	last := ""
	first := true
	for {
		frame, err := render(ctx)
		if ctx.Err() != nil {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if frame.Text == "" && !frame.Done {
			select {
			case <-ctx.Done():
				return false, nil
			case <-ticker.C:
			}
			continue
		}
		if first || frame.Key != last {
			if !first {
				fmt.Fprint(out, separator()) //nolint:errcheck
			}
			fmt.Fprint(out, frame.Text) //nolint:errcheck
			if !strings.HasSuffix(frame.Text, "\n") {
				fmt.Fprintln(out) //nolint:errcheck
			}
			last = frame.Key
			first = false
		}
		if frame.Done {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-ticker.C:
		}
	}
}
