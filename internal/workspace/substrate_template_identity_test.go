package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type templateIdentitySubstrateControl struct {
	recordingSubstrateControlClient
	actor       *substrateActor
	afterCreate *substrateActor
	afterResume *substrateActor
}

func (c *templateIdentitySubstrateControl) GetActor(context.Context, string) (*substrateActor, error) {
	c.getCalls++
	if c.actor == nil {
		return nil, NewError("get actor", ErrorKindNotFound, "actor absent", false, nil)
	}
	return c.actor, nil
}

func (c *templateIdentitySubstrateControl) CreateActor(context.Context, string, string, string) (*substrateActor, error) {
	c.createCalls++
	c.actor = c.afterCreate
	return c.actor, c.createErr
}

func (c *templateIdentitySubstrateControl) ResumeActor(context.Context, string, bool) (*substrateActor, error) {
	c.resumeCalls++
	c.actor = c.afterResume
	return c.actor, nil
}

func templateIdentityActor(uid, state string) *substrateActor {
	actor := &substrateActor{
		ActorID: "actor", ActorUID: "actor-uid", TemplateNamespace: "ate-demo", TemplateName: "mcp",
		TemplateUID: uid, Status: state,
	}
	if state == substrateStatusRunning {
		actor.PodIP, actor.PodName = "10.0.0.1", "worker"
	}
	return actor
}

func TestSubstrateClaimPinsImmutableTemplateIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		uid        string
		state      string
		create     bool
		concurrent bool
		wantErr    bool
	}{
		{name: "current running actor", uid: "current", state: substrateStatusRunning},
		{name: "recreated template", uid: "old", state: substrateStatusRunning, wantErr: true},
		{name: "unbound running actor", state: substrateStatusRunning, wantErr: true},
		{name: "old suspended state", uid: "old", state: substrateStatusSuspended, wantErr: true},
		{name: "unbooted actor", state: substrateStatusSuspended},
		{name: "new unbooted actor", state: substrateStatusSuspended, create: true},
		{name: "wrong created identity", uid: "old", state: substrateStatusRunning, create: true, wantErr: true},
		{name: "concurrent old actor", uid: "old", state: substrateStatusRunning, create: true, concurrent: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor := templateIdentityActor(tc.uid, tc.state)
			control := &templateIdentitySubstrateControl{actor: actor}
			if tc.create {
				control.actor, control.afterCreate = nil, actor
			}
			if tc.concurrent {
				control.createErr = NewError("create", ErrorKindAlreadyExists, "actor exists", false, nil)
			}
			executor := &SubstrateWorkspaceExecutor{control: control, now: time.Now}
			claim, err := executor.Claim(t.Context(), ClaimRequest{
				ClaimName: "actor", CreateIfMissing: true,
				Template: TemplateRef{Namespace: "ate-demo", Name: "mcp", UID: "current"},
			})
			if tc.wantErr {
				require.True(t, IsKind(err, ErrorKindFailedPrecondition), "claim error = %v", err)
				require.Nil(t, claim)
			} else {
				require.NoError(t, err)
				require.Equal(t, "current", claim.Template.UID)
			}
		})
	}
}

func TestSubstrateReadinessRechecksTemplateIdentityBeforeAndAfterBoot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		beforeUID   string
		beforeState string
		afterUID    string
		wantErr     bool
		wantResumes int
	}{
		{name: "first boot", beforeState: substrateStatusSuspended, afterUID: "current", wantResumes: 1},
		{name: "replaced before resume", beforeUID: "old", beforeState: substrateStatusRunning, wantErr: true},
		{name: "template replaced during first boot", beforeState: substrateStatusSuspended, afterUID: "old", wantErr: true, wantResumes: 1},
		{name: "missing boot identity", beforeState: substrateStatusSuspended, wantErr: true, wantResumes: 1},
		{name: "changed during resume", beforeUID: "current", beforeState: substrateStatusRunning, afterUID: "old", wantErr: true, wantResumes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := &templateIdentitySubstrateControl{
				actor: templateIdentityActor(tc.beforeUID, tc.beforeState), afterResume: templateIdentityActor(tc.afterUID, substrateStatusRunning),
			}
			executor := &SubstrateWorkspaceExecutor{control: control, now: time.Now}
			ready, err := executor.WaitReady(t.Context(), WaitReadyRequest{
				Ref: WorkspaceRef{Namespace: "ate-demo", ID: "actor.ate-demo"}, Timeout: time.Second,
				Template: TemplateRef{Namespace: "ate-demo", Name: "mcp", UID: "current"}, SkipDaemonHealthCheck: true,
			})
			if tc.wantErr {
				require.True(t, IsKind(err, ErrorKindFailedPrecondition), "readiness error = %v", err)
				require.Nil(t, ready)
			} else {
				require.NoError(t, err)
				require.Equal(t, PhaseReady, ready.Phase)
			}
			require.Equal(t, tc.wantResumes, control.resumeCalls)
		})
	}
}
