/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const liveViewTestImage = "ghcr.io/orka-agents/orka/browser-worker@sha256:" +
	"1111111111111111111111111111111111111111111111111111111111111111"

// liveViewFixture runs the internal and public live view handlers over one
// frame holder. Tasks "shop" and "other" each have a worker Pod; "shop" runs
// the browser image and "other" a plain container image.
type liveViewFixture struct {
	app    *fiber.App
	frames *LiveViewFrames
	client client.Client
	reads  *countingReader
	// body, when set, replaces a frame post's body with a stream of length
	// bytes; -1 sends it chunked.
	body func(c fiber.Ctx)
}

// countingReader counts the reads the worker proof makes.
type countingReader struct {
	client.Reader
	gets atomic.Int64
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets.Add(1)
	return r.Reader.Get(ctx, key, obj, opts...)
}

func newLiveViewFixture(t *testing.T, enabled bool) *liveViewFixture {
	t.Helper()
	scheme := internalCallerAuthScheme(t)
	objects := make([]client.Object, 0, 9)
	for name, image := range map[string]string{"shop": liveViewTestImage, "other": "busybox:1.37", "custom": liveViewTestImage} {
		task := internalCallerAuthTaskObject(name, name+"-uid", name+"-job", "", "")
		task.Status.Attempts = 1
		task.Spec.Type = corev1alpha1.TaskTypeContainer
		task.Spec.Image = image
		if name == "custom" {
			// A command replaces the image entrypoint, and with it the worker.
			task.Spec.Command = []string{"sh", "-c"}
		}
		job := internalCallerAuthJob(task, name+"-job", name+"-job-uid")
		pod := internalCallerAuthPod(task, name+"-pod", name+"-pod-uid", job)
		objects = append(objects, task, job, pod)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&corev1alpha1.Task{}).Build()
	var frames *LiveViewFrames
	if enabled {
		frames = NewLiveViewFrames()
	}
	reads := &countingReader{Reader: k8sClient}
	internal := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{
		Client: k8sClient, APIReader: reads, LiveViewFrames: frames, LiveViewBrowserImage: liveViewTestImage,
	})
	public := NewHandlers(HandlersConfig{Client: k8sClient, APIReader: k8sClient, LiveViewFrames: frames, LiveViewBrowserImage: liveViewTestImage})
	f := &liveViewFixture{frames: frames, client: k8sClient, reads: reads}
	f.app = fiber.New()
	// X-Test-Pod names the worker Pod a frame post comes from.
	f.app.Use(func(c fiber.Ctx) error {
		if pod := c.Get("X-Test-Pod"); pod != "" {
			c.Locals(UserInfoContextKey, internalCallerAuthWorkerUser(pod, pod+"-uid"))
		}
		if f.body != nil {
			f.body(c)
		}
		return c.Next()
	})
	f.app.Post("/internal/v1/live/:namespace/:taskName", internal.SubmitLiveViewFrame)
	f.app.Get("/api/v1/tasks/:id/live", public.GetTaskLiveView)
	return f
}

func (f *liveViewFixture) post(t *testing.T, task, pod, contentType string, body []byte) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/live/default/"+task, bytes.NewReader(body))
	req.Header.Set("X-Test-Pod", pod)
	req.Header.Set("Content-Type", contentType)
	resp, err := f.app.Test(req)
	require.NoError(t, err)
	return resp
}

func (f *liveViewFixture) get(t *testing.T, task string, header ...string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+task+"/live?namespace=default", nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := f.app.Test(req)
	require.NoError(t, err)
	return resp
}

func TestLiveViewShowsTheWorkersLatestFrame(t *testing.T) {
	f := newLiveViewFixture(t, true)

	resp := f.get(t, "shop")
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "no frame has arrived yet")

	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("frame-1")).StatusCode)
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("frame-2")).StatusCode)

	resp = f.get(t, "shop")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "frame-2", string(body), "a newer frame replaces the previous one")
	require.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"))
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.NotEmpty(t, resp.Header.Get("X-Orka-Frame-Time"))
	etag := resp.Header.Get("ETag")
	require.NotEmpty(t, etag)

	require.Equal(t, http.StatusNotModified, f.get(t, "shop", "If-None-Match", etag).StatusCode)
}

