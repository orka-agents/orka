package patchverification

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"slices"
	"strings"
	"time"

	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

func ApplicationSubjectTLSDNSNames(bundle ApplicationSubjectTLSBundle, namespace string) ([]string, error) {
	if !applicationName(bundle.ID) || !applicationName(namespace) || len(bundle.DNSNames) > 8 {
		return nil, ErrEvidence
	}
	names := bundle.DNSNames
	if len(names) == 0 {
		names = []string{"localhost"}
	}
	result := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		parts := strings.Split(name, ".")
		for i, part := range parts {
			if part == "{{namespace}}" {
				parts[i] = namespace
			} else if strings.ContainsAny(part, "{}") {
				return nil, ErrEvidence
			}
		}
		value := strings.Join(parts, ".")
		if len(kvalidation.IsDNS1123Subdomain(value)) != 0 || seen[value] {
			return nil, ErrEvidence
		}
		seen[value] = true
		result = append(result, value)
	}
	slices.Sort(result)
	return result, nil
}

func ApplicationTLSBundleDigest(ca, certificate, key []byte) (string, error) {
	for _, content := range [][]byte{ca, certificate, key} {
		if len(content) == 0 || len(content) > 16<<10 {
			return "", ErrLimit
		}
	}
	content, err := json.Marshal(map[string][]byte{"ca.crt": ca, "tls.crt": certificate, "tls.key": key})
	if err != nil {
		return "", ErrEvidence
	}
	return Digest(content), nil
}

func ValidateApplicationTLSBundle(bundle ApplicationSubjectTLSBundle, namespace string, ca, certificate, key []byte) (string, error) {
	digest, err := ApplicationTLSBundleDigest(ca, certificate, key)
	if err != nil {
		return "", err
	}
	names, err := ApplicationSubjectTLSDNSNames(bundle, namespace)
	if err != nil {
		return "", err
	}
	block, remaining := pem.Decode(ca)
	if block == nil || block.Type != "CERTIFICATE" || strings.TrimSpace(string(remaining)) != "" {
		return "", errors.New("application startup TLS requires a bounded self-signed lab CA")
	}
	authority, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !authority.IsCA || !authority.BasicConstraintsValid || authority.CheckSignatureFrom(authority) != nil {
		return "", ErrEvidence
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil || len(pair.Certificate) != 1 {
		return "", errors.New("application startup TLS certificate and private key do not form the expected pair")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.IsCA || len(leaf.IPAddresses) != 0 || len(leaf.URIs) != 0 || len(leaf.EmailAddresses) != 0 {
		return "", ErrEvidence
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority)
	now := time.Now()
	if authority.NotAfter.After(now.Add(24*time.Hour)) || leaf.NotAfter.After(now.Add(24*time.Hour)) {
		return "", errors.New("application startup TLS material must be short-lived synthetic lab data")
	}
	actual := slices.Clone(leaf.DNSNames)
	slices.Sort(actual)
	if !slices.Equal(names, actual) {
		return "", ErrBinding
	}
	for _, name := range names {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, CurrentTime: now,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return "", errors.New("application startup TLS material is invalid for its frozen lab names")
		}
	}
	return digest, nil
}

func ValidateApplicationSubjectTLSIdentities(policy *ApplicationPolicy, target ApplicationTarget, identities []ApplicationSubjectTLSIdentity) error {
	profiles := []ApplicationSubjectTLSBundle(nil)
	if policy != nil {
		profiles = policy.SubjectTLS
	}
	if len(identities) != len(profiles) {
		return ErrBinding
	}
	seen := map[string]bool{}
	for _, identity := range identities {
		found := false
		for _, profile := range profiles {
			found = found || profile.ID == identity.ID
		}
		if !found || seen[identity.ID] || identity.Namespace != target.Namespace ||
			identity.NamespaceUID != target.NamespaceUID || !applicationName(identity.Name) ||
			!identifierPattern.MatchString(identity.UID) || !sha256Pattern.MatchString(identity.Digest) {
			return ErrBinding
		}
		seen[identity.ID] = true
	}
	return nil
}

func applicationFixtureProfile(plan ApplicationPlan, policy *ApplicationPolicy, id string) (*ApplicationFixtureProfile, *ApplicationFixture) {
	if policy == nil {
		return nil, nil
	}
	for i := range plan.Fixtures {
		fixture := &plan.Fixtures[i]
		if fixture.ID != id {
			continue
		}
		for j := range policy.Fixtures {
			if policy.Fixtures[j].ID == fixture.Profile {
				return &policy.Fixtures[j], fixture
			}
		}
	}
	return nil, nil
}

func ValidateApplicationFixtureSecretIdentities(input ApplicationWorkerInput) error {
	profile, fixture := applicationFixtureProfile(*input.Manifest.Application, input.Manifest.ApplicationPolicy, input.FixtureID)
	if profile == nil || fixture == nil || fixture.Kind != ApplicationFixtureImage {
		if len(input.FixtureSecrets) == 0 {
			return nil
		}
		return ErrBinding
	}
	if len(input.FixtureSecrets) != len(profile.SecretFiles) {
		return ErrBinding
	}
	seen := map[string]bool{}
	for _, identity := range input.FixtureSecrets {
		found := false
		for _, file := range profile.SecretFiles {
			found = found || file.ID == identity.ID
		}
		if !found || seen[identity.ID] || !applicationName(identity.Namespace) ||
			!identifierPattern.MatchString(identity.NamespaceUID) || identity.Namespace == input.Target.Namespace ||
			identity.NamespaceUID == input.Target.NamespaceUID || !applicationName(identity.Name) ||
			!identifierPattern.MatchString(identity.UID) || len(kvalidation.IsConfigMapKey(identity.Key)) != 0 ||
			!sha256Pattern.MatchString(identity.Digest) {
			return ErrBinding
		}
		seen[identity.ID] = true
	}
	return nil
}
