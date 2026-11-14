package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspace/daemonprotocol"
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

func TestNativeSubstrateWorkspaceBootstrapRetriesIssuedCredential(t *testing.T) {
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
					RouterURL: "http://router.test", BootstrapToken: secret,
					SealedBootstrap: true, ControlClient: control, HTTPClient: &http.Client{Transport: transport},
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
			// The controller issues its own random per-Actor credential and never
			// installs the caller-supplied static handoff value.
			first := installed
			require.Regexp(t, "^[0-9a-f]{64}$", first)
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
				return
			}
			require.NoError(t, err, "retry must confirm the same installed credential")
			_, err = executor.Upload(t.Context(), request)
			require.NoError(t, err, "confirmed bootstrap remains idempotent")
			if failure == "replaced Actor" || failure == "replaced Pod" {
				require.Regexp(t, "^[0-9a-f]{64}$", installed)
				require.NotEqual(t, first, installed, "a new Actor or Pod lifetime receives a new credential")
			} else {
				require.Equal(t, first, installed)
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
				require.Equal(t, "ate-demo/direct", r.Header.Get(daemonprotocol.TargetActorHeader))
				require.Equal(t, harnessv2.CredentialBootstrapPath, r.URL.Path)
				require.Empty(t, r.Header.Get("Authorization"))
				if r.Method == http.MethodGet {
					require.NoError(t, json.NewEncoder(w).Encode(receiver.Challenge))
					return
				}
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
				if r.Method == http.MethodPost {
					// A fresh process has no installed credential to recover.
					require.True(t, request.Recover)
					reply, err := json.Marshal(harnessv2.WorkspaceBootstrapRequest{})
					require.NoError(t, err)
					sealed, err := receiver.SealResponse(body, reply)
					require.NoError(t, err)
					_, _ = w.Write(sealed)
					return
				}
				puts++
				// The controller delivers its own per-Actor credential, never the
				// static value supplied with the upload.
				require.Regexp(t, "^[0-9a-f]{64}$", request.HandoffToken)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer router.Close()
			executor, err := NewSubstrateExecutor(SubstrateConfig{RouterURL: router.URL, BootstrapToken: secret, SealedBootstrap: true,
				HTTPClient: router.Client(), ControlClient: &recordingSubstrateControlClient{getStatuses: slices.Repeat([]string{substrateStatusRunning}, 8)}})
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
	executor, err := NewSubstrateExecutor(SubstrateConfig{RouterURL: router.URL, BootstrapToken: "fixture-secret", SealedBootstrap: true,
		HTTPClient: router.Client(), ControlClient: &recordingSubstrateControlClient{getStatuses: []string{substrateStatusRunning}}})
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
		RouterURL: router.URL, BootstrapToken: "fixture-secret", SealedBootstrap: true, HTTPClient: router.Client(),
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
