/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agenthost"
	"github.com/orka-agents/orka/internal/connectors/credential"
)

const testHostConnection = "github-alice"

type fakeHostCredentials struct {
	token       string
	unavailable bool
	// broken makes every call fail as a store outage would.
	broken bool
	// connection is the Connection that feeds hosts; empty means testHostConnection.
	connection string
	// neverExpires hands out a token without an expiry.
	neverExpires bool
}

func (f *fakeHostCredentials) HostCredential(_ context.Context, namespace, name string, _ time.Duration) (credential.HostCredential, error) {
	if err := f.CheckHostLink(context.Background(), namespace, name); err != nil {
		return credential.HostCredential{}, err
	}
	held := credential.HostCredential{AccessToken: f.token}
	if !f.neverExpires {
		held.ExpiresAt = time.Now().Add(8 * time.Hour)
	}
	return held, nil
}

func (f *fakeHostCredentials) CheckHostLink(_ context.Context, namespace, name string) error {
	want := f.connection
	if want == "" {
		want = testHostConnection
	}
	if f.broken {
		return errors.New("credential store unavailable")
	}
	if f.unavailable || namespace != testUserNamespace || name != want {
		return fmt.Errorf("%w: the Connection is not linked", credential.ErrHostLinkUnavailable)
	}
	return nil
}

// fakeGitHubAccounts maps a token to its account.
type fakeGitHubAccounts map[string]corev1alpha1.AgentHostGitHubAccount

func (f fakeGitHubAccounts) Account(_ context.Context, token string) (corev1alpha1.AgentHostGitHubAccount, error) {
	account, ok := f[token]
	if !ok {
		return corev1alpha1.AgentHostGitHubAccount{}, errors.New("bad credentials")
	}
	return account, nil
}

func connectionHost() *corev1alpha1.AgentHost {
	host := testHost()
	host.Spec.GitHubTokenSecretRef = nil
	host.Spec.GitHubTokenFrom = &corev1alpha1.AgentHostTokenSource{
		ConnectionRef: corev1alpha1.LocalObjectReference{Name: testHostConnection},
	}
	return host
}

var alice = corev1alpha1.AgentHostGitHubAccount{ID: 1001, Login: "alice"}

func newConnectionHostHarness(t *testing.T) (*agentHostHarness, *fakeHostCredentials, fakeGitHubAccounts) {
	t.Helper()
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		connectionHost())
	credentials := &fakeHostCredentials{token: "ghu_first"}
	accounts := fakeGitHubAccounts{"ghu_first": alice}
	h.reconciler.HostCredentials = credentials
	h.reconciler.GitHubAccounts = accounts
	return h, credentials, accounts
}

func (h *agentHostHarness) tokenCopy() (string, bool) {
	h.t.Helper()
	secret := &corev1.Secret{}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().GitHubTokenSecret}, secret)
	if apierrors.IsNotFound(err) {
		return "", false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return string(secret.Data[agenthost.TokenKey]), true
}

func (h *agentHostHarness) volumeUID() types.UID {
	h.t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().PVC}, pvc); err != nil {
		h.t.Fatalf("the state volume must survive: %v", err)
	}
	return pvc.UID
}

func TestAgentHostTakesItsTokenFromItsConnection(t *testing.T) {
	h, credentials, accounts := newConnectionHostHarness(t)
	h.settle()
	pod, ok := h.pod()
	if !ok {
		t.Fatalf("no Pod; status = %+v", h.host().Status)
	}
	if got := pod.Annotations[agenthost.AnnotationGitHubToken]; got != "connection:"+testHostConnection {
		t.Fatalf("token source = %q", got)
	}
	if token, _ := h.tokenCopy(); token != "ghu_first" {
		t.Fatalf("token copy = %q", token)
	}
	if got := h.host().Status.GitHub; got == nil || *got != alice {
		t.Fatalf("pinned account = %+v", got)
	}

	// A refreshed token replaces the copy without restarting the host, and
	// the host comes back before its token is due.
	credentials.token = "ghu_second"
	accounts["ghu_second"] = alice
	result, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testUserNamespace, Name: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter == 0 || result.RequeueAfter > agentHostResync {
		t.Fatalf("requeue after %s", result.RequeueAfter)
	}
	if token, _ := h.tokenCopy(); token != "ghu_second" {
		t.Fatalf("token copy = %q", token)
	}
	if after, _ := h.pod(); after.UID != pod.UID {
		t.Fatal("a refreshed token must not restart the host")
	}
}

