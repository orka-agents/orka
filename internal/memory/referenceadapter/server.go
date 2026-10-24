/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package referenceadapter implements a deterministic, durable, single-active
// SQLite reference server for orka.memory.v1.
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

	memoryv1 "github.com/orka-agents/orka/pkg/memory/v1"
)

const (
	defaultCapabilityTTL    = 5 * time.Minute
	defaultSnapshotTTL      = 15 * time.Minute
	defaultSnapshotRecords  = 256
	referenceAdapterName    = "orka-memory-reference-adapter"
	referenceAdapterVersion = "v1-sqlite-1"
	referenceRevision       = "orka.memory.v1.reference.sqlite.1"
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
	if maxSnapshotRecords > memoryv1.MaxSnapshotRecords {
		return nil, fmt.Errorf("max snapshot records exceeds %d", memoryv1.MaxSnapshotRecords)
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

// Handler returns the authenticated memory protocol HTTP contract. Callers provide the TLS
// serving layer.
type serverRoute struct {
	method  string
	handler http.HandlerFunc
}

func (s *Server) Handler() http.Handler {
	routes := map[string]serverRoute{
		memoryv1.PathHealth:         {method: http.MethodGet, handler: s.handleHealth},
		memoryv1.PathStoreResolve:   {method: http.MethodPost, handler: s.handleStoreResolve},
		memoryv1.PathCapabilities:   {method: http.MethodPost, handler: s.handleCapabilities},
		memoryv1.PathOwnershipClaim: {method: http.MethodPost, handler: s.handleOwnershipClaim},
		memoryv1.PathRoutingFence:   {method: http.MethodPost, handler: s.handleRoutingFence},
		memoryv1.PathMutations:      {method: http.MethodPost, handler: s.handleMutation},
		memoryv1.PathRecordsGet:     {method: http.MethodPost, handler: s.handleGet},
		memoryv1.PathOperationsGet:  {method: http.MethodPost, handler: s.handleOperationLookup},
		memoryv1.PathSearch:         {method: http.MethodPost, handler: s.handleSearch},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticate(w, r) {
			return
		}
		route, found := routes[r.URL.Path]
		if !found {
			s.writeError(w, http.StatusNotFound, nil, memoryv1.ErrorCodeNotFound, "endpoint not found", false, 0)
			return
		}
		if r.Method != route.method {
			w.Header().Set("Allow", route.method)
			s.writeError(w, http.StatusMethodNotAllowed, nil, memoryv1.ErrorCodeMethodNotAllowed, "method not allowed", false, 0)
			return
		}
		route.handler(w, r)
	})
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) bool {
	got := memoryv1.BearerToken(r.Header.Get("Authorization"))
	if !memoryv1.ConstantTimeBearerEqual(got, s.authValue) {
		s.writeError(w, http.StatusUnauthorized, nil, memoryv1.ErrorCodeUnauthorized, "unauthorized", false, 0)
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, memoryv1.HealthResponse{ProtocolVersion: memoryv1.Version, Status: "ok"})
}

func (s *Server) handleStoreResolve(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := memoryv1.DecodeStoreResolveRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "invalid store resolve request", false, 0)
		return
	}
	storeUUID, err := s.db.resolveStore(r.Context(), request.Binding, request.StoreName, s.now())
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, nil, memoryv1.ErrorCodeInternal, "store resolution unavailable", true, 1)
		return
	}
	response := memoryv1.StoreResolveResponse{
		ProtocolVersion: memoryv1.Version, Binding: request.Binding,
		StoreName: request.StoreName, StoreUUID: storeUUID,
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := memoryv1.DecodeCapabilitiesRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "invalid capabilities request", false, 0)
		return
	}
	now := s.now()
	response := memoryv1.CapabilitiesResponse{
		ProtocolVersion: memoryv1.Version, Binding: request.Binding,
		AdapterName: referenceAdapterName, AdapterVersion: referenceAdapterVersion,
		Revision: referenceRevision, ExpiresAt: now.Add(s.capabilityTTL),
		Capabilities: memoryv1.Capabilities{
			DurableIdempotency: true, IdempotencyDigestConflicts: true, CreateIfAbsent: true,
			ConditionalMutation: true, MonotonicGenerations: true, DeleteHighWatermark: true,
			DurableRoutingFence: true, OperationLookup: true, ExactGet: true, StablePagination: true,
			ExclusiveOwnership: true, KeywordSearch: true, AuditVersionVisibility: true,
			SemanticSearch: false, HybridSearch: false,
		},
		Limits: memoryv1.CapabilityLimits{
			MaxRequestBytes: memoryv1.MaxHTTPBodyBytes, MaxResponseBytes: memoryv1.MaxAdapterResponseBytes,
			MaxContentBytes: memoryv1.MaxContentBytes, MaxTags: memoryv1.MaxTags, MaxTagBytes: memoryv1.MaxTagBytes,
			MaxMetadataEntries: memoryv1.MaxMetadataEntries, MaxMetadataKeyBytes: memoryv1.MaxMetadataKeyBytes,
			MaxMetadataValueBytes: memoryv1.MaxMetadataValueBytes, MaxQueryBytes: memoryv1.MaxQueryBytes,
			MaxPageSize: memoryv1.MaxPageSize, MaxSnapshotRecords: s.maxSnapshotRecords,
			SnapshotTTLSeconds: int(s.snapshotTTL / time.Second),
		},
	}
	if err := memoryv1.ValidateCapabilitiesResponse(&response, now.Add(-time.Nanosecond)); err != nil {
		s.writeError(w, http.StatusInternalServerError, &request.Binding, memoryv1.ErrorCodeInternal, "capability configuration is invalid", false, 0)
		return
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleOwnershipClaim(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := memoryv1.DecodeOwnershipClaimRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "invalid ownership claim request", false, 0)
		return
	}
	decision, err := s.db.claimOwnership(r.Context(), request.Binding, s.now())
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, &request.Binding, memoryv1.ErrorCodeInternal, "ownership claim unavailable", true, 1)
		return
	}
	response := memoryv1.OwnershipClaimResponse{
		ProtocolVersion: memoryv1.Version, Binding: request.Binding, Result: decision.result,
		BindingDigest: memoryv1.BindingDigest(request.Binding), ClaimIdentity: decision.claimIdentity,
		MaximumRoutingEpoch: decision.maxRouting, ClaimedAt: decision.claimedAt,
	}
	status := http.StatusOK
	if decision.result != memoryv1.ResultApplied {
		status = http.StatusConflict
	}
	s.writeJSON(w, status, response)
}

