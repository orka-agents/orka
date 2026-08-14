/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package referenceadapter implements a deterministic, durable, single-active
// SQLite reference server for orka.oms.v0alpha1.
package referenceadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	defaultCapabilityTTL    = 5 * time.Minute
	defaultSnapshotTTL      = 15 * time.Minute
	defaultSnapshotRecords  = 256
	referenceAdapterName    = "orka-oms-reference-adapter"
	referenceAdapterVersion = "v0alpha1-sqlite-1"
	referenceRevision       = "orka.oms.v0alpha1.reference.sqlite.1"
)

// Config configures one durable single-active reference adapter instance.
type Config struct {
	DatabasePath       string
	BearerToken        string
	CapabilityTTL      time.Duration
	SnapshotTTL        time.Duration
	MaxSnapshotRecords int
	Clock              func() time.Time
}

// Server owns the process lock and SQLite state. Close must be called before a
// second process opens the same database path.
type Server struct {
	authValue          string
	db                 *database
	capabilityTTL      time.Duration
	snapshotTTL        time.Duration
	maxSnapshotRecords int
	clock              func() time.Time
	closeOnce          sync.Once
	closeErr           error
}

// Open opens and migrates the durable reference database and acquires its
// single-active process lock.
func Open(ctx context.Context, config Config) (*Server, error) {
	token := strings.TrimSpace(config.BearerToken)
	if token == "" || len(token) > 4096 || strings.ContainsFunc(token, unicode.IsSpace) || strings.ContainsFunc(token, unicode.IsControl) {
		return nil, errors.New("bearer token is required and must not contain whitespace or controls")
	}
	capabilityTTL := config.CapabilityTTL
	if capabilityTTL <= 0 {
		capabilityTTL = defaultCapabilityTTL
	}
	snapshotTTL := config.SnapshotTTL
	if snapshotTTL <= 0 {
		snapshotTTL = defaultSnapshotTTL
	}
	maxSnapshotRecords := config.MaxSnapshotRecords
	if maxSnapshotRecords <= 0 {
		maxSnapshotRecords = defaultSnapshotRecords
	}
	if maxSnapshotRecords > protocol.MaxSnapshotRecords {
		return nil, fmt.Errorf("max snapshot records exceeds %d", protocol.MaxSnapshotRecords)
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	db, err := openDatabase(ctx, config.DatabasePath)
	if err != nil {
		return nil, err
	}
	return &Server{
		authValue: token, db: db, capabilityTTL: capabilityTTL,
		snapshotTTL: snapshotTTL, maxSnapshotRecords: maxSnapshotRecords, clock: clock,
	}, nil
}

// Close releases the SQLite connection and process lock.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.db.close() })
	return s.closeErr
}

// Handler returns the authenticated OMS HTTP contract. Callers provide the TLS
// serving layer.
type serverRoute struct {
	method  string
	handler http.HandlerFunc
}

func (s *Server) Handler() http.Handler {
	routes := map[string]serverRoute{
		protocol.PathHealth:         {method: http.MethodGet, handler: s.handleHealth},
		protocol.PathStoreResolve:   {method: http.MethodPost, handler: s.handleStoreResolve},
		protocol.PathCapabilities:   {method: http.MethodPost, handler: s.handleCapabilities},
		protocol.PathOwnershipClaim: {method: http.MethodPost, handler: s.handleOwnershipClaim},
		protocol.PathRoutingFence:   {method: http.MethodPost, handler: s.handleRoutingFence},
		protocol.PathMutations:      {method: http.MethodPost, handler: s.handleMutation},
		protocol.PathRecordsGet:     {method: http.MethodPost, handler: s.handleGet},
		protocol.PathOperationsGet:  {method: http.MethodPost, handler: s.handleOperationLookup},
		protocol.PathSearch:         {method: http.MethodPost, handler: s.handleSearch},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticate(w, r) {
			return
		}
		route, found := routes[r.URL.Path]
		if !found {
			s.writeError(w, http.StatusNotFound, nil, protocol.ErrorCodeNotFound, "endpoint not found", false, 0)
			return
		}
		if r.Method != route.method {
			w.Header().Set("Allow", route.method)
			s.writeError(w, http.StatusMethodNotAllowed, nil, protocol.ErrorCodeMethodNotAllowed, "method not allowed", false, 0)
			return
		}
		route.handler(w, r)
	})
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) bool {
	got := protocol.BearerToken(r.Header.Get("Authorization"))
	if !protocol.ConstantTimeBearerEqual(got, s.authValue) {
		s.writeError(w, http.StatusUnauthorized, nil, protocol.ErrorCodeUnauthorized, "unauthorized", false, 0)
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, protocol.HealthResponse{ProtocolVersion: protocol.Version, Status: "ok"})
}

