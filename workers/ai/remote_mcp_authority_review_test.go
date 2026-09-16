package main

import (
	"context"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tools"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type remoteDependencyReadRecorder struct {
	client.Client
	secrets      []client.ObjectKey
	policyReads  int
	changePolicy func(int, *corev1alpha1.OutboundAccessPolicy)
}

func (r *remoteDependencyReadRecorder) Get(
	ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption,
) error {
	if _, secret := object.(*corev1.Secret); secret {
		r.secrets = append(r.secrets, key)
	}
	if err := r.Client.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if policy, ok := object.(*corev1alpha1.OutboundAccessPolicy); ok {
		r.policyReads++
		if r.changePolicy != nil {
			r.changePolicy(r.policyReads, policy)
		}
	}
	return nil
}

func remoteReviewCA(t *testing.T, f *nativeRemoteFixture, namespace, name string) {
	t.Helper()
	policy := &corev1alpha1.OutboundAccessPolicy{}
	if err := f.client.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "egress"}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.Gateway.TLS = &corev1alpha1.OutboundTLSConfig{
		CASecretRef: &corev1alpha1.NamespacedSecretKeySelector{Namespace: namespace, Name: name, Key: "ca.crt"},
	}
	policy.Generation++
	if err := f.client.Update(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: "foreign-ca"},
		Data:       map[string][]byte{"ca.crt": []byte("test-ca")},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRemoteMCPForeignCANamespaceRejectedBeforeSecretReads(t *testing.T) {
	f := newNativeRemoteFixture(t)
	remoteReviewCA(t, f, "other-team", "auth")
	recorder := &remoteDependencyReadRecorder{Client: f.client}
	f.client = recorder
	if _, _, err := f.prepare(t); err == nil {
		t.Error("remote preparation accepted foreign CA namespace")
	}
	if len(recorder.secrets) != 0 {
		t.Fatalf("read Secrets before validating full credential authority: %v", recorder.secrets)
	}
	if f.requests.Load() != 0 {
		t.Fatal("invalid CA authority reached discovery")
	}
}

func TestNativeRemoteMCPApprovalRefreshRevalidatesBeforeSecretReads(t *testing.T) {
	for _, foreignNamespace := range []bool{false, true} {
		name := "foreign name"
		if foreignNamespace {
			name = "foreign namespace"
		}
		t.Run(name, func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			ctx, loaded, err := f.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			namespace, secretName := "team", "other-auth"
			if foreignNamespace {
				namespace, secretName = "other-team", "auth"
			}
			remoteReviewCA(t, f, namespace, secretName)
			recorder := &remoteDependencyReadRecorder{Client: f.client}
			toolContext := prepareApprovalToolContext(&tools.ToolContext{
				Client: recorder, Namespace: "team", TaskID: "task", TaskUID: "task-uid",
			}, nil)
			if err := toolContext.ApprovalTargetRefresh(ctx, "health", loaded["health"]); err == nil {
				t.Error("approval refresh accepted changed remote policy")
			}
			if len(recorder.secrets) != 0 {
				t.Fatalf("approval refresh read Secrets before remote validation: %v", recorder.secrets)
			}
			if f.calls.Load() != 0 {
				t.Fatal("approval refresh executed remote tool")
			}
		})
	}
}

func TestNativeRemoteMCPBindsOnlyAuthorizedPolicySnapshot(t *testing.T) {
	f := newNativeRemoteFixture(t)
	recorder := &remoteDependencyReadRecorder{Client: f.client}
	recorder.changePolicy = func(read int, policy *corev1alpha1.OutboundAccessPolicy) {
		if read < 2 {
			return
		}
		policy.ResourceVersion = "changed-policy"
		policy.Spec.Gateway.TLS = &corev1alpha1.OutboundTLSConfig{
			CASecretRef: &corev1alpha1.NamespacedSecretKeySelector{Namespace: "other-team", Name: "auth", Key: "ca.crt"},
		}
	}
	f.client = recorder
	if _, _, err := f.prepare(t); err == nil {
		t.Fatal("policy mutation during binding was accepted")
	}
	for _, ref := range recorder.secrets {
		if ref.Namespace != "team" || ref.Name != "auth" {
			t.Fatalf("policy reread introduced an unauthorized credential read: %v", ref)
		}
	}
}
