// Copyright (c) 2026. MIT License - see LICENSE file for details.

package outboundaccess

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

func reservedSAPolicy(subject, actor string) *corev1alpha1.OutboundAccessPolicy {
	direct := &corev1alpha1.DirectOutboundAccess{
		Grant:         corev1alpha1.OutboundGrantTokenExchange,
		TokenEndpoint: corev1alpha1.OutboundTokenEndpoint{URL: "https://issuer.example.test/token"},
		Subject: corev1alpha1.OutboundTokenSource{
			Source: corev1alpha1.OutboundTokenSourceServiceAccount,
			ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{
				Name: subject, Audiences: []string{"api://AzureADTokenExchange"},
			},
		},
		ExpectedIssuedTokenType: tokenexchange.TokenTypeAccessToken,
	}
	if actor != "" {
		direct.Actor = &corev1alpha1.OutboundTokenSource{
			Source:            corev1alpha1.OutboundTokenSourceServiceAccount,
			ServiceAccountRef: &corev1alpha1.OutboundServiceAccountReference{Name: actor},
		}
	}
	return readyPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{Direct: direct})
}

func TestValidateReservedServiceAccount(t *testing.T) {
	controller := ReservedServiceAccount{Namespace: "tenant", Name: "orka-controller"}
	for name, tc := range map[string]struct {
		policy   *corev1alpha1.OutboundAccessPolicy
		reserved ReservedServiceAccount
		refused  bool
	}{
		"controller as subject":          {policy: reservedSAPolicy("orka-controller", ""), reserved: controller, refused: true},
		"controller as actor":            {policy: reservedSAPolicy("workload", "orka-controller"), reserved: controller, refused: true},
		"another account":                {policy: reservedSAPolicy("workload", ""), reserved: controller},
		"same name in another namespace": {policy: reservedSAPolicy("orka-controller", ""), reserved: ReservedServiceAccount{Namespace: "orka-system", Name: "orka-controller"}},
		"nothing reserved":               {policy: reservedSAPolicy("orka-controller", "")},
	} {
		t.Run(name, func(t *testing.T) {
			issue := ValidateReservedServiceAccount(tc.policy, tc.reserved)
			if tc.refused != (issue != nil) {
				t.Fatalf("ValidateReservedServiceAccount() = %v, want refused=%v", issue, tc.refused)
			}
		})
	}
}

// The resolver refuses the controller's own ServiceAccount before any
// TokenRequest, so its token never reaches a policy's token endpoint.
func TestKubernetesResolverNeverMintsTheReservedServiceAccountToken(t *testing.T) {
	scheme := resolverScheme(t)
	for name, policy := range map[string]*corev1alpha1.OutboundAccessPolicy{
		"subject": reservedSAPolicy("orka-controller", ""),
		"actor":   reservedSAPolicy("workload", "orka-controller"),
	} {
		t.Run(name, func(t *testing.T) {
			reader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(policy,
				&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "orka-controller", Namespace: "tenant"}},
				&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant"}},
			).Build()
			clientset := k8sfake.NewSimpleClientset()
			var requested []string
			clientset.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "token" {
					return false, nil, nil
				}
				requested = append(requested, action.(ktesting.CreateActionImpl).Name)
				return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "minted"}}, nil
			})
			exchanger := &captureExchanger{result: tokenexchange.Result{AccessToken: "x", IssuedTokenType: tokenexchange.TokenTypeAccessToken}}
			resolver := &KubernetesResolver{
				Reader: reader, KubeClient: clientset, Exchanger: exchanger,
				ReservedServiceAccount: ReservedServiceAccount{Namespace: "tenant", Name: "orka-controller"},
			}
			_, err := resolver.Resolve(context.Background(), ResolveRequest{
				Namespace: "tenant", PolicyName: "direct", TargetScheme: "https", CredentialScopeAllowed: true,
			})
			if err == nil || !strings.Contains(err.Error(), "controller's own ServiceAccount") {
				t.Fatalf("Resolve() error = %v, want the controller's ServiceAccount refused", err)
			}
			if exchanger.request.SubjectToken != "" || slices.Contains(requested, "orka-controller") {
				t.Fatalf("requested tokens for %v, exchanged %q; want none for the controller", requested, exchanger.request.SubjectToken)
			}
		})
	}
}

func TestServiceAccountFromToken(t *testing.T) {
	jwt := func(payload string) string {
		return "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
	}
	got, err := ServiceAccountFromToken(jwt(`{"sub":"system:serviceaccount:orka-system:orka-controller"}`) + "\n")
	if err != nil || got != (ReservedServiceAccount{Namespace: "orka-system", Name: "orka-controller"}) {
		t.Fatalf("ServiceAccountFromToken() = %+v, %v", got, err)
	}
	for _, token := range []string{"", "not-a-jwt", jwt(`{"sub":"user@example.com"}`), jwt(`{"sub":"system:serviceaccount:ns:"}`), "a.!!.c"} {
		if _, err := ServiceAccountFromToken(token); err == nil {
			t.Fatalf("ServiceAccountFromToken(%q) = nil error", token)
		}
	}
}
