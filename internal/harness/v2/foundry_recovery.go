package v2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	FoundryBrokerProtocol          = "orka.foundry.broker.v1"
	FoundryBootRetirementPath      = "/v2/recovery/foundry/retire-boot"
	FoundryBootRetirementOperation = "retire_foundry_boot"
)

// FoundryBrokerIdentity is obtained through the authenticated supervisor before
// admission. A new or replaced ledger cannot prove cleanup of an older ledger.
type FoundryBrokerIdentity struct {
	Protocol                 string `json:"protocol"`
	LedgerIdentityDigest     string `json:"ledgerIdentityDigest"`
	AgentConfigurationDigest string `json:"agentConfigurationDigest"`
}

func (i FoundryBrokerIdentity) Validate() error {
	if i.Protocol != FoundryBrokerProtocol {
		return fmt.Errorf("unsupported Foundry broker recovery protocol")
	}
	if err := validateSHA256Digest(i.LedgerIdentityDigest); err != nil {
		return fmt.Errorf("foundry broker ledger identity: %w", err)
	}
	if err := validateSHA256Digest(i.AgentConfigurationDigest); err != nil {
		return fmt.Errorf("foundry broker configuration identity: %w", err)
	}
	return nil
}

// FoundryBootRetirementRequest uses the current supervisor only as an
// authenticated relay. RetiredFence identifies a different, previously
// witnessed boot; it never grants that boot admission or execution authority.
type FoundryBootRetirementRequest struct {
	Protocol     string                `json:"protocol"`
	Metadata     MutationMetadata      `json:"metadata"`
	RetiredFence Fence                 `json:"retiredFence"`
	Broker       FoundryBrokerIdentity `json:"broker"`
}

func (r FoundryBootRetirementRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.ValidateAt(now); err != nil {
		return err
	}
	if r.Metadata.Fence.RuntimeSessionUID != "" || r.Metadata.Fence.RuntimeSessionGeneration != 0 ||
		r.Metadata.TaskUID != "" || r.Metadata.TaskAttempt != 0 || r.Metadata.PromptID != "" {
		return fmt.Errorf("foundry boot recovery requires pool-wide relay authority")
	}
	if err := r.RetiredFence.Validate(false); err != nil {
		return fmt.Errorf("retired Foundry boot: %w", err)
	}
	if r.RetiredFence.RuntimeSessionUID != "" || r.RetiredFence.RuntimeSessionGeneration != 0 {
		return fmt.Errorf("foundry boot recovery cannot certify a single session as a whole boot")
	}
	relay := r.Metadata.Fence
	if r.RetiredFence.SupervisorBootID == relay.SupervisorBootID ||
		r.RetiredFence.ControllerEpoch > relay.ControllerEpoch ||
		r.RetiredFence.RuntimeInstanceID != relay.RuntimeInstanceID ||
		r.RetiredFence.RuntimePoolUID != relay.RuntimePoolUID ||
		r.RetiredFence.RuntimePoolGeneration != relay.RuntimePoolGeneration ||
		r.RetiredFence.RuntimeProfileDigest != relay.RuntimeProfileDigest {
		return fmt.Errorf("foundry recovery subject does not match an earlier boot of the relay's runtime")
	}
	if err := r.Broker.Validate(); err != nil {
		return err
	}
	return r.Metadata.ValidateDigest(r)
}

// BrokerRequestBody is the exact broker wire representation. Its byte digest
// is echoed in the authenticated proof, independently of the v2 request digest.
func (r FoundryBootRetirementRequest) BrokerRequestBody() ([]byte, error) {
	return json.Marshal(struct {
		Protocol                 string `json:"protocol"`
		LedgerIdentityDigest     string `json:"ledgerIdentityDigest"`
		AgentConfigurationDigest string `json:"agentConfigurationDigest"`
		RetiredFence             Fence  `json:"retiredFence"`
		OperationID              string `json:"operationID"`
	}{r.Broker.Protocol, r.Broker.LedgerIdentityDigest, r.Broker.AgentConfigurationDigest,
		r.RetiredFence, string(r.Metadata.OperationID)})
}

// FoundryBootRetirementProof certifies the broker's permanent admission seal
// and every owner under the retired boot. It is not a prompt outcome receipt.
type FoundryBootRetirementProof struct {
	Protocol                 string `json:"protocol"`
	LedgerIdentityDigest     string `json:"ledgerIdentityDigest"`
	AgentConfigurationDigest string `json:"agentConfigurationDigest"`
	OperationID              string `json:"operationID"`
	ContextSHA256            string `json:"contextSHA256"`
	RetiredFenceDigest       string `json:"retiredFenceDigest"`
	State                    string `json:"state"`
	Sealed                   bool   `json:"sealed"`
	SettlementProven         bool   `json:"settlementProven"`
	RetirementProven         bool   `json:"retirementProven"`
	OwnerCount               uint64 `json:"ownerCount"`
	OwnerSetDigest           string `json:"ownerSetDigest"`
	ActiveInvocations        uint64 `json:"activeInvocations"`
	AmbiguousInvocations     uint64 `json:"ambiguousInvocations"`
	PendingCreates           uint64 `json:"pendingCreates"`
	ProofDigest              string `json:"proofDigest"`
}

