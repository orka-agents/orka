package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type taskMutationBodyReader struct {
	io.Reader
	beforeRead func() error
}

func (r *taskMutationBodyReader) Read(data []byte) (int, error) {
	if r.beforeRead != nil {
		mutate := r.beforeRead
		r.beforeRead = nil
		if err := mutate(); err != nil {
			return 0, err
		}
	}
	return r.Reader.Read(data)
}

func TestInternalWritesRevalidateAfterStreamedBody(t *testing.T) {
	for _, endpoint := range []struct {
		name    string
		path    string
		body    string
		handler func(*InternalHandlers) fiber.Handler
		read    func(*sqlite.Store) error
	}{
		{"result", "/internal/v1/results/default/task-a", "result", func(h *InternalHandlers) fiber.Handler { return h.SubmitResult },
			func(s *sqlite.Store) error {
				_, err := s.GetResult(context.Background(), "default", "task-a")
				return err
			}},
		{"artifact", "/internal/v1/artifacts/default/task-a/output.txt", "artifact", func(h *InternalHandlers) fiber.Handler { return h.UploadArtifact },
			func(s *sqlite.Store) error {
				_, _, err := s.GetArtifact(context.Background(), "default", "task-a", "output.txt")
				return err
			}},
		{"plan", "/internal/v1/plans/default/task-a", `{"summary":"plan"}`, func(h *InternalHandlers) fiber.Handler { return h.SubmitPlan },
			func(s *sqlite.Store) error {
				_, err := s.GetPlan(context.Background(), "default", "task-a")
				return err
			}},
		{"event", "/internal/v1/events/default/task/task-a", `{"type":"WorkerStarted"}`, func(h *InternalHandlers) fiber.Handler { return h.SubmitExecutionEvent },
			func(s *sqlite.Store) error {
				seq, err := s.GetLatestExecutionEventSeq(context.Background(), "default", "task", "task-a")
				if err == nil && seq == 0 {
					return store.ErrNotFound
				}
				return err
			}},
	} {
		for _, change := range []string{"unchanged", "completed", "recreated"} {
			t.Run(endpoint.name+"/"+change, func(t *testing.T) {
				task := internalCallerAuthTask()
				job := internalCallerAuthJob(task, "job-a", "job-uid")
				pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
				kube := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).WithObjects(task, job, pod).Build()
				db, err := sqlite.NewDB(":memory:")
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				dataStore := sqlite.NewStore(db, ":memory:")
				h := NewInternalHandlers(dataStore, dataStore, dataStore, dataStore, dataStore, InternalHandlersConfig{
					Client: kube, APIReader: kube, ExecutionEventStore: dataStore,
				})
				app := fiber.New()
				bodyRead := false
				app.Use(func(c fiber.Ctx) error {
					c.Locals(UserInfoContextKey, internalCallerAuthWorkerUser("pod-a", "pod-uid"))
					c.Request().SetBodyStream(&taskMutationBodyReader{
						Reader: bytes.NewBufferString(endpoint.body),
						beforeRead: func() error {
							bodyRead = true
							current := &corev1alpha1.Task{}
							if err := kube.Get(c.Context(), client.ObjectKeyFromObject(task), current); err != nil {
								return err
							}
							switch change {
							case "completed":
								current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
								return kube.Update(c.Context(), current)
							case "recreated":
								if err := kube.Delete(c.Context(), current); err != nil {
									return err
								}
								current.UID, current.ResourceVersion = "replacement-task-uid", ""
								return kube.Create(c.Context(), current)
							default:
								return nil
							}
						},
					}, len(endpoint.body))
					return c.Next()
				})
				route := "/internal/v1/" + endpoint.name + "s/:namespace/:taskName"
				switch endpoint.name {
				case "artifact":
					route += "/:filename"
				case "event":
					route = "/internal/v1/events/:namespace/:streamType/:streamID"
				}
				app.Post(route, endpoint.handler(h))
				request := httptest.NewRequest(http.MethodPost, endpoint.path, nil)
				request.Header.Set("Content-Type", "application/json")
				response, err := app.Test(request)
				require.NoError(t, err)
				t.Cleanup(func() { _ = response.Body.Close() })
				require.True(t, bodyRead)
				if change == "unchanged" {
					require.Contains(t, []int{http.StatusCreated, http.StatusNoContent}, response.StatusCode)
					require.NoError(t, endpoint.read(dataStore))
				} else {
					wantStatus := http.StatusForbidden
					if endpoint.name == "event" && change == "completed" {
						wantStatus = http.StatusConflict
					}
					require.Equal(t, wantStatus, response.StatusCode)
					require.ErrorIs(t, endpoint.read(dataStore), store.ErrNotFound)
				}
			})
		}
	}
}
