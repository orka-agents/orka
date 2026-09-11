package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/stretchr/testify/require"
)

type substrateBootstrapRoundTripFunc func(*http.Request) (*http.Response, error)

func (f substrateBootstrapRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type substrateBootstrapRecoveryControl struct {
	*recordingSubstrateControlClient
	actor       substrateActor
	unavailable bool
}

func (c *substrateBootstrapRecoveryControl) GetActor(context.Context, string) (*substrateActor, error) {
	if c.unavailable {
		c.unavailable = false
		return nil, errors.New("Actor observation temporarily unavailable")
	}
	actor := c.actor
	return &actor, nil
}

func TestNativeSubstrateWorkspaceBootstrapRetriesMintedCredential(t *testing.T) {
	for _, failure := range []string{"lost response", "failed observation", "closed executor", "restarted executor", "restarted after observation", "replaced during recovery", "replaced Actor", "replaced Pod", "changed during challenge"} {
		t.Run(failure, func(t *testing.T) {
			const secret = "fixture-signing-secret"
			key, err := harnessv2.WorkspaceBootstrapPublicKey(secret)
			require.NoError(t, err)
			identity := harnessv2.SubstrateActorIdentity{Atespace: "ate-demo", Name: "direct", UID: "first-actor-uid"}
			receiver, err := harnessv2.NewCredentialBootstrapReceiver(harnessv2.WorkspaceBootstrapNonce(key), identity)
			require.NoError(t, err)
			control := &substrateBootstrapRecoveryControl{actor: substrateActor{
				Atespace: identity.Atespace, ActorID: identity.Name, ActorUID: identity.UID,
				PodUID: "first-pod-uid", Status: substrateStatusRunning,
			}}
			mint := &recordingSubstrateSessionIdentityClient{jwt: "first-minted-credential"}
			installed, puts := "", 0
			transport := substrateBootstrapRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				var body []byte
				statusCode := http.StatusOK
				if request.Method == http.MethodGet {
					if failure == "changed during challenge" {
						control.actor.PodUID = "replacement-pod-uid"
					}
					body, err = json.Marshal(receiver.Challenge)
					require.NoError(t, err)
				} else {
					sealed, err := io.ReadAll(request.Body)
					require.NoError(t, err)
					require.NoError(t, harnessv2.VerifyCredentialBootstrap(key, receiver.Challenge.Nonce, sealed,
						request.Header.Get(harnessv2.CredentialBootstrapSignatureHeader)))
					var envelope harnessv2.SealedCredentialBootstrap
					require.NoError(t, json.Unmarshal(sealed, &envelope))
					plain, err := receiver.Open(envelope)
					require.NoError(t, err)
					var payload harnessv2.WorkspaceBootstrapRequest
					require.NoError(t, json.Unmarshal(plain, &payload))
					if request.Method == http.MethodPost {
						require.True(t, payload.Recover)
						plain, err := json.Marshal(harnessv2.WorkspaceBootstrapRequest{HandoffToken: installed})
						require.NoError(t, err)
						body, err = receiver.SealResponse(sealed, plain)
						require.NoError(t, err)
						if installed != "" && failure == "replaced during recovery" {
							control.actor.PodUID = "replacement-pod-uid"
						}
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
					}
					puts++
					statusCode = http.StatusNoContent
					if installed != "" && installed != payload.HandoffToken {
						statusCode = http.StatusConflict
					} else {
						installed = payload.HandoffToken
					}
					if puts == 1 {
						mint.jwt = "second-minted-credential"
						if failure == "failed observation" || failure == "restarted after observation" {
							control.unavailable = true
						} else {
							return nil, errors.New("bootstrap response was lost")
						}
					}
				}
				return &http.Response{StatusCode: statusCode, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})
			newExecutor := func() *SubstrateWorkspaceExecutor {
				executor, err := NewSubstrateExecutor(SubstrateConfig{
					RouterURL: "http://router.test", ActorDNSSuffix: "actors.test", BootstrapToken: secret,
					SealedBootstrap: true, ControlClient: control, HTTPClient: &http.Client{Transport: transport},
					SessionIdentityRequired: true, SessionIdentityToken: "fixture-session-identity", SessionIdentityClient: mint,
				})
				require.NoError(t, err)
				return executor
			}
			executor := newExecutor()
			request := UploadRequest{Ref: WorkspaceRef{ID: "direct.ate-demo"}, BootstrapHandoff: true,
				Artifacts: []UploadArtifact{{Path: substrateHandoffTokenUploadPath, Data: []byte("fixture-handoff")}}}
			_, err = executor.Upload(t.Context(), request)
			require.Error(t, err)
			if failure == "changed during challenge" {
				require.Zero(t, puts, "changed placement must fail before sending bootstrap credentials")
				return
			}
			require.Equal(t, "first-minted-credential", installed)
			if failure == "closed executor" || failure == "restarted executor" || failure == "restarted after observation" || failure == "replaced during recovery" {
				if failure == "closed executor" {
					require.NoError(t, executor.Close())
				}
				executor = newExecutor()
			}
			if failure == "replaced Actor" || failure == "replaced Pod" {
				control.actor.PodUID = "second-pod-uid"
				if failure == "replaced Actor" {
					control.actor.ActorUID = "second-actor-uid"
				}
				identity.UID = control.actor.ActorUID
				receiver, err = harnessv2.NewCredentialBootstrapReceiver(harnessv2.WorkspaceBootstrapNonce(key), identity)
				require.NoError(t, err)
				installed = ""
			}
			_, err = executor.Upload(t.Context(), request)
			if failure == "replaced during recovery" {
				require.Error(t, err, "a changed Pod must not accept a recovered credential")
				require.Equal(t, 1, puts, "recovery must stop before another credential delivery")
				require.Equal(t, 1, mint.calls)
				return
			}
			require.NoError(t, err, "retry must confirm the same installed credential")
			_, err = executor.Upload(t.Context(), request)
			require.NoError(t, err, "confirmed bootstrap remains idempotent")
			if failure == "replaced Actor" || failure == "replaced Pod" {
				require.Equal(t, 2, mint.calls)
				require.Equal(t, "second-minted-credential", installed)
			} else {
				require.Equal(t, 1, mint.calls)
				require.Equal(t, "first-minted-credential", installed)
			}
		})
	}
}

