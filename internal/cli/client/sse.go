/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package client

import (
	"bufio"
	"io"
	"strings"
)

type SSEReader struct {
	scanner *bufio.Scanner
}

func NewSSEReader(r io.Reader) *SSEReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	return &SSEReader{scanner: s}
}

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

		if line == "" {
			evt = SSEEvent{}
		}
	}
	return SSEEvent{}, false
}

func (r *SSEReader) Err() error {
	return r.scanner.Err()
}
