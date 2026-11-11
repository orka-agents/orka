package patchverification

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func applicationLabTLS(t *testing.T, names []string) ([]byte, []byte, []byte) {
	t.Helper()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	authority := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic lab CA"},
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(2 * time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, authority, authority, caPublic, caPrivate)
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: names,
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certificateDER, err := x509.CreateCertificate(rand.Reader, leaf, authority, public, caPrivate)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func TestSyntheticStartupTLSBindsOnlyValidShortLivedMaterialMetadata(t *testing.T) {
	profile := ApplicationSubjectTLSBundle{ID: "server"}
	ca, certificate, key := applicationLabTLS(t, []string{"localhost"})
	digest, err := ValidateApplicationTLSBundle(profile, "run-owned", ca, certificate, key)
	require.NoError(t, err)
	canonical, err := ApplicationTLSBundleDigest(ca, certificate, key)
	require.NoError(t, err)
	require.Equal(t, canonical, digest)
	policy := &ApplicationPolicy{Version: ApplicationProtocolVersion, SubjectTLS: []ApplicationSubjectTLSBundle{profile}}
	identity := ApplicationSubjectTLSIdentity{ID: "server", Namespace: "run-owned", NamespaceUID: "namespace-uid",
		Name: "synthetic-tls", UID: "synthetic-tls-uid", Digest: digest}
	require.NoError(t, ValidateApplicationSubjectTLSIdentities(policy,
		ApplicationTarget{Namespace: "run-owned", NamespaceUID: "namespace-uid"}, []ApplicationSubjectTLSIdentity{identity}))
	metadata, err := json.Marshal(identity)
	require.NoError(t, err)
	require.False(t, bytes.Contains(metadata, key))
	_, _, anotherKey := applicationLabTLS(t, []string{"localhost"})
	_, err = ValidateApplicationTLSBundle(profile, "run-owned", ca, certificate, anotherKey)
	require.Error(t, err)
	profile.DNSNames = []string{"wrong.example"}
	_, err = ValidateApplicationTLSBundle(profile, "run-owned", ca, certificate, key)
	require.Error(t, err)
	profile.ID = "../outside"
	_, err = ApplicationSubjectTLSDNSNames(profile, "run-owned")
	require.Error(t, err)
}

func TestFixtureSecretFilesAreOnlySelectedGeneratedPlanKeys(t *testing.T) {
	policy := applicationTestPolicy(t)
	policy.Fixtures = []ApplicationFixtureProfile{{
		ID: "service-profile", Image: "example/service@" + Digest([]byte("service-image")), ImageID: Digest([]byte("service-config")),
		Args: []string{"/usr/bin/service", "--credential-file=/fixture-secrets/credential"}, Ports: []int{8080},
		SecretFiles: []ApplicationFixtureSecretFile{{ID: "credential", SecretName: "generated", Key: "value"}},
	}}
	plan := *applicationTestManifest(t).Application
	plan.Fixtures = []ApplicationFixture{{ID: "service", Kind: "image", Profile: "service-profile", Port: 8080}}
	plan.SyntheticSecrets = []ApplicationSyntheticSecret{{Name: "generated", Keys: []ApplicationSyntheticSecretKey{{Name: "value", MarkerID: "generated-marker"}}}}
	require.NoError(t, ValidateApplicationPlanWithPolicy(plan, policy))
	plan.SyntheticSecrets[0].Name = "another"
	require.Error(t, ValidateApplicationPlanWithPolicy(plan, policy))
	plan.SyntheticSecrets[0].Name = "generated"
	policy.Fixtures[0].SecretFiles[0].ID = "../outside"
	require.Error(t, ValidateApplicationPolicy(*policy))
	policy.Fixtures[0].SecretFiles[0].ID = "credential"
	policy.Fixtures[0].ReplicaTarget = &ApplicationReplicaTargetProfile{Name: "service", MaxReplicas: 2}
	require.Error(t, ValidateApplicationPolicy(*policy), "replica fixtures cannot acquire credential files")
}

func TestStartupTLSProfilesRejectUnsafeAndExcessiveMaterialDeclarations(t *testing.T) {
	for _, profile := range []ApplicationSubjectTLSBundle{
		{ID: "../outside"},
		{ID: "server", DNSNames: []string{"*.example"}},
		{ID: "server", DNSNames: []string{"https://example"}},
		{ID: "server", DNSNames: []string{"prefix{{namespace}}.example"}},
		{ID: "server", DNSNames: []string{"localhost", "localhost"}},
	} {
		policy := &ApplicationPolicy{Version: ApplicationProtocolVersion, SubjectTLS: []ApplicationSubjectTLSBundle{profile}}
		require.Error(t, ValidateApplicationPolicy(*policy))
	}
	policy := &ApplicationPolicy{Version: ApplicationProtocolVersion, SubjectTLS: []ApplicationSubjectTLSBundle{
		{ID: "one"}, {ID: "two"}, {ID: "three"},
	}}
	require.Error(t, ValidateApplicationPolicy(*policy))
	names, err := ApplicationSubjectTLSDNSNames(ApplicationSubjectTLSBundle{ID: "server",
		DNSNames: []string{"service.{{namespace}}.svc", "localhost"}}, "run-owned")
	require.NoError(t, err)
	require.Equal(t, []string{"localhost", "service.run-owned.svc"}, names)
	_, err = ApplicationTLSBundleDigest(nil, []byte("certificate"), []byte("key"))
	require.Error(t, err)
	_, err = ApplicationTLSBundleDigest(make([]byte, (16<<10)+1), []byte("certificate"), []byte("key"))
	require.Error(t, err)
}

func TestFixtureMaterialBindingsRejectForeignNamespaceAndUnknownSelection(t *testing.T) {
	manifest := applicationTestManifest(t)
	policy := applicationTestPolicy(t)
	policy.Fixtures = []ApplicationFixtureProfile{{ID: "service", Image: "example/service@" + Digest([]byte("image")),
		ImageID: Digest([]byte("config")), Args: []string{"/usr/bin/service"}, Ports: []int{8080},
		SecretFiles: []ApplicationFixtureSecretFile{{ID: "credential", SecretName: "generated", Key: "value"}}}}
	manifest.ApplicationPolicy = policy
	manifest.Application.Fixtures = []ApplicationFixture{{ID: "service", Kind: "image", Profile: "service", Port: 8080}}
	manifest.Application.SyntheticSecrets = []ApplicationSyntheticSecret{{Name: "generated",
		Keys: []ApplicationSyntheticSecretKey{{Name: "value", MarkerID: "generated-marker"}}}}
	input := ApplicationWorkerInput{Manifest: manifest, FixtureID: "service",
		Target: ApplicationTarget{Namespace: "run-owned", NamespaceUID: "run-uid"},
		FixtureSecrets: []ApplicationFixtureSecretIdentity{{ID: "credential", Namespace: "protected-run",
			NamespaceUID: "protected-uid", Name: "fixture-material", UID: "material-uid", Key: "credential", Digest: Digest([]byte("value"))}}}
	require.NoError(t, ValidateApplicationFixtureSecretIdentities(input))
	input.FixtureSecrets[0].Namespace = "run-owned"
	require.Error(t, ValidateApplicationFixtureSecretIdentities(input))
	input.FixtureSecrets[0].Namespace = "protected-run"
	input.FixtureSecrets[0].ID = "unselected"
	require.Error(t, ValidateApplicationFixtureSecretIdentities(input))
	input.FixtureSecrets[0].ID = "credential"
	input.FixtureSecrets[0].UID = ""
	require.Error(t, ValidateApplicationFixtureSecretIdentities(input))
}
