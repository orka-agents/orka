/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package credential

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/store"
)

// serveHosts makes the fixture provider serve agent hosts from template, with
// the Connection's consent covering it, as a fresh link would.
func (h *harness) serveHosts(template string) {
	h.t.Helper()
	ctx := context.Background()
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		h.t.Fatal(err)
	}
	provider.Spec.AgentHost = &corev1alpha1.ConnectorAgentHost{TemplateRef: corev1alpha1.LocalObjectReference{Name: template}}
	if err := h.client.Update(ctx, provider); err != nil {
		h.t.Fatal(err)
	}
	connection := h.reload()
	connection.Status.Consent = connectors.ConsentFor(provider)
	if err := h.client.Status().Update(ctx, connection); err != nil {
		h.t.Fatal(err)
	}
}

func TestHostCredentialReturnsTheAccessTokenAndRefreshesBeforeExpiry(t *testing.T) {
	h := newHarness(t)
	h.serveHosts("copilotd")
	h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(4 * time.Hour)})

	held, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if held.AccessToken != "ghu_held" || h.refresher.calls.Load() != 0 {
		t.Fatalf("held = %+v, refreshes = %d", held, h.refresher.calls.Load())
	}
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); err != nil {
		t.Fatal(err)
	}

	// A token that expires within the horizon is refreshed, and the new one
	// returned.
	h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(10 * time.Minute)})
	held, err = h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, time.Minute)
	if err != nil || held.AccessToken != "ghu_held" {
		t.Fatalf("held = %+v, %v", held, err)
	}
	refreshed, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "gho_new" || h.refresher.calls.Load() != 1 || refreshed.Version == held.Version {
		t.Fatalf("refreshed = %+v, refreshes = %d", refreshed, h.refresher.calls.Load())
	}

	// A host keeps its template when the provider moves to another one: once
	// the person consents to the change, their host is fed again.
	h.serveHosts("other")
	if _, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute); err != nil {
		t.Fatalf("a host must outlive its provider's template change: %v", err)
	}
}

func TestHostCredentialIsUnavailableWhenTheLinkCannotFeedAHost(t *testing.T) {
	for name, setup := range map[string]func(h *harness){
		"provider serves no hosts": func(h *harness) {},
		"consent predates hosts": func(h *harness) {
			h.serveHosts("copilotd")
			connection := h.reload()
			connection.Status.Consent.AuthorityDigest = "stale"
			if err := h.client.Status().Update(context.Background(), connection); err != nil {
				h.t.Fatal(err)
			}
		},
		"connection gone": func(h *harness) {
			h.serveHosts("copilotd")
			if err := h.client.Delete(context.Background(), h.reload()); err != nil {
				h.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(4 * time.Hour)})
			setup(h)
			_, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute)
			if !errors.Is(err, ErrHostLinkUnavailable) {
				t.Fatalf("err = %v, want ErrHostLinkUnavailable", err)
			}
			if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); !errors.Is(err, ErrHostLinkUnavailable) {
				t.Fatalf("check = %v, want ErrHostLinkUnavailable", err)
			}
		})
	}
}

// A provider that stops serving hosts while a refresh is in flight releases no
// token, refreshed or not.
func TestHostCredentialFailsClosedWhenTheProviderOptsOutDuringARefresh(t *testing.T) {
	h := newHarness(t)
	h.serveHosts("copilotd")
	h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(5 * time.Minute)})
	h.refresher.onRefresh = func() {
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
			h.t.Error(err)
			return
		}
		provider.Spec.AgentHost = nil
		if err := h.client.Update(context.Background(), provider); err != nil {
			h.t.Error(err)
		}
	}
	held, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute)
	if !errors.Is(err, ErrHostLinkUnavailable) || held.AccessToken != "" {
		t.Fatalf("held = %q, err = %v; want no token and ErrHostLinkUnavailable", held.AccessToken, err)
	}
	if h.refresher.calls.Load() != 1 {
		t.Fatalf("refreshes = %d, want the race to have run", h.refresher.calls.Load())
	}
}

