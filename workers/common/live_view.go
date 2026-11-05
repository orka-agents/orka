/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/orka-agents/orka/internal/workerenv"
)

// The live view streams what the Task's browser shows to the dashboard. With
// live view on, the browser image's Chromium listens for DevTools on
// 127.0.0.1 and writes its port to DevToolsActivePort in its profile; the
// worker screencasts the pages it finds there, a few at a time, and posts
// the latest frame.
// Frames are posted only when the page repainted, at most twice a second.

const (
	// liveViewReadLimit fits one base64 screencast frame in a CDP message.
	liveViewReadLimit = 4 << 20
	// liveViewMaxFrameBytes matches the controller's limit for one frame.
	liveViewMaxFrameBytes = 1 << 20
	// liveViewQuality and the size bound keep a frame near 100 KB.
	liveViewQuality   = 60
	liveViewMaxWidth  = 1280
	liveViewMaxHeight = 800
	// liveViewMaxPages bounds the screencasts at once: the dashboard shows one
	// frame, and a Task that opens many tabs must not pay to encode them all.
	liveViewMaxPages = 4
)

var (
	// liveViewProfiles is where Chromium names its DevTools port: Playwright
	// and chromium-headless keep their profiles directly under /tmp.
	liveViewProfiles = "/tmp/*/DevToolsActivePort"
	// liveViewFrameInterval paces both the browser's encoding and the posts.
	liveViewFrameInterval = 500 * time.Millisecond
	// liveViewScanInterval is how often new browsers and pages are found.
	liveViewScanInterval = time.Second
	// liveViewKeepalive re-sends an unchanged frame, so a page that sits still
	// keeps its picture on the controller, which expires frames after 90 s.
	liveViewKeepalive = 30 * time.Second
)

type liveView struct {
	endpoint string
	client   *http.Client

	mu     sync.Mutex
	latest []byte
	seq    uint64
	pages  map[string]bool
	wg     sync.WaitGroup
	posted bool
}

// StartLiveView screencasts the browser the command opens and posts its
// latest frame to the controller, when the operator turned live view on for
// this Task. It is best effort and never fails the Task. stop ends it and
// waits for its connections to close.
func StartLiveView(ctx context.Context) (stop func()) {
	if os.Getenv(workerenv.BrowserLiveView) != "1" {
		return func() {}
	}
	endpoint, err := liveViewEndpoint()
	if err != nil {
		fmt.Fprintf(os.Stderr, "live view: %v\n", err)
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	lv := &liveView{endpoint: endpoint, client: &http.Client{Timeout: 10 * time.Second}, pages: map[string]bool{}}
	lv.wg.Go(func() { lv.discover(ctx) })
	lv.wg.Go(func() { lv.publish(ctx, cancel) })
	return func() {
		cancel()
		lv.wg.Wait()
	}
}

func liveViewEndpoint() (string, error) {
	controllerURL := strings.TrimRight(os.Getenv(workerenv.ControllerURL), "/")
	if controllerURL == "" {
		return "", fmt.Errorf("%s must be set", workerenv.ControllerURL)
	}
	namespace := os.Getenv(workerenv.TaskNamespace)
	if namespace == "" {
		data, err := os.ReadFile(saNamespacePath)
		if err != nil {
			return "", fmt.Errorf("%s not set and cannot read namespace from SA: %w", workerenv.TaskNamespace, err)
		}
		namespace = strings.TrimSpace(string(data))
	}
	taskName := os.Getenv(workerenv.TaskName)
	if taskName == "" {
		return "", fmt.Errorf("%s must be set", workerenv.TaskName)
	}
	return fmt.Sprintf("%s/internal/v1/live/%s/%s",
		controllerURL, url.PathEscape(namespace), url.PathEscape(taskName)), nil
}

func (lv *liveView) discover(ctx context.Context) {
	ticker := time.NewTicker(liveViewScanInterval)
	defer ticker.Stop()
	for {
		lv.scan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// scan attaches to the pages of every browser that named its DevTools port,
// up to liveViewMaxPages at once.
func (lv *liveView) scan(ctx context.Context) {
	files, _ := filepath.Glob(liveViewProfiles)
	for _, file := range files {
		port, ok := devToolsPort(file)
		if !ok {
			continue
		}
		pages, err := lv.pageTargets(ctx, port)
		if err != nil {
			continue
		}
		for _, wsURL := range pages {
			// DevTools lists the most recently active pages first; once
			// liveViewMaxPages stream, another waits for one to close.
			lv.mu.Lock()
			attach := !lv.pages[wsURL] && len(lv.pages) < liveViewMaxPages
			if attach {
				lv.pages[wsURL] = true
			}
			lv.mu.Unlock()
			if !attach {
				continue
			}
			lv.wg.Go(func() {
				lv.screencast(ctx, wsURL)
				lv.mu.Lock()
				delete(lv.pages, wsURL)
				lv.mu.Unlock()
			})
		}
	}
}

// devToolsPort reads the port on the first line of a DevToolsActivePort file.
// The Task's code can write under /tmp, so only a regular file is read, and
// only its start: opening without blocking keeps a FIFO by that name from
// stalling discovery, and with it the worker's exit.
func devToolsPort(file string) (int, bool) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	data, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return 0, false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	port, err := strconv.Atoi(strings.TrimSpace(line))
	return port, err == nil && port > 0 && port < 65536
}

// pageTargets lists the WebSocket URLs of a browser's pages. Only loopback
// URLs on the browser's own port are used.
func (lv *liveView) pageTargets(ctx context.Context, port int) ([]string, error) {
	listURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := lv.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", listURL, resp.StatusCode)
	}
	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, err
	}
	pages := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.Type != "page" {
			continue
		}
		u, err := url.Parse(target.WebSocketDebuggerURL)
		if err != nil || u.Scheme != "ws" || u.Port() != strconv.Itoa(port) ||
			(u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
			continue
		}
		pages = append(pages, u.String())
	}
	return pages, nil
}