// Code in the host can read its token, so a Connection token that never
// expires, as a GitHub App without user-token expiration issues, is refused.
func TestAgentHostRefusesATokenThatNeverExpires(t *testing.T) {
	h, credentials, _ := newConnectionHostHarness(t)
	credentials.neverExpires = true
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a host must not run on a token that never expires")
	}
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("a token that never expires must not be copied into the host")
	}
	if ready := agentHostReady(h.host()); ready == nil || ready.Reason != agentHostReasonGitHubTokenNeverExpires {
		t.Fatalf("ready = %+v", ready)
	}
	// Once the link holds an expiring token, the host runs.
	credentials.neverExpires = false
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("an expiring token must start the host")
	}
}

// A person who declines a host after it was provisioned stops it: its Pod
// and token copy go and its volume stays, and it runs again once they take
// the decline back.
func TestAgentHostStopsWhenItsPersonDeclinesIt(t *testing.T) {
	h, credentials, _ := newConnectionHostHarness(t)
	connection := &corev1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: testHostConnection, Namespace: testUserNamespace}}
	if err := h.client.Create(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("no Pod before the decline")
	}
	volume := h.volumeUID()
	connection.Annotations = map[string]string{corev1alpha1.ConnectionAnnotationAgentHost: corev1alpha1.ConnectionAgentHostDeclined}
	if err := h.client.Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a declined host must stop")
	}
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("a declined host must lose its token copy")
	}
	if ready := agentHostReady(h.host()); ready == nil || ready.Reason != agentHostReasonConnectionUnavailable {
		t.Fatalf("ready = %+v", ready)
	}
	if h.volumeUID() != volume {
		t.Fatal("a declined host must keep its volume")
	}
	connection.Annotations = nil
	if err := h.client.Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("taking the decline back must restart the host")
	}

	// A decline stops the host even while the link check can only be
	// retried, as when its provider's status lags.
	credentials.broken = true
	connection.Annotations = map[string]string{corev1alpha1.ConnectionAnnotationAgentHost: corev1alpha1.ConnectionAgentHostDeclined}
	if err := h.client.Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testUserNamespace, Name: "alice"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.pod(); ok {
		t.Fatal("a decline must stop the host while its link check is retried")
	}
}

func TestAgentHostStopsWhenItsConnectionIsUnavailable(t *testing.T) {
	h, credentials, _ := newConnectionHostHarness(t)
	h.settle()
	volume := h.volumeUID()

	credentials.unavailable = true
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a host whose Connection is unavailable must stop")
	}
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("the token copy must be deleted")
	}
	if got := h.host(); got.Status.Phase != corev1alpha1.AgentHostPhaseFailed || agentHostReady(got).Reason != agentHostReasonConnectionUnavailable {
		t.Fatalf("status = %+v", got.Status)
	}

	// Reconnecting restarts the host on the same volume.
	credentials.unavailable = false
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatalf("the host did not restart; status = %+v", h.host().Status)
	}
	if h.volumeUID() != volume {
		t.Fatal("the host must restart on its volume")
	}
}

// A suspended host keeps no token copy, and waking it checks the account.
func TestAgentHostSuspendedChecksItsConnection(t *testing.T) {
	h, credentials, accounts := newConnectionHostHarness(t)
	h.settle()
	host := h.host()
	host.Spec.Suspended = true
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("a suspended host keeps no token copy")
	}
	credentials.unavailable = true
	h.settle()
	if got := agentHostReady(h.host()); got.Reason != agentHostReasonConnectionUnavailable {
		t.Fatalf("ready = %+v", got)
	}

	// Waking the host after the link was given to another GitHub account
	// writes no token and fails the host.
	credentials.unavailable = false
	credentials.token = "ghu_mallory"
	accounts["ghu_mallory"] = corev1alpha1.AgentHostGitHubAccount{ID: 2002, Login: "mallory"}
	host = h.host()
	host.Spec.Suspended = false
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("another account's token must not reach the host")
	}
	if got := agentHostReady(h.host()); got.Reason != agentHostReasonGitHubIdentityChanged {
		t.Fatalf("ready = %+v", got)
	}
}

