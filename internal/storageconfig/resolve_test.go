package storageconfig

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testNamespace = "orka-system"
	testToken     = "0123456789abcdef0123456789abcdef-token"
)

// countingReader counts Secret reads and can fail them.
type countingReader struct {
	client.Reader
	gets int
	err  error
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.gets++
	if r.err != nil {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func newReader(objects ...client.Object) *countingReader {
	return &countingReader{Reader: fake.NewClientBuilder().WithObjects(objects...).Build()}
}

func secret(namespace, name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Data: data}
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "orka-storage-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func loadAdapterConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := loadFlags(t, "--storage-config="+writeConfig(t, adapterFile))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func adapterSecrets(t *testing.T, namespace string) []client.Object {
	t.Helper()
	return []client.Object{
		secret(namespace, "orka-storage-postgres-ca", map[string][]byte{"ca.crt": testCAPEM(t)}),
		secret(namespace, "orka-storage-postgres-client", map[string][]byte{"token": []byte(testToken)}),
	}
}

func TestResolveSQLiteMakesNoAPICalls(t *testing.T) {
	cfg, err := loadFlags(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	reader := &countingReader{err: errors.New("unexpected read")}
	resolved, err := cfg.Resolve(context.Background(), reader, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if reader.gets != 0 || len(resolved.Credentials) != 0 || resolved.Config != cfg {
		t.Fatalf("gets = %d, credentials = %v; want no reads and no credentials", reader.gets, resolved.Credentials)
	}
}

func TestResolveReadsAdapterSecretsOnce(t *testing.T) {
	cfg := loadAdapterConfig(t)
	reader := newReader(adapterSecrets(t, testNamespace)...)
	resolved, err := cfg.Resolve(context.Background(), reader, testNamespace)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved.Credentials) != 1 {
		t.Fatalf("credentials = %v, want one entry for the shared adapter", resolved.Credentials)
	}
	credentials := resolved.Credentials["postgres"]
	if credentials == nil || string(credentials.BearerToken()) != testToken {
		t.Fatalf("bearer token not resolved")
	}
	if !x509.NewCertPool().AppendCertsFromPEM(credentials.CABundle()) {
		t.Fatal("CA bundle not resolved")
	}
	if reader.gets != 2 {
		t.Fatalf("Secret reads = %d, want 2 for two contracts sharing one adapter", reader.gets)
	}
	token := credentials.BearerToken()
	token[0] = 'X'
	if string(credentials.BearerToken()) != testToken {
		t.Fatal("BearerToken returned the stored slice, not a copy")
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, value := range []any{credentials, *credentials, resolved} {
			if rendered := fmt.Sprintf(format, value); strings.Contains(rendered, testToken) {
				t.Fatalf("%s of %T prints the bearer token: %s", format, value, rendered)
			}
		}
	}
}

func TestResolveSharedSecretIsReadOnce(t *testing.T) {
	shared := strings.ReplaceAll(adapterFile, "name: orka-storage-postgres-ca", "name: orka-storage-postgres-client")
	cfg, err := loadFlags(t, "--storage-config="+writeConfig(t, shared))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	reader := newReader(secret(testNamespace, "orka-storage-postgres-client", map[string][]byte{
		"ca.crt": testCAPEM(t), "token": []byte(testToken),
	}))
	if _, err := cfg.Resolve(context.Background(), reader, testNamespace); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if reader.gets != 1 {
		t.Fatalf("Secret reads = %d, want 1 for a CA and token in one Secret", reader.gets)
	}
}

func TestResolveFailures(t *testing.T) {
	ca := testCAPEM(t)
	for _, tc := range []struct {
		name      string
		objects   []client.Object
		readerErr error
		namespace string
		want      Reason
	}{
		{name: "token Secret missing", objects: []client.Object{
			secret(testNamespace, "orka-storage-postgres-ca", map[string][]byte{"ca.crt": ca}),
		}, want: ReasonSecretNotFound},
		{name: "Secret only in another namespace", objects: adapterSecrets(t, "other"), want: ReasonSecretNotFound},
		{name: "token key missing", objects: []client.Object{
			secret(testNamespace, "orka-storage-postgres-ca", map[string][]byte{"ca.crt": ca}),
			secret(testNamespace, "orka-storage-postgres-client", map[string][]byte{"other": []byte(testToken)}),
		}, want: ReasonSecretKeyMissing},
		{name: "token empty", objects: []client.Object{
			secret(testNamespace, "orka-storage-postgres-ca", map[string][]byte{"ca.crt": ca}),
			secret(testNamespace, "orka-storage-postgres-client", map[string][]byte{"token": {}}),
		}, want: ReasonSecretKeyMissing},
		{name: "token too short", objects: []client.Object{
			secret(testNamespace, "orka-storage-postgres-ca", map[string][]byte{"ca.crt": ca}),
			secret(testNamespace, "orka-storage-postgres-client", map[string][]byte{"token": []byte("short-token")}),
		}, want: ReasonInvalidSecret},
		{name: "token with trailing newline", objects: []client.Object{
			secret(testNamespace, "orka-storage-postgres-ca", map[string][]byte{"ca.crt": ca}),
			secret(testNamespace, "orka-storage-postgres-client", map[string][]byte{"token": []byte(testToken + "\n")}),
		}, want: ReasonInvalidSecret},
		{name: "token with inner space", objects: []client.Object{
			secret(testNamespace, "orka-storage-postgres-ca", map[string][]byte{"ca.crt": ca}),
			secret(testNamespace, "orka-storage-postgres-client", map[string][]byte{"token": []byte("Bearer " + testToken)}),
		}, want: ReasonInvalidSecret},
		{name: "CA Secret missing", objects: []client.Object{
			secret(testNamespace, "orka-storage-postgres-client", map[string][]byte{"token": []byte(testToken)}),
		}, want: ReasonSecretNotFound},
		{name: "CA not PEM", objects: []client.Object{
			secret(testNamespace, "orka-storage-postgres-ca", map[string][]byte{"ca.crt": []byte("not a certificate")}),
			secret(testNamespace, "orka-storage-postgres-client", map[string][]byte{"token": []byte(testToken)}),
		}, want: ReasonInvalidSecret},
		{
			name:      "read forbidden",
			readerErr: apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "orka-storage-postgres-client", errors.New("denied")),
			want:      ReasonSecretUnavailable,
		},
		{name: "API unavailable", readerErr: apierrors.NewServiceUnavailable("down"), want: ReasonSecretUnavailable},
		{name: "unknown namespace", objects: adapterSecrets(t, testNamespace), namespace: "-", want: ReasonInvalidConfiguration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadAdapterConfig(t)
			reader := newReader(tc.objects...)
			reader.err = tc.readerErr
			namespace := testNamespace
			if tc.namespace == "-" {
				namespace = ""
			}
			resolved, err := cfg.Resolve(context.Background(), reader, namespace)
			if resolved != nil {
				t.Fatalf("Resolve returned %v with error %v", resolved, err)
			}
			configErr := requireConfigError(t, err, tc.want)
			if tc.want != ReasonInvalidConfiguration && configErr.Backend != "postgres" {
				t.Fatalf("error backend = %q, want postgres", configErr.Backend)
			}
			if strings.Contains(err.Error(), testToken) || bytes.Contains([]byte(err.Error()), []byte("short-token")) {
				t.Fatalf("error prints a Secret value: %v", err)
			}
		})
	}
}

func TestResolveReadsOnlySecretsInTheInstallationNamespace(t *testing.T) {
	cfg := loadAdapterConfig(t)
	var namespaces []string
	reader := fake.NewClientBuilder().WithObjects(adapterSecrets(t, testNamespace)...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); !ok {
				return fmt.Errorf("unexpected %T read", obj)
			}
			namespaces = append(namespaces, key.Namespace)
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	if _, err := cfg.Resolve(context.Background(), reader, testNamespace); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(namespaces) != 2 || namespaces[0] != testNamespace || namespaces[1] != testNamespace {
		t.Fatalf("Secret reads in namespaces %v, want two in %s", namespaces, testNamespace)
	}
}
