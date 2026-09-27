/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"encoding/json"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestValidateToolArguments(t *testing.T) {
	schema := &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","required":["q"],"properties":{"q":{"type":"string"},"limit":{"type":"integer","maximum":10}},"additionalProperties":false}`)}
	cases := []struct {
		name       string
		parameters *apiextensionsv1.JSON
		arguments  string
		wantErr    bool
	}{
		{name: "valid", parameters: schema, arguments: `{"q":"x","limit":5}`},
		{name: "missing required", parameters: schema, arguments: `{"limit":5}`, wantErr: true},
		{name: "over maximum", parameters: schema, arguments: `{"q":"x","limit":11}`, wantErr: true},
		{name: "wrong type", parameters: schema, arguments: `{"q":7}`, wantErr: true},
		{name: "unknown property", parameters: schema, arguments: `{"q":"x","extra":true}`, wantErr: true},
		{name: "not an object", parameters: schema, arguments: `["q"]`, wantErr: true},
		{name: "trailing value", parameters: schema, arguments: `{"q":"x"} {}`, wantErr: true},
		{name: "no schema still needs an object", parameters: nil, arguments: `"q"`, wantErr: true},
		{name: "no schema accepts any object", parameters: nil, arguments: `{"anything":1}`},
		{name: "invalid schema fails closed", parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":7}`)}, arguments: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateToolArguments(tc.parameters, json.RawMessage(tc.arguments))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