func TestLiveViewTakesFramesOnlyFromTheTasksOwnWorker(t *testing.T) {
	f := newLiveViewFixture(t, true)

	resp := f.post(t, "shop", "other-pod", "image/jpeg", []byte("forged"))
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "another Task's worker posted a frame")
	require.Equal(t, http.StatusForbidden, f.post(t, "other", "other-pod", "image/jpeg", []byte("frame")).StatusCode,
		"a Task off the browser image has no live view")
	require.Equal(t, http.StatusNoContent, f.get(t, "shop").StatusCode, "a refused frame was stored")
}

func TestLiveViewRefusesFramesItCannotShow(t *testing.T) {
	f := newLiveViewFixture(t, true)

	require.Equal(t, http.StatusUnsupportedMediaType, f.post(t, "shop", "shop-pod", "image/png", []byte("png")).StatusCode)
	require.Equal(t, http.StatusBadRequest, f.post(t, "shop", "shop-pod", "image/jpeg", nil).StatusCode)
	tooLarge := bytes.Repeat([]byte("x"), maxLiveViewFrameBytes+1)
	require.Equal(t, http.StatusRequestEntityTooLarge, f.post(t, "shop", "shop-pod", "image/jpeg", tooLarge).StatusCode)
}

func TestLiveViewOnlyForRunningBrowserTasks(t *testing.T) {
	f := newLiveViewFixture(t, true)
	require.Equal(t, http.StatusNotFound, f.get(t, "other").StatusCode, "a plain container Task has no live view")
	require.Equal(t, http.StatusNotFound, f.get(t, "custom").StatusCode, "a Task whose command replaces the entrypoint has no live view")
	require.Equal(t, http.StatusForbidden, f.post(t, "custom", "custom-pod", "image/jpeg", []byte("frame")).StatusCode)

	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("frame")).StatusCode)
	task := &corev1alpha1.Task{}
	require.NoError(t, f.client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "shop"}, task))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	require.NoError(t, f.client.Status().Update(t.Context(), task))

	require.Equal(t, http.StatusNotFound, f.get(t, "shop").StatusCode, "a finished Task still showed a frame")
	_, kept := f.frames.get(liveViewKey("default", "shop", task.UID))
	require.False(t, kept, "a finished Task's frame stayed in memory")
}

func TestLiveViewIsOffByDefault(t *testing.T) {
	f := newLiveViewFixture(t, false)
	require.Equal(t, http.StatusNotFound, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("frame")).StatusCode)
	require.Equal(t, http.StatusNotFound, f.get(t, "shop").StatusCode)
}

func TestLiveViewFramesExpireAndStayBounded(t *testing.T) {
	frames := NewLiveViewFrames()
	now := time.Unix(1_800_000_000, 0)
	frames.now = func() time.Time { return now }

	frames.put("default", "a", "a-uid", "job-uid", 1, []byte("old"))
	now = now.Add(liveViewFrameTTL + time.Second)
	_, ok := frames.get(liveViewKey("default", "a", "a-uid"))
	require.False(t, ok, "a frame outlived its time to live")

	for i := 0; i <= maxLiveViewTasks; i++ {
		now = now.Add(time.Millisecond)
		frames.put("default", fmt.Sprintf("task-%d", i), "uid", "job-uid", 1, []byte("frame"))
	}
	require.Len(t, frames.frames, maxLiveViewTasks)
	_, ok = frames.get(liveViewKey("default", "task-0", "uid"))
	require.False(t, ok, "the oldest frame was kept past the bound")

	// A recreated Task with the same name has a new UID, so a new key.
	require.NotEqual(t, liveViewKey("default", "shop", "uid-1"), liveViewKey("default", "shop", "uid-2"))
}

