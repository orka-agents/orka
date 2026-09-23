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

// watchRender produces one frame of a watched view. The frame is printed
// only when it differs from the previous one; done stops the loop.
type watchRender func(ctx context.Context) (frame string, done bool, err error)

// watchLoop reprints a view whenever it changes, with a timestamp line
// between frames, until the view reports it is done or the user interrupts
// with Ctrl-C. An interrupt is not an error.
func watchLoop(ctx context.Context, out io.Writer, interval time.Duration, render watchRender) error {
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
		frame, done, err := render(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if frame != last {
			if !first {
				fmt.Fprintf(out, "\n--- %s\n", time.Now().Format(time.RFC3339)) //nolint:errcheck
			}
			fmt.Fprint(out, frame) //nolint:errcheck
			if !strings.HasSuffix(frame, "\n") {
				fmt.Fprintln(out) //nolint:errcheck
			}
			last = frame
			first = false
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