// An edit to the provider that its controller has not judged yet stops no
// host and releases no token; once judged, a provider that no longer accepts
// the link stops it.
func TestHostLinkWaitsForTheProviderStatusToCatchUp(t *testing.T) {
	h := newHarness(t)
	h.serveHosts("copilotd")
	h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(4 * time.Hour)})
	setConditions := func(observed int64, status metav1.ConditionStatus) {
		t.Helper()
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
			t.Fatal(err)
		}
		for i := range provider.Status.Conditions {
			provider.Status.Conditions[i].ObservedGeneration = observed
			provider.Status.Conditions[i].Status = status
		}
		if err := h.client.Update(context.Background(), provider); err != nil {
			t.Fatal(err)
		}
	}

	setConditions(0, metav1.ConditionTrue)
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); err == nil || errors.Is(err, ErrHostLinkUnavailable) {
		t.Fatalf("check = %v, want a retryable error", err)
	}
	if held, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute); err == nil ||
		errors.Is(err, ErrHostLinkUnavailable) || held.AccessToken != "" {
		t.Fatalf("held = %q, err = %v; want no token and a retryable error", held.AccessToken, err)
	}

	// A reference check that could not complete is no verdict either.
	setConditions(1, metav1.ConditionTrue)
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&provider.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.ConnectorProviderConditionResolvedRefs, Status: metav1.ConditionUnknown,
		Reason: connectors.ReasonResolutionFailed, ObservedGeneration: 1,
	})
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); err == nil || errors.Is(err, ErrHostLinkUnavailable) {
		t.Fatalf("unresolved references: check = %v, want a retryable error", err)
	}

	setConditions(1, metav1.ConditionFalse)
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); !errors.Is(err, ErrHostLinkUnavailable) {
		t.Fatalf("check = %v, want ErrHostLinkUnavailable", err)
	}
}

// A Connection status that has not caught up with an edit, or that records a
// provider read that failed, stops no host and releases no token. What the
// status would judge is checked directly instead: a mode the grant does not
// cover, or a provider that is gone, still stops the host.
func TestHostLinkWaitsForTheConnectionStatusToCatchUp(t *testing.T) {
	retryable := func(h *harness, stage string) {
		t.Helper()
		if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); err == nil || errors.Is(err, ErrHostLinkUnavailable) {
			t.Fatalf("%s: check = %v, want a retryable error", stage, err)
		}
		if held, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute); err == nil ||
			errors.Is(err, ErrHostLinkUnavailable) || held.AccessToken != "" {
			t.Fatalf("%s: held = %q, err = %v; want no token and a retryable error", stage, held.AccessToken, err)
		}
	}
	unavailable := func(h *harness, stage string) {
		t.Helper()
		if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); !errors.Is(err, ErrHostLinkUnavailable) {
			t.Fatalf("%s: check = %v, want ErrHostLinkUnavailable", stage, err)
		}
	}

	t.Run("mode edit", func(t *testing.T) {
		h := newHarness(t)
		h.serveHosts("copilotd")
		h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(4 * time.Hour)})
		// The mode changes and the Connection controller has not judged it:
		// the status still describes the previous generation.
		connection := h.reload()
		connection.Spec.Mode = corev1alpha1.ConnectionModeReadWrite
		connection.Generation++
		if err := h.client.Update(context.Background(), connection); err != nil {
			t.Fatal(err)
		}
		if h.reload().Generation == h.connection.Generation {
			t.Fatal("the edit must move the Connection to a new generation")
		}
		retryable(h, "status behind a covered mode")

		// A mode the grant does not cover is a verdict of its own.
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
			t.Fatal(err)
		}
		provider.Spec.OAuth.Scopes.Write = []string{"repo"}
		if err := h.client.Update(context.Background(), provider); err != nil {
			t.Fatal(err)
		}
		unavailable(h, "status behind an uncovered mode")
	})

	t.Run("provider read failed", func(t *testing.T) {
		h := newHarness(t)
		h.serveHosts("copilotd")
		h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(4 * time.Hour)})
		// What the Connection controller records when it cannot read the
		// provider; Ready stays True.
		connection := h.reload()
		meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
			Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionUnknown,
			Reason: corev1alpha1.ConnectionReasonProviderReadFailed, ObservedGeneration: connection.Generation,
		})
		meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
			Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionUnknown,
			Reason: "ProviderUnresolved", ObservedGeneration: connection.Generation,
		})
		if err := h.client.Status().Update(context.Background(), connection); err != nil {
			t.Fatal(err)
		}
		retryable(h, "recorded read failure")

		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
			t.Fatal(err)
		}
		if err := h.client.Delete(context.Background(), provider); err != nil {
			t.Fatal(err)
		}
		unavailable(h, "provider gone")
	})
}

