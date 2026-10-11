package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestACPSessionCreationReusesBoundNativeIdentity(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart-%t", restart), func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "session.db")
			openPersistence := func() (*sqlite.Store, func() error) {
				db, err := sqlite.NewDB(path)
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				persistence := sqlite.NewStore(db, path)
				cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{7}, 32))
				require.NoError(t, err)
				require.NoError(t, persistence.SetAgentExecutionSnapshotCipher(cipher))
				return persistence, db.Close
			}
			persistence, closeDB := openPersistence()
			snapshot := storetest.NativeSessionSnapshot(t, "staged import survives partial creation")
			snapshot.RuntimeSessionUID, snapshot.RuntimeProfileDigest, snapshot.WorkingDirectory = "", "", ""
			_, err := persistence.StageNativeSessionImport(ctx, store.NativeSessionImport{
				Namespace: "ns", SessionName: "imported", OperationID: "import",
				RequestDigest: store.NativeSessionImportDigest("ns", "imported", snapshot.DataDigest), Snapshot: snapshot,
			})
			require.NoError(t, err)

			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			require.NoError(t, coordinationv1.AddToScheme(scheme))
			baseClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.ControllerEpoch{}, &corev1alpha1.RuntimeSessionControl{}).Build()
			failCreate := true
			kubeClient := interceptor.NewClient(withControllerEpochLeaseUIDs(t, baseClient), interceptor.Funcs{
				Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
					if _, ok := object.(*corev1alpha1.RuntimeSessionControl); ok && failCreate {
						failCreate = false
						return apierrors.NewServerTimeout(schema.GroupResource{Group: "orka.ai", Resource: "runtimesessioncontrols"}, "create", 1)
					}
					return delegate.Create(ctx, object, options...)
				},
			})
			generated := 0
			newContinuity := func() (*ACPSessionContinuity, *kubestore.Store) {
				controls, err := kubestore.NewComposite(kubeClient, "orka-system", persistence)
				require.NoError(t, err)
				continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
					SessionControls: controls, Transcripts: persistence, Publications: controls, BranchClaims: controls,
					NewSessionUID: func() (string, error) {
						generated++
						return fmt.Sprintf("generated-session-%d", generated), nil
					},
				})
				require.NoError(t, err)
				return continuity, controls
			}
			continuity, controls := newContinuity()
			epoch, err := controls.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{
				NewEpoch: 1, HolderID: "controller", RequestDigest: acpSessionTestDigest("epoch"), UpdatedAt: time.Now().UTC(),
			})
			require.NoError(t, err)
			request := ACPEnsureSessionRequest{
				Namespace: "ns", SessionName: "imported", RequireExistingTranscript: true,
				Fence: store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID},
			}
			_, err = continuity.EnsureSession(ctx, request)
			require.True(t, apierrors.IsServerTimeout(err), "must fail after binding, before control creation: %v", err)
			_, err = controls.GetSessionControl(ctx, "ns", "imported")
			require.ErrorIs(t, err, store.ErrNotFound)
			boundUID, err := persistence.GetSessionCleanupIdentity(ctx, "ns", "imported")
			require.NoError(t, err)
			require.Equal(t, "generated-session-1", boundUID)

			mismatch := request
			mismatch.ExpectedSessionUID = "different-session"
			_, err = continuity.EnsureSession(ctx, mismatch)
			require.ErrorIs(t, err, store.ErrConflict, "explicit identity must not override the partial creation")
			if restart {
				require.NoError(t, closeDB())
				persistence, _ = openPersistence()
				continuity, _ = newContinuity()
			}
			control, err := continuity.EnsureSession(ctx, request)
			require.NoError(t, err)
			require.Equal(t, boundUID, control.SessionUID)
			require.Equal(t, 1, generated, "recovery must not generate another Session UID")
			native, err := persistence.GetNativeSession(ctx, "ns", "imported", control.SessionUID)
			require.NoError(t, err)
			require.Equal(t, snapshot.Data, native.Snapshot.Data)
			_, err = continuity.EnsureSession(ctx, mismatch)
			require.ErrorIs(t, err, store.ErrConflict)
		})
	}
}

type failingCleanupIdentityStore struct {
	*sqlite.Store
	err error
}

func (s *failingCleanupIdentityStore) GetSessionCleanupIdentity(context.Context, string, string) (string, error) {
	return "", s.err
}

func TestACPSessionCreationFailsClosedOnCleanupIdentityReadError(t *testing.T) {
	persistence, fence, closeDB := newACPSessionTestStore(t, ":memory:")
	t.Cleanup(closeDB)
	readErr := errors.New("cleanup identity unavailable")
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: persistence, Transcripts: &failingCleanupIdentityStore{Store: persistence, err: readErr},
		Publications: persistence, BranchClaims: persistence,
		NewSessionUID: func() (string, error) {
			t.Fatal("identity read failure must not generate a new UID")
			return "", nil
		},
	})
	require.NoError(t, err)
	_, err = continuity.EnsureSession(t.Context(), ACPEnsureSessionRequest{Namespace: "ns", SessionName: "unavailable", Fence: fence})
	require.ErrorIs(t, err, readErr)
	_, err = persistence.GetSessionControl(t.Context(), "ns", "unavailable")
	require.ErrorIs(t, err, store.ErrNotFound)
}