func TestLiveViewSweepDropsFramesOfTasksThatStopped(t *testing.T) {
	frames := NewLiveViewFrames()
	now := time.Unix(1_800_000_000, 0)
	frames.now = func() time.Time { return now }
	frames.put("default", "running", "running-uid", "job-uid", 1, []byte("keep"))
	frames.put("default", "finished", "finished-uid", "job-uid", 1, []byte("drop"))
	frames.put("default", "stale", "stale-uid", "job-uid", 1, []byte("drop"))
	frames.frames[liveViewKey("default", "stale", "stale-uid")] = liveViewFrame{
		at: now.Add(-liveViewFrameTTL - time.Second), namespace: "default", name: "stale", uid: "stale-uid",
	}

	frames.sweep(t.Context(), func(_ context.Context, _, name string, _ types.UID) bool {
		return name == "running" || name == "stale"
	})

	require.Len(t, frames.frames, 1, "a finished or expired Task's frame stayed in memory")
	_, kept := frames.get(liveViewKey("default", "running", "running-uid"))
	require.True(t, kept)
}

func TestLiveViewTaskRunningMatchesTheTaskUID(t *testing.T) {
	f := newLiveViewFixture(t, true)
	s := &Server{client: f.client}
	require.True(t, s.liveViewTaskRunning(t.Context(), "default", "shop", "shop-uid"))
	require.False(t, s.liveViewTaskRunning(t.Context(), "default", "shop", "an-earlier-uid"), "a recreated Task kept the old frame")
	require.False(t, s.liveViewTaskRunning(t.Context(), "default", "gone", "gone-uid"), "a deleted Task kept its frame")
}

func TestLiveViewFrameUploadsAreBounded(t *testing.T) {
	for _, target := range []string{
		"/internal/v1/live/default/shop",
		"/INTERNAL/v1/Live/default/shop/",
		// The router drops a fragment, so the bound must too.
		"/internal/v1/live/default/shop#ignored/fragment",
		"http://orka-api.orka-system.svc:8080/internal/v1/live/default/shop",
	} {
		header := &fasthttp.RequestHeader{}
		header.SetMethod(http.MethodPost)
		header.SetRequestURI(target)
		config := requestBodyConfig(header)
		require.Equal(t, maxLiveViewFrameBytes, config.MaxRequestBodySize, target)
		require.Equal(t, liveViewReadTimeout, config.ReadTimeout, target)
	}
}

// The server streams request bodies, and fasthttp's body limit does not cover
// a streamed chunked body, so the handler bounds its own read.
func TestLiveViewBoundsAStreamedFrame(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%t", chunked), func(t *testing.T) {
			f := newLiveViewFixture(t, true)
			stream := &liveViewCountingStream{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 4*maxLiveViewFrameBytes))}
			f.body = func(c fiber.Ctx) {
				length := 4 * maxLiveViewFrameBytes
				if chunked {
					length = -1
				}
				c.Request().SetBodyStream(stream, length)
			}
			resp := f.post(t, "shop", "shop-pod", "image/jpeg", nil)
			require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
			if chunked {
				require.Equal(t, maxLiveViewFrameBytes+1, stream.read, "the handler read past the frame limit")
			} else {
				require.Zero(t, stream.read, "a declared oversized frame was read")
			}
			require.Equal(t, http.StatusNoContent, f.get(t, "shop").StatusCode, "an oversized frame was stored")
		})
	}

	f := newLiveViewFixture(t, true)
	stream := &liveViewCountingStream{Reader: bytes.NewReader([]byte("chunked-frame"))}
	f.body = func(c fiber.Ctx) { c.Request().SetBodyStream(stream, -1) }
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", nil).StatusCode)
	f.body = nil
	resp := f.get(t, "shop")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "chunked-frame", string(body))
}

type liveViewCountingStream struct {
	io.Reader
	read int
}

func (s *liveViewCountingStream) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	s.read += n
	return n, err
}

