/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package aimockcapture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Validate the public schema without buffering the full capture on the test
// client either. Each decoded body is compared and then discarded.
func checkCaptureStream(reader io.Reader, expected Capture) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	for _, token := range []any{json.Delim('{'), "instanceId"} {
		if err := checkToken(decoder, token); err != nil {
			return err
		}
	}
	var instanceID string
	if err := decoder.Decode(&instanceID); err != nil || instanceID == "" || instanceID != expected.InstanceID {
		return errors.New("capture instance identity changed")
	}
	for _, token := range []any{"entries", json.Delim('[')} {
		if err := checkToken(decoder, token); err != nil {
			return err
		}
	}
	count := 0
	for decoder.More() {
		var entry Entry
		if err := decoder.Decode(&entry); err != nil || count >= len(expected.Entries) || entry != expected.Entries[count] {
			return errors.New("capture entry did not round-trip exactly")
		}
		count++
	}
	if count != len(expected.Entries) {
		return errors.New("capture entry count changed")
	}
	for _, token := range []any{json.Delim(']'), "rejected"} {
		if err := checkToken(decoder, token); err != nil {
			return err
		}
	}
	var rejected uint64
	if err := decoder.Decode(&rejected); err != nil || rejected != expected.Rejected {
		return errors.New("capture rejection count changed")
	}
	if err := checkToken(decoder, json.Delim('}')); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("capture contains trailing data")
	}
	return nil
}

func checkToken(decoder *json.Decoder, expected any) error {
	token, err := decoder.Token()
	if err != nil || token != expected {
		return errors.New("capture JSON wire shape changed")
	}
	return nil
}

type captureExportWriter struct {
	sink     io.Writer
	header   http.Header
	maxWrite int
	written  int64
	index    int
	started  chan<- int
	release  <-chan struct{}
	first    sync.Once
}

func (w *captureExportWriter) Header() http.Header { return w.header }

func (*captureExportWriter) WriteHeader(int) {}

func (w *captureExportWriter) Write(data []byte) (int, error) {
	w.first.Do(func() {
		if w.started != nil {
			w.started <- w.index
			<-w.release
		}
	})
	w.maxWrite = max(w.maxWrite, len(data))
	n, err := w.sink.Write(data)
	w.written += int64(n)
	return n, err
}

func TestCaptureExportEscapingHeavyAtBodyCapacity(t *testing.T) {
	h, _ := localFixture(t, limits{MaxBodyBytes, MaxTotalBodyBytes, MaxEntries})
	body := strings.Repeat("<>&", int(MaxBodyBytes)/3) + "<"
	for range MaxTotalBodyBytes / MaxBodyBytes {
		if post(t, h, body, false) != http.StatusOK {
			t.Fatal("cannot fill fixture to the total body capacity")
		}
	}
	if post(t, h, "rejected", false) != http.StatusServiceUnavailable {
		t.Fatal("fixture exceeded its total body capacity")
	}
	expected := h.snapshot()
	reader, pipe := io.Pipe()
	result := make(chan error, 1)
	go func() {
		defer func() { _ = reader.Close() }()
		result <- checkCaptureStream(reader, expected)
	}()
	writer := &captureExportWriter{sink: pipe, header: make(http.Header)}
	h.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, capturePath, nil))
	_ = pipe.Close()
	if err := <-result; err != nil {
		t.Fatal("near-capacity capture lost body bytes or changed its JSON schema")
	}
	if writer.maxWrite > int(MaxBodyBytes)+1024 {
		t.Fatal("export buffered multiple entries or HTML-expanded a body")
	}
	if writer.written < MaxTotalBodyBytes || writer.written > MaxTotalBodyBytes+32<<10 {
		t.Fatal("escaping-heavy capture was HTML-expanded or truncated")
	}
	if writer.Header().Get("Content-Type") != "application/json" || writer.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("capture response headers changed")
	}
}

func TestCaptureExportConcurrentReadersSerializedAndCancelable(t *testing.T) {
	h, _ := localFixture(t, limits{MaxBodyBytes, MaxTotalBodyBytes, MaxEntries})
	_ = post(t, h, strings.Repeat("<>&", 16<<10), false)
	before := h.snapshot()
	const readers = 8
	started := make(chan int, readers)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	results := make(chan error, readers)
	began := make(chan struct{}, readers)
	var wg sync.WaitGroup
	start := func(index int, expected Capture) {
		reader, pipe := io.Pipe()
		writer := &captureExportWriter{
			sink: pipe, header: make(http.Header), index: index, started: started, release: release,
		}
		wg.Go(func() {
			defer func() { _ = reader.Close() }()
			results <- checkCaptureStream(reader, expected)
		})
		wg.Go(func() {
			defer func() { _ = pipe.Close() }()
			began <- struct{}{}
			h.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, capturePath, nil))
		})
	}
	start(0, before)
	<-began
	if index := <-started; index != 0 {
		t.Error("unexpected first capture reader")
	}
	// A blocked export must not hold the store lock or stop model admission.
	admitted := make(chan int, 1)
	go func() { admitted <- post(t, h, "later request", false) }()
	select {
	case status := <-admitted:
		if status != http.StatusOK {
			t.Error("blocked export prevented model admission")
		}
	case <-time.After(time.Second):
		t.Error("blocked export held the capture store lock")
		releaseOnce.Do(func() { close(release) })
		<-admitted
	}
	after := h.snapshot()
	for index := 1; index < readers; index++ {
		start(index, after)
	}
	for range readers - 1 {
		<-began
	}
	select {
	case <-started:
		t.Error("concurrent export allocated or wrote before the active export finished")
	case <-time.After(100 * time.Millisecond):
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan struct{})
	canceledWriter := &captureExportWriter{sink: io.Discard, header: make(http.Header)}
	go func() {
		h.ServeHTTP(canceledWriter, httptest.NewRequest(http.MethodGet, capturePath, nil).WithContext(ctx))
		close(canceled)
	}()
	cancel()
	select {
	case <-canceled:
		if canceledWriter.written != 0 {
			t.Error("canceled waiting reader produced a capture")
		}
	case <-time.After(time.Second):
		t.Error("canceled reader remained queued behind the active export")
	}
	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	for range readers {
		if err := <-results; err != nil {
			t.Error("concurrent capture lost a snapshot or changed the wire schema")
		}
	}
	if h.snapshot().Rejected != after.Rejected {
		t.Fatal("capture readers changed the model rejection count")
	}
	// Ensure a disconnected reader releases the export gate for subsequent GETs.
	h.ServeHTTP(&captureExportWriter{sink: failingCaptureSink{}, header: make(http.Header)},
		httptest.NewRequest(http.MethodGet, capturePath, nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, capturePath, nil))
	if err := checkCaptureStream(w.Body, after); err != nil {
		t.Fatal("failed export retained its gate or corrupted later snapshots")
	}
}

type failingCaptureSink struct{}

func (failingCaptureSink) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
