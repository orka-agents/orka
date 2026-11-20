/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/provideridentity"
)

type fakeProviderIdentities struct {
	checkErr  error
	verifyErr error
	verifies  int
}

func (f *fakeProviderIdentities) Check(identity, baseURL string) error {
	return f.checkErr
}

func (f *fakeProviderIdentities) Verify(context.Context, string, string) error {
	f.verifies++
	return f.verifyErr
}

func keylessTestProvider() *corev1alpha1.Provider {
	return &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: "default", UID: "uid-1", Generation: 1},
		Spec: corev1alpha1.ProviderSpec{
			Type:    corev1alpha1.ProviderTypeAzureOpenAI,
			BaseURL: "https://example.openai.azure.com",
			Auth:    &corev1alpha1.ProviderAuth{Entra: &corev1alpha1.ProviderEntraAuth{Identity: "models"}},
			Azure:   &corev1alpha1.AzureConfig{DeploymentName: "gpt-example"},
		},
	}
}

func reconcileKeyless(t *testing.T, r *ProviderReconciler, c client.Client) (*corev1alpha1.Provider, reconcile.Result) {
	t.Helper()
	key := types.NamespacedName{Name: "azure", Namespace: "default"}
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := &corev1alpha1.Provider{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	return got, result
}

func newKeylessReconciler(identities ProviderIdentityVerifier, now *time.Time, provider *corev1alpha1.Provider) (*ProviderReconciler, client.Client) {
	scheme := newProviderScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Provider{}).WithObjects(provider).Build()
	return &ProviderReconciler{Client: c, Scheme: scheme, Identities: identities, Now: func() time.Time { return *now }}, c
}

func TestProviderReconcileKeylessReadsNoSecretAndReportsCredentialReady(t *testing.T) {
	now := time.Now()
	identities := &fakeProviderIdentities{}
	r, c := newKeylessReconciler(identities, &now, keylessTestProvider())
	got, result := reconcileKeyless(t, r, c)
	if !got.Status.Ready {
		t.Fatalf("Ready = false: %s", got.Status.Message)
	}
	credential := meta.FindStatusCondition(got.Status.Conditions, ConditionProviderCredentialReady)
	if credential == nil || credential.Status != metav1.ConditionTrue || credential.Reason != reasonTokenIssued {
		t.Fatalf("CredentialReady = %+v", credential)
	}
	if result.RequeueAfter != providerCredentialRecheck {
		t.Fatalf("RequeueAfter = %v, want the hourly recheck", result.RequeueAfter)
	}
	// A second reconcile within the hour, as a status update causes, does
	// not ask the issuer again.
	now = now.Add(10 * time.Minute)
	got, result = reconcileKeyless(t, r, c)
	if identities.verifies != 1 || !got.Status.Ready {
		t.Fatalf("verifies = %d, Ready = %v; want one verification per hour", identities.verifies, got.Status.Ready)
	}
	if result.RequeueAfter != 50*time.Minute {
		t.Fatalf("RequeueAfter = %v, want the rest of the hour", result.RequeueAfter)
	}
	now = now.Add(50 * time.Minute)
	reconcileKeyless(t, r, c)
	if identities.verifies != 2 {
		t.Fatalf("verifies = %d, want a recheck after an hour", identities.verifies)
	}
}

func TestProviderReconcileKeylessVerifiesAgainOnAGenerationChange(t *testing.T) {
	now := time.Now()
	identities := &fakeProviderIdentities{}
	r, c := newKeylessReconciler(identities, &now, keylessTestProvider())
	reconcileKeyless(t, r, c)
	stored := &corev1alpha1.Provider{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "azure", Namespace: "default"}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Generation = 2
	if err := c.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reconcileKeyless(t, r, c)
	if identities.verifies != 2 {
		t.Fatalf("verifies = %d, want a new verification for the new generation", identities.verifies)
	}
}

