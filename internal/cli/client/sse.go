/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

const (
	sseInitialBufferBytes = 256 * 1024
	// Task-log producers accept log lines up to 1 MiB, so the client must
	// also allow the SSE field prefix around a maximum-sized data line.
	sseMaxDataBytes    = 1024 * 1024
	sseMaxLineBytes    = len("data: ") + sseMaxDataBytes
	sseScannerMaxBytes = sseMaxLineBytes + len("\ufeff") + 2
)

// SSEReader reads Server-Sent Events from a stream.
type SSEReader struct {
	scanner   *bufio.Scanner
	err       error
	done      bool
	firstLine bool
}

// NewSSEReader creates a new SSE reader from an io.Reader.
func NewSSEReader(r io.Reader) *SSEReader {
	s := bufio.NewScanner(r)
	s.Split(newSSELineSplitter())
	s.Buffer(make([]byte, 0, sseInitialBufferBytes), sseScannerMaxBytes)
	return &SSEReader{scanner: s, firstLine: true}
}

// Next reads the next SSE event from the stream.
// Returns the event and true if an event was read, or a zero value and false at EOF.
func (r *SSEReader) Next() (SSEEvent, bool) {
	if r.done {
		return SSEEvent{}, false
	}

	var eventType string
	var data strings.Builder
	dataLines := 0

	for r.scanner.Scan() {
		line := r.scanner.Text()
		if r.firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			r.firstLine = false
		}
		if len(line) > sseMaxLineBytes {
			r.err = fmt.Errorf("SSE line exceeds %d bytes", sseMaxLineBytes)
			r.done = true
			return SSEEvent{}, false
		}

		if line == "" {
			if dataLines == 0 {
				eventType = ""
				continue
			}
			return SSEEvent{Event: eventType, Data: data.String()}, true
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}

		switch field {
		case "event":
			eventType = value
		case "data":
			additionalBytes := len(value)
			if dataLines > 0 {
				additionalBytes++
			}
			if data.Len()+additionalBytes > sseMaxDataBytes {
				r.err = fmt.Errorf("SSE event data exceeds %d bytes", sseMaxDataBytes)
				r.done = true
				return SSEEvent{}, false
			}
			if dataLines > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			dataLines++
		}
	}

	if err := r.scanner.Err(); err != nil {
		r.err = err
	}
	// The SSE parsing algorithm discards an event that was not terminated by
	// a blank line before EOF.
	r.done = true
	return SSEEvent{}, false
}

// Err returns any error from the underlying scanner or SSE parser.
func (r *SSEReader) Err() error {
	if r.err != nil {
		return r.err
	}
	return r.scanner.Err()
}

func newSSELineSplitter() bufio.SplitFunc {
	skipLeadingLF := false
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if skipLeadingLF && len(data) > 0 {
			skipLeadingLF = false
			if data[0] == '\n' {
				return 1, nil, nil
			}
		}
		for i, b := range data {
			switch b {
			case '\n':
				return i + 1, data[:i], nil
			case '\r':
				if i+1 < len(data) && data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				if i+1 == len(data) && !atEOF {
					skipLeadingLF = true
				}
				return i + 1, data[:i], nil
			}
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
}
