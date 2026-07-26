package gateway

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	orkalabels "github.com/orka-agents/orka/internal/labels"
)

func TestReadBearerSecretSupportsLongGatewayNames(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	name := strings.Repeat("gateway-", 12) + "edge"
	object := &gatewayv1alpha1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gatewayv1alpha1.GatewaySpec{
			InboundAuthRef: gatewayv1alpha1.GatewayBearerAuthReference{Name: "inbound", Key: "token"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "inbound", Namespace: "default",
			Labels: map[string]string{
				GatewayInboundAuthLabel: GatewayAuthEnabledValue,
				GatewayAuthNameLabel:    orkalabels.SelectorValue(name),
			},
			Annotations: map[string]string{GatewayAuthNameAnnotation: name},
		},
		Data: map[string][]byte{"token": []byte("auth-value")},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	result, err := ReadBearerSecret(context.Background(), client, object, AuthDirectionInbound, "")
	if err != nil || result.Token != "auth-value" {
		t.Fatalf("ReadBearerSecret() = (%+v, %v)", result, err)
	}
}
