package artifactcap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const requestDigestDomain = "orka.acp.artifact-request.v1\x00"

var objectDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ValidateObjectDigest(value string) error {
	if !objectDigestPattern.MatchString(value) {
		return fmt.Errorf("%w: object digest must be lowercase sha256", ErrInvalidRequest)
	}
	return nil
}

func DigestHex(value string) (string, error) {
	if err := ValidateObjectDigest(value); err != nil {
		return "", err
	}
	return value[len("sha256:"):], nil
}

func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func RequestDigest(request OperationRequest) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	binding := struct {
		Version string           `json:"version"`
		Method  string           `json:"method"`
		Path    string           `json:"path"`
		Request OperationRequest `json:"request"`
	}{
		Version: CapabilityVersion,
		Method:  request.Method(),
		Path:    request.Path(),
		Request: request,
	}
	canonical, err := harnessv2.CanonicalValue(binding)
	if err != nil {
		return "", fmt.Errorf("canonicalize artifact request: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(requestDigestDomain))
	_, _ = hash.Write(canonical)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func IsRequestDigest(value string) bool { return objectDigestPattern.MatchString(value) }
