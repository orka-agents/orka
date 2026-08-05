/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestRuntimePoolDefaultConstants(t *testing.T) {
	if DefaultRuntimePoolDesiredReplicas != 0 {
		t.Fatalf("DefaultRuntimePoolDesiredReplicas = %d, want 0", DefaultRuntimePoolDesiredReplicas)
	}
	if DefaultRuntimePoolMaxResidentSessions != 10 {
		t.Fatalf("DefaultRuntimePoolMaxResidentSessions = %d, want 10", DefaultRuntimePoolMaxResidentSessions)
	}
	if DefaultRuntimePoolMaxRunningPrompts != 4 {
		t.Fatalf("DefaultRuntimePoolMaxRunningPrompts = %d, want 4", DefaultRuntimePoolMaxRunningPrompts)
	}
	if DefaultRuntimePoolColdStartTimeoutSeconds != 120 {
		t.Fatalf("DefaultRuntimePoolColdStartTimeoutSeconds = %d, want 120", DefaultRuntimePoolColdStartTimeoutSeconds)
	}
}

func TestRuntimePoolLifecycleConstants(t *testing.T) {
	tests := []struct {
		got  RuntimePoolLifecycle
		want string
	}{
		{RuntimePoolLifecycleStopped, "Stopped"},
		{RuntimePoolLifecycleStarting, "Starting"},
		{RuntimePoolLifecycleServing, "Serving"},
		{RuntimePoolLifecycleDraining, "Draining"},
		{RuntimePoolLifecycleQuiescent, "Quiescent"},
		{RuntimePoolLifecycleStopping, "Stopping"},
		{RuntimePoolLifecycleDegraded, "Degraded"},
		{RuntimePoolLifecycleAmbiguous, "Ambiguous"},
	}
	for _, tt := range tests {
		if string(tt.got) != tt.want {
			t.Errorf("RuntimePoolLifecycle = %q, want %q", tt.got, tt.want)
		}
	}
}

func TestRuntimePoolFieldsRoundTrip(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	pool := RuntimePool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupVersion.String(),
			Kind:       "RuntimePool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "codex-default",
			Namespace: "tenant-a",
		},
		Spec: RuntimePoolSpec{
			TrustDomain: RuntimePoolTrustDomain{
				Namespace: "tenant-a",
				Identity:  "tenant-a/default",
			},
			RuntimeNamespace: "orka-runtimes",
			Runtime: RuntimePoolRuntimeSpec{
				Image: "docker.io/example/acp-runtime@" + digest,
				Profile: RuntimePoolProfileSpec{
					ProtocolVersion:     RuntimePoolProtocolHarnessV2,
					Digest:              digest,
					DigestSchemaVersion: "v1",
					ResourceClass:       "standard",
				},
			},
			DesiredReplicas: 1,
			Capacity: &RuntimePoolCapacitySpec{
				MaxResidentSessions: 10,
				MaxRunningPrompts:   4,
			},
			ColdStartTimeoutSeconds: 120,
		},
		Status: RuntimePoolStatus{
			ObservedGeneration: 3,
			ControllerEpoch:    9,
			DesiredReplicas:    1,
			CurrentReplicas:    1,
			Lifecycle:          RuntimePoolLifecycleServing,
			AdmissionState:     RuntimePoolAdmissionAccepting,
			ActiveInstance: &RuntimePoolActiveInstanceStatus{
				PodNamespace:               "orka-runtimes",
				PodName:                    "codex-default-0",
				PodAddress:                 "10.0.0.42",
				PodUID:                     "11111111-2222-3333-4444-555555555555",
				BootID:                     "boot-1",
				RuntimeInstanceID:          "runtime-instance-1",
				ControllerEpoch:            9,
				ProtocolVersion:            RuntimePoolProtocolHarnessV2,
				ProfileDigest:              digest,
				ProfileDigestSchemaVersion: "v1",
			},
			Capacity: RuntimePoolCapacityStatus{
				MaxResidentSessions: 10,
				MaxRunningPrompts:   4,
				ResidentSessions:    3,
				RunningPrompts:      2,
				QueuedTasks:         5,
				ReservedSessions:    1,
				ReservedPrompts:     1,
				Reservations: []RuntimePoolCapacityReservationStatus{{
					PoolUID: "pool-uid", TaskUID: "task-uid", Attempt: 1, ControllerEpoch: 9,
					RuntimeInstanceID: "runtime-instance-1", ResidentSlots: 1, PromptSlots: 1,
					ReservedAt: metav1.NewTime(time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)),
					ExpiresAt:  metav1.NewTime(time.Date(2026, 7, 25, 12, 2, 0, 0, time.UTC)),
				}},
			},
			Conditions: []metav1.Condition{{
				Type:   RuntimePoolConditionAdmissionReady,
				Status: "True",
				Reason: "Serving",
			}},
		},
	}

	encoded, err := json.Marshal(&pool)
	if err != nil {
		t.Fatalf("json.Marshal(RuntimePool): %v", err)
	}
	var decoded RuntimePool
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(RuntimePool): %v", err)
	}

	if decoded.Spec.TrustDomain.Identity != pool.Spec.TrustDomain.Identity {
		t.Errorf("trust domain identity = %q, want %q", decoded.Spec.TrustDomain.Identity, pool.Spec.TrustDomain.Identity)
	}
	if decoded.Status.ActiveInstance == nil {
		t.Fatal("active instance was lost during JSON round trip")
	}
	if decoded.Status.ActiveInstance.RuntimeInstanceID != "runtime-instance-1" {
		t.Errorf("runtime instance ID = %q, want runtime-instance-1", decoded.Status.ActiveInstance.RuntimeInstanceID)
	}
	if decoded.Status.Capacity.QueuedTasks != 5 {
		t.Errorf("queued tasks = %d, want 5", decoded.Status.Capacity.QueuedTasks)
	}
	if len(decoded.Status.Capacity.Reservations) != 1 || decoded.Status.Capacity.Reservations[0].TaskUID != "task-uid" || decoded.Status.Capacity.ReservedPrompts != 1 {
		t.Fatalf("capacity reservations = %#v", decoded.Status.Capacity)
	}
}

func TestRuntimePoolReservationDeepCopy(t *testing.T) {
	pool := &RuntimePool{Status: RuntimePoolStatus{Capacity: RuntimePoolCapacityStatus{
		Reservations: []RuntimePoolCapacityReservationStatus{{
			PoolUID: "pool-uid", TaskUID: "task-uid", Attempt: 1, ControllerEpoch: 1, RuntimeInstanceID: "instance",
			ResidentSlots: 1, PromptSlots: 1, ReservedAt: metav1.Now(), ExpiresAt: metav1.Now(),
		}},
	}}}
	copy := pool.DeepCopy()
	copy.Status.Capacity.Reservations[0].TaskUID = "changed"
	if pool.Status.Capacity.Reservations[0].TaskUID != "task-uid" {
		t.Fatal("RuntimePool DeepCopy aliased capacity reservations")
	}
}

func TestRuntimePoolRegisteredWithScheme(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	obj, err := scheme.New(GroupVersion.WithKind("RuntimePool"))
	if err != nil {
		t.Fatalf("scheme.New(RuntimePool): %v", err)
	}
	if _, ok := obj.(*RuntimePool); !ok {
		t.Fatalf("scheme.New(RuntimePool) returned %T", obj)
	}
}