func (s *Server) handleStoreResolve(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeStoreResolveRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "invalid store resolve request", false, 0)
		return
	}
	storeUUID, err := s.db.resolveStore(r.Context(), request.Binding, request.StoreName, s.now())
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, nil, protocol.ErrorCodeInternal, "store resolution unavailable", true, 1)
		return
	}
	response := protocol.StoreResolveResponse{
		ProtocolVersion: protocol.Version, Binding: request.Binding,
		StoreName: request.StoreName, StoreUUID: storeUUID,
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeCapabilitiesRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "invalid capabilities request", false, 0)
		return
	}
	now := s.now()
	response := protocol.CapabilitiesResponse{
		ProtocolVersion: protocol.Version, Binding: request.Binding,
		AdapterName: referenceAdapterName, AdapterVersion: referenceAdapterVersion,
		Revision: referenceRevision, ExpiresAt: now.Add(s.capabilityTTL),
		Capabilities: protocol.Capabilities{
			DurableIdempotency: true, IdempotencyDigestConflicts: true, CreateIfAbsent: true,
			ConditionalMutation: true, MonotonicGenerations: true, DeleteHighWatermark: true,
			DurableRoutingFence: true, OperationLookup: true, ExactGet: true, StablePagination: true,
			ExclusiveOwnership: true, KeywordSearch: true, AuditVersionVisibility: true,
			SemanticSearch: false, HybridSearch: false,
		},
		Limits: protocol.CapabilityLimits{
			MaxRequestBytes: protocol.MaxHTTPBodyBytes, MaxResponseBytes: protocol.MaxAdapterResponseBytes,
			MaxContentBytes: protocol.MaxContentBytes, MaxTags: protocol.MaxTags, MaxTagBytes: protocol.MaxTagBytes,
			MaxMetadataEntries: protocol.MaxMetadataEntries, MaxMetadataKeyBytes: protocol.MaxMetadataKeyBytes,
			MaxMetadataValueBytes: protocol.MaxMetadataValueBytes, MaxQueryBytes: protocol.MaxQueryBytes,
			MaxPageSize: protocol.MaxPageSize, MaxSnapshotRecords: s.maxSnapshotRecords,
			SnapshotTTLSeconds: int(s.snapshotTTL / time.Second),
		},
	}
	if err := protocol.ValidateCapabilitiesResponse(&response, now.Add(-time.Nanosecond)); err != nil {
		s.writeError(w, http.StatusInternalServerError, &request.Binding, protocol.ErrorCodeInternal, "capability configuration is invalid", false, 0)
		return
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleOwnershipClaim(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeOwnershipClaimRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "invalid ownership claim request", false, 0)
		return
	}
	decision, err := s.db.claimOwnership(r.Context(), request.Binding, s.now())
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, &request.Binding, protocol.ErrorCodeInternal, "ownership claim unavailable", true, 1)
		return
	}
	response := protocol.OwnershipClaimResponse{
		ProtocolVersion: protocol.Version, Binding: request.Binding, Result: decision.result,
		BindingDigest: protocol.BindingDigest(request.Binding), ClaimIdentity: decision.claimIdentity,
		MaximumRoutingEpoch: decision.maxRouting, ClaimedAt: decision.claimedAt,
	}
	status := http.StatusOK
	if decision.result != protocol.ResultApplied {
		status = http.StatusConflict
	}
	s.writeJSON(w, status, response)
}

func (s *Server) handleRoutingFence(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeRoutingFenceRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "invalid routing fence request", false, 0)
		return
	}
	now := s.now()
	decision, err := s.db.advanceRoutingFence(r.Context(), request.Binding, now)
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, &request.Binding, protocol.ErrorCodeInternal, "routing fence unavailable", true, 1)
		return
	}
	response := protocol.RoutingFenceResponse{
		ProtocolVersion: protocol.Version, Binding: request.Binding, Result: decision.result,
		BindingDigest: protocol.BindingDigest(request.Binding), MaximumRoutingEpoch: decision.maxRouting,
		CompletedAt: now,
	}
	status := http.StatusOK
	if decision.result != protocol.ResultApplied {
		status = http.StatusConflict
	}
	s.writeJSON(w, status, response)
}

