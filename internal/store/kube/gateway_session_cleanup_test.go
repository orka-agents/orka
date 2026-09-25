package kube

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	controlstore "github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReclaimGatewaySessionArchivesTurnsAndPreservesUnrelatedState(t *testing.T) {
	ctx := context.Background()
	s, kubeClient, persistence, db, fence := newSessionCleanupTestStore(t, nil)
	f := seedGatewaySessionCleanupState(t, ctx, s, kubeClient, persistence, db, fence, "gateway-complete")
	otherControl, otherClaim := seedSessionCleanupState(t, ctx, s, persistence, fence, "unrelated-session")
	otherSession, err := persistence.GetSession(ctx, otherControl.Namespace, otherControl.SessionName)
	require.NoError(t, err)
	otherLease := &coordinationv1.Lease{}
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKey{Namespace: otherControl.Namespace, Name: runtimeSessionLeaseName(otherControl.SessionUID)}, otherLease))
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.control.Namespace, Name: "shared-runtime-pool", UID: "shared-pool-uid"},
		Spec:       corev1alpha1.RuntimePoolSpec{DesiredReplicas: 1},
	}
	require.NoError(t, kubeClient.Create(ctx, pool))
	poolBefore := pool.DeepCopy()

	var calls int
	s.sessionRuntimeCleanup = func(callCtx context.Context, intent controlstore.SessionCleanupIntent, actual controlstore.ControllerEpochFence) error {
		calls++
		require.Equal(t, fence, actual)
		assertGatewayCleanupRuntimeProof(t, callCtx, persistence, intent, f)
		_, err := s.GetSessionControl(callCtx, f.control.Namespace, f.control.SessionName)
		require.NoError(t, err, "Session authority must survive until runtime retirement")
		return nil
	}
	publicRequest := gatewayPublicSessionCleanupRequest(f, fence)
	publicRequest.OperationID = "ordinary-delete-gateway"
	publicRequest.OperationDigest = testDigest(publicRequest.OperationID)
	require.ErrorIs(t, s.ReclaimSession(ctx, publicRequest), controlstore.ErrGatewayOwnedSession)
	require.Zero(t, calls)

	require.NoError(t, s.ReclaimGatewaySession(ctx, f.request))
	require.Equal(t, 1, calls)
	assertGatewaySessionCleanupComplete(t, ctx, s, kubeClient, persistence, db, f)
	require.NoError(t, s.ReclaimGatewaySession(ctx, f.request), "completed retention must be idempotent")
	require.Equal(t, 1, calls, "completed cleanup must not retire a runtime again")
	require.ErrorIs(t, s.ReclaimSession(ctx, gatewayPublicSessionCleanupRequest(f, fence)), controlstore.ErrGatewayOwnedSession)

	gotSession, err := persistence.GetSession(ctx, otherControl.Namespace, otherControl.SessionName)
	require.NoError(t, err)
	require.Equal(t, otherSession, gotSession)
	gotControl, err := s.GetSessionControl(ctx, otherControl.Namespace, otherControl.SessionName)
	require.NoError(t, err)
	require.Equal(t, otherControl, gotControl)
	gotClaim, err := s.GetBranchClaim(ctx, otherClaim.ID)
	require.NoError(t, err)
	require.Equal(t, otherClaim, gotClaim)
	gotLease := &coordinationv1.Lease{}
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(otherLease), gotLease))
	require.Equal(t, otherLease, gotLease)
	gotPool := &corev1alpha1.RuntimePool{}
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(poolBefore), gotPool))
	require.Equal(t, poolBefore, gotPool)
}

