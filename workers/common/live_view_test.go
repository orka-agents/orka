/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orka-agents/orka/internal/workerenv"
)

// fakeBrowser serves DevTools the way Chromium does: /json/list names the
// pages, and each page's WebSocket answers Page.startScreencast with frames,
// sending the next one only after the previous was acknowledged.
type fakeBrowser struct {
	server *httptest.Server
	frames [][]byte
	pageWS string
	mu     sync.Mutex
	acks   int
}

func newFakeBrowser(t *testing.T, frames [][]byte) *fakeBrowser {
	t.Helper()
	b := &fakeBrowser{frames: frames}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"type": "page", "webSocketDebuggerUrl": b.pageWS},
			{"type": "service_worker", "webSocketDebuggerUrl": b.pageWS + "-worker"},
		})
	})
	mux.HandleFunc("/devtools/page/1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		if _, msg, err := conn.Read(ctx); err != nil || !strings.Contains(string(msg), "Page.startScreencast") {
			return
		}
		for i, frame := range b.frames {
			event := fmt.Sprintf(`{"method":"Page.screencastFrame","params":{"data":%q,"sessionId":%d}}`,
				base64.StdEncoding.EncodeToString(frame), i+1)
			if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
				return
			}
			if _, msg, err := conn.Read(ctx); err != nil || !strings.Contains(string(msg), "Page.screencastFrameAck") {
				return
			}
			b.mu.Lock()
			b.acks++
			b.mu.Unlock()
		}
		<-ctx.Done()
	})
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	b.pageWS = "ws://" + b.server.Listener.Addr().String() + "/devtools/page/1"
	return b
}

func (b *fakeBrowser) port(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(b.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

type postedFrame struct {
	path, contentType, auth string
	body                    []byte
}

// liveViewController records frame posts and answers them with status.
func liveViewController(t *testing.T, status int) (*httptest.Server, func() []postedFrame) {
	t.Helper()
	return liveViewControllerFunc(t, func(int) int { return status })
}

// liveViewControllerFunc answers the nth post (from 0) with status(n).
func liveViewControllerFunc(t *testing.T, status func(n int) int) (*httptest.Server, func() []postedFrame) {
	t.Helper()
	var mu sync.Mutex
	var posts []postedFrame
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only frame posts count: anything else probing a local port is not the worker.
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/internal/v1/live/") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(posts)
		posts = append(posts, postedFrame{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), body})
		mu.Unlock()
		w.WriteHeader(status(n))
	}))
	t.Cleanup(server.Close)
	return server, func() []postedFrame {
		mu.Lock()
		defer mu.Unlock()
		return append([]postedFrame(nil), posts...)
	}
}

