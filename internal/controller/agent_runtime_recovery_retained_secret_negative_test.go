package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type retainedBootSecretReader struct {
	client.Reader
	key    client.ObjectKey
	change func(*corev1.Secret)
}

func (r retainedBootSecretReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	if key == r.key {
		if secret, ok := object.(*corev1.Secret); ok {
			r.change(secret)
		}
	}
	return nil
}

func changeRetainedBootSnapshot(t *testing.T, secret *corev1.Secret, change func(*agentRuntimeDeletionSnapshot)) {
	t.Helper()
	var snapshot agentRuntimeDeletionSnapshot
	if err := json.Unmarshal(secret.Data[agentRuntimeCleanupSecretAuthorityKey], &snapshot); err != nil {
		t.Fatal(err)
	}
	change(&snapshot)
	data, err := harnessv2.CanonicalValue(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	secret.Data[agentRuntimeCleanupSecretAuthorityKey] = data
}

func TestAgentRuntimeRecoveryRetainedBootSecretRejectsLostAuthority(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *corev1.Secret)
	}{
		{"missing-finalizer", func(_ *testing.T, s *corev1.Secret) { s.Finalizers = nil }},
		{"mutable", func(_ *testing.T, s *corev1.Secret) { s.Immutable = new(false) }},
		{"wrong-owner", func(_ *testing.T, s *corev1.Secret) { s.OwnerReferences[0].UID = "different-runtime-uid" }},
		{"wrong-type", func(_ *testing.T, s *corev1.Secret) { s.Type = corev1.SecretTypeOpaque }},
		{"malformed-authority", func(_ *testing.T, s *corev1.Secret) { s.Data[agentRuntimeCleanupSecretAuthorityKey] = []byte(`{}`) }},
		{"runtime-generation", func(t *testing.T, s *corev1.Secret) {
			changeRetainedBootSnapshot(t, s, func(v *agentRuntimeDeletionSnapshot) { v.Generation++ })
		}},
		{"controller-auth-uid", func(t *testing.T, s *corev1.Secret) {
			changeRetainedBootSnapshot(t, s, func(v *agentRuntimeDeletionSnapshot) { v.ControllerAuthSecretUID = "different-auth-uid" })
		}},
		{"capability-auth-version", func(t *testing.T, s *corev1.Secret) {
			changeRetainedBootSnapshot(t, s, func(v *agentRuntimeDeletionSnapshot) { v.CapabilityAuthResourceVersion = "different-version" })
		}},
		{"boot-id", func(t *testing.T, s *corev1.Secret) {
			changeRetainedBootSnapshot(t, s, func(v *agentRuntimeDeletionSnapshot) { v.ObservedCapabilities.SupervisorBootID = "different-boot" })
		}},
		{"invalid-auth-material", func(_ *testing.T, s *corev1.Secret) { s.Data[agentRuntimeCleanupSecretControllerAuthKey] = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			f.reconcile(t)
			if !f.runtime.Status.Ready {
				t.Fatal("fixture did not conform")
			}
			witness := f.witness(t)
			key := client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}
			secret := &corev1.Secret{}
			if err := f.r.Get(t.Context(), key, secret); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Delete(t.Context(), secret); err != nil {
				t.Fatal(err)
			}
			f.r.APIReader = retainedBootSecretReader{Reader: f.r.Client, key: key, change: func(s *corev1.Secret) {
				if s.DeletionTimestamp == nil {
					t.Fatal("fixture authority was not deletion-pending")
				}
				tc.change(t, s)
			}}
			before := f.server.Counts()
			if _, err := f.r.recoveryBootClient(t.Context(), witness, f.fence); err == nil {
				t.Fatal("changed retained authority created an old-boot cleanup client")
			}
			if f.server.Counts() != before {
				t.Fatal("rejected authority reached runtime mutations")
			}
		})
	}
}

func TestAgentRuntimeRecoveryRetainedDeletingBootClientRemainsDrainOnly(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	f.reconcile(t)
	witness := f.witness(t)
	key := client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}
	secret := &corev1.Secret{}
	if err := f.r.Get(t.Context(), key, secret); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	c, err := f.r.recoveryBootClient(t.Context(), witness, f.fence)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{}
	task.UID = "retained-secret-new-task"
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "retained-secret-new-prompt"}
	_, workspace, err := emptyRuntimeWorkspace(task, "")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildAgentRuntimeMCPConfigurationWithRegistry(t.Context(), f.r.Client, f.runtime, f.config.Profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessionFence := witness.Fence
	sessionFence.RuntimeSessionUID = "retained-secret-new-session"
	sessionFence.RuntimeSessionGeneration = 1
	metadata := mutationMetadata(sessionFence, task, "create", false, time.Now().UTC().Add(30*time.Second))
	create := harnessv2.CreateRuntimeSessionRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: metadata, RuntimeSessionID: "retained-secret-new-session",
		Profile: f.config.Profile, MCPConfiguration: configuration, Workspace: workspace,
	}
	if err := sealMutation(&create.Metadata.RequestDigest, create); err != nil {
		t.Fatal(err)
	}
	if err := create.ValidateAt(time.Now().UTC()); err != nil {
		t.Fatalf("non-drain fixture is invalid: %v", err)
	}
	before := f.server.Counts()
	_, err = c.CreateRuntimeSession(t.Context(), create)
	if err == nil || !strings.Contains(err.Error(), "old-boot recovery only authorizes drain") {
		t.Fatalf("retained old-boot authority did not reject at the drain-only guard: %v", err)
	}
	if f.server.Counts() != before {
		t.Fatal("non-drain request reached runtime mutations")
	}
	// Runtime identity validation still precedes any authorized old-boot drain.
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	f.pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
	f.pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.Now()
	if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	request, err := newAgentRuntimeDeletionDrainRequest(witness.Fence, metav1.Now().Time)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Drain(t.Context(), request); err == nil {
		t.Fatal("changed original process still accepted drain")
	}
	if f.server.Counts() != before {
		t.Fatal("changed process reached runtime mutations")
	}
}
