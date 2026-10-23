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

	setConditions(1, metav1.ConditionFalse)
	if err := h.source.CheckHostLink(context.Background(), testNamespace, h.connection.Name); !errors.Is(err, ErrHostLinkUnavailable) {
		t.Fatalf("check = %v, want ErrHostLinkUnavailable", err)
	}
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
