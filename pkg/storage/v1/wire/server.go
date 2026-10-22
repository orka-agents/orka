package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
	"google.golang.org/protobuf/proto"
)

// Authenticator authenticates a call from its request headers before the
// server decodes the request. ctx carries the connection's peer, including
// its verified TLS state.
type Authenticator interface {
	Authenticate(ctx context.Context, fullMethod string, header metadata.MD) error
}

// Limits bounds the calls a Server runs at once and how long each may run.
// A missing or zero entry means no limit.
type Limits struct {
	// MaxConcurrent caps the calls of each class the server runs at once. A
	// call over its class's limit is refused with OVERLOADED before the
	// provider runs, and before an upload's blob is read.
	MaxConcurrent map[Class]int
	// MaxDuration caps how long the provider runs for a call of each class;
	// the provider still gets the caller's deadline when that is shorter.
	MaxDuration map[Class]time.Duration
}

// CallInfo describes one finished call, without its content.
type CallInfo struct {
	// Method is the contract method name, such as "SaveArtifact".
	Method   string
	Kind     Kind
	Class    Class
	Code     codes.Code
	Reason   string
	Duration time.Duration
}

// CallObserver receives every storage call's outcome, for metrics.
type CallObserver interface {
	ObserveCall(CallInfo)
}

// ServerConfig configures a Server.
type ServerConfig struct {
	// State is the state.v1 provider the server serves, or nil when it
	// serves only artifacts.v1.
	State storagev1.StateProvider
	// Artifacts is the artifacts.v1 provider the server serves, or nil when
	// it serves only state.v1. At least one of State and Artifacts is
	// required. When both are set they must serve the same dataset, and
	// State answers Describe for both. A server answers every RPC of a
	// contract part it does not serve with UNIMPLEMENTED.
	Artifacts storagev1.ArtifactProvider
	// Info names the adapter. The server library sets ProtocolRevision and
	// LibraryVersion itself.
	Info ServerInfo
	// Authenticator, when set, authenticates every call before decoding.
	// The server library does not authenticate calls without one.
	Authenticator Authenticator
	// Logger receives one entry per call and the detail of provider errors.
	// It never receives request or response content. Nil means
	// slog.Default().
	Logger *slog.Logger
	// Limits bounds concurrency and call duration per class.
	Limits Limits
	// MaxBlobBytes caps the blob an upload may carry. Zero means
	// DefaultMaxBlobBytes. An artifact is also capped at the artifacts
	// provider's own MaxArtifactBytes.
	MaxBlobBytes int64
	// Observer, when set, receives every call's outcome.
	Observer CallObserver
}

// Server serves storage providers over gRPC.
type Server struct {
	state         storagev1.StateProvider
	artifacts     storagev1.ArtifactProvider
	dataset       datasetProvider
	info          ServerInfo
	authenticator Authenticator
	logger        *slog.Logger
	observer      CallObserver
	maxDuration   map[Class]time.Duration
	admission     map[Class]chan struct{}
	maxBlobBytes  int64

	// datasetID is the serving dataset from the provider's last Describe.
	datasetID atomic.Pointer[string]
	// artifactLimit is the artifacts provider's MaxArtifactBytes from its
	// last Describe.
	artifactLimit atomic.Int64
	describeMu    sync.Mutex
}

// NewServer returns a Server for config.
func NewServer(config ServerConfig) (*Server, error) {
	if config.State == nil && config.Artifacts == nil {
		return nil, errors.New("storage server requires a state or artifacts provider")
	}
	info := config.Info
	info.ProtocolRevision = ProtocolRevision
	info.LibraryVersion = libraryVersion()
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBlob := config.MaxBlobBytes
	if maxBlob <= 0 {
		maxBlob = DefaultMaxBlobBytes
	}
	s := &Server{
		state: config.State, artifacts: config.Artifacts, info: info, authenticator: config.Authenticator,
		logger: logger, observer: config.Observer, maxDuration: map[Class]time.Duration{},
		admission: map[Class]chan struct{}{}, maxBlobBytes: maxBlob,
	}
	s.dataset = datasetProvider{server: s}
	for class, limit := range config.Limits.MaxConcurrent {
		if limit > 0 {
			s.admission[class] = make(chan struct{}, limit)
		}
	}
	for class, duration := range config.Limits.MaxDuration {
		if duration > 0 {
			s.maxDuration[class] = duration
		}
	}
	return s, nil
}

