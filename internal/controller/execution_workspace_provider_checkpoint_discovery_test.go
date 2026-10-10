package controller

import (
	"context"
	"errors"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestExecutionWorkspaceProviderDeletionDiscoversOptionalCheckpointAPI(t *testing.T) {
	discoveryErr := errors.New("checkpoint discovery unavailable")
	checkpointResource := workspacev1alpha1.GroupVersion.WithResource("executionworkspacecheckpoints").GroupResource()
	for _, test := range []struct {
		name                      string
		checkpointAPI, noMapper   bool
		checkpointReference       bool
		discoveryError, listError error
		wantCheckpointLists       int
		wantProviderProtection    bool
	}{
		{name: "four base APIs permit cleanup without checkpoint API",
			listError: apierrors.NewNotFound(checkpointResource, "")},
		{name: "installed checkpoint API is scanned", checkpointAPI: true, wantCheckpointLists: 1},
		{name: "independent checkpoint protects provider", checkpointAPI: true,
			checkpointReference: true, wantCheckpointLists: 1, wantProviderProtection: true},
		{name: "discovery outage preserves protection", discoveryError: discoveryErr, wantProviderProtection: true},
		{name: "installed API list outage preserves protection", checkpointAPI: true,
			listError: errors.New("checkpoint API unavailable"), wantCheckpointLists: 1, wantProviderProtection: true},
		{name: "installed API not found is not discovery proof", checkpointAPI: true,
			listError: apierrors.NewNotFound(checkpointResource, ""), wantCheckpointLists: 1, wantProviderProtection: true},
		{name: "unknown discovery still scans and fails closed", noMapper: true,
			listError:           apierrors.NewForbidden(checkpointResource, "", errors.New("checkpoint list denied")),
			wantCheckpointLists: 1, wantProviderProtection: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			provider := testGenericProvider("cleanup-provider")
			provider.Finalizers = []string{executionWorkspaceProviderFinalizer}
			now := metav1.Now()
			provider.DeletionTimestamp = &now
			objects := []client.Object{provider}
			if test.checkpointReference {
				objects = append(objects, &workspacev1alpha1.ExecutionWorkspaceCheckpoint{ObjectMeta: metav1.ObjectMeta{
					Namespace: "other-tenant", Name: "independent-data",
					Labels: map[string]string{workspaceCheckpointProviderNameLabel: provider.Name},
				}})
			}
			checkpointLists := 0
			c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithStatusSubresource(provider).
				WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*workspacev1alpha1.ExecutionWorkspaceCheckpointList); ok {
						checkpointLists++
						if test.listError != nil {
							return test.listError
						}
					}
					return c.List(ctx, list, opts...)
				},
			}).Build()
			mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{workspacev1alpha1.GroupVersion})
			mapper.Add(workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspaceProvider"), apimeta.RESTScopeRoot)
			for _, kind := range []string{"ExecutionWorkspacePool", "ExecutionWorkspaceClass", "ExecutionWorkspace"} {
				mapper.Add(workspacev1alpha1.GroupVersion.WithKind(kind), apimeta.RESTScopeNamespace)
			}
			if test.checkpointAPI {
				mapper.Add(workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspaceCheckpoint"), apimeta.RESTScopeNamespace)
			}
			r := &ExecutionWorkspaceProviderReconciler{Client: c, APIReader: c, RESTMapper: mapper, CleanupOnly: true}
			if test.noMapper {
				r.RESTMapper = nil
			}
			if test.discoveryError != nil {
				r.RESTMapper = providerCheckpointDiscoveryErrorMapper{RESTMapper: mapper, err: test.discoveryError}
			}
			key := types.NamespacedName{Name: provider.Name}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			var wantErr error
			if test.discoveryError != nil {
				wantErr = test.discoveryError
			} else if test.wantCheckpointLists != 0 {
				wantErr = test.listError
			}
			if (wantErr == nil && err != nil) || (wantErr != nil && !errors.Is(err, wantErr)) {
				t.Fatalf("Reconcile error = %v, want %v", err, wantErr)
			}
			if checkpointLists != test.wantCheckpointLists {
				t.Fatalf("checkpoint scans = %d, want %d", checkpointLists, test.wantCheckpointLists)
			}
			got := &workspacev1alpha1.ExecutionWorkspaceProvider{}
			err = c.Get(ctx, key, got)
			if test.wantProviderProtection {
				if err != nil || got.UID != provider.UID || len(got.Finalizers) != 1 || got.Finalizers[0] != executionWorkspaceProviderFinalizer {
					t.Fatalf("provider protection was lost: provider=%+v, error=%v", got.ObjectMeta, err)
				}
			} else if !apierrors.IsNotFound(err) {
				t.Fatalf("provider deletion did not finish: %v", err)
			}
		})
	}
}

type providerCheckpointDiscoveryErrorMapper struct {
	apimeta.RESTMapper
	err error
}

func (m providerCheckpointDiscoveryErrorMapper) RESTMapping(schema.GroupKind, ...string) (*apimeta.RESTMapping, error) {
	return nil, m.err
}
