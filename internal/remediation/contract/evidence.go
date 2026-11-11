package contract

import "encoding/json"

// EvidenceScope identifies the execution boundary, not a successful outcome.
// Neither a report's coverage claims nor a model's verdict establish this scope.
type EvidenceScope string

const (
	SourceHarness      EvidenceScope = "source-harness"
	ApplicationRuntime EvidenceScope = "application-runtime"

	// EvidenceScopeKey binds backend-selected scope into the execution manifest
	// and therefore its input, environment, and observation digests.
	EvidenceScopeKey = "orka.remediation.evidence-scope"
)

func (scope EvidenceScope) Valid() bool {
	return scope == SourceHarness || scope == ApplicationRuntime
}

func (scope *EvidenceScope) UnmarshalJSON(content []byte) error {
	var value *string
	if json.Unmarshal(content, &value) != nil || value == nil || !EvidenceScope(*value).Valid() {
		return ErrInvalid
	}
	*scope = EvidenceScope(*value)
	return nil
}

// RequireEvidenceScope defaults only an omitted request requirement. An absent
// backend capability or retained evidence label is never runtime authority.
func RequireEvidenceScope(required, supported EvidenceScope) error {
	if required == "" {
		required = SourceHarness
	}
	if !required.Valid() {
		return ErrInvalid
	}
	if !supported.Valid() || required != supported {
		return ErrEvidenceScopeUnsupported
	}
	return nil
}

// EvidenceScopedBackend is required for new admission. Keeping cleanup on the
// original interface lets an unscoped backend still stop its exact owned work.
type EvidenceScopedBackend interface {
	ExecutionBackend
	EvidenceScope() EvidenceScope
}
