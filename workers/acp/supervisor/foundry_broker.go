package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	foundryBrokerProtocol       = "orka.foundry.broker.v1"
	foundryContextHeader        = "X-Orka-Foundry-Context"
	foundryCleanupTimeout       = 30 * time.Second
	foundryControlResponseLimit = 32 << 10
)

var errFoundryCleanupUnproven = errors.New("remote Foundry execution cleanup could not be proven")

// foundryBrokerContext is constructed solely from authenticated controller
// requests. The ACP child cannot select an owner, target, lease, or operation.
// Keep the wire contract in sync with agent-runtime-foundry/broker_protocol.go.
type foundryBrokerContext struct {
	Protocol                 string                  `json:"protocol"`
	Owner                    harnessv2.Fence         `json:"owner"`
	AgentConfigurationDigest string                  `json:"agentConfigurationDigest"`
	TaskUID                  harnessv2.TaskUID       `json:"taskUID,omitempty"`
	TaskAttempt              uint32                  `json:"taskAttempt,omitempty"`
	PromptID                 harnessv2.PromptID      `json:"promptID,omitempty"`
	PromptRequestDigest      harnessv2.RequestDigest `json:"promptRequestDigest,omitempty"`
	LeaseGeneration          uint64                  `json:"leaseGeneration,omitempty"`
	LeaseExpiresAt           *time.Time              `json:"leaseExpiresAt,omitempty"`
	OperationID              string                  `json:"operationID"`
	InvocationSequence       uint64                  `json:"invocationSequence,omitempty"`
	BodySHA256               string                  `json:"bodySHA256"`
}

type foundryBrokerProof struct {
	Protocol             string `json:"protocol"`
	OwnerDigest          string `json:"ownerDigest"`
	OperationID          string `json:"operationID"`
	ContextSHA256        string `json:"contextSHA256"`
	State                string `json:"state"`
	SettlementProven     bool   `json:"settlementProven"`
	RetirementProven     bool   `json:"retirementProven"`
	ActiveInvocations    uint64 `json:"activeInvocations"`
	AmbiguousInvocations uint64 `json:"ambiguousInvocations"`
	CreatePending        bool   `json:"createPending"`
	// Historical creation evidence remains true after confirmed deletion. The
	// broker's retired state and proof, not this flag, establish retirement.
	RemoteSessionCreated bool   `json:"remoteSessionCreated"`
	LeaseGeneration      uint64 `json:"leaseGeneration"`
	LeaseExpiresAt       string `json:"leaseExpiresAt"`
	ProofDigest          string `json:"proofDigest"`
}

type foundryBrokerPrompt struct {
	metadata         harnessv2.MutationMetadata
	lease            harnessv2.PromptLease
	closed           bool
	settleContext    []byte
	settleDone       chan struct{}
	settleErr        error
	settlementProven bool
}

type foundryBrokerSession struct {
	proxy               *providerProxy
	owner               harnessv2.Fence
	ownerDigest         string
	configurationDigest string

	mu               sync.Mutex
	prompt           *foundryBrokerPrompt
	retiring         bool
	retireContext    []byte
	retireDone       chan struct{}
	retireErr        error
	retirementProven bool
}

