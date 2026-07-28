/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// TrustedServiceReferences contains exact namespace/name/port tuples. Wildcards
// are intentionally unsupported.
type TrustedServiceReferences map[string]struct{}

// TrustConfig separates gateway and token-endpoint trust domains.
type TrustConfig struct {
	Gateways       TrustedServiceReferences
	TokenEndpoints TrustedServiceReferences
}

// ParseTrustedServiceReferences parses comma-separated namespace/name:port
// tuples. Empty input produces an empty allowlist.
func ParseTrustedServiceReferences(value string) (TrustedServiceReferences, error) {
	refs := make(TrustedServiceReferences)
	for raw := range strings.SplitSeq(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if strings.ContainsAny(raw, "*?[]") {
			return nil, fmt.Errorf("trusted Service reference %q must not contain wildcards", raw)
		}
		namespacedName, portText, ok := strings.Cut(raw, ":")
		if !ok || portText == "" {
			return nil, fmt.Errorf("trusted Service reference %q must use namespace/name:port", raw)
		}
		namespace, name, ok := strings.Cut(namespacedName, "/")
		if !ok || namespace == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("trusted Service reference %q must use namespace/name:port", raw)
		}
		if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
			return nil, fmt.Errorf("trusted Service reference %q has an invalid namespace", raw)
		}
		if errs := validation.IsDNS1035Label(name); len(errs) > 0 {
			return nil, fmt.Errorf("trusted Service reference %q has an invalid name", raw)
		}
		port, err := strconv.ParseInt(portText, 10, 32)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("trusted Service reference %q has an invalid port", raw)
		}
		refs[serviceReferenceKey(namespace, name, int32(port))] = struct{}{}
	}
	return refs, nil
}

type TrustedServiceReference struct {
	Namespace string
	Name      string
	Port      int32
}

func (r TrustedServiceReferences) References() []TrustedServiceReference {
	out := make([]TrustedServiceReference, 0, len(r))
	for key := range r {
		namespacedName, portText, ok := strings.Cut(key, ":")
		if !ok {
			continue
		}
		namespace, name, ok := strings.Cut(namespacedName, "/")
		if !ok {
			continue
		}
		port, err := strconv.ParseInt(portText, 10, 32)
		if err != nil {
			continue
		}
		out = append(out, TrustedServiceReference{Namespace: namespace, Name: name, Port: int32(port)})
	}
	return out
}

func (r TrustedServiceReferences) Allows(ref corev1alpha1.OutboundServiceReference, policyNamespace string) bool {
	namespace := strings.TrimSpace(ref.Namespace)
	if namespace == "" {
		namespace = strings.TrimSpace(policyNamespace)
	}
	_, ok := r[serviceReferenceKey(namespace, strings.TrimSpace(ref.Name), ref.Port)]
	return ok
}

func serviceReferenceKey(namespace, name string, port int32) string {
	return fmt.Sprintf("%s/%s:%d", strings.TrimSpace(namespace), strings.TrimSpace(name), port)
}