func TestResumeGatewaySessionCleanupAfterRuntimeFailure(t *testing.T) {
	ctx := context.Background()
	s, kubeClient, persistence, db, fence := newSessionCleanupTestStore(t, nil)
	f := seedGatewaySessionCleanupState(t, ctx, s, kubeClient, persistence, db, fence, "gateway-runtime-retry")
	wantErr := errors.New("runtime retirement interrupted")
	var calls int
	s.sessionRuntimeCleanup = func(callCtx context.Context, intent controlstore.SessionCleanupIntent, actual controlstore.ControllerEpochFence) error {
		calls++
		require.Equal(t, fence, actual)
		assertGatewayCleanupRuntimeProof(t, callCtx, persistence, intent, f)
		return wantErr
	}
	require.ErrorIs(t, s.ReclaimGatewaySession(ctx, f.request), wantErr)
	require.Equal(t, 1, calls)
	_, err := s.GetSessionControl(ctx, f.control.Namespace, f.control.SessionName)
	require.NoError(t, err)
	_, err = s.GetBranchClaim(ctx, f.claim.ID)
	require.NoError(t, err)
	assertGatewaySessionCleanupUncommitted(t, ctx, s, persistence, f)
	// Even knowledge of the exact Gateway operation does not authorize the
	// general-purpose Session deletion endpoint to resume it.
	require.ErrorIs(t, s.ReclaimSession(ctx, gatewayPublicSessionCleanupRequest(f, fence)), controlstore.ErrGatewayOwnedSession)
	require.Equal(t, 1, calls)

	epoch, err := s.GetControllerEpoch(ctx, fence.Name)
	require.NoError(t, err)
	next, err := s.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: epoch.Name, ExpectedVersion: epoch.Version, ExpectedEpoch: epoch.Epoch, NewEpoch: epoch.Epoch + 1,
		HolderID: "gateway-cleanup-successor", UpdatedAt: f.request.RequestedAt.Add(time.Minute),
		RequestDigest: testDigest("gateway-cleanup-successor"),
	})
	require.NoError(t, err)
	nextFence := controlstore.ControllerEpochFence{Name: next.Name, Epoch: next.Epoch, HolderID: next.HolderID}
	restarted, err := NewComposite(kubeClient, testControlNamespace, persistence,
		WithSessionRuntimeCleanup(func(callCtx context.Context, intent controlstore.SessionCleanupIntent, actual controlstore.ControllerEpochFence) error {
			calls++
			require.Equal(t, nextFence, actual)
			assertGatewayCleanupRuntimeProof(t, callCtx, persistence, intent, f)
			return nil
		}),
	)
	require.NoError(t, err)
	require.NoError(t, restarted.ResumeSessionCleanups(ctx, nextFence))
	require.Equal(t, 2, calls)
	assertGatewaySessionCleanupComplete(t, ctx, restarted, kubeClient, persistence, db, f)
}

func TestResumeGatewaySessionCleanupAfterStorageRollback(t *testing.T) {
	ctx := context.Background()
	s, kubeClient, persistence, db, fence := newSessionCleanupTestStore(t, nil)
	f := seedGatewaySessionCleanupState(t, ctx, s, kubeClient, persistence, db, fence, "gateway-storage-retry")
	var calls int
	cleanup := func(callCtx context.Context, intent controlstore.SessionCleanupIntent, actual controlstore.ControllerEpochFence) error {
		calls++
		require.Equal(t, fence, actual)
		assertGatewayCleanupRuntimeProof(t, callCtx, persistence, intent, f)
		return nil
	}
	s.sessionRuntimeCleanup = cleanup
	// Fail after receipt insertion and turn/outbox deletion. The transaction
	// must retain all SQLite recovery evidence despite completed runtime and
	// Kubernetes ownership cleanup.
	execSessionCleanupMutation(t, ctx, db, `CREATE TRIGGER fail_gateway_cleanup BEFORE DELETE ON sessions
		BEGIN SELECT RAISE(ABORT, 'injected Gateway cleanup storage failure'); END`)
	err := s.ReclaimGatewaySession(ctx, f.request)
	require.ErrorContains(t, err, "injected Gateway cleanup storage failure")
	require.Equal(t, 1, calls)
	_, err = s.GetSessionControl(ctx, f.control.Namespace, f.control.SessionName)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	_, err = s.GetBranchClaim(ctx, f.claim.ID)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	assertGatewaySessionCleanupUncommitted(t, ctx, s, persistence, f)

	execSessionCleanupMutation(t, ctx, db, `DROP TRIGGER fail_gateway_cleanup`)
	restarted, err := NewComposite(kubeClient, testControlNamespace, persistence, WithSessionRuntimeCleanup(cleanup))
	require.NoError(t, err)
	require.NoError(t, restarted.ResumeSessionCleanups(ctx, fence))
	require.Equal(t, 2, calls)
	assertGatewaySessionCleanupComplete(t, ctx, restarted, kubeClient, persistence, db, f)
}