func (p *FoundryBootRetirementProof) UnmarshalJSON(data []byte) error {
	if _, err := CanonicalJSON(data); err != nil {
		return err
	}
	type wireProof FoundryBootRetirementProof
	var value wireProof
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"protocol", "ledgerIdentityDigest", "agentConfigurationDigest", "operationID", "contextSHA256",
		"retiredFenceDigest", "state", "sealed", "settlementProven", "retirementProven", "ownerCount", "ownerSetDigest",
		"activeInvocations", "ambiguousInvocations", "pendingCreates", "proofDigest"} {
		if raw, ok := fields[name]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("foundry retirement proof is missing %s", name)
		}
	}
	*p = FoundryBootRetirementProof(value)
	return nil
}

func (p FoundryBootRetirementProof) ValidateFor(r FoundryBootRetirementRequest) error {
	if err := r.Broker.Validate(); err != nil {
		return err
	}
	if p.Protocol != r.Broker.Protocol || p.LedgerIdentityDigest != r.Broker.LedgerIdentityDigest ||
		p.AgentConfigurationDigest != r.Broker.AgentConfigurationDigest || p.OperationID != string(r.Metadata.OperationID) {
		return fmt.Errorf("foundry retirement proof authority does not match the request")
	}
	body, err := r.BrokerRequestBody()
	if err != nil {
		return err
	}
	fence, err := json.Marshal(r.RetiredFence)
	if err != nil {
		return err
	}
	if p.ContextSHA256 != fmt.Sprintf("sha256:%x", sha256.Sum256(body)) ||
		p.RetiredFenceDigest != fmt.Sprintf("sha256:%x", sha256.Sum256(fence)) {
		return fmt.Errorf("foundry retirement proof does not bind the exact retired boot request")
	}
	if p.State != "retired" || !p.Sealed || !p.SettlementProven || !p.RetirementProven ||
		p.ActiveInvocations != 0 || p.AmbiguousInvocations != 0 || p.PendingCreates != 0 {
		return fmt.Errorf("foundry boot retirement is incomplete")
	}
	if err := validateSHA256Digest(p.OwnerSetDigest); err != nil {
		return fmt.Errorf("foundry retirement owner set: %w", err)
	}
	if p.OwnerCount == 0 && p.OwnerSetDigest != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("[]"))) {
		return fmt.Errorf("foundry empty owner set digest is inconsistent")
	}
	if p.ProofDigest != p.CanonicalProofDigest() {
		return fmt.Errorf("foundry retirement proof digest does not match its evidence")
	}
	return nil
}

// CanonicalProofDigest binds the durable retirement evidence independently of
// the relay operation identity. A fresh retry may prove the same sealed boot.
func (p FoundryBootRetirementProof) CanonicalProofDigest() string {
	body, _ := json.Marshal(struct {
		Protocol                 string `json:"protocol"`
		LedgerIdentityDigest     string `json:"ledgerIdentityDigest"`
		AgentConfigurationDigest string `json:"agentConfigurationDigest"`
		RetiredFenceDigest       string `json:"retiredFenceDigest"`
		State                    string `json:"state"`
		Sealed                   bool   `json:"sealed"`
		SettlementProven         bool   `json:"settlementProven"`
		RetirementProven         bool   `json:"retirementProven"`
		OwnerCount               uint64 `json:"ownerCount"`
		OwnerSetDigest           string `json:"ownerSetDigest"`
		ActiveInvocations        uint64 `json:"activeInvocations"`
		AmbiguousInvocations     uint64 `json:"ambiguousInvocations"`
		PendingCreates           uint64 `json:"pendingCreates"`
	}{p.Protocol, p.LedgerIdentityDigest, p.AgentConfigurationDigest, p.RetiredFenceDigest,
		p.State, p.Sealed, p.SettlementProven, p.RetirementProven, p.OwnerCount,
		p.OwnerSetDigest, p.ActiveInvocations, p.AmbiguousInvocations, p.PendingCreates})
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body))
}

type FoundryBootRetirementResponse struct {
	Protocol       string                     `json:"protocol"`
	Classification Classification             `json:"classification"`
	RelayFence     Fence                      `json:"relayFence"`
	RequestDigest  RequestDigest              `json:"requestDigest"`
	Proof          FoundryBootRetirementProof `json:"proof"`
}

func (r FoundryBootRetirementResponse) ValidateFor(request FoundryBootRetirementRequest) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return err
	}
	if r.Classification.Class != RequestClassificationFresh && r.Classification.Class != RequestClassificationDuplicate {
		return fmt.Errorf("foundry recovery did not return a successful operation classification")
	}
	if r.RelayFence != request.Metadata.Fence || r.RequestDigest != request.Metadata.RequestDigest {
		return fmt.Errorf("foundry recovery response does not match the exact relay request")
	}
	return r.Proof.ValidateFor(request)
}

func (c *Client) RetireFoundryBoot(ctx context.Context, request FoundryBootRetirementRequest) (*FoundryBootRetirementResponse, error) {
	if err := request.ValidateAt(time.Now().UTC()); err != nil {
		return nil, c.validationError(FoundryBootRetirementOperation, err)
	}
	var response FoundryBootRetirementResponse
	if err := c.mutateJSON(ctx, FoundryBootRetirementOperation, http.MethodPut, FoundryBootRetirementPath,
		request.Metadata, request, &response); err != nil {
		return nil, err
	}
	if err := response.ValidateFor(request); err != nil {
		return nil, c.protocolError(FoundryBootRetirementOperation, 0, err)
	}
	return &response, nil
}
