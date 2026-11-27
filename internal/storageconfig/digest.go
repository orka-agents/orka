package storageconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const digestDomain = "orka.storage.configuration.v1\x00"

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