func foundrySHA256(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func (p *providerProxy) newSessionForRequest(request harnessv2.CreateRuntimeSessionRequest) (*providerProxySession, ProviderProxyBinding, error) {
	if p.providerKind == providerKindFoundry {
		if err := request.Metadata.Fence.Validate(true); err != nil {
			return nil, ProviderProxyBinding{}, fmt.Errorf("external Foundry sessions require an exact runtime-session fence")
		}
		if err := harnessv2.ValidateProfileDigest(harnessv2.ProfileDigest(request.Profile.AgentConfigurationDigest)); err != nil {
			return nil, ProviderProxyBinding{}, fmt.Errorf("external Foundry sessions require a frozen agent configuration digest")
		}
	}
	session, binding, err := p.newSession()
	if err != nil || p.providerKind != providerKindFoundry {
		return session, binding, err
	}
	owner, err := json.Marshal(request.Metadata.Fence)
	if err != nil {
		session.close()
		return nil, ProviderProxyBinding{}, err
	}
	session.foundry = &foundryBrokerSession{
		proxy: p, owner: request.Metadata.Fence, ownerDigest: foundrySHA256(owner),
		configurationDigest: request.Profile.AgentConfigurationDigest,
	}
	return session, binding, nil
}

func (s *providerProxySession) activatePrompt(request harnessv2.StartPromptRequest, maxTurns int32, now time.Time) error {
	if err := s.activateWithMaxTurns(string(request.Metadata.PromptID), maxTurns, request.Lease.ExpiresAt, now); err != nil {
		return err
	}
	if s.foundry != nil {
		if err := s.foundry.beginPrompt(request.Metadata, request.Lease); err != nil {
			s.deactivate(string(request.Metadata.PromptID))
			return err
		}
	}
	return nil
}

func (s *foundryBrokerSession) beginPrompt(metadata harnessv2.MutationMetadata, lease harnessv2.PromptLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiring || (s.prompt != nil && !s.prompt.settlementProven) {
		return errFoundryCleanupUnproven
	}
	if harnessv2.CompareFence(s.owner, metadata.Fence, true) != harnessv2.FenceMatch || metadata.PromptID == "" || metadata.TaskUID == "" || metadata.TaskAttempt == 0 {
		return fmt.Errorf("remote Foundry prompt owner does not match the runtime session")
	}
	s.prompt = &foundryBrokerPrompt{metadata: metadata, lease: lease}
	return nil
}

func (s *foundryBrokerSession) contextFor(path string, prompt *foundryBrokerPrompt, sequence uint64, body []byte) ([]byte, error) {
	value := foundryBrokerContext{
		Protocol: foundryBrokerProtocol, Owner: s.owner, AgentConfigurationDigest: s.configurationDigest,
		InvocationSequence: sequence, BodySHA256: foundrySHA256(body),
	}
	if prompt != nil {
		value.TaskUID = prompt.metadata.TaskUID
		value.TaskAttempt = prompt.metadata.TaskAttempt
		value.PromptID = prompt.metadata.PromptID
		value.PromptRequestDigest = prompt.metadata.RequestDigest
		value.LeaseGeneration = prompt.lease.Generation
		expires := prompt.lease.ExpiresAt
		value.LeaseExpiresAt = &expires
	}
	identity, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	value.OperationID = fmt.Sprintf("foundry-%x", sha256.Sum256(append([]byte(path+"\n"), identity...)))
	return json.Marshal(value)
}

func (s *foundryBrokerSession) inferenceContext(promptID string, sequence uint64, body []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiring || s.prompt == nil || s.prompt.closed || string(s.prompt.metadata.PromptID) != promptID || sequence == 0 || !time.Now().Before(s.prompt.lease.ExpiresAt) {
		return "", fmt.Errorf("remote Foundry inference lease is not active")
	}
	value, err := s.contextFor("/v1/responses", s.prompt, sequence, body)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// renew is called outside the supervisor mutex. The broker acknowledges the
// remote lease before the supervisor publishes a successful local renewal.
func (s *foundryBrokerSession) renew(ctx context.Context, promptID string, lease harnessv2.PromptLease) error {
	s.mu.Lock()
	prompt := s.prompt
	if s.retiring || prompt == nil || prompt.closed || string(prompt.metadata.PromptID) != promptID {
		s.mu.Unlock()
		return fmt.Errorf("remote Foundry prompt is no longer active")
	}
	candidate := *prompt
	candidate.lease = lease
	value, err := s.contextFor("/internal/v1/renew", &candidate, 0, []byte("{}"))
	s.mu.Unlock()
	if err != nil {
		return err
	}
	proof, err := s.control(ctx, "/internal/v1/renew", value)
	acknowledgedExpiry, expiryErr := time.Parse(time.RFC3339Nano, proof.LeaseExpiresAt)
	if err != nil || expiryErr != nil || proof.State != "open" || proof.LeaseGeneration != lease.Generation || !acknowledgedExpiry.Equal(lease.ExpiresAt) {
		return fmt.Errorf("remote Foundry lease renewal was not proven")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prompt != prompt || prompt.closed || s.retiring {
		return fmt.Errorf("remote Foundry prompt closed during lease renewal")
	}
	if lease.Generation <= prompt.lease.Generation {
		if lease == prompt.lease {
			return nil
		}
		return fmt.Errorf("remote Foundry lease renewal is stale")
	}
	prompt.lease = lease
	return nil
}

// startSettlement closes local admission synchronously; cleanup itself uses a
// bounded independent context so an abandoned ACP connection cannot cancel it.
func (s *foundryBrokerSession) startSettlement(promptID string) (*foundryBrokerPrompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prompt := s.prompt
	if prompt == nil || string(prompt.metadata.PromptID) != promptID {
		return nil, fmt.Errorf("remote Foundry settlement prompt does not match")
	}
	prompt.closed = true
	if prompt.settlementProven {
		return prompt, nil
	}
	if prompt.settleDone != nil {
		select {
		case <-prompt.settleDone:
			// A failed observation is not a proof. A later cleanup request
			// may retry the same idempotent control operation.
			prompt.settleDone = nil
		default:
			return prompt, nil
		}
	}
	if len(prompt.settleContext) == 0 {
		var err error
		prompt.settleContext, err = s.contextFor("/internal/v1/settle", prompt, 0, []byte("{}"))
		if err != nil {
			return nil, err
		}
	}
	prompt.settleDone = make(chan struct{})
	done := prompt.settleDone
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), foundryCleanupTimeout)
		defer cancel()
		proof, err := s.control(ctx, "/internal/v1/settle", prompt.settleContext)
		if err == nil && (!proof.SettlementProven || (proof.State != "settled" && proof.State != "retired") || !proof.quiescent() || !validFoundryProofDigest(proof.ProofDigest)) {
			err = errFoundryCleanupUnproven
		}
		s.mu.Lock()
		prompt.settleErr = err
		prompt.settlementProven = err == nil
		close(done)
		s.mu.Unlock()
	}()
	return prompt, nil
}

