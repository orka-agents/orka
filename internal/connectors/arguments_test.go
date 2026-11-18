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
		// A parameterless call arrives empty and is judged as {}.
		{name: "empty payload is the empty object", parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)}, arguments: ``},
		{name: "blank payload is the empty object", parameters: nil, arguments: "  \n"},
		{name: "empty payload still meets required fields", parameters: schema, arguments: ``, wantErr: true},
		{name: "invalid schema fails closed", parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":7}`)}, arguments: `{}`, wantErr: true},
		// The schema is judged on a float64 copy; a number that copy cannot
		// hold exactly could pass a bound the raw value violates, so it is
		// refused outright.
		{name: "exact large integer", parameters: nil, arguments: `{"n":9007199254740992}`},
		{name: "inexact large integer", parameters: nil, arguments: `{"n":9007199254740993}`, wantErr: true},
		{name: "inexact nested decimal", parameters: nil, arguments: `{"a":[{"n":0.1000000000000000055511151231257827}]}`, wantErr: true},
		{name: "exact decimal", parameters: nil, arguments: `{"n":0.5}`},
		{name: "ordinary decimal", parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"n":{"type":"number","maximum":0.1}}}`)}, arguments: `{"n":0.1}`},
		{name: "ordinary decimal over bound", parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"n":{"type":"number","maximum":0.1}}}`)}, arguments: `{"n":0.2}`, wantErr: true},
		{name: "scientific decimal", parameters: nil, arguments: `{"n":1.5e-7}`},
		// A schema whose own constants would round is refused, or an
		// argument equal to the rounded value would pass a constraint the
		// provider never declared.
		{name: "inexact schema const", parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"id":{"const":9007199254740993}}}`)}, arguments: `{"id":9007199254740992}`, wantErr: true},
		{name: "exact schema const", parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"id":{"const":9007199254740992}}}`)}, arguments: `{"id":9007199254740992}`},
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