func TestReclaimGatewaySessionRejectsChangedCandidateBeforeRuntime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*controlstore.GatewaySessionCleanupCandidate)
	}{
		{"Session UID", func(candidate *controlstore.GatewaySessionCleanupCandidate) {
			candidate.SessionUID = "replacement-session-uid"
		}},
		{"Gateway UID", func(candidate *controlstore.GatewaySessionCleanupCandidate) {
			candidate.Proof.GatewayUID = "replacement-gateway-uid"
		}},
		{"Binding UID", func(candidate *controlstore.GatewaySessionCleanupCandidate) {
			candidate.Proof.BindingUID = "replacement-binding-uid"
		}},
		{"transcript incarnation", func(candidate *controlstore.GatewaySessionCleanupCandidate) {
			candidate.Proof.CreatedAt = candidate.Proof.CreatedAt.Add(time.Second)
		}},
		{"retained turn", func(candidate *controlstore.GatewaySessionCleanupCandidate) {
			candidate.Proof.TerminalCutoff = testNow.Add(30 * time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, kubeClient, persistence, db, fence := newSessionCleanupTestStore(t, nil)
			f := seedGatewaySessionCleanupState(t, ctx, s, kubeClient, persistence, db, fence, "gateway-stale-candidate")
			var calls int
			s.sessionRuntimeCleanup = func(context.Context, controlstore.SessionCleanupIntent, controlstore.ControllerEpochFence) error {
				calls++
				return nil
			}
			request := f.request
			tc.mutate(&request.Session)
			require.ErrorIs(t, s.ReclaimGatewaySession(ctx, request), controlstore.ErrConflict)
			require.Zero(t, calls)
			_, err := persistence.GetSessionCleanupIntent(ctx, f.control.Namespace, f.control.SessionName)
			require.ErrorIs(t, err, controlstore.ErrNotFound)
			control, err := s.GetSessionControl(ctx, f.control.Namespace, f.control.SessionName)
			require.NoError(t, err)
			require.Equal(t, f.control, control)
			claim, err := s.GetBranchClaim(ctx, f.claim.ID)
			require.NoError(t, err)
			require.Equal(t, f.claim, claim)
			_, err = persistence.GetSessionTurn(ctx, f.turn.ID)
			require.NoError(t, err)
		})
	}
}

func TestResumeGatewaySessionCleanupPreservesReplacement(t *testing.T) {
	for _, kind := range []string{"transcript", "control object"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, kubeClient, persistence, db, fence := newSessionCleanupTestStore(t, nil)
			f := seedGatewaySessionCleanupState(t, ctx, s, kubeClient, persistence, db, fence, "gateway-replaced")
			interrupted := errors.New("controller stopped after intent")
			s.sessionRuntimeCleanup = func(context.Context, controlstore.SessionCleanupIntent, controlstore.ControllerEpochFence) error {
				return interrupted
			}
			require.ErrorIs(t, s.ReclaimGatewaySession(ctx, f.request), interrupted)

			var replacement *corev1alpha1.RuntimeSessionControl
			replacementCreatedAt := testNow.Add(2 * time.Hour)
			if kind == "transcript" {
				execSessionCleanupMutation(t, ctx, db, `UPDATE sessions SET created_at = ? WHERE namespace = ? AND name = ?`,
					replacementCreatedAt, f.control.Namespace, f.control.SessionName)
			} else {
				original, err := s.getSessionControlObject(ctx, f.control.Namespace, f.control.SessionName)
				require.NoError(t, err)
				require.NoError(t, kubeClient.Delete(ctx, original))
				replacement = original.DeepCopy()
				replacement.UID = "replacement-control-object-uid"
				replacement.ResourceVersion = ""
				require.NoError(t, kubeClient.Create(ctx, replacement))
			}
			restarted, err := NewComposite(kubeClient, testControlNamespace, persistence)
			require.NoError(t, err)
			require.ErrorIs(t, restarted.ResumeSessionCleanups(ctx, fence), controlstore.ErrConflict)
			assertGatewaySessionCleanupUncommitted(t, ctx, restarted, persistence, f)
			if kind == "transcript" {
				session, err := persistence.GetSession(ctx, f.control.Namespace, f.control.SessionName)
				require.NoError(t, err)
				require.True(t, session.CreatedAt.Equal(replacementCreatedAt), "replacement transcript was changed")
			} else {
				current := &corev1alpha1.RuntimeSessionControl{}
				require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(replacement), current))
				require.Equal(t, replacement, current)
			}
		})
	}
}

type gatewaySessionCleanupFixture struct {
	control    *controlstore.SessionControl
	claim      *controlstore.BranchClaim
	turn       *controlstore.SessionTurn
	projection *controlstore.OutboxProjection
	request    controlstore.ReclaimGatewaySessionRequest
}

