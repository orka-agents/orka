package v1alpha1

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	omsprotocol "github.com/orka-agents/orka/pkg/oms/protocol"
)

func TestMemoryBackendRequestedLifecycleDefaultsToStaged(t *testing.T) {
	if got := (MemoryBackendSpec{}).RequestedLifecycle(); got != MemoryBackendLifecycleStaged {
		t.Fatalf("RequestedLifecycle() = %q, want %q", got, MemoryBackendLifecycleStaged)
	}
	if got := (MemoryBackendSpec{LifecycleState: MemoryBackendLifecycleActive}).RequestedLifecycle(); got != MemoryBackendLifecycleActive {
		t.Fatalf("RequestedLifecycle() = %q, want %q", got, MemoryBackendLifecycleActive)
	}
}

func TestMemoryBackendDeepCopySeparatesStatusSlicesAndTimes(t *testing.T) {
	now := metav1.NewTime(time.Unix(100, 0).UTC())
	backend := &MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{Name: MemoryBackendDefaultName, Namespace: "example"},
		Status: MemoryBackendStatus{
			ObservedCapabilities: &MemoryBackendObservedCapabilities{
				Revision: "revision-1", AdapterName: "reference", AdapterVersion: "v1.2.3",
				Native:    []MemoryBackendCapability{MemoryBackendCapabilityKeywordSearch},
				Effective: []MemoryBackendCapability{MemoryBackendCapabilityKeywordSearch},
				ExpiresAt: now,
				Limits:    MemoryBackendCapabilityLimits{MaxPageSize: 100},
			},
			LastValidated: &now,
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", Message: "ready",
			}},
		},
	}

	copy := backend.DeepCopy()
	copy.Status.ObservedCapabilities.Native[0] = MemoryBackendCapabilitySemanticSearch
	copy.Status.ObservedCapabilities.Effective[0] = MemoryBackendCapabilityHybridSearch
	copy.Status.ObservedCapabilities.ExpiresAt.Time = time.Unix(200, 0).UTC()
	copy.Status.LastValidated.Time = time.Unix(300, 0).UTC()
	copy.Status.Conditions[0].Message = "changed"

	if backend.Status.ObservedCapabilities.Native[0] != MemoryBackendCapabilityKeywordSearch {
		t.Fatal("DeepCopy shared native capability storage")
	}
	if backend.Status.ObservedCapabilities.Effective[0] != MemoryBackendCapabilityKeywordSearch {
		t.Fatal("DeepCopy shared effective capability storage")
	}
	if copy.Status.ObservedCapabilities.AdapterName != "reference" || copy.Status.ObservedCapabilities.AdapterVersion != "v1.2.3" {
		t.Fatalf("DeepCopy lost adapter identity: %#v", copy.Status.ObservedCapabilities)
	}
	if backend.Status.ObservedCapabilities.ExpiresAt.Time.Equal(copy.Status.ObservedCapabilities.ExpiresAt.Time) {
		t.Fatal("DeepCopy shared capability expiry storage")
	}
	if backend.Status.LastValidated.Time.Equal(copy.Status.LastValidated.Time) {
		t.Fatal("DeepCopy shared validation timestamp storage")
	}
	if backend.Status.Conditions[0].Message != "ready" {
		t.Fatal("DeepCopy shared condition storage")
	}
}

func TestMemoryBackendSchemaMarkersRemainPresent(t *testing.T) {
	source, err := os.ReadFile("memory_backend_types.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, marker := range []string{
		`+kubebuilder:metadata:annotations=memory.orka.ai/schema-version=v1alpha1`,
		`self.metadata.name == 'default'`,
		`self.protocol == oldSelf.protocol`,
		`self.store.name == oldSelf.store.name`,
		`+kubebuilder:subresource:status`,
		`+kubebuilder:default:=Staged`,
		`+kubebuilder:validation:Maximum=` + strconv.Itoa(omsprotocol.MaxHTTPBodyBytes),
	} {
		if !strings.Contains(text, marker) {
			t.Errorf("memory_backend_types.go is missing schema marker %q", marker)
		}
	}
}

func TestMemoryBackendRequiredCapabilityConstantsAreUnique(t *testing.T) {
	capabilities := []MemoryBackendCapability{
		MemoryBackendCapabilityDurableIdempotency,
		MemoryBackendCapabilityIdempotencyDigestConflicts,
		MemoryBackendCapabilityCreateIfAbsent,
		MemoryBackendCapabilityConditionalMutation,
		MemoryBackendCapabilityMonotonicGenerations,
		MemoryBackendCapabilityDeleteHighWatermark,
		MemoryBackendCapabilityDurableRoutingFence,
		MemoryBackendCapabilityOperationLookup,
		MemoryBackendCapabilityExactGet,
		MemoryBackendCapabilityStablePagination,
		MemoryBackendCapabilityExclusiveOwnership,
		MemoryBackendCapabilityKeywordSearch,
		MemoryBackendCapabilityAuditVersionVisibility,
	}
	seen := make(map[MemoryBackendCapability]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" {
			t.Fatal("required capability constant is empty")
		}
		if _, exists := seen[capability]; exists {
			t.Fatalf("duplicate required capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
}
