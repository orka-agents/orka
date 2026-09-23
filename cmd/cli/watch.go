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

// watchFrame is one rendering of a watched view. Key is a stable
// description of the state that matters (names and phases, stage counts),
// so a frame is reprinted only when that state changes and not when a
// relative age such as "12s" ticks over.
type watchFrame struct {
	Key  string
	Text string
	Done bool
}

// watchRender produces one frame of a watched view.
type watchRender func(ctx context.Context) (watchFrame, error)

// watchSeparator returns what is printed between two frames. Table output
// gets a timestamp line for people; json frames are concatenated documents
// a streaming decoder reads one after another, and yaml frames are
// separated by a bare document marker.
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

// watchLoop reprints a view whenever its state key changes, with a
// format-aware separator between frames, until the view reports it is done
// or the user interrupts with Ctrl-C. An interrupt is not an error.
func watchLoop(ctx context.Context, out io.Writer, interval time.Duration, format string, render watchRender) error {
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
			return nil
		}
		if err != nil {
			return err
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
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
