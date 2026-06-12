package common_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/sozercan/orka/internal/api"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
	common "github.com/sozercan/orka/workers/common"
)

type failingExecutionEventStore struct{}

func (failingExecutionEventStore) AppendExecutionEvent(
	context.Context,
	*store.ExecutionEvent,
) (*store.ExecutionEvent, error) {
	return nil, errors.New("append failed")
}

func (failingExecutionEventStore) ListExecutionEvents(
	context.Context,
	store.ExecutionEventFilter,
) ([]store.ExecutionEvent, error) {
	return nil, errors.New("not implemented")
}

func (failingExecutionEventStore) GetLatestExecutionEventSeq(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("not implemented")
}

func (failingExecutionEventStore) DeleteExecutionEvents(context.Context, string, string, string) error {
	return errors.New("not implemented")
}

func TestEventRecorderHTTPIntegrationPostsToInternalAPI(t *testing.T) {
	eventStore := newWorkerSQLiteExecutionEventStore(t)
	const bearerToken = "event-recorder-integration-token"
	tokenSeen := make(chan string, 1)
	app := setupWorkerInternalEventAPI(t, eventStore, bearerToken, tokenSeen)
	controllerURL := startFiberAppForWorkerTest(t, app)
	secret := testWorkerDashToken("bearer")

	recorder := common.NewHTTPEventRecorder(common.HTTPEventRecorderConfig{
		ControllerURL: controllerURL,
		Namespace:     "default",
		TaskName:      "task-worker",
		BearerPath:    writeWorkerTestSAToken(t, bearerToken),
		Timeout:       time.Second,
	})
	recorder.Record(context.Background(), events.ExecutionEventTypeWorkerStarted,
		common.WithEventSummary("worker started"),
		common.WithEventContent(mustRawJSONIntegration(t, map[string]any{"token": secret, "safe": "ok"})),
	)

	select {
	case got := <-tokenSeen:
		if got != bearerToken {
			t.Fatalf("TokenReview token = %q, want bearer token", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for TokenReview")
	}

	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-worker",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 1 || listed[0].Type != events.ExecutionEventTypeWorkerStarted || listed[0].Seq != 1 {
		t.Fatalf("listed events = %#v, want WorkerStarted seq 1", listed)
	}
	content := string(listed[0].Content)
	if strings.Contains(content, secret) || !strings.Contains(content, events.ExecutionEventRedactedValue) {
		t.Fatalf("persisted content was not redacted: %s", listed[0].Content)
	}
}

func TestEventRecorderHTTPIntegrationBearerTokenRequired(t *testing.T) {
	eventStore := newWorkerSQLiteExecutionEventStore(t)
	app := setupWorkerInternalEventAPI(t, eventStore, "required-token", nil)
	controllerURL := startFiberAppForWorkerTest(t, app)

	recorder := common.NewHTTPEventRecorder(common.HTTPEventRecorderConfig{
		ControllerURL: controllerURL,
		Namespace:     "default",
		TaskName:      "task-worker",
		BearerPath:    filepath.Join(t.TempDir(), "missing-token"),
		Timeout:       time.Second,
	})
	recorder.Record(context.Background(), events.ExecutionEventTypeWorkerStarted)

	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-worker",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("listed events = %#v, want no write without bearer token", listed)
	}
}

func TestEventRecorderHTTPIntegrationNonFatalFailures(t *testing.T) {
	t.Run("real API 500", func(t *testing.T) {
		const bearerToken = "event-recorder-500-token"
		app := setupWorkerInternalEventAPI(t, failingExecutionEventStore{}, bearerToken, nil)
		controllerURL := startFiberAppForWorkerTest(t, app)
		recorder := common.NewHTTPEventRecorder(common.HTTPEventRecorderConfig{
			ControllerURL: controllerURL,
			Namespace:     "default",
			TaskName:      "task-worker",
			BearerPath:    writeWorkerTestSAToken(t, bearerToken),
			Timeout:       time.Second,
		})
		recorder.Record(context.Background(), events.ExecutionEventTypeWorkerFailed)
	})

	t.Run("connection refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		recorder := common.NewHTTPEventRecorder(common.HTTPEventRecorderConfig{
			ControllerURL: "http://" + addr,
			Namespace:     "default",
			TaskName:      "task-worker",
			BearerPath:    writeWorkerTestSAToken(t, "token"),
			Timeout:       50 * time.Millisecond,
		})
		recorder.Record(context.Background(), events.ExecutionEventTypeWorkerFailed)
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusCreated)
		}))
		defer server.Close()
		recorder := common.NewHTTPEventRecorder(common.HTTPEventRecorderConfig{
			ControllerURL: server.URL,
			Namespace:     "default",
			TaskName:      "task-worker",
			BearerPath:    writeWorkerTestSAToken(t, "token"),
			Timeout:       10 * time.Millisecond,
		})
		start := time.Now()
		recorder.Record(context.Background(), events.ExecutionEventTypeWorkerFailed)
		if elapsed := time.Since(start); elapsed > 80*time.Millisecond {
			t.Fatalf("Record elapsed = %v, want timeout before server completes", elapsed)
		}
	})
}

func setupWorkerInternalEventAPI(
	t *testing.T,
	eventStore store.ExecutionEventStore,
	validToken string,
	tokenSeen chan<- string,
) *fiber.App {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = authenticationv1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if tr, ok := obj.(*authenticationv1.TokenReview); ok {
					if tokenSeen != nil {
						select {
						case tokenSeen <- tr.Spec.Token:
						default:
						}
					}
					if tr.Spec.Token == validToken {
						tr.Status.Authenticated = true
						tr.Status.User = authenticationv1.UserInfo{
							Username: "system:serviceaccount:default:worker",
							UID:      "worker-uid",
							Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:default"},
						}
					}
				}
				return nil
			},
		}).
		Build()

	app := fiber.New()
	app.Use(api.NewAuthMiddleware(k8sClient))
	h := api.NewInternalHandlers(nil, nil, nil, nil, nil, api.InternalHandlersConfig{ExecutionEventStore: eventStore})
	app.Post("/internal/v1/events/:namespace/:streamType/:streamID", h.SubmitExecutionEvent)
	return app
}

func startFiberAppForWorkerTest(t *testing.T, app *fiber.App) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- app.Listener(ln) }()
	t.Cleanup(func() {
		_ = app.Shutdown()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Logf("fiber listener exited: %v", err)
			}
		case <-time.After(time.Second):
			t.Log("timed out waiting for fiber shutdown")
		}
	})
	return "http://" + ln.Addr().String()
}

func newWorkerSQLiteExecutionEventStore(t *testing.T) *sqlite.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "events.db")
	db, err := sqlite.NewDB(dbPath)
	if err != nil {
		t.Fatalf("sqlite.NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db, dbPath)
}

func mustRawJSONIntegration(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw JSON: %v", err)
	}
	return json.RawMessage(data)
}

func writeWorkerTestSAToken(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func testWorkerDashToken(prefix string) string {
	return strings.Join([]string{prefix, "value", "for", "redaction"}, "-")
}