// A retry runs a new Job, whose worker may take a while to paint: until it
// does, the previous attempt's last frame must not show as this attempt's.
func TestLiveViewShowsOnlyTheCurrentAttemptsFrames(t *testing.T) {
	f := newLiveViewFixture(t, true)
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("attempt-1")).StatusCode)
	require.Equal(t, http.StatusOK, f.get(t, "shop").StatusCode)

	task := &corev1alpha1.Task{}
	require.NoError(t, f.client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "shop"}, task))
	task.Status.Attempts = 2
	task.Status.JobName, task.Status.JobUID = "shop-job-2", "shop-job-2-uid"
	require.NoError(t, f.client.Status().Update(t.Context(), task))
	require.Equal(t, http.StatusNoContent, f.get(t, "shop").StatusCode, "the previous attempt's frame was shown")

	// The same picture from the new attempt's worker is a new frame, not a keepalive.
	f.frames.put("default", "shop", task.UID, "shop-job-2-uid", 2, []byte("attempt-1"))
	resp := f.get(t, "shop")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "attempt-1", string(body))
}

// The previous attempt's worker may still be shutting down when the retry's
// worker sends its first frame, and its proof is remembered for a while: a
// late frame from it must not replace the retry's.
func TestLiveViewRefusesAnEarlierAttemptsLateFrame(t *testing.T) {
	f := newLiveViewFixture(t, true)
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("attempt-1")).StatusCode)

	task := &corev1alpha1.Task{}
	require.NoError(t, f.client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "shop"}, task))
	task.Status.Attempts = 2
	task.Status.JobName, task.Status.JobUID = "shop-job-2", "shop-job-2-uid"
	require.NoError(t, f.client.Status().Update(t.Context(), task))
	job := internalCallerAuthJob(task, "shop-job-2", "shop-job-2-uid")
	require.NoError(t, f.client.Create(t.Context(), job))
	require.NoError(t, f.client.Create(t.Context(), internalCallerAuthPod(task, "shop-pod-2", "shop-pod-2-uid", job)))
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod-2", "image/jpeg", []byte("attempt-2")).StatusCode)

	require.Equal(t, http.StatusForbidden, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("late")).StatusCode,
		"the earlier attempt's worker was not told to stop")
	resp := f.get(t, "shop")
	require.Equal(t, http.StatusOK, resp.StatusCode, "the earlier attempt's frame replaced the retry's")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "attempt-2", string(body))
}

// The dashboard names the Task it shows by UID: a Task recreated under the
// same name between two of its Task polls must not stream into the old card.
func TestLiveViewServesOnlyTheTaskTheDashboardShows(t *testing.T) {
	f := newLiveViewFixture(t, true)
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("frame")).StatusCode)
	get := func(uid string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/shop/live?namespace=default&uid="+uid, nil)
		resp, err := f.app.Test(req)
		require.NoError(t, err)
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, get("shop-uid"))
	require.Equal(t, http.StatusNotFound, get("replaced-shop-uid"), "another Task of this name streamed into the card")
}

// Fiber's route parameters point into a request buffer that later requests
// reuse; a stored frame must keep its Task's namespace regardless.
func TestLiveViewFrameKeepsItsNamespaceAcrossRequests(t *testing.T) {
	f := newLiveViewFixture(t, true)
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("frame")).StatusCode)
	for range 3 {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/live/zzzzzzz/shop", bytes.NewReader([]byte("x")))
		req.Header.Set("Content-Type", "image/jpeg")
		resp, err := f.app.Test(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}
	for key, frame := range f.frames.frames {
		require.Equal(t, "default", frame.namespace, key)
	}
}

// A retry keeps the Task's UID, so its first frame lands under the same key
// as the previous attempt's: cleanup that judged the old frame must not take
// the new one.
func TestLiveViewCleanupKeepsANewerAttemptsFrame(t *testing.T) {
	frames := NewLiveViewFrames()
	frames.put("default", "shop", "shop-uid", "job-1", 1, []byte("attempt-1"))
	frames.sweep(t.Context(), func(context.Context, string, string, types.UID) bool {
		// The new attempt posts while the sweeper sees the Task between attempts.
		frames.put("default", "shop", "shop-uid", "job-2", 2, []byte("attempt-2"))
		return false
	})
	frame, ok := frames.get(liveViewKey("default", "shop", "shop-uid"))
	require.True(t, ok, "the sweep dropped the new attempt's frame")
	require.Equal(t, "attempt-2", string(frame.data))

	f := newLiveViewFixture(t, true)
	task := &corev1alpha1.Task{}
	require.NoError(t, f.client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "shop"}, task))
	task.Status.Phase, task.Status.JobName, task.Status.JobUID = corev1alpha1.TaskPhasePending, "", ""
	require.NoError(t, f.client.Status().Update(t.Context(), task))
	f.frames.put("default", "shop", task.UID, "shop-job-2-uid", 2, []byte("attempt-2"))
	require.Equal(t, http.StatusNotFound, f.get(t, "shop").StatusCode)
	_, ok = f.frames.get(liveViewKey("default", "shop", task.UID))
	require.True(t, ok, "a read of the Task between attempts dropped the next attempt's frame")

	task.Status.Phase, task.Status.JobUID = corev1alpha1.TaskPhaseSucceeded, "shop-job-2-uid"
	require.NoError(t, f.client.Status().Update(t.Context(), task))
	require.Equal(t, http.StatusNotFound, f.get(t, "shop").StatusCode)
	_, ok = f.frames.get(liveViewKey("default", "shop", task.UID))
	require.False(t, ok, "a finished Task kept its last frame")
}

