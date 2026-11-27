package store

import (
	"context"
	"time"
)

type SessionCleanupStore interface {
	ReclaimSession(ctx context.Context, request ReclaimSessionRequest) error
}

type SessionCleanupRecoveryStore interface {
	ResumeSessionCleanups(ctx context.Context, fence ControllerEpochFence) error
}

type SessionRuntimeCleanupFunc func(context.Context, SessionCleanupIntent, ControllerEpochFence) error

type ReclaimSessionRequest struct {
	Namespace       string               `json:"namespace"`
	SessionName     string               `json:"sessionName"`
	Fence           ControllerEpochFence `json:"fence"`
	OperationID     string               `json:"operationId"`
	OperationDigest string               `json:"operationDigest"`
	RequestedAt     time.Time            `json:"requestedAt"`
}
