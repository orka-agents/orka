package conformance_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/conformance"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
)

// Delay only bytes after the original accepted frame. The fixture's complete
// stream and ownership remain unchanged; neither endpoint nor client replays it.
type delayedConformanceBody struct {
	io.ReadCloser
	first  *bytes.Reader
	rest   *bufio.Reader
	ctx    context.Context
	delay  time.Duration
	waited bool
}

func (b *delayedConformanceBody) Read(p []byte) (int, error) {
	if b.first.Len() > 0 {
		return b.first.Read(p)
	}
	if !b.waited {
		b.waited = true
		timer := time.NewTimer(b.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	return b.rest.Read(p)
}

func TestCheckProbeTimeoutSeparatesOriginalStreamFromControls(t *testing.T) {
	for _, test := range []struct {
		name         string
		probeTimeout time.Duration
		delay        time.Duration
		wantPassed   bool
	}{
		{name: "longer lifecycle", probeTimeout: 2 * time.Second, delay: 350 * time.Millisecond, wantPassed: true},
		{name: "zero retains control budget", delay: 350 * time.Millisecond},
		{name: "whole probe expires", probeTimeout: 350 * time.Millisecond, delay: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, config := testTargetAndConfig(t)
			target.ProbeLifecycle = true
			target.ControlTimeout = 200 * time.Millisecond
			target.ProbeTimeout = test.probeTimeout
			fixture, err := conformancetest.NewServer(config)
			if err != nil {
				t.Fatal(err)
			}
			defer fixture.Close()
			upstream, err := url.Parse(fixture.URL())
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			proxy.Transport = harnessv2.NewProxylessTransport()
			proxy.FlushInterval = -1
			var delayed atomic.Bool
			proxy.ModifyResponse = func(response *http.Response) error {
				if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != harnessv2.NDJSONMediaType ||
					!delayed.CompareAndSwap(false, true) {
					return nil
				}
				reader := bufio.NewReader(response.Body)
				first, err := reader.ReadBytes('\n')
				if err != nil {
					return err
				}
				response.Body = &delayedConformanceBody{ReadCloser: response.Body, first: bytes.NewReader(first), rest: reader,
					ctx: response.Request.Context(), delay: test.delay}
				response.ContentLength = -1
				response.Header.Del("Content-Length")
				return nil
			}
			server := httptest.NewServer(proxy)
			defer server.Close()
			target.BaseURL = server.URL
			started := time.Now()
			result := conformance.Check(t.Context(), target)
			if result.Passed != test.wantPassed || !delayed.Load() {
				t.Fatalf("original stream outcome changed: passed=%v, message=%s", result.Passed, result.Message)
			}
			counts := fixture.Counts()
			if test.wantPassed {
				if time.Since(started) < test.delay || counts.PromptStarts != 2 || counts.SessionDeletes != 2 {
					t.Fatal("long stream did not complete both original lifecycle sessions")
				}
			} else if counts.PromptStarts != 1 || !strings.Contains(result.Message, "workspace probe prompt stream") {
				t.Fatal("expired stream was replayed or failed outside the original workspace stream")
			}
		})
	}
}

func TestCheckLongProbeKeepsControlHTTPBounds(t *testing.T) {
	for _, path := range []string{harnessv2.HealthPath, harnessv2.CapabilitiesPath, harnessv2.StatusPath} {
		t.Run(path, func(t *testing.T) {
			target, config := testTargetAndConfig(t)
			target.ControlTimeout = 200 * time.Millisecond
			target.ProbeTimeout = 2 * time.Second
			fixture, err := conformancetest.NewServer(config)
			if err != nil {
				t.Fatal(err)
			}
			defer fixture.Close()
			upstream, err := url.Parse(fixture.URL())
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			proxy.Transport = harnessv2.NewProxylessTransport()
			var reached atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == path {
					reached.Store(true)
					<-request.Context().Done()
					return
				}
				proxy.ServeHTTP(w, request)
			}))
			defer server.Close()
			target.BaseURL = server.URL
			started := time.Now()
			result := conformance.Check(t.Context(), target)
			elapsed := time.Since(started)
			if result.Passed || !reached.Load() || elapsed < target.ControlTimeout || elapsed > target.ProbeTimeout/2 {
				t.Fatalf("ordinary or direct HTTP control used the lifecycle budget: passed=%v, elapsed=%s", result.Passed, elapsed)
			}
		})
	}
}

func TestCheckRejectsNegativeProbeTimeoutBeforeTransport(t *testing.T) {
	target, _ := testTargetAndConfig(t)
	target.BaseURL = "http://conformance.invalid"
	target.ProbeTimeout = -time.Second
	result := conformance.Check(t.Context(), target)
	if result.Passed || result.Message != "probe timeout must not be negative" {
		t.Fatalf("invalid probe budget was not rejected before transport: %s", result.Message)
	}
}