// The Connection controller can judge a provider edit before the provider
// controller does, marking the link's provider invalid. That stops no host
// either, until the provider's current status refuses the link.
func TestHostLinkWaitsForTheConnectionToSeeItsProvider(t *testing.T) {
	h := newHarness(t)
	h.serveHosts("copilotd")
	h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(4 * time.Hour)})
	connection := h.reload()
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionFalse,
		Reason: corev1alpha1.ConnectionReasonProviderInvalid, ObservedGeneration: connection.Generation,
	})
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionUnknown,
		Reason: "ProviderUnresolved", ObservedGeneration: connection.Generation,
	})
	if err := h.client.Status().Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	setProvider := func(observed int64, status metav1.ConditionStatus) {
		t.Helper()
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
			t.Fatal(err)
		}
		for i := range provider.Status.Conditions {
			provider.Status.Conditions[i].ObservedGeneration = observed
			provider.Status.Conditions[i].Status = status
		}
		if err := h.client.Update(context.Background(), provider); err != nil {
			t.Fatal(err)
		}
	}
	retryable := func(stage string) {
		t.Helper()
		if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); err == nil || errors.Is(err, ErrHostLinkUnavailable) {
			t.Fatalf("%s: check = %v, want a retryable error", stage, err)
		}
		if held, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute); err == nil ||
			errors.Is(err, ErrHostLinkUnavailable) || held.AccessToken != "" {
			t.Fatalf("%s: held = %q, err = %v; want no token and a retryable error", stage, held.AccessToken, err)
		}
	}

	// The provider controller has not judged the edit.
	setProvider(0, metav1.ConditionTrue)
	retryable("provider status behind")
	// It has accepted it, and the Connection controller has not caught up.
	setProvider(1, metav1.ConditionTrue)
	retryable("connection status behind")
	// It has refused it: the link no longer feeds the host.
	setProvider(1, metav1.ConditionFalse)
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); !errors.Is(err, ErrHostLinkUnavailable) {
		t.Fatalf("refused provider: check = %v, want ErrHostLinkUnavailable", err)
	}
}

// A Connection being deleted is a disconnect, even while its finalizer holds
// it and its provider's status lags, which would otherwise be retried.
func TestHostLinkEndsWhenItsConnectionIsBeingDeleted(t *testing.T) {
	h := newHarness(t)
	h.serveHosts("copilotd")
	h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(4 * time.Hour)})
	connection := h.reload()
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionFalse,
		Reason: corev1alpha1.ConnectionReasonProviderInvalid, ObservedGeneration: connection.Generation,
	})
	if err := h.client.Status().Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	for i := range provider.Status.Conditions {
		provider.Status.Conditions[i].ObservedGeneration = 0
	}
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); err == nil || errors.Is(err, ErrHostLinkUnavailable) {
		t.Fatalf("before deletion: check = %v, want the lag retried", err)
	}

	connection = h.reload()
	connection.Finalizers = append(connection.Finalizers, "test.orka.ai/held")
	if err := h.client.Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), h.reload()); err != nil {
		t.Fatal(err)
	}
	if h.reload().DeletionTimestamp.IsZero() {
		t.Fatal("the finalizer must hold the Connection")
	}
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); !errors.Is(err, ErrHostLinkUnavailable) {
		t.Fatalf("while deleting: check = %v, want ErrHostLinkUnavailable", err)
	}
}

type failingHostCustody struct {
	store.ConnectorCredentialStore
	reads int
	err   error
}

func (c *failingHostCustody) GetConnectorCredential(context.Context, store.ConnectorCredentialRef) (store.ConnectorCredential, error) {
	c.reads++
	return store.ConnectorCredential{}, c.err
}