func TestProviderReconcileKeylessRefusalAndOutage(t *testing.T) {
	now := time.Now()
	identities := &fakeProviderIdentities{verifyErr: errors.New("Entra refused the token request: invalid_client (AADSTS700016, HTTP 400)")}
	r, c := newKeylessReconciler(identities, &now, keylessTestProvider())
	got, _ := reconcileKeyless(t, r, c)
	credential := meta.FindStatusCondition(got.Status.Conditions, ConditionProviderCredentialReady)
	if got.Status.Ready || credential == nil || credential.Status != metav1.ConditionFalse || credential.Reason != reasonTokenRefused ||
		!strings.Contains(credential.Message, "invalid_client") {
		t.Fatalf("Ready = %v, CredentialReady = %+v; want a refusal", got.Status.Ready, credential)
	}

	identities = &fakeProviderIdentities{verifyErr: fmt.Errorf("down: %w", provideridentity.ErrIssuerUnavailable)}
	r, c = newKeylessReconciler(identities, &now, keylessTestProvider())
	got, result := reconcileKeyless(t, r, c)
	credential = meta.FindStatusCondition(got.Status.Conditions, ConditionProviderCredentialReady)
	if !got.Status.Ready || credential == nil || credential.Status != metav1.ConditionUnknown || credential.Reason != reasonIssuerUnavailable {
		t.Fatalf("Ready = %v, CredentialReady = %+v; want Unknown without blocking the Provider", got.Status.Ready, credential)
	}
	if result.RequeueAfter != providerCredentialRetry {
		t.Fatalf("RequeueAfter = %v, want a retry", result.RequeueAfter)
	}
}

func TestProviderReconcileKeylessConfigurationErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		identities ProviderIdentityVerifier
		mutate     func(*corev1alpha1.Provider)
		want       string
	}{
		"no identities":       {identities: nil, want: "no Provider identities"},
		"unknown identity":    {identities: &fakeProviderIdentities{checkErr: provideridentity.ErrNotConfigured}, want: "is not configured"},
		"endpoint not listed": {identities: &fakeProviderIdentities{checkErr: provideridentity.ErrEndpointNotAllowed}, want: "allowedEndpoints"},
		"key and auth": {identities: &fakeProviderIdentities{}, mutate: func(p *corev1alpha1.Provider) {
			p.Spec.SecretRef = &corev1alpha1.ProviderSecretRef{Name: "key"}
		}, want: "exactly one"},
		"wrong type": {identities: &fakeProviderIdentities{}, mutate: func(p *corev1alpha1.Provider) {
			p.Spec.Type = corev1alpha1.ProviderTypeOpenAI
		}, want: "only for azure-openai"},
		"prefixed baseURL": {identities: &fakeProviderIdentities{}, mutate: func(p *corev1alpha1.Provider) {
			p.Spec.BaseURL = "https://example.openai.azure.com/azure"
		}, want: "resource's origin"},
		"baseURL with a query": {identities: &fakeProviderIdentities{}, mutate: func(p *corev1alpha1.Provider) {
			p.Spec.BaseURL = "https://example.openai.azure.com/?api-version=1"
		}, want: "resource's origin"},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			provider := keylessTestProvider()
			if tc.mutate != nil {
				tc.mutate(provider)
			}
			var identities ProviderIdentityVerifier
			if tc.identities != nil {
				identities = tc.identities
			}
			r, c := newKeylessReconciler(identities, &now, provider)
			got, _ := reconcileKeyless(t, r, c)
			if got.Status.Ready || !strings.Contains(got.Status.Message, tc.want) {
				t.Fatalf("Ready = %v, Message = %q; want not ready with %q", got.Status.Ready, got.Status.Message, tc.want)
			}
			if fake, ok := tc.identities.(*fakeProviderIdentities); ok && fake.verifies != 0 {
				t.Fatal("an invalid Provider was verified with the issuer")
			}
		})
	}
}

func TestProviderReconcileWithoutAuthOrSecretIsNotReady(t *testing.T) {
	now := time.Now()
	provider := keylessTestProvider()
	provider.Spec.Auth = nil
	r, c := newKeylessReconciler(nil, &now, provider)
	got, _ := reconcileKeyless(t, r, c)
	if got.Status.Ready || !strings.Contains(got.Status.Message, "spec.secretRef is required") {
		t.Fatalf("Ready = %v, Message = %q", got.Status.Ready, got.Status.Message)
	}
}