// ServerOptions returns the options a gRPC server needs to serve this
// Server: authentication before decoding, the call interceptors, message size
// limits, keepalive enforcement that admits the client's pings, and a maximum
// connection age so long-lived connections spread across adapter replicas.
// Callers add TLS credentials. A gRPC server built with these options serves
// only the storage services: it authenticates every call and refuses any
// method the storage protocol does not define.
func (s *Server) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.InTapHandle(s.tap),
		grpc.ChainUnaryInterceptor(s.intercept),
		grpc.ChainStreamInterceptor(s.interceptStream),
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.MaxSendMsgSize(MaxMessageBytes),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             20 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 5 * time.Minute,
		}),
	}
}

// Register registers the storage services on registrar: the Dataset service
// always, and the services of each contract part the server serves. A gRPC
// server built with ServerOptions answers any other RPC with UNIMPLEMENTED.
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	s.registerServices(registrar)
}

// Refresh describes the served providers and records the serving dataset,
// which every call's dataset header must name. A server describes its
// providers on its first call that needs the dataset; an adapter calls
// Refresh at startup and periodically, so that a dataset change, such as a
// database replaced under the adapter, is noticed without a client Describe.
func (s *Server) Refresh(ctx context.Context) error {
	_, err := s.describe(ctx)
	return err
}

// DatasetID returns the serving dataset from the providers' last
// description, or "" before one succeeded.
func (s *Server) DatasetID() string {
	if dataset := s.datasetID.Load(); dataset != nil {
		return *dataset
	}
	return ""
}

// describe describes the served providers and records the serving dataset.
func (s *Server) describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	var info storagev1.ProviderInfo
	var err error
	if s.state != nil {
		info, err = s.state.Describe(ctx)
	} else {
		info, err = s.artifacts.Describe(ctx)
	}
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	if info.DatasetID == "" {
		s.forgetDataset(ctx, "the storage provider reports no dataset ID")
		return storagev1.ProviderInfo{}, errors.New("the storage provider reports no dataset ID")
	}
	limits := info.Limits
	if s.state != nil && s.artifacts != nil {
		artifacts, err := s.artifacts.Describe(ctx)
		if err != nil {
			return storagev1.ProviderInfo{}, err
		}
		if artifacts.DatasetID != info.DatasetID {
			s.forgetDataset(ctx, "the state and artifacts providers serve different datasets")
			return storagev1.ProviderInfo{}, fmt.Errorf("the state provider serves dataset %q and the artifacts provider %q",
				info.DatasetID, artifacts.DatasetID)
		}
		limits = artifacts.Limits
	}
	if s.artifacts != nil {
		s.artifactLimit.Store(max(limits.MaxArtifactBytes, storagev1.MaxArtifactBytes))
	}
	dataset := info.DatasetID
	if previous := s.datasetID.Swap(&dataset); previous != nil && *previous != dataset {
		s.logger.WarnContext(ctx, "storage provider now serves another dataset", "previous", *previous, "dataset", dataset)
	}
	return info, nil
}

// forgetDataset drops the serving dataset after a description that names no
// single dataset, so no call is admitted until a consistent description
// succeeds. A description that merely fails, such as one that times out,
// keeps the dataset: the providers' own errors then report the outage.
func (s *Server) forgetDataset(ctx context.Context, reason string) {
	if previous := s.datasetID.Swap(nil); previous != nil {
		s.logger.ErrorContext(ctx, "storage server no longer has a serving dataset", "previous", *previous, "reason", reason)
	}
}