func (s *Server) handleMutation(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeMutationEnvelope(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "invalid mutation envelope", false, 0)
		return
	}
	receipt, err := s.db.applyMutation(r.Context(), request, s.now())
	if err != nil {
		receipt = protocol.MutationReceipt{
			ProtocolVersion: protocol.Version, Binding: request.Binding, Result: protocol.ResultRetryableError,
			OperationID: request.OperationID, BindingDigest: protocol.BindingDigest(request.Binding),
			ContentDigest: request.ContentDigest, MutationDigest: request.MutationDigest, CompletedAt: s.now(),
		}
	}
	status := mutationHTTPStatus(receipt.Result)
	s.writeJSON(w, status, receipt)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeGetRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "invalid exact-get request", false, 0)
		return
	}
	record, found, err := s.db.lookupRecord(r.Context(), request.Binding, request.UpsertKey)
	if err != nil {
		s.writeAccessError(w, request.Binding, err)
		return
	}
	response := protocol.GetResponse{ProtocolVersion: protocol.Version, Binding: request.Binding, Found: found}
	if found {
		response.Record = &record
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleOperationLookup(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeOperationLookupRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "invalid operation lookup request", false, 0)
		return
	}
	receipt, found, err := s.db.lookupOperation(r.Context(), request.Binding, request.OperationID)
	if err != nil {
		s.writeAccessError(w, request.Binding, err)
		return
	}
	response := protocol.OperationLookupResponse{ProtocolVersion: protocol.Version, Binding: request.Binding, Found: found}
	if found {
		response.Receipt = &receipt
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := protocol.DecodeSearchRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "invalid search request", false, 0)
		return
	}
	if request.Mode == protocol.SearchModeSemantic || request.Mode == protocol.SearchModeHybrid {
		s.writeError(w, http.StatusUnprocessableEntity, &request.Binding, protocol.ErrorCodeSearchModeUnsupported, "requested search mode is unsupported", false, 0)
		return
	}
	page, err := s.db.search(r.Context(), request, s.now(), s.snapshotTTL, s.maxSnapshotRecords)
	if err != nil {
		switch {
		case errors.Is(err, errIdentityConflict):
			s.writeError(w, http.StatusConflict, &request.Binding, protocol.ErrorCodeIdentityConflict, "binding is not the claimed owner", false, 0)
		case errors.Is(err, errRoutingFenced):
			s.writeError(w, http.StatusConflict, &request.Binding, protocol.ErrorCodeRoutingFenced, "routing epoch is fenced", false, 0)
		case errors.Is(err, errSnapshotInvalid):
			s.writeError(w, http.StatusConflict, &request.Binding, protocol.ErrorCodePageTokenInvalid, "page token is invalid for this request", false, 0)
		case errors.Is(err, errSnapshotExpired):
			s.writeError(w, http.StatusConflict, &request.Binding, protocol.ErrorCodePageTokenExpired, "page token has expired", false, 0)
		case errors.Is(err, errSnapshotCapacity):
			s.writeError(w, http.StatusServiceUnavailable, &request.Binding, protocol.ErrorCodeSnapshotCapacity, "snapshot capacity exceeded", true, 1)
		default:
			s.writeError(w, http.StatusServiceUnavailable, &request.Binding, protocol.ErrorCodeInternal, "search unavailable", true, 1)
		}
		return
	}
	response := protocol.SearchResponse{
		ProtocolVersion: protocol.Version, Binding: request.Binding,
		RequestedMode: request.Mode, ActualMode: page.actualMode, Records: page.records,
		NextPageToken: page.nextToken, Exhausted: page.exhausted, SnapshotExpiresAt: page.expiresAt,
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeAccessError(w http.ResponseWriter, binding protocol.Binding, err error) {
	switch {
	case errors.Is(err, errIdentityConflict):
		s.writeError(w, http.StatusConflict, &binding, protocol.ErrorCodeIdentityConflict, "binding is not the claimed owner", false, 0)
	case errors.Is(err, errRoutingFenced):
		s.writeError(w, http.StatusConflict, &binding, protocol.ErrorCodeRoutingFenced, "routing epoch is fenced", false, 0)
	default:
		s.writeError(w, http.StatusServiceUnavailable, &binding, protocol.ErrorCodeInternal, "adapter state unavailable", true, 1)
	}
}

func (s *Server) readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		s.writeError(w, http.StatusUnsupportedMediaType, nil, protocol.ErrorCodeInvalidRequest, "content type must be application/json", false, 0)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, protocol.MaxHTTPBodyBytes+1))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, protocol.ErrorCodeInvalidRequest, "request body could not be read", false, 0)
		return nil, false
	}
	if len(body) > protocol.MaxHTTPBodyBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, nil, protocol.ErrorCodeInvalidRequest, "request body exceeds the profile limit", false, 0)
		return nil, false
	}
	return body, true
}

func (s *Server) writeError(w http.ResponseWriter, status int, binding *protocol.Binding, code, message string, retryable bool, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	}
	response := protocol.ErrorResponse{
		ProtocolVersion: protocol.Version, Binding: binding, Code: code,
		Message:   protocol.SanitizeMessage(message, protocol.MaxErrorMessageBytes),
		Retryable: retryable, RetryAfterSeconds: retryAfter,
	}
	s.writeJSON(w, status, response)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil || len(body) > protocol.MaxAdapterResponseBytes {
		fallback, _ := json.Marshal(protocol.ErrorResponse{
			ProtocolVersion: protocol.Version, Binding: nil, Code: protocol.ErrorCodeResponseTooLarge,
			Message: "response could not be encoded within the profile limit", Retryable: false, RetryAfterSeconds: 0,
		})
		body = fallback
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) now() time.Time {
	return s.clock().UTC()
}

func mutationHTTPStatus(result string) int {
	switch result {
	case protocol.ResultApplied, protocol.ResultNotFound:
		return http.StatusOK
	case protocol.ResultPreconditionFailed, protocol.ResultIdempotencyConflict, protocol.ResultIdentityConflict:
		return http.StatusConflict
	case protocol.ResultRetryableError:
		return http.StatusServiceUnavailable
	case protocol.ResultNonRetryableError:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
