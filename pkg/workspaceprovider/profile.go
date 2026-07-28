package workspaceprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

// ClassProfileHash returns the deterministic SHA-256 digest pinned into a
// concrete workspace class binding. Resolved values should include immutable
// identity and functional provider-profile or pool data, but never status or
// credentials.
func ClassProfileHash(
	spec workspacev1alpha1.ExecutionWorkspaceClassSpec,
	resolved ...any,
) (string, error) {
	canonical := *spec.DeepCopy()
	slices.Sort(canonical.RequiredFeatures)
	slices.Sort(canonical.AllowedReuseScopes)
	slices.Sort(canonical.Lifecycle.AllowedOnDetach)
	data, err := json.Marshal(struct {
		Spec     workspacev1alpha1.ExecutionWorkspaceClassSpec `json:"spec"`
		Resolved []any                                         `json:"resolved,omitempty"`
	}{Spec: canonical, Resolved: resolved})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