// servingDataset returns the serving dataset, describing the providers if
// no description has succeeded yet.
func (s *Server) servingDataset(ctx context.Context) (string, error) {
	if dataset := s.DatasetID(); dataset != "" {
		return dataset, nil
	}
	s.describeMu.Lock()
	defer s.describeMu.Unlock()
	if dataset := s.DatasetID(); dataset != "" {
		return dataset, nil
	}
	if _, err := s.describe(ctx); err != nil {
		return "", err
	}
	return s.DatasetID(), nil
}

// datasetProvider answers Describe and HealthCheck for every contract part
// the server serves.
type datasetProvider struct {
	server *Server
}

func (p datasetProvider) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	return p.server.describe(ctx)
}

func (p datasetProvider) HealthCheck(ctx context.Context) error {
	if p.server.state != nil {
		if err := p.server.state.HealthCheck(ctx); err != nil {
			return err
		}
	}
	if p.server.artifacts != nil {
		return p.server.artifacts.HealthCheck(ctx)
	}
	return nil
}

// tap authenticates every call before its request is read. It does not
// depend on recognizing the method, so a path the method table does not name
// is never served unauthenticated.
func (s *Server) tap(ctx context.Context, info *tap.Info) (context.Context, error) {
	if s.authenticator == nil {
		return ctx, nil
	}
	if err := s.authenticator.Authenticate(ctx, info.FullMethodName, info.Header); err != nil {
		call := CallInfo{Method: unknownName, Code: codes.Unauthenticated, Reason: ReasonUnauthenticated}
		if m := methodByFullName(info.FullMethodName); m != nil {
			call.Method, call.Kind, call.Class = m.name, m.kind, m.class()
		}
		s.logger.DebugContext(ctx, "storage authentication failed", "method", call.Method, "error", err)
		if s.observer != nil {
			s.observer.ObserveCall(call)
		}
		return ctx, errorStatus(codes.Unauthenticated, ReasonUnauthenticated, "storage authentication failed", nil)
	}
	return ctx, nil
}

// unknownMethodStatus refuses a call the method table does not name. A
// server built with ServerOptions serves only the storage services, so such
// a call never reaches a handler without the protocol checks.
func unknownMethodStatus() error {
	return status.Error(codes.Unimplemented, "unknown storage method")
}

// intercept applies the protocol checks to every unary storage call before
// the provider sees it, so each refusal here means nothing was committed.
func (s *Server) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	m := methodByFullName(info.FullMethod)
	if m == nil {
		return nil, unknownMethodStatus()
	}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			resp, err = nil, s.panicked(ctx, m, recovered)
		}
		s.logCall(ctx, m, started, err)
	}()
	callCtx, release, err := s.admit(ctx, m)
	// Every authenticated call, including one refused, answers with the
	// contract, revision and, once known, the serving dataset.
	if headerErr := grpc.SetHeader(ctx, s.responseHeader()); headerErr != nil {
		if err == nil {
			release()
		}
		return nil, status.Error(codes.Internal, "cannot set storage response headers")
	}
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = callCtx
	if message, ok := req.(proto.Message); ok && hasUnknownFields(message.ProtoReflect()) {
		return nil, unsupportedFieldStatus()
	}
	resp, err = handler(ctx, req)
	if err != nil {
		return nil, s.statusError(ctx, m, err)
	}
	if message, ok := resp.(proto.Message); ok {
		if field, invalid := firstInvalidString(message.ProtoReflect()); invalid {
			return nil, s.encodeError(ctx, m, fmt.Errorf("response field %s is not valid UTF-8", field))
		}
	}
	return resp, nil
}