func (s *Server) handleRoutingFence(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := memoryv1.DecodeRoutingFenceRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "invalid routing fence request", false, 0)
		return
	}
	now := s.now()
	decision, err := s.db.advanceRoutingFence(r.Context(), request.Binding, now)
	if err != nil {
		s.writeError(w, http.StatusServiceUnavailable, &request.Binding, memoryv1.ErrorCodeInternal, "routing fence unavailable", true, 1)
		return
	}
	response := memoryv1.RoutingFenceResponse{
		ProtocolVersion: memoryv1.Version, Binding: request.Binding, Result: decision.result,
		BindingDigest: memoryv1.BindingDigest(request.Binding), MaximumRoutingEpoch: decision.maxRouting,
		CompletedAt: now,
	}
	status := http.StatusOK
	if decision.result != memoryv1.ResultApplied {
		status = http.StatusConflict
	}
	s.writeJSON(w, status, response)
}

func (s *Server) handleMutation(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSONBody(w, r)
	if !ok {
		return
	}
	request, err := memoryv1.DecodeMutationEnvelope(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "invalid mutation envelope", false, 0)
		return
	}
	receipt, err := s.db.applyMutation(r.Context(), request, s.now())
	if err != nil {
		receipt = memoryv1.MutationReceipt{
			ProtocolVersion: memoryv1.Version, Binding: request.Binding, Result: memoryv1.ResultRetryableError,
			OperationID: request.OperationID, BindingDigest: memoryv1.BindingDigest(request.Binding),
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
	request, err := memoryv1.DecodeGetRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "invalid exact-get request", false, 0)
		return
	}
	record, found, err := s.db.lookupRecord(r.Context(), request.Binding, request.UpsertKey)
	if err != nil {
		s.writeAccessError(w, request.Binding, err)
		return
	}
	response := memoryv1.GetResponse{ProtocolVersion: memoryv1.Version, Binding: request.Binding, Found: found}
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
	request, err := memoryv1.DecodeOperationLookupRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "invalid operation lookup request", false, 0)
		return
	}
	receipt, found, err := s.db.lookupOperation(r.Context(), request.Binding, request.OperationID)
	if err != nil {
		s.writeAccessError(w, request.Binding, err)
		return
	}
	response := memoryv1.OperationLookupResponse{ProtocolVersion: memoryv1.Version, Binding: request.Binding, Found: found}
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
	request, err := memoryv1.DecodeSearchRequest(body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "invalid search request", false, 0)
		return
	}
	if request.Mode == memoryv1.SearchModeSemantic || request.Mode == memoryv1.SearchModeHybrid {
		s.writeError(w, http.StatusUnprocessableEntity, &request.Binding, memoryv1.ErrorCodeSearchModeUnsupported, "requested search mode is unsupported", false, 0)
		return
	}
	page, err := s.db.search(r.Context(), request, s.now(), s.snapshotTTL, s.maxSnapshotRecords)
	if err != nil {
		switch {
		case errors.Is(err, errIdentityConflict):
			s.writeError(w, http.StatusConflict, &request.Binding, memoryv1.ErrorCodeIdentityConflict, "binding is not the claimed owner", false, 0)
		case errors.Is(err, errRoutingFenced):
			s.writeError(w, http.StatusConflict, &request.Binding, memoryv1.ErrorCodeRoutingFenced, "routing epoch is fenced", false, 0)
		case errors.Is(err, errSnapshotInvalid):
			s.writeError(w, http.StatusConflict, &request.Binding, memoryv1.ErrorCodePageTokenInvalid, "page token is invalid for this request", false, 0)
		case errors.Is(err, errSnapshotExpired):
			s.writeError(w, http.StatusConflict, &request.Binding, memoryv1.ErrorCodePageTokenExpired, "page token has expired", false, 0)
		case errors.Is(err, errSnapshotCapacity):
			s.writeError(w, http.StatusServiceUnavailable, &request.Binding, memoryv1.ErrorCodeSnapshotCapacity, "snapshot capacity exceeded", true, 1)
		default:
			s.writeError(w, http.StatusServiceUnavailable, &request.Binding, memoryv1.ErrorCodeInternal, "search unavailable", true, 1)
		}
		return
	}
	response := memoryv1.SearchResponse{
		ProtocolVersion: memoryv1.Version, Binding: request.Binding,
		RequestedMode: request.Mode, ActualMode: page.actualMode, Records: page.records,
		NextPageToken: page.nextToken, Exhausted: page.exhausted, SnapshotExpiresAt: page.expiresAt,
	}
	s.writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeAccessError(w http.ResponseWriter, binding memoryv1.Binding, err error) {
	switch {
	case errors.Is(err, errIdentityConflict):
		s.writeError(w, http.StatusConflict, &binding, memoryv1.ErrorCodeIdentityConflict, "binding is not the claimed owner", false, 0)
	case errors.Is(err, errRoutingFenced):
		s.writeError(w, http.StatusConflict, &binding, memoryv1.ErrorCodeRoutingFenced, "routing epoch is fenced", false, 0)
	default:
		s.writeError(w, http.StatusServiceUnavailable, &binding, memoryv1.ErrorCodeInternal, "adapter state unavailable", true, 1)
	}
}