// screencast follows one page until it closes. Each frame is acknowledged
// only after a frame interval, so the browser encodes no more than it sends.
func (lv *liveView) screencast(ctx context.Context, wsURL string) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(liveViewReadLimit)
	start := fmt.Sprintf(`{"id":1,"method":"Page.startScreencast",`+
		`"params":{"format":"jpeg","quality":%d,"maxWidth":%d,"maxHeight":%d}}`,
		liveViewQuality, liveViewMaxWidth, liveViewMaxHeight)
	if err := conn.Write(ctx, websocket.MessageText, []byte(start)); err != nil {
		return
	}
	id := 1
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var event struct {
			Method string `json:"method"`
			Params struct {
				Data      string `json:"data"`
				SessionID int    `json:"sessionId"`
			} `json:"params"`
		}
		if json.Unmarshal(msg, &event) != nil || event.Method != "Page.screencastFrame" {
			continue
		}
		if frame, err := base64.StdEncoding.DecodeString(event.Params.Data); err == nil &&
			len(frame) > 0 && len(frame) <= liveViewMaxFrameBytes {
			lv.offer(frame)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(liveViewFrameInterval):
		}
		id++
		ack := fmt.Sprintf(`{"id":%d,"method":"Page.screencastFrameAck","params":{"sessionId":%d}}`,
			id, event.Params.SessionID)
		if err := conn.Write(ctx, websocket.MessageText, []byte(ack)); err != nil {
			return
		}
	}
}

func (lv *liveView) offer(frame []byte) {
	lv.mu.Lock()
	defer lv.mu.Unlock()
	lv.latest = frame
	lv.seq++
}

// publish posts the latest frame when it changed, and again as a keepalive
// while it does not. A frame counts as sent only once the controller took it,
// so a failed post is retried on the next tick. A controller that does not
// take frames for this Task ends the live view.
func (lv *liveView) publish(ctx context.Context, stopAll context.CancelFunc) {
	ticker := time.NewTicker(liveViewFrameInterval)
	defer ticker.Stop()
	var sent uint64
	var sentAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		lv.mu.Lock()
		frame, seq := lv.latest, lv.seq
		lv.mu.Unlock()
		if frame == nil || (seq == sent && time.Since(sentAt) < liveViewKeepalive) {
			continue
		}
		status, err := lv.post(ctx, frame)
		switch {
		case err != nil:
			continue
		case status == http.StatusNotFound || status == http.StatusForbidden:
			fmt.Fprintf(os.Stderr, "live view: the controller does not take frames for this Task (HTTP %d); stopped\n", status)
			stopAll()
			return
		case status/100 == 2:
			sent, sentAt = seq, time.Now()
			if !lv.posted {
				lv.posted = true
				fmt.Fprintln(os.Stderr, "live view: streaming the browser to the dashboard")
			}
		}
	}
}

func (lv *liveView) post(ctx context.Context, frame []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lv.endpoint, bytes.NewReader(frame))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "image/jpeg")
	if token := workerServiceAccountToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := lv.client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
