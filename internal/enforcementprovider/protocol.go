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
	"sort"
	"strings"
	"time"
)

var policyDomain = []byte("agentsuite-effective-policy\x00v1\x00")

func PolicyDigest(b []byte) string {
	h := sha256.New()
	h.Write(policyDomain)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	h.Write(n[:])
	h.Write(b)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
func descriptorMessage(d Descriptor) []byte {
	parts := []string{ProtocolVersion, d.ProviderID, d.EndpointID, d.Algorithm, d.KeyID, base64.StdEncoding.EncodeToString(d.PublicKey), fmt.Sprint(d.Epoch), d.IssuedAt.UTC().Format(time.RFC3339Nano), d.ExpiresAt.UTC().Format(time.RFC3339Nano), strings.Join(d.ProtocolVersions, ","), strings.Join(d.Capabilities, ",")}
	return []byte(strings.Join(parts, "\x00"))
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
	seen := make(map[string]bool, len(required))
	for _, capability := range required {
		if capability == "" || strings.TrimSpace(capability) != capability {
			return errors.New("required capability is invalid")
		}
		if seen[capability] {
			return fmt.Errorf("duplicate required capability %q", capability)
		}
		seen[capability] = true
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
func equalStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validatePolicyCapabilityContract(policy []byte, required []string) error {
	// Orka keeps policy bytes opaque for execution, but validates the published
	// AgentSuite v1 envelope so callers cannot omit or alter its mandatory set.
	var envelope struct {
		Version              string   `json:"version"`
		DenyWritePaths       []string `json:"denyWritePaths"`
		RequiredCapabilities []string `json:"requiredCapabilities"`
	}
	decoder := json.NewDecoder(bytes.NewReader(policy))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return fmt.Errorf("invalid effective policy envelope: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("effective policy has trailing data")
	}
	if envelope.Version != "agentsuite.policy/v1" || len(envelope.DenyWritePaths) == 0 {
		return errors.New("unsupported or empty effective policy")
	}
	if !equalStringSlice(envelope.RequiredCapabilities, required) {
		return errors.New("authorization capabilities differ from effective policy")
	}
	if len(required) != 1 || required[0] != "filesystem/deny-write/v1" {
		return errors.New("effective policy capability contract is unsupported")
	}
	return nil
}
func validateAuthorizationContract(a Authorization) error {
	c := a.Context
	if err := validateRequiredCapabilities(c.RequiredCapabilities); err != nil {
		return err
	}
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
func readbackDigest(r ReadBack) string {
	parts := []string{r.ActivationID, r.WorkloadID, r.PolicyDigest, r.ConfigurationDigest}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(h[:])
}
func proofMessage(p Proof) []byte {
	parts := []string{ProtocolVersion, p.ID, p.AuthorizationID, p.ActivationID, p.WorkloadID, p.ProviderID, p.Audience, p.Challenge, p.PolicyDigest, p.ReadBackDigest, p.KeyID, fmt.Sprint(p.Epoch), p.IssuedAt.UTC().Format(time.RFC3339Nano), p.ExpiresAt.UTC().Format(time.RFC3339Nano)}
	return []byte(strings.Join(parts, "\x00"))
}
func SignProof(p *Proof, key ed25519.PrivateKey) { p.Signature = ed25519.Sign(key, proofMessage(*p)) }
func VerifyProof(p Proof, a Authorization, d Descriptor, trust TrustProfile, expectedActivationID string, rb ReadBack, now time.Time) error {
	if err := authenticateDescriptorContract(d, trust); err != nil {
		return err
	}
	if err := validateAuthorizationContract(a); err != nil {
		return err
	}
	c := a.Context
	if p.AuthorizationID != a.ID || p.WorkloadID != c.WorkloadID || p.ProviderID != c.ProviderID || p.Audience != c.Audience || p.Challenge != c.Challenge || p.PolicyDigest != c.PolicyDigest {
		return errors.New("proof binding mismatch")
	}
	if p.Epoch != d.Epoch || p.KeyID != d.KeyID || now.Before(p.IssuedAt) || !now.Before(p.ExpiresAt) {
		return errors.New("stale or revoked proof")
	}
	if expectedActivationID == "" || p.ActivationID != expectedActivationID || rb.ActivationID != expectedActivationID || rb.WorkloadID != c.WorkloadID || rb.PolicyDigest != c.PolicyDigest || p.ReadBackDigest != readbackDigest(rb) {
		return errors.New("independent readback mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(d.PublicKey), proofMessage(p), p.Signature) {
		return errors.New("invalid proof signature")
	}
	return nil
}
func stableID(prefix string, parts ...string) string {
	sort.Strings(parts[:0])
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(h[:16])
}
func sameBytes(a, b []byte) bool { return bytes.Equal(a, b) }

// ParseTrustProfileJSON loads operator-controlled trust. PublicKey is base64.
func ParseTrustProfileJSON(data []byte) (TrustProfile, error) {
	var wire struct {
		ProviderID   string `json:"providerId"`
		EndpointID   string `json:"endpointId"`
		Algorithm    string `json:"algorithm"`
		KeyID        string `json:"keyId"`
		PublicKey    string `json:"publicKey"`
		MinimumEpoch uint64 `json:"minimumEpoch"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return TrustProfile{}, fmt.Errorf("decode provider trust: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(wire.PublicKey)
	if err != nil {
		return TrustProfile{}, fmt.Errorf("decode provider trust key: %w", err)
	}
	profile := TrustProfile{ProviderID: wire.ProviderID, EndpointID: wire.EndpointID, Algorithm: wire.Algorithm, KeyID: wire.KeyID, PublicKey: key, MinimumEpoch: wire.MinimumEpoch}
	if profile.ProviderID == "" || profile.EndpointID == "" || profile.Algorithm != "ed25519" || profile.KeyID == "" || len(profile.PublicKey) != ed25519.PublicKeySize {
		return TrustProfile{}, errors.New("invalid provider trust profile")
	}
	return profile, nil
}