// interceptStream applies the same checks to every streaming storage call.
// The request messages are checked for unknown fields as they arrive.
func (s *Server) interceptStream(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	m := methodByFullName(info.FullMethod)
	if m == nil {
		return unknownMethodStatus()
	}
	ctx := stream.Context()
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = s.panicked(ctx, m, recovered)
		}
		s.logCall(ctx, m, started, err)
	}()
	callCtx, release, err := s.admit(ctx, m)
	if headerErr := stream.SetHeader(s.responseHeader()); headerErr != nil {
		if err == nil {
			release()
		}
		return status.Error(codes.Internal, "cannot set storage response headers")
	}
	if err != nil {
		return err
	}
	defer release()
	ctx = callCtx
	err = handler(srv, &checkedStream{ServerStream: stream, ctx: ctx})
	if _, isStatus := status.FromError(err); err != nil && !isStatus {
		return s.providerError(ctx, m, err)
	}
	return err
}

// checkedStream gives a streaming handler the call's checked context and
// refuses any request message with a field the server does not know.
type checkedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *checkedStream) Context() context.Context { return s.ctx }

func (s *checkedStream) RecvMsg(message any) error {
	if err := s.ServerStream.RecvMsg(message); err != nil {
		return err
	}
	if decoded, ok := message.(proto.Message); ok && hasUnknownFields(decoded.ProtoReflect()) {
		return unsupportedFieldStatus()
	}
	return nil
}

func unsupportedFieldStatus() error {
	return errorStatus(codes.InvalidArgument, ReasonUnsupportedField,
		"the request carries a field this storage server does not know", nil)
}

// admit checks a call's contract and dataset headers, admits it within its
// class's concurrency limit, decodes its write envelope and caps its
// duration. release frees the call's admission when it ends.
func (s *Server) admit(ctx context.Context, m *method) (_ context.Context, release func(), err error) {
	header, _ := metadata.FromIncomingContext(ctx)
	if err := checkContract(header); err != nil {
		return nil, nil, err
	}
	if err := s.checkDataset(ctx, m, header); err != nil {
		return nil, nil, err
	}
	release = func() {}
	if slots := s.admission[m.class()]; slots != nil {
		select {
		case slots <- struct{}{}:
			release = func() { <-slots }
		default:
			return nil, nil, errorStatus(codes.Unavailable, ReasonOverloaded,
				"the storage server is at its "+m.class().String()+" call limit", nil)
		}
	}
	if ctx, err = withEnvelope(ctx, header); err != nil {
		release()
		return nil, nil, err
	}
	if limit := s.maxDuration[m.class()]; limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		admitted := release
		release = func() { cancel(); admitted() }
	}
	return ctx, release, nil
}

// checkContract refuses a call without the orka.storage.v1 contract header
// and a protocol revision. The server serves every older revision.
func checkContract(header metadata.MD) error {
	contracts := header.Get(HeaderContract)
	revisions := header.Get(HeaderRevision)
	if len(contracts) != 1 || contracts[0] != storagev1.ContractVersion || len(revisions) != 1 {
		return errorStatus(codes.FailedPrecondition, ReasonContractMismatch,
			"the call does not name contract "+storagev1.ContractVersion, nil)
	}
	if revision, err := strconv.Atoi(revisions[0]); err != nil || revision < 1 {
		return errorStatus(codes.FailedPrecondition, ReasonContractMismatch, "the call names an invalid protocol revision", nil)
	}
	return nil
}

// checkDataset refuses a call that does not name the serving dataset. Only
// Describe and HealthCheck may omit the header, so that a client can learn
// the dataset before it binds; when they name one, it must match too.
func (s *Server) checkDataset(ctx context.Context, m *method, header metadata.MD) error {
	values := header.Get(HeaderDataset)
	if len(values) == 0 && m.class() == ClassHealth {
		return nil
	}
	if len(values) != 1 || values[0] == "" {
		return datasetMismatchStatus("the call must name the dataset it expects")
	}
	serving, err := s.servingDataset(ctx)
	if err != nil {
		if encoded, ok := contractStatus(err); ok {
			return encoded
		}
		s.logger.ErrorContext(ctx, "storage server cannot describe its provider", "method", m.name, "error", err)
		return errorStatus(codes.Unavailable, ReasonUnavailable, "the storage server cannot determine its dataset", nil)
	}
	if values[0] != serving {
		return datasetMismatchStatus("the call names a dataset this storage server does not serve")
	}
	return nil
}

