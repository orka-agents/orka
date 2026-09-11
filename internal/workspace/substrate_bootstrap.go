package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func isSubstrateHandoffUpload(path string) bool {
	clean := filepath.Clean(path)
	return clean == substrateHandoffTokenUploadPath || clean == "/app/"+substrateHandoffTokenUploadPath || clean == "/workspace/"+substrateHandoffTokenUploadPath
}

func (e *SubstrateWorkspaceExecutor) seedNativeWorkspaceCredential(ctx context.Context, actorID, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("native workspace handoff credential is empty")
	}
	actor, err := e.control.GetActor(ctx, actorID)
	if err != nil {
		return err
	}
	if actor == nil || actor.ActorUID == "" || actor.Status != substrateStatusRunning {
		return fmt.Errorf("native bootstrap requires an exact running Actor")
	}
	if (e.sessionIdentity != nil || e.sessionIdentityToken != "") && e.cacheSessionIdentityHandoff(actorID, actor, "") != token {
		return fmt.Errorf("native workspace credential belongs to another Actor or Pod lifetime")
	}
	payload, err := json.Marshal(harnessv2.WorkspaceBootstrapRequest{HandoffToken: token})
	if err != nil {
		return err
	}
	response, _, err := e.nativeWorkspaceBootstrapRequest(ctx, actorID, actor, http.MethodPut, payload)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("native workspace rejected sealed bootstrap with HTTP %d", response.StatusCode)
	}
	return e.confirmNativeWorkspaceBootstrapLifetime(ctx, actorID, actor)
}

func (e *SubstrateWorkspaceExecutor) recoverNativeWorkspaceCredential(ctx context.Context, actorID string, actor *substrateActor) (string, error) {
	response, exchange, err := e.nativeWorkspaceBootstrapRequest(ctx, actorID, actor, http.MethodPost, []byte(`{"recover":true}`))
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("native workspace credential recovery is unavailable with HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return "", fmt.Errorf("native workspace credential recovery response is invalid")
	}
	plaintext, err := exchange.OpenResponse(body)
	if err != nil {
		return "", err
	}
	var recovered harnessv2.WorkspaceBootstrapRequest
	if json.Unmarshal(plaintext, &recovered) != nil || recovered.Recover || len(recovered.HandoffToken) > 32768 ||
		recovered.HandoffToken != strings.TrimSpace(recovered.HandoffToken) {
		return "", fmt.Errorf("native workspace credential recovery payload is invalid")
	}
	if err := e.confirmNativeWorkspaceBootstrapLifetime(ctx, actorID, actor); err != nil {
		return "", err
	}
	// An authenticated empty response proves this process has not been seeded.
	// A recovered JWT stays in memory and never replaces the installed token.
	return recovered.HandoffToken, nil
}

func (e *SubstrateWorkspaceExecutor) nativeWorkspaceBootstrapRequest(
	ctx context.Context, actorID string, actor *substrateActor, method string, payload []byte,
) (*http.Response, *harnessv2.CredentialBootstrapExchange, error) {
	secret, err := e.requireBootstrapToken("native bootstrap")
	if err != nil {
		return nil, nil, err
	}
	if actor == nil || actor.ActorUID == "" || actor.Status != substrateStatusRunning {
		return nil, nil, fmt.Errorf("native bootstrap requires an exact running Actor")
	}
	ref, err := substrateObjectRef(actorID, actor.Atespace)
	if err != nil {
		return nil, nil, err
	}
	identity := harnessv2.SubstrateActorIdentity{Atespace: ref.Atespace, Name: ref.Name, UID: actor.ActorUID}
	key, err := harnessv2.WorkspaceBootstrapPublicKey(secret)
	if err != nil {
		return nil, nil, err
	}
	nonce := harnessv2.WorkspaceBootstrapNonce(key)
	url := e.routerURL + harnessv2.CredentialBootstrapPath
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid native bootstrap route")
	}
	get.Host = SubstrateActorKey(ref.Atespace, ref.Name) + "." + e.actorDNSSuffix
	client := *e.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(get)
	if err != nil {
		return nil, nil, fmt.Errorf("native workspace bootstrap challenge is unavailable")
	}
	var challenge harnessv2.SealedBootstrapChallenge
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&challenge)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil {
		return nil, nil, fmt.Errorf("native workspace did not provide a sealed bootstrap challenge")
	}
	// The provider projects Actor identity but not Pod identity into the
	// process. A read after the challenge binds its process key to the observed
	// assignment; later rerouting cannot decrypt this process's envelope.
	observed, err := e.control.GetActor(ctx, actorID)
	if err != nil {
		return nil, nil, err
	}
	if !nativeWorkspaceBootstrapLifetimeMatches(actor, observed) || observed.ActorVersion != actor.ActorVersion {
		return nil, nil, fmt.Errorf("native Actor lifetime changed before credential bootstrap")
	}
	body, exchange, err := harnessv2.SealCredentialBootstrapExchange(challenge, nonce, identity, payload)
	if err != nil {
		return nil, nil, err
	}
	signature, err := harnessv2.SignCredentialBootstrap(harnessv2.WorkspaceBootstrapSigningSeed(secret), nonce, body)
	if err != nil {
		return nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("invalid native bootstrap route")
	}
	request.Host = get.Host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(harnessv2.CredentialBootstrapNonceHeader, nonce)
	request.Header.Set(harnessv2.CredentialBootstrapSignatureHeader, signature)
	response, err = client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("native workspace bootstrap result is unconfirmed")
	}
	return response, exchange, nil
}

func (e *SubstrateWorkspaceExecutor) confirmNativeWorkspaceBootstrapLifetime(ctx context.Context, actorID string, actor *substrateActor) error {
	observed, err := e.control.GetActor(ctx, actorID)
	if err != nil {
		return err
	}
	if !nativeWorkspaceBootstrapLifetimeMatches(actor, observed) {
		return fmt.Errorf("native Actor lifetime changed during credential bootstrap")
	}
	return nil
}

func nativeWorkspaceBootstrapLifetimeMatches(expected, observed *substrateActor) bool {
	return expected != nil && observed != nil && observed.ActorUID == expected.ActorUID &&
		observed.PodUID == expected.PodUID && observed.Status == substrateStatusRunning
}