// Confirmed provider refusals end the link even when custody is unavailable.
// Observation lag must still reach custody: missing custody ends the link,
// while a storage outage alone remains retryable.
func TestHostLinkProviderRefusalPrecedesCustodyFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		condition string
		stale     bool
	}{
		{name: "deleting"},
		{name: "refused", condition: corev1alpha1.ConnectorProviderConditionAccepted},
		{name: "unresolved refs", condition: corev1alpha1.ConnectorProviderConditionResolvedRefs},
		{name: "stale refusal", condition: corev1alpha1.ConnectorProviderConditionAccepted, stale: true},
		{name: "stale refs", condition: corev1alpha1.ConnectorProviderConditionResolvedRefs, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.serveHosts("copilotd")
			custody := &failingHostCustody{ConnectorCredentialStore: h.store, err: context.DeadlineExceeded}
			h.source.Credentials = custody
			check := func(want error) {
				t.Helper()
				if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); !errors.Is(err, want) {
					t.Fatalf("check = %v, want %v", err, want)
				}
				held, err := h.source.HostCredential(context.Background(), testNamespace, h.connection.Name, 15*time.Minute)
				if !errors.Is(err, want) || held != (HostCredential{}) {
					t.Fatalf("credential error = %v, want %v and no credential", err, want)
				}
			}
			check(context.DeadlineExceeded)
			if custody.reads != 2 {
				t.Fatalf("custody reads = %d, want 2 before refusal", custody.reads)
			}
			provider := &corev1alpha1.ConnectorProvider{}
			key := types.NamespacedName{Namespace: testNamespace, Name: "github"}
			if err := h.client.Get(context.Background(), key, provider); err != nil {
				t.Fatal(err)
			}
			if tc.condition == "" {
				provider.Finalizers = append(provider.Finalizers, "test.orka.ai/held")
			} else {
				generation := provider.Generation
				if tc.stale {
					generation--
				}
				meta.SetStatusCondition(&provider.Status.Conditions, metav1.Condition{
					Type: tc.condition, Status: metav1.ConditionFalse, Reason: "Refused", ObservedGeneration: generation,
				})
			}
			if err := h.client.Update(context.Background(), provider); err != nil {
				t.Fatal(err)
			}
			if tc.condition == "" {
				if err := h.client.Delete(context.Background(), provider); err != nil {
					t.Fatal(err)
				}
				if err := h.client.Get(context.Background(), key, provider); err != nil {
					t.Fatal(err)
				}
				if provider.DeletionTimestamp.IsZero() {
					t.Fatal("the finalizer must hold the deleting provider")
				}
			}
			if tc.stale {
				check(context.DeadlineExceeded)
				custody.err = store.ErrNotFound
				check(ErrHostLinkUnavailable)
				if custody.reads != 6 {
					t.Fatalf("custody reads = %d, want 6 while provider status lags", custody.reads)
				}
			} else {
				check(ErrHostLinkUnavailable)
				if custody.reads != 2 {
					t.Fatalf("custody reads = %d, want no reads after confirmed refusal", custody.reads)
				}
			}
		})
	}
}

// A provider read that fails is retried, never taken for a link that stopped
// authorizing the host: a host is stopped only for a provider it has read.
func TestHostLinkReadFailuresAreRetried(t *testing.T) {
	h := newHarness(t)
	h.serveHosts("copilotd")
	h.put(store.ConnectorCredential{AccessToken: "ghu_held", RefreshToken: "ghr_held", ExpiresAt: h.now.Add(4 * time.Hour)})
	failProvider := func(n int) client.Reader {
		reads := 0
		return interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1alpha1.ConnectorProvider); ok {
					if reads++; reads == n {
						return context.DeadlineExceeded
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	}
	h.source.APIReader = failProvider(1)
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); err == nil || errors.Is(err, ErrHostLinkUnavailable) {
		t.Fatalf("err = %v, want a retryable error", err)
	}
	// A link check reads its provider once, so no second read can fail it.
	h.source.APIReader = failProvider(2)
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); err != nil {
		t.Fatalf("err = %v", err)
	}
}