func datasetMismatchStatus(message string) error {
	return errorStatus(codes.FailedPrecondition, ReasonDatasetMismatch, message, nil)
}

// withEnvelope decodes a write's envelope header into ctx, where the provider
// reads it exactly as it does in process.
func withEnvelope(ctx context.Context, header metadata.MD) (context.Context, error) {
	values := header.Get(HeaderEnvelope)
	if len(values) == 0 {
		return ctx, nil
	}
	if len(values) > 1 {
		return ctx, errorStatus(codes.InvalidArgument, ReasonValidation, "the call carries more than one write envelope",
			map[string]string{messageKey: "the call carries more than one write envelope"})
	}
	var encoded storagepb.WriteEnvelope
	if err := proto.Unmarshal([]byte(values[0]), &encoded); err != nil {
		return ctx, errorStatus(codes.InvalidArgument, ReasonValidation, "the write envelope cannot be decoded",
			map[string]string{messageKey: "the write envelope cannot be decoded"})
	}
	if hasUnknownFields(encoded.ProtoReflect()) {
		return ctx, errorStatus(codes.InvalidArgument, ReasonUnsupportedField,
			"the write envelope carries a field this storage server does not know", nil)
	}
	envelope, err := fromWireWriteEnvelope(&encoded)
	if err != nil {
		return ctx, requestStatus(err)
	}
	return storagev1.WithWriteEnvelope(ctx, envelope), nil
}

// responseHeader names the contract, this library's revision and, once a
// description has reported it, the serving dataset.
func (s *Server) responseHeader() metadata.MD {
	header := metadata.Pairs(HeaderContract, storagev1.ContractVersion, HeaderRevision, strconv.Itoa(ProtocolRevision))
	if dataset := s.DatasetID(); dataset != "" {
		header.Set(HeaderDataset, dataset)
	}
	return header
}

// libraryDescribe sets the server library's part of a Describe response.
func (s *Server) libraryDescribe(resp *storagepb.DescribeResponse, _ storagev1.ProviderInfo) {
	resp.Server = serverInfoToWire(s.info)
}

// blobLimit is the largest blob an upload of m may carry.
func (s *Server) blobLimit(m *method) int64 {
	if m.contract == storagev1.ArtifactsContract {
		limit := s.artifactLimit.Load()
		if limit <= 0 {
			limit = storagev1.MaxArtifactBytes
		}
		return min(limit, s.maxBlobBytes)
	}
	return s.maxBlobBytes
}

// receiveUpload reads a streaming write's request and reassembles its blob.
// The provider runs only after the client ends the stream and the blob
// matches its declared size and digest, so a refused or broken upload never
// reaches it.
func (s *Server) receiveUpload(stream grpc.ServerStream, m *method, request proto.Message) error {
	header, _ := metadata.FromIncomingContext(stream.Context())
	declared, err := parseBlobHeader(header)
	if err != nil {
		return blobValidationStatus(err)
	}
	if limit := s.blobLimit(m); declared.size > limit {
		encoded, _ := contractStatus(storagev1.TooLargeError{What: m.name + " blob", Size: declared.size, Limit: limit})
		return encoded
	}
	if err := stream.RecvMsg(request); err != nil {
		if errors.Is(err, io.EOF) {
			return blobValidationStatus(fmt.Errorf("%w: the stream carries no request", errBlobFraming))
		}
		return err
	}
	err = reassemble(request, func() (proto.Message, error) {
		part := request.ProtoReflect().New().Interface()
		return part, stream.RecvMsg(part)
	}, declared)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errBlobFraming):
		return blobValidationStatus(err)
	case errors.Is(err, errBlobShort), errors.Is(err, errBlobDigest):
		return errorStatus(codes.Unavailable, ReasonUnavailable, err.Error(), nil)
	default:
		return err
	}
}

