package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// A GitHubApp reference freezes the App Secret as a token reference freezes
// its Secret: the binding names the private-key entry, and a PEM is accepted.
func TestPromptCredentialBindingsFreezeGitHubAppSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publisher-app", UID: "app-secret-uid"},
		Data:       map[string][]byte{"appId": []byte("1"), "privateKey": []byte("-----BEGIN RSA PRIVATE KEY-----\nMII\n-----END RSA PRIVATE KEY-----\n")},
	}).Build()
	ref := &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey", Type: corev1alpha1.WorkspaceCredentialGitHubApp}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: corev1alpha1.TaskSpec{
		Workspace: &corev1alpha1.WorkspaceConfig{ReadCredentialRef: ref, PublicationCredentialRef: ref},
	}}
	bindings, versions, err := resolvePromptCredentialBindings(context.Background(), reader, task)
	if err != nil || len(bindings) != 2 || versions.SourceRead == "" || versions.SourceRead != versions.TargetWrite ||
		bindings[0].SecretKey != "privateKey" || bindings[0].SecretUID != "app-secret-uid" {
		t.Fatalf("bindings = %#v, versions = %#v, err = %v", bindings, versions, err)
	}
	dispatcher := &ACPDispatcher{Client: reader}
	if version, err := dispatcher.workspaceCredentialResourceVersion(context.Background(), task, ref); err != nil || version != versions.SourceRead {
		t.Fatalf("frozen version = %q, err = %v", version, err)
	}
	// Leaving key at its default misses the private key, so reservation fails.
	if _, err := dispatcher.workspaceCredentialResourceVersion(context.Background(), task,
		&corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Type: corev1alpha1.WorkspaceCredentialGitHubApp}); err == nil {
		t.Fatal("default key on an App Secret: want error")
	}
}
