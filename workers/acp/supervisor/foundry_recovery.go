package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const maxFoundryRecoveryOperations = 128

type foundryRecoveryOperation struct {
	record    harnessv2.OperationRecord
	expiresAt time.Time
}

func (p *providerProxy) foundryRecoveryJSON(ctx context.Context, method, path string, body []byte, into any) error {
	if p == nil || p.providerKind != providerKindFoundry {
		return errFoundryCleanupUnproven
	}
	target := *p.upstreamBase
	target.Path, target.RawPath, target.RawQuery = path, "", ""
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return errFoundryCleanupUnproven
	}
	request.Header.Set(providerAuthorizationHeader, "Bearer "+string(p.upstreamToken))
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return errFoundryCleanupUnproven
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK {
		return errFoundryCleanupUnproven
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, foundryControlResponseLimit+1))
	if err != nil || len(data) > foundryControlResponseLimit {
		return errFoundryCleanupUnproven
	}
	if _, err := harnessv2.CanonicalJSON(data); err != nil {
		return errFoundryCleanupUnproven
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil || ensureProviderJSONEOF(decoder) != nil {
		return errFoundryCleanupUnproven
	}
	return nil
}

func (p *providerProxy) foundryBrokerIdentity(ctx context.Context) (*harnessv2.FoundryBrokerIdentity, error) {
	var identity harnessv2.FoundryBrokerIdentity
	if err := p.foundryRecoveryJSON(ctx, http.MethodGet, "/internal/v1/identity", nil, &identity); err != nil {
		return nil, err
	}
	if err := identity.Validate(); err != nil {
		return nil, errFoundryCleanupUnproven
	}
	return &identity, nil
}

func (s *Server) foundryStatusIdentity(ctx context.Context) (*harnessv2.FoundryBrokerIdentity, error) {
	if !s.cfg.Capabilities.SupportsFoundryRecovery || s.cfg.Provider.Kind != providerKindFoundry {
		return nil, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	identity, err := s.providerProxy.foundryBrokerIdentity(probeCtx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.foundryIdentity == nil {
		if err != nil {
			return nil, errFoundryCleanupUnproven
		}
		value := *identity
		s.foundryIdentity = &value
	} else if err != nil || *identity != *s.foundryIdentity {
		return nil, errFoundryCleanupUnproven
	}
	return identity, nil
}

func (s *Server) classifyFoundryRecovery(request harnessv2.FoundryBootRetirementRequest, now time.Time) (harnessv2.Classification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for operationID, operation := range s.foundryRecoveryOps {
		if now.After(operation.expiresAt.Add(operationReplayRetentionSlack)) {
			delete(s.foundryRecoveryOps, operationID)
		}
	}
	var existing *harnessv2.OperationRecord
	if operation, ok := s.foundryRecoveryOps[request.Metadata.OperationID]; ok {
		existing = &operation.record
	}
	classification, err := harnessv2.ClassifyOperation(s.cfg.Fence, request.Metadata, existing, false, now)
	if err != nil || classification.Class != harnessv2.RequestClassificationFresh {
		return classification, err
	}
	if len(s.foundryRecoveryOps) >= maxFoundryRecoveryOperations {
		return classification, errors.New("foundry recovery replay capacity is full")
	}
	s.foundryRecoveryOps[request.Metadata.OperationID] = foundryRecoveryOperation{
		record: operationRecord(request.Metadata, harnessv2.OperationPhaseRecorded, "", now), expiresAt: request.Metadata.ExpiresAt,
	}
	return classification, nil
}

func (s *Server) handleRetireFoundryBoot(w http.ResponseWriter, r *http.Request) {
	var request harnessv2.FoundryBootRetirementRequest
	if !s.decodeAuthenticatedJSON(w, r, &request) {
		return
	}
	if err := request.ValidateAt(time.Now().UTC()); err != nil {
		writeError(w, http.StatusBadRequest, harnessv2.ErrorCodeInvalidRequest, err.Error(), nil, false)
		return
	}
	if !s.cfg.RequireCapabilities || !s.authorizeMutation(w, r, request.Metadata, false) {
		if !s.cfg.RequireCapabilities {
			writeError(w, http.StatusForbidden, harnessv2.ErrorCodeForbidden, "Foundry recovery requires operation capabilities", nil, false)
		}
		return
	}
	if !s.cfg.Capabilities.SupportsFoundryRecovery || s.cfg.Provider.Kind != providerKindFoundry {
		writeError(w, http.StatusBadRequest, harnessv2.ErrorCodeInvalidRequest, "runtime does not support Foundry recovery", nil, false)
		return
	}
	classification, err := s.classifyFoundryRecovery(request, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusTooManyRequests, harnessv2.ErrorCodeRateLimited, "Foundry recovery replay capacity is unavailable", nil, true)
		return
	}
	if classification.Class != harnessv2.RequestClassificationFresh && classification.Class != harnessv2.RequestClassificationDuplicate {
		writeClassificationError(w, classification)
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), request.Metadata.ExpiresAt)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, foundryCleanupTimeout)
	defer timeoutCancel()
	identity, err := s.foundryStatusIdentity(ctx)
	if err != nil || identity == nil || *identity != request.Broker {
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeCleanupUnproven, "Foundry recovery ledger identity is unavailable or changed", nil, true)
		return
	}
	body, err := request.BrokerRequestBody()
	if err != nil {
		writeError(w, http.StatusBadRequest, harnessv2.ErrorCodeInvalidRequest, "invalid Foundry recovery request", nil, false)
		return
	}
	var proof harnessv2.FoundryBootRetirementProof
	if err := s.providerProxy.foundryRecoveryJSON(ctx, http.MethodPost, "/internal/v1/retire-boot", body, &proof); err != nil || proof.ValidateFor(request) != nil {
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeCleanupUnproven, "Foundry boot retirement is not proven", nil, true)
		return
	}
	s.mu.Lock()
	operation := s.foundryRecoveryOps[request.Metadata.OperationID]
	operation.record.Phase, operation.record.UpdatedAt = harnessv2.OperationPhaseApplied, time.Now().UTC()
	s.foundryRecoveryOps[request.Metadata.OperationID] = operation
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, harnessv2.FoundryBootRetirementResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: classification, RelayFence: s.cfg.Fence,
		RequestDigest: request.Metadata.RequestDigest, Proof: proof,
	})
}
