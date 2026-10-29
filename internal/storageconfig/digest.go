package storageconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// digestDomain separates configuration digests from every other SHA-256 Orka
// computes. Changing the canonical encoding requires a new domain.
const digestDomain = "orka.storage.configuration.v1\x00"

// canonicalConfig is the digest input: bindings in Contracts order, encoded
// as JSON with fixed field order. Source kind and file path are excluded.
type canonicalConfig struct {
	Bindings []Binding `json:"bindings"`
}

func computeDigest(bindings map[string]Binding) (string, error) {
	canonical := canonicalConfig{Bindings: make([]Binding, 0, len(Contracts))}
	for _, contract := range Contracts {
		binding, ok := bindings[contract]
		if !ok {
			return "", invalidf("contract %s is not bound", contract)
		}
		canonical.Bindings = append(canonical.Bindings, binding)
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode storage configuration digest input: %w", err)
	}
	sum := sha256.Sum256(append([]byte(digestDomain), encoded...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