// During an outage, Providers sharing an identity wait for the retry instead
// of each waiting out the issuer's timeout.
func TestProviderReconcileKeylessOutageBacksOffPerIdentity(t *testing.T) {
	now := time.Now()
	identities := &fakeProviderIdentities{verifyErr: fmt.Errorf("down: %w", provideridentity.ErrIssuerUnavailable)}
	second := keylessTestProvider()
	second.Name, second.UID = "azure-2", "uid-2"
	r, c := newKeylessReconciler(identities, &now, keylessTestProvider())
	if err := c.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	reconcileKeyless(t, r, c)

	now = now.Add(10 * time.Second)
	key := types.NamespacedName{Name: "azure-2", Namespace: "default"}
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	got := &corev1alpha1.Provider{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	credential := meta.FindStatusCondition(got.Status.Conditions, ConditionProviderCredentialReady)
	if identities.verifies != 1 || credential == nil || credential.Status != metav1.ConditionUnknown || credential.Reason != reasonIssuerUnavailable {
		t.Fatalf("verifies = %d, CredentialReady = %+v; want Unknown without asking the issuer again", identities.verifies, credential)
	}
	if result.RequeueAfter != providerCredentialRetry-10*time.Second {
		t.Fatalf("RequeueAfter = %v, want the rest of the backoff", result.RequeueAfter)
	}

	// After the backoff the issuer is asked again, and an answer ends it.
	now = now.Add(providerCredentialRetry)
	identities.verifyErr = nil
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if identities.verifies != 2 {
		t.Fatalf("verifies = %d, want a retry after the backoff", identities.verifies)
	}
	if _, ok := r.issuerBackoff.Load("models"); ok {
		t.Fatal("the backoff outlived the issuer's answer")
	}
}

// A deleted Provider's verification is forgotten, and one recreated under the
// same name is verified again.
func TestProviderReconcileKeylessForgetsDeletedProviders(t *testing.T) {
	now := time.Now()
	identities := &fakeProviderIdentities{}
	r, c := newKeylessReconciler(identities, &now, keylessTestProvider())
	reconcileKeyless(t, r, c)
	key := types.NamespacedName{Name: "azure", Namespace: "default"}
	if err := c.Delete(context.Background(), keylessTestProvider()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.verifications.Load(key); ok {
		t.Fatal("a deleted Provider's verification was kept")
	}

	// A replacement that reuses the name but not the UID, before the delete
	// was observed, is verified on its own.
	r.verifications.Store(key, providerVerification{uid: "uid-old", generation: 1, identity: "models",
		baseURL: "https://example.openai.azure.com", at: now, condition: metav1.Condition{Status: metav1.ConditionTrue}})
	if err := c.Create(context.Background(), keylessTestProvider()); err != nil {
		t.Fatal(err)
	}
	reconcileKeyless(t, r, c)
	if identities.verifies != 2 {
		t.Fatalf("verifies = %d, want the replacement verified", identities.verifies)
	}
}

// A keyless Provider switched to a Secret that does not exist keeps no
// CredentialReady condition from its keyless days.
func TestProviderReconcileDropsCredentialStateWhenSwitchedToAMissingSecret(t *testing.T) {
	now := time.Now()
	r, c := newKeylessReconciler(&fakeProviderIdentities{}, &now, keylessTestProvider())
	got, _ := reconcileKeyless(t, r, c)
	if meta.FindStatusCondition(got.Status.Conditions, ConditionProviderCredentialReady) == nil {
		t.Fatal("the keyless Provider reported no CredentialReady condition")
	}
	got.Spec.Auth = nil
	got.Spec.SecretRef = &corev1alpha1.ProviderSecretRef{Name: "missing"}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	got, _ = reconcileKeyless(t, r, c)
	if got.Status.Ready {
		t.Fatal("a Provider with a missing Secret is Ready")
	}
	if condition := meta.FindStatusCondition(got.Status.Conditions, ConditionProviderCredentialReady); condition != nil {
		t.Fatalf("CredentialReady = %+v, want it removed", condition)
	}
	if _, ok := r.verifications.Load(client.ObjectKeyFromObject(got)); ok {
		t.Fatal("the keyless verification outlived the switch to a Secret")
	}
}