func TestAgentHostRefusesAnotherGitHubAccount(t *testing.T) {
	h, credentials, accounts := newConnectionHostHarness(t)
	h.settle()
	volume := h.volumeUID()

	credentials.token = "ghu_mallory"
	accounts["ghu_mallory"] = corev1alpha1.AgentHostGitHubAccount{ID: 2002, Login: "mallory"}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a host must not run for another GitHub account")
	}
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("another account's token must not reach the host")
	}
	got := h.host()
	if agentHostReady(got).Reason != agentHostReasonGitHubIdentityChanged || got.Status.GitHub == nil || *got.Status.GitHub != alice {
		t.Fatalf("status = %+v", got.Status)
	}
	if h.volumeUID() != volume {
		t.Fatal("the volume must be kept")
	}
}

func TestAgentHostFromAConnectionNeedsConnectors(t *testing.T) {
	h, _, _ := newConnectionHostHarness(t)
	h.reconciler.HostCredentials = nil
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("no Pod without a credential source")
	}
	if got := agentHostReady(h.host()); got.Reason != agentHostReasonConnectionUnavailable {
		t.Fatalf("ready = %+v", got)
	}
}

func TestAgentHostFromAConnectionNeedsARelayTemplate(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialRequired),
		connectionHost())
	h.reconciler.HostCredentials = &fakeHostCredentials{token: "ghu_first"}
	h.reconciler.GitHubAccounts = fakeGitHubAccounts{"ghu_first": alice}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a Connection's token must not reach a Standalone host")
	}
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("no token copy for a Standalone host")
	}
	if got := agentHostReady(h.host()); got.Reason != agentHostReasonInvalidTemplate {
		t.Fatalf("ready = %+v", got)
	}
}

// A running host whose template is recreated without Relay access stops and
// loses the token copy it already had.
func TestAgentHostFromAConnectionLosesItsTokenWhenItsTemplateTurnsStandalone(t *testing.T) {
	h, _, _ := newConnectionHostHarness(t)
	h.settle()
	if _, ok := h.tokenCopy(); !ok {
		t.Fatal("the host has no token copy to lose")
	}
	template := &corev1alpha1.AgentHostTemplate{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: "copilotd"}, template); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), template); err != nil {
		t.Fatal(err)
	}
	recreated := testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialRequired)
	recreated.UID = "template-uid-2"
	if err := h.client.Create(context.Background(), recreated); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("the host must stop")
	}
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("the token copy must be deleted")
	}
	if got := agentHostReady(h.host()); got.Reason != agentHostReasonInvalidTemplate {
		t.Fatalf("ready = %+v", got)
	}
}

// A suspension goes ahead while the link cannot be checked; a running host
// in the same state keeps retrying instead.
func TestAgentHostSuspendsWhileItsLinkCannotBeChecked(t *testing.T) {
	h, credentials, _ := newConnectionHostHarness(t)
	h.settle()
	credentials.broken = true
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testUserNamespace, Name: "alice"}}
	if _, err := h.reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("a running host whose link cannot be checked must retry")
	}
	if _, ok := h.pod(); !ok {
		t.Fatal("a check that could not run must not stop a running host")
	}
	if _, ok := h.tokenCopy(); !ok {
		t.Fatal("a check that could not run must not delete the token copy")
	}
	host := h.host()
	host.Spec.Suspended = true
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := h.reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("suspension must not wait on the link: %v", err)
		}
	}
	if _, ok := h.pod(); ok {
		t.Fatal("the host must stop")
	}
	if _, ok := h.tokenCopy(); ok {
		t.Fatal("the token copy must be deleted")
	}
}

// A provider edit reaches every host its links feed, and no other.
func TestAgentHostProviderChangeReachesTheHostsItsLinksFeed(t *testing.T) {
	fed := connectionHost()
	secretFed := testHost()
	secretFed.Name = "bob"
	elsewhere := connectionHost()
	elsewhere.Namespace = "other"
	h := newAgentHostHarness(t, fed, secretFed, elsewhere)
	provider := &corev1alpha1.ConnectorProvider{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: testUserNamespace}}

	requests := h.reconciler.connectionFedHosts(context.Background(), provider)
	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: testUserNamespace, Name: "alice"}}}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
}
