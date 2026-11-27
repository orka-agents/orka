package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

type failingAdmissionStore struct {
	store.GatewayEventStore
	err error
}

func (s *failingAdmissionStore) AdmitGatewayEvent(ctx context.Context, request store.GatewayEventAdmission) (*store.GatewayEvent, bool, error) {
	if request.Event.State != store.GatewayEventRejected {
		return nil, false, s.err
	}
	return s.GatewayEventStore.AdmitGatewayEvent(ctx, request)
}

func TestGatewayIngressDoesNotRejectAnEventWhoseAdmissionWasFenced(t *testing.T) {
	service, db, _ := newGatewayServiceFixture(t)
	ctx := context.Background()
	service.EventStore = &failingAdmissionStore{GatewayEventStore: db, err: store.FencedErrorf("the controller was replaced")}
	response, err := service.AdmitEvent(ctx, "default", "chat", "Bearer inbound-token", gatewayEventBody(t, "fenced-event", "user-1"))
	if !errors.Is(err, store.ErrFenced) || response != nil {
		t.Fatalf("fenced admission = %+v, %v; want the fenced error", response, err)
	}
	if responseError, ok := errors.AsType[*HTTPError](err); ok {
		t.Fatalf("fenced admission answered %d %q", responseError.Code, responseError.Message)
	}
	records, err := db.ListGatewayEvents(ctx, store.GatewayEventFilter{Namespace: "default"})
	if err != nil || len(records) != 0 {
		t.Fatalf("fenced admission recorded events %+v, %v", records, err)
	}
}