func TestNativeSubstrateWorkspaceSealedBootstrap(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact actor", true: "replaced actor"}[mismatch], func(t *testing.T) {
			const secret = "fixture-signing-secret"
			key, err := harnessv2.WorkspaceBootstrapPublicKey(secret)
			require.NoError(t, err)
			identity := harnessv2.SubstrateActorIdentity{Atespace: "ate-demo", Name: "direct", UID: "test-actor-uid"}
			if mismatch {
				identity.UID = "replacement-actor-uid"
			}
			receiver, err := harnessv2.NewCredentialBootstrapReceiver(harnessv2.WorkspaceBootstrapNonce(key), identity)
			require.NoError(t, err)
			puts := 0
			router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "direct.ate-demo.actors.test", r.Host)
				require.Equal(t, harnessv2.CredentialBootstrapPath, r.URL.Path)
				require.Empty(t, r.Header.Get("Authorization"))
				if r.Method == http.MethodGet {
					require.NoError(t, json.NewEncoder(w).Encode(receiver.Challenge))
					return
				}
				puts++
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NotContains(t, string(body), secret)
				require.NotContains(t, string(body), "fixture-handoff")
				require.NoError(t, harnessv2.VerifyCredentialBootstrap(key, receiver.Challenge.Nonce, body, r.Header.Get(harnessv2.CredentialBootstrapSignatureHeader)))
				var envelope harnessv2.SealedCredentialBootstrap
				require.NoError(t, json.Unmarshal(body, &envelope))
				plain, err := receiver.Open(envelope)
				require.NoError(t, err)
				var request harnessv2.WorkspaceBootstrapRequest
				require.NoError(t, json.Unmarshal(plain, &request))
				require.Equal(t, "fixture-handoff", request.HandoffToken)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer router.Close()
			executor, err := NewSubstrateExecutor(SubstrateConfig{RouterURL: router.URL, ActorDNSSuffix: "actors.test", BootstrapToken: secret, SealedBootstrap: true,
				ControlClient: &recordingSubstrateControlClient{getStatuses: []string{substrateStatusRunning, substrateStatusRunning, substrateStatusRunning}}})
			require.NoError(t, err)
			_, err = executor.Upload(t.Context(), UploadRequest{Ref: WorkspaceRef{ID: "direct.ate-demo"}, BootstrapHandoff: true, Timeout: time.Second,
				Artifacts: []UploadArtifact{{Path: substrateHandoffTokenUploadPath, Data: []byte("fixture-handoff")}}})
			if mismatch {
				require.Error(t, err)
				require.Zero(t, puts, "a replacement Actor must not receive a credential")
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, puts)
			}
		})
	}
}

func TestNativeSubstrateWorkspaceRefusesPlaintextFallback(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.URL.Path, "/v2/"))
		w.WriteHeader(http.StatusNotFound)
	}))
	defer router.Close()
	executor, err := NewSubstrateExecutor(SubstrateConfig{RouterURL: router.URL, ActorDNSSuffix: "actors.test", BootstrapToken: "fixture-secret", SealedBootstrap: true,
		ControlClient: &recordingSubstrateControlClient{getStatuses: []string{substrateStatusRunning}}})
	require.NoError(t, err)
	_, err = executor.Upload(t.Context(), UploadRequest{Ref: WorkspaceRef{ID: "direct.ate-demo"}, BootstrapHandoff: true, Timeout: time.Second,
		Artifacts: []UploadArtifact{{Path: substrateHandoffTokenUploadPath, Data: []byte("fixture-handoff")}}})
	require.Error(t, err)
}

func TestNativeSubstrateWorkspaceBootstrapRefusesRedirect(t *testing.T) {
	followed := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer destination.Close()
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer router.Close()
	executor, err := NewSubstrateExecutor(SubstrateConfig{
		RouterURL: router.URL, ActorDNSSuffix: "actors.test", BootstrapToken: "fixture-secret", SealedBootstrap: true,
		ControlClient: &recordingSubstrateControlClient{getStatuses: []string{substrateStatusRunning}},
	})
	require.NoError(t, err)
	_, err = executor.Upload(t.Context(), UploadRequest{
		Ref: WorkspaceRef{ID: "direct.ate-demo"}, BootstrapHandoff: true, Timeout: time.Second,
		Artifacts: []UploadArtifact{{Path: substrateHandoffTokenUploadPath, Data: []byte("fixture-handoff")}},
	})
	require.Error(t, err)
	require.False(t, followed, "the provider route must remain the bootstrap destination")
}
