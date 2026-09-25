package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/stretchr/testify/require"
)

type nativeBootstrapRoundTripFunc func(*http.Request) (*http.Response, error)

func (f nativeBootstrapRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestNativeSubstrateBootstrapChecksPlacementAfterChallenge(t *testing.T) {
	for _, change := range []string{"none", "assignment", "assignment ABA", "worker UID", "process after validation"} {
		t.Run(change, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record != nil && record.Attempt != nil && record.Attempt.Worker != nil
			})
			record := h.record(t)
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			cfg, err := h.r.runtimePoolConfigForDrain(&pool)
			require.NoError(t, err)
			auth, _, err := h.r.ensureRuntimePoolSecrets(t.Context(), &pool, cfg)
			require.NoError(t, err)
			nonce := string(auth.Data[runtimePoolBootstrapNonceKey])
			identity := harnessv2.SubstrateActorIdentity{Atespace: record.Atespace, Name: record.Attempt.Name, UID: record.Attempt.UID}
			receiver, err := harnessv2.NewCredentialBootstrapReceiver(nonce, identity)
			require.NoError(t, err)
			puts, opened := 0, 0
			h.r.SubstrateCredentialSeeder = nil
			h.r.substrateSupervisorOnce.Do(func() {
				h.r.substrateSupervisorHTTP = &http.Client{Transport: nativeBootstrapRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					statusCode := http.StatusOK
					var body []byte
					if request.Method == http.MethodGet {
						actor := h.api.actors[record.Attempt.Name]
						switch change {
						case "assignment":
							actor.Metadata.Version++
							actor.Status.WorkerAssignment.WorkerPodUid = "replacement-pod-uid"
						case "assignment ABA":
							actor.Metadata.Version += 2
						case "worker UID":
							h.api.workers[record.Attempt.Worker.Name].Metadata.Uid = "replacement-worker-uid"
						}
						body, err = json.Marshal(receiver.Challenge)
						require.NoError(t, err)
					} else {
						puts++
						var envelope harnessv2.SealedCredentialBootstrap
						require.NoError(t, json.NewDecoder(request.Body).Decode(&envelope))
						if change == "process after validation" {
							receiver, err = harnessv2.NewCredentialBootstrapReceiver(nonce, identity)
							require.NoError(t, err)
						}
						if _, err := receiver.Open(envelope); err != nil {
							statusCode = http.StatusForbidden
						} else {
							opened++
							statusCode = http.StatusCreated
						}
					}
					return &http.Response{StatusCode: statusCode, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
				})}
			})
			h.step(t)
			pool = runtimePoolTestGetPool(t, h.r, h.pool)
			if change == "none" {
				require.Equal(t, 1, opened)
				require.True(t, nativeTestServing(&pool, h.record(t)))
				return
			}
			require.Zero(t, opened, "an unvalidated replacement must not receive bootstrap plaintext")
			require.Equal(t, corev1alpha1.RuntimePoolAdmissionClosed, pool.Status.AdmissionState)
			if change != "process after validation" {
				require.Zero(t, puts, "changed placement must fail before credential delivery")
				require.Empty(t, h.record(t).Attempt.BootstrapChallenge, "an unvalidated challenge must not become the durable process binding")
			}
		})
	}
}