func blobValidationStatus(err error) error {
	message := sanitizeText(err.Error(), messageLimit)
	return errorStatus(codes.InvalidArgument, ReasonValidation, message, map[string]string{messageKey: message})
}

// sendDownload sends a streaming read's response with its blob in parts.
func (s *Server) sendDownload(stream grpc.ServerStream, m *method, response proto.Message) error {
	ctx := stream.Context()
	path := blobPathOf(response.ProtoReflect().Descriptor())
	data := takeBlob(response.ProtoReflect(), path)
	if field, invalid := firstInvalidString(response.ProtoReflect()); invalid {
		return s.encodeError(ctx, m, fmt.Errorf("response field %s is not valid UTF-8", field))
	}
	if size := proto.Size(response); size > MaxMessageBytes-MaxChunkBytes {
		return s.encodeError(ctx, m, fmt.Errorf("the response is %d bytes without its blob", size))
	}
	if err := stream.SetHeader(metadata.Pairs(describeBlob(data).pairs()...)); err != nil {
		return status.Error(codes.Internal, "cannot set storage response headers")
	}
	return sendBlob(stream.SendMsg, response, path, data)
}

// requestStatus reports a request the server cannot decode into Go values.
func requestStatus(err error) error {
	if encoded, ok := contractStatus(err); ok {
		return encoded
	}
	return errorStatus(codes.InvalidArgument, ReasonValidation, "the request cannot be decoded",
		map[string]string{messageKey: "the request cannot be decoded"})
}

// statusError passes a status the server library built and encodes any
// other error the handler returned.
func (s *Server) statusError(ctx context.Context, m *method, err error) error {
	if st, ok := status.FromError(err); ok && storageErrorInfo(st) != nil {
		return err
	}
	return s.providerError(ctx, m, err)
}

// providerError encodes a provider's error. A contract error keeps its
// reason; any other error is logged with a correlation ID and reported
// without its text.
func (s *Server) providerError(ctx context.Context, m *method, err error) error {
	if encoded, ok := contractStatus(err); ok {
		return encoded
	}
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage provider error", "method", m.name, "correlation", correlation, "error", err)
	return providerErrorStatus(correlation)
}

// encodeError reports a provider result the server cannot encode. The call
// may have committed, so the error never carries a contract reason that would
// tell the client nothing happened.
func (s *Server) encodeError(ctx context.Context, m *method, err error) error {
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage response cannot be encoded", "method", m.name, "correlation", correlation, "error", err)
	return providerErrorStatus(correlation)
}

// panicked reports a provider panic as a provider error.
func (s *Server) panicked(ctx context.Context, m *method, recovered any) error {
	correlation := newCorrelationID()
	s.logger.ErrorContext(ctx, "storage provider panicked", "method", m.name, "correlation", correlation,
		"panic", fmt.Sprint(recovered))
	return providerErrorStatus(correlation)
}

// logCall records one call without request or response content.
func (s *Server) logCall(ctx context.Context, m *method, started time.Time, err error) {
	code, reason := codes.OK, ""
	if err != nil {
		st := status.Convert(err)
		code = st.Code()
		if info := storageErrorInfo(st); info != nil {
			reason = info.GetReason()
		}
	}
	duration := time.Since(started)
	if s.observer != nil {
		s.observer.ObserveCall(CallInfo{Method: m.name, Kind: m.kind, Class: m.class(), Code: code, Reason: reason, Duration: duration})
	}
	s.logger.DebugContext(ctx, "storage call", "method", m.name, "kind", m.kind.String(), "code", code.String(),
		"reason", reason, "duration", duration, "dataset", s.DatasetID())
}
