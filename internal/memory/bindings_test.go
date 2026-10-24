package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orka-agents/orka/internal/apierror"
	"github.com/orka-agents/orka/internal/store"
)

type bindingPageStore struct {
	store.GovernedMemoryStore
	bindings []store.MemoryBackendBinding
	pages    int
}

func (s *bindingPageStore) GetMemoryBackendBinding(context.Context, string) (*store.MemoryBackendBinding, error) {
	return nil, store.ErrNotFound
}

func (s *bindingPageStore) ListMemoryBackendBindings(
	_ context.Context,
	filter store.MemoryBackendBindingFilter,
) ([]store.MemoryBackendBinding, error) {
	s.pages++
	limit := filter.Limit
	if limit <= 0 {
		limit = memoryBindingPageSize
	}
	result := make([]store.MemoryBackendBinding, 0, limit)
	for _, binding := range s.bindings {
		if binding.NamespaceUID <= filter.BeforeNamespaceUID ||
			len(filter.Modes) > 0 && !slices.Contains(filter.Modes, binding.Mode) ||
			len(filter.States) > 0 && !slices.Contains(filter.States, binding.State) {
			continue
		}
		result = append(result, binding)
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func TestBindingPaginationDoesNotCapReadinessScan(t *testing.T) {
	bindings := make([]store.MemoryBackendBinding, 0, memoryBindingPageSize+1)
	for i := 0; i <= memoryBindingPageSize; i++ {
		bindings = append(bindings, store.MemoryBackendBinding{
			NamespaceUID: fmt.Sprintf("uid-%04d", i), Mode: store.MemoryBackendModeRemote,
			MinimumFeatureEpoch: 2,
		})
	}
	bindings[len(bindings)-1].MinimumFeatureEpoch = 3
	governed := &bindingPageStore{bindings: bindings}
	if err := CheckFeatureEpochReadiness(context.Background(), governed, 2, true); err == nil {
		t.Fatal("readiness accepted an incompatible binding beyond the first page")
	}
	if governed.pages < 2 {
		t.Fatalf("pages = %d, want pagination beyond first page", governed.pages)
	}
}

func TestFeatureEpochReadinessFailsWhenFeatureDisabledAndBindingExists(t *testing.T) {
	governed := &bindingPageStore{bindings: []store.MemoryBackendBinding{{
		NamespaceUID: "uid-0001", Mode: store.MemoryBackendModeRemote,
	}}}
	if err := CheckFeatureEpochReadiness(context.Background(), governed, 2, false); err == nil {
		t.Fatal("readiness accepted a remote binding while the feature was disabled")
	}
}

func TestBackendResolverRejectsHistoricalNamespaceIncarnationBeyondFirstPage(t *testing.T) {
	const namespace = "team-a"
	bindings := make([]store.MemoryBackendBinding, 0, memoryBindingPageSize+1)
	for i := range memoryBindingPageSize {
		bindings = append(bindings, store.MemoryBackendBinding{
			Namespace: fmt.Sprintf("team-%04d", i), NamespaceUID: fmt.Sprintf("uid-%04d", i),
			Mode: store.MemoryBackendModeRemote,
		})
	}
	bindings = append(bindings, store.MemoryBackendBinding{
		Namespace: namespace, NamespaceUID: "uid-9999", Mode: store.MemoryBackendModeRemote,
	})
	governed := &bindingPageStore{bindings: bindings}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID("current-uid")},
	}).Build()
	resolver := &BackendResolver{Reader: reader, Store: governed}
	_, err := resolver.ResolveLocal(context.Background(), namespace)
	var structured *apierror.Error
	if !errors.As(err, &structured) || structured.Reason != ReasonIdentityMismatch {
		t.Fatalf("ResolveLocal() error = %#v, want identity mismatch", err)
	}
	if governed.pages < 2 {
		t.Fatalf("pages = %d, want historical-incarnation pagination", governed.pages)
	}
}
