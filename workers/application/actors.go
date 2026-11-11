//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/rest"
)

type actorHTTPClient struct {
	client   *http.Client
	host     string
	identity pv.ApplicationActorIdentity
	policy   *pv.ApplicationPolicy
	check    *pv.Check
}

func actorClients(
	ctx context.Context, input pv.ApplicationWorkerInput, config *rest.Config,
) (map[string]pv.ApplicationActorClient, func(), error) {
	result := map[string]pv.ApplicationActorClient{}
	var check *pv.Check
	for index := range input.Manifest.Checks {
		if input.Manifest.Checks[index].ID == input.CheckID {
			check = &input.Manifest.Checks[index]
		}
	}
	httpClients := []*http.Client{}
	cleanup := func() {
		for _, client := range httpClients {
			client.CloseIdleConnections()
		}
	}
	for _, actor := range input.Actors {
		actorConfig := rest.CopyConfig(config)
		actorConfig.BearerToken, actorConfig.BearerTokenFile = "", "/actors/"+actor.ID+"/token"
		transport, err := rest.TransportFor(actorConfig)
		if err != nil {
			cleanup()
			return nil, nil, errors.New("application actor token is unavailable")
		}
		client := &http.Client{Transport: transport, Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		httpClients = append(httpClients, client)
		status, body, err := boundaryRequest(ctx, client, config.Host, http.MethodPost,
			"/apis/authentication.k8s.io/v1/selfsubjectreviews",
			[]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`))
		var review struct {
			Status struct {
				UserInfo struct {
					Username string `json:"username"`
					UID      string `json:"uid"`
				} `json:"userInfo"`
			} `json:"status"`
		}
		if err != nil || status != http.StatusCreated || json.Unmarshal(body, &review) != nil ||
			review.Status.UserInfo.Username != "system:serviceaccount:"+actor.Namespace+":"+actor.ServiceAccountName ||
			review.Status.UserInfo.UID != actor.ServiceAccountUID ||
			actor.ServiceAccountUID == input.Worker.ServiceAccountUID ||
			actor.Namespace != input.Worker.Namespace || actor.NamespaceUID != input.Worker.NamespaceUID {
			cleanup()
			return nil, nil, errors.New("application actor credentials authenticate a different identity")
		}
		result[actor.ID] = &actorHTTPClient{
			client: client, host: config.Host, identity: actor, policy: input.Manifest.ApplicationPolicy, check: check,
		}
	}
	return result, cleanup, nil
}

func (actor *actorHTTPClient) Apply(
	ctx context.Context, namespace string, action pv.ApplicationAction, target pv.ApplicationActionTarget,
) (pv.ApplicationActionObservation, error) {
	if pv.ValidateApplicationActionTarget(action, target, target.Index, actor.policy) != nil ||
		action.ActorID != actor.identity.ID {
		return pv.ApplicationActionObservation{}, pv.ErrBinding
	}
	object, err := pv.ValidateApplicationResourceWithPolicy(action.Resource, actor.policy)
	if err != nil {
		return pv.ApplicationActionObservation{}, err
	}
	object.SetNamespace(namespace)
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[pv.ApplicationRunLabel] = target.OwnerUID
	object.SetLabels(labels)
	method := http.MethodPost
	if action.Verb == pv.ApplicationActionUpdate {
		method = http.MethodPut
		object.SetUID(types.UID(target.UID))
		object.SetResourceVersion(target.ResourceVersion)
	}
	version, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return pv.ApplicationActionObservation{}, pv.ErrEvidence
	}
	path := "/api/" + version.Version
	if version.Group != "" {
		path = "/apis/" + version.Group + "/" + version.Version
	}
	path += "/namespaces/" + namespace + "/" + target.Resource
	if action.Verb == pv.ApplicationActionUpdate {
		path += "/" + target.Name
	}
	query := url.Values{"fieldManager": {"orka-application-verification"}, "fieldValidation": {"Strict"}}
	content, err := json.Marshal(object.Object)
	if err != nil {
		return pv.ApplicationActionObservation{}, pv.ErrEvidence
	}
	request, err := http.NewRequestWithContext(ctx, method, actor.host+path+"?"+query.Encode(), bytes.NewReader(content))
	if err != nil {
		return pv.ApplicationActionObservation{}, pv.ErrEvidence
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := actor.client.Do(request)
	if err != nil {
		return pv.ApplicationActionObservation{}, errors.New("application actor effect outcome is unknown")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 || ctx.Err() != nil {
		return pv.ApplicationActionObservation{}, errors.New("application actor response was incomplete")
	}
	receipt := pv.ApplicationActionObservation{Index: target.Index, Actor: actor.identity, APIVersion: target.APIVersion,
		Resource: target.Resource, Name: target.Name, StatusCode: response.StatusCode, Target: target}
	switch response.StatusCode {
	case 400, 401, 403, 404, 409, 422, 429:
		var status struct {
			Kind   string `json:"kind"`
			Status string `json:"status"`
			Reason string `json:"reason"`
			Code   int    `json:"code"`
		}
		if json.Unmarshal(body, &status) != nil || status.Kind != "Status" || status.Status != "Failure" ||
			status.Code != response.StatusCode {
			return pv.ApplicationActionObservation{}, pv.ErrEvidence
		}
		if response.StatusCode == 403 {
			receipt.AuthorizationDenied = actor.authorizationDenied(ctx, namespace, action.Verb, target)
		}
	case 200, 201:
		var result unstructured.Unstructured
		if kjson.Unmarshal(body, &result.Object) != nil || result.GetName() != target.Name ||
			result.GetNamespace() != namespace || result.GetAPIVersion() != target.APIVersion ||
			result.GetKind() != object.GetKind() || result.GetDeletionTimestamp() != nil ||
			result.GetLabels()[pv.ApplicationRunLabel] != target.OwnerUID {
			return pv.ApplicationActionObservation{}, pv.ErrEvidence
		}
		receipt.UID, receipt.ResourceVersion = string(result.GetUID()), result.GetResourceVersion()
		receipt.InitialAPI = actor.initialAPI(namespace, action, &result)
	default:
		return pv.ApplicationActionObservation{},
			errors.New("application actor effect did not establish the admitted outcome")
	}
	if err := pv.ValidateApplicationActionObservation(action, target, actor.identity, receipt, actor.policy); err != nil {
		return pv.ApplicationActionObservation{}, err
	}
	return receipt, nil
}

func (actor *actorHTTPClient) initialAPI(
	namespace string, action pv.ApplicationAction, object *unstructured.Unstructured,
) *pv.ApplicationAPIIdentity {
	if actor.check == nil || !pv.ApplicationActionObservesAPI(*actor.check, action, actor.policy) {
		return nil
	}
	initial, err := pv.ApplicationAPIIdentityFromObject(
		object, namespace, *actor.check.Application.Kubernetes, actor.policy,
	)
	if err != nil {
		// Preserve the known mutation receipt even if status cannot be reduced.
		// Missing initial evidence rejects the final normal observation.
		return nil
	}
	return initial
}

func (actor *actorHTTPClient) authorizationDenied(
	ctx context.Context, namespace, verb string, target pv.ApplicationActionTarget,
) bool {
	version, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return false
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
		"spec": map[string]any{"resourceAttributes": map[string]string{
			"namespace": namespace, "verb": verb, "group": version.Group, "version": version.Version,
			"resource": target.Resource, "name": target.Name,
		}},
	})
	if err != nil {
		return false
	}
	status, content, err := boundaryRequest(ctx, actor.client, actor.host, http.MethodPost,
		"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", body)
	var review struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
		Status     struct {
			Allowed         *bool  `json:"allowed"`
			EvaluationError string `json:"evaluationError"`
		} `json:"status"`
	}
	return err == nil && status == 201 && json.Unmarshal(content, &review) == nil &&
		review.Kind == "SelfSubjectAccessReview" && review.APIVersion == "authorization.k8s.io/v1" &&
		review.Status.Allowed != nil && !*review.Status.Allowed && review.Status.EvaluationError == ""
}
