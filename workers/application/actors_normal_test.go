//go:build linux

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

func TestApplicationNormalActorFreezesWriteResponseWithoutLaterGET(t *testing.T) {
	for _, initial := range []string{"true", "null", "9007199254740993"} {
		t.Run(initial, func(t *testing.T) {
			actor, action, target := applicationNormalActor(t, json.RawMessage(initial))
			receipt, err := actor.Apply(t.Context(), "run-owned", action, target)
			require.NoError(t, err)
			require.Equal(t, 201, receipt.StatusCode)
			require.NotNil(t, receipt.InitialAPI)
			require.Equal(t, "created-uid", receipt.InitialAPI.UID)
			require.Equal(t, "17", receipt.InitialAPI.ResourceVersion)
			require.Equal(t, "/status/accepted", receipt.InitialAPI.Pointer)
			require.True(t, receipt.InitialAPI.Result.Exists)
			require.True(t, receipt.InitialAPI.Result.Found)
			require.Equal(t, pv.Digest([]byte(initial)), receipt.InitialAPI.Result.ValueDigest)
			require.Equal(t, len(initial), receipt.InitialAPI.Result.ValueBytes)
		})
	}
}

func TestApplicationNormalActorNeverTransportsRawObservedStatus(t *testing.T) {
	value := "observed-only-" + rand.Text()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	actor, action, target := applicationNormalActor(t, raw)
	receipt, err := actor.Apply(t.Context(), "run-owned", action, target)
	require.NoError(t, err)
	require.NotNil(t, receipt.InitialAPI)
	content, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.NotContains(t, string(content), value)
	require.Equal(t, pv.Digest(raw), receipt.InitialAPI.Result.ValueDigest)
}

func TestApplicationNormalActorPreservesMutationReceiptWhenStatusIsOversized(t *testing.T) {
	raw, err := json.Marshal(strings.Repeat("x", 17<<10))
	require.NoError(t, err)
	actor, action, target := applicationNormalActor(t, raw)
	receipt, err := actor.Apply(t.Context(), "run-owned", action, target)
	require.NoError(t, err, "known UID/RV must remain available for cleanup even when normal evidence is unusable")
	require.Equal(t, "created-uid", receipt.UID)
	require.Equal(t, "17", receipt.ResourceVersion)
	require.Equal(t, 201, receipt.StatusCode)
	require.Nil(t, receipt.InitialAPI, "a missing initial status cannot satisfy final normal evidence")
}

func applicationNormalActor(
	t *testing.T, initial json.RawMessage,
) (actorHTTPClient, pv.ApplicationAction, pv.ApplicationActionTarget) {
	t.Helper()
	policy := &pv.ApplicationPolicy{Version: pv.ApplicationProtocolVersion,
		Resources: []pv.ApplicationResourceRule{{
			APIVersion: "testing.example/v1", Kind: "Widget", Resource: "widgets", Namespaced: true, Status: true,
			Verbs: []string{"get", "create", "update", "patch"},
			CRD: &pv.ApplicationCRDIdentity{
				Name: "widgets.testing.example", UID: "approved-crd", SchemaDigest: pv.Digest([]byte("schema")),
			},
		}}}
	require.NoError(t, pv.ValidateApplicationPolicy(*policy))
	action := pv.ApplicationAction{ActorID: "author", Verb: "create", ExpectedStatus: 201,
		Resource: json.RawMessage(
			`{"apiVersion":"testing.example/v1","kind":"Widget","metadata":{"name":"normal"},"spec":{"enabled":true}}`,
		)}
	target := pv.ApplicationActionTarget{Index: 0, APIVersion: "testing.example/v1", Resource: "widgets",
		Name: "normal", MustBeAbsent: true, OwnerUID: "run-owner"}
	check := &pv.Check{ID: "normal", Kind: pv.Normal, Application: &pv.ApplicationCheck{
		Kubernetes: &pv.ApplicationKubernetesCheck{APIVersion: "testing.example/v1", Resource: "widgets",
			Name: "normal", Pointer: "/status/accepted", NormalValue: []byte("true")},
	}}
	requests := 0
	client := &http.Client{Transport: applicationTransportFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		require.Equal(t, 1, requests, "initial status must come from the write response, not a racing later GET")
		require.Equal(t, http.MethodPost, request.Method)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil {
			return nil, err
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(object["metadata"], &metadata); err != nil {
			return nil, err
		}
		metadata["uid"], metadata["resourceVersion"] = json.RawMessage(`"created-uid"`), json.RawMessage(`"17"`)
		object["metadata"], err = json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		object["status"] = json.RawMessage(`{"accepted":` + string(initial) + `}`)
		response, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(bytes.NewReader(response)), Header: make(http.Header)}, nil
	})}
	return actorHTTPClient{client: client, host: "https://target.invalid", policy: policy, check: check,
		identity: pv.ApplicationActorIdentity{ID: "author", Namespace: "protected-run", NamespaceUID: "protected-uid",
			ServiceAccountName: "author", ServiceAccountUID: "author-uid"}}, action, target
}
