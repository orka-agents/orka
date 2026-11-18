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
	// Authenticator, when set, authenticates every storage call before its
	// request is decoded. It does not cover other services registered on
	// the same grpc.Server, and without one the server library
	// authenticates nothing.
	Authenticator Authenticator
	// Logger receives one entry per call and the text of the provider
	// errors the server does not forward, which can quote stored values. It
	// never receives request or response messages. Nil means
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
	// contract, revision and serving dataset. A call that names, or must
	// name, its dataset needs the serving dataset first.
	header, _ := metadata.FromIncomingContext(ctx)
	served, describeErr := s.servingDataset(ctx, m, header)
	if err := grpc.SetHeader(ctx, s.responseHeader()); err != nil {
		correlation := newCorrelationID()
		s.logger.ErrorContext(ctx, "storage response headers cannot be set", "method", m.name, "correlation", correlation,
			"error", err)
		return nil, providerErrorStatus(correlation)
	}
	if err := checkContract(header); err != nil {
		return nil, err
	}
	if message, ok := req.(proto.Message); ok && hasUnknownFields(message.ProtoReflect()) {
		return nil, errorStatus(codes.InvalidArgument, ReasonUnsupportedField,
			"the request carries a field this storage server does not know", nil)
	}
	if err := s.checkDataset(ctx, m, header, served, describeErr); err != nil {
		return nil, err
	}
	if ctx, err = withEnvelope(ctx, m, header); err != nil {
		return nil, err
	}
	if ctx, err = withTaskDataFence(ctx, header); err != nil {
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
// reads it exactly as it does in process. Only a write carries an envelope,
// so the server refuses one on a read or an administrative call.
func withEnvelope(ctx context.Context, m *method, header metadata.MD) (context.Context, error) {
	var encoded storagepb.WriteEnvelope
	found, err := decodeHeader(header, HeaderEnvelope, "write envelope", &encoded)
	if err != nil || !found {
		return ctx, err
	}
	if m.kind != storagev1.MethodWrite {
		return ctx, validationStatus("a " + m.kind.String() + " call carries no write envelope")
	}
	envelope, err := fromWireWriteEnvelope(&encoded, true)
	if err != nil {
		return ctx, requestStatus(err)
	}
	return storagev1.WithWriteEnvelope(ctx, envelope), nil
}

// withTaskDataFence decodes a call's Task data fence header into ctx, where
// the provider reads it exactly as it does in process.
func withTaskDataFence(ctx context.Context, header metadata.MD) (context.Context, error) {
	var encoded storagepb.TaskDataFence
	found, err := decodeHeader(header, HeaderTaskDataFence, "Task data fence", &encoded)
	if err != nil || !found {
		return ctx, err
	}
	fence, err := fromWireTaskDataFence(&encoded, true)
	if err != nil {
		return ctx, requestStatus(err)
	}
	return storagev1.WithTaskDataFence(ctx, fence), nil
}

// decodeHeader decodes the binary header key into message. found is false
// when the call does not carry the header.
func decodeHeader(header metadata.MD, key, what string, message proto.Message) (found bool, err error) {
	values := header.Get(key)
	if len(values) == 0 {
		return false, nil
	}
	if len(values) > 1 {
		return false, validationStatus("the call carries more than one " + what)
	}
	if err := proto.Unmarshal([]byte(values[0]), message); err != nil {
		return false, validationStatus("the " + what + " cannot be decoded")
	}
	if hasUnknownFields(message.ProtoReflect()) {
		return false, errorStatus(codes.InvalidArgument, ReasonUnsupportedField,
			"the "+what+" carries a field this storage server does not know", nil)
	}
	if field, invalid := firstInvalidString(message.ProtoReflect()); invalid {
		return false, validationStatus("the " + what + " field " + field + " is not valid UTF-8")
	}
	return true, nil
}

// checkDigest refuses a write whose envelope's request digest is not the
// digest of the request the server decoded, before the provider runs, so an
// operation ID can never be bound to a request other than the one its client
// digested. A write without an envelope has no digest to check.
func (s *Server) checkDigest(ctx context.Context, m *method, args ...any) error {
	envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
	if !ok {
		return nil
	}
	digest, err := storagev1.RequestDigest(m.name, args...)
	if err != nil {
		return requestStatus(err)
	}
	if digest != envelope.RequestDigest {
		return validationStatus("the request does not match its write envelope's request digest")
	}
	return nil
}

// servingDataset returns the dataset the server checks a call against. A
// read or an administrative call that names a dataset describes the provider
// again before it runs, so a provider whose dataset changed while it serves
// never answers it for the dataset it served before. A write is checked
// against the dataset the provider last described, as its envelope's dataset
// is checked again by the provider itself, and a Describe or HealthCheck that
// names none needs no dataset. libraryDescribe updates the dataset on every
// Describe.
func (s *Server) servingDataset(ctx context.Context, m *method, header metadata.MD) (string, error) {
	loaded := s.dataset.Load()
	known := loaded != nil && *loaded != ""
	omitted := m.mayOmitDataset() && len(header.Get(HeaderDataset)) == 0
	if known && (omitted || m.kind == storagev1.MethodWrite) {
		return *loaded, nil
	}
	if omitted {
		return "", nil
	}
	info, err := s.state.Describe(ctx)
	if err != nil {
		return "", err
	}
	if info.DatasetID == "" {
		return "", errors.New("the provider describes no dataset")
	}
	dataset := info.DatasetID
	s.dataset.Store(&dataset)
	return dataset, nil
}

// checkDataset refuses, before the provider runs, a call that names a
// dataset other than the serving one, or that names none when it must.
func (s *Server) checkDataset(ctx context.Context, m *method, header metadata.MD, served string, describeErr error) error {
	named := header.Get(HeaderDataset)
	switch {
	case len(named) > 1:
		return validationStatus("the call names more than one dataset")
	case len(named) == 0 && m.mayOmitDataset():
		return nil
	case len(named) == 0:
		return validationStatus("a " + m.name + " call must name the dataset it expects")
	case describeErr != nil:
		if encoded, ok := contractStatus(describeErr); ok {
			return encoded
		}
		s.logger.ErrorContext(ctx, "storage provider cannot describe its dataset", "method", m.name, "error", describeErr)
		return errorStatus(codes.Unavailable, ReasonUnavailable, "the storage server cannot name its dataset", nil)
	case named[0] != served:
		return errorStatus(codes.FailedPrecondition, ReasonDatasetMismatch,
			"the call names a dataset this storage server does not serve", nil)
	}
	return nil
}

// responseHeader names the contract, this library's revision and, once the
// server knows it, the serving dataset.
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
	return validationStatus("the request cannot be decoded")
}

// validationStatus refuses a request with a VALIDATION reason and a
// client-safe message.
func validationStatus(message string) error {
	return errorStatus(codes.InvalidArgument, ReasonValidation, message, map[string]string{messageKey: message})
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
	s.logger.DebugContext(ctx, "storage call", "method", m.name, "kind", m.kind.String(), "code", code.String(),
		"reason", reason, "duration", time.Since(started), "dataset", dataset)
}
