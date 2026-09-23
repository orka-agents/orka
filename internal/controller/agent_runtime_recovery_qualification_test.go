package controller

import (
	"errors"
	"reflect"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestAgentRuntimeFoundryRecoveryUnqualifiedEnrollmentAndDrain(t *testing.T) {
	for _, lostBoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "live boot drains", true: "lost boot remains retained"}[lostBoot], func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			configureFoundryRuntimeRecoveryFixture(t, f)
			// An unqualified or legacy supervisor omits both the capability and
			// broker status identity. Use its original endpoint without overlays.
			f.slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, f.server.URL()))
			if err := f.r.Update(t.Context(), f.slice); err != nil {
				t.Fatal(err)
			}
			f.updateServiceTargetPort(t)
			f.reconcile(t)
			if !f.runtime.Status.Ready {
				t.Fatalf("unqualified Foundry runtime failed enrollment: %s", f.runtime.Status.Message)
			}
			witness := f.witness(t)
			if witness.FoundryBroker != nil {
				t.Fatal("unqualified boot acquired broker retirement authority")
			}
			before := f.server.Counts()
			if before.SessionCreates == 0 || before.PromptStarts == 0 {
				t.Fatal("unqualified runtime never completed lifecycle conformance")
			}
			if lostBoot {
				if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
					t.Fatal(err)
				}
				f.pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: recoveryTerminal(witness)}
				if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
					t.Fatal(err)
				}
			}
			f.advanceEpoch(t)
			f.reconcile(t)
			f.reconcile(t)
			retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness)
			if err != nil || retired == lostBoot || f.runtime.Status.Ready {
				t.Fatalf("unqualified boot retirement=%t lost=%t ready=%t err=%v", retired, lostBoot, f.runtime.Status.Ready, err)
			}
			_, _, epoch, err := f.r.recoveryDeployment(t.Context(), f.runtime)
			if err != nil || epoch != map[bool]uint64{false: 2, true: 1}[lostBoot] {
				t.Fatalf("unqualified recovery epoch=%d err=%v", epoch, err)
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil ||
				!controllerutil.ContainsFinalizer(f.pod, agentRuntimeRecoveryPodFinalizer) {
				t.Fatalf("live or unproven Foundry Pod lost retention: %v", err)
			}
			if f.server.Counts() != before {
				t.Fatal("unqualified epoch recovery replayed lifecycle work")
			}
		})
	}
}

func TestAgentRuntimeFoundryRecoveryQualificationGatesBrokerWitness(t *testing.T) {
	for _, tc := range []struct {
		name         string
		qualified    bool
		broker       *harnessv2.FoundryBrokerIdentity
		wrongProfile bool
		wantReady    bool
	}{
		{name: "qualified identity", qualified: true, broker: testFoundryRecoveryBrokerIdentity(), wantReady: true},
		{name: "qualified identity missing", qualified: true},
		{name: "qualified identity invalid", qualified: true, broker: &harnessv2.FoundryBrokerIdentity{Protocol: harnessv2.FoundryBrokerProtocol}},
		{name: "qualified capabilities profile mismatch", qualified: true, broker: testFoundryRecoveryBrokerIdentity(), wrongProfile: true},
		{name: "qualified broker configuration mismatch", qualified: true, broker: &harnessv2.FoundryBrokerIdentity{
			Protocol: harnessv2.FoundryBrokerProtocol, LedgerIdentityDigest: testControllerDigest("foundry-ledger"),
			AgentConfigurationDigest: testControllerDigest("another-agent"),
		}},
		{name: "unqualified identity does not grant recovery", broker: testFoundryRecoveryBrokerIdentity(), wantReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			configureFoundryRuntimeRecoveryFixture(t, f)
			statusProxy := newExternalRuntimeStatusProxy(t, f.server.URL(), func(status *harnessv2.StatusResponse) {
				status.FoundryBroker = tc.broker
			})
			capabilitiesProxy := newExternalRuntimeCapabilitiesProxy(t, statusProxy.URL, func(capabilities *harnessv2.CapabilitiesResponse) {
				capabilities.SupportsFoundryRecovery = tc.qualified
				if tc.wrongProfile {
					capabilities.RuntimeProfileDigest = harnessv2.ProfileDigest(testControllerDigest("another-qualified-profile"))
				}
			})
			f.slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, capabilitiesProxy.URL))
			if err := f.r.Update(t.Context(), f.slice); err != nil {
				t.Fatal(err)
			}
			f.updateServiceTargetPort(t)
			before := f.server.Counts()
			f.reconcile(t)
			if f.runtime.Status.Ready != tc.wantReady {
				t.Fatalf("qualification ready=%t want=%t message=%s", f.runtime.Status.Ready, tc.wantReady, f.runtime.Status.Message)
			}
			witness, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, f.server.Fence().SupervisorBootID)
			if !tc.wantReady {
				if !errors.Is(err, store.ErrNotFound) || f.server.Counts() != before {
					t.Fatalf("invalid qualified recovery published a witness or admitted work: witnessError=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.qualified && !reflect.DeepEqual(witness.FoundryBroker, tc.broker) || !tc.qualified && witness.FoundryBroker != nil {
				t.Fatal("boot witness did not preserve its exact recovery qualification")
			}
		})
	}
}