func seedGatewaySessionCleanupState(t *testing.T, ctx context.Context, s *Store, kubeClient client.Client, persistence *sqlitestore.Store, db *sql.DB, fence controlstore.ControllerEpochFence, name string) gatewaySessionCleanupFixture {
	t.Helper()
	control, claim := seedPublishedSessionCleanupState(t, ctx, s, persistence, db, fence, name)
	proof := controlstore.GatewaySessionCleanupProof{
		GatewayUID: "gateway-owner-uid", BindingUID: "gateway-binding-uid", CreatedAt: testNow, TerminalCutoff: testNow.Add(30 * time.Minute),
	}
	// Model an earlier retention pass that removed bounded message/event
	// history but left the finalized ACP turn and its Session foreign key.
	execSessionCleanupMutation(t, ctx, db, `UPDATE sessions SET session_type = 'gateway', owner_type = 'gateway', owner_ref = ?, updated_at = ?
		WHERE namespace = ? AND name = ?`, proof.GatewayUID+"/"+proof.BindingUID, testNow.Add(2*time.Minute), control.Namespace, name)
	object, err := s.getSessionControlObject(ctx, control.Namespace, control.SessionName)
	require.NoError(t, err)
	object.UID = types.UID(name + "-control-object-uid")
	require.NoError(t, kubeClient.Update(ctx, object))
	control, err = s.GetSessionControl(ctx, control.Namespace, control.SessionName)
	require.NoError(t, err)
	var turnID string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM session_turns WHERE namespace = ? AND session_name = ?`, control.Namespace, name).Scan(&turnID))
	turn, err := persistence.GetSessionTurn(ctx, turnID)
	require.NoError(t, err)
	projection, err := persistence.GetOutboxProjection(ctx, turn.ProjectionID)
	require.NoError(t, err)
	execSessionCleanupMutation(t, ctx, db, `INSERT INTO gateway_event_tombstones
		(namespace, gateway_uid, external_event_id, event_id, task_name, task_uid, envelope_digest, session_name, transcript_order, expires_at, created_at)
		VALUES (?, ?, ?, ?, 'published-task', ?, ?, ?, 1, ?, ?)`,
		control.Namespace, proof.GatewayUID, name+"-external-event", name+"-event", turn.Key.TaskUID,
		testDigest(name+"-envelope"), name, testNow.Add(48*time.Hour), testNow.Add(time.Hour))
	page, err := persistence.ListGatewaySessionCleanupCandidates(ctx, controlstore.GatewaySessionCleanupFilter{
		Namespace: control.Namespace, TerminalCutoff: proof.TerminalCutoff, Limit: 25,
	})
	require.NoError(t, err)
	candidates := page.Candidates
	require.Equal(t, []controlstore.GatewaySessionCleanupCandidate{{
		Namespace: control.Namespace, SessionName: name, SessionUID: control.SessionUID, Proof: proof,
	}}, candidates, "retention must still find the Session after its source event was compacted")
	return gatewaySessionCleanupFixture{
		control: control, claim: claim, turn: turn, projection: projection,
		request: controlstore.ReclaimGatewaySessionRequest{
			Session: candidates[0],
			Fence:   fence, RequestedAt: testNow.Add(time.Hour),
		},
	}
}

func gatewayPublicSessionCleanupRequest(f gatewaySessionCleanupFixture, fence controlstore.ControllerEpochFence) controlstore.ReclaimSessionRequest {
	operationID, digest := controlstore.GatewaySessionCleanupOperation(f.control.Namespace, f.control.SessionName, f.control.SessionUID, f.request.Session.Proof.GatewayUID, f.request.Session.Proof.BindingUID)
	return controlstore.ReclaimSessionRequest{
		Namespace: f.control.Namespace, SessionName: f.control.SessionName, Fence: fence,
		OperationID: operationID, OperationDigest: digest, RequestedAt: f.request.RequestedAt,
	}
}

func assertGatewayCleanupRuntimeProof(t *testing.T, ctx context.Context, persistence *sqlitestore.Store, intent controlstore.SessionCleanupIntent, f gatewaySessionCleanupFixture) {
	t.Helper()
	require.Equal(t, f.control.SessionUID, intent.SessionUID)
	require.Equal(t, f.control.SessionName, intent.SessionName)
	require.Equal(t, f.control.Namespace, intent.Namespace)
	require.Equal(t, f.control.SessionName+"-control-object-uid", intent.ControlObjectUID)
	require.Equal(t, f.control.RequestDigest, intent.ControlRequestDigest)
	require.NotNil(t, intent.Gateway)
	require.Equal(t, f.request.Session.Proof, *intent.Gateway)
	saved, err := persistence.GetSessionCleanupIntent(ctx, intent.Namespace, intent.SessionName)
	require.NoError(t, err)
	require.Equal(t, intent, *saved, "runtime callback must receive the durable intent")
	turns, err := persistence.ListSessionCleanupTurns(ctx, intent)
	require.NoError(t, err)
	require.Len(t, turns, 1)
	require.Equal(t, f.turn.Key, turns[0].Key)
	require.Empty(t, turns[0].UserPrompt)
	require.Empty(t, turns[0].TerminalContent)
}

func assertGatewaySessionCleanupUncommitted(t *testing.T, ctx context.Context, s *Store, persistence *sqlitestore.Store, f gatewaySessionCleanupFixture) {
	t.Helper()
	_, err := persistence.GetSession(ctx, f.control.Namespace, f.control.SessionName)
	require.NoError(t, err)
	_, err = persistence.GetSessionTurn(ctx, f.turn.ID)
	require.NoError(t, err)
	_, err = persistence.GetOutboxProjection(ctx, f.projection.ID)
	require.NoError(t, err)
	intent, err := persistence.GetSessionCleanupIntent(ctx, f.control.Namespace, f.control.SessionName)
	require.NoError(t, err)
	require.NotNil(t, intent.Gateway)
	require.Equal(t, f.request.Session.Proof, *intent.Gateway)
	_, err = persistence.GetSessionCleanupCompletion(ctx, f.control.Namespace, f.control.SessionName)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	_, err = s.GetSessionTurnCleanupReceipt(ctx, f.control.Namespace, f.control.SessionName, f.turn.PromptAttemptID)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
}

func assertGatewaySessionCleanupComplete(t *testing.T, ctx context.Context, s *Store, kubeClient client.Client, persistence *sqlitestore.Store, db *sql.DB, f gatewaySessionCleanupFixture) {
	t.Helper()
	_, err := persistence.GetSession(ctx, f.control.Namespace, f.control.SessionName)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	_, err = persistence.GetSessionTurn(ctx, f.turn.ID)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	_, err = persistence.GetOutboxProjection(ctx, f.projection.ID)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	_, err = persistence.GetSessionCleanupIntent(ctx, f.control.Namespace, f.control.SessionName)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	_, err = s.GetSessionControl(ctx, f.control.Namespace, f.control.SessionName)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	_, err = s.GetBranchClaim(ctx, f.claim.ID)
	require.ErrorIs(t, err, controlstore.ErrNotFound)
	err = kubeClient.Get(ctx, client.ObjectKey{Namespace: f.control.Namespace, Name: runtimeSessionLeaseName(f.control.SessionUID)}, &coordinationv1.Lease{})
	require.True(t, apierrors.IsNotFound(err), "Session Lease survived cleanup: %v", err)
	completion, err := persistence.GetSessionCleanupCompletion(ctx, f.control.Namespace, f.control.SessionName)
	require.NoError(t, err)
	require.Equal(t, f.control.SessionUID, completion.SessionUID)
	request := gatewayPublicSessionCleanupRequest(f, f.request.Fence)
	require.Equal(t, request.OperationID, completion.OperationID)
	require.Equal(t, request.OperationDigest, completion.OperationDigest)
	receipt, err := s.GetSessionTurnCleanupReceipt(ctx, f.control.Namespace, f.control.SessionName, f.turn.PromptAttemptID)
	require.NoError(t, err)
	require.Equal(t, f.turn.Key, receipt.Key)
	require.Equal(t, completion.OperationID, receipt.OperationID)
	require.Equal(t, completion.OperationDigest, receipt.OperationDigest)
	require.Equal(t, []byte(f.projection.Payload), receipt.Payload)
	require.Equal(t, f.projection.DeliveryDigest, receipt.DeliveryDigest)
	require.Empty(t, receipt.SessionTurn().UserPrompt)
	require.Empty(t, receipt.SessionTurn().TerminalContent)
	var envelopeDigest string
	var expiresAt time.Time
	require.NoError(t, db.QueryRowContext(ctx, `SELECT envelope_digest, expires_at FROM gateway_event_tombstones
		WHERE namespace = ? AND event_id = ?`, f.control.Namespace, f.control.SessionName+"-event").Scan(&envelopeDigest, &expiresAt))
	require.Equal(t, testDigest(f.control.SessionName+"-envelope"), envelopeDigest)
	require.True(t, expiresAt.Equal(testNow.Add(48*time.Hour)), "cleanup changed duplicate protection lifetime")
}
