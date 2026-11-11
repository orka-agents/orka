//go:build linux

package main

import (
	"bytes"
	"errors"
	"sync"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

type outputCapture struct {
	mu        sync.Mutex
	content   []byte
	truncated bool
	readyText []byte
	ready     chan struct{}
	overflow  func()
}

func newCapture(ready string, overflow func()) *outputCapture {
	return &outputCapture{readyText: []byte(ready), ready: make(chan struct{}), overflow: overflow}
}

func (capture *outputCapture) Write(content []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	remaining := pv.MaxOutputBytes - len(capture.content)
	capture.content = append(capture.content, content[:min(remaining, len(content))]...)
	if len(content) > remaining && !capture.truncated {
		capture.truncated = true
		if capture.overflow != nil {
			capture.overflow()
		}
	}
	if len(capture.readyText) != 0 && bytes.HasPrefix(capture.content, capture.readyText) {
		capture.readyText = nil
		close(capture.ready)
	}
	// Always drain the pipe; truncation must not deadlock the child or observer.
	return len(content), nil
}

func (capture *outputCapture) snapshot() ([]byte, bool) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return bytes.Clone(capture.content), capture.truncated
}

func captureEvidence(evidence *pv.ExecutionEvidence, output *outputCapture) pv.CapturedOutput {
	content, truncated := output.snapshot()
	digest := pv.Digest(content)
	evidence.Blobs[digest] = content
	evidence.Observation.OutputTruncated = evidence.Observation.OutputTruncated || truncated
	return pv.CapturedOutput{Digest: digest, Bytes: len(content), Truncated: truncated}
}

func captureHTTPDiagnostics(evidence *pv.ExecutionEvidence, server *childProcess) error {
	capture := newCapture("", nil)
	for _, stream := range []struct {
		label  string
		output *outputCapture
	}{{"subject stdout:\n", server.stdout}, {"\nsubject stderr:\n", server.stderr}} {
		content, truncated := stream.output.snapshot()
		evidence.Observation.OutputTruncated = evidence.Observation.OutputTruncated || truncated
		_, _ = capture.Write([]byte(stream.label))
		_, _ = capture.Write(content)
	}
	output := captureEvidence(evidence, capture)
	evidence.Observation.StderrDigest, evidence.Observation.StderrBytes = output.Digest, output.Bytes
	if evidence.Observation.OutputTruncated {
		return errors.New("HTTP subject diagnostics exceeded the capture limit")
	}
	return nil
}
