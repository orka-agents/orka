package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/store"
)

var errACPMCPAuthorityUnavailable = errors.New("MCP authority could not be read")

// Mark failures at the read boundary, where an unavailable backend can still be
// distinguished from the identity and policy checks that consume its result.
// A missing or invalid record is definitive; an unreadable one is not.
func acpMCPAuthorityReadError(err error) error {
	if err == nil || apierrors.IsNotFound(err) || errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrValidation) {
		return err
	}
	return fmt.Errorf("%w: %w", errACPMCPAuthorityUnavailable, err)
}

type acpMCPAuthorityReader struct{ client.Reader }

func (r acpMCPAuthorityReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	return acpMCPAuthorityReadError(r.Reader.Get(ctx, key, object, opts...))
}

func (r acpMCPAuthorityReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return acpMCPAuthorityReadError(r.Reader.List(ctx, list, opts...))
}

type acpMCPAuthoritySnapshots struct {
	store.AgentExecutionSnapshotStore
}

func (s acpMCPAuthoritySnapshots) GetAgentExecutionSnapshot(ctx context.Context, key store.AgentExecutionSnapshotKey) (*store.AgentExecutionSnapshot, error) {
	snapshot, err := s.AgentExecutionSnapshotStore.GetAgentExecutionSnapshot(ctx, key)
	return snapshot, acpMCPAuthorityReadError(err)
}
