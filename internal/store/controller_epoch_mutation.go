package store

import "context"

type ControllerEpochMutationStore interface {
	WithControllerEpochMutation(context.Context, ControllerEpochFence, func(context.Context) error) error
}
