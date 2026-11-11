//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

func TestActorCreateAndUpdatePreserveFrozenNumericSpecOnWire(t *testing.T) {
	for _, verb := range []string{"create", "update"} {
		for _, number := range []string{
			"9007199254740991", "9007199254740992", "9007199254740993", "9223372036854775807", "-9223372036854775808",
		} {
			t.Run(verb+"/"+number, func(t *testing.T) {
				policy := &pv.ApplicationPolicy{Version: pv.ApplicationProtocolVersion,
					Resources: []pv.ApplicationResourceRule{{APIVersion: "testing.example/v1", Kind: "Widget", Resource: "widgets",
						Namespaced: true, Verbs: []string{"create", "update"},
						CRD: &pv.ApplicationCRDIdentity{
							Name: "widgets.testing.example", UID: "approved-crd", SchemaDigest: pv.Digest([]byte("schema")),
						}}}}
				require.NoError(t, pv.ValidateApplicationPolicy(*policy))
				spec := fmt.Sprintf(
					`{"count":%s,"nested":{"values":[%s,{"fraction":0.125,"negative":-3.75,"value":%s}]}}`,
					number, number, number,
				)
				raw := json.RawMessage(
					`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"numeric"},"spec":` + spec + `}`,
				)
				before := bytes.Clone(raw)
				status := http.StatusCreated
				target := pv.ApplicationActionTarget{Index: 0, APIVersion: "testing.example/v1", Resource: "widgets",
					Name: "numeric", MustBeAbsent: true, OwnerUID: "run-owner"}
				if verb == "update" {
					status, target.MustBeAbsent, target.UID, target.ResourceVersion = http.StatusOK, false, "object-uid", "7"
				}
				action := pv.ApplicationAction{ActorID: "author", Verb: verb, Resource: raw, ExpectedStatus: status}
				var sent []byte
				client := &http.Client{Transport: applicationTransportFunc(func(request *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					sent = body
					var object map[string]json.RawMessage
					if err := json.Unmarshal(body, &object); err != nil {
						return nil, err
					}
					var metadata map[string]json.RawMessage
					if err := json.Unmarshal(object["metadata"], &metadata); err != nil {
						return nil, err
					}
					metadata["uid"], metadata["resourceVersion"] = json.RawMessage(`"object-uid"`), json.RawMessage(`"8"`)
					object["metadata"], err = json.Marshal(metadata)
					if err != nil {
						return nil, err
					}
					response, err := json.Marshal(object)
					if err != nil {
						return nil, err
					}
					return &http.Response{
						StatusCode: status, Body: io.NopCloser(bytes.NewReader(response)), Header: make(http.Header),
					}, nil
				})}
				actor := actorHTTPClient{client: client, host: "https://target.invalid", policy: policy,
					identity: pv.ApplicationActorIdentity{ID: "author", Namespace: "protected-run", NamespaceUID: "protected-uid",
						ServiceAccountName: "author", ServiceAccountUID: "author-uid"}}
				receipt, err := actor.Apply(t.Context(), "run-owned", action, target)
				require.NoError(t, err)
				require.Equal(t, status, receipt.StatusCode)
				require.Equal(t, before, []byte(action.Resource))
				var wire map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(sent, &wire))
				decoder := json.NewDecoder(bytes.NewReader([]byte(spec)))
				decoder.UseNumber()
				var expected map[string]any
				require.NoError(t, decoder.Decode(&expected))
				want, err := json.Marshal(expected)
				require.NoError(t, err)
				var compact bytes.Buffer
				require.NoError(t, json.Compact(&compact, wire["spec"]))
				require.Equal(t, want, compact.Bytes(), "wire spec must retain the exact integer, not its float64 approximation")
			})
		}
	}
}
