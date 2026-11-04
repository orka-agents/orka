package wire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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
// server decodes the request.
type Authenticator interface {
	Authenticate(ctx context.Context, fullMethod string, header metadata.MD) error
}

// ServerConfig configures a Server.
type ServerConfig struct {
	// State is the state.v1 provider the server serves. It is required.
	State storagev1.StateProvider
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
}

// Server serves a storage provider over gRPC.
type Server struct {
	state         storagev1.StateProvider
	info          ServerInfo
	authenticator Authenticator
	logger        *slog.Logger
	dataset       atomic.Pointer[string]
}

// NewServer returns a Server for config.
func NewServer(config ServerConfig) (*Server, error) {
	if config.State == nil {
		return nil, errors.New("storage server requires a state provider")
	}
	info := config.Info
	info.ProtocolRevision = ProtocolRevision
	info.LibraryVersion = libraryVersion()
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{state: config.State, info: info, authenticator: config.Authenticator, logger: logger}, nil
}

// ServerOptions returns the options a gRPC server needs to serve this
// Server: authentication before decoding, the call interceptor, message size
// limits, keepalive enforcement that admits the client's pings, and a maximum
// connection age so long-lived connections spread across adapter replicas.
// Callers add TLS credentials.
func (s *Server) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.InTapHandle(s.tap),
		grpc.ChainUnaryInterceptor(s.intercept),
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

// Register registers the storage services on registrar. A gRPC server built
// with ServerOptions answers an RPC it does not serve with UNIMPLEMENTED.
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	s.registerServices(registrar)
}

// tap authenticates a call before its request is read.
func (s *Server) tap(ctx context.Context, info *tap.Info) (context.Context, error) {
	if s.authenticator == nil || methodByFullName(info.FullMethodName) == nil {
		return ctx, nil
	}
	if err := s.authenticator.Authenticate(ctx, info.FullMethodName, info.Header); err != nil {
		return ctx, errorStatus(codes.Unauthenticated, ReasonUnauthenticated, "storage authentication failed", nil)
	}
	return ctx, nil
}

// intercept applies the protocol checks to every storage call before the
// provider sees it, so each refusal here means nothing was committed.
func (s *Server) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	m := methodByFullName(info.FullMethod)
	if m == nil {
		return handler(ctx, req)
	}
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			correlation := newCorrelationID()
			s.logger.ErrorContext(ctx, "storage provider panicked", "method", m.name, "correlation", correlation,
				"panic", fmt.Sprint(recovered))
			resp, err = nil, providerErrorStatus(correlation)
		}
		s.logCall(ctx, m, started, err)
	}()
	// Every authenticated call, including one refused below, answers with the
	// contract, revision and serving dataset.
	if err := grpc.SetHeader(ctx, s.responseHeader()); err != nil {
		return nil, status.Error(codes.Internal, "cannot set storage response headers")
	}
	header, _ := metadata.FromIncomingContext(ctx)
	if err := checkContract(header); err != nil {
		return nil, err
	}
	if message, ok := req.(proto.Message); ok && hasUnknownFields(message.ProtoReflect()) {
		return nil, errorStatus(codes.InvalidArgument, ReasonUnsupportedField,
			"the request carries a field this storage server does not know", nil)
	}
	if ctx, err = withEnvelope(ctx, header); err != nil {
		return nil, err
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
// Describe has reported it, the serving dataset.
func (s *Server) responseHeader() metadata.MD {
	header := metadata.Pairs(HeaderContract, storagev1.ContractVersion, HeaderRevision, strconv.Itoa(ProtocolRevision))
	if dataset := s.dataset.Load(); dataset != nil && *dataset != "" {
		header.Set(HeaderDataset, *dataset)
	}
	return header
}

// libraryDescribe sets the server library's part of a Describe response and
// notes the serving dataset for later response headers.
func (s *Server) libraryDescribe(resp *storagepb.DescribeResponse, info storagev1.ProviderInfo) {
	resp.Server = serverInfoToWire(s.info)
	dataset := info.DatasetID
	s.dataset.Store(&dataset)
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
	dataset := ""
	if loaded := s.dataset.Load(); loaded != nil {
		dataset = *loaded
	}
	s.logger.DebugContext(ctx, "storage call", "method", m.name, "kind", int(m.kind), "code", code.String(),
		"reason", reason, "duration", time.Since(started), "dataset", dataset)
}