func (s *Server) readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		s.writeError(w, http.StatusUnsupportedMediaType, nil, memoryv1.ErrorCodeInvalidRequest, "content type must be application/json", false, 0)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, memoryv1.MaxHTTPBodyBytes+1))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, nil, memoryv1.ErrorCodeInvalidRequest, "request body could not be read", false, 0)
		return nil, false
	}
	if len(body) > memoryv1.MaxHTTPBodyBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, nil, memoryv1.ErrorCodeInvalidRequest, "request body exceeds the profile limit", false, 0)
		return nil, false
	}
	return body, true
}

func (s *Server) writeError(w http.ResponseWriter, status int, binding *memoryv1.Binding, code, message string, retryable bool, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	}
	response := memoryv1.ErrorResponse{
		ProtocolVersion: memoryv1.Version, Binding: binding, Code: code,
		Message:   memoryv1.SanitizeMessage(message, memoryv1.MaxErrorMessageBytes),
		Retryable: retryable, RetryAfterSeconds: retryAfter,
	}
	s.writeJSON(w, status, response)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil || len(body) > memoryv1.MaxAdapterResponseBytes {
		fallback, _ := json.Marshal(memoryv1.ErrorResponse{
			ProtocolVersion: memoryv1.Version, Binding: nil, Code: memoryv1.ErrorCodeResponseTooLarge,
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
	case memoryv1.ResultApplied, memoryv1.ResultNotFound:
		return http.StatusOK
	case memoryv1.ResultPreconditionFailed, memoryv1.ResultIdempotencyConflict, memoryv1.ResultIdentityConflict:
		return http.StatusConflict
	case memoryv1.ResultRetryableError:
		return http.StatusServiceUnavailable
	case memoryv1.ResultNonRetryableError:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