func TestLiveViewKeepaliveKeepsTheFrameAndItsTime(t *testing.T) {
	frames := NewLiveViewFrames()
	start := time.Unix(1_800_000_000, 0)
	now := start
	frames.now = func() time.Time { return now }
	key := liveViewKey("default", "shop", "shop-uid")

	frames.put("default", "shop", "shop-uid", "job-uid", 1, []byte("still-page"))
	first, _ := frames.get(key)
	now = start.Add(liveViewFrameTTL - time.Second)
	frames.put("default", "shop", "shop-uid", "job-uid", 1, []byte("still-page"))
	now = start.Add(liveViewFrameTTL + time.Second)

	kept, ok := frames.get(key)
	require.True(t, ok, "a keepalive did not renew the frame")
	require.Equal(t, first.seq, kept.seq, "a keepalive changed the ETag")
	require.Equal(t, start, kept.changedAt, "a keepalive moved the time the page last changed")

	frames.put("default", "shop", "shop-uid", "job-uid", 1, []byte("new-page"))
	changed, _ := frames.get(key)
	require.NotEqual(t, first.seq, changed.seq)
	require.Equal(t, now, changed.changedAt)
}

func TestLiveViewETagsDifferAcrossControllerRestarts(t *testing.T) {
	before, after := NewLiveViewFrames(), NewLiveViewFrames()
	before.put("default", "shop", "shop-uid", "job-uid", 1, []byte("frame-a"))
	after.put("default", "shop", "shop-uid", "job-uid", 1, []byte("frame-b"))
	a, _ := before.get(liveViewKey("default", "shop", "shop-uid"))
	b, _ := after.get(liveViewKey("default", "shop", "shop-uid"))
	require.Equal(t, a.seq, b.seq, "both holders start their sequence at the same number")
	require.NotEqual(t, before.etag(a), after.etag(b), "a restarted controller reused an ETag for a different frame")
}

func TestLiveViewProvesTheWorkerOncePerInterval(t *testing.T) {
	f := newLiveViewFixture(t, true)
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("frame-1")).StatusCode)
	proof := f.reads.gets.Load()
	require.Positive(t, proof)
	for i := 2; i <= 5; i++ {
		require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte(fmt.Sprintf("frame-%d", i))).StatusCode)
	}
	require.Equal(t, proof, f.reads.gets.Load(), "every frame proved the worker against the API server again")

	// Another Pod is still refused: only a successful proof is remembered.
	require.Equal(t, http.StatusForbidden, f.post(t, "shop", "other-pod", "image/jpeg", []byte("forged")).StatusCode)
	require.Equal(t, http.StatusForbidden, f.post(t, "shop", "other-pod", "image/jpeg", []byte("forged")).StatusCode)

	// After the interval, the worker proves itself again.
	f.frames.now = func() time.Time { return time.Now().Add(liveViewCallerTTL + time.Second) }
	before := f.reads.gets.Load()
	require.Equal(t, http.StatusNoContent, f.post(t, "shop", "shop-pod", "image/jpeg", []byte("frame-6")).StatusCode)
	require.Greater(t, f.reads.gets.Load(), before, "an expired proof was trusted")
}
