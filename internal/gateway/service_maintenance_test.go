package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestServiceStartDispatchesAndDeliversDuringBlockedMaintenance(t *testing.T) {
	// Keep the fixture's real HTTP listener outside the synctest bubble. The
	// delivery request still runs through the adapter's handler without a socket.
	service, db, adapter := newGatewayServiceFixture(t)
	handler := adapter.Handler()
	service.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Result(), nil
	})}

	synctest.Test(t, func(t *testing.T) {
		blocker := blockGatewayServiceMaintenance(service)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		var startErr error
		go func() {
			defer close(done)
			startErr = service.Start(ctx)
		}()
		defer func() {
			cancel()
			blocker.release()
			<-done
			if startErr != nil {
				t.Errorf("Service.Start() returned %v", startErr)
			}
		}()

		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case <-blocker.entered:
		default:
			t.Fatal("retention cleanup did not start at the maintenance tick")
		}

		accepted, err := service.AdmitEvent(ctx, "default", "chat", "Bearer inbound-token",
			gatewayEventBody(t, "during-blocked-maintenance", "user-1"))
		if err != nil || accepted.Status != ingressStatusAccepted {
			t.Fatalf("admission while cleanup is blocked = %+v, %v", accepted, err)
		}
		time.Sleep(2 * service.Config.PollInterval)
		synctest.Wait()
		event, err := db.GetGatewayEvent(ctx, "default", accepted.EventID)
		if err != nil {
			t.Fatal(err)
		}
		if event.State != store.GatewayEventTaskCreated || event.TaskUID == "" {
			t.Fatalf("blocked maintenance stalled automatic dispatch: event state = %q, Task UID = %q", event.State, event.TaskUID)
		}
		task := &corev1alpha1.Task{}
		if err := service.Client.Get(ctx, client.ObjectKey{Namespace: event.Namespace, Name: event.TaskName}, task); err != nil {
			t.Fatal(err)
		}
		const result = "response delivered while retention cleanup is blocked"
		if err := db.SaveResult(ctx, task.Namespace, task.Name, []byte(result)); err != nil {
			t.Fatal(err)
		}
		task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
		if err := service.Client.Status().Update(ctx, task); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * service.Config.PollInterval)
		synctest.Wait()
		event, err = db.GetGatewayEvent(ctx, event.Namespace, event.ID)
		if err != nil {
			t.Fatal(err)
		}
		if event.State != store.GatewayEventCompleted || event.DeliveryID == "" {
			t.Fatalf("blocked maintenance stalled terminal projection: event = %+v", event)
		}
		delivery, err := db.GetGatewayDelivery(ctx, event.Namespace, event.DeliveryID)
		if err != nil || delivery.State != store.GatewayDeliveryDelivered {
			t.Fatalf("blocked maintenance stalled automatic delivery: delivery = %+v, error = %v", delivery, err)
		}
		deliveries := adapter.Deliveries()
		if len(deliveries) != 1 || deliveries[0].OriginatingEvent != accepted.EventID || deliveries[0].Text != result ||
			deliveries[0].TaskRef == nil || deliveries[0].TaskRef.Name != task.Name ||
			deliveries[0].SessionRef == nil || deliveries[0].SessionRef.Name != event.SessionName {
			t.Fatalf("adapter did not receive the exact event result once: %+v", deliveries)
		}
		if blocker.cleanupCalls.Load() != 1 {
			t.Fatalf("cleanup attempts = %d, want one still-blocked attempt", blocker.cleanupCalls.Load())
		}
	})
}

func TestServiceStartSerializesMaintenanceAndJoinsCancellation(t *testing.T) {
	service, _, _ := newGatewayServiceFixture(t)
	synctest.Test(t, func(t *testing.T) {
		blocker := blockGatewayServiceMaintenance(service)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		var startErr error
		go func() {
			defer close(done)
			startErr = service.Start(ctx)
		}()
		defer func() {
			cancel()
			blocker.release()
			<-done
			if startErr != nil {
				t.Errorf("Service.Start() returned %v", startErr)
			}
		}()

		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case <-blocker.entered:
		default:
			t.Fatal("retention cleanup did not start at the maintenance tick")
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if blocker.maintenanceCalls.Load() != 1 || blocker.cleanupCalls.Load() != 1 {
			t.Fatalf("maintenance overlapped its blocked pass: maintenance calls = %d, cleanup calls = %d",
				blocker.maintenanceCalls.Load(), blocker.cleanupCalls.Load())
		}

		cancel()
		synctest.Wait()
		select {
		case <-blocker.canceled:
		default:
			t.Fatal("maintenance did not receive the Service.Start cancellation")
		}
		select {
		case <-done:
			t.Fatal("Service.Start returned before its maintenance callback finished")
		default:
		}
		blocker.release()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Service.Start did not finish after its maintenance callback exited")
		}
	})
}

type gatewayMaintenanceBlocker struct {
	store.GatewayDeliveryStore
	maintenanceCalls atomic.Int32
	cleanupCalls     atomic.Int32
	entered          chan struct{}
	canceled         chan struct{}
	exit             chan struct{}
	releaseOnce      sync.Once
}

func blockGatewayServiceMaintenance(service *Service) *gatewayMaintenanceBlocker {
	blocker := &gatewayMaintenanceBlocker{
		GatewayDeliveryStore: service.DeliveryStore,
		entered:              make(chan struct{}), canceled: make(chan struct{}), exit: make(chan struct{}),
	}
	service.Config.Namespace = "default"
	service.Config.PollInterval = time.Second
	service.Config.BatchSize = 1
	service.DeliveryStore = blocker
	service.SessionCleanup = blocker
	service.SessionCleanupCandidates = blocker
	service.SessionCleanupEpochs = blocker
	return blocker
}

func (b *gatewayMaintenanceBlocker) MaintainGatewayRecords(ctx context.Context, namespace string, now, cutoff time.Time) (store.GatewayMaintenanceResult, error) {
	b.maintenanceCalls.Add(1)
	return b.GatewayDeliveryStore.MaintainGatewayRecords(ctx, namespace, now, cutoff)
}

func (*gatewayMaintenanceBlocker) ListGatewaySessionCleanupCandidates(_ context.Context, namespace string, cutoff time.Time) ([]store.GatewaySessionCleanupCandidate, error) {
	return []store.GatewaySessionCleanupCandidate{{
		Namespace: namespace, SessionName: "blocked-retained-session", SessionUID: "blocked-session-uid",
		Proof: store.GatewaySessionCleanupProof{
			GatewayUID: "gateway-uid", BindingUID: "binding-uid", CreatedAt: cutoff.Add(-time.Hour), TerminalCutoff: cutoff,
		},
	}}, nil
}

func (*gatewayMaintenanceBlocker) CurrentFence(context.Context) (store.ControllerEpochFence, error) {
	return store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "maintenance-controller"}, nil
}

func (b *gatewayMaintenanceBlocker) ReclaimGatewaySession(ctx context.Context, _ store.ReclaimGatewaySessionRequest) error {
	first := b.cleanupCalls.Add(1) == 1
	if first {
		close(b.entered)
	}
	<-ctx.Done()
	if first {
		close(b.canceled)
	}
	// Model a bounded runtime mutation which must finish after cancellation
	// before the service can relinquish its lifecycle.
	<-b.exit
	return ctx.Err()
}

func (b *gatewayMaintenanceBlocker) release() {
	b.releaseOnce.Do(func() { close(b.exit) })
}
