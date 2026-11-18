package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderAuthProxyPreservesEOFAfterResponseHeaders(t *testing.T) {
	const keepalive = ": keepalive\n\n"
	proxy := newTestProxy(t, "http://upstream.example", testSharedProviderToken)
	observed := make(chan [2]error, 1)
	proxy.client.Transport = requestBodyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		// Like net/http's transport, send exactly Content-Length bytes before
		// checking for excess data with one final read from the request body.
		if _, err := io.CopyN(io.Discard, request.Body, request.ContentLength); err != nil {
			return nil, err
		}
		body := request.Body.(*boundedReadCloser)
		response := strings.NewReader(keepalive)
		checked := false
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"text/event-stream"}},
			ContentLength: -1,
			Body: io.NopCloser(requestBodyReadFunc(func(buffer []byte) (int, error) {
				if !checked {
					checked = true
					// ServeHTTP flushes response headers before reading this
					// response body. The real HTTP/1 server has now closed the
					// incoming body, before the transport's final EOF check.
					var extra [1]byte
					_, closedErr := body.ReadCloser.Read(extra[:])
					_, finalErr := body.Read(extra[:])
					observed <- [2]error{closedErr, finalErr}
					if finalErr != io.EOF {
						return 0, finalErr
					}
				}
				return response.Read(buffer)
			})),
		}, nil
	})
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/responses", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(authorizationHeader, "Bearer "+testSharedProviderToken)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	data, readErr := io.ReadAll(response.Body)
	checks := <-observed
	if checks[0] != http.ErrBodyReadAfterClose {
		t.Fatalf("underlying request body read = %v, want closed body", checks[0])
	}
	if checks[1] != io.EOF {
		t.Fatalf("final transport request body read = %v, want EOF", checks[1])
	}
	if readErr != nil || response.StatusCode != http.StatusOK || string(data) != keepalive {
		t.Fatalf("streamed response = status %d, %d bytes, error %v", response.StatusCode, len(data), readErr)
	}
}

func TestBoundedReadCloserTerminalReads(t *testing.T) {
	readFailure := errors.New("request body read failed")
	for _, test := range []struct {
		name  string
		limit int64
		reads []requestBodyReadStep
		want  []requestBodyReadStep
	}{
		{
			name: "final data with EOF", limit: 4,
			reads: []requestBodyReadStep{{"data", io.EOF}},
			want:  []requestBodyReadStep{{"data", io.EOF}, {"", io.EOF}},
		},
		{
			name: "EOF after final data", limit: 4,
			reads: []requestBodyReadStep{{"data", nil}, {"", io.EOF}},
			want:  []requestBodyReadStep{{"data", nil}, {"", io.EOF}, {"", io.EOF}},
		},
		{
			name: "non EOF errors remain unchanged", limit: 4,
			reads: []requestBodyReadStep{{"da", readFailure}, {"ta", nil}, {"", io.EOF}},
			want:  []requestBodyReadStep{{"da", readFailure}, {"ta", nil}, {"", io.EOF}},
		},
		{
			name: "oversize final data with EOF", limit: 3,
			reads: []requestBodyReadStep{{"data", io.EOF}},
			want:  []requestBodyReadStep{{"dat", errRequestBodyTooLarge}, {"", errRequestBodyTooLarge}},
		},
		{
			name: "excess data after exact limit", limit: 3,
			reads: []requestBodyReadStep{{"dat", nil}, {"a", io.EOF}},
			want:  []requestBodyReadStep{{"dat", nil}, {"", errRequestBodyTooLarge}, {"", errRequestBodyTooLarge}},
		},
		{
			name: "empty body at zero limit", limit: 0,
			reads: []requestBodyReadStep{{"", io.EOF}},
			want:  []requestBodyReadStep{{"", io.EOF}, {"", io.EOF}},
		},
		{
			name: "data at zero limit", limit: 0,
			reads: []requestBodyReadStep{{"a", io.EOF}},
			want:  []requestBodyReadStep{{"", errRequestBodyTooLarge}, {"", errRequestBodyTooLarge}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			next := 0
			source := requestBodyReadFunc(func(buffer []byte) (int, error) {
				if next == len(test.reads) {
					return 0, errors.New("unexpected read after terminal EOF")
				}
				step := test.reads[next]
				next++
				return copy(buffer, step.data), step.err
			})
			body := &boundedReadCloser{ReadCloser: io.NopCloser(source), remaining: test.limit}
			for i, want := range test.want {
				var buffer [8]byte
				n, err := body.Read(buffer[:])
				if string(buffer[:n]) != want.data || err != want.err {
					t.Fatalf("read %d = %q, %v; want %q, %v", i, buffer[:n], err, want.data, want.err)
				}
			}
			if next != len(test.reads) {
				t.Fatalf("underlying reads = %d, want %d", next, len(test.reads))
			}
		})
	}
}

type requestBodyReadStep struct {
	data string
	err  error
}

type requestBodyReadFunc func([]byte) (int, error)

func (read requestBodyReadFunc) Read(buffer []byte) (int, error) { return read(buffer) }

type requestBodyRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip requestBodyRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}
