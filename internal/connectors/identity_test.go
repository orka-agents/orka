/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"bytes"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestSubjectDigestAndConnectionName(t *testing.T) {
	a := SubjectDigest("https://issuer.example.test", "alice")
	b := SubjectDigest("https://issuer.example.test", "bob")
	c := SubjectDigest("https://other.example.test", "alice")
	if a == b || a == c || len(a) != 64 {
		t.Fatalf("digests must differ per issuer and subject: %s %s %s", a, b, c)
	}
	// Claims are opaque: a differently spaced subject is a different person.
	if SubjectDigest(" https://issuer.example.test ", " alice ") == a {
		t.Fatal("digest must hash the exact verified claims")
	}
	name := ConnectionName("github", "https://issuer.example.test", "alice")
	if !strings.HasPrefix(name, "github-") || len(name) != len("github-")+connectionNameDigestLength {
		t.Fatalf("name = %q", name)
	}
	if name == ConnectionName("github", "https://issuer.example.test", "bob") {
		t.Fatal("names must differ per subject")
	}
	long := ConnectionName(strings.Repeat("p", 80), "i", "s")
	if len(long) > maxConnectionNameLength {
		t.Fatalf("name %q exceeds the DNS label bound", long)
	}
	if !strings.HasPrefix(ConnectionName("", "i", "s"), "connection-") {
		t.Fatal("empty provider must fall back to a generic prefix")
	}
	// Providers that share a truncated prefix still get distinct names.
	sharedPrefix := strings.Repeat("p", 55)
	if ConnectionName(sharedPrefix+"-one", "i", "s") == ConnectionName(sharedPrefix+"-two", "i", "s") {
		t.Fatal("the digest must cover the full provider name")
	}
}

func TestCredentialRef(t *testing.T) {
	if _, err := CredentialRef(nil); err == nil {
		t.Fatal("nil connection must fail")
	}
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "github-abc", Namespace: "tenant"},
		Spec: corev1alpha1.ConnectionSpec{
			Subject:     corev1alpha1.ConnectionSubject{Issuer: "https://issuer.example.test", Subject: "alice"},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"},
		},
	}
	if _, err := CredentialRef(connection); err == nil {
		t.Fatal("connection without UID must fail")
	}
	connection.UID = types.UID("uid-1")
	ref, err := CredentialRef(connection)
	if err != nil {
		t.Fatal(err)
	}
	if ref.ConnectionUID != "uid-1" || ref.Provider != "github" || ref.SubjectDigest != SubjectDigest("https://issuer.example.test", "alice") {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestSignAndVerifyState(t *testing.T) {
	key := bytes.Repeat([]byte{7}, MinStateKeyBytes)
	state, err := SignState(key, "nonce123")
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := VerifyState(key, state)
	if err != nil || nonce != "nonce123" {
		t.Fatalf("VerifyState = %q, %v", nonce, err)
	}
	other := bytes.Repeat([]byte{8}, MinStateKeyBytes)
	if _, err := VerifyState(other, state); err == nil {
		t.Fatal("a different key must not verify")
	}
	for name, bad := range map[string]string{
		"tampered nonce":  "nonce124" + state[len("nonce123"):],
		"tampered sig":    state[:len(state)-1] + "A",
		"no separator":    "nonce123",
		"empty":           "",
		"bad base64":      "nonce123.***",
		"too long":        strings.Repeat("a", 300) + ".sig",
		"empty signature": "nonce123.",
	} {
		if _, err := VerifyState(key, bad); err == nil {
			t.Fatalf("%s must fail", name)
		}
	}
	if _, err := SignState(key[:8], "nonce"); err == nil {
		t.Fatal("short key must fail")
	}
	if _, err := VerifyState(key[:8], state); err == nil {
		t.Fatal("short key must fail on verify")
	}
	if _, err := SignState(key, "with.dot"); err == nil {
		t.Fatal("nonce containing the separator must fail")
	}
	if _, err := SignState(key, " "); err == nil {
		t.Fatal("blank nonce must fail")
	}
}
