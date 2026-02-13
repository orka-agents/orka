/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package client

import (
	"bufio"
	"io"
	"strings"
)

// SSEReader reads Server-Sent Events from a stream.
type SSEReader struct {
	scanner *bufio.Scanner
}

// NewSSEReader creates a new SSE reader from an io.Reader.
func NewSSEReader(r io.Reader) *SSEReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	return &SSEReader{scanner: s}
}

// Next reads the next SSE event from the stream.
// Returns the event and true if an event was read, or a zero value and false at EOF.
func (r *SSEReader) Next() (SSEEvent, bool) {
	var evt SSEEvent
	for r.scanner.Scan() {
		line := r.scanner.Text()

		if after, ok := strings.CutPrefix(line, "event: "); ok {
			evt.Event = after
			continue
		}

		if after, ok := strings.CutPrefix(line, "data: "); ok {
			evt.Data = after
			return evt, true
		}

		// blank line resets
		if line == "" {
			evt = SSEEvent{}
		}
	}
	return SSEEvent{}, false
}

// Err returns any error from the underlying scanner.
func (r *SSEReader) Err() error {
	return r.scanner.Err()
}
