package v1alpha1

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	types "k8s.io/apimachinery/pkg/types"
)

func TestWorkspaceResourcesRoundTrip(t *testing.T) {
	t.Parallel()

	now := metav1.NewTime(time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC))
	lifecycle := testLifecycle()
	objects := []runtime.Object{
		&ExecutionWorkspaceProvider{
			TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "ExecutionWorkspaceProvider"},
			ObjectMeta: metav1.ObjectMeta{Name: "fake"},
			Spec: ExecutionWorkspaceProviderSpec{
				ControllerName:    "fake.workspace.orka.ai/v1",
				ParametersRef:     TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakeProviderConfig", Name: "default"},
				LifecycleState:    ExecutionWorkspaceProviderActive,
				RequiredContracts: []string{ContractVersionV1},
			},
			Status: ExecutionWorkspaceProviderStatus{
				ObservedGeneration: 1,
				Adapter:            &ExecutionWorkspaceAdapterStatus{Version: "1.0.0"},
				SupportedFeatures:  []ExecutionWorkspaceFeature{WorkspaceFeatureExec},
				LastHeartbeat:      &now,
			},
		},
		&ExecutionWorkspacePool{
			TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "ExecutionWorkspacePool"},
			ObjectMeta: metav1.ObjectMeta{Name: "coding", Namespace: "default"},
			Spec: ExecutionWorkspacePoolSpec{
				ProviderRef:   ClusterObjectReference{Name: "fake"},
				ParametersRef: TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakePoolParameters", Name: "coding"},
				Capacity:      ExecutionWorkspacePoolCapacity{MinReady: 2, MaxSize: 10},
			},
			Status: ExecutionWorkspacePoolStatus{Available: 2, Allocated: 1, Total: 3},
		},
		&ExecutionWorkspaceClass{
			TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "ExecutionWorkspaceClass"},
			ObjectMeta: metav1.ObjectMeta{Name: "coding-v1", Namespace: "default"},
			Spec: ExecutionWorkspaceClassSpec{
				PoolRef:            &LocalObjectReference{Name: "coding"},
				Mode:               ExecutionWorkspaceModeInteractive,
				RequiredFeatures:   []ExecutionWorkspaceFeature{WorkspaceFeatureExec, WorkspaceFeatureFiles},
				AllowedReuseScopes: []WorkspaceReuseScope{WorkspaceReuseScopeNone, WorkspaceReuseScopeSession},
				Lifecycle:          lifecycle,
			},
		},
		&ExecutionWorkspace{
			TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "ExecutionWorkspace"},
			ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: "default"},
			Spec: ExecutionWorkspaceSpec{
				Mode:            ExecutionWorkspaceModeInteractive,
				ClassBinding:    ImmutableObjectBinding{Name: "coding-v1", UID: types.UID("class-uid"), Generation: 1, ProfileHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				ProviderBinding: ImmutableObjectBinding{Name: "fake", UID: types.UID("provider-uid"), Generation: 1},
				Slot:            "default",
				DesiredState:    ExecutionWorkspaceDesiredReady,
				Lifecycle:       lifecycle,
				Attachment:      testAttachment(now),
			},
			Status: ExecutionWorkspaceStatus{State: ExecutionWorkspaceStateAttached, AttachedEpoch: 1},
		},
	}

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	codecs := serializer.NewCodecFactory(scheme)
	decoder := codecs.UniversalDeserializer()

	for _, object := range objects {
		t.Run(reflect.TypeOf(object).Elem().Name(), func(t *testing.T) {
			raw, err := json.Marshal(object)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			decoded, gvk, err := decoder.Decode(raw, nil, nil)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if gvk.GroupVersion() != GroupVersion {
				t.Fatalf("decoded GVK = %s, want group version %s", gvk, GroupVersion)
			}
			roundTripped, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("marshal decoded object: %v", err)
			}
			if !bytes.Equal(raw, roundTripped) {
				t.Fatalf("round-trip JSON mismatch\nwant: %s\n got: %s", raw, roundTripped)
			}
		})
	}
}

func testAttachment(expiresAt metav1.Time) *ExecutionWorkspaceAttachment {
	attachment := &ExecutionWorkspaceAttachment{
		TaskRef:     ObjectIdentityReference{Name: "task-1", UID: types.UID("task-uid")},
		Epoch:       1,
		TokenSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ExpiresAt:   expiresAt,
	}
	attachment.TokenSecretRef.Name = "task-1-attachment-1"
	return attachment
}

func testLifecycle() ExecutionWorkspaceLifecycle {
	return ExecutionWorkspaceLifecycle{
		DefaultOnDetach: WorkspaceOnDetachSuspend,
		AllowedOnDetach: []WorkspaceOnDetach{WorkspaceOnDetachSuspend, WorkspaceOnDetachDelete},
		DetachTimeout:   metav1.Duration{Duration: 2 * time.Minute},
		IdleTimeout:     &metav1.Duration{Duration: 30 * time.Minute},
		MaxLifetime:     &metav1.Duration{Duration: 24 * time.Hour},
		DeletionPolicy: ExecutionWorkspaceDeletionPolicy{
			ProviderResources: WorkspaceDeletionActionDelete,
			PersistentVolumes: WorkspaceDeletionActionRetain,
			Checkpoints:       WorkspaceDeletionActionDelete,
		},
	}
}
