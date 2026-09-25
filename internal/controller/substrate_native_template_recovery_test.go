package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
)

func nativeTemplateRecoveryHarness(t *testing.T) (*nativeSubstrateTemplateStore, *nativeTemplateTestAPI, *corev1alpha1.RuntimePool) {
	t.Helper()
	r, pool := runtimePoolSubstrateTestReconciler(t, nil, &fakeSubstrateActorControl{})
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).Build()
	r.ControllerNamespace = "orka-system"
	api := &nativeTemplateTestAPI{templates: map[string]*ateapipb.ActorTemplate{}}
	r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
		return &workspace.SubstrateNativeClient{Control: api}, nil
	}
	return &nativeSubstrateTemplateStore{r: r}, api, pool
}

func TestNativeSubstrateTemplateRejectedCreateAllowsCorrectedRevision(t *testing.T) {
	for _, code := range []codes.Code{codes.InvalidArgument, codes.FailedPrecondition, codes.Unauthenticated, codes.PermissionDenied} {
		for _, operation := range []string{"create", "update"} {
			t.Run(code.String()+"/"+operation, func(t *testing.T) {
				store, api, pool := nativeTemplateRecoveryHarness(t)
				var previous *unstructured.Unstructured
				if operation == "update" {
					original := nativeSubstrateTestRender(t, store.r, "original")
					require.NoError(t, store.Create(t.Context(), pool, original))
					var err error
					previous, err = store.Get(t.Context(), original.GetNamespace(), original.GetName())
					require.NoError(t, err)
				}
				rejected := nativeSubstrateTestRender(t, store.r, "rejected")
				api.createErr = status.Error(code, "native template was rejected before persistence")
				err := store.put(t.Context(), previous, rejected)
				require.Equal(t, code, status.Code(err))
				_, binding, err := store.read(t.Context(), rejected.GetNamespace(), rejected.GetName())
				require.NoError(t, err)
				require.Nil(t, binding.Pending, "definitive rejection must not reserve the invalid revision")
				if previous != nil {
					require.Equal(t, string(previous.GetUID()), binding.Current.UID)
					previous, err = store.Get(t.Context(), rejected.GetNamespace(), rejected.GetName())
					require.NoError(t, err)
				}
				api.createErr = nil
				nonce := "corrected"
				if code == codes.Unauthenticated || code == codes.PermissionDenied {
					// Control access changes independently of template content.
					// The exact rejected revision must become retryable too.
					nonce = "rejected"
				}
				corrected := nativeSubstrateTestRender(t, store.r, nonce)
				require.NoError(t, store.put(t.Context(), previous, corrected))
				observed, err := store.Get(t.Context(), corrected.GetNamespace(), corrected.GetName())
				require.NoError(t, err)
				require.NotNil(t, observed)
				require.NoError(t, store.Delete(t.Context(), observed))
				require.Empty(t, api.templates, "all committed revisions must remain owned and collectable")
			})
		}
	}
}

func TestNativeSubstrateTemplateMissingAfterAmbiguousCreateKeepsIntent(t *testing.T) {
	store, api, pool := nativeTemplateRecoveryHarness(t)
	first := nativeSubstrateTestRender(t, store.r, "first")
	api.createErr = status.Error(codes.Unavailable, "create outcome unknown")
	require.Error(t, store.Create(t.Context(), pool, first))
	api.createErr = nil
	corrected := nativeSubstrateTestRender(t, store.r, "corrected")
	require.ErrorContains(t, store.Create(t.Context(), pool, corrected), "another Substrate template revision")
	require.ErrorContains(t, store.Create(t.Context(), pool, first), "creation is unresolved")
	require.Equal(t, 1, api.createCalls, "a negative read must not replay an uncertain create")
	_, binding, err := store.read(t.Context(), first.GetNamespace(), first.GetName())
	require.NoError(t, err)
	require.NotNil(t, binding.Pending)
	// The original request commits late. Its exact immutable revision can now
	// be recovered, and only then can a new configuration be materialized.
	native, err := nativeSubstrateRuntimeTemplate(first)
	require.NoError(t, err)
	_, err = api.CreateActorTemplate(t.Context(), &ateapipb.CreateActorTemplateRequest{ActorTemplate: native})
	require.NoError(t, err)
	require.NoError(t, store.Create(t.Context(), pool, first))
	previous, err := store.Get(t.Context(), first.GetNamespace(), first.GetName())
	require.NoError(t, err)
	require.NoError(t, store.Update(t.Context(), previous, corrected))
	observed, err := store.Get(t.Context(), corrected.GetNamespace(), corrected.GetName())
	require.NoError(t, err)
	require.NoError(t, store.Delete(t.Context(), observed))
	require.Empty(t, api.templates, "the late commit must not be orphaned")
}
