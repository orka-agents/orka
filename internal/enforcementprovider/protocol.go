package enforcementprovider

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	maxPolicyBytes       = 1 << 20
	maxTrustProfileBytes = 64 << 10
)

var policyDomain = []byte("orka.enforcement.policy/v2")

type canonicalEncoder struct{ bytes.Buffer }

func (e *canonicalEncoder) field(tag uint16, value []byte) {
	var h [10]byte
	binary.BigEndian.PutUint16(h[:2], tag)
	binary.BigEndian.PutUint64(h[2:], uint64(len(value)))
	e.Write(h[:])
	e.Write(value)
}
func (e *canonicalEncoder) text(tag uint16, value string) { e.field(tag, []byte(value)) }
func (e *canonicalEncoder) number(tag uint16, value uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], value)
	e.field(tag, b[:])
}
func (e *canonicalEncoder) instant(tag uint16, value time.Time) {
	e.text(tag, value.UTC().Format(time.RFC3339Nano))
}
func encodeStrings(values []string) []byte {
	var e canonicalEncoder
	e.number(1, uint64(len(values)))
	for _, value := range values {
		e.text(2, value)
	}
	return e.Bytes()
}
func transcript(domain string, fields func(*canonicalEncoder)) []byte {
	var e canonicalEncoder
	e.text(1, domain)
	e.number(2, 1)
	fields(&e)
	return e.Bytes()
}
func hashTranscript(domain string, fields func(*canonicalEncoder)) string {
	h := sha256.Sum256(transcript(domain, fields))
	return "sha256:" + hex.EncodeToString(h[:])
}
func PolicyDigest(b []byte) string {
	return hashTranscript(string(policyDomain), func(e *canonicalEncoder) { e.field(10, b) })
}
func descriptorMessage(d Descriptor) []byte {
	return transcript("orka.enforcement.descriptor/v2", func(e *canonicalEncoder) {
		e.text(10, ProtocolVersion)
		e.text(11, d.ProviderID)
		e.text(12, d.EndpointID)
		e.text(13, d.Algorithm)
		e.text(14, d.KeyID)
		e.field(15, d.PublicKey)
		e.number(16, d.Epoch)
		e.instant(17, d.IssuedAt)
		e.instant(18, d.ExpiresAt)
		e.field(19, encodeStrings(d.ProtocolVersions))
		e.field(20, encodeStrings(d.Capabilities))
	})
}
func SignDescriptor(d *Descriptor, key ed25519.PrivateKey) {
	d.Signature = ed25519.Sign(key, descriptorMessage(*d))
}
func authenticateDescriptorContract(d Descriptor, trust TrustProfile) error {
	if trust.ProviderID == "" || trust.EndpointID == "" || trust.Algorithm != "ed25519" || trust.KeyID == "" || len(trust.PublicKey) != ed25519.PublicKeySize {
		return errors.New("invalid provider trust profile")
	}
	if d.ProviderID != trust.ProviderID || d.EndpointID != trust.EndpointID || d.Algorithm != trust.Algorithm || d.KeyID != trust.KeyID || !sameBytes(d.PublicKey, trust.PublicKey) {
		return errors.New("descriptor identity is not trusted")
	}
	if d.Epoch < trust.MinimumEpoch {
		return errors.New("descriptor epoch is revoked")
	}
	if !ed25519.Verify(ed25519.PublicKey(trust.PublicKey), descriptorMessage(d), d.Signature) {
		return errors.New("invalid descriptor signature")
	}
	return nil
}
func AuthenticateDescriptor(d Descriptor, trust TrustProfile, now time.Time) error {
	if err := authenticateDescriptorContract(d, trust); err != nil {
		return err
	}
	if now.Before(d.IssuedAt) || !now.Before(d.ExpiresAt) {
		return errors.New("stale descriptor")
	}
	return nil
}
func negotiateCapabilities(d Descriptor, required []string) error {
	ok := false
	for _, v := range d.ProtocolVersions {
		if v == ProtocolVersion {
			ok = true
		}
	}
	if !ok {
		return errors.New("unsupported protocol")
	}
	have := map[string]bool{}
	for _, c := range d.Capabilities {
		have[c] = true
	}
	for _, c := range required {
		if !have[c] {
			return fmt.Errorf("unsupported capability %q", c)
		}
	}
	return nil
}
func validateRequiredCapabilities(required []string) error {
	if len(required) == 0 {
		return errors.New("required capabilities must not be empty")
	}
	seen := map[string]bool{}
	for _, c := range required {
		if c == "" || strings.TrimSpace(c) != c || len(c) > 256 || strings.ContainsRune(c, '\x00') {
			return errors.New("required capability is invalid")
		}
		if seen[c] {
			return fmt.Errorf("duplicate required capability %q", c)
		}
		seen[c] = true
	}
	return nil
}
func Negotiate(d Descriptor, trust TrustProfile, required []string, now time.Time) error {
	if err := validateRequiredCapabilities(required); err != nil {
		return err
	}
	if err := AuthenticateDescriptor(d, trust, now); err != nil {
		return err
	}
	return negotiateCapabilities(d, required)
}
func negotiateAdmittedDescriptor(d Descriptor, trust TrustProfile, required []string) error {
	if err := validateRequiredCapabilities(required); err != nil {
		return err
	}
	if err := authenticateDescriptorContract(d, trust); err != nil {
		return err
	}
	return negotiateCapabilities(d, required)
}
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func validatePolicyCapabilityContract(policy []byte, required []string) error {
	if len(policy) == 0 {
		return errors.New("effective policy must not be empty")
	}
	if len(policy) > maxPolicyBytes {
		return errors.New("effective policy exceeds limit")
	}
	return validateRequiredCapabilities(required)
}
func validateAuthorizationContract(a Authorization) error {
	c := a.Context
	if err := validatePolicyCapabilityContract(c.PolicyBytes, c.RequiredCapabilities); err != nil {
		return err
	}
	if a.ID == "" || c.RequestID == "" || c.AttemptID == "" || c.WorkloadID == "" || c.ProviderID == "" || c.Audience == "" || c.Challenge == "" || c.IdempotencyKey == "" {
		return errors.New("authorization binding is incomplete")
	}
	if PolicyDigest(c.PolicyBytes) != c.PolicyDigest {
		return errors.New("policy digest mismatch")
	}
	return nil
}
func ValidateAuthorization(a Authorization, now time.Time) error {
	if err := validateAuthorizationContract(a); err != nil {
		return err
	}
	if now.Before(a.NotBefore) || now.Before(a.IssuedAt) || !now.Before(a.ExpiresAt) {
		return errors.New("authorization is not fresh")
	}
	return nil
}
func authorizationDigest(a Authorization) string {
	return hashTranscript("orka.enforcement.authorization/v2", func(e *canonicalEncoder) {
		c := a.Context
		e.text(10, a.ID)
		e.text(11, c.RequestID)
		e.text(12, c.AttemptID)
		e.text(13, c.WorkloadID)
		e.text(14, c.ProviderID)
		e.text(15, c.Audience)
		e.text(16, c.Challenge)
		e.text(17, c.IdempotencyKey)
		e.text(18, c.PolicyDigest)
		e.field(19, c.PolicyBytes)
		e.field(20, encodeStrings(c.RequiredCapabilities))
		e.instant(21, a.IssuedAt)
		e.instant(22, a.NotBefore)
		e.instant(23, a.ExpiresAt)
		e.number(24, a.Epoch)
	})
}
func readbackDigest(r ReadBack) string {
	return hashTranscript("orka.enforcement.readback/v2", func(e *canonicalEncoder) {
		e.text(10, r.ActivationID)
		e.text(11, r.WorkloadID)
		e.text(12, r.PolicyDigest)
		e.text(13, r.ConfigurationDigest)
	})
}
func proofMessage(p Proof) []byte {
	return transcript("orka.enforcement.proof/v2", func(e *canonicalEncoder) {
		e.text(10, ProtocolVersion)
		e.text(11, p.ID)
		e.text(12, p.AuthorizationID)
		e.text(13, p.AuthorizationDigest)
		e.text(14, p.ActivationID)
		e.text(15, p.WorkloadID)
		e.text(16, p.ProviderID)
		e.text(17, p.Audience)
		e.text(18, p.Challenge)
		e.text(19, p.PolicyDigest)
		e.text(20, p.ReadBackDigest)
		e.text(21, p.KeyID)
		e.number(22, p.Epoch)
		e.instant(23, p.IssuedAt)
		e.instant(24, p.ExpiresAt)
	})
}
func SignProof(p *Proof, key ed25519.PrivateKey) { p.Signature = ed25519.Sign(key, proofMessage(*p)) }
func VerifyProof(p Proof, a Authorization, d Descriptor, trust TrustProfile, expected string, rb ReadBack, now time.Time) error {
	if err := authenticateDescriptorContract(d, trust); err != nil {
		return err
	}
	if err := validateAuthorizationContract(a); err != nil {
		return err
	}
	c := a.Context
	if p.AuthorizationID != a.ID || p.AuthorizationDigest != authorizationDigest(a) || p.WorkloadID != c.WorkloadID || p.ProviderID != c.ProviderID || p.Audience != c.Audience || p.Challenge != c.Challenge || p.PolicyDigest != c.PolicyDigest {
		return errors.New("proof binding mismatch")
	}
	if p.Epoch != d.Epoch || p.KeyID != d.KeyID || now.Before(p.IssuedAt) || !now.Before(p.ExpiresAt) {
		return errors.New("stale or revoked proof")
	}
	if expected == "" || p.ActivationID != expected || rb.ActivationID != expected || rb.WorkloadID != c.WorkloadID || rb.PolicyDigest != c.PolicyDigest || p.ReadBackDigest != readbackDigest(rb) {
		return errors.New("independent readback mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(d.PublicKey), proofMessage(p), p.Signature) {
		return errors.New("invalid proof signature")
	}
	return nil
}
func stableID(prefix string, parts ...string) string {
	return prefix + strings.TrimPrefix(hashTranscript("orka.enforcement.id/v2", func(e *canonicalEncoder) { e.text(10, prefix); e.field(11, encodeStrings(parts)) }), "sha256:")[:32]
}
func sameBytes(a, b []byte) bool { return bytes.Equal(a, b) }
func ParseTrustProfileJSON(data []byte) (TrustProfile, error) {
	if len(data) > maxTrustProfileBytes {
		return TrustProfile{}, errors.New("provider trust profile exceeds size limit")
	}
	var w struct {
		ProviderID   string `json:"providerId"`
		EndpointID   string `json:"endpointId"`
		Algorithm    string `json:"algorithm"`
		KeyID        string `json:"keyId"`
		PublicKey    string `json:"publicKey"`
		MinimumEpoch uint64 `json:"minimumEpoch"`
	}
	if err := rejectDuplicateJSONKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return TrustProfile{}, fmt.Errorf("decode provider trust: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&w); err != nil {
		return TrustProfile{}, fmt.Errorf("decode provider trust: %w", err)
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return TrustProfile{}, errors.New("decode provider trust: trailing data")
	}
	key, err := base64.StdEncoding.DecodeString(w.PublicKey)
	if err != nil {
		return TrustProfile{}, fmt.Errorf("decode provider trust key: %w", err)
	}
	r := TrustProfile{w.ProviderID, w.EndpointID, w.Algorithm, w.KeyID, key, w.MinimumEpoch}
	if r.ProviderID == "" || r.EndpointID == "" || r.Algorithm != "ed25519" || r.KeyID == "" || len(r.PublicKey) != ed25519.PublicKeySize {
		return TrustProfile{}, errors.New("invalid provider trust profile")
	}
	return r, nil
}
