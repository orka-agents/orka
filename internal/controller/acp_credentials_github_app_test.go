package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// A GitHubApp reference freezes the App Secret exactly as a token reference
// freezes its Secret: the binding names the private-key entry and the Secret
// version, and a multi-line PEM value is accepted.
func TestPromptCredentialBindingsFreezeGitHubAppSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publisher-app", UID: "app-secret-uid"},
		Data: map[string][]byte{
			"appId":      []byte("123456"),
			"privateKey": []byte("-----BEGIN RSA PRIVATE KEY-----\nMIIfixture\n-----END RSA PRIVATE KEY-----\n"),
		},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	appRef := &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey", Type: corev1alpha1.WorkspaceCredentialGitHubApp}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "app-task"},
		Spec: corev1alpha1.TaskSpec{Type: "agent", Workspace: &corev1alpha1.WorkspaceConfig{
			ReadCredentialRef: appRef, PublicationCredentialRef: appRef,
		}},
	}
	bindings, versions, err := resolvePromptCredentialBindings(context.Background(), reader, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || versions.SourceRead == "" || versions.SourceRead != versions.TargetWrite {
		t.Fatalf("bindings = %#v, versions = %#v", bindings, versions)
	}
	for _, binding := range bindings {
		if binding.SecretName != "publisher-app" || binding.SecretKey != "privateKey" || binding.SecretUID != "app-secret-uid" ||
			(binding.Role != store.PromptCredentialSourceRead && binding.Role != store.PromptCredentialTargetWrite) {
			t.Fatalf("binding = %#v", binding)
		}
	}
	dispatcher := &ACPDispatcher{Client: reader}
	version, err := dispatcher.workspaceCredentialResourceVersion(context.Background(), task, appRef)
	if err != nil || version != versions.SourceRead {
		t.Fatalf("frozen version = %q, err = %v, want %q", version, err, versions.SourceRead)
	}
	// A reference that leaves key at its default does not find the App's
	// private key, so reservation fails instead of freezing the wrong entry.
	defaultKey := &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Type: corev1alpha1.WorkspaceCredentialGitHubApp}
	if _, err := dispatcher.workspaceCredentialResourceVersion(context.Background(), task, defaultKey); err == nil {
		t.Fatal("default key on an App Secret: want error")
	}
}
