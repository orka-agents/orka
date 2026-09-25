package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

type mcpAuthorityFailureReader struct {
	client.Reader
	source string
	err    error
}

func (r mcpAuthorityFailureReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if r.source == "task list" {
		return r.err
	}
	return r.Reader.List(ctx, list, opts...)
}

func (r mcpAuthorityFailureReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	switch object.(type) {
	case *corev1alpha1.AgentRuntime:
		if r.source == "runtime" {
			return r.err
		}
	case *corev1.Secret:
		if r.source == "auth secret" {
			return r.err
		}
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

type mcpAuthorityFailureSnapshots struct {
	store.AgentExecutionSnapshotStore
	err error
}

func (s mcpAuthorityFailureSnapshots) GetAgentExecutionSnapshot(context.Context, store.AgentExecutionSnapshotKey) (*store.AgentExecutionSnapshot, error) {
	return nil, s.err
}

func TestKubernetesMCPAuthorityReadFailuresRemainDistinguishable(t *testing.T) {
	for _, source := range []string{"task list", "runtime", "auth secret", "snapshot"} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{name: "unavailable", err: errors.New("simulated authority read outage")},
			{name: "timeout", err: apierrors.NewTimeoutError("simulated authority read timeout", 1)},
			{name: "missing", err: apierrors.NewNotFound(schema.GroupResource{Resource: "authority"}, "missing")},
		} {
			t.Run(source+"/"+failure.name, func(t *testing.T) {
				f, _, request, resolver := newSnapshotBackedExternalMCPResolver(t)
				_, err := resolver.ResolveACPMCPBrokerCredentials(f.ctx, request)
				require.NoError(t, err)
				if source == "snapshot" {
					if failure.name == "missing" {
						failure.err = store.ErrNotFound
					}
					resolver.AgentExecutionSnapshots = mcpAuthorityFailureSnapshots{AgentExecutionSnapshotStore: resolver.AgentExecutionSnapshots, err: failure.err}
				} else {
					resolver.Reader = mcpAuthorityFailureReader{Reader: resolver.Reader, source: source, err: failure.err}
				}
				_, err = resolver.ResolveACPMCPBrokerCredentials(f.ctx, request)
				require.Error(t, err)
				if failure.name == "missing" {
					require.NotErrorIs(t, err, errACPMCPAuthorityUnavailable)
				} else {
					require.ErrorIs(t, err, errACPMCPAuthorityUnavailable)
					require.ErrorIs(t, err, failure.err)
				}
			})
		}
	}
}

func TestKubernetesMCPAuthorityDriftRemainsDefinitive(t *testing.T) {
	f, _, request, resolver := newSnapshotBackedExternalMCPResolver(t)
	changed := f.runtime.DeepCopy()
	changed.Generation++
	require.NoError(t, f.client.Update(f.ctx, changed))
	_, err := resolver.ResolveACPMCPBrokerCredentials(f.ctx, request)
	require.Error(t, err)
	require.NotErrorIs(t, err, errACPMCPAuthorityUnavailable)
}
