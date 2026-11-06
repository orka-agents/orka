/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskwebhook

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testSecretNamespace = "orka-system"
	testSecretName      = "orka-task-webhook-signing"
)

func newFakeClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func newPEM(t *testing.T) []byte {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodePrivateKeyPEM(private)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func signingSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testSecretNamespace},
		Data:       data,
	}
}

func getSecret(t *testing.T, client ctrlclient.Client) (*corev1.Secret, error) {
	t.Helper()
	secret := &corev1.Secret{}
	err := client.Get(context.Background(), ctrlclient.ObjectKey{Namespace: testSecretNamespace, Name: testSecretName}, secret)
	return secret, err
}

func TestSecretKeySourceCreatesSecretAndMintsCurrentKey(t *testing.T) {
	t.Parallel()
	client := newFakeClient(t)
	source := &SecretKeySource{Reader: client, Writer: client, Namespace: testSecretNamespace, Name: testSecretName}

	published, err := source.PublishedKeys(context.Background())
	if err != nil || published.Current != nil {
		t.Fatalf("PublishedKeys() before mint = %#v, %v; want empty", published, err)
	}
	if _, err := getSecret(t, client); !apierrors.IsNotFound(err) {
		t.Fatalf("PublishedKeys() wrote the Secret: %v", err)
	}

	keys, err := source.SigningKeys(context.Background())
	if err != nil || keys.Current == nil {
		t.Fatalf("SigningKeys() = %#v, %v", keys, err)
	}
	secret, err := getSecret(t, client)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := ParsePrivateKeyPEM(secret.Data[SecretKeyCurrent])
	if err != nil || stored.KeyID != keys.Current.KeyID {
		t.Fatalf("stored key = %v (%v), want minted kid %s", stored, err, keys.Current.KeyID)
	}

	again, err := source.PublishedKeys(context.Background())
	if err != nil || again.Current == nil || again.Current.KeyID != keys.Current.KeyID {
		t.Fatalf("PublishedKeys() after mint = %#v, %v", again, err)
	}
}

func TestSecretKeySourceMintsIntoEmptyExistingSecret(t *testing.T) {
	t.Parallel()
	client := newFakeClient(t, signingSecret(nil))
	source := &SecretKeySource{Reader: client, Writer: client, Namespace: testSecretNamespace, Name: testSecretName}
	keys, err := source.SigningKeys(context.Background())
	if err != nil || keys.Current == nil {
		t.Fatalf("SigningKeys() = %#v, %v", keys, err)
	}
	secret, _ := getSecret(t, client)
	if len(secret.Data[SecretKeyCurrent]) == 0 {
		t.Fatal("minted key was not stored")
	}
}

func TestSecretKeySourceFailsClosedOnInvalidKeysWithoutOverwriting(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]map[string][]byte{
		"invalid current":           {SecretKeyCurrent: []byte("not a key")},
		"invalid previous":          {SecretKeyCurrent: newPEM(t), SecretKeyPrevious: []byte("bad")},
		"previous without current":  {SecretKeyPrevious: newPEM(t)},
		"non-ed25519 shaped string": {SecretKeyCurrent: []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := newFakeClient(t, signingSecret(data))
			source := &SecretKeySource{Reader: client, Writer: client, Namespace: testSecretNamespace, Name: testSecretName}
			if _, err := source.SigningKeys(context.Background()); !errors.Is(err, ErrSigningUnavailable) {
				t.Fatalf("SigningKeys() error = %v, want ErrSigningUnavailable", err)
			}
			secret, _ := getSecret(t, client)
			for item, value := range data {
				if string(secret.Data[item]) != string(value) {
					t.Fatalf("item %q was overwritten", item)
				}
			}
		})
	}
}

func TestSecretKeySourcePicksUpRotationAfterCacheTTL(t *testing.T) {
	t.Parallel()
	oldPEM, newPEMBytes := newPEM(t), newPEM(t)
	client := newFakeClient(t, signingSecret(map[string][]byte{SecretKeyCurrent: oldPEM}))
	now := time.Unix(1_800_000_000, 0)
	source := &SecretKeySource{
		Reader: client, Writer: client, Namespace: testSecretNamespace, Name: testSecretName,
		CacheTTL: 10 * time.Second, Now: func() time.Time { return now },
	}
	before, err := source.SigningKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	secret, _ := getSecret(t, client)
	secret.Data = map[string][]byte{SecretKeyCurrent: newPEMBytes, SecretKeyPrevious: oldPEM}
	if err := client.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}

	cached, _ := source.SigningKeys(context.Background())
	if cached.Current.KeyID != before.Current.KeyID {
		t.Fatal("rotation visible before cache TTL; expected cached key")
	}
	now = now.Add(11 * time.Second)
	after, err := source.SigningKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Current.KeyID == before.Current.KeyID || after.Previous == nil || after.Previous.KeyID != before.Current.KeyID {
		t.Fatalf("after rotation current=%s previous=%v, want new current and old previous", after.Current.KeyID, after.Previous)
	}
	published, err := source.PublishedKeys(context.Background())
	if err != nil || len(published.JWKS().Keys) != 2 {
		t.Fatalf("published JWKS = %#v, %v; want two keys", published.JWKS(), err)
	}
}

func TestSecretKeySourceWithoutConfigurationIsUnavailable(t *testing.T) {
	t.Parallel()
	var source *SecretKeySource
	if _, err := source.SigningKeys(context.Background()); !errors.Is(err, ErrSigningUnavailable) {
		t.Fatalf("nil source error = %v", err)
	}
	client := newFakeClient(t)
	readOnly := &SecretKeySource{Reader: client, Namespace: testSecretNamespace, Name: testSecretName}
	if _, err := readOnly.SigningKeys(context.Background()); !errors.Is(err, ErrSigningUnavailable) {
		t.Fatalf("read-only source error = %v, want ErrSigningUnavailable", err)
	}
	if _, err := (StaticKeySource{}).SigningKeys(context.Background()); !errors.Is(err, ErrSigningUnavailable) {
		t.Fatalf("empty static source error = %v", err)
	}
}
