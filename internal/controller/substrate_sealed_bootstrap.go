package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type substrateNativeBootstrapControl interface {
	NativeBootstrapSupported() bool
	NativeBootstrapIdentity(context.Context, string) (harnessv2.SubstrateActorIdentity, error)
}

func (c *nativeSubstrateRuntimeActorControl) NativeBootstrapSupported() bool { return true }

func (c *nativeSubstrateRuntimeActorControl) NativeBootstrapIdentity(ctx context.Context, actorID string) (harnessv2.SubstrateActorIdentity, error) {
	actor, err := c.SubstrateRuntimeActorControl.GetActor(ctx, actorID)
	if err != nil {
		return harnessv2.SubstrateActorIdentity{}, err
	}
	if actor == nil || !actor.Running() || actor.Atespace != c.atespace || actor.PodUID == "" {
		return harnessv2.SubstrateActorIdentity{}, errSubstrateCredentialFenceConflict
	}
	_, binding, err := c.store.read(ctx, c.atespace, c.logicalTemplate)
	if err != nil {
		return harnessv2.SubstrateActorIdentity{}, err
	}
	if binding == nil || actor.TemplateName != binding.Current.Name || actor.TemplateUID != binding.Current.UID {
		return harnessv2.SubstrateActorIdentity{}, errSubstrateCredentialFenceConflict
	}
	identity := harnessv2.SubstrateActorIdentity{Atespace: actor.Atespace, Name: actor.ActorID, UID: actor.ActorUID}
	return identity, identity.Validate()
}

func (c *substrateRuntimeActorControlWithTimeout) NativeBootstrapSupported() bool {
	native, ok := c.delegate.(substrateNativeBootstrapControl)
	return ok && native.NativeBootstrapSupported()
}

func (c *substrateRuntimeActorControlWithTimeout) NativeBootstrapIdentity(ctx context.Context, actorID string) (harnessv2.SubstrateActorIdentity, error) {
	native, ok := c.delegate.(substrateNativeBootstrapControl)
	if !ok || !native.NativeBootstrapSupported() {
		return harnessv2.SubstrateActorIdentity{}, fmt.Errorf("native Substrate bootstrap is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return native.NativeBootstrapIdentity(ctx, actorID)
}

func seedSealedSubstrateCredentials(ctx context.Context, transport *http.Client, endpoint, nonce string, signingSeed, plaintext []byte, expected harnessv2.SubstrateActorIdentity, recordChallenge ...func(harnessv2.SealedBootstrapChallenge) error) (bool, error) {
	// Bootstrap never follows redirects. The provider route is the trusted
	// transport boundary; the sealed body additionally binds the destination
	// process across replacement between the challenge and credential write.
	client := *transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	if response.StatusCode == http.StatusNotFound {
		_ = response.Body.Close()
		// The fully started supervisor has closed bootstrap. Admission still
		// requires its authenticated v2 identity/capacity probe.
		return true, nil
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return false, fmt.Errorf("native Substrate bootstrap challenge returned HTTP %d", response.StatusCode)
	}
	var challenge harnessv2.SealedBootstrapChallenge
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8193))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&challenge)
	if err == nil {
		if trailing := decoder.Decode(&struct{}{}); trailing != io.EOF {
			err = fmt.Errorf("bootstrap challenge has trailing data")
		}
	}
	_ = response.Body.Close()
	if err != nil {
		return false, fmt.Errorf("native Substrate bootstrap challenge is invalid")
	}
	if err := challenge.Validate(nonce, expected); err != nil {
		return false, errSubstrateCredentialFenceConflict
	}
	for _, record := range recordChallenge {
		if err := record(challenge); err != nil {
			return false, err
		}
	}
	envelope, err := harnessv2.SealCredentialBootstrap(challenge, nonce, expected, plaintext)
	if err != nil {
		return false, fmt.Errorf("seal Substrate bootstrap payload: %w", err)
	}
	signature, err := harnessv2.SignCredentialBootstrap(signingSeed, nonce, envelope)
	if err != nil {
		return false, err
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(envelope))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(harnessv2.CredentialBootstrapNonceHeader, nonce)
	request.Header.Set(harnessv2.CredentialBootstrapSignatureHeader, signature)
	response, err = client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close() //nolint:errcheck
	switch response.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return false, nil
	case http.StatusNotFound:
		return true, nil
	case http.StatusConflict:
		return false, errSubstrateCredentialConflict
	case http.StatusForbidden:
		return false, errSubstrateCredentialFenceConflict
	default:
		return false, fmt.Errorf("sealed Substrate bootstrap returned HTTP %d", response.StatusCode)
	}
}
