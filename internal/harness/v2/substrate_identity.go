package v2

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const WorkspaceBootstrapPublicKeyEnv = "ORKA_WORKSPACE_BOOTSTRAP_PUBLIC_KEY"

type WorkspaceBootstrapRequest struct {
	HandoffToken string `json:"handoffToken"`
	Recover      bool   `json:"recover,omitempty"`
}

func WorkspaceBootstrapSigningSeed(secret string) []byte {
	sum := sha256.Sum256([]byte("orka.workspace-bootstrap/v1\x00" + secret))
	return sum[:]
}

func WorkspaceBootstrapPublicKey(secret string) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("workspace bootstrap signing secret is required")
	}
	return CredentialBootstrapPublicKey(WorkspaceBootstrapSigningSeed(secret))
}

func WorkspaceBootstrapNonce(publicKey string) string {
	sum := sha256.Sum256([]byte("orka.workspace-bootstrap/nonce/v1\x00" + publicKey))
	return hex.EncodeToString(sum[:])
}

// ReadSubstrateActorIdentity reads the provider's read-only SystemInfo projection.
// Absence identifies an ordinary Kubernetes runtime; partial projections fail closed.
func ReadSubstrateActorIdentity(directory string) (*SubstrateActorIdentity, error) {
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.IsDir() {
		return nil, errors.New("substrate identity projection is not a directory")
	}
	identity := &SubstrateActorIdentity{}
	for name, target := range map[string]*string{"atespace": &identity.Atespace, "name": &identity.Name, "uid": &identity.UID} {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 128 {
			return nil, errors.New("substrate identity projection is incomplete or invalid")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("substrate identity projection is unreadable")
		}
		*target = strings.TrimSpace(string(data))
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	return identity, nil
}