// setUpLiveView points the live view at a profile directory naming port and
// at controllerURL, with fast intervals.
func setUpLiveView(t *testing.T, port, controllerURL string) {
	t.Helper()
	profiles := t.TempDir()
	profile := filepath.Join(profiles, "playwright_chromiumdev_profile-test")
	if err := os.Mkdir(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	portFile := filepath.Join(profile, "DevToolsActivePort")
	if err := os.WriteFile(portFile, []byte(port+"\n/devtools/browser/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("callback-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := func(profiles string, frame, scan, keepalive time.Duration) func() {
		return func() {
			liveViewProfiles, liveViewFrameInterval, liveViewScanInterval, liveViewKeepalive = profiles, frame, scan, keepalive
		}
	}
	t.Cleanup(restore(liveViewProfiles, liveViewFrameInterval, liveViewScanInterval, liveViewKeepalive))
	liveViewProfiles = filepath.Join(profiles, "*", "DevToolsActivePort")
	liveViewFrameInterval, liveViewScanInterval = 20*time.Millisecond, 20*time.Millisecond
	liveViewKeepalive = time.Hour
	t.Setenv(workerenv.BrowserLiveView, "1")
	t.Setenv(workerenv.ControllerURL, controllerURL)
	t.Setenv(workerenv.TaskNamespace, "team-a")
	t.Setenv(workerenv.TaskName, "shop")
	t.Setenv(workerenv.ServiceAccountTokenPath, tokenFile)
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartLiveViewPostsTheBrowsersFrames(t *testing.T) {
	frames := [][]byte{[]byte("jpeg-frame-1"), []byte("jpeg-frame-2"), []byte("jpeg-frame-3")}
	browser := newFakeBrowser(t, frames)
	controller, posts := liveViewController(t, http.StatusNoContent)
	setUpLiveView(t, browser.port(t), controller.URL)

	stop := StartLiveView(context.Background())
	waitFor(t, "the last frame", func() bool {
		got := posts()
		return len(got) > 0 && string(got[len(got)-1].body) == "jpeg-frame-3"
	})
	stop()

	for _, post := range posts() {
		if post.path != "/internal/v1/live/team-a/shop" {
			t.Errorf("posted to %q", post.path)
		}
		if post.contentType != "image/jpeg" || post.auth != "Bearer callback-token" {
			t.Errorf("post headers: Content-Type %q, Authorization %q", post.contentType, post.auth)
		}
	}
	browser.mu.Lock()
	defer browser.mu.Unlock()
	if browser.acks < len(frames)-1 {
		t.Errorf("acknowledged %d frames, want at least %d", browser.acks, len(frames)-1)
	}
}

func TestStartLiveViewIsOffWithoutTheOperatorSwitch(t *testing.T) {
	browser := newFakeBrowser(t, [][]byte{[]byte("jpeg-frame-1")})
	controller, posts := liveViewController(t, http.StatusNoContent)
	setUpLiveView(t, browser.port(t), controller.URL)
	t.Setenv(workerenv.BrowserLiveView, "")

	stop := StartLiveView(context.Background())
	time.Sleep(200 * time.Millisecond)
	stop()
	if got := posts(); len(got) != 0 {
		t.Fatalf("posted %d frames with live view off; first: %s %q", len(got), got[0].path, got[0].body)
	}
}

func TestStartLiveViewStopsWhenTheControllerRefusesFrames(t *testing.T) {
	frames := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")}
	browser := newFakeBrowser(t, frames)
	controller, posts := liveViewController(t, http.StatusNotFound)
	setUpLiveView(t, browser.port(t), controller.URL)

	stop := StartLiveView(context.Background())
	waitFor(t, "the first post", func() bool { return len(posts()) > 0 })
	time.Sleep(200 * time.Millisecond)
	stop()
	if got := posts(); len(got) != 1 {
		t.Fatalf("posted %d frames after the controller answered 404, want 1", len(got))
	}
}

func TestLiveViewIgnoresPagesOffLoopback(t *testing.T) {
	browser := newFakeBrowser(t, [][]byte{[]byte("frame")})
	browser.pageWS = "ws://198.51.100.7:" + browser.port(t) + "/devtools/page/1"
	lv := &liveView{client: http.DefaultClient, pages: map[string]bool{}}
	var port int
	if _, err := fmt.Sscan(browser.port(t), &port); err != nil {
		t.Fatal(err)
	}
	pages, err := lv.pageTargets(context.Background(), port)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 0 {
		t.Fatalf("followed a page URL off loopback: %v", pages)
	}
}

func TestStartLiveViewRetriesAFrameTheControllerDidNotTake(t *testing.T) {
	browser := newFakeBrowser(t, [][]byte{[]byte("only-frame")})
	controller, posts := liveViewControllerFunc(t, func(n int) int {
		if n < 2 {
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	setUpLiveView(t, browser.port(t), controller.URL)

	stop := StartLiveView(context.Background())
	waitFor(t, "the frame to be taken after two failures", func() bool { return len(posts()) >= 3 })
	stop()
	for _, post := range posts()[:3] {
		if string(post.body) != "only-frame" {
			t.Fatalf("retried %q, want the same frame", post.body)
		}
	}
}

func TestStartLiveViewStreamsAFewPagesAtOnce(t *testing.T) {
	var mu sync.Mutex
	streaming := 0
	mux := http.NewServeMux()
	var addr string
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, _ *http.Request) {
		pages := make([]map[string]string, 0, 3*liveViewMaxPages)
		for i := range 3 * liveViewMaxPages {
			wsURL := fmt.Sprintf("ws://%s/devtools/page/%d", addr, i)
			pages = append(pages, map[string]string{"type": "page", "webSocketDebuggerUrl": wsURL})
		}
		_ = json.NewEncoder(w).Encode(pages)
	})
	mux.HandleFunc("/devtools/page/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		mu.Lock()
		streaming++
		mu.Unlock()
		_, _, _ = conn.Read(r.Context())
		<-r.Context().Done()
	})
	browser := httptest.NewServer(mux)
	t.Cleanup(browser.Close)
	addr = browser.Listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	controller, _ := liveViewController(t, http.StatusNoContent)
	setUpLiveView(t, port, controller.URL)

	stop := StartLiveView(t.Context())
	defer stop()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return streaming
	}
	waitFor(t, "the first pages to stream", func() bool { return count() >= liveViewMaxPages })
	time.Sleep(5 * liveViewScanInterval)
	if got := count(); got != liveViewMaxPages {
		t.Fatalf("streaming %d pages at once, want %d", got, liveViewMaxPages)
	}
}

func TestStartLiveViewResendsAStillPage(t *testing.T) {
	browser := newFakeBrowser(t, [][]byte{[]byte("still-page")})
	controller, posts := liveViewController(t, http.StatusNoContent)
	setUpLiveView(t, browser.port(t), controller.URL)
	liveViewKeepalive = 40 * time.Millisecond

	stop := StartLiveView(context.Background())
	waitFor(t, "keepalive posts of an unchanged frame", func() bool { return len(posts()) >= 3 })
	stop()
	for _, post := range posts() {
		if string(post.body) != "still-page" {
			t.Fatalf("posted %q, want the unchanged frame", post.body)
		}
	}
}
