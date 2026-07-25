/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package gateway

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	orkalabels "github.com/orka-agents/orka/internal/labels"
)

// AuthDirection identifies one side of the gateway trust boundary.
type AuthDirection string

const (
	AuthDirectionInbound  AuthDirection = "inbound"
	AuthDirectionOutbound AuthDirection = "outbound"
)

// BearerSecretResult contains a token for immediate use plus non-secret version metadata.
type BearerSecretResult struct {
	Token           string
	ResourceVersion string
}

// ReadBearerSecret resolves and validates one Gateway-bound bearer Secret.
func ReadBearerSecret(
	ctx context.Context,
	kubeClient client.Reader,
	object *gatewayv1alpha1.Gateway,
	direction AuthDirection,
	resolvedEndpoint string,
) (BearerSecretResult, error) {
	if kubeClient == nil || object == nil {
		return BearerSecretResult{}, fmt.Errorf("gateway and Kubernetes client are required")
	}
	var ref gatewayv1alpha1.GatewayBearerAuthReference
	var optInLabel string
	switch direction {
	case AuthDirectionInbound:
		ref = object.Spec.InboundAuthRef
		optInLabel = GatewayInboundAuthLabel
	case AuthDirectionOutbound:
		ref = object.Spec.OutboundAuthRef
		optInLabel = GatewayOutboundAuthLabel
	default:
		return BearerSecretResult{}, fmt.Errorf("unsupported gateway auth direction %q", direction)
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: object.Namespace, Name: strings.TrimSpace(ref.Name)}
	if err := kubeClient.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return BearerSecretResult{}, fmt.Errorf("%s bearer Secret %q not found", direction, key.Name)
		}
		return BearerSecretResult{}, fmt.Errorf("read %s bearer Secret %q: %w", direction, key.Name, err)
	}
	if secret.Labels[optInLabel] != GatewayAuthEnabledValue {
		return BearerSecretResult{}, fmt.Errorf("%s bearer Secret %q must be labeled %s=%s", direction, key.Name, optInLabel, GatewayAuthEnabledValue)
	}
	expectedLabel := orkalabels.SelectorValue(object.Name)
	if boundName := strings.TrimSpace(secret.Labels[GatewayAuthNameLabel]); boundName != "" && boundName != expectedLabel {
		return BearerSecretResult{}, fmt.Errorf("%s bearer Secret %q has a mismatched Gateway name label", direction, key.Name)
	}
	if boundName := strings.TrimSpace(secret.Annotations[GatewayAuthNameAnnotation]); boundName != object.Name {
		return BearerSecretResult{}, fmt.Errorf("%s bearer Secret %q must be annotated with the exact Gateway name", direction, key.Name)
	}
	if direction == AuthDirectionOutbound {
		boundEndpoint := strings.TrimSpace(secret.Annotations[GatewayAuthEndpointAnnotation])
		if boundEndpoint == "" {
			return BearerSecretResult{}, fmt.Errorf("outbound bearer Secret %q must be annotated %s=<resolved adapter endpoint>", key.Name, GatewayAuthEndpointAnnotation)
		}
		if boundEndpoint != strings.TrimSpace(resolvedEndpoint) {
			return BearerSecretResult{}, fmt.Errorf("outbound bearer Secret %q is bound to a different adapter endpoint", key.Name)
		}
	}
	value := strings.TrimSpace(string(secret.Data[strings.TrimSpace(ref.Key)]))
	if value == "" {
		return BearerSecretResult{}, fmt.Errorf("%s bearer Secret %q key %q is empty or missing", direction, key.Name, ref.Key)
	}
	return BearerSecretResult{Token: value, ResourceVersion: secret.ResourceVersion}, nil
}
