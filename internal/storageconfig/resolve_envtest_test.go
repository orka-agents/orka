package storageconfig

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestResolveAgainstAPIServer resolves adapter Secrets through an uncached
// client against a real API server, the way the controller does before its
// manager starts.
func TestResolveAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run the API server Secret resolution test")
	}
	environment := &envtest.Environment{}
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	direct, err := client.New(restConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	ctx := context.Background()
	for _, namespace := range []string{testNamespace, "other"} {
		if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Fatalf("create namespace %s: %v", namespace, err)
		}
	}
	for _, object := range adapterSecrets(t, "other") {
		if err := direct.Create(ctx, object); err != nil {
			t.Fatalf("create %s: %v", object.GetName(), err)
		}
	}
	cfg := loadAdapterConfig(t)

	_, err = cfg.Resolve(ctx, direct, testNamespace)
	requireReason(t, err, ReasonSecretNotFound)

	for _, object := range adapterSecrets(t, testNamespace) {
		if err := direct.Create(ctx, object); err != nil {
			t.Fatalf("create %s: %v", object.GetName(), err)
		}
	}
	resolved, err := cfg.Resolve(ctx, direct, testNamespace)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := resolved.Credentials["postgres"]; got == nil || string(got.BearerToken()) != testToken || len(got.CABundle()) == 0 {
		t.Fatal("credentials were not resolved from the installation namespace")
	}

	token := &corev1.Secret{}
	if err := direct.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "orka-storage-postgres-client"}, token); err != nil {
		t.Fatalf("get token Secret: %v", err)
	}
	token.Data = map[string][]byte{"other": []byte(testToken)}
	if err := direct.Update(ctx, token); err != nil {
		t.Fatalf("update token Secret: %v", err)
	}
	_, err = cfg.Resolve(ctx, direct, testNamespace)
	requireReason(t, err, ReasonSecretKeyMissing)
}
