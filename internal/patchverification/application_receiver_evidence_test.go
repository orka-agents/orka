package patchverification

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestApplicationReceiverCompletionRequiresBoundRoute(t *testing.T) {
	service, worker, endpointSlice := applicationReceiverRouteFixture(t)
	route, err := FreezeApplicationReceiverRoute(service, worker, "127.0.0.1", "tcp", endpointSlice)
	require.NoError(t, err)
	expected := &ApplicationTCPCheck{Mode: "receive", WindowSeconds: 1}
	proof := ApplicationTCPIdentity{Mode: "receive", WindowSeconds: 1, ReceiverRoute: &route,
		Endpoint: ApplicationEndpoint{Pod: worker}}
	started := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	observation := Observation{PodUID: worker.PodUID, ContainerID: worker.ContainerID,
		StartedAt: started, FinishedAt: started.Add(time.Second)}
	require.NoError(t, validateApplicationTCPCompletion(expected, &proof, observation))
	for name, modify := range map[string]func(*ApplicationTCPIdentity){
		"missing route":       func(p *ApplicationTCPIdentity) { p.ReceiverRoute = nil },
		"foreign Pod":         func(p *ApplicationTCPIdentity) { p.ReceiverRoute.PodUID = "other-pod" },
		"foreign namespace":   func(p *ApplicationTCPIdentity) { p.ReceiverRoute.Namespace = "other-namespace" },
		"missing Service":     func(p *ApplicationTCPIdentity) { p.ReceiverRoute.ServiceUID = "" },
		"missing slice UID":   func(p *ApplicationTCPIdentity) { p.ReceiverRoute.EndpointSliceUID = "" },
		"missing slice RV":    func(p *ApplicationTCPIdentity) { p.ReceiverRoute.EndpointSliceResourceVersion = "" },
		"wrong route version": func(p *ApplicationTCPIdentity) { p.ReceiverRoute.Version++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed, changedRoute := proof, route
			changed.ReceiverRoute = &changedRoute
			modify(&changed)
			require.Error(t, validateApplicationTCPCompletion(expected, &changed, observation))
		})
	}
	observation.FinishedAt = started.Add(500 * time.Millisecond)
	require.Error(t, validateApplicationTCPCompletion(expected, &proof, observation),
		"a capsule cannot turn an incomplete window into evidence")
	expected.Mode, proof.Mode, proof.WindowSeconds = "", "", 0
	proof.Result.Connected = true
	require.Error(t, validateApplicationTCPCompletion(expected, &proof, observation),
		"request-mode evidence cannot smuggle a receiver capsule")
}

func applicationReceiverTestEvidence(t *testing.T) (Manifest, Binding, []Observation) {
	t.Helper()
	manifest := applicationTestManifest(t)
	manifest.ApplicationPolicy = applicationTestPolicy(t)
	manifest.Application.Fixtures = []ApplicationFixture{
		{ID: "protected-value", Kind: "marker", Port: 8081},
		{ID: "receiver", Kind: "receiver", Port: 8080},
	}
	none, err := ApplicationReceiveExpectation(false, false)
	require.NoError(t, err)
	present, err := ApplicationReceiveExpectation(true, true)
	require.NoError(t, err)
	for index := range manifest.Checks {
		check := &manifest.Checks[index]
		resource, err := json.Marshal(map[string]any{"apiVersion": "testing.example/v1", "kind": "Widget",
			"metadata": map[string]string{"name": "case-" + check.ID}, "spec": map[string]bool{"enabled": true}})
		require.NoError(t, err)
		check.Application = &ApplicationCheck{Version: ApplicationProtocolVersion,
			Actions: []ApplicationAction{{ActorID: "author", Verb: "create", Resource: resource, ExpectedStatus: 201}},
			TCP: &ApplicationTCPCheck{Mode: "receive", Target: "receiver", Port: 8080,
				MarkerID: "protected-value", MaxBytes: 1024, WindowSeconds: 1}}
		check.Healthy, check.Failure = none, present
		if check.Kind == Normal {
			check.Healthy, check.Failure = present, none
		}
	}
	require.NoError(t, ValidateManifest(manifest))
	binding, observations := applicationTestEvidence(t, manifest)
	for index := range observations {
		observation := &observations[index]
		identity := applicationTestIdentity("observer-"+observation.Side+"-"+observation.CheckID, observation.ImageID)
		identity.Namespace, identity.NamespaceUID = "protected-"+observation.Side, "protected-uid-"+observation.Side
		tcp := observation.Application.TCP
		tcp.Endpoint = ApplicationEndpoint{ID: "receiver", Address: "10.0.0.7", Port: 8080, Pod: identity}
		tcp.Mode, tcp.WindowSeconds, tcp.MarkerID = "receive", 1, "protected-value"
		tcp.ReceiverRoute = &ApplicationReceiverRoute{
			Version: 1, Namespace: identity.Namespace, ServiceUID: "receiver-service-" + observation.Side,
			PodUID: identity.PodUID, EndpointSliceName: "receiver-generated-" + observation.CheckID,
			EndpointSliceUID: "slice-" + identity.PodUID, EndpointSliceResourceVersion: "17",
		}
		tcp.MarkerRef = &ApplicationProtectedMarkerRef{
			Namespace: identity.Namespace, NamespaceUID: identity.NamespaceUID,
			Name: "protected-value", UID: "protected-value-" + observation.Side, Key: "marker", Digest: tcp.MarkerDigest,
		}
		tcp.Bytes = 0
		if tcp.Result.Connected {
			tcp.Connections, tcp.Bytes = 1, 64
		}
		actor := ApplicationActorIdentity{ID: "author", Namespace: identity.Namespace, NamespaceUID: identity.NamespaceUID,
			ServiceAccountName: "actor-author", ServiceAccountUID: "actor-author-" + observation.Side}
		target := ApplicationActionTarget{Index: 0, APIVersion: "testing.example/v1", Resource: "widgets",
			Name: "case-" + observation.CheckID, MustBeAbsent: true, OwnerUID: "run-ledger"}
		action := ApplicationActionObservation{
			Index: 0, Actor: actor, APIVersion: "testing.example/v1", Resource: "widgets", Name: target.Name,
			StatusCode: 201, UID: "created-" + observation.CheckID, ResourceVersion: "12", Target: target,
		}
		observation.Application.Actions = []ApplicationActionObservation{action}
	}
	return manifest, binding, observations
}

func TestApplicationReceiverEvaluationRequiresBothSideControlsAndRouteIdentity(t *testing.T) {
	manifest, binding, observations := applicationReceiverTestEvidence(t)
	for _, observation := range observations {
		require.NoError(t, ValidateObservation(manifest, binding, observation))
	}
	require.Equal(t, Verified, Evaluate(manifest, binding, observations).Conclusion)
	for index := range observations {
		missing := append([]Observation{}, observations[:index]...)
		missing = append(missing, observations[index+1:]...)
		require.NotEqual(t, Verified, Evaluate(manifest, binding, missing).Conclusion,
			"original/candidate normal controls and reproductions are all required")
	}
	originalRoute := observations[0].Application.TCP.ReceiverRoute
	observations[0].Application.TCP.ReceiverRoute = nil
	require.Error(t, ValidateObservation(manifest, binding, observations[0]))
	require.NotEqual(t, Verified, Evaluate(manifest, binding, observations).Conclusion)
	observations[0].Application.TCP.ReceiverRoute = originalRoute
	observations[2].Application.TCP.ReceiverRoute = originalRoute
	require.Error(t, ValidateObservation(manifest, binding, observations[2]),
		"an original observer route cannot be replayed as a candidate observer identity")
}
