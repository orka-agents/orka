package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const GatewaySessionCleanupOperationPrefix = "gateway-retention-v1-"

type GatewaySessionCleanupStore interface {
	ReclaimGatewaySession(context.Context, ReclaimGatewaySessionRequest) error
}

type ReclaimGatewaySessionRequest struct {
	Session     GatewaySessionCleanupCandidate
	Fence       ControllerEpochFence
	RequestedAt time.Time
}

func GatewaySessionCleanupOperation(namespace, sessionName, sessionUID, gatewayUID, bindingUID string) (string, string) {
	encoded, _ := json.Marshal([]string{"orka.gateway.session-retention.v1", namespace, sessionName, sessionUID, gatewayUID, bindingUID})
	digest := CanonicalBytesDigest(encoded)
	return GatewaySessionCleanupOperationPrefix + strings.TrimPrefix(digest, "sha256:"), digest
}
