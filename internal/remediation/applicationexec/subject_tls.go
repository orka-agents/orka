package applicationexec

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"strings"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func subjectTLSName(id string) string {
	return "orka-tls-" + strings.TrimPrefix(pv.Digest([]byte(id)), "sha256:")[:24]
}

func mintLabTLS(bundle pv.ApplicationSubjectTLSBundle, namespace string, deadline time.Time) (map[string][]byte, error) {
	names, err := pv.ApplicationSubjectTLSDNSNames(bundle, namespace)
	if err != nil || len(names) == 0 {
		return nil, contract.ErrInvalid
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	caSerial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	serverSerial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	caSerial.Add(caSerial, big.NewInt(1))
	serverSerial.Add(serverSerial, big.NewInt(1))
	notBefore, notAfter := time.Now().UTC().Add(-5*time.Minute), deadline.Add(time.Hour)
	ca := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "Orka ephemeral application lab CA"},
		NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	server := &x509.Certificate{SerialNumber: serverSerial, Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: notBefore, NotAfter: notAfter, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	// The CA signing key is never serialized or retained. Only synthetic
	// run-local server material is delivered through an immutable target Secret.
	return map[string][]byte{
		caCertificateKey:        pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func (b *Backend) ensureSubjectTLS(ctx context.Context, r *run, side string) error {
	policy := r.state.Request.ApplicationPolicy
	if policy == nil {
		return nil
	}
	for _, bundle := range policy.SubjectTLS {
		key := resourceKey(side, "subject-tls-"+bundle.ID)
		if r.effect(key) != nil {
			continue
		}
		data, err := mintLabTLS(bundle, namespaceName(r, side), r.state.Deadline)
		if err != nil {
			return err
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: subjectTLSName(bundle.ID), Namespace: namespaceName(r, side)},
			Immutable: new(true), Type: corev1.SecretTypeTLS, Data: data}
		if _, err := b.ensureObject(ctx, r, key, secret); err != nil {
			return err
		}
	}
	identities, err := b.subjectTLSIdentities(ctx, r, side)
	if err != nil {
		return err
	}
	if len(identities) == 0 {
		return nil
	}
	if previous, found := r.state.SubjectTLS[side]; found {
		if !reflect.DeepEqual(previous, identities) {
			return contract.ErrConflict
		}
		return nil
	}
	if r.state.SubjectTLS == nil {
		r.state.SubjectTLS = make(map[string][]pv.ApplicationSubjectTLSIdentity)
	}
	r.state.SubjectTLS[side] = identities
	return b.save(ctx, r)
}

func (b *Backend) subjectTLSIdentities(ctx context.Context, r *run, side string) ([]pv.ApplicationSubjectTLSIdentity, error) {
	policy := r.state.Request.ApplicationPolicy
	if policy == nil || len(policy.SubjectTLS) == 0 {
		return nil, nil
	}
	namespace := r.effect(resourceKey(side, "namespace"))
	if namespace == nil || namespace.UID == "" {
		return nil, contract.ErrConflict
	}
	var identities []pv.ApplicationSubjectTLSIdentity
	for _, bundle := range policy.SubjectTLS {
		object, err := b.boundObject(ctx, r, resourceKey(side, "subject-tls-"+bundle.ID))
		if err != nil {
			return nil, err
		}
		secret := object.(*corev1.Secret)
		if !ptr.Deref(secret.Immutable, false) || secret.Type != corev1.SecretTypeTLS || len(secret.Data) != 3 {
			return nil, contract.ErrConflict
		}
		digest, err := pv.ValidateApplicationTLSBundle(bundle, namespace.Name, secret.Data[caCertificateKey], secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
		if err != nil {
			return nil, contract.ErrConflict
		}
		identities = append(identities, pv.ApplicationSubjectTLSIdentity{ID: bundle.ID, Namespace: namespace.Name,
			NamespaceUID: string(namespace.UID), Name: secret.Name, UID: string(secret.UID), Digest: digest})
	}
	return identities, nil
}