func (s *foundryBrokerSession) settle(ctx context.Context, promptID string) error {
	prompt, err := s.startSettlement(promptID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	done := prompt.settleDone
	proven := prompt.settlementProven
	s.mu.Unlock()
	if proven {
		return nil
	}
	select {
	case <-ctx.Done():
		return errFoundryCleanupUnproven
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return prompt.settleErr
	}
}

func (s *foundryBrokerSession) retire(ctx context.Context) error {
	s.mu.Lock()
	s.retiring = true
	if s.prompt != nil {
		s.prompt.closed = true
	}
	if s.retirementProven {
		s.mu.Unlock()
		return nil
	}
	if s.retireDone != nil {
		select {
		case <-s.retireDone:
			s.retireDone = nil
		default:
		}
	}
	if s.retireDone == nil {
		var err error
		s.retireContext, err = s.contextFor("/internal/v1/retire", nil, 0, []byte("{}"))
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.retireDone = make(chan struct{})
		done := s.retireDone
		go func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), foundryCleanupTimeout)
			defer cancel()
			proof, err := s.control(cleanupCtx, "/internal/v1/retire", s.retireContext)
			if err == nil && (!proof.RetirementProven || !proof.SettlementProven || proof.State != "retired" || !proof.quiescent() || !validFoundryProofDigest(proof.ProofDigest)) {
				err = errFoundryCleanupUnproven
			}
			s.mu.Lock()
			s.retireErr = err
			s.retirementProven = err == nil
			close(done)
			s.mu.Unlock()
		}()
	}
	done := s.retireDone
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return errFoundryCleanupUnproven
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.retireErr
	}
}

func (p foundryBrokerProof) quiescent() bool {
	return p.ActiveInvocations == 0 && p.AmbiguousInvocations == 0 && !p.CreatePending
}

func validFoundryProofDigest(value string) bool {
	return harnessv2.ValidateProfileDigest(harnessv2.ProfileDigest(value)) == nil
}

func (s *foundryBrokerSession) control(ctx context.Context, path string, value []byte) (foundryBrokerProof, error) {
	var identity foundryBrokerContext
	if err := json.Unmarshal(value, &identity); err != nil {
		return foundryBrokerProof{}, errFoundryCleanupUnproven
	}
	target := *s.proxy.upstreamBase
	target.Path, target.RawPath, target.RawQuery = path, "", ""
	for {
		proof, status, err := s.controlOnce(ctx, &target, value, identity.OperationID)
		if err != nil || status != http.StatusConflict {
			return proof, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return foundryBrokerProof{}, errFoundryCleanupUnproven
		case <-timer.C:
		}
	}
}

func (s *foundryBrokerSession) controlOnce(ctx context.Context, target *url.URL, value []byte, operationID string) (foundryBrokerProof, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader([]byte("{}")))
	if err != nil {
		return foundryBrokerProof{}, 0, errFoundryCleanupUnproven
	}
	request.Header.Set(providerAuthorizationHeader, "Bearer "+string(s.proxy.upstreamToken))
	request.Header.Set(foundryContextHeader, base64.RawURLEncoding.EncodeToString(value))
	request.Header.Set("Content-Type", "application/json")
	response, err := s.proxy.client.Do(request)
	if err != nil {
		return foundryBrokerProof{}, 0, errFoundryCleanupUnproven
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusConflict {
		return foundryBrokerProof{}, response.StatusCode, errFoundryCleanupUnproven
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, foundryControlResponseLimit+1))
	if err != nil || len(data) > foundryControlResponseLimit {
		return foundryBrokerProof{}, response.StatusCode, errFoundryCleanupUnproven
	}
	var proof foundryBrokerProof
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proof); err != nil || ensureProviderJSONEOF(decoder) != nil {
		return foundryBrokerProof{}, response.StatusCode, errFoundryCleanupUnproven
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return foundryBrokerProof{}, response.StatusCode, errFoundryCleanupUnproven
	}
	for _, required := range []string{"settlementProven", "retirementProven", "activeInvocations", "ambiguousInvocations", "createPending", "remoteSessionCreated"} {
		if raw, ok := fields[required]; !ok || bytes.Equal(raw, []byte("null")) {
			return foundryBrokerProof{}, response.StatusCode, errFoundryCleanupUnproven
		}
	}
	if proof.Protocol != foundryBrokerProtocol || proof.OwnerDigest != s.ownerDigest || proof.OperationID != operationID || proof.ContextSHA256 != foundrySHA256(value) {
		return foundryBrokerProof{}, response.StatusCode, errFoundryCleanupUnproven
	}
	return proof, response.StatusCode, nil
}
