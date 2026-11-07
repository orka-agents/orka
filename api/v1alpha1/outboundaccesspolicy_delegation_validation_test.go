package v1alpha1

import (
	"testing"

	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

// TestMaxDelegationAgeMustDecodeAsABoundedDuration covers values the API
// server must refuse: one that cannot decode as a Go duration would keep the
// controller from reading any OutboundAccessPolicy.
func TestMaxDelegationAgeMustDecodeAsABoundedDuration(t *testing.T) {
	const crd = "core.orka.ai_outboundaccesspolicies.yaml"
	rootValidator := loadRootValidator(t, crd)
	validator, structural := loadSpecCELValidator(t, crd)
	for _, tc := range []struct {
		age       string
		wantError bool
	}{
		{age: "1h"}, {age: "24h0m0s"}, {age: "90m"}, {age: "8760h"}, {age: "8760h0m0s"}, {age: "30s"},
		{age: "not-a-duration", wantError: true}, {age: "999999999999h", wantError: true},
		{age: "9223372037s", wantError: true}, {age: "0s", wantError: true}, {age: "-1h", wantError: true},
		{age: "8761h", wantError: true}, {age: "", wantError: true},
	} {
		spec := map[string]any{"agentIdentity": map[string]any{
			"mode":      "Delegated",
			"delegated": map[string]any{"providerRef": map[string]any{"name": "entra-signin"}, "maxDelegationAge": tc.age},
			"entra":     map[string]any{"resource": "https://graph.microsoft.com"},
		}}
		schemaErrs := schemavalidation.ValidateCustomResource(nil, map[string]any{"spec": spec}, rootValidator)
		var celErrs []error
		if len(schemaErrs) == 0 {
			errs, _ := validator.Validate(t.Context(), nil, structural, spec, nil, celconfig.RuntimeCELCostBudget)
			for _, err := range errs {
				celErrs = append(celErrs, err)
			}
		}
		if (len(schemaErrs)+len(celErrs) != 0) != tc.wantError {
			t.Errorf("maxDelegationAge %q: schema errors = %v, cel errors = %v, want rejection %t", tc.age, schemaErrs, celErrs, tc.wantError)
		}
	}
}
