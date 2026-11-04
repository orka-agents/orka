package v1

import (
	"context"
	"errors"
)

// ErrTaskJobRevoked rejects authority from a completed or superseded Job.
var ErrTaskJobRevoked = errors.New("task Job authority revoked")

type TaskJobIdentity struct {
	Namespace string
	TaskUID   string
	JobUID    string
}

// TaskJobAuthorityStore revokes a Job before its Task binding or active state
// changes. Revocations survive ordinary data cleanup and are reclaimed only by
// the Task finalizer, together with a fresh cleanup generation.
type TaskJobAuthorityStore interface {
	RevokeTaskJob(context.Context, TaskJobIdentity) error
	CheckTaskJobAuthority(context.Context, TaskJobIdentity) error
	DeleteTaskJobRevocations(context.Context, string, string, string) error
}
