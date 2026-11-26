package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// A GitHubApp reference freezes the App Secret as a token reference freezes
// its Secret: the binding names the private-key entry, and a PEM is accepted.
func TestPromptCredentialBindingsFreezeGitHubAppSecret(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	validPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publisher-app", UID: "app-secret-uid"},
			Type:       corev1.SecretType(corev1alpha1.GitHubAppSecretType),
			Data:       map[string][]byte{"appId": []byte("1"), "privateKey": validPEM},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "untyped-app", UID: "untyped-secret-uid"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"appId": []byte("1"), "privateKey": validPEM},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bad-key-app", UID: "bad-key-uid"},
			Type:       corev1.SecretType(corev1alpha1.GitHubAppSecretType),
			Data:       map[string][]byte{"appId": []byte("1"), "privateKey": []byte("not-a-pem")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "missing-id-app", UID: "missing-id-uid"},
			Type:       corev1.SecretType(corev1alpha1.GitHubAppSecretType),
			Data:       map[string][]byte{"privateKey": validPEM},
		},
	).Build()
	ref := &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey", Type: corev1alpha1.WorkspaceCredentialGitHubApp}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: corev1alpha1.TaskSpec{
		Workspace: &corev1alpha1.WorkspaceConfig{ReadCredentialRef: ref, PublicationCredentialRef: ref},
	}}
	bindings, versions, err := resolvePromptCredentialBindings(context.Background(), reader, task)
	if err != nil || len(bindings) != 2 || versions.SourceRead == "" || versions.SourceRead != versions.TargetWrite {
		t.Fatalf("bindings = %#v, versions = %#v, err = %v", bindings, versions, err)
	}
	for i, role := range []store.PromptCredentialRole{store.PromptCredentialSourceRead, store.PromptCredentialTargetWrite} {
		want := store.PromptCredentialBinding{Role: role, Namespace: "default", SecretName: "publisher-app", SecretKey: "privateKey",
			SecretUID: "app-secret-uid", ResourceVersion: versions.SourceRead}
		if bindings[i] != want {
			t.Fatalf("binding %d = %#v, want %#v", i, bindings[i], want)
		}
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
	// An untyped Secret is refused for a GitHubApp reference before freezing.
	untypedTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: corev1alpha1.TaskSpec{
		Workspace: &corev1alpha1.WorkspaceConfig{
			ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "untyped-app", Key: "privateKey", Type: corev1alpha1.WorkspaceCredentialGitHubApp},
		},
	}}
	if _, _, err := resolvePromptCredentialBindings(context.Background(), reader, untypedTask); err == nil {
		t.Fatal("untyped Secret for GitHubApp ref: want error")
	}
	if _, err := dispatcher.workspaceCredentialResourceVersion(context.Background(), untypedTask, untypedTask.Spec.Workspace.ReadCredentialRef); err == nil {
		t.Fatal("untyped Secret workspaceCredentialResourceVersion: want error")
	}
	// An orka.ai/github-app Secret is refused for a plain Token reference before freezing.
	rawRefTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: corev1alpha1.TaskSpec{
		Workspace: &corev1alpha1.WorkspaceConfig{
			ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "publisher-app", Key: "privateKey"},
		},
	}}
	if _, _, err := resolvePromptCredentialBindings(context.Background(), reader, rawRefTask); err == nil {
		t.Fatal("GitHubApp Secret for untyped ref: want error")
	}
	// A malformed private key or missing appId is rejected before freezing.
	for _, secretName := range []string{"bad-key-app", "missing-id-app"} {
		badTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: corev1alpha1.TaskSpec{
			Workspace: &corev1alpha1.WorkspaceConfig{
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: secretName, Key: "privateKey", Type: corev1alpha1.WorkspaceCredentialGitHubApp},
			},
		}}
		if _, _, err := resolvePromptCredentialBindings(context.Background(), reader, badTask); err == nil {
			t.Fatalf("%s: want reservation error", secretName)
		}
	}
}
